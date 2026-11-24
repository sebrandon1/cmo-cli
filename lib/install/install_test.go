package install

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/cluster"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/kube/kubetest"
)

func TestVersionAndChannel(t *testing.T) {
	if err := ValidateVersion("v1.18.0"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"1.18.0", "v1.18", "latest"} {
		if ValidateVersion(bad) == nil {
			t.Errorf("ValidateVersion(%q) accepted", bad)
		}
	}
	if got := ChannelForVersion("v1.18.3"); got != "stable-v1.18" {
		t.Errorf("ChannelForVersion = %q", got)
	}
	if got := CSVName("v1.18.3"); got != "cert-manager-operator.v1.18.3" {
		t.Errorf("CSVName = %q", got)
	}
}

func packageManifest(channel string) *unstructured.Unstructured {
	return kubetest.Obj("packages.operators.coreos.com/v1", "PackageManifest", CatalogNamespace, OperatorPackage,
		map[string]any{"status": map[string]any{"defaultChannel": channel}})
}

func TestResolveChannel(t *testing.T) {
	ctx := context.Background()
	c := kubetest.Clients(nil, packageManifest("stable-v1"))
	opts := OLMOptions{Source: CatalogSource, SourceNamespace: CatalogNamespace}
	tests := []struct {
		name string
		opts OLMOptions
		want string
	}{
		{"package default", opts, "stable-v1"},
		{"pinned version", OLMOptions{Version: "v1.17.0", SourceNamespace: CatalogNamespace}, "stable-v1.17"},
		{"explicit channel", OLMOptions{Channel: "tech-preview", Version: "v1.17.0"}, "tech-preview"},
	}
	for _, tt := range tests {
		got, err := ResolveChannel(ctx, c.Dyn, tt.opts)
		if err != nil || got != tt.want {
			t.Errorf("%s: ResolveChannel = %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
	empty := kubetest.Clients(nil)
	if _, err := ResolveChannel(ctx, empty.Dyn, opts); err == nil || !strings.Contains(err.Error(), "not in any catalog") {
		t.Errorf("missing package: err = %v", err)
	}
}

func TestOLMObjects(t *testing.T) {
	objs := OLMObjects(OLMOptions{Source: "redhat-operators", SourceNamespace: "openshift-marketplace"}, "stable-v1")
	kinds := []string{}
	for _, o := range objs {
		kinds = append(kinds, o.GetKind())
		if o.GetLabels()[kube.ManagedByLabel] != kube.ManagedByValue {
			t.Errorf("%s is not labeled as managed by cmo", o.GetKind())
		}
	}
	if !slices.Equal(kinds, []string{"Namespace", "OperatorGroup", "Subscription"}) {
		t.Errorf("kinds = %v", kinds)
	}
	sub := objs[2]
	if v, _, _ := unstructured.NestedString(sub.Object, "spec", "installPlanApproval"); v != "Automatic" {
		t.Errorf("unpinned approval = %q", v)
	}
	if _, ok, _ := unstructured.NestedString(sub.Object, "spec", "startingCSV"); ok {
		t.Error("unpinned subscription has a startingCSV")
	}
	pinned := OLMObjects(OLMOptions{Version: "v1.18.0"}, "stable-v1.18")[2]
	if v, _, _ := unstructured.NestedString(pinned.Object, "spec", "installPlanApproval"); v != "Manual" {
		t.Errorf("pinned approval = %q", v)
	}
	if v, _, _ := unstructured.NestedString(pinned.Object, "spec", "startingCSV"); v != "cert-manager-operator.v1.18.0" {
		t.Errorf("startingCSV = %q", v)
	}
}

func installPlan(name string, approved bool, csvs ...string) *unstructured.Unstructured {
	list := make([]any, len(csvs))
	for i, c := range csvs {
		list[i] = c
	}
	return kubetest.Obj("operators.coreos.com/v1alpha1", "InstallPlan", OperatorNamespace, name,
		map[string]any{"spec": map[string]any{"approved": approved, "clusterServiceVersionNames": list}})
}

func TestPlansToApprove(t *testing.T) {
	plans := []unstructured.Unstructured{
		*installPlan("ours", false, "cert-manager-operator.v1.18.0"),
		*installPlan("newer", false, "cert-manager-operator.v1.18.1"),
		*installPlan("done", true, "cert-manager-operator.v1.18.0"),
	}
	got := PlansToApprove(plans, "cert-manager-operator.v1.18.0")
	if !slices.Equal(got, []string{"ours"}) {
		t.Errorf("PlansToApprove = %v", got)
	}
}

func subWith(conds ...map[string]any) *unstructured.Unstructured {
	list := make([]any, len(conds))
	for i, c := range conds {
		list[i] = c
	}
	return kubetest.Obj("operators.coreos.com/v1alpha1", "Subscription", OperatorNamespace, OperatorPackage,
		map[string]any{"status": map[string]any{"conditions": list}})
}

func TestSubscriptionProblem(t *testing.T) {
	tests := []struct {
		name     string
		cond     map[string]any
		want     string
		terminal bool
	}{
		{"healthy", map[string]any{"type": "CatalogSourcesUnhealthy", "status": "False"}, "", false},
		{"catalog starting", map[string]any{"type": "ResolutionFailed", "status": "True", "message": "rpc error: code = Unavailable desc = connection refused"}, "resolution failed", false},
		{"no such package", map[string]any{"type": "ResolutionFailed", "status": "True", "message": "constraints not satisfiable: no operators found"}, "resolution failed", true},
		{"plan failed", map[string]any{"type": "InstallPlanFailed", "status": "True", "message": "bundle unpacking failed"}, "install plan failed", true},
		{"catalog unhealthy", map[string]any{"type": "CatalogSourcesUnhealthy", "status": "True"}, "catalog sources unhealthy", false},
	}
	for _, tt := range tests {
		msg, terminal := SubscriptionProblem(subWith(tt.cond))
		if !strings.HasPrefix(msg, tt.want) || (tt.want == "" && msg != "") || terminal != tt.terminal {
			t.Errorf("%s: SubscriptionProblem = %q, %v", tt.name, msg, terminal)
		}
	}
}

func csv(name, phase string) *unstructured.Unstructured {
	return kubetest.Obj("operators.coreos.com/v1alpha1", "ClusterServiceVersion", OperatorNamespace, name,
		map[string]any{"status": map[string]any{"phase": phase, "message": "it broke"}})
}

func TestWaitForCSVPinnedApprovesOnlyItsPlan(t *testing.T) {
	ctx := context.Background()
	const want = "cert-manager-operator.v1.18.0"
	c := kubetest.Clients(nil, subWith(), installPlan("ours", false, want), installPlan("newer", false, "cert-manager-operator.v1.18.1"), csv(want, "Succeeded"))
	got, err := WaitForCSV(ctx, c.Dyn, OLMOptions{Version: "v1.18.0"}, time.Second, nil)
	if err != nil || got != want {
		t.Fatalf("WaitForCSV = %q, %v", got, err)
	}
	for name, approved := range map[string]bool{"ours": true, "newer": false} {
		p, err := c.Dyn.Resource(kube.GVRInstallPlan).Namespace(OperatorNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got, _, _ := unstructured.NestedBool(p.Object, "spec", "approved"); got != approved {
			t.Errorf("plan %s approved = %v, want %v", name, got, approved)
		}
	}
}

func TestWaitForCSVUnpinnedFollowsInstalledCSV(t *testing.T) {
	sub := subWith()
	_ = unstructured.SetNestedField(sub.Object, "cert-manager-operator.v1.19.0", "status", "installedCSV")
	c := kubetest.Clients(nil, sub, csv("cert-manager-operator.v1.19.0", "Succeeded"))
	got, err := WaitForCSV(context.Background(), c.Dyn, OLMOptions{}, time.Second, nil)
	if err != nil || got != "cert-manager-operator.v1.19.0" {
		t.Errorf("WaitForCSV = %q, %v", got, err)
	}
}

func TestWaitForCSVFailures(t *testing.T) {
	ctx := context.Background()
	failed := kubetest.Clients(nil, subWith(), csv("cert-manager-operator.v1.18.0", "Failed"))
	if _, err := WaitForCSV(ctx, failed.Dyn, OLMOptions{Version: "v1.18.0"}, time.Second, nil); !errors.Is(err, ErrTerminal) || !strings.Contains(err.Error(), "it broke") {
		t.Errorf("failed CSV: err = %v", err)
	}
	unresolvable := kubetest.Clients(nil, subWith(map[string]any{"type": "ResolutionFailed", "status": "True", "message": "no operators found"}))
	if _, err := WaitForCSV(ctx, unresolvable.Dyn, OLMOptions{}, time.Second, nil); !errors.Is(err, ErrTerminal) {
		t.Errorf("unresolvable: err = %v", err)
	}
	pending := kubetest.Clients(nil, subWith())
	var notes []string
	_, err := WaitForCSV(ctx, pending.Dyn, OLMOptions{}, 50*time.Millisecond, func(s string) { notes = append(notes, s) })
	if err == nil || !strings.Contains(err.Error(), "did not install within") {
		t.Errorf("pending: err = %v", err)
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "resolve") {
		t.Errorf("notes = %v", notes)
	}
}

const manifest = `---
apiVersion: v1
kind: Namespace
metadata:
  name: cert-manager
---
# a comment-only document
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: certificates.cert-manager.io
spec:
  group: cert-manager.io
`

func TestParseManifest(t *testing.T) {
	objs, err := ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].GetKind() != "Namespace" {
		t.Fatalf("objs = %v", objs)
	}
	if got := WithoutCRDs(objs); len(got) != 1 || got[0].GetKind() != "Namespace" {
		t.Errorf("WithoutCRDs = %v", got)
	}
	if _, err := ParseManifest([]byte("---\n")); err == nil {
		t.Error("empty manifest accepted")
	}
	if _, err := ParseManifest([]byte("apiVersion: v1\nkind: ConfigMap\n")); err == nil {
		t.Error("object without a name accepted")
	}
}

func TestFetchManifest(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ok" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(manifest))
	}))
	defer srv.Close()
	b, err := FetchManifest(ctx, srv.Client(), srv.URL+"/ok")
	if err != nil || string(b) != manifest {
		t.Errorf("FetchManifest = %q, %v", b, err)
	}
	if _, err := FetchManifest(ctx, srv.Client(), srv.URL+"/missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing: err = %v", err)
	}
	path := filepath.Join(t.TempDir(), "cm.yaml")
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := FetchManifest(ctx, nil, path); err != nil || string(b) != manifest {
		t.Errorf("file: %q, %v", b, err)
	}
	if _, err := FetchManifest(ctx, nil, filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("missing file accepted")
	}
	if got := ManifestURL("v1.2.3"); got != "https://github.com/cert-manager/cert-manager/releases/download/v1.2.3/cert-manager.yaml" {
		t.Errorf("ManifestURL = %q", got)
	}
}

