package issuer

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/cert"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/kube/kubetest"
)

func TestRefs(t *testing.T) {
	c := NewRef("ca", "")
	if c.Kind != KindClusterIssuer || c.String() != "ClusterIssuer/ca" {
		t.Errorf("cluster ref = %+v %s", c, c)
	}
	n := NewRef("ca", "team")
	if n.Kind != KindIssuer || n.String() != "team/Issuer/ca" {
		t.Errorf("namespaced ref = %+v %s", n, n)
	}
}

func TestBuilders(t *testing.T) {
	ss, err := SelfSigned(NewRef("ss", ""))
	if err != nil || ss.GetKind() != KindClusterIssuer || Type(ss) != TypeSelfSigned {
		t.Errorf("SelfSigned = %v, %v", ss, err)
	}
	if ss.GetLabels()[kube.ManagedByLabel] != kube.ManagedByValue {
		t.Error("issuer is not labeled as managed by cmo")
	}
	ca, err := CA(NewRef("ca", "team"), "kp")
	if err != nil || ca.GetKind() != KindIssuer || ca.GetNamespace() != "team" || Type(ca) != TypeCA {
		t.Errorf("CA = %v, %v", ca, err)
	}
	if s, _, _ := unstructured.NestedString(ca.Object, "spec", "ca", "secretName"); s != "kp" {
		t.Errorf("secretName = %q", s)
	}
	if _, err := CA(NewRef("ca", ""), ""); err == nil {
		t.Error("CA without a secret accepted")
	}
}

