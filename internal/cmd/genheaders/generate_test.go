package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatch(t *testing.T) {
	f := filepath.Join(t.TempDir(), "h.h")
	if err := os.WriteFile(f, []byte("a\n#if X && P\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := patch(f, "#if X && P", "#if 0"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f); string(b) != "a\n#if 0\nb\n" {
		t.Errorf("patched %q", b)
	}
	if err := patch(f, "#if X && P", "#if 0"); err == nil || !strings.Contains(err.Error(), "occurs 0 times") {
		t.Errorf("a vanished pattern: %v, want an error", err)
	}
}

// TestPatchesApplied checks the committed tree carries every patch, i.e. it
// was regenerated after clangPatches last changed.
func TestPatchesApplied(t *testing.T) {
	for name, p := range clangPatches {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "headers", "include", "clang", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), p[0]) || !strings.Contains(string(b), p[1]) {
			t.Errorf("headers/include/clang/%s is not patched; run genheaders", name)
		}
	}
}