func availableDeployment(name string, labels map[string]string) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: OperandNamespace, Labels: labels},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
		}},
	}
}

func operands(version string) []runtime.Object {
	var out []runtime.Object
	for _, n := range Deployments {
		out = append(out, availableDeployment(n, map[string]string{versionLabel: version}))
	}
	return out
}

func crd(name, group string, labels map[string]string) *unstructured.Unstructured {
	u := kubetest.Obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", name, map[string]any{"spec": map[string]any{"group": group}})
	u.SetLabels(labels)
	return u
}

func TestGetStatusManifest(t *testing.T) {
	managed := kubetest.Obj("cert-manager.io/v1", "ClusterIssuer", "", "ca", map[string]any{"status": kubetest.Ready("True", "", "")})
	managed.SetLabels(map[string]string{kube.ManagedByLabel: kube.ManagedByValue})
	c := kubetest.Clients(operands("v1.21.3"),
		crd("certificates.cert-manager.io", "cert-manager.io", nil),
		managed,
		kubetest.Obj("cert-manager.io/v1", "Certificate", "default", "web", map[string]any{"status": kubetest.Ready("True", "", "")}),
		kubetest.Obj("cert-manager.io/v1", "Certificate", "default", "bad", map[string]any{"status": kubetest.Ready("False", "", "")}),
	)
	s, err := GetStatus(context.Background(), c.Kube, c.Dyn, cluster.Info{Platform: cluster.Kubernetes})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Installed || !s.Ready || s.Method != MethodManifest || s.Version != "v1.21.3" || s.CertManagerVersion != "v1.21.3" || !s.CRDs || !s.APIs {
		t.Errorf("status = %+v", s)
	}
	want := Counts{ClusterIssuers: 1, Certificates: 2, CertificatesReady: 1, Managed: 1}
	if s.Resources != want {
		t.Errorf("counts = %+v, want %+v", s.Resources, want)
	}
}

