package cparse_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-embedded-c/c/internal/cparse"
	"github.com/go-embedded-c/c/internal/oracle/oracletest"
	"modernc.org/cc/v4"
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
			// Known, filed upstream defect (go-embedded-c/c issue): cc/v4
			// v4.30.5 panics on some malformed input, see knownPanics.
			// Source recovers it, which is what this target checks; a
			// panic outside cc, a hang or a crash still fails.
			if strings.Contains(pe.Stack, "modernc.org/cc/v4.") {
				t.Skip("known: cc/v4 panics on malformed input; recovered")
			}
			t.Fatalf("%v\n%s", pe, pe.Stack)
		}
	})
}

// knownPanics are inputs that make cc/v4 v4.30.5 panic (found by FuzzSource),
// with the function that panics.
var knownPanics = []struct{ src, in string }{
	{"#define", "(*cpp).define"},                                   // cpp.go:2918, bare directive at EOF
	{"#define M(\nM(", "(*cpp).parseMacroArgs"},                    // cpp.go:1495, unterminated invocation
	{"struct s { int a : QQQQQ3; };", "(*StructDeclarator).check"}, // check.go:3877, non-constant bit width
}

// TestKnownPanicsAreRecovered: each input returns a *PanicError from Source
// instead of crashing. When cc/v4 is fixed the entry stops panicking and is
// reported, to be dropped from the list.
func TestKnownPanicsAreRecovered(t *testing.T) {
	for _, k := range knownPanics {
		err := front(t).Source("p.c", []byte(k.src), cparse.Check)
		var pe *cparse.PanicError
		if !errors.As(err, &pe) {
			t.Logf("cc/v4 no longer panics on %q (%v): drop it from knownPanics", k.src, err)
			continue
		}
		if !strings.Contains(pe.Stack, k.in) || pe.Error() == "" {
			t.Errorf("%q: panicked elsewhere: %v", k.src, pe)
		}
	}
}
