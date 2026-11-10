package cparse_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/cparse"
	"github.com/go-embedded-c/c/internal/oracle/oracletest"
)

func front(t testing.TB) *cparse.Front {
	t.Helper()
	f, err := cparse.New("", "")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestABIIsWasm32(t *testing.T) {
	abi, err := cparse.ABI()
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[cc.Kind][2]int64{
		cc.Char: {1, 1}, cc.Short: {2, 2}, cc.Int: {4, 4}, cc.Long: {4, 4}, cc.LongLong: {8, 8},
		cc.Ptr: {4, 4}, cc.Float: {4, 4}, cc.Double: {8, 8}, cc.LongDouble: {16, 16},
		cc.ComplexDouble: {16, 8}, cc.ComplexLongDouble: {32, 16},
	} {
		if got := abi.Types[k]; got.Size != want[0] || int64(got.Align) != want[1] {
			t.Errorf("%v: %+v, want size %d align %d", k, got, want[0], want[1])
		}
	}
	if !abi.SignedChar {
		t.Error("char is signed on wasm32")
	}
}

func TestPredefinedDescribesWasm32(t *testing.T) {
	p := cparse.Predefined()
	for _, want := range []string{"#define __wasm32__ 1", "#define __SIZEOF_POINTER__ 4", "#define __SIZEOF_LONG_DOUBLE__ 16", "#define __wasi__ 1"} {
		if !strings.Contains(p, want) {
			t.Errorf("predefined.h lacks %q", want)
		}
	}
}

func TestSourceParsesAndChecks(t *testing.T) {
	f := front(t)
	good := []byte("struct s { int a; char b; };\nint f(struct s *p) { return p->a + sizeof(long); }\n")
	for _, lvl := range []cparse.Level{cparse.ParseOnly, cparse.Check} {
		if err := f.Source("good.c", good, lvl); err != nil {
			t.Errorf("level %d: %v", lvl, err)
		}
	}
	if err := f.Source("syntax.c", []byte("int f( { "), cparse.ParseOnly); err == nil {
		t.Error("a syntax error must be reported")
	}
	semantic := []byte("int f(void) { return undeclared; }")
	if err := f.Source("sem.c", semantic, cparse.ParseOnly); err != nil {
		t.Errorf("parse-only must not type-check: %v", err)
	}
	if err := f.Source("sem.c", semantic, cparse.Check); err == nil {
		t.Error("the checker must report an undeclared identifier")
	}
}

func TestFileFromDisk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.c")
	if err := os.WriteFile(p, []byte("int x;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := front(t).File(p, cparse.Check); err != nil {
		t.Fatal(err)
	}
	if err := front(t).File(p+".missing", cparse.Check); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

// TestHeadersFromTheSysroot: with the toolchain, the hand-filled config
// reads the real wasi-libc headers.
func TestHeadersFromTheSysroot(t *testing.T) {
	tc := oracletest.Require(t)
	inc, err := tc.ResourceInclude(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f, err := cparse.New(inc, tc.Sysroot)
	if err != nil {
		t.Fatal(err)
	}
	src := []byte("#include <stdio.h>\n#include <stdlib.h>\n#include <string.h>\n#include <math.h>\nint main(void){ printf(\"%f\", sqrt(2.0)); return 0; }\n")
	if err := f.Source("hdr.c", src, cparse.Check); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratePredefinedMatchesCommitted(t *testing.T) {
	tc := oracletest.Require(t)
	got, err := cparse.GeneratePredefined(t.Context(), tc.Clang)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != cparse.Predefined() {
		t.Fatal("internal/cparse/predefined.h is stale: run `coracle predefined`")
	}
	if _, err := cparse.GeneratePredefined(t.Context(), "/nonexistent/clang"); err == nil {
		t.Fatal("a missing clang must be an error")
	}
}

// The corpora: how many files cc/v4 takes. A front-end bump must not lower
// the floors. gcc 15.2.0 torture: 1881 / 1890. Eight of the nine refusals are
// wasi-libc's own #error (signal.h, setjmp.h, sys/mman.h), which clang
// refuses too; pr117432.c uses C23 `f(...)` with no named parameter, which
// cc/v4 does not take. The refused files are logged, the first twenty.
func corpusRate(t *testing.T, env string, lvl cparse.Level, floor int) {
	dir := oracletest.Dir(t, env)
	tc := oracletest.Require(t)
	inc, _ := tc.ResourceInclude(t.Context())
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".c") {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := cparse.New(inc, tc.Sysroot)
	if err != nil {
		t.Fatal(err)
	}
	ok, refused := 0, 0
	for _, p := range files {
		if err := f.File(p, lvl); err == nil {
			ok++
		} else if refused++; refused <= 20 {
			t.Logf("refused: %s: %v", p, err)
		}
	}
	t.Logf("cc %s over %s: %d / %d files", cparse.Version, env, ok, len(files))
	if ok < floor {
		t.Errorf("%d / %d files parse, the floor is %d", ok, len(files), floor)
	}
}

func TestCorpusCTestsuite(t *testing.T) { corpusRate(t, "CORACLE_CTESTSUITE", cparse.Check, 217) }
func TestCorpusGCCTorture(t *testing.T) { corpusRate(t, "CORACLE_GCC_TORTURE", cparse.Check, 1881) }

// FuzzSource: the front end is fed untrusted text. It may refuse anything;
// it must not panic.
func FuzzSource(f *testing.F) {
	fr := front(f)
	f.Add([]byte("int main(void) { return 0; }"))
	f.Add([]byte("struct s { int a : 3; } x = { .a = 1 };\n#define M(a, ...) a ## __VA_ARGS__\nint y = M(1,2);"))
	f.Add([]byte("#if 1 +\n#endif\n#include \"\"\n"))
	f.Add([]byte("_Static_assert(sizeof(long) == 4, \"ilp32\");\nvoid g(void) { goto l; l: switch (1) { default: break; } }"))
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 4096 {
			return
		}
		err := fr.Source("fuzz.c", src, cparse.Check)
		var pe *cparse.PanicError
		if errors.As(err, &pe) {
			t.Fatalf("%v\n%s", pe, pe.Stack)
		}
	})
}

// formerPanics are inputs that made cc/v4 v4.30.5 panic (found by
// FuzzSource, go-embedded-c/c#2); internal/cc's patches make each an error.
// The controls are the nearest valid inputs, which must still be accepted.
var formerPanics = []struct{ src, msg, control string }{
	{"#define", "macro name must be an identifier", "#define M\n"},
	{"#define\n", "macro name must be an identifier", "#define M 1\n"},
	{"#define M(\nM(", "unterminated argument list invoking a macro", "#define M(a) a\nint x = M(1);\n"},
	{"struct s { int a : QQQQQ3; };", "expected integer expression", "struct s { int a : 3; };"},
	// Not a panic but a hang: the type-qualifier loop shifted EOF forever.
	{"^", "expected type qualifier", "int (^b)(void);"},
	{"#define M##\nM", "'##' cannot appear at either end", "#define M(a, b) a##b\nint M(x, y);\n"},
	{"#define M ##x\nM", "'##' cannot appear at either end", "#define M x\nint M;\n"},
	// Hangs: the literal loops shifted EOF forever.
	{"\"(0!0!\xd2\\", "not terminated", "char *s = \"(0!0!\\\\\";"},
	{"char *s = \"abc", "not terminated", "char *s = \"abc\";"},
	{"int c = 'a", "not terminated", "int c = 'a';"},
	{"0={#,", "expected direct declarator", "int a[] = {0, 1};"},
	{"\"\"\\", "not terminated", "char *s = \"\";"},
	{"0\n\\", "unexpected", "int x = 0;\n"},
	// An unnamed parameter of an invalid type: DeclarationSpecifiers.check
	// returns nil, and .Decay() on it panicked (FuzzSource in CI,
	// 2026-11-08, run 42491474333).
	{"void f(int float) {}", "missed/failed type check", "void f(int) {}"},
	{"void f(long char);", "missed/failed type check", "void f(long);"},
	{"void f(int float x) {}", "missed/failed type check", "void f(int x) {}"},
	{"int float y; int (*fp)(long char);", "missed/failed type check", "int y; int (*fp)(long);"},
}

// TestEmptyVariadicArguments: `M()` for `M(A,...)` gives A and __VA_ARGS__
// no tokens (C23 6.10.5.1; it panicked, go-embedded-c/c#2).
func TestEmptyVariadicArguments(t *testing.T) {
	for _, src := range []string{
		"#define M(A,...) __VA_ARGS__\nM()",
		"#define M(A,...) A __VA_ARGS__\nint M(x);\n",
	} {
		var pe *cparse.PanicError
		if err := front(t).Source("p.c", []byte(src), cparse.Check); errors.As(err, &pe) {
			t.Errorf("%q panics: %v", src, pe)
		} else if err != nil {
			t.Errorf("%q: %v", src, err)
		}
	}
}

func TestFormerPanicsAreErrors(t *testing.T) {
	for _, k := range formerPanics {
		err := front(t).Source("p.c", []byte(k.src), cparse.Check)
		var pe *cparse.PanicError
		switch {
		case errors.As(err, &pe):
			t.Errorf("%q still panics: %v", k.src, pe)
		case err == nil || !strings.Contains(err.Error(), k.msg):
			t.Errorf("%q: %v, want %q", k.src, err, k.msg)
		}
		if err := front(t).Source("p.c", []byte(k.control), cparse.Check); err != nil {
			t.Errorf("control %q: %v", k.control, err)
		}
	}
	if err := front(t).Source("p.c", []byte("#define M"), cparse.Check); err != nil {
		t.Errorf("a name-only #define at the end of input: %v", err)
	}
}
