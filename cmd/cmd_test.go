package cmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/sebrandon1/cmo-cli/lib/install"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/kube/kubetest"
)

func crd() *unstructured.Unstructured {
	return kubetest.Obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "certificates.cert-manager.io",
		map[string]any{"spec": map[string]any{"group": "cert-manager.io"}})
}

func TestVersionAndWide(t *testing.T) {
	SetVersion("v9.9.9")
	r := run(t, "version")
	if r.err != nil || r.stdout != "cmo version v9.9.9\n" {
		t.Errorf("version = %q, %v", r.stdout, r.err)
	}
	r = run(t, "version", "-o", "json")
	mustContain(t, "version json", r.stdout, `"version": "v9.9.9"`)
	r = run(t, "status", "-o", "wide")
	if r.err == nil || !strings.Contains(r.err.Error(), "no wide output") {
		t.Errorf("status -o wide: %v", r.err)
	}
	r = run(t, "status", "-o", "xml")
	if r.err == nil {
		t.Error("-o xml accepted")
	}
}

func TestStatus(t *testing.T) {
	use(t, fakeCluster(t, false, nil))
	r := run(t, "status")
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, "status", r.stdout, "kubernetes v1.34.2", "not installed", "cmo install")

	use(t, fakeCluster(t, false, operandDeployments("v1.21.3"), crd()))
	r = run(t, "status", "-o", "json")
	var st install.Status
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil {
		t.Fatalf("status json: %v\n%s", err, r.stdout)
	}
	if !st.Installed || !st.Ready || st.Version != "v1.21.3" || st.Method != "manifest" {
		t.Errorf("status = %+v", st)
	}
	r = run(t, "status")
	mustContain(t, "status", r.stdout, "ready", "v1.21.3 via manifest", "✓ cert-manager-webhook 1/1", "0 cluster issuers")
}

const testManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: cert-manager
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: certificates.cert-manager.io
`

func manifestServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testManifest))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/cert-manager.yaml"
}

func TestInstallKubernetes(t *testing.T) {
	rec := use(t, fakeCluster(t, false, operandDeployments("")))
	r := run(t, "install", "--manifest", manifestServer(t))
	if r.err != nil {
		t.Fatalf("install: %v\n%s", r.err, r.stderr)
	}
	if !slices.Equal(rec.applied, []string{"Namespace/cert-manager", "CustomResourceDefinition/certificates.cert-manager.io"}) {
		t.Errorf("applied %v", rec.applied)
	}
	mustContain(t, "install stdout", r.stdout, "cert-manager is ready (kubernetes, manifest)")
	mustContain(t, "install stderr", r.stderr, "Applying cert-manager", "webhook is serving")

	r = run(t, "install", "--manifest", manifestServer(t), "--dry-run")
	mustContain(t, "dry run", r.stdout, "kind: Namespace", "---", "kind: CustomResourceDefinition")

	r = run(t, "install", "--manifest", manifestServer(t), "--no-wait", "-o", "json")
	if r.err != nil || r.stderr != "" {
		t.Fatalf("json install: %v, stderr %q", r.err, r.stderr)
	}
	var res installResult
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil || res.Ready || len(res.Changes) != 2 {
		t.Errorf("json = %+v, %v\n%s", res, err, r.stdout)
	}
}

func TestInstallOpenShiftPinned(t *testing.T) {
	const csvName = "cert-manager-operator.v1.18.0"
	plan := kubetest.Obj("operators.coreos.com/v1alpha1", "InstallPlan", install.OperatorNamespace, "install-abc",
		map[string]any{"spec": map[string]any{"approved": false, "clusterServiceVersionNames": []any{csvName}}})
	csv := kubetest.Obj("operators.coreos.com/v1alpha1", "ClusterServiceVersion", install.OperatorNamespace, csvName,
		map[string]any{"status": map[string]any{"phase": "Succeeded"}})
	// A rerun: the Subscription is there from the first install, so the
	// operands it made count as an OLM install.
	sub := kubetest.Obj("operators.coreos.com/v1alpha1", "Subscription", install.OperatorNamespace, install.OperatorPackage, nil)
	c := fakeCluster(t, true, operandDeployments(""), plan, csv, sub)
	rec := use(t, c)
	r := run(t, "install", "--version", "v1.18.0")
	if r.err != nil {
		t.Fatalf("install: %v\n%s", r.err, r.stderr)
	}
	if !slices.Equal(rec.applied, []string{"Namespace/cert-manager-operator", "OperatorGroup/cert-manager-operator", "Subscription/openshift-cert-manager-operator"}) {
		t.Errorf("applied %v", rec.applied)
	}
	mustContain(t, "stderr", r.stderr, "channel stable-v1.18", csvName+" succeeded")
	mustContain(t, "stdout", r.stdout, csvName+" is ready (openshift, olm)")
	p, err := c.Dyn.Resource(kube.GVRInstallPlan).Namespace(install.OperatorNamespace).Get(t.Context(), "install-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := unstructured.NestedBool(p.Object, "spec", "approved"); !ok {
		t.Error("the pinned InstallPlan was not approved")
	}
}

func TestInstallFlagChecks(t *testing.T) {
	use(t, fakeCluster(t, true, nil))
	for _, args := range [][]string{
		{"install", "--manifest", "x.yaml"},
		{"install", "--version", "1.18"},
		{"install", "--platform", "eks"},
	} {
		if r := run(t, args...); r.err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	use(t, fakeCluster(t, false, nil))
	if r := run(t, "install", "--channel", "stable-v1"); r.err == nil || !strings.Contains(r.err.Error(), "OLM install on OpenShift") {
		t.Errorf("--channel on kubernetes: %v", r.err)
	}
	if r := run(t, "install", "--platform", "openshift"); r.err == nil || !strings.Contains(r.err.Error(), "OLM API") {
		t.Errorf("openshift without OLM: %v", r.err)
	}
	// Installed by manifest on an OpenShift cluster: install won't switch.
	use(t, fakeCluster(t, true, operandDeployments("v1.21.3")))
	if r := run(t, "install"); r.err == nil || !strings.Contains(r.err.Error(), "uninstall it first") {
		t.Errorf("method switch: %v", r.err)
	}
}

func TestUninstall(t *testing.T) {
	use(t, fakeCluster(t, false, nil))
	r := run(t, "uninstall")
	if r.err != nil || !strings.Contains(r.stdout, "not installed") {
		t.Errorf("not installed: %q, %v", r.stdout, r.err)
	}

	c := fakeCluster(t, false, operandDeployments("v1.21.3"), crd(), kubetest.Obj("cert-manager.io/v1", "Certificate", "default", "web", nil))
	rec := use(t, c)
	r = run(t, "uninstall")
	if r.err == nil || !strings.Contains(r.err.Error(), "--yes") {
		t.Errorf("without --yes: %v", r.err)
	}
	mustContain(t, "summary", r.stderr, "v1.21.3 (manifest)", "1 certificate and 0 issuers remain")

	// Release v1.21.3 is fetched from GitHub; point the download at a local server.
	old := httpClient
	t.Cleanup(func() { httpClient = old })
	httpClient = &http.Client{Transport: rewrite{to: manifestServer(t)}}
	r = run(t, "uninstall", "--yes", "--no-wait")
	if r.err != nil {
		t.Fatalf("uninstall: %v\n%s", r.err, r.stderr)
	}
	// The CRDs stay without --purge.
	if !slices.Equal(rec.deleted, []string{"Namespace/cert-manager"}) {
		t.Errorf("deleted %v", rec.deleted)
	}
	mustContain(t, "stdout", r.stdout, "cert-manager is uninstalled")
}

// rewrite sends every request to one URL.
type rewrite struct{ to string }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	out, err := http.NewRequestWithContext(req.Context(), req.Method, r.to, http.NoBody)
	if err != nil {
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(out)
}

func TestIssuerCommands(t *testing.T) {
	c := fakeCluster(t, false, nil)
	readyOnCreate(c)
	use(t, c)
	r := run(t, "issuer", "create", "selfsigned", "ss")
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, "create", r.stdout, "ClusterIssuer/ss created", "cmo cert create NAME --issuer ss")
	r = run(t, "issuer", "create", "selfsigned", "local", "-n", "team")
	mustContain(t, "create -n", r.stdout, "team/Issuer/local created", "-n team")
	r = run(t, "issuer", "create", "selfsigned", "ss")
	if r.err == nil || !strings.Contains(r.err.Error(), "already exists") {
		t.Errorf("duplicate: %v", r.err)
	}
	r = run(t, "issuer", "create", "ca", "root", "--generate")
	if r.err != nil {
		t.Fatalf("generate: %v\n%s", r.err, r.stderr)
	}
	mustContain(t, "generate", r.stderr, "root cert-manager/root-root-ca signed by ClusterIssuer/root-selfsigned")

	r = run(t, "issuer", "list")
	mustContain(t, "list", r.stdout, "KIND", "root-selfsigned", "selfsigned", "local", "team")
	r = run(t, "issuer", "list", "-o", "wide")
	mustContain(t, "list wide", r.stdout, "MANAGED")
	r = run(t, "issuer", "get", "root")
	mustContain(t, "get", r.stdout, "Type", "ca", "Generated root  cert-manager/root-root-ca")
	r = run(t, "issuer", "get", "root", "-o", "yaml")
	mustContain(t, "get yaml", r.stdout, "kind: ClusterIssuer", "secretName: root-root-ca")
	if strings.Contains(r.stdout, "managedFields") {
		t.Error("-o yaml shows managedFields")
	}

	r = run(t, "issuer", "delete")
	if r.err == nil {
		t.Error("delete with no names accepted")
	}
	r = run(t, "issuer", "delete", "root")
	mustContain(t, "delete", r.stdout, "ClusterIssuer/root deleted", "cert-manager/Certificate/root-root-ca deleted", "ClusterIssuer/root-selfsigned deleted")
	r = run(t, "issuer", "delete", "--all-managed")
	if r.err == nil || !strings.Contains(r.err.Error(), "--yes") {
		t.Errorf("all-managed without --yes: %v", r.err)
	}
	r = run(t, "issuer", "delete", "--all-managed", "-y")
	mustContain(t, "delete all", r.stdout, "ClusterIssuer/ss deleted", "team/Issuer/local deleted")
	r = run(t, "issuer", "list")
	mustContain(t, "empty list", r.stdout, "No issuers")
}

func TestIssuerCreateDryRunAndChecks(t *testing.T) {
	use(t, fakeCluster(t, false, nil))
	r := run(t, "issuer", "create", "ca", "root", "--generate", "--dry-run")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if n := strings.Count(r.stdout, "\n---\n"); n != 2 {
		t.Errorf("want 3 documents, got %d separators:\n%s", n, r.stdout)
	}
	mustContain(t, "dry run", r.stdout, "name: root-selfsigned", "isCA: true", "secretName: root-root-ca")
	if r := run(t, "issuer", "create", "ca", "x"); r.err == nil {
		t.Error("ca without --secret or --generate accepted")
	}
	if r := run(t, "issuer", "create", "ca", "x", "--secret", "s", "--common-name", "cn"); r.err == nil {
		t.Error("--common-name without --generate accepted")
	}
	r = run(t, "issuer", "create", "acme", "le", "--server", "letsencrypt", "--email", "a@b.c", "--http01-ingress-class", "nginx", "--dry-run")
	mustContain(t, "acme", r.stdout, "acme-v02.api.letsencrypt.org", "ingressClassName: nginx")
	if r := run(t, "issuer", "create", "acme", "le", "--server", "letsencrypt"); r.err == nil || !strings.Contains(r.err.Error(), "choose a solver") {
		t.Errorf("acme without solver: %v", r.err)
	}
}

// issuedSecret is a TLS secret holding a fresh certificate for web.example.com.
func issuedSecret(t *testing.T, ns, name string) *corev1.Secret {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "web"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(90 * 24 * time.Hour), DNSNames: []string{"web.example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})},
	}
}

func TestCertCommands(t *testing.T) {
	ca := kubetest.Obj("cert-manager.io/v1", "ClusterIssuer", "", "ca", map[string]any{"spec": map[string]any{"ca": map[string]any{"secretName": "x"}}})
	c := fakeCluster(t, false, []runtime.Object{issuedSecret(t, "default", "web-tls")}, ca)
	readyOnCreate(c)
	use(t, c)

	r := run(t, "cert", "create", "web", "--issuer", "ca", "--dns", "web.example.com", "--dry-run")
	mustContain(t, "dry run", r.stdout, "kind: Certificate", "kind: ClusterIssuer", "secretName: web-tls", "- web.example.com")
	r = run(t, "cert", "create", "web", "--issuer", "nope", "--dns", "web.example.com")
	if r.err == nil || !strings.Contains(r.err.Error(), "available here: ca") {
		t.Errorf("unknown issuer: %v", r.err)
	}
	r = run(t, "cert", "create", "web", "--issuer", "ca")
	if r.err == nil || !strings.Contains(r.err.Error(), "at least one of") {
		t.Errorf("no SANs: %v", r.err)
	}
	r = run(t, "cert", "create", "web", "--issuer", "ca", "--dns", "web.example.com,www.example.com")
	if r.err != nil {
		t.Fatalf("create: %v\n%s", r.err, r.stderr)
	}
	mustContain(t, "create", r.stdout, "certificate default/web issued into secret web-tls")

	r = run(t, "cert", "list")
	mustContain(t, "list", r.stdout, "NAMESPACE", "web", "True", "ca (cluster)")
	r = run(t, "cert", "list", "-A", "-o", "wide")
	mustContain(t, "list wide", r.stdout, "DNS NAMES", "web.example.com,www.example.com")
	r = run(t, "cert", "list", "-n", "other")
	mustContain(t, "empty list", r.stdout, "No certificates in namespace other")
	r = run(t, "cert", "get", "web")
	mustContain(t, "get", r.stdout, "default/web", "ClusterIssuer/ca", "web.example.com, www.example.com")

	r = run(t, "cert", "inspect", "web")
	if r.err != nil {
		t.Fatal(r.err)
	}
	mustContain(t, "inspect", r.stdout, "CN=web", "(self-signed)", "Serial", "2A", "ECDSA 256")
	r = run(t, "cert", "inspect", "web", "-o", "json")
	mustContain(t, "inspect json", r.stdout, `"keyAlgorithm": "ECDSA"`)

	// tls.key is what makes it exportable; issuedSecret has only tls.crt.
	sec, err := c.Kube.CoreV1().Secrets("default").Get(t.Context(), "web-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sec.Data["tls.key"] = []byte("KEY\n")
	if _, err := c.Kube.CoreV1().Secrets("default").Update(t.Context(), sec, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	r = run(t, "cert", "export", "web", "--dir", dir)
	mustContain(t, "export", r.stdout, dir+"/web.crt", dir+"/web.key")
	if r = run(t, "cert", "export", "web", "--dir", dir); r.err == nil {
		t.Error("export overwrote files without --force")
	}
	r = run(t, "cert", "export", "web", "--stdout")
	mustContain(t, "export --stdout", r.stdout, "BEGIN CERTIFICATE", "KEY")
	if r = run(t, "cert", "export", "web", "--stdout", "--dir", dir); r.err == nil {
		t.Error("--stdout with --dir accepted")
	}

	r = run(t, "cert", "renew", "web", "--no-wait")
	mustContain(t, "renew", r.stdout, "renewal of default/web triggered")

	r = run(t, "cert", "delete", "web", "-A")
	if r.err == nil {
		t.Error("-A without --all-managed accepted")
	}
	root := kubetest.Obj("cert-manager.io/v1", "Certificate", "cert-manager", "ca-root-ca", map[string]any{"spec": map[string]any{"secretName": "ca-root-ca"}})
	root.SetLabels(map[string]string{kube.ManagedByLabel: kube.ManagedByValue, "cmo-cli/role": "ca-root"})
	if _, err := c.Dyn.Resource(kube.GVRCertificate).Namespace("cert-manager").Create(t.Context(), root, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	r = run(t, "cert", "delete", "--all-managed", "-A", "-y")
	mustContain(t, "delete", r.stdout, "default/Certificate/web deleted", "default/Secret/web-tls deleted")
	mustContain(t, "delete stderr", r.stderr, "Keeping 1 root certificate of generated CAs")
	if strings.Contains(r.stdout, "ca-root-ca") {
		t.Error("--all-managed deleted a generated CA's root")
	}
	r = run(t, "cert", "delete", "--all-managed", "-A", "-y")
	mustContain(t, "delete again", r.stdout, "No certificates made by cmo")
}

func TestDoctor(t *testing.T) {
	use(t, fakeCluster(t, false, nil))
	r := run(t, "doctor")
	if r.err != nil {
		t.Errorf("not installed is a warning, not a failure: %v", r.err)
	}
	mustContain(t, "doctor", r.stdout, "✓ cluster: kubernetes v1.34.2", "! install: cert-manager is not installed")

	notReady := kubetest.Obj("cert-manager.io/v1", "Certificate", "default", "web", map[string]any{"status": kubetest.Ready("False", "Issuing", "stuck")})
	use(t, fakeCluster(t, false, operandDeployments("v1.21.3"), crd(), notReady))
	r = run(t, "doctor", "-o", "json")
	if r.err != nil {
		t.Fatalf("doctor: %v\n%s", r.err, r.stdout)
	}
	var checks []check
	if err := json.Unmarshal([]byte(r.stdout), &checks); err != nil {
		t.Fatal(err)
	}
	byName := map[string]check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if byName["webhook"].Status != checkOK || byName["crds"].Status != checkOK {
		t.Errorf("checks = %+v", checks)
	}
	if c := byName["certificate default/web"]; c.Status != checkWarn || !strings.Contains(c.Detail, "stuck") {
		t.Errorf("certificate check = %+v", c)
	}

	// On OpenShift with no catalog, the catalog check fails the run.
	use(t, fakeCluster(t, true, nil))
	r = run(t, "doctor")
	if r.err == nil || !strings.Contains(r.stdout, "✗ catalog") {
		t.Errorf("missing catalog: %v\n%s", r.err, r.stdout)
	}
}

func TestConfirm(t *testing.T) {
	use(t, fakeCluster(t, false, nil))
	stdinIsTTY = func() bool { return true }
	cmd := certDeleteCmd
	cmd.SetIn(strings.NewReader("y\n"))
	cmd.SetErr(&strings.Builder{})
	assumeYes = false
	if err := confirm(cmd, "Go?"); err != nil {
		t.Errorf("y: %v", err)
	}
	cmd.SetIn(strings.NewReader("\n"))
	if err := confirm(cmd, "Go?"); err == nil {
		t.Error("empty answer confirmed")
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "30s", 5 * time.Minute: "5m", 3 * time.Hour: "3h", 72 * time.Hour: "3d",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %s, want %s", d, got, want)
		}
	}
	if until(time.Now().Add(-time.Hour)) != "expired" || age(time.Time{}) != "-" {
		t.Error("until/age edge cases")
	}
	if plural(1, "issuer") != "1 issuer" || plural(2, "issuer") != "2 issuers" {
		t.Error("plural")
	}
}

func TestUninstallPurgesLeftoverCRDs(t *testing.T) {
	// The state a failed OpenShift uninstall left on a real spoke: no operator,
	// but the operator's CRD still defined.
	left := kubetest.Obj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "certmanagers.operator.openshift.io",
		map[string]any{"spec": map[string]any{"group": "operator.openshift.io"}})
	left.SetLabels(map[string]string{"operators.coreos.com/openshift-cert-manager-operator.cert-manager-operator": ""})
	use(t, fakeCluster(t, true, nil, left))
	r := run(t, "status")
	mustContain(t, "status", r.stdout, "not installed", "Leftovers", "cmo uninstall --purge")
	r = run(t, "uninstall")
	if r.err != nil || !strings.Contains(r.stdout, "not installed; nothing to do") {
		t.Errorf("without --purge: %q, %v", r.stdout, r.err)
	}
	r = run(t, "uninstall", "--purge", "--yes", "--no-wait", "-o", "json")
	if r.err != nil {
		t.Fatalf("purge: %v\n%s", r.err, r.stderr)
	}
	mustContain(t, "purge", r.stdout, `"name": "certmanagers.operator.openshift.io"`, `"action": "deleted"`)
	r = run(t, "status")
	if strings.Contains(r.stdout, "Leftovers") {
		t.Errorf("leftovers after purge:\n%s", r.stdout)
	}
}

func TestStatusOLMShowsBothVersions(t *testing.T) {
	sub := kubetest.Obj("operators.coreos.com/v1alpha1", "Subscription", install.OperatorNamespace, install.OperatorPackage,
		map[string]any{"spec": map[string]any{"channel": "stable-v1"}, "status": map[string]any{"installedCSV": "cert-manager-operator.v1.20.1"}})
	csv := kubetest.Obj("operators.coreos.com/v1alpha1", "ClusterServiceVersion", install.OperatorNamespace, "cert-manager-operator.v1.20.1",
		map[string]any{"status": map[string]any{"phase": "Succeeded"}})
	cv := kubetest.Obj("config.openshift.io/v1", "ClusterVersion", "", "version",
		map[string]any{"status": map[string]any{"desired": map[string]any{"version": "4.22.1"}}})
	use(t, fakeCluster(t, true, operandDeployments("v1.20.4"), sub, csv, cv, crd()))
	r := run(t, "status")
	mustContain(t, "status", r.stdout, "openshift 4.22.1 (kubernetes v1.34.2), context fake",
		"ready  v1.20.4 via olm", "cert-manager-operator.v1.20.1  Succeeded  channel stable-v1")
	r = run(t, "doctor")
	mustContain(t, "doctor", r.stdout, "install: cert-manager v1.20.4 via olm (operator v1.20.1)")
	r = run(t, "uninstall")
	mustContain(t, "uninstall summary", r.stderr, "cert-manager v1.20.4 (olm, operator v1.20.1)")
}

func TestUnknownSubcommand(t *testing.T) {
	for _, args := range [][]string{{"cert", "frob"}, {"issuer", "create", "vault", "x"}} {
		if r := run(t, args...); r.err == nil || !strings.Contains(r.err.Error(), "unknown command") {
			t.Errorf("%v: %v", args, r.err)
		}
	}
	if r := run(t, "issuer"); r.err != nil || !strings.Contains(r.stdout, "Usage:") {
		t.Errorf("bare group: %v", r.err)
	}
}

func TestCertListExpiringWithin(t *testing.T) {
	soon := kubetest.Obj("cert-manager.io/v1", "Certificate", "default", "soon", map[string]any{
		"spec":   map[string]any{"secretName": "soon-tls", "issuerRef": map[string]any{"name": "ca"}},
		"status": map[string]any{"notAfter": time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)},
	})
	later := kubetest.Obj("cert-manager.io/v1", "Certificate", "default", "later", map[string]any{
		"spec":   map[string]any{"secretName": "later-tls", "issuerRef": map[string]any{"name": "ca"}},
		"status": map[string]any{"notAfter": time.Now().Add(60 * 24 * time.Hour).UTC().Format(time.RFC3339)},
	})
	use(t, fakeCluster(t, false, nil, soon, later))

	r := run(t, "cert", "list", "--expiring-within", "1d")
	if ExitCode(r.err) != ExitFound || !strings.Contains(r.err.Error(), "1 certificate expire") {
		t.Errorf("err = %v (exit %d), want exit %d", r.err, ExitCode(r.err), ExitFound)
	}
	mustContain(t, "expiring", r.stdout, "soon")
	if strings.Contains(r.stdout, "later") {
		t.Error("listed a certificate outside the window")
	}

	r = run(t, "cert", "list", "--expiring-within", "1h")
	if r.err != nil || !strings.Contains(r.stdout, "No certificates expire within 1h") {
		t.Errorf("nothing expiring: %q, %v", r.stdout, r.err)
	}
	r = run(t, "cert", "list", "--expiring-within", "1h", "-o", "json")
	if r.err != nil || strings.TrimSpace(r.stdout) != "[]" {
		t.Errorf("json, nothing expiring: %q, %v", r.stdout, r.err)
	}
	if r = run(t, "cert", "list", "--expiring-within", "soon"); ExitCode(r.err) != 1 {
		t.Errorf("bad window: exit %d", ExitCode(r.err))
	}
}

func TestExitCode(t *testing.T) {
	if ExitCode(nil) != 0 || ExitCode(errors.New("x")) != 1 {
		t.Error("plain exit codes")
	}
	wrapped := fmt.Errorf("context: %w", &ExitError{Code: ExitFound, Err: errors.New("found")})
	if ExitCode(wrapped) != ExitFound || wrapped.Error() != "context: found" {
		t.Errorf("wrapped ExitError: %d %q", ExitCode(wrapped), wrapped.Error())
	}
}
