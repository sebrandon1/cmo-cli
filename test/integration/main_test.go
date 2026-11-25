package integration

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The two mains must stay identical, apart from cmd/cmo's doc comment, or
// go install and make build ship different programs.
func TestMainsMatch(t *testing.T) {
	root, err := os.ReadFile(filepath.Join(repoRoot(), "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	sub, err := os.ReadFile(filepath.Join(repoRoot(), "cmd", "cmo", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if i := bytes.Index(sub, []byte("package main")); i >= 0 {
		sub = sub[i:]
	}
	if !bytes.Equal(root, sub) {
		t.Error("cmd/cmo/main.go differs from main.go; keep them in step")
	}
}
