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
// the floors. gcc 15.2.0 torture: 1881 / 1890. Eight of the refusals are
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

// 1881 while cc/v4 ignored the mode and packed attributes, with the wrong
// sizes; 1867 with mode refused (2026-11-14); 1866 with both implemented
// (2026-11-15), a packed bit-field refused; 1879 with the Itanium bit-field
// layout, packed bit-fields included (go-embedded-c/c#29); 1881 with a mode
// on a vector type (pr23135.c, simd-1.c).
func TestCorpusGCCTorture(t *testing.T) { corpusRate(t, "CORACLE_GCC_TORTURE", cparse.Check, 1881) }

// FuzzSource: the front end is fed untrusted text. It may refuse anything;
// it must not panic.
func FuzzSource(f *testing.F) {
	fr := front(f)
	f.Add([]byte("int main(void) { return 0; }"))
	f.Add([]byte("struct s { int a : 3; } x = { .a = 1 };\n#define M(a, ...) a ## __VA_ARGS__\nint y = M(1,2);"))
	f.Add([]byte("#if 1 +\n#endif\n#include \"\"\n"))
	f.Add([]byte("_Static_assert(sizeof(long) == 4, \"ilp32\");\nvoid g(void) { goto l; l: switch (1) { default: break; } }"))
	f.Add([]byte(modeSource))
	f.Add([]byte(packedSource))
	f.Add([]byte(bitfieldSource))
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
	// A ',' after a refused first attribute value: the list was nil
	// (FuzzSource, 2026-11-15, once seeded with attributes).
	{"(000000 000 __attribute 0!00,", "expected direct declarator", "int x __attribute__((unused, used));"},
	{"int x __attribute__((0, used));", "expected attribute value", "int x __attribute__((unused, used));"},
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

// modeSource gives declarations the width of their mode attribute in each
// place the attribute is written. clang 23.1.4 (wasm32-wasip1, -std=gnu17
// -fsyntax-only) holds every assertion, measured 2026-11-15.
const modeSource = `typedef int w __attribute__((mode(word)));
typedef int p __attribute__((mode(pointer)));
typedef int b __attribute__((__mode__(__byte__)));
typedef char c8 __attribute__((mode(QI)));
typedef unsigned char uc16 __attribute__((mode(HI)));
typedef float f8 __attribute__((mode(DF)));
typedef double d4 __attribute__((mode(SF)));
enum E { A, B };
enum N { M = -1, P };
struct s { enum E __attribute__((mode(byte))) e; enum N __attribute__((mode(byte))) n; };
struct s v;
typedef unsigned int __attribute__((mode(QI))) u8;
u8 *pu8;
typedef const unsigned int __attribute__((mode(QI))) cu8;
cu8 *pc, c1;
_Static_assert(sizeof(*pc) == 1 && sizeof c1 == 1, "a typedef with another attribute");
typedef unsigned int u64 __attribute__((__mode__(__DI__)));
typedef unsigned int a8 __attribute__((mode(QI))), a32;
int x __attribute__((mode(HI)));
const int __attribute__((mode(QI))) cq = 1;
_Static_assert(sizeof(w) == 4 && sizeof(p) == 4 && sizeof(b) == 1, "word, pointer, byte");
_Static_assert((c8)-1 < 0 && (uc16)-1 > 0 && sizeof(uc16) == 2, "signedness kept");
_Static_assert(sizeof(f8) == 8 && sizeof(d4) == 4, "floating modes");
_Static_assert(sizeof(struct s) == 2 && (__typeof__(v.e))-1 > 0 && (__typeof__(v.n))-1 < 0, "enumeration members");
_Static_assert(sizeof(*pu8) == 1, "through a typedef and a pointer");
_Static_assert(sizeof(u64) == 8 && (u64)-1 > 0, "after the declarator");
_Static_assert(sizeof(a8) == 1 && sizeof(a32) == 4, "after one declarator only");
_Static_assert(sizeof x == 2 && sizeof cq == 1, "variables");
int f(int __attribute__((mode(QI))) a) { _Static_assert(sizeof a == 1, "a parameter"); return a; }
`

// TestModeAttribute: cc/v4 ignored __attribute__((mode(QI))), so the type
// kept its declared size (4 bytes where clang gives 1), and the generator
// wrote past packed members (gcc torture memclr.c, misalign.c). The patch
// gives the declaration the mode's type, and refuses the attribute where no
// declarator takes it, or where clang would not apply it either.
func TestModeAttribute(t *testing.T) {
	if err := front(t).Source("m.c", []byte(modeSource), cparse.Check); err != nil {
		var pe *cparse.PanicError
		if errors.As(err, &pe) {
			t.Fatal(pe.Stack)
		}
		t.Fatal(err)
	}
	for _, k := range []struct{ src, msg string }{
		{"int y = (int __attribute__((mode(QI))))300;", "attribute mode is not supported here"},
		{"struct t { int a __attribute__((mode(QI))); };", "attribute mode is not supported here"},
		{"typedef int __attribute__((mode(TI))) t;", "no type of 128 bits"},
		{"typedef int __attribute__((mode(SF))) t;", "attribute mode SF on a type that is not floating"},
		{"typedef float __attribute__((mode(SI))) t;", "attribute mode SI on a type that is not an integer"},
		{"int __attribute__((mode(QI))) *q;", "attribute mode QI on a type that is not an integer"},
		{"typedef int __attribute__((mode(XQ))) t;", "attribute mode XQ is not supported"},
		{"typedef int __attribute__((mode(QI))) __attribute__((mode(HI))) t;", "conflicting mode attributes: QI and HI"},
		{"typedef int __attribute__((mode(1))) t;", "attribute mode expects one identifier"},
	} {
		err := front(t).Source("m.c", []byte(k.src), cparse.Check)
		if err == nil || !strings.Contains(err.Error(), k.msg) {
			t.Errorf("%q: %v, want %q", k.src, err, k.msg)
		}
	}
}

// packedSource lays out packed structs and unions in each place the
// attribute is written. clang 23.1.4 (wasm32-wasip1, -std=gnu17
// -fsyntax-only, with <stddef.h>'s offsetof) holds every assertion, measured
// 2026-11-15.
const packedSource = `#define offsetof(t, m) __builtin_offsetof(t, m)
struct __attribute__((packed)) p1 { char x; long long y; short z; };
struct p2 { char x; long long y; short z; } __attribute__((__packed__));
typedef struct { char c; int i; } __attribute__((packed)) p3;
struct p4 { char c; int i __attribute__((packed)); short s; };
struct p5 { char c; double d __attribute__((aligned(2), packed)); };
struct p6 { char c; int i; } __attribute__((packed)) __attribute__((aligned(4)));
union __attribute__((packed)) u1 { char c; int i; };
struct n1 { char c; struct p1 m; };
struct n2 { char c; struct p6 m; };
struct p7 { char c; char a, *b __attribute__((packed)); };
struct p8 { char c; int *a, b __attribute__((packed)); };
struct p10 { char c; int i __attribute__((unused, packed, deprecated)); };
_Static_assert(offsetof(struct p10, i) == 1, "the middle of three attributes");
struct __attribute__((aligned(4))) p9 { char c; int i; } __attribute__((packed));
_Static_assert(sizeof(struct p9) == 8 && _Alignof(struct p9) == 4 && offsetof(struct p9, i) == 1, "aligned before the tag, packed after the brace");
_Static_assert(sizeof(struct p1) == 11 && _Alignof(struct p1) == 1 && offsetof(struct p1, z) == 9, "before the tag");
_Static_assert(sizeof(struct p2) == 11 && offsetof(struct p2, y) == 1, "after the brace");
_Static_assert(sizeof(p3) == 5 && _Alignof(p3) == 1, "typedef of an unnamed struct");
_Static_assert(offsetof(struct p4, i) == 1 && offsetof(struct p4, s) == 6 && sizeof(struct p4) == 8 && _Alignof(struct p4) == 2, "one member");
_Static_assert(offsetof(struct p5, d) == 2 && sizeof(struct p5) == 10 && _Alignof(struct p5) == 2, "aligned and packed member");
_Static_assert(sizeof(struct p6) == 8 && _Alignof(struct p6) == 4 && offsetof(struct p6, i) == 1, "packed and aligned struct");
_Static_assert(sizeof(union u1) == 4 && _Alignof(union u1) == 1, "union");
_Static_assert(offsetof(struct n1, m) == 1 && sizeof(struct n1) == 12, "a packed struct as a member");
_Static_assert(offsetof(struct n2, m) == 4 && sizeof(struct n2) == 12, "a packed and aligned struct as a member");
_Static_assert(offsetof(struct p7, b) == 2 && offsetof(struct p8, a) == 4 && sizeof(struct p8) == 12, "after the last declarator only");
`

// bitfieldSource: bit-fields laid out by the Itanium rule, packed or not.
// clang 23.1.4 (wasm32-wasip1, -std=gnu17 -fsyntax-only) holds every
// assertion, measured 2026-11-15; cc/v4's group allocator gave struct s29
// size 16 and h at 8 (go-embedded-c/c#29).
const bitfieldSource = `#define offsetof(t, m) __builtin_offsetof(t, m)
struct s29 { unsigned long long f:44; _Bool g:1; short h; };
struct __attribute__((packed)) pk { int g:31; int h:6; char c; };
struct pk2 { char a; unsigned long w:32 __attribute__((packed)); char b; };
struct anon { char a; int :4; char b; };
union u { char c; int :20; };
struct cross { int a:30; int b:10; char c; };
_Static_assert(offsetof(struct cross, c) == 6 && sizeof(struct cross) == 8, "a bit-field does not straddle its type's alignment");
_Static_assert(sizeof(struct s29) == 8 && offsetof(struct s29, h) == 6, "go-embedded-c/c#29");
_Static_assert(sizeof(struct pk) == 6 && _Alignof(struct pk) == 1 && offsetof(struct pk, c) == 5, "packed bit-fields");
_Static_assert(sizeof(struct pk2) == 6 && offsetof(struct pk2, b) == 5, "a packed bit-field member");
_Static_assert(sizeof(struct anon) == 3 && _Alignof(struct anon) == 1, "an unnamed bit-field does not align");
_Static_assert(sizeof(union u) == 3 && _Alignof(union u) == 1, "nor a union");
`

// complexTypingSource: the type of mixed real and complex operations keeps
// the complex domain (C11 6.3.1.8), and an imaginary constant has the type
// of its suffix. clang 23.1.4 (wasm32-wasip1, -std=gnu17 -fsyntax-only)
// holds every assertion, measured 2026-11-15; cc/v4 typed ld + dc as long
// double, d + fc as double, and 2.0iL as _Complex long.
const complexTypingSource = `long double ld; double d; float _Complex fc; double _Complex dc; __complex__ int ic;
_Static_assert(sizeof(ld + dc) == 32 && sizeof(dc + ld) == 32, "long double and double _Complex");
_Static_assert(sizeof(d + fc) == 16 && sizeof(fc * d) == 16, "double and float _Complex");
_Static_assert(sizeof(ic + d) == 16 && sizeof(ic + 1.0f) == 8, "integer complex and a floating real");
_Static_assert(sizeof(2.0iL) == 32 && sizeof(2.0i) == 16 && sizeof(2.0fi) == 8, "imaginary constants");
`

// TestComplexTyping checks complexTypingSource.
func TestComplexTyping(t *testing.T) {
	if err := front(t).Source("c.c", []byte(complexTypingSource), cparse.Check); err != nil {
		t.Fatal(err)
	}
}

// vectorTypingSource: GNU vectors keep their type through the operators,
// a comparison is a vector of integers as wide, an element has the
// element's type, and a vector is aligned on its size. clang 23.1.4
// (wasm32-wasip1, -std=gnu17 -fsyntax-only) holds every assertion,
// measured 2026-11-15; cc/v4 typed s + 2, s << 2 and 1 << s as int, s[1] as the
// whole vector, s < s as int, aligned a vector on its element, counted the
// initialisers of a vector declared with the attribute before the type in
// bytes, and the mode patch refused a mode on a vector, which applies to
// its element.
const vectorTypingSource = `typedef short v8hi __attribute__((vector_size(16)));
typedef int v8si __attribute__((vector_size(32)));
typedef double v2df __attribute__((vector_size(16)));
typedef char v2qi __attribute__((vector_size(2)));
typedef short __attribute__((vector_size(16))) v8pre;
v8hi s = {1, 2, 3, 4, 5, 6, 7, 8}; v2df d = {1, 2};
v8pre pre = {1, 2, 3, 4, 5, 6, 7, 8};
struct m { char c; v8hi v; };
_Static_assert(sizeof(s + 2) == 16 && sizeof(2 * s) == 16 && sizeof(s << 2) == 16 && sizeof(1 << s) == 16 && sizeof(-s) == 16 && sizeof(~s) == 16, "operators keep the vector");
_Static_assert(sizeof(s < s) == 16 && sizeof(d == d) == 16 && sizeof((d < d)[0]) == 8 && sizeof((s != s)[1]) == 2, "comparisons");
_Static_assert(sizeof(s[1]) == 2 && sizeof(d[0]) == 8, "an element");
_Static_assert(_Alignof(v8hi) == 16 && _Alignof(v8si) == 32 && _Alignof(v2qi) == 2 && __builtin_offsetof(struct m, v) == 16, "alignment");
typedef int __attribute__((mode(SI))) __attribute__((vector_size(16))) vsi;
typedef int __attribute__((vector_size(16))) __attribute__((mode(QI))) vqi;
vsi ma; vqi mb;
_Static_assert(sizeof(vsi) == 16 && sizeof(ma[0]) == 4 && sizeof(vqi) == 16 && sizeof(mb[0]) == 1 && _Alignof(vqi) == 16, "mode on a vector");
`

// TestVectorTyping checks vectorTypingSource.
func TestVectorTyping(t *testing.T) {
	if err := front(t).Source("v.c", []byte(vectorTypingSource), cparse.Check); err != nil {
		t.Fatal(err)
	}
}

// TestBitfieldLayout: the constant sizeof, _Alignof and offsetof of
// structures with bit-fields, which cc/v4 folds itself and which no
// generator-side check could catch (go-embedded-c/c#29).
func TestBitfieldLayout(t *testing.T) {
	if err := front(t).Source("b.c", []byte(bitfieldSource), cparse.Check); err != nil {
		t.Fatal(err)
	}
}

// TestPackedAttribute: cc/v4 ignored __attribute__((packed)), and a packed
// struct kept an unpacked layout (sizeof 24 where clang gives 11; gcc torture
// misalign.c wrote past its member). The patch packs the members of a packed
// definition and a packed member, and refuses what it does not implement and
// the attribute where nothing takes it.
func TestPackedAttribute(t *testing.T) {
	if err := front(t).Source("p.c", []byte(packedSource), cparse.Check); err != nil {
		t.Fatal(err)
	}
	for _, k := range []struct{ src, msg string }{
		{"enum e { A, B } __attribute__((packed));", "attribute packed is not supported here"},
		{"__attribute__((packed)) int x;", "attribute packed is not supported here"},
		{"struct s { __attribute__((packed)) int x; };", "attribute packed is not supported here"},
	} {
		err := front(t).Source("p.c", []byte(k.src), cparse.Check)
		if err == nil || !strings.Contains(err.Error(), k.msg) {
			t.Errorf("%q: %v, want %q", k.src, err, k.msg)
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
