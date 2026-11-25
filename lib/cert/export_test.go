package cert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func tlsSecret(withCA bool) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "web-tls"}, Data: map[string][]byte{
		corev1.TLSCertKey: []byte("CERT"), corev1.TLSPrivateKeyKey: []byte("KEY\n"),
	}}
	if withCA {
		s.Data["ca.crt"] = []byte("CA")
	}
	return s
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestExport(t *testing.T) {
	dir := t.TempDir()
	files, err := Export(tlsSecret(true), "web", dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("files = %v", files)
	}
	for name, want := range map[string]string{"web.crt": "CERT", "web.key": "KEY\n", "web-ca.crt": "CA"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", name, b, err)
		}
	}
	if m := mode(t, filepath.Join(dir, "web.key")); m != 0o600 {
		t.Errorf("key mode = %o, want 600", m)
	}

	if _, err := Export(tlsSecret(true), "web", dir, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("second export: %v", err)
	}
	// --force overwrites, and a key that was loosened goes back to 0600.
	if err := os.Chmod(filepath.Join(dir, "web.key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(tlsSecret(true), "web", dir, true); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, filepath.Join(dir, "web.key")); m != 0o600 {
		t.Errorf("key mode after --force = %o, want 600", m)
	}
}

func TestExportWithoutCA(t *testing.T) {
	dir := t.TempDir()
	files, err := Export(tlsSecret(false), "web", dir, false)
	if err != nil || len(files) != 2 {
		t.Fatalf("files = %v, %v", files, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "web-ca.crt")); !os.IsNotExist(err) {
		t.Error("wrote a CA file with no ca.crt")
	}
}

func TestExportRefusesHalfIssued(t *testing.T) {
	s := tlsSecret(false)
	delete(s.Data, corev1.TLSPrivateKeyKey)
	if _, err := Export(s, "web", t.TempDir(), false); err == nil {
		t.Error("exported a secret with no key")
	}
	if _, err := Bundle(s); err == nil {
		t.Error("bundled a secret with no key")
	}
	if _, err := Export(tlsSecret(false), "web", filepath.Join(t.TempDir(), "missing"), false); err == nil {
		t.Error("wrote into a directory that doesn't exist")
	}
}

func TestBundle(t *testing.T) {
	b, err := Bundle(tlsSecret(false))
	if err != nil || string(b) != "CERT\nKEY\n" {
		t.Errorf("Bundle = %q, %v", b, err)
	}
}