func TestACME(t *testing.T) {
	r := NewRef("le", "")
	u, err := ACME(r, ACMEOptions{Server: "letsencrypt-staging", Email: "me@example.com", HTTP01IngressClass: "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if Type(u) != TypeACME {
		t.Errorf("Type = %s", Type(u))
	}
	acme, _, _ := unstructured.NestedMap(u.Object, "spec", "acme")
	if acme["server"] != "https://acme-staging-v02.api.letsencrypt.org/directory" || acme["email"] != "me@example.com" {
		t.Errorf("acme = %v", acme)
	}
	if key, _, _ := unstructured.NestedString(u.Object, "spec", "acme", "privateKeySecretRef", "name"); key != "le-account-key" {
		t.Errorf("account key = %q", key)
	}
	solvers, _, _ := unstructured.NestedSlice(u.Object, "spec", "acme", "solvers")
	class, _, _ := unstructured.NestedString(solvers[0].(map[string]any), "http01", "ingress", "ingressClassName")
	if class != "nginx" {
		t.Errorf("ingressClassName = %q", class)
	}

	dns, err := ACME(r, ACMEOptions{Server: "https://acme.test/dir", RFC2136: RFC2136{Nameserver: "10.0.0.53:53", TSIGKeyName: "k", TSIGAlgorithm: "HMACSHA256", TSIGSecret: "tsig"}})
	if err != nil {
		t.Fatal(err)
	}
	solvers, _, _ = unstructured.NestedSlice(dns.Object, "spec", "acme", "solvers")
	rfc, _, _ := unstructured.NestedMap(solvers[0].(map[string]any), "dns01", "rfc2136")
	ref, _ := rfc["tsigSecretSecretRef"].(map[string]any)
	if rfc["nameserver"] != "10.0.0.53:53" || ref["name"] != "tsig" || ref["key"] != "tsig-secret" {
		t.Errorf("rfc2136 = %v", rfc)
	}

	for name, o := range map[string]ACMEOptions{
		"http server":  {Server: "http://acme.test", HTTP01IngressClass: "nginx"},
		"no solver":    {Server: "letsencrypt"},
		"both solvers": {Server: "letsencrypt", HTTP01IngressClass: "nginx", RFC2136: RFC2136{Nameserver: "x:53"}},
	} {
		if _, err := ACME(r, o); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func issuerObj(kind, ns, name string, ready string) *unstructured.Unstructured {
	u := kubetest.Obj("cert-manager.io/v1", kind, ns, name, map[string]any{
		"spec":   map[string]any{"selfSigned": map[string]any{}},
		"status": kubetest.Ready(ready, "", "msg"),
	})
	return u
}

func threeIssuers() *kube.Clients {
	return kubetest.Clients(nil,
		issuerObj("Issuer", "b", "local", "True"),
		issuerObj("ClusterIssuer", "", "ca", "True"),
		issuerObj("Issuer", "a", "ca", "False"),
	)
}

func TestList(t *testing.T) {
	ctx := context.Background()
	c := threeIssuers()
	all, err := List(ctx, c.Dyn, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range all {
		got = append(got, s.String())
	}
	if !slices.Equal(got, []string{"ClusterIssuer/ca", "a/Issuer/ca", "b/Issuer/local"}) {
		t.Errorf("List = %v", got)
	}
	if all[1].Message != "msg" || all[0].Message != "" {
		t.Errorf("messages = %q, %q", all[0].Message, all[1].Message)
	}
	if inA, _ := List(ctx, c.Dyn, "a", ""); len(inA) != 2 {
		t.Errorf("List(a) = %v", inA)
	}
}

func TestGet(t *testing.T) {
	ctx := context.Background()
	c := threeIssuers()
	if _, err := Get(ctx, c.Dyn, NewRef("ca", "a")); err != nil {
		t.Error(err)
	}
	if _, err := Get(ctx, c.Dyn, NewRef("nope", "")); err == nil || !strings.Contains(err.Error(), "pass -n") {
		t.Errorf("Get missing cluster issuer: %v", err)
	}
	if _, err := Get(ctx, c.Dyn, NewRef("nope", "a")); err == nil || !strings.Contains(err.Error(), "leave out -n") {
		t.Errorf("Get missing issuer: %v", err)
	}
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	c := threeIssuers()
	tests := []struct {
		name, ns    string
		clusterOnly bool
		want        string
	}{
		{"ca", "a", false, KindIssuer},       // the namespace's Issuer wins
		{"ca", "a", true, KindClusterIssuer}, // unless asked for the ClusterIssuer
		{"ca", "b", false, KindClusterIssuer},
		{"local", "b", false, KindIssuer},
	}
	for _, tt := range tests {
		ref, err := Resolve(ctx, c.Dyn, tt.name, tt.ns, tt.clusterOnly)
		if err != nil || ref.Kind != tt.want || ref.Name != tt.name {
			t.Errorf("Resolve(%s, %s, %v) = %+v, %v; want %s", tt.name, tt.ns, tt.clusterOnly, ref, err, tt.want)
		}
	}
	_, err := Resolve(ctx, c.Dyn, "nope", "b", false)
	if err == nil || !strings.Contains(err.Error(), "available here: ca, local") {
		t.Errorf("Resolve missing: %v", err)
	}
	empty := kubetest.Clients(nil)
	if _, err := Resolve(ctx, empty.Dyn, "nope", "b", true); err == nil || !strings.Contains(err.Error(), "cmo issuer create") {
		t.Errorf("Resolve with no issuers: %v", err)
	}
}

func TestWaitReady(t *testing.T) {
	ctx := context.Background()
	c := kubetest.Clients(nil, issuerObj("ClusterIssuer", "", "ok", "True"), issuerObj("ClusterIssuer", "", "bad", "False"))
	if err := WaitReady(ctx, c.Dyn, NewRef("ok", ""), time.Second, nil); err != nil {
		t.Error(err)
	}
	if err := WaitReady(ctx, c.Dyn, NewRef("bad", ""), 50*time.Millisecond, nil); err == nil || !strings.Contains(err.Error(), "not ready after") || !strings.Contains(err.Error(), "msg") {
		t.Errorf("not ready: %v", err)
	}
}

// readyOnCreate makes the fake cluster act like cert-manager: everything is
// Ready, at revision 1, as soon as it is created.
func readyOnCreate(c *dynfake.FakeDynamicClient) {
	c.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		u := a.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		status := kubetest.Ready("True", "Ready", "")
		status["revision"] = int64(1)
		u.Object["status"] = status
		return false, nil, nil
	})
}

func TestGenerateCAAndDelete(t *testing.T) {
	ctx := context.Background()
	c := kubetest.Clients([]runtime.Object{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "root-root-ca", Namespace: ClusterResourceNamespace}}})
	readyOnCreate(c.Dyn.(*dynfake.FakeDynamicClient))
	r := NewRef("root", "")
	var steps []string
	if err := GenerateCA(ctx, c.Dyn, r, CAOptions{CommonName: "Lab Root"}, time.Second, func(s string) { steps = append(steps, s) }); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Errorf("steps = %v", steps)
	}
	ca, err := Get(ctx, c.Dyn, r)
	if err != nil {
		t.Fatal(err)
	}
	if ca.GetAnnotations()[AnnCACertificate] != "cert-manager/root-root-ca" || ca.GetAnnotations()[AnnCABootstrap] != "root-selfsigned" {
		t.Errorf("annotations = %v", ca.GetAnnotations())
	}
	checkRoot(t, c, "Lab Root")

	changes, err := DeleteManaged(ctx, c.Dyn, c.Kube, "")
	if err != nil {
		t.Fatal(err)
	}
	removed := removedRefs(changes)
	want := []string{"ClusterIssuer/root", "cert-manager/Certificate/root-root-ca", "cert-manager/Secret/root-root-ca", "ClusterIssuer/root-selfsigned"}
	if !slices.Equal(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	if left, _ := List(ctx, c.Dyn, "", ""); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	if ch, err := Delete(ctx, c.Dyn, c.Kube, r); err != nil || ch[0].Action != apply.Absent {
		t.Errorf("Delete again = %v, %v", ch, err)
	}
}

