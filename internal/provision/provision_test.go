package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssetFor(t *testing.T) {
	for _, p := range [][2]string{{"darwin", "arm64"}, {"linux", "amd64"}} {
		a, err := AssetFor(p[0], p[1])
		if err != nil || len(a.SHA256) != 64 || !strings.Contains(a.URL(), "llvmorg-23.1.4/"+a.Name) {
			t.Errorf("%v: %+v, %v", p, a, err)
		}
	}
	if _, err := AssetFor("plan9", "386"); err == nil {
		t.Error("an unpinned platform must be an error")
	}
}

func TestDownload(t *testing.T) {
	body := []byte("archive bytes")
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "sub", "a.bin")
	ctx := context.Background()
	if err := Download(ctx, srv.URL+"/a", dst, want); err != nil {
		t.Fatal(err)
	}
	if err := Download(ctx, srv.URL+"/a", dst, want); err != nil || hits != 1 {
		t.Fatalf("a file with the right sum must not be fetched again: hits=%d, %v", hits, err)
	}
	bad := filepath.Join(t.TempDir(), "b.bin")
	if err := Download(ctx, srv.URL+"/a", bad, strings.Repeat("0", 64)); err == nil {
		t.Fatal("a wrong sum must be an error")
	}
	if _, err := os.Stat(bad); err == nil {
		t.Fatal("a file with a wrong sum must be deleted")
	}
	if err := Download(ctx, srv.URL+"/missing", bad, want); err == nil {
		t.Fatal("a 404 must be an error")
	}
	if err := Download(ctx, "http://127.0.0.1:1/x", bad, want); err == nil {
		t.Fatal("an unreachable server must be an error")
	}
	if err := Download(ctx, "://bad", bad, want); err == nil {
		t.Fatal("a bad URL must be an error")
	}
}

func TestFileSHA256(t *testing.T) {
	if _, err := FileSHA256(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

func TestInstallBuiltins(t *testing.T) {
	home := t.TempDir()
	llvm := filepath.Join(home, "llvm")
	if err := installBuiltins(home, llvm); err == nil {
		t.Fatal("no resource dir must be an error")
	}
	if err := os.MkdirAll(filepath.Join(llvm, "lib", "clang", "23"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installBuiltins(home, llvm); err == nil {
		t.Fatal("missing builtins must be an error")
	}
	from := filepath.Join(home, "build", "temp-resource-dir", "lib", "wasm32-unknown-wasip1")
	if err := os.MkdirAll(from, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "libclang_rt.builtins.a"), []byte("ar"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installBuiltins(home, llvm); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(llvm, "lib", "clang", "23", "lib", "wasm32-unknown-wasip1", "libclang_rt.builtins.a")); err != nil {
		t.Fatal(err)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	home := t.TempDir()
	for _, p := range []string{"llvm/bin/clang", "sysroot/lib/wasm32-wasip1/libc.a"} {
		full := filepath.Join(home, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var log strings.Builder
	if err := Install(context.Background(), Options{Home: home, Log: &log}); err != nil || !strings.Contains(log.String(), "already installed") {
		t.Fatalf("%q, %v", log.String(), err)
	}
}
