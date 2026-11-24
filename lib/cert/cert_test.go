package cert

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/sebrandon1/cmo-cli/lib/apply"
	"github.com/sebrandon1/cmo-cli/lib/kube"
	"github.com/sebrandon1/cmo-cli/lib/kube/kubetest"
	"github.com/sebrandon1/cmo-cli/lib/wait"
)

func valid() *Options {
	return &Options{Name: "web", Namespace: "default", Issuer: IssuerRef{Name: "ca", Kind: "ClusterIssuer"}, DNSNames: []string{"web.example.com"}}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		mod  func(o *Options)
		want string
	}{
		{"valid", func(*Options) {}, ""},
		{"no name", func(o *Options) { o.Name = "" }, "needs a name"},
		{"no issuer", func(o *Options) { o.Issuer.Name = "" }, "--issuer is required"},
		{"no subject", func(o *Options) { o.DNSNames = nil }, "at least one of"},
		{"common name only", func(o *Options) { o.DNSNames, o.CommonName = nil, "client" }, ""},
		{"bad ip", func(o *Options) { o.IPs = []string{"10.0.0"} }, "not an IP"},
		{"bad uri", func(o *Options) { o.URIs = []string{"no-scheme"} }, "absolute URI"},
		{"bad algorithm", func(o *Options) { o.KeyAlgorithm = "dsa" }, "choose rsa"},
		{"bad rsa size", func(o *Options) { o.KeyAlgorithm, o.KeySize = "rsa", 1024 }, "not valid for rsa"},
		{"good ecdsa size", func(o *Options) { o.KeyAlgorithm, o.KeySize = "ECDSA", 384 }, ""},
		{"ed25519 size", func(o *Options) { o.KeyAlgorithm, o.KeySize = "ed25519", 256 }, "does not apply"},
		{"size alone", func(o *Options) { o.KeySize = 2048 }, "needs --key-algorithm"},
		{"short duration", func(o *Options) { o.Duration = time.Minute }, "at least 1h"},
		{"renew too late", func(o *Options) { o.Duration, o.RenewBefore = 2*time.Hour, 2*time.Hour }, "shorter than"},
	}
	for _, tt := range tests {
		o := valid()
		tt.mod(o)
		err := o.Validate()
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestBuild(t *testing.T) {
	o := valid()
	o.IPs = []string{"10.0.0.1"}
	o.Duration, o.RenewBefore = 48*time.Hour, 24*time.Hour
	o.KeyAlgorithm, o.KeySize = "ecdsa", 256
	o.Labels = map[string]string{"team": "a"}
	u, err := Build(o)
	if err != nil {
		t.Fatal(err)
	}
	if u.GetKind() != "Certificate" || u.GetAPIVersion() != "cert-manager.io/v1" {
		t.Errorf("type = %s %s", u.GetAPIVersion(), u.GetKind())
	}
	if l := u.GetLabels(); l[kube.ManagedByLabel] != kube.ManagedByValue || l["team"] != "a" {
		t.Errorf("labels = %v", l)
	}
	checks := map[string][]string{
		"web-tls":         {"spec", "secretName"},
		"ClusterIssuer":   {"spec", "issuerRef", "kind"},
		"cert-manager.io": {"spec", "issuerRef", "group"},
		"48h0m0s":         {"spec", "duration"},
		"ECDSA":           {"spec", "privateKey", "algorithm"},
		"Always":          {"spec", "privateKey", "rotationPolicy"},
	}
	for want, path := range checks {
		if got, _, _ := unstructured.NestedString(u.Object, path...); got != want {
			t.Errorf("%v = %q, want %q", path, got, want)
		}
	}
	if _, ok := u.Object["status"]; ok {
		t.Error("built object carries a status")
	}
	if _, err := Build(&Options{}); err == nil {
		t.Error("Build accepted empty options")
	}
}

func certObj(ns, name string, status map[string]any) *unstructured.Unstructured {
	return kubetest.Obj("cert-manager.io/v1", "Certificate", ns, name, map[string]any{
		"spec":   map[string]any{"secretName": name + "-tls", "dnsNames": []any{name + ".example.com"}, "issuerRef": map[string]any{"name": "ca", "kind": "ClusterIssuer"}},
		"status": status,
	})
}

func readyStatus(rev int64) map[string]any {
	s := kubetest.Ready("True", "Ready", "issued")
	s["revision"] = rev
	s["notAfter"] = "2030-01-02T03:04:05Z"
	return s
}

func request(c *unstructured.Unstructured, name string, rev string, status map[string]any) *unstructured.Unstructured {
	r := kubetest.Obj("cert-manager.io/v1", "CertificateRequest", c.GetNamespace(), name, map[string]any{"status": status})
	r.SetAnnotations(map[string]string{revisionAnnotation: rev})
	return kubetest.Owned(r, c)
}

func TestSummarizeAndList(t *testing.T) {
	ctx := context.Background()
	c := kubetest.Clients(nil,
		certObj("b", "zeta", readyStatus(1)),
		certObj("a", "web", kubetest.Ready("False", "Issuing", "waiting")),
		certObj("a", "api", readyStatus(2)),
	)
	all, err := List(ctx, c.Dyn, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range all {
		names = append(names, s.Namespace+"/"+s.Name)
	}
	if !slices.Equal(names, []string{"a/api", "a/web", "b/zeta"}) {
		t.Errorf("order = %v", names)
	}
	api := all[0]
	if api.Ready != "True" || api.Secret != "api-tls" || api.Issuer != "ca" || api.IssuerKind != "ClusterIssuer" || api.NotAfter.Year() != 2030 {
		t.Errorf("summary = %+v", api)
	}
	if all[1].Reason != "Issuing" {
		t.Errorf("not-ready reason = %q", all[1].Reason)
	}
	inA, err := List(ctx, c.Dyn, "a", "")
	if err != nil || len(inA) != 2 {
		t.Errorf("List(a) = %d, %v", len(inA), err)
	}
}

func TestCreateAndGet(t *testing.T) {
	ctx := context.Background()
	c := kubetest.Clients(nil)
	u, err := Build(valid())
	if err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, c.Dyn, u); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, c.Dyn, u); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second Create: %v", err)
	}
	if _, err := Get(ctx, c.Dyn, "default", "web"); err != nil {
		t.Error(err)
	}
	_, err = Get(ctx, c.Dyn, "default", "nope")
	if err == nil || !apierrors.IsNotFound(err) || !strings.Contains(err.Error(), "cmo cert list") {
		t.Errorf("missing: %v", err)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: "default"}}
	c := kubetest.Clients([]runtime.Object{secret}, certObj("default", "web", readyStatus(1)))
	ch, err := Delete(ctx, c.Dyn, c.Kube, "default", "web", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 2 || ch[1].Kind != "Secret" || ch[1].Action != apply.Removed {
		t.Errorf("changes = %v", ch)
	}
	if _, err := c.Kube.CoreV1().Secrets("default").Get(ctx, "web-tls", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("secret survived")
	}
	c = kubetest.Clients([]runtime.Object{secret.DeepCopy()}, certObj("default", "web", readyStatus(1)))
	if ch, err := Delete(ctx, c.Dyn, c.Kube, "default", "web", true); err != nil || len(ch) != 1 {
		t.Errorf("keep secret: %v, %v", ch, err)
	}
	if _, err := Delete(ctx, c.Dyn, c.Kube, "default", "web", false); err == nil {
		t.Error("deleting a missing certificate succeeded")
	}
}