func TestGetStatusOLM(t *testing.T) {
	sub := subWith()
	_ = unstructured.SetNestedField(sub.Object, "stable-v1", "spec", "channel")
	_ = unstructured.SetNestedField(sub.Object, "cert-manager-operator.v1.18.0", "status", "installedCSV")
	c := kubetest.Clients(nil, sub, csv("cert-manager-operator.v1.18.0", "Succeeded"))
	s, err := GetStatus(context.Background(), c.Kube, c.Dyn, cluster.Info{Platform: cluster.OpenShift, OLM: true})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Installed || s.Ready || s.Method != MethodOLM || s.Version != "v1.18.0" || s.CSVPhase != "Succeeded" || s.Channel != "stable-v1" {
		t.Errorf("status = %+v", s)
	}
	if len(s.Deployments) != 3 || s.Deployments[0].Found {
		t.Errorf("deployments = %+v", s.Deployments)
	}
}

func TestGetStatusNotInstalled(t *testing.T) {
	c := kubetest.Clients(nil)
	s, err := GetStatus(context.Background(), c.Kube, c.Dyn, cluster.Info{Platform: cluster.Kubernetes})
	if err != nil || s.Installed || s.Ready || s.CRDs {
		t.Errorf("status = %+v, %v", s, err)
	}
}