func removedRefs(changes []apply.Change) []string {
	var out []string
	for _, ch := range changes {
		if ch.Action == apply.Removed {
			out = append(out, ch.Ref())
		}
	}
	return out
}

func checkRoot(t *testing.T, c *kube.Clients, cn string) {
	t.Helper()
	root, err := c.Dyn.Resource(kube.GVRCertificate).Namespace(ClusterResourceNamespace).Get(context.Background(), "root-root-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if isCA, _, _ := unstructured.NestedBool(root.Object, "spec", "isCA"); !isCA {
		t.Error("root certificate is not a CA")
	}
	if got, _, _ := unstructured.NestedString(root.Object, "spec", "commonName"); got != cn {
		t.Errorf("commonName = %q, want %q", got, cn)
	}
}

func TestPlanCANamespaced(t *testing.T) {
	p := PlanCA(NewRef("ca", "team"))
	if p.Bootstrap.String() != "team/Issuer/ca-selfsigned" || p.CertNamespace != "team" || p.CertName != "ca-root-ca" {
		t.Errorf("plan = %+v", p)
	}
	co, err := p.RootCertificate(CAOptions{Duration: 24 * time.Hour, RenewBefore: 8 * time.Hour})
	if err != nil || co.CommonName != "ca-root-ca" || !co.IsCA || co.Duration != 24*time.Hour {
		t.Errorf("root = %+v, %v", co, err)
	}
}

func TestACMEServer(t *testing.T) {
	if ACMEServer("letsencrypt") != "https://acme-v02.api.letsencrypt.org/directory" || ACMEServer("https://x") != "https://x" {
		t.Error("ACMEServer shorthand")
	}
}

func TestDeleteACMERemovesAccountKey(t *testing.T) {
	ctx := context.Background()
	key := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "le-account-key", Namespace: ClusterResourceNamespace}}
	c := kubetest.Clients([]runtime.Object{key})
	u, err := ACME(NewRef("le", ""), ACMEOptions{Server: "letsencrypt", HTTP01IngressClass: "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, c.Dyn, u); err != nil {
		t.Fatal(err)
	}
	changes, err := Delete(ctx, c.Dyn, c.Kube, NewRef("le", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := removedRefs(changes); !slices.Equal(got, []string{"ClusterIssuer/le", "cert-manager/Secret/le-account-key"}) {
		t.Errorf("removed %v", got)
	}

	// An ACME issuer cmo didn't create keeps its key.
	theirs := kubetest.Obj("cert-manager.io/v1", "Issuer", "team", "theirs", map[string]any{"spec": map[string]any{"acme": map[string]any{
		"privateKeySecretRef": map[string]any{"name": "shared-key"},
	}}})
	shared := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shared-key", Namespace: "team"}}
	c = kubetest.Clients([]runtime.Object{shared}, theirs)
	if _, err := Delete(ctx, c.Dyn, c.Kube, NewRef("theirs", "team")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Kube.CoreV1().Secrets("team").Get(ctx, "shared-key", metav1.GetOptions{}); err != nil {
		t.Errorf("deleted the key of an issuer cmo didn't create: %v", err)
	}
}

func TestGetHintsTheOtherKind(t *testing.T) {
	ctx := context.Background()
	c := threeIssuers()
	if _, err := Get(ctx, c.Dyn, NewRef("local", "")); err == nil || !strings.Contains(err.Error(), "there is an Issuer local in b: pass -n b") {
		t.Errorf("ClusterIssuer miss: %v", err)
	}
	if _, err := Get(ctx, c.Dyn, NewRef("ca", "b")); err == nil || !strings.Contains(err.Error(), "there is a ClusterIssuer ca: leave out -n") {
		t.Errorf("Issuer miss: %v", err)
	}
}

func TestRootCertificateIsLabeled(t *testing.T) {
	co, err := PlanCA(NewRef("ca", "")).RootCertificate(CAOptions{})
	if err != nil || co.Labels[cert.RoleLabel] != cert.RoleCARoot {
		t.Errorf("labels = %v, %v", co.Labels, err)
	}
}