func TestWaitReady(t *testing.T) {
	ctx := context.Background()
	ready := certObj("default", "web", readyStatus(2))
	c := kubetest.Clients(nil, ready)
	if err := WaitReady(ctx, c.Dyn, "default", "web", 2, time.Second, nil); err != nil {
		t.Errorf("ready: %v", err)
	}
	// Ready, but at an older revision than a renewal waits for.
	if err := WaitReady(ctx, c.Dyn, "default", "web", 3, 50*time.Millisecond, nil); err == nil || !strings.Contains(err.Error(), "not ready after") {
		t.Errorf("old revision: %v", err)
	}

	failing := certObj("default", "bad", kubetest.Ready("False", "Issuing", "issuing"))
	cr := request(failing, "bad-1", "1", kubetest.Ready("False", "Failed", "the CA said no"))
	c = kubetest.Clients(nil, failing, cr)
	err := WaitReady(ctx, c.Dyn, "default", "bad", 1, time.Second, nil)
	if !errors.Is(err, ErrFailed) || !strings.Contains(err.Error(), "the CA said no") || !strings.Contains(err.Error(), "request bad-1 Ready=False") {
		t.Errorf("failed request: %v", err)
	}

	denied := certObj("default", "denied", kubetest.Ready("False", "Issuing", ""))
	dcr := request(denied, "denied-1", "1", map[string]any{"conditions": []any{
		map[string]any{"type": "Denied", "status": "True", "message": "policy"},
	}})
	c = kubetest.Clients(nil, denied, dcr)
	if err := WaitReady(ctx, c.Dyn, "default", "denied", 1, time.Second, nil); !errors.Is(err, ErrFailed) || !strings.Contains(err.Error(), "denied") {
		t.Errorf("denied: %v", err)
	}
}