func TestUninstallOLM(t *testing.T) {
	ctx := context.Background()
	c := kubetest.Clients(nil,
		subWith(),
		func() *unstructured.Unstructured {
			u := csv("cert-manager-operator.v1.18.0", "Succeeded")
			u.SetLabels(map[string]string{olmLabel: ""})
			return u
		}(),
		kubetest.Obj("v1", "Namespace", "", OperatorNamespace, nil),
		kubetest.Obj("v1", "Namespace", "", OperandNamespace, nil),
		kubetest.Obj("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "", WebhookConfigName, nil),
		crd("certificates.cert-manager.io", "cert-manager.io", nil),
		crd("certmanagers.operator.openshift.io", "operator.openshift.io", map[string]string{olmLabel: ""}),
		crd("widgets.example.io", "example.io", nil),
		kubetest.Obj("operator.openshift.io/v1alpha1", "CertManager", "", "cluster", nil),
		labeled(kubetest.Obj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "cert-manager-controller-issuers", nil), "app.kubernetes.io/instance", "cert-manager"),
		labeled(kubetest.Obj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "cert-manager-cainjector", nil), "app.kubernetes.io/instance", "cert-manager"),
		labeled(kubetest.Obj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "cert-manager-operator-metrics-reader", nil), "app.kubernetes.io/part-of", "cert-manager-operator"),
		kubetest.Obj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "admin", nil),
	)
	changes, err := UninstallOLM(ctx, c.Dyn, false)
	if err != nil {
		t.Fatal(err)
	}
	removed := removedRefs(changes)
	for _, want := range []string{
		"cert-manager-operator/Subscription/openshift-cert-manager-operator",
		"cert-manager-operator/ClusterServiceVersion/cert-manager-operator.v1.18.0",
		"ValidatingWebhookConfiguration/cert-manager-webhook",
		"Namespace/cert-manager-operator", "Namespace/cert-manager",
		"ClusterRole/cert-manager-controller-issuers", "ClusterRoleBinding/cert-manager-cainjector",
		"ClusterRole/cert-manager-operator-metrics-reader",
	} {
		if !slices.Contains(removed, want) {
			t.Errorf("%s was not removed; removed %v", want, removed)
		}
	}
	if slices.Contains(removed, "CertManager/cluster") {
		t.Error("deleted the CertManager config; the operator would only recreate it")
	}
	if slices.Contains(removed, "ClusterRole/admin") {
		t.Error("removed an unrelated ClusterRole")
	}
	if slices.ContainsFunc(removed, func(s string) bool { return strings.HasPrefix(s, "CustomResourceDefinition") }) {
		t.Error("CRDs removed without purge")
	}
	changes, err = UninstallOLM(ctx, c.Dyn, true)
	if err != nil {
		t.Fatal(err)
	}
	removed = removedRefs(changes)
	want := []string{"CustomResourceDefinition/certificates.cert-manager.io", "CustomResourceDefinition/certmanagers.operator.openshift.io"}
	if !slices.Equal(removed, want) {
		t.Errorf("purge removed %v, want %v", removed, want)
	}
}

