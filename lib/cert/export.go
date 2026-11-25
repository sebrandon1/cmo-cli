package cert

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
)

// ExportFile is one file Export writes.
type ExportFile struct {
	Path string `json:"path"`
	// Key is the Secret key the file holds: tls.crt, tls.key or ca.crt.
	Key  string `json:"key"`
	mode os.FileMode
}

// exportKeys are the Secret keys Export writes, with each file's suffix and
// mode. The private key is readable by its owner only.
var exportKeys = []struct {
	key, suffix string
	mode        os.FileMode
}{
	{corev1.TLSCertKey, ".crt", 0o644},
	{corev1.TLSPrivateKeyKey, ".key", 0o600},
	{"ca.crt", "-ca.crt", 0o644},
}

// Export writes the key pair in s to dir as NAME.crt, NAME.key and, when the
// Secret has one, NAME-ca.crt. It refuses to overwrite a file unless force,
// and writes nothing when any file is in the way.
func Export(s *corev1.Secret, name, dir string, force bool) ([]ExportFile, error) {
	if len(s.Data[corev1.TLSCertKey]) == 0 || len(s.Data[corev1.TLSPrivateKeyKey]) == 0 {
		return nil, fmt.Errorf("secret %s has no tls.crt and tls.key to export", s.Name)
	}
	var files []ExportFile
	for _, k := range exportKeys {
		if len(s.Data[k.key]) == 0 {
			continue
		}
		p := filepath.Join(dir, name+k.suffix)
		if _, err := os.Stat(p); err == nil && !force {
			return nil, fmt.Errorf("%s exists; pass --force to overwrite it", p)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		files = append(files, ExportFile{Path: p, Key: k.key, mode: k.mode})
	}
	for i, f := range files {
		// Write then chmod: WriteFile keeps an existing file's mode, and a key
		// overwritten with --force must still end up 0600.
		if err := os.WriteFile(f.Path, s.Data[f.Key], f.mode); err != nil { // #nosec G306 -- certificates are public; the key is 0600
			return files[:i], fmt.Errorf("writing %s: %w", f.Path, err)
		}
		if err := os.Chmod(f.Path, f.mode); err != nil {
			return files[:i+1], fmt.Errorf("setting the mode of %s: %w", f.Path, err)
		}
	}
	return files, nil
}

// Bundle is the certificate chain followed by the private key, as one PEM
// stream, for --stdout.
func Bundle(s *corev1.Secret) ([]byte, error) {
	crt, key := s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey]
	if len(crt) == 0 || len(key) == 0 {
		return nil, fmt.Errorf("secret %s has no tls.crt and tls.key to export", s.Name)
	}
	out := append([]byte{}, crt...)
	if out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, key...), nil
}
