package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	small(t)
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

// small makes Download fetch in 5-byte ranges without waiting between
// attempts, for the duration of a test.
func small(t *testing.T) {
	c, w := chunkSize, retryWait
	chunkSize, retryWait = 5, 0
	t.Cleanup(func() { chunkSize, retryWait = c, w })
}

// TestDownloadRanges: the archive comes in ranges; a 504 on a range, the
// failure that blocked CI on 2026-11-08, is retried; a server that ignores
// Range still works; a client error is not retried.
func TestDownloadRanges(t *testing.T) {
	small(t)
	body := []byte("0123456789abcdefghijk") // 21 bytes: five ranges, the last short
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])
	ctx := context.Background()
	modTime := time.Unix(0, 0)

	var ranges, fails int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/flaky":
			// Every range fails twice with 504 before it is served.
			if fails++; fails%3 != 0 {
				w.WriteHeader(http.StatusGatewayTimeout)
				return
			}
		case "/whole":
			w.Write(body) // no Range support: 200 with everything
			return
		case "/forbidden":
			ranges++
			w.WriteHeader(http.StatusForbidden)
			return
		case "/down":
			ranges++
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		case "/shifted":
			w.Header().Set("Content-Range", "bytes 1-5/21")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(body[1:6])
			return
		}
		if r.Header.Get("Range") != "" {
			ranges++
		}
		http.ServeContent(w, r, "a", modTime, bytes.NewReader(body))
	}))
	defer srv.Close()
	dir := t.TempDir()

	if err := Download(ctx, srv.URL+"/a", filepath.Join(dir, "a"), want); err != nil || ranges != 5 {
		t.Fatalf("ranges: %d requests, %v; want 5, nil", ranges, err)
	}
	if err := Download(ctx, srv.URL+"/flaky", filepath.Join(dir, "flaky"), want); err != nil || fails != 15 {
		t.Fatalf("flaky: %d failures, %v; want 15 (2 per range, 5 ranges, 5 successes), nil", fails, err)
	}
	if err := Download(ctx, srv.URL+"/whole", filepath.Join(dir, "whole"), want); err != nil {
		t.Fatalf("whole: %v", err)
	}
	ranges = 0
	if err := Download(ctx, srv.URL+"/forbidden", filepath.Join(dir, "f"), want); err == nil || ranges != 1 {
		t.Fatalf("403: %d requests, %v; want 1 and an error", ranges, err)
	}
	ranges = 0
	if err := Download(ctx, srv.URL+"/down", filepath.Join(dir, "d"), want); err == nil || ranges != attempts {
		t.Fatalf("504 for good: %d requests, %v; want %d and an error", ranges, err, attempts)
	}
	if err := Download(ctx, srv.URL+"/shifted", filepath.Join(dir, "s"), want); err == nil {
		t.Fatal("a range that does not start at the offset asked for must be an error")
	}
	if _, err := os.Stat(filepath.Join(dir, "s")); err == nil {
		t.Fatal("a failed download must leave no file")
	}
}

func TestParseContentRange(t *testing.T) {
	for _, c := range []struct {
		s                 string
		first, last, size int64
		ok                bool
	}{
		{"bytes 0-4/21", 0, 4, 21, true},
		{"bytes 20-20/21", 20, 20, 21, true},
		{"bytes 0-4/*", 0, 0, 0, false},
		{"bytes 5-4/21", 0, 0, 0, false},
		{"bytes 0-21/21", 0, 0, 0, false},
		{"bytes -1-4/21", 0, 0, 0, false},
		{"items 0-4/21", 0, 0, 0, false},
		{"bytes 0-4", 0, 0, 0, false},
		{"bytes 04/21", 0, 0, 0, false},
		{"bytes 0-99999999999999999999/1", 0, 0, 0, false},
	} {
		first, last, size, err := parseContentRange(c.s)
		if (err == nil) != c.ok || c.ok && (first != c.first || last != c.last || size != c.size) {
			t.Errorf("%q: %d %d %d %v", c.s, first, last, size, err)
		}
	}
}

// FuzzParseContentRange: the header comes from the network; whatever it
// says, an accepted range is inside the file.
func FuzzParseContentRange(f *testing.F) {
	for _, s := range []string{"bytes 0-4/21", "bytes 0-4/*", "bytes 5-4/21", "bytes 9223372036854775806-9223372036854775806/9223372036854775807"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		first, last, size, err := parseContentRange(s)
		if err == nil && !(0 <= first && first <= last && last < size) {
			t.Fatalf("%q: accepted %d-%d/%d", s, first, last, size)
		}
	})
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