// removedRefs are the refs of the changes that deleted something.
func removedRefs(changes []apply.Change) []string {
	var out []string
	for _, c := range changes {
		if c.Action == apply.Removed {
			out = append(out, c.Ref())
		}
	}
	return out
}

func TestWaitNamespacesGone(t *testing.T) {
	ctx := context.Background()
	c := kubetest.Clients(nil, kubetest.Obj("v1", "Namespace", "", "stuck", nil))
	if err := WaitNamespacesGone(ctx, c.Dyn, []string{"gone"}, time.Second, nil); err != nil {
		t.Errorf("gone: %v", err)
	}
	if err := WaitNamespacesGone(ctx, c.Dyn, []string{"stuck"}, 50*time.Millisecond, nil); err == nil || !strings.Contains(err.Error(), "stuck is still terminating") {
		t.Errorf("stuck: err = %v", err)
	}
}

func TestCheckAPIAndOperands(t *testing.T) {
	ctx := context.Background()
	c := kubetest.Clients(operands("v1.21.3"))
	if err := WaitForOperands(ctx, c.Kube, time.Second, nil); err != nil {
		t.Errorf("WaitForOperands: %v", err)
	}
	if err := CheckAPI(ctx, c.Dyn, time.Second, nil); err != nil {
		t.Errorf("CheckAPI: %v", err)
	}
}

func labeled(u *unstructured.Unstructured, k, v string) *unstructured.Unstructured {
	u.SetLabels(map[string]string{k: v})
	return u
}

func TestClearFinalizers(t *testing.T) {
	ctx := context.Background()
	def := crd("certmanagers.operator.openshift.io", "operator.openshift.io", nil)
	_ = unstructured.SetNestedField(def.Object, "certmanagers", "spec", "names", "plural")
	_ = unstructured.SetNestedSlice(def.Object, []any{
		map[string]any{"name": "v1alpha1", "storage": true},
	}, "spec", "versions")
	stuck := kubetest.Obj("operator.openshift.io/v1alpha1", "CertManager", "", "cluster", nil)
	stuck.SetFinalizers([]string{"cert-manager-operator.operator.openshift.io/cert-manager-controller-deployment"})
	c := kubetest.Clients(nil, def, stuck)
	if err := clearFinalizers(ctx, c.Dyn, def); err != nil {
		t.Fatal(err)
	}
	got, err := c.Dyn.Resource(kube.GVRCertManagerOperator).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetFinalizers()) != 0 {
		t.Errorf("finalizers = %v", got.GetFinalizers())
	}
	if _, ok := crdResource(crd("x.example.io", "example.io", nil)); ok {
		t.Error("a CRD with no versions has a resource")
	}
}