func TestDiagnoseACME(t *testing.T) {
	ctx := context.Background()
	c0 := certObj("default", "web", kubetest.Ready("False", "Issuing", "waiting"))
	cr := request(c0, "web-1", "1", kubetest.Ready("False", "Pending", "waiting on order"))
	order := kubetest.Owned(kubetest.Obj("acme.cert-manager.io/v1", "Order", "default", "web-1-123", map[string]any{
		"status": map[string]any{"state": "pending"},
	}), cr)
	ch := kubetest.Owned(kubetest.Obj("acme.cert-manager.io/v1", "Challenge", "default", "web-1-123-0", map[string]any{
		"spec":   map[string]any{"type": "HTTP-01", "dnsName": "web.example.com"},
		"status": map[string]any{"state": "pending", "reason": "self check failed"},
	}), order)
	stranger := kubetest.Obj("acme.cert-manager.io/v1", "Order", "default", "other", nil)
	c := kubetest.Clients(nil, c0, cr, order, ch, stranger)
	got := strings.Join(Diagnose(ctx, c.Dyn, "default", "web"), "\n")
	for _, want := range []string{"certificate Ready=False Issuing: waiting", "request web-1 Ready=False Pending", "order web-1-123 state=pending", "challenge web-1-123-0 HTTP-01 for web.example.com state=pending self check failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("Diagnose lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "other") {
		t.Error("Diagnose reported an order the certificate doesn't own")
	}
	if got := Diagnose(ctx, c.Dyn, "default", "nope"); len(got) != 1 || !strings.Contains(got[0], "not found") {
		t.Errorf("missing cert: %v", got)
	}
}

func TestRenew(t *testing.T) {
	ctx := context.Background()
	c := kubetest.Clients(nil, certObj("default", "web", readyStatus(4)))
	rev, err := Renew(ctx, c.Dyn, "default", "web")
	if err != nil || rev != 4 {
		t.Fatalf("Renew = %d, %v", rev, err)
	}
	u, err := Get(ctx, c.Dyn, "default", "web")
	if err != nil {
		t.Fatal(err)
	}
	if cond, ok := wait.GetCondition(u, "Issuing"); !ok || cond.Status != "True" || cond.Reason != "ManuallyTriggered" {
		t.Errorf("Issuing = %+v, %v", cond, ok)
	}
	if _, err := Renew(ctx, c.Dyn, "default", "web"); err == nil || !strings.Contains(err.Error(), "already being issued") {
		t.Errorf("second Renew: %v", err)
	}
}

func TestReadSecret(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: "default"}, Data: map[string][]byte{"tls.crt": []byte("x")}}
	c := kubetest.Clients([]runtime.Object{secret}, certObj("default", "web", readyStatus(1)), certObj("default", "new", nil))
	if s, err := ReadSecret(ctx, c.Dyn, c.Kube, "default", "web"); err != nil || string(s.Data["tls.crt"]) != "x" {
		t.Errorf("ReadSecret = %v, %v", s, err)
	}
	if _, err := ReadSecret(ctx, c.Dyn, c.Kube, "default", "new"); err == nil || !strings.Contains(err.Error(), "not been issued") {
		t.Errorf("unissued: %v", err)
	}
}

// selfSigned makes a PEM certificate for key, signed by itself.
func selfSigned(t *testing.T, pub, priv any) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(0xABCDEF),
		Subject:      pkix.Name{CommonName: "web"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"web.example.com"},
		IPAddresses:  []net.IP{net.ParseIP("10.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestInspect(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	tests := []struct {
		name string
		pem  []byte
		alg  string
		size int
	}{
		{"ecdsa", selfSigned(t, &ec.PublicKey, ec), "ECDSA", 384},
		{"rsa", selfSigned(t, &rk.PublicKey, rk), "RSA", 2048},
		{"ed25519", selfSigned(t, edPub, edPriv), "Ed25519", 0},
	}
	for _, tt := range tests {
		chain := append([]byte("junk\n"), tt.pem...)
		chain = append(chain, tt.pem...)
		info, err := Inspect(chain, []byte("ca"))
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if info.KeyAlgorithm != tt.alg || info.KeySize != tt.size {
			t.Errorf("%s: key = %s %d", tt.name, info.KeyAlgorithm, info.KeySize)
		}
		if info.Subject != "CN=web" || info.Serial != "ABCDEF" || !info.SelfSigned || info.ChainLength != 2 || !info.HasCA {
			t.Errorf("%s: info = %+v", tt.name, info)
		}
		if !slices.Equal(info.DNSNames, []string{"web.example.com"}) || !slices.Equal(info.IPAddresses, []string{"10.0.0.1"}) {
			t.Errorf("%s: SANs = %v %v", tt.name, info.DNSNames, info.IPAddresses)
		}
	}
	if _, err := Inspect([]byte("not pem"), nil); err == nil {
		t.Error("Inspect accepted junk")
	}
	bad := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("nope")})
	if _, err := Inspect(bad, nil); err == nil {
		t.Error("Inspect accepted a corrupt certificate")
	}
}
