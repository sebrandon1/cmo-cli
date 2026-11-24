// Package integration runs the built cmo binary as a child process and
// asserts on what it does as a program: its exit code, its stdout and its
// stderr.
//
// The unit tests in cmd and lib drive the commands in process, against fakes.
// These cover what only a real process has: that -v traces to stderr and
// leaves stdout alone, that -o json stays parseable with tracing on, and that
// a refusal is an error on stderr with nothing on stdout. No cluster is
// needed: a fake API server stands in for one.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var (
	// cmo is the binary under test, built once.
	cmo string
	// home is an empty directory, so the CLI never reads a real ~/.kube/config.
	home string
)

func TestMain(m *testing.M) {
	dir, code := build()
	if code == 0 {
		code = m.Run()
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func build() (string, int) {
	dir, err := os.MkdirTemp("", "cmo-integration-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: temp dir: %v\n", err)
		return "", 1
	}
	cmo = filepath.Join(dir, "cmo")
	home = filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "integration: home dir: %v\n", err)
		return dir, 1
	}
	c := exec.CommandContext(context.Background(), "go", "build", "-ldflags", "-X main.version=v0.0.0-test", "-o", cmo, ".")
	c.Dir = repoRoot()
	if out, err := c.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "integration: go build: %v\n%s", err, out)
		return dir, 1
	}
	return dir, 0
}

// repoRoot is the directory holding go.mod, two levels above this file.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

type result struct {
	stdout, stderr string
	code           int
}

// run runs the binary with a clean environment: no inherited kubeconfig, no
// color and no real home.
func run(t *testing.T, kubeconfig string, args ...string) result {
	t.Helper()
	if kubeconfig == "" {
		kubeconfig = filepath.Join(t.TempDir(), "no-such-kubeconfig")
	}
	c := exec.CommandContext(t.Context(), cmo, args...)
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "NO_COLOR=1", "KUBECONFIG=" + kubeconfig}
	var stdout, stderr strings.Builder
	c.Stdout, c.Stderr = &stdout, &stderr
	code := 0
	if err := c.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running cmo %s: %v", strings.Join(args, " "), err)
		}
		code = exit.ExitCode()
	}
	return result{stdout.String(), stderr.String(), code}
}

const notFound = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","message":"not found","code":404}`

// fakeAPI is a Kubernetes API server with nothing installed: discovery
// answers, and every object is not found.
func fakeAPI(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			_, _ = io.WriteString(w, `{"major":"1","minor":"34","gitVersion":"v1.34.2"}`)
		case "/api":
			_, _ = io.WriteString(w, `{"kind":"APIVersions","versions":["v1"]}`)
		case "/apis":
			_, _ = io.WriteString(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}}]}`)
		case "/apis/cert-manager.io/v1/certificates":
			// One certificate, expiring in an hour.
			notAfter := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			_, _ = io.WriteString(w, `{"apiVersion":"cert-manager.io/v1","kind":"CertificateList","items":[`+
				`{"apiVersion":"cert-manager.io/v1","kind":"Certificate","metadata":{"name":"soon","namespace":"default"},`+
				`"spec":{"secretName":"soon-tls","issuerRef":{"name":"ca"}},"status":{"notAfter":"`+notAfter+`"}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, notFound)
		}
	}))
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	cfg := "apiVersion: v1\nkind: Config\n" +
		"clusters:\n- name: fake\n  cluster:\n    server: " + srv.URL + "\n" +
		"contexts:\n- name: fake\n  context:\n    cluster: fake\n    user: fake\n" +
		"current-context: fake\nusers:\n- name: fake\n  user: {}\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHelp(t *testing.T) {
	r := run(t, "", "--help")
	if r.code != 0 || r.stderr != "" {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	for _, want := range []string{"cmo install", "cmo cert create", "-v, --verbose", "-o, --output"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

func TestVersion(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"version"}} {
		r := run(t, "", args...)
		if r.code != 0 || r.stdout != "cmo version v0.0.0-test\n" {
			t.Errorf("%v: exit %d, stdout %q", args, r.code, r.stdout)
		}
	}
}

func TestRefusalsGoToStderr(t *testing.T) {
	for _, args := range [][]string{
		{"status", "-o", "xml"},
		{"status", "-o", "wide"},
		{"cert", "create", "web"}, // --issuer is required
		{"issuer", "create", "nope", "x"},
	} {
		r := run(t, "", args...)
		if r.code == 0 {
			t.Errorf("%v: exit 0", args)
		}
		if !strings.Contains(r.stderr, "Error") {
			t.Errorf("%v: stderr %q has no error", args, r.stderr)
		}
		if strings.Contains(r.stdout, "Error") {
			t.Errorf("%v: the error went to stdout", args)
		}
	}
}

func TestNoCluster(t *testing.T) {
	r := run(t, "", "status")
	if r.code == 0 || r.stdout != "" {
		t.Errorf("exit %d, stdout %q", r.code, r.stdout)
	}
}

func TestStatusAgainstFakeAPI(t *testing.T) {
	kc := fakeAPI(t)
	r := run(t, kc, "status")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "not installed") || !strings.Contains(r.stdout, "kubernetes v1.34.2") {
		t.Errorf("stdout = %q", r.stdout)
	}

	// -v traces to stderr; stdout stays parseable JSON.
	r = run(t, kc, "status", "-o", "json", "-v")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var st struct {
		Installed bool   `json:"installed"`
		Platform  string `json:"platform"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, r.stdout)
	}
	if st.Installed || st.Platform != "kubernetes" {
		t.Errorf("status = %+v", st)
	}
	if !strings.Contains(r.stderr, "→ get deployments.apps cert-manager -n cert-manager") {
		t.Errorf("trace missing from stderr:\n%s", r.stderr)
	}
}

func TestDoctorAgainstFakeAPI(t *testing.T) {
	r := run(t, fakeAPI(t), "doctor")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "! install: cert-manager is not installed") {
		t.Errorf("stdout = %q", r.stdout)
	}
}

func TestUninstallNothingInstalled(t *testing.T) {
	r := run(t, fakeAPI(t), "uninstall")
	if r.code != 0 || !strings.Contains(r.stdout, "not installed") {
		t.Errorf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

// --expiring-within exits 3 when it finds something, so a script can tell
// "found" from "failed" (1).
func TestExpiringWithinExitCode(t *testing.T) {
	kc := fakeAPI(t)
	r := run(t, kc, "cert", "list", "-A", "--expiring-within", "1d")
	if r.code != 3 || !strings.Contains(r.stdout, "soon") {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 3", r.code, r.stdout, r.stderr)
	}
	if r = run(t, kc, "cert", "list", "-A", "--expiring-within", "30m"); r.code != 0 {
		t.Errorf("nothing expiring: exit %d, stderr %q", r.code, r.stderr)
	}
}