func TestGetStatusLeftoverCRDs(t *testing.T) {
	c := kubetest.Clients(nil, crd("certmanagers.operator.openshift.io", "operator.openshift.io", map[string]string{olmLabel: ""}))
	s, err := GetStatus(context.Background(), c.Kube, c.Dyn, cluster.Info{Platform: cluster.OpenShift})
	if err != nil {
		t.Fatal(err)
	}
	if s.Installed || !s.CRDs || s.APIs {
		t.Errorf("status = %+v", s)
	}
}

// recordingDeleter stands in for the SSA deleter in UninstallManifest.
type recordingDeleter struct{ got []string }

func (d *recordingDeleter) Delete(_ context.Context, objs []*unstructured.Unstructured) ([]apply.Change, error) {
	var out []apply.Change
	for _, o := range objs {
		d.got = append(d.got, o.GetKind()+"/"+o.GetName())
		out = append(out, apply.Change{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), Action: apply.Removed})
	}
	return out, nil
}

func TestUninstallManifest(t *testing.T) {
	ctx := context.Background()
	objs, err := ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	c := kubetest.Clients(nil, crd("certificates.cert-manager.io", "cert-manager.io", nil),
		labeled(kubetest.Obj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "cert-manager-edit", nil), "app.kubernetes.io/instance", "cert-manager"))
	d := &recordingDeleter{}
	changes, err := UninstallManifest(ctx, d, c.Dyn, objs, false)
	if err != nil {
		t.Fatal(err)
	}
	// The manifest's CRD stays without purge.
	if !slices.Equal(d.got, []string{"Namespace/cert-manager"}) {
		t.Errorf("deleter got %v", d.got)
	}
	n := 0
	for _, ch := range changes {
		if ch.Kind == "Namespace" && ch.Name == OperandNamespace {
			n++
		}
	}
	if n != 1 {
		t.Errorf("namespace cert-manager reported %d times: %v", n, changes)
	}
	if !slices.Contains(removedRefs(changes), "ClusterRole/cert-manager-edit") {
		t.Errorf("operand RBAC not removed: %v", changes)
	}

	// Leftover CRDs with nothing installed: purge deletes them.
	changes, err = UninstallManifest(ctx, &recordingDeleter{}, c.Dyn, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(removedRefs(changes), "CustomResourceDefinition/certificates.cert-manager.io") {
		t.Errorf("purge left the CRD: %v", changes)
	}
}

func TestClearFinalizersNamespaced(t *testing.T) {
	ctx := context.Background()
	def := crd("istiocsrs.operator.openshift.io", "operator.openshift.io", nil)
	_ = unstructured.SetNestedField(def.Object, "istiocsrs", "spec", "names", "plural")
	_ = unstructured.SetNestedSlice(def.Object, []any{map[string]any{"name": "v1alpha1", "storage": true}}, "spec", "versions")
	stuck := kubetest.Obj("operator.openshift.io/v1alpha1", "IstioCSR", "istio-system", "default", nil)
	stuck.SetFinalizers([]string{"operator.openshift.io/istiocsr"})
	gvr := schema.GroupVersionResource{Group: "operator.openshift.io", Version: "v1alpha1", Resource: "istiocsrs"}
	c := &kube.Clients{Dyn: dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "IstioCSRList"}, stuck)}
	if err := clearFinalizers(ctx, c.Dyn, def); err != nil {
		t.Fatal(err)
	}
	got, err := c.Dyn.Resource(gvr).Namespace("istio-system").Get(ctx, "default", metav1.GetOptions{})
	if err != nil || len(got.GetFinalizers()) != 0 {
		t.Errorf("finalizers = %v, %v", got.GetFinalizers(), err)
	}
}

func TestGetStatusOLMMissingCSV(t *testing.T) {
	sub := subWith()
	_ = unstructured.SetNestedField(sub.Object, "cert-manager-operator.v1.20.0", "spec", "startingCSV")
	c := kubetest.Clients(nil, sub)
	s, err := GetStatus(context.Background(), c.Kube, c.Dyn, cluster.Info{Platform: cluster.OpenShift, OLM: true})
	if err != nil || s.CSV != "cert-manager-operator.v1.20.0" || s.CSVPhase != "Missing" || s.Version != "v1.20.0" {
		t.Errorf("status = %+v, %v", s, err)
	}
}
