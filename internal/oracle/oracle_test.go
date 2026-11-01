package oracle_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-embedded-c/c/internal/oracle"
	"github.com/go-embedded-c/c/internal/oracle/oracletest"
)

func TestParseVersion(t *testing.T) {
	for _, c := range []struct {
		in, want string
		bad      bool
	}{
		{"clang version 23.1.4 (https://github.com/llvm/llvm-project abc)\nTarget: x", "23.1.4", false},
		{"Homebrew clang version 23.1.2\nTarget", "23.1.2", false},
		{"clang version 23.1.4", "23.1.4", false},
		{"clang version \n", "", true},
		{"gcc 15", "", true},
	} {
		got, err := oracle.ParseVersion([]byte(c.in))
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("ParseVersion(%q) = %q, %v", c.in, got, err)
		}
	}
}

func TestLocate(t *testing.T) {
	t.Setenv(oracle.EnvClang, "")
	t.Setenv(oracle.EnvSysroot, "")
	t.Setenv(oracle.EnvHome, "")
	if _, err := oracle.Locate(); !errors.Is(err, oracle.ErrNoToolchain) {
		t.Fatalf("Locate with nothing set: %v", err)
	}
	t.Setenv(oracle.EnvHome, "/h")
	tc, err := oracle.Locate()
	if err != nil || tc.Clang != "/h/llvm/bin/clang" || tc.Sysroot != "/h/sysroot" {
		t.Fatalf("Locate with home: %+v, %v", tc, err)
	}
	t.Setenv(oracle.EnvClang, "/c")
	t.Setenv(oracle.EnvSysroot, "/s")
	if tc, _ := oracle.Locate(); tc.Clang != "/c" || tc.Sysroot != "/s" {
		t.Fatalf("explicit variables must win: %+v", tc)
	}
}

func TestVerifyRefusesMissing(t *testing.T) {
	tc := &oracle.Toolchain{Clang: "/nonexistent/clang", Sysroot: t.TempDir()}
	if err := tc.Verify(context.Background()); !errors.Is(err, oracle.ErrNoToolchain) {
		t.Fatalf("missing clang: %v", err)
	}
}

func TestArgs(t *testing.T) {
	tc := &oracle.Toolchain{Clang: "clang", Sysroot: "/sr"}
	got := strings.Join(tc.Args(oracle.CompileOptions{Extra: []string{"-DX=1"}}, "o.wasm"), " ")
	for _, want := range []string{"--target=wasm32-wasip1", "--sysroot=/sr", "-O0", "-std=gnu89", "-DX=1", "-x c - -o o.wasm"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q lack %q", got, want)
		}
	}
}

func newRunner(t *testing.T, lim oracle.Limits) *oracle.Runner {
	t.Helper()
	r, err := oracle.NewRunner(context.Background(), lim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(context.Background()) })
	return r
}

func TestRunInvalidModule(t *testing.T) {
	if _, err := newRunner(t, oracle.Limits{}).Run(context.Background(), []byte("not wasm"), nil, nil); err == nil {
		t.Fatal("an invalid module must be a harness error")
	}
}

// Live oracle tests: they need the pinned toolchain.

func TestOracleVersionIsPinned(t *testing.T) {
	tc := oracletest.Require(t)
	v, err := tc.Version(context.Background())
	if err != nil || v != oracle.ClangVersion {
		t.Fatalf("version %q, %v", v, err)
	}
}

func run(t *testing.T, lim oracle.Limits, src string, args ...string) oracle.Result {
	t.Helper()
	tc := oracletest.Require(t)
	res, err := tc.Output(context.Background(), newRunner(t, lim), []byte(src), oracle.CompileOptions{Std: "gnu17"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestOracleStdoutAndExit(t *testing.T) {
	res := run(t, oracle.Limits{}, `#include <stdio.h>
int main(void){ puts("out"); fputs("err\n", stderr); return 5; }`)
	if string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" || res.Exit != 5 || res.Trap != "" {
		t.Fatalf("%+v", res)
	}
}

func TestOracleArgsAndStdin(t *testing.T) {
	tc := oracletest.Require(t)
	r := newRunner(t, oracle.Limits{})
	wasm, err := tc.Compile(context.Background(), []byte(`#include <stdio.h>
int main(int argc, char **argv){ int c = getchar(); printf("%d %s %c\n", argc, argv[1], c); return 0; }`), oracle.CompileOptions{Std: "gnu17"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), wasm, []string{"arg"}, []byte("Z"))
	if err != nil || string(res.Stdout) != "2 arg Z\n" {
		t.Fatalf("%+v, %v", res, err)
	}
}

func TestOracleTrap(t *testing.T) {
	res := run(t, oracle.Limits{}, `#include <stdlib.h>
int main(void){ abort(); }`)
	if !strings.Contains(res.Trap, "unreachable") {
		t.Fatalf("%+v", res)
	}
}

func TestOracleTimeout(t *testing.T) {
	res := run(t, oracle.Limits{Timeout: 300 * time.Millisecond}, `int main(void){ volatile int x = 1; while (x) {} return 0; }`)
	if !res.TimedOut {
		t.Fatalf("%+v", res)
	}
}

func TestOracleOutputBound(t *testing.T) {
	res := run(t, oracle.Limits{MaxOutput: 100}, `#include <stdio.h>
int main(void){ for (;;) puts("0123456789"); }`)
	if !res.Truncated || len(res.Stdout) != 100 {
		t.Fatalf("truncated=%v len=%d trap=%q", res.Truncated, len(res.Stdout), res.Trap)
	}
}

// A guest's allocation bomb is refused by the memory bound, not served by
// the host: malloc returns NULL.
func TestOracleMemoryBound(t *testing.T) {
	res := run(t, oracle.Limits{MemoryPages: 256}, `#include <stdio.h>
#include <stdlib.h>
int main(void){ void *p = malloc(200u << 20); printf("%d\n", p == NULL); return 0; }`)
	if string(res.Stdout) != "1\n" {
		t.Fatalf("%+v", res)
	}
}

func TestOracleCompileError(t *testing.T) {
	tc := oracletest.Require(t)
	_, err := tc.Compile(context.Background(), []byte("int main(void){ return x; }"), oracle.CompileOptions{})
	var ce *oracle.CompileError
	if !errors.As(err, &ce) || !strings.Contains(ce.Diagnostics, "undeclared identifier") {
		t.Fatalf("%v", err)
	}
}

func TestOracleCompileTimeout(t *testing.T) {
	tc := oracletest.Require(t)
	_, err := tc.Compile(context.Background(), []byte("int main(void){return 0;}"), oracle.CompileOptions{Timeout: time.Nanosecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

func TestOracleDeterministic(t *testing.T) {
	src := `#include <stdio.h>
#include <stdlib.h>
#include <time.h>
int main(void){ printf("%d %ld\n", rand(), (long)time(NULL) != 0); return 0; }`
	a, b := run(t, oracle.Limits{}, src), run(t, oracle.Limits{}, src)
	if string(a.Stdout) != string(b.Stdout) {
		t.Fatalf("%q vs %q", a.Stdout, b.Stdout)
	}
}
