package main

import (
	"bytes"
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeModule writes a module directory that holds every patch anchor once,
// inside a comment so that the patched files still format.
func fakeModule(t *testing.T, drop string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "v4@"+Version)
	files := map[string]string{
		"go.mod":       "module modernc.org/cc/v4\n",
		"LICENSE":      "BSD\n",
		"doc_test.go":  "package cc\n",
		"cc.go":        "package cc\n\n/*\n",
		"cpp.go":       "package cc\n\n/*\n",
		"scanner.go":   "package cc\n\n/*\n",
		"ignored.txt":  "",
		"sub/other.go": "package sub\n",
	}
	for _, p := range patches {
		if p.Old == drop {
			continue
		}
		files[p.File] += p.Old
	}
	for _, n := range []string{"cc.go", "cpp.go", "scanner.go"} {
		files[n] += "*/\n"
	}
	for n, s := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, n)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGenerateRefusesAMissedPatch(t *testing.T) {
	for _, p := range patches {
		err := Generate(filepath.Join(t.TempDir(), "out"), fakeModule(t, p.Old))
		if err == nil || !strings.Contains(err.Error(), "occurs 0 times") {
			t.Errorf("patch %q (%s) missing: %v", p.Old, p.Why, err)
		}
	}
}

func TestGenerateRefusesAnotherModule(t *testing.T) {
	if err := Generate(t.TempDir(), t.TempDir()); err == nil {
		t.Error("a directory not named v4@" + Version + " was accepted")
	}
	dir := fakeModule(t, "")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(t.TempDir(), dir); err == nil {
		t.Error("a module that is not cc/v4 was accepted")
	}
}

// TestInternalCCIsGenerated: internal/cc is exactly what vendorcc makes from
// the pinned cc/v4 in the module cache. CI downloads it first.
func TestInternalCCIsGenerated(t *testing.T) {
	cache := os.Getenv("GOMODCACHE")
	if cache == "" {
		cache = filepath.Join(build.Default.GOPATH, "pkg", "mod")
	}
	src := filepath.Join(cache, "modernc.org", "cc", "v4@"+Version)
	if _, err := os.Stat(src); err != nil {
		t.Skipf("%s: %v (go mod download modernc.org/cc/v4@%s)", src, err, Version)
	}
	out := filepath.Join(t.TempDir(), "cc")
	if err := Generate(out, src); err != nil {
		t.Fatal(err)
	}
	committed := filepath.Join("..", "..", "cc")
	want, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadDir(committed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("internal/cc has %d files, vendorcc makes %d", len(got), len(want))
	}
	for _, e := range want {
		a, _ := os.ReadFile(filepath.Join(out, e.Name()))
		b, err := os.ReadFile(filepath.Join(committed, e.Name()))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("internal/cc/%s is stale: run vendorcc", e.Name())
		}
	}
}

func TestRootCmd(t *testing.T) {
	cmd := RootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--cc", fakeModule(t, ""), "--out", filepath.Join(t.TempDir(), "cc")})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "wrote ") {
		t.Errorf("output %q", out.String())
	}
	cmd = RootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil {
		t.Error("--cc is required")
	}
}
