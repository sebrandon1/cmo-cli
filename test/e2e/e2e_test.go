//go:build e2e

// Package e2e drives the built cmo binary through the whole lifecycle against
// the cluster $KUBECONFIG points at: install, a generated CA, a certificate,
// inspect, renew, cleanup and uninstall. It runs in CI on Kind (quick-k8s)
// and OpenShift (quick-ocp); run it locally with make test-e2e.
//
// CMO_E2E_INSTALL_ARGS adds arguments to cmo install, such as
// "--version v1.18.0".
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var cmo string

func TestMain(m *testing.M) {
	if os.Getenv("KUBECONFIG") == "" {
		fmt.Fprintln(os.Stderr, "e2e: set KUBECONFIG to the cluster to test against")
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "cmo-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cmo = filepath.Join(dir, "cmo")
	_, file, _, _ := runtime.Caller(0)
	build := exec.CommandContext(context.Background(), "go", "build", "-o", cmo, ".")
	build.Dir = filepath.Join(filepath.Dir(file), "..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: go build: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// cmoRun runs cmo with --no-color and returns stdout; it fails the test on a
// non-zero exit.
func cmoRun(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, err := cmoTry(t, args...)
	if err != nil {
		t.Fatalf("cmo %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, out, errOut)
	}
	return out
}

func cmoTry(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	c := exec.CommandContext(t.Context(), cmo, append(args, "--no-color")...)
	var stdout, stderr strings.Builder
	c.Stdout, c.Stderr = &stdout, &stderr
	t.Logf("$ cmo %s", strings.Join(args, " "))
	err := c.Run()
	if s := stderr.String(); s != "" {
		t.Log(strings.TrimSpace(s))
	}
	return stdout.String(), stderr.String(), err
}

func cmoJSON(t *testing.T, v any, args ...string) {
	t.Helper()
	out := cmoRun(t, append(args, "-o", "json")...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("cmo %s: output is not JSON: %v\n%s", strings.Join(args, " "), err, out)
	}
}

type status struct {
	Installed bool   `json:"installed"`
	Ready     bool   `json:"ready"`
	Method    string `json:"method"`
	Platform  string `json:"platform"`
	Version   string `json:"version"`
}

type inspect struct {
	Serial   string   `json:"serial"`
	DNSNames []string `json:"dnsNames"`
	Issuer   string   `json:"issuer"`
	IsCA     bool     `json:"isCA"`
}

const ns = "cmo-e2e"

func TestLifecycle(t *testing.T) {
	// Steps depend on the ones before; stop at the first failure.
	steps := []struct {
		name string
		f    func(t *testing.T)
	}{
		{"install", testInstall},
		{"install again is a no-op", testReinstall},
		{"doctor", func(t *testing.T) { cmoRun(t, "doctor") }},
		{"generated CA", testCA},
		{"certificate", testCertificate},
		{"renew", testRenew},
		{"namespaced issuer", testNamespacedIssuer},
		{"cleanup", testCleanup},
		{"uninstall", testUninstall},
	}
	createNamespace(t)
	for _, s := range steps {
		if !t.Run(s.name, s.f) {
			t.Fatalf("step %q failed; skipping the rest", s.name)
		}
	}
}

func installArgs() []string {
	args := []string{"install", "--timeout", "20m"}
	if extra := strings.Fields(os.Getenv("CMO_E2E_INSTALL_ARGS")); len(extra) > 0 {
		args = append(args, extra...)
	}
	return args
}

func testInstall(t *testing.T) {
	cmoRun(t, installArgs()...)
	var st status
	cmoJSON(t, &st, "status")
	if !st.Installed || !st.Ready {
		t.Fatalf("after install: %+v", st)
	}
	want := "manifest"
	if st.Platform == "openshift" {
		want = "olm"
	}
	if st.Method != want {
		t.Errorf("method = %s on %s, want %s", st.Method, st.Platform, want)
	}
}

func testReinstall(t *testing.T) {
	cmoRun(t, installArgs()...)
}

func testCA(t *testing.T) {
	cmoRun(t, "issuer", "create", "ca", "e2e-ca", "--generate", "--common-name", "cmo e2e root")
	out := cmoRun(t, "issuer", "list")
	for _, want := range []string{"e2e-ca", "e2e-ca-selfsigned"} {
		if !strings.Contains(out, want) {
			t.Errorf("issuer list lacks %s:\n%s", want, out)
		}
	}
}

func testCertificate(t *testing.T) {
	if _, _, err := cmoTry(t, "cert", "create", "web", "--issuer", "e2e-ca", "--dns", "web.example.com", "-n", "does-not-exist"); err == nil {
		t.Error("creating a certificate in a missing namespace succeeded")
	}
	cmoRun(t, "cert", "create", "web", "-n", ns, "--issuer", "e2e-ca",
		"--dns", "web.example.com,www.example.com", "--ip", "10.0.0.10", "--key-algorithm", "ecdsa", "--key-size", "384")
	var info inspect
	cmoJSON(t, &info, "cert", "inspect", "web", "-n", ns)
	if strings.Join(info.DNSNames, ",") != "web.example.com,www.example.com" {
		t.Errorf("SANs = %v", info.DNSNames)
	}
	if !strings.Contains(info.Issuer, "cmo e2e root") || info.IsCA {
		t.Errorf("inspect = %+v", info)
	}
	out := cmoRun(t, "cert", "list", "-n", ns)
	if !strings.Contains(out, "True") {
		t.Errorf("cert list:\n%s", out)
	}
}

func testRenew(t *testing.T) {
	var before, after inspect
	cmoJSON(t, &before, "cert", "inspect", "web", "-n", ns)
	cmoRun(t, "cert", "renew", "web", "-n", ns)
	cmoJSON(t, &after, "cert", "inspect", "web", "-n", ns)
	if before.Serial == after.Serial {
		t.Errorf("serial unchanged after renew: %s", after.Serial)
	}
}

func testNamespacedIssuer(t *testing.T) {
	cmoRun(t, "issuer", "create", "selfsigned", "local", "-n", ns)
	cmoRun(t, "cert", "create", "self", "-n", ns, "--issuer", "local", "--common-name", "self", "--key-algorithm", "ed25519")
	out := cmoRun(t, "cert", "inspect", "self", "-n", ns)
	if !strings.Contains(out, "self-signed") || !strings.Contains(out, "Ed25519") {
		t.Errorf("inspect:\n%s", out)
	}
}

func testCleanup(t *testing.T) {
	cmoRun(t, "cert", "delete", "--all-managed", "-A", "--yes")
	cmoRun(t, "issuer", "delete", "--all-managed", "--yes")
	var certs []any
	cmoJSON(t, &certs, "cert", "list", "-A", "--managed")
	if len(certs) != 0 {
		t.Errorf("left behind %d certificates", len(certs))
	}
	var issuers []struct {
		Name    string `json:"name"`
		Managed bool   `json:"managed"`
	}
	cmoJSON(t, &issuers, "issuer", "list")
	for _, i := range issuers {
		if i.Managed {
			t.Errorf("left behind issuer %s", i.Name)
		}
	}
}

func testUninstall(t *testing.T) {
	if _, _, err := cmoTry(t, "uninstall"); err == nil {
		t.Error("uninstall without --yes and without a terminal went ahead")
	}
	cmoRun(t, "uninstall", "--purge", "--yes", "--timeout", "10m")
	var st status
	cmoJSON(t, &st, "status")
	if st.Installed {
		t.Errorf("still installed: %+v", st)
	}
}

// createNamespace makes the test namespace with kubectl or oc, and removes it
// at the end.
func createNamespace(t *testing.T) {
	t.Helper()
	cli := "kubectl"
	if _, err := exec.LookPath(cli); err != nil {
		cli = "oc"
	}
	out, err := exec.CommandContext(t.Context(), cli, "create", "namespace", ns).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "AlreadyExists") {
		t.Fatalf("%s create namespace: %v\n%s", cli, err, out)
	}
	t.Cleanup(func() {
		err := exec.CommandContext(context.Background(), cli, "delete", "namespace", ns, "--wait=false").Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Logf("deleting namespace: %v", err)
		}
	})
}
