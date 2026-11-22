package wasmgen_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/go-embedded-c/c/internal/cparse"
	"github.com/go-embedded-c/c/internal/oracle"
	"github.com/go-embedded-c/c/internal/oracle/oracletest"
	"github.com/go-embedded-c/c/wasmgen"
)

func compile(t testing.TB, f *cparse.Front, src string) ([]byte, error) {
	t.Helper()
	ast, err := f.Translate("t.c", []byte(src))
	if err != nil {
		t.Fatalf("%q: %v", src, err)
	}
	return wasmgen.Compile(ast, "t.c")
}

// TestInt128Refusals: a refusal inside any operand of an __int128 operation
// reaches the caller, from every position the generator evaluates one in.
// __builtin_classify_type is refused, so it marks where the error must come
// out; a position that dropped it would compile and run something else.
func TestInt128Refusals(t *testing.T) {
	f, err := cparse.New("", "")
	if err != nil {
		t.Fatal(err)
	}
	const refused = "builtin __builtin_classify_type"
	const u = "__builtin_classify_type(1)"
	const pre = "typedef __int128 S; S g(int); "
	for _, body := range []string{
		"S f(S d) { return d + (S)" + u + "; }",
		"S f(S d) { return (S)" + u + " + d; }",
		"S f(void) { return (S)" + u + "; }",
		"S f(void) { return (S)(long double)" + u + "; }",
		"long double f(S d) { return (long double)(d + (S)" + u + "); }",
		"int f(S d) { return (int)(d + (S)" + u + "); }",
		"int f(void) { if ((S)" + u + ") return 1; return 0; }",
		"S f(void) { return -(S)" + u + "; }",
		"S f(void) { return ~(S)" + u + "; }",
		"S f(S d) { return d << " + u + "; }",
		"S f(void) { return ((S)" + u + ") << 3; }",
		"S f(S d) { return " + u + " ? d : d; }",
		"S f(S d) { return d ? (S)" + u + " : d; }",
		"S f(S d) { return d ? d : (S)" + u + "; }",
		"S f(S d) { return (" + u + ", d); }",
		"S f(void) { return g(" + u + "); }",
		"int f(S d) { return d < (S)" + u + "; }",
		"int f(S d) { return (S)" + u + " < d; }",
		"S f(void) { return ++*(S *)" + u + "; }",
		"S f(void) { return (*(S *)" + u + ")--; }",
		"void f(S d) { d = (S)" + u + "; }",
		"void f(S d) { d += (S)" + u + "; }",
		"void f(S d) { d <<= " + u + "; }",
		"void f(S d) { *(S *)" + u + " = d; }",
		"void f(S d) { d += (double)" + u + "; }",
		"void f(S d) { *(S *)" + u + " += 1.5; }",
		"void f(S d) { int k = 1; k += (S)" + u + "; }",
		"void f(S d) { *(int *)" + u + " += d; }",
		"int f(S d) { return d < (double)" + u + "; }",
		"int f(void) { return !(S)" + u + "; }",
		"void f(void) { g(" + u + "); }",
		"void f(void) { -(S)" + u + "; }",
		"void v(int, ...); void f(void) { v(1, (S)" + u + "); }",
		"S f(S d) { return (S)(" + u + " ? 1.5 : 2); }",
	} {
		_, err := compile(t, f, pre+body+" int main(void) { return 0; }")
		var un *wasmgen.Unsupported
		if !errors.As(err, &un) || un.What != refused {
			t.Errorf("%q: %v, want unsupported %q", body, err, refused)
		}
	}
	_, err = compile(t, f, pre+"S f(S d) { return d ?: d; } int main(void) { return 0; }")
	var un *wasmgen.Unsupported
	if !errors.As(err, &un) || un.What != "?: without a middle operand" {
		t.Errorf("?: of an __int128: %v", err)
	}
	// Each nested operation enters one level: past the limit the
	// expression is refused, not walked to a stack overflow. The refusal is
	// at a negation: one at the leaf would mean the walk went all the way
	// down before anything stopped it.
	nest := pre + "S f(S d) { return " + strings.Repeat("- ", 1001)
	_, err = compile(t, f, nest+"d; } int main(void) { return 0; }")
	if !errors.As(err, &un) || un.What != "expression nested deeper than the limit" || un.Pos == fmt.Sprintf("t.c:1:%d", len(nest)+1) {
		t.Errorf("1001 nested negations: %v", err)
	}
	// Comparing an __int128 with a vector is refused, not miscompiled.
	_, err = compile(t, f, pre+"typedef long v2 __attribute__((vector_size(16))); int f(S d, v2 v) { return d < v; } int main(void) { return 0; }")
	if !errors.As(err, &un) {
		t.Errorf("__int128 < vector: %v", err)
	}
	// So is converting a vector of the same size to one.
	_, err = compile(t, f, pre+"typedef int v4 __attribute__((vector_size(16))); S f(v4 v) { return (S)v; } int main(void) { return 0; }")
	if !errors.As(err, &un) || !strings.HasPrefix(un.What, "conversion of ") {
		t.Errorf("vector to __int128: %v", err)
	}
	// A constant division by zero is undefined; it is left to run time, not
	// folded (math/big would panic).
	if _, err := compile(t, f, pre+"S f(void) { return (S)1 / (S)0 + (S)1 % (S)0; } int main(void) { return 0; }"); err != nil {
		t.Errorf("constant __int128 division by zero: %v", err)
	}
}

// TestInt128Inline: a shift by a constant below 128, and the operations clang
// inlines, call no library routine; a variable shift calls one. That the
// results are right is TestAgainstClang's; that the inline path was taken is
// only visible here.
func TestInt128Inline(t *testing.T) {
	f, err := cparse.New("", "")
	if err != nil {
		t.Fatal(err)
	}
	has := func(src, name string) bool {
		obj, err := compile(t, f, "typedef __int128 S; "+src+" int main(void) { return 0; }")
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		return bytes.Contains(obj, []byte(name))
	}
	if has("S f(S d) { return (d << 5) + (d >> 70) + (d >> 64); }", "__ashlti3") || has("S f(S d) { return d >> 5; }", "__ashrti3") {
		t.Error("a shift by a constant called the library")
	}
	if !has("S f(S d, int n) { return d << n; }", "__ashlti3") {
		t.Error("a shift by a variable did not call __ashlti3")
	}
}

// TestUnsupported: everything past W2 is refused by name, not miscompiled.
func TestUnsupported(t *testing.T) {
	f, err := cparse.New("", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ src, what string }{
		{"int main(int c, char **v, char **e) { return 0; }", "main with parameters other than (int, char **)"},
		{"int f(int i) { typedef struct { int c[i + 2]; } c; return sizeof(c) * 3; } int main(void) { return f(1); }", "variable-length type"},
		{"int f(int i) { return sizeof(int[i]); } int main(void) { return f(1); }", "sizeof of a variable-length type"},
		{"int main(void) { int x = 1; switch (x) { case 1 ... 3: return 1; } return 0; }", "case range"},
		{"int main(void) { int x = 1; return x ?: 2; }", "?: without a middle operand"},
		{"struct S { long double d; }; struct S f(struct S s) { return s; } int main(void) { return 0; }", "parameter of type struct S {d long double}"},
		{"struct S { long double d[1]; }; int g(struct S); int main(void) { struct S s = {1}; return g(s); }", "parameter of type struct S {d array of 1 long double}"},
		{"long double d = 1e-4950L; int main(void) { return 0; }", "non-constant initializer"},
		{"int main(void) { long double d = 1e-4950L; return d; }", "long double constant 1e-4950L"},
		{"int n; long double d = n; int main(void) { return 0; }", "non-constant initializer"},
		{"int main(void) { long double d = 1; int k = 3; k %= d; return k; }", "compound assignment of a scalar by a long double"},
		{"int n; __int128 d = n; int main(void) { return 0; }", "non-constant initializer"},
		{"int main(void) { long double l = 1; __int128 d = 1; d += l; return (int)d; }", "compound assignment of an __int128 by long double"},
		{"int main(void) { double f = 1; __int128 d = 1; f += d; return (int)f; }", "compound assignment by an __int128 in double"},
		{"int main(void) { int k = 3; __int128 d = 1; k <<= d; return k; }", "compound assignment of a scalar by an __int128"},
		{"int main(void) { __int128 d = 1; int *p = 0; p += d; return p != 0; }", "compound assignment of a pointer by an __int128"},
		{"struct S { __int128 x; }; int f(struct S s) { return (int)s.x; } int main(void) { return 0; }", "parameter of type struct S {x __int128}"},
		{"union U { __int128 x; long y; }; int main(void) { union U u = (union U)(__int128)5; return (int)u.y; }", "cast of __int128 to a union"},
		{"int main(void) { return (int)__builtin_nanl(\"0x1g\"); }", "__builtin_nanl of a string clang does not read as a number"},
		{"typedef float v4 __attribute__((vector_size(16))); v4 f(v4 a) { return a + 1.0L; } int main(void) { return 0; }", "vector operand of type long double"},
		{"void f(int n) { struct S { int i[n]; int k; } s; } int main(void) { f(1); return 0; }", "local of type struct S {i array of int; k int}"},
		{"int f(int n, void *p) { struct S { int i[n]; int k; } *s = p; return s->k; } int main(void) { return 0; }", "local of type pointer to struct S"},
		{"int g(int n) { __builtin_va_start(n, n); return 0; } int main(void) { return g(1); }", "va_start outside a variadic function"},
		{"int x; struct S { int a: 3; } s = {x}; int main(void) { return 0; }", "non-constant bit-field initializer"},
		// Statement expressions: jumping out of one is permitted by GCC and
		// refused here; jumping into one is not permitted.
		{"int main(void) { int i, s = 0; for (i = 0; i < 3; i++) s += ({ if (i == 1) break; i; }); return s; }", "jump out of a statement expression"},
		{"int main(void) { int i, s = 0; for (i = 0; i < 3; i++) s += ({ if (i == 1) continue; i; }); return s; }", "jump out of a statement expression"},
		{"int main(int c, char **v) { switch (c) { case 1: ({ case 2: c = 5; }); } return c; }", "jump out of a statement expression"},
		{"int main(void) { int x = ({ int y = 1; goto l; l: y++; y; }); return x; }", "label or goto in a statement expression"},
		// A packed bit-field of 64 bits from bit 3 spans 9 bytes, more than
		// any wasm access.
		{"struct __attribute__((packed)) S { char c:3; unsigned long long x:64; }; int main(void) { struct S s; s.x = 1; return (int)s.x; }", "bit-field in a storage unit of odd size"},
		{"int f(); int g(int (*p)()) { return p(1); } int main(void) { return 0; }", "call through a pointer to a function without a prototype"},
		{"int f(int n) { int a[2][n]; return a[0][0]; } int main(void) { return f(2); }", "variable-length array with a variable inner dimension"},
		{"int f(int n) { int a[n] __attribute__((aligned(32))); return a[0]; } int main(void) { return f(2); }", "over-aligned variable-length array"},
		{"int f(int n) { int (a[n]); return 0; } int main(void) { return f(2); }", "variable-length array in a parenthesized declarator"},
		{"int f(int n) { int a[(int)__builtin_nanl(\"0x1g\")]; return 0; } int main(void) { return f(2); }", "__builtin_nanl of a string clang does not read as a number"},
		{"int f(int n) { static int a[n]; return 0; } int main(void) { return f(2); }", "object of incomplete type"},
		{"int f(int n) { typedef int T[n]; T a; return 0; } int main(void) { return f(2); }", "variable-length type"},
		{"int f(int n) { for (int a[n]; n; n--) ; return 0; } int main(void) { return f(2); }", "variable-length array in a for declaration"},
		{"int f(int n) { goto l; { int a[n]; l: a[0] = 1; return a[0]; } } int main(void) { return f(2); }", "jump into the scope of a variable-length array"},
		{"int f(int n) { int a[n]; a[0] = 1; goto l; { int b[n]; l: b[0] = 2; return a[0] + b[0]; } } int main(void) { return f(2); }", "jump into the scope of a variable-length array"},
		{"int f(int n) { int a[n]; void *p = &&l; goto *p; l: return 0; } int main(void) { return f(2); }", "computed goto in the scope of a variable-length array"},
		{"int main(void) { return __builtin_classify_type(1); }", "builtin __builtin_classify_type"},
		{"int main(void) { asm (\"nop\"); return 0; }", "asm with a template"},
		{"int main(void) { int x; asm (\"\" : \"=r\" (x)); return x; }", "asm output that no input gives a value"},
		{"int main(void) { return __builtin_imaxabs(-1); }", "builtin __builtin_imaxabs"},
		{"int main(void) { return __builtin_nan(\"0x1g\") != 0; }", "__builtin_nan of a string clang does not read as a number"},
		{"int main(void) { return __builtin_signbitl(1.0); }", "__builtin_signbitl of double"},
		{"int main(void) { return __builtin_mul_overflow_p(1, 2, 0); }", "builtin __builtin_mul_overflow_p"},
		{"int f(int n, char (*p)[n], char (*q)[n]) { return (int)(q - p); } int main(void) { return 0; }", "difference of pointers to a variable-length array"},
		{"int f(int n, char (*p)[n]) { p += 1; return p[0][0]; } int main(void) { return 0; }", "compound assignment of a pointer to a variable-length array"},
		{"int f(int n, char (*p)[n]) { p++; return p[0][0]; } int main(void) { return 0; }", "increment of a pointer to a variable-length array"},
		{"int f(int n, char (*p)[n]) { --p; return p[0][0]; } int main(void) { return 0; }", "increment of a pointer to a variable-length array"},
		{"int f(int n, void *v) { char (*p)[n] = v; return p[1][0]; } int main(void) { return 0; }", "pointer to a variable-length array that is not a parameter's"},
		{"int f(int n, void *v) { return ((char (*)[n])v)[1][0]; } int main(void) { return 0; }", "cast to a pointer to a variable-length array"},
		{"int main(void) { return (int)__builtin_return_address(0); }", "builtin __builtin_return_address"},
		{"int f(); int main(void) { return f(1); }", "call with arguments of a function declared without them in another unit"},
		{"union U { int i; char *p; }; int main(void) { union U u = (union U)1.5; return u.i; }", "cast of double to a union without a member of its type"},
		{"union U { int i; long double d; }; int main(void) { union U u = (union U)1.5L; return u.i; }", "cast of long double to a union"},
		// A refusal inside a cast to a union or a statement expression is
		// the construct's: a scalar member, an aggregate member, a scalar
		// value, an aggregate value.
		{"union U { int i; }; int main(void) { union U u = (union U)__builtin_classify_type(1); return u.i; }", "builtin __builtin_classify_type"},
		{"union U { int i; char *p; }; union V { union U u; }; int main(void) { union V v = (union V)(union U)1.5; return v.u.i; }", "cast of double to a union without a member of its type"},
		{"int main(void) { return ({ __builtin_classify_type(1); }); }", "builtin __builtin_classify_type"},
		{"union U { int i; char *p; }; int main(void) { union U d; d = ({ (union U)1.5; }); return d.i; }", "cast of double to a union without a member of its type"},
	} {
		_, err := compile(t, f, tc.src)
		var u *wasmgen.Unsupported
		if !errors.As(err, &u) || u.What != tc.what {
			t.Errorf("%q: %v, want unsupported %q", tc.src, err, tc.what)
		}
	}
	obj, err := compile(t, f, "int g = 3; int f(int a) { return a * g; } int main(void) { return f(2) + 1; }")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	if _, err := rt.CompileModule(ctx, obj); err != nil {
		t.Errorf("the object is not a valid module: %v", err)
	}
}

// TestIrreducibleBound: each loop entered at more than one block costs a
// dispatch block, and a function may need at most 64 of them; past that it
// is refused rather than compiled at any size. 64 loops each entered by a
// goto compile, 65 do not.
func TestIrreducibleBound(t *testing.T) {
	f, err := cparse.New("", "")
	if err != nil {
		t.Fatal(err)
	}
	src := func(n int) string {
		var b strings.Builder
		b.WriteString("int f(int x) { int i = 0;\n")
		for k := range n {
			fmt.Fprintf(&b, "if (x) goto in%d; while (i < 10) { i += 2; in%d: i++; }\n", k, k)
		}
		b.WriteString("return i; }\nint main(void) { return f(1); }\n")
		return b.String()
	}
	if _, err := compile(t, f, src(64)); err != nil {
		t.Errorf("64 irreducible loops: %v", err)
	}
	_, err = compile(t, f, src(65))
	var u *wasmgen.Unsupported
	if !errors.As(err, &u) || u.What != "irreducible control flow beyond the limit" {
		t.Errorf("65 irreducible loops: %v, want the bound", err)
	}
}

// TestCompileSeeds: every seed of FuzzCompile reaches the generator and is
// compiled. The fuzz target skips what the front end or the generator
// refuses, so a seed that stopped compiling would protect nothing, silently.
func TestCompileSeeds(t *testing.T) {
	front, err := cparse.New("", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range compileSeeds {
		ast, err := front.Translate("t.c", []byte(s))
		if err != nil {
			t.Errorf("%q: front end: %v", s, err)
			continue
		}
		if _, err := wasmgen.Compile(ast, "t.c"); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
}

var compileSeeds = []string{
	"int main(void) { return 0; }",
	// Complex numbers.
	"double _Complex f(double _Complex a, float _Complex b) { a *= b; return a / 2.0 + ~b - 1.0i; } int main(void) { double _Complex z = f(1.0 + 2.0i, 3.0f); return (int)__real__ z + (z != 0) + !z; }",
	// Irreducible loops (dispatch blocks) and labels as values.
	"int f(int x) { int i = 0, a = 0; if (x) goto in; while (i < 10) { i += 2; in: i++; if (i & 4) goto B; } A: a++; if (a < 3) goto B; return i + a; B: a += 2; goto A; } int main(void) { return f(1); }",
	"int f(int k) { static void *t[] = { &&a, &&b }; int r = 0; goto *t[k & 1]; a: r++; if (r < 5) goto *t[r & 1]; return r; b: r += 2; goto a; } int main(void) { return f(1) + ({ int q = 2; q * 3; }); }",
	"long double g = 0.1L; long double f(long double x, int n, ...) { long double r = x * 2 + n; r += g; return r > 1 ? r : -r; } int main(void) { return (int)f(1.5L, 2) + !g; }",
	"float f = 1.5f; double g(double d, float x) { return -d * x + (int)d; } int main(void) { double d = f; d /= 3; return g(d, 0.25f) < 1 && !d ? (unsigned)d : (long long)2.5e10; }",
	"int main(void) { return 1 << 31; }",
	"int main(void) { return (char)300; }",
	"int main() { return sizeof(long long); }",
	"int x; int main(void) { return x; }",
	"unsigned char c = 200; long long f(short a, unsigned b) { return a * (long long)b; } int main(void) { c += f(-3, 7); return c++ + --c; }",
	"void abort(void); static int s = -2; int main(void) { s <<= 3; exit(s); abort(); }",
	"long long v(int n, ...) { __builtin_va_list ap; long long t = 0; __builtin_va_start(ap, n); while (n--) t += __builtin_va_arg(ap, long long); __builtin_va_end(ap); return t; } int main(void) { return v(2, 1LL, 2LL) + v(0); }",
	"struct P { int a; long long b; } g = {1, 2}; union U { char c; } u; struct P f(struct P p, union U v) { p.a += v.c; return p; } int main(void) { struct P q = f(g, u), *r = &q; return (q = g).a + r->b; }",
}

// FuzzCompile: the generator is fed the AST of untrusted text. It may
// refuse anything; it must not panic, and what it accepts is a valid module.
func FuzzCompile(f *testing.F) {
	front, err := cparse.New("", "")
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range compileSeeds {
		f.Add([]byte(s))
	}
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 4096 {
			return
		}
		ast, err := front.Translate("t.c", src)
		if err != nil {
			return
		}
		obj, err := wasmgen.Compile(ast, "t.c")
		var u *wasmgen.Unsupported
		if errors.As(err, &u) {
			return
		}
		if err != nil {
			t.Fatalf("%q: an error that is not *Unsupported: %v", src, err)
		}
		if _, err := rt.CompileModule(ctx, obj); err != nil {
			t.Fatalf("%q: invalid module: %v", src, err)
		}
	})
}

// harness compiles a program both ways and runs both in the same wazero.
type harness struct {
	t     testing.TB
	tc    *oracle.Toolchain
	front *cparse.Front
	r     *oracle.Runner
}

func newHarness(t *testing.T) *harness {
	tc := oracletest.Require(t)
	ctx := t.Context()
	inc, err := tc.ResourceInclude(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f, err := cparse.New(inc, tc.Sysroot)
	if err != nil {
		t.Fatal(err)
	}
	r, err := oracle.NewRunner(ctx, oracle.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(context.Background()) })
	return &harness{t: t, tc: tc, front: f, r: r}
}

func (h *harness) ours(src string) (oracle.Result, error) {
	ctx := context.Background()
	obj, err := compile(h.t, h.front, src)
	if err != nil {
		return oracle.Result{}, err
	}
	mod, err := h.tc.Link(ctx, obj)
	if err != nil {
		return oracle.Result{}, fmt.Errorf("link: %w", err)
	}
	return h.r.Run(ctx, mod, nil, nil)
}

// mixed links our object for src with clang's for other, and runs it: a
// call across the two is a call across the two implementations of the ABI.
func (h *harness) mixed(src, other string) (oracle.Result, error) {
	ctx := context.Background()
	obj, err := compile(h.t, h.front, src)
	if err != nil {
		return oracle.Result{}, err
	}
	theirs, err := h.tc.Compile(ctx, []byte(other), oracle.CompileOptions{Extra: []string{"-c"}})
	if err != nil {
		return oracle.Result{}, err
	}
	mod, err := h.tc.Link(ctx, obj, theirs)
	if err != nil {
		return oracle.Result{}, fmt.Errorf("link: %w", err)
	}
	return h.r.Run(ctx, mod, nil, nil)
}

func (h *harness) theirs(src string) (oracle.Result, error) {
	return h.tc.Output(context.Background(), h.r, []byte(src), oracle.CompileOptions{})
}

func same(a, b oracle.Result) bool {
	return a.Exit == b.Exit && a.Trap == b.Trap && a.TimedOut == b.TimedOut &&
		bytes.Equal(a.Stdout, b.Stdout) && bytes.Equal(a.Stderr, b.Stderr)
}

// check compiles and runs src both ways and reports a difference.
func (h *harness) check(src string) {
	h.t.Helper()
	a, err := h.ours(src)
	if err != nil {
		h.t.Errorf("%s\nours: %v", src, err)
		return
	}
	b, err := h.theirs(src)
	if err != nil {
		h.t.Errorf("%s\nclang: %v", src, err)
		return
	}
	if !same(a, b) {
		h.t.Errorf("%s\nours  %+v\nclang %+v", src, a, b)
	}
}

// TestAgainstClang: each program exits as clang's does, in the same wazero.
// The control pairs our program with clang's for another value: the
// harness must see the difference.
func TestAgainstClang(t *testing.T) {
	h := newHarness(t)
	for _, src := range []string{
		"int main(void) { return 0; }",
		"int main(void) { return 2 * 21; }",
		"int main(void) { return -1; }",
		// globals: initialised, tentative, static, and the prototype-less
		// K&R call of exit that clang types from the library.
		"int g = 7; int z; static short s = -2;\nint add(int a, int b) { return a + b; }\nint main(void) { z = add(g, s); return z * 10 + s; }",
		"int main() { exit(7); }",
		"void abort(void); int main(void) { abort(); return 1; }",
		// conversions at every width
		"int main(void) { signed char c = 200; unsigned char u = -1; short s = 70000; unsigned short us = -3; return c + u + s + us; }",
		"int main(void) { long long a = 0x123456789LL; unsigned long long b = -1; int lo = a; unsigned hi = a >> 32; return lo ^ hi ^ (int)(b >> 33); }",
		"int main(void) { _Bool b = 256; _Bool c = 0.0 == 0; unsigned char k = b + c; return k * 3 + (b != 0); }",
		// compound assignment, increments, comma
		"unsigned g; int main(void) { unsigned x = 5; x *= 7; x -= 3; x <<= 2; x >>= 1; x %= 11; g = x; g++; ++g; g--; return (x++, x) + g; }",
		"int main(void) { signed char c = 127; c += 1; unsigned char u = 255; u++; return c * 1000 + u; }",
		"long long f(long long a, int b) { return a / b + a % b; } int main(void) { return f(-1000000000000LL, 7) & 0xffff; }",
		"int main(void) { int a = -7; unsigned b = 3; return (a / 2) * 100 + (a % 2) * 10 + (a < b) + (a >> 1); }",
		// W3: control flow
		"int f(int n) { if (n < 2) return n; return f(n - 1) + f(n - 2); } int main(void) { return f(20); }",
		"int main(void) { int s = 0, i; for (i = 0; i < 100; i++) { if (i % 3 == 0) continue; if (i > 90) break; s += i; } return s; }",
		"int main(void) { int n = 27, k = 0; while (n != 1) { n = n & 1 ? 3 * n + 1 : n / 2; k++; } return k; }",
		"int main(void) { int i = 0, s = 0; do { s = s * 3 + i; } while (++i < 9); return s; }",
		"int g(int x) { switch (x) { case 0: return 10; case 1: case 2: x += 5; case 3: return x * 2; default: break; case -1: return 99; } return x; }\nint main(void) { return g(0) + g(1) * 3 + g(2) * 5 + g(3) * 7 + g(-1) + g(42); }",
		"int calls; int t(int v) { calls++; return v; } int main(void) { int a = t(0) && t(1); int b = t(1) || t(0); int c = t(1) && t(2); t(0) || t(3); return a + b * 2 + c * 4 + calls * 8; }",
		"int main(void) { int i, j, s = 0; for (i = 0; i < 10; i++) for (j = 0; j < 10; j++) { if (j > i) break; s += i * j; } return s; }",
		"int main(void) { long long x = 0; int i; for (i = 0; i < 64; i++) x += (i & 1) ? -((long long)i << i % 40) : i; return (int)(x ^ x >> 32); }",
		"void abort(void); int main(void) { unsigned u = 5; if (u - 6 < 0) abort(); if (!(u > 4)) abort(); return 0; }",
		// the four gcc torture files the first W3 census got wrong
		"enum foo { FOO, BAR }; int main(void) { int i, n = 0; for (i = BAR; i >= FOO; --i) n++; return n * 10 + (FOO - 1 < 0) + sizeof(FOO); }",
		"int f(void) { return 26; }\n#line 1 \"renamed.c\"\nint main(void) { return f(); }",
		"void abort(void); long long a = -7; long long llabs(long long); int main(void) { return (int)llabs(a) + abs(-3) * 10; } long long llabs(long long b) { abort(); }",
		"void f(void) __attribute__((aligned(256))); void f(void) {} void g(void) {} int x __attribute__((aligned(64))); long long ll;\nint main(void) { return __alignof__(f) + __alignof__(g) * 1000 + __alignof__(x) * 100000 + __alignof__(ll) * 10000000; }",
		// W4a: memory
		"int g[4] = {1, 2, 3, 4}; int *gp = &g[2]; const char *s = \"hi\";\nstatic void set(int *p, int v) { *p = v; }\nint main(void) { int x = 5; int a[3]; set(&x, 7); a[1] = x + *gp; return a[1] + s[1] + g[3]; }",
		"int sum(int *p, int n) { int s = 0; while (n--) s += *p++; return s; } int main(void) { int a[10], i; for (i = 0; i < 10; i++) a[i] = i * i; return sum(a, 10) + sum(a + 3, 4) * 1000; }",
		"int f(int x) { int *p = &x; *p += 4; return x; } int main(void) { return f(38); }",
		"int main(void) { char s[8] = \"abc\"; char *p = s; int n = 0; while (*p) n = n * 256 + *p++; return n + s[5] + sizeof s; }",
		"int m[3][4]; int main(void) { int i, j, t = 0; for (i = 0; i < 3; i++) for (j = 0; j < 4; j++) m[i][j] = i * 10 + j; for (i = 0; i < 3; i++) t += m[i][3 - i]; return t + (&m[2][1] - &m[0][0]); }",
		"int a[] = {5, 6, 7}; int *b[3] = {&a[2], a, &a[1]}; int main(void) { return *b[0] * 100 + *b[1] * 10 + b[2][0] + (b[0] - b[1]) * 1000; }",
		"static char *msg = \"hello\"; int main(void) { char *p = msg; unsigned h = 0; while (*p) h = h * 31 + *p++; return h + (p - msg); }",
		"int main(void) { int a[4] = {1, 2}, *p = a, *q = &a[3]; long long ll[2] = {-1LL, 1LL << 40}; return (q > p) + (p == a) * 2 + a[3] * 4 + (int)(ll[1] >> 38) * 8 + (ll[0] < 0) * 64; }",
		"void abort(void); int g; int *get(void) { return &g; } int main(void) { *get() = 9; ++*get(); (*get())++; if (g != 11) abort(); return g; }",
		// W4b: structures and unions
		"struct pt { int x, y; }; struct pt g = {3, 4}, h[2] = {{1, 2}, {5, 6}};\nint main(void) { struct pt a = g, *p = &h[1]; a.x += p->y; p->x = a.y; h[0] = *p; return a.x * 1000 + h[0].x * 100 + h[0].y * 10 + h[1].x; }",
		"struct s { char tag; int v[3]; struct { short lo, hi; } r; };\nint sum(struct s *p) { int i, t = p->tag; for (i = 0; i < 3; i++) t += p->v[i]; return t + p->r.lo - p->r.hi; }\nint main(void) { struct s a = {1, {2, 3, 4}, {50, 6}}, b, c; int *q = a.v; c = b = a; q[1] = 30; return sum(&a) * 1000 + sum(&c) + (int)sizeof(struct s); }",
		"union u { int i; unsigned char b[4]; short h[2]; }; int main(void) { union u x; x.i = 0x01020304; return x.b[0] * 1000 + x.b[3] * 100 + (x.h[1] == 0x0102) + sizeof x; }",
		"struct v { long long a; int b; };\nstruct v pick(int c, struct v x, struct v y) { return c ? x : y; }\nint main(void) { struct v x = {1LL << 40, 2}, y = {3, 4}, z; z = pick(0, x, y); x = pick(1, x, y); return (int)(x.a >> 38) + z.b * 10 + ((void)0, x).b * 100 + (z = x, z).b * 1000 + (z = y).b * 10000; }",
		"struct n { int v; struct n *next; }; int main(void) { struct n c = {3, 0}, b = {2, &c}, a = {1, &b}; struct n *p; int t = 0; for (p = &a; p; p = p->next) t = t * 10 + p->v; return t; }",
		"struct a { int k; union { int i; char c; }; struct { short s; }; }; int main(void) { struct a x; x.k = 1; x.i = 0x141; x.s = 7; return x.k + x.c * 10 + x.s * 1000 + sizeof x; }",
		"struct m { const char *s; int n; } t[] = {{\"ab\", 1}, {\"cde\", 2}}; int main(void) { int i, r = 0; for (i = 0; i < 2; i++) r = r * 100 + t[i].s[t[i].n] * t[i].n; return r; }",
		// pointers to arrays, which cc/v4 types already decayed (strlen-4.c)
		"typedef char A5[5]; typedef A5 A2[2]; A2 a[2] = {{\"ab\", \"cd\"}, {\"ef\", \"gh\"}};\nint main(void) { return (*(&a[0][0] + 1))[1] * 100000 + (*&a[1][1])[1] * 1000 + ((char *)(&a[0][0] + 1) - (char *)a) * 10 + (int)sizeof(*&a[0][0]); }",
		"char a[3][2][5] = {{\"ab\", \"cd\"}, {\"ef\", \"gh\"}}; char (*p)[5] = &a[1][1]; char *q = a[1][1]; char (*r)[5] = &a[0][1];\nint main(void) { return (*p)[1] * 1000 + q[0] + (*r)[1] * 1000000; }",
		// calls without a prototype, with more or fewer arguments (920501-1.c)
		"int s[2]; int calls;\nx(){if(!s[0]){s[1+s[1]]=s[1];return 1;}}\ny(a, b) int a, b; { return a * 10 + b; }\nint t(int v) { calls++; return v; }\nmain(){s[0]=s[1]=0;if(x(t(0), t(1))!=1)abort();return calls * 100 + y(4, 2);}",
		// function pointers: in data, in locals, through structures, as
		// arguments, compared, called through (*p) and p
		"struct op { const char *name; int (*fn)(int, int); };\nstatic int add(int a, int b) { return a + b; } int mul(int a, int b) { return a * b; } long long big(long long x) { return x << 33; }\nstruct op ops[] = {{\"add\", add}, {\"mul\", &mul}}; int (*gp)(int, int) = mul; void *vp = (void *)add;\nint apply(int (*f)(int, int), int x) { return f(x, x + 1); }\nint main(void) { int (*p)(int, int) = add, i, t = 0; long long (*q)(long long) = big; for (i = 0; i < 2; i++) t = t * 100 + ops[i].fn(3, 4); t += (*p)(1, 2) * 10000 + apply(mul, 5) + apply(gp, 2) * 1000000; return t + (p == add) + (ops[1].fn == mul) * 2 + (vp == (void *)add) * 4 + (int)(q(1) >> 33) * 8 + (p != gp) * 16; }",
		// W7: goto
		"int f(int n) { int i = 0, s = 0; again: if (i >= n) goto done; s += i * i; i++; goto again; done: return s; }\nint g(int a) { int i, j, k = 0; for (i = 0; i < 10; i++) for (j = 0; j < 10; j++) { k += i ^ j; if (k > a) goto out; } return -1; out: return i * 100 + j; }\nint h(int x) { switch (x) { case 0: goto two; case 1: x += 10; two: x *= 3; break; default: goto end; } x++; end: return x; }\nint main(void) { return f(10) + g(150) * 1000 + h(0) + h(1) * 10 + h(5) * 100; }",
		// compound literals (20000722-1.c, 20010123-1.c), in a function and
		// at file scope
		"struct s { char *p; int t; }; int calls;\nvoid foo(struct s *p) { calls = calls * 10 + p->t; p->t = 2; }\nvoid bar(void) { foo(&(struct s){\"hi\", 1}); }\nstruct s *gp = &(struct s){\"gl\", 7}; int *ga = (int[3]){4, 5, 6};\nint main(void) { int i, k = 0; bar(); bar(); for (i = 0; i < 3; i++) { int *q = (int[2]){i, i * 2}; struct s *t = &(struct s){.t = 3}; k = k * 10 + q[1] + t->t + (t->p == 0); t->t = 9; t->p = \"x\"; } return calls + k * 100 + gp->t * 1000000 + ga[2] * 10000000 + (int){3} + gp->p[1] + ((struct s){0, 8}).t; }",
		// library functions called without a declaration (cc/v4 resolves
		// them to its builtins)
		"int main() { char b[16], *p; memset(b, 'x', 15); b[15] = 0; memcpy(b + 2, \"abc\", 3); p = malloc(8); strcpy(p, \"hey\"); strcat(p, \"!\"); puts(b); printf(\"%d %d %d %s %d %d\\n\", (int)strlen(b), strcmp(p, \"hey!\"), memcmp(b, \"xxab\", 4) < 0, strchr(b, 'c'), (int)strspn(b, \"x\"), strncmp(p, \"he\", 2)); free(p); putchar('\\n'); return __builtin_strlen(\"four\"); }",
		// W6: bit-fields, with the bytes of the objects printed so that the
		// layout shows
		"#include <stdio.h>\nstruct S { unsigned a:3; int b:5; unsigned c:1; signed char d:4; long long e:40; unsigned long long f:44; _Bool g:1; int h:16; int i:7; };\nstruct S gs = {5, -3, 1, -8, -123456789012LL, 0xabcdef12345ULL, 1, -2, 63};\nvoid dump(void *o, int n) { unsigned char *p = o; while (n--) printf(\"%02x\", *p++); printf(\"\\n\"); }\nint main(void) {\n int x = 2; struct S s = {1, x, 0, 3, 4, 5, 0, 6, 7};\n s.a += 6; s.b = s.b * 9; s.c++; s.d -= 9; s.e <<= 3; s.f = -1; s.g = 5; s.i = (s.h = 300) + 100;\n int v = (s.b = 100), w = s.c--;\n printf(\"%u %d %u %d %lld %llx %d %d %d %d %d\\n\", s.a, s.b, s.c, s.d, s.e, s.f, s.g, s.h, s.i, v, w);\n dump(&s, sizeof s);\n printf(\"%u %d %u %d %lld %llx %d %d %d %d\\n\", gs.a, gs.b, gs.c, gs.d, gs.e, gs.f, gs.g, gs.h, gs.i, (int)sizeof gs);\n dump(&gs, sizeof gs);\n return 0;\n}",
		"#include <stdio.h>\nstruct T { short i : 12; char c1 : 1; char c2 : 1; char c3 : 1; char c4 : 1; } t0 = {341, 1, 1, 1, 1};\nunion U { unsigned a:4; unsigned char b; };\nstruct P { struct T t; unsigned k:20; } gp = {{-1, 0, 1, 0, -1}, 0xfffff};\nint main(void) { struct T t = {341, 1, 1, 1, 1}; union U u; struct P p = gp; u.b = 0xff; u.a = 0; p.t.i += 2; p.k >>= 4; printf(\"%d %d %d %x %d %x %d\\n\", *(char *)&t, *(char *)&t0, t.c4, u.b, p.t.i, p.k, sizeof(struct P)); return t.c1 + t.c2 * 2; }",
		// W5: float and double, printed with %a so that every bit shows
		"#include <stdio.h>\nint main(void) {\n float f = 1.1f; double d = 1.1; float g = d; double h = f;\n printf(\"%a %a %a %a\\n\", f, d, g, h);\n printf(\"%a %a %a %a\\n\", f * 3.0f, d / 3, f + d, f / 3);\n int i = d * 10; unsigned u = 3.9; long long ll = -2.5e10; unsigned long long ull = 1e19; signed char c = -3.7; unsigned short us = 65535.5;\n printf(\"%d %u %lld %llu %d %d\\n\", i, u, ll, ull, c, us);\n printf(\"%a %a %a %a\\n\", (double)-7, (float)4000000001u, (double)(1ULL << 63), (float)-9007199254740993LL);\n _Bool b = 0.5; _Bool z = -0.0;\n printf(\"%d %d %a %a %a\\n\", b, z, -0.0, -d, -(0.0f));\n return (f < d) + (g == f) * 2;\n}",
		"#include <stdio.h>\nint main(void) { double z = 0, n = z / z, inf = 1 / z; int c = 0; if (n) c += 1; if (!n) c += 2; c += (n == n) * 4 + (n != n) * 8 + (n < 1) * 16 + (inf > 1e308) * 32 + (n ? 64 : 128) + (z == -z) * 256 + (n >= n || n <= n) * 512; printf(\"%f %f %d %d\\n\", inf, -inf, c, z && n); return 0; }",
		"#include <stdio.h>\nstruct V { float x; }; struct W { double a, b; };\nfloat scale(float x, double k) { return x * k; }\nstruct V mkv(float x) { struct V v = {x}; return v; }\nstruct W mkw(double a) { struct W w = {a, a / 2}; return w; }\nstatic double tab[3] = {0.5, 1.0 / 3, 2e-310}; float gf = 1.0 / 3; int gi = 7.9; double gd = 'A'; float gff = 0.1f;\nint main(void) { double s = 0; int i; float f = 0; for (i = 0; i < 3; i++) s += tab[i]; f += 0.1f; f *= 3; f -= 1; f /= 7; f++; --f; printf(\"%a %a %a %a %d %a %a\\n\", s, f, scale(1.5f, 1.0 / 3), gf, gi, gd, gff); printf(\"%a %a %a %.17g\\n\", mkv(2.5f).x, mkw(3).b, mkw(1e300).a * 10, 0.1 + 0.2); return 0; }",
		"#include <stdarg.h>\n#include <stdio.h>\ndouble avg(int n, ...) { va_list ap; double t = 0; int k = n; va_start(ap, n); while (k--) t += va_arg(ap, double); va_end(ap); return t / n; }\nint main(void) { float f = 0.75f; printf(\"%a %a %g\\n\", avg(3, 1.0, f, 2.5), avg(1, (double)3), 1e-5); return 0; }",
		// W4d: static locals, main(argc, argv)
		"int next(void) { static int n = 40; static int *p = &n; return (*p)++; }\nint other(void) { static int n; return n++; }\nint main(void) { int a = next(), b = next(), c = other(); { static char s[] = \"xy\"; s[0]++; c += s[0]; } return a * 1000 + b * 10 + c + other() * 100000; }",
		"struct S { int a; long long b; }; struct S *get(void) { static struct S s = {1, 2}; s.a++; return &s; }\nint main(void) { get(); return get()->a * 10 + (int)get()->b; }",
		// a string literal added while a static initializer is written
		// moved the segments under it (pr53084.c)
		"int main(void) { static const char *const q[] = {\"foo\" + 1, \"x\"}; static const char *p = \"baz\"; return q[0][0] * 1000 + q[1][0] + p[2] * 100000; }",
		"int x; const char *p; const char *p = \"bar\" + 1; int main(void) { return p[0]; }",
		"int main(int argc, char **argv) { return argc * 100 + (argv[1] == 0) * 10 + (argv[0][0] != 0); }",
		"int main(int argc, char *argv[]) { if (argc > 1) return main(argc - 1, argv) + 1; return 40; }",
		// W4c: variadic functions
		"#include <stdarg.h>\nint sum(int n, ...) { va_list ap; int t = 0; va_start(ap, n); while (n--) t += va_arg(ap, int); va_end(ap); return t; }\nint main(void) { return sum(4, 1, 2, 3, 40) * 10 + sum(0); }",
		"#include <stdarg.h>\nint vsum(int n, va_list ap) { int t = 0; while (n--) t = t * 10 + va_arg(ap, int); return t; }\nint two(int n, ...) { va_list ap, aq; int a, b; va_start(ap, n); va_copy(aq, ap); a = vsum(n, ap); b = vsum(n, aq); va_end(aq); va_end(ap); return a + b * 1000; }\nint main(void) { return two(3, 1, 2, 3); }",
		"#include <stdio.h>\nint main(void) { int n = printf(\"%d %s %lld %c|\\n\", 42, \"str\", -(1LL << 40), 'x'); return n; }",
		"int main() { printf(\"%u-%hhd\\n\", 4000000000u, 300); return 0; }",
		// builtins: alloca (5000 calls of 1000 bytes overflow the 1 MiB
		// stack unless each return frees them), the overflow builtins at
		// every width, the quiet comparisons, the bit counts
		builtinsProgram,
		builtinsProgram2,
		staticInitProgram,
		asmProgram,
		modeProgram,
		packedProgram,
		bitfieldLayoutProgram,
		bitfieldTypeProgram,
		stmtExprProgram,
		labelProgram,
		irreducibleProgram,
		complexProgram,
		intComplexProgram,
		ldComplexProgram,
		vectorProgram,
		aggValueProgram,
		int128Program,
		vlaProgram,
		vlaParamProgram,
		// __func__ (C11 6.4.2.2): each function's own, printed, indexed, its
		// size, its address stable across calls
		"#include <stdio.h>\nstatic const char *who(void) { return __func__; }\nint main(void) { printf(\"%s %s %c %zu %d\\n\", __func__, who(), __func__[1], sizeof __func__, who() == who()); return 0; }",
		// long double (binary128): storage, parameters, results, every
		// operation, comparison and conversion, varargs
		quadProgram,
		quadProgram2,
	} {
		h.check(src)
	}
	a, err := h.ours("int main(void) { return 3; }")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.theirs("int main(void) { return 4; }")
	if err != nil {
		t.Fatal(err)
	}
	if same(a, b) {
		t.Error("control: the harness did not tell exit 3 from exit 4")
	}
}

const builtinsProgram = `#include <stdio.h>
#include <limits.h>
int grow(int n) { char *p = __builtin_alloca(n); int i; for (i = 0; i < n; i++) p[i] = i; return p[n - 1]; }
int deep(int n) { char *p = __builtin_alloca(n + 1); int r; p[n] = n; r = n ? deep(n - 1) : 0; return r + p[n]; }
int main(void) {
	long long ll; int i, j, k = 0; unsigned u; short s; unsigned char uc; unsigned long long ull;
	double z = 0, n = z / z; float f = 1.5f;
	for (j = 0; j < 3; j++) { char *q = __builtin_alloca(10 * j + 1); q[0] = j; k += q[0]; }
	for (j = 0; j < 5000; j++) k += grow(1000) - 231;
	printf("%d %d\n", deep(20), k);
	printf("%d %d %d ", __builtin_add_overflow(INT_MAX, 1, &i), i, __builtin_add_overflow(-1, 1u, &u));
	printf("%u %d %d\n", u, __builtin_sub_overflow(0u, 1, &i), i);
	printf("%d %d %d %d ", __builtin_mul_overflow(1 << 20, 1 << 12, &i), i, __builtin_mul_overflow(-3, 4, &s), s);
	printf("%d %u %d %u\n", __builtin_mul_overflow(200, 2, &uc), uc, __builtin_mul_overflow(4000000000u, 4000000000u, &u), u);
	printf("%d %lld ", __builtin_mul_overflow(LLONG_MAX, 2LL, &ll), ll);
	printf("%d %lld ", __builtin_mul_overflow(LLONG_MIN, -1LL, &ll), ll);
	printf("%d %llu ", __builtin_mul_overflow(ULLONG_MAX, ULLONG_MAX, &ull), ull);
	printf("%d %d\n", __builtin_mul_overflow(0xffffffffffULL, 3, &i), i);
	printf("%d %lld ", __builtin_add_overflow(ULLONG_MAX, LLONG_MIN, &ll), ll);
	printf("%d %llu ", __builtin_sub_overflow(0LL, LLONG_MIN, &ull), ull);
	printf("%d %lld ", __builtin_sub_overflow(0LL, LLONG_MIN, &ll), ll);
	printf("%d %llu\n", __builtin_mul_overflow(-1, ULLONG_MAX, &ull), ull);
	printf("%d %d %d %d ", __builtin_isunordered(n, 1.0), __builtin_isunordered(f, 2.0), __builtin_isgreater(f, 1), __builtin_isless(n, 1.0));
	printf("%d %d %d %d\n", __builtin_islessgreater(f, 1.5), __builtin_islessequal(f, 1.5), __builtin_isgreaterequal(n, n), __builtin_islessgreater(f, 2.0));
	printf("%d %d %d %d %d %d\n", __builtin_clz(1), __builtin_ctz(8), __builtin_popcount(0xff0f), __builtin_clzll(1), __builtin_ctzll(1ULL << 40), __builtin_popcountl(-1L));
	__builtin_prefetch(&k, 0, 3);
	if (__builtin_expect(k == 12345, 0)) __builtin_unreachable();
	return (int)__builtin_expect(k, 0) & 0x7f;
}
`

// builtinsProgram2: the floating-point and byte builtins, printed with %a
// so that every bit shows, and the library functions called by their
// __builtin_ names.
const builtinsProgram2 = `#include <stdio.h>
int main(void) {
	double z = 0, n = __builtin_nan(""), m = -n, d = -2.5; float f = -0.0f, nf = __builtin_nanf("");
	char b[16] = "abcdefgh", c[16];
	unsigned long long w = 0x0102030405060708ULL;
	printf("%a %a %a %a %a %a\n", __builtin_fabs(d), __builtin_fabsf(f), __builtin_copysign(3, -0.0), __builtin_copysignf(2, nf), __builtin_inf(), (double)__builtin_huge_valf());
	printf("%d %d %d %d %d %d %d %d\n", __builtin_isinf(1 / z), __builtin_isinf(-1 / z), __builtin_isinf(n), __builtin_isfinite(d), __builtin_isfinite(n), __builtin_isfinite(__builtin_inff()), __builtin_isnan(n), __builtin_isnan(f));
	printf("%d %d %d %d %d %d %d\n", __builtin_signbit(f), __builtin_signbit(d), __builtin_signbit(z), __builtin_signbit(m), __builtin_signbit(2.0f), __builtin_signbit(2.0), n != n);
	printf("%d %d %d %d %d\n", __builtin_ffs(0), __builtin_ffs(8), __builtin_ffs(-1), __builtin_ffsl(0x80000000), __builtin_ffsll(1ULL << 40));
	printf("%x %x %llx %x\n", __builtin_bswap16(0x1234), __builtin_bswap32(0x12345678), __builtin_bswap64(w), __builtin_bswap16(0xff01) + 1);
	__builtin_memmove(b + 2, b, 5);
	*(char *)__builtin_mempcpy(c, "xyz", 3) = 0;
	__builtin_stpcpy(c + 3, "!");
	__builtin_bzero(b + 8, 2);
	printf("%s %s %d %d\n", b, c, (int)((char *)__builtin_index(b, 'c') - b), (int)((char *)__builtin_rindex(b, 'c') - b));
	__builtin_sprintf(c, "%d-%s", 42, "q");
	__builtin_fprintf(stdout, "[%s]\n", c);
	__builtin_snprintf(c, 4, "%s", "truncated");
	puts(c);
	return 0;
}
`

// staticInitProgram: address constants of members (C11 6.6p9), and the
// floating builtins in static initializers, printed as their bits.
// modeProgram: __attribute__((mode)) gives each declaration the width of its
// mode and keeps the declared signedness: wraparound at 8, 16 and 64 bits,
// enumeration members of one byte, the alignment of members of each width,
// pointer arithmetic over the narrowed type, a floating mode.
const modeProgram = `#include <stdio.h>
typedef unsigned int __attribute__((mode(QI))) u8;
typedef signed int __attribute__((mode(QI))) s8;
typedef unsigned int u16 __attribute__((mode(HI)));
typedef int s16 __attribute__((__mode__(__HI__)));
typedef unsigned int __attribute__((mode(DI))) u64;
typedef int word __attribute__((mode(word)));
typedef double __attribute__((mode(SF))) f32;
enum E { A, B = 200 };
enum N { M = -100, P = 100 };
struct e { enum E __attribute__((mode(byte))) a, b; enum N __attribute__((mode(__byte__))) n; };
struct pk { u8 x; u64 y; u16 z; };
int main(void) {
	u8 a = 250, arr[4] = { 1, 2, 3, 4 }, *p = arr; s8 b = 120; u16 c = 65535; s16 d = -32768; u64 e = 1;
	struct e s = { A, B, M }; struct pk k = { 255, 0x0102030405060708ULL, 0xabcd }; f32 f = 1.0 / 3; word w = -1;
	a += 10; b += 10; c++; d--; e <<= 63;
	printf("%u %d %u %d %llu\n", a, b, c, d, (unsigned long long)e);
	printf("%zu %zu %zu %zu %zu %zu\n", sizeof(u8), sizeof(u16), sizeof(u64), sizeof(struct e), sizeof(struct pk), sizeof(word));
	s.b += 100; s.n -= 100;
	printf("%d %d %d %d\n", s.a, s.b, s.n, s.n < 0);
	p += 3; *p -= 5;
	printf("%u %d %u\n", *p, (int)(p - arr), arr[3] > 200);
	printf("%u %llx %x %a %d\n", k.x, (unsigned long long)k.y, k.z, (double)f, w < 0);
	return 0;
}
`

// packedProgram: packed structs and members at run time: the bytes of each
// layout, misaligned loads and stores of every width, an array of packed
// structs, a packed struct copied and passed by value, a union.
// bitfieldLayoutProgram: the Itanium bit-field layout (go-embedded-c/c#29),
// packed or not. For each struct: its size, alignment and the offsets of its
// ordinary members, then its bytes after each bit-field alone is set to all
// ones, then the value read back from each. Nothing in it is a constant
// taken from clang: the comparison is the whole output.
const bitfieldLayoutProgram = `#include <stdio.h>
#include <stddef.h>
#include <string.h>
static void dump(const void *v, size_t n) { const unsigned char *b = v; size_t i; for (i = 0; i < n; i++) printf("%02x", b[i]); printf(" "); }
#define SET(T, f) do { T s; memset(&s, 0, sizeof s); s.f = -1; dump(&s, sizeof s); printf("%lld|", (long long)s.f); } while (0)
#define HEAD(T) printf("\n%s %zu %zu: ", #T, sizeof(T), _Alignof(T))
struct s29 { unsigned long long f:44; _Bool g:1; short h; };
struct cross { char a:7; char b:3; int c:30; int d:4; long long e:40; long long g:33; };
struct zero { char a:3; int :0; char b:2; long long :0; char c; };
struct anon { char a; int :4; char b; };
struct mixed { char c; int x:5; short s; unsigned y:20; char t; };
struct __attribute__((packed)) pk { int g:31; int h:6; char c; };
struct pk2 { char a; unsigned long w:32 __attribute__((packed)); char b; };
struct __attribute__((packed)) pk3 { char a:3; long long l:60; short s:9; int :0; char z; };
struct pk4 { char c; struct __attribute__((packed)) { int a:13; int b:13; } in; char d; };
union u { char c; int x:3; long long y:33; };
union __attribute__((packed)) up { char c; int x:20; };
int main(void) {
	HEAD(struct s29); printf("%zu ", offsetof(struct s29, h)); SET(struct s29, f); SET(struct s29, g); SET(struct s29, h);
	HEAD(struct cross); SET(struct cross, a); SET(struct cross, b); SET(struct cross, c); SET(struct cross, d); SET(struct cross, e); SET(struct cross, g);
	HEAD(struct zero); printf("%zu ", offsetof(struct zero, c)); SET(struct zero, a); SET(struct zero, b); SET(struct zero, c);
	HEAD(struct anon); printf("%zu ", offsetof(struct anon, b)); SET(struct anon, a); SET(struct anon, b);
	HEAD(struct mixed); printf("%zu %zu ", offsetof(struct mixed, s), offsetof(struct mixed, t)); SET(struct mixed, x); SET(struct mixed, y); SET(struct mixed, s);
	HEAD(struct pk); printf("%zu ", offsetof(struct pk, c)); SET(struct pk, g); SET(struct pk, h); SET(struct pk, c);
	HEAD(struct pk2); printf("%zu ", offsetof(struct pk2, b)); SET(struct pk2, w); SET(struct pk2, b);
	HEAD(struct pk3); printf("%zu ", offsetof(struct pk3, z)); SET(struct pk3, a); SET(struct pk3, l); SET(struct pk3, s); SET(struct pk3, z);
	HEAD(struct pk4); printf("%zu %zu ", offsetof(struct pk4, in), offsetof(struct pk4, d)); SET(struct pk4, in.a); SET(struct pk4, in.b);
	HEAD(union u); SET(union u, x); SET(union u, y);
	HEAD(union up); SET(union up, x);
	{ struct pk p = { -5, 17, 'q' }; struct pk3 q = { 2, -3LL, 100, 'z' }; printf("\n%d %d %c %d %lld %d %c\n", p.g, p.h, p.c, q.a, (long long)q.l, q.s, q.z); }
	return 0;
}
`

// bitfieldTypeProgram: the type a bit-field has in expressions, by its
// declared type and width (clang isPromotableBitField: narrower than int,
// int; as wide, int or unsigned by the declared signedness; wider, the
// declared type), seen through its size and signedness, arithmetic, and the
// arguments it makes to `...`.
const bitfieldTypeProgram = `#include <stdio.h>
#define T(e) (int)(sizeof(e) * 10 + ((e) * 0 - 1 < 0))
struct s { long long a:24; unsigned long long b:31; long long c:32; unsigned long long d:32; long long e:33; unsigned long long f:40; long g:32; unsigned h:32; int i:7; };
int main(void) {
	struct s v = { -5, 0x7fffffff, -7, 0xfffffff0u, -9, 0xffffffffffULL, -11, 0xfffffff5u, -3 };
	printf("%d %d %d %d %d %d %d %d %d\n", T(v.a + 0), T(v.b + 0), T(v.c + 0), T(v.d + 0), T(v.e + 0), T(v.f + 0), T(v.g + 0), T(v.h + 0), T(v.i + 0));
	printf("%d %d %d %d %d\n", T(-v.a), T(~v.c), T(-v.d), T(-v.e), T(+v.g));
	printf("%d %d %d %u %lld %llu %d %u %d\n", v.a, v.b, v.c, v.d, v.e, v.f, v.g, v.h, v.i);
	printf("%d %u %lld %d\n", v.a * 2 - 1, v.d + 32u, v.e << 20, v.b > -1);
	v.a += 0x7fffff; v.c = v.c * 3; v.e -= 1; v.i = v.a;
	printf("%d %d %lld %d\n", v.a, v.c, v.e, v.i);
	return 0;
}
`

// stmtExprProgram: GNU statement expressions in every position an
// expression takes, with control flow inside them: an operand above values
// already on the stack, a short-circuited one, conditional arms, call
// arguments, an array index, a loop condition; loops, a switch, breaks and
// a return inside; nesting; a void one; the value converted to the
// construct's type. Calls print, so the order of evaluation shows.
const stmtExprProgram = `#include <stdio.h>
static int t(int v) { printf("<%d>", v); return v; }
static int f(int n) {
	int s = ({ int i, a = 0; for (i = 0; i < n; i++) { if (i == 3) continue; a += i; } a; });
	int w = 0;
	while (({ int k = w * w; k < 50; })) w++;
	return s * 100 + w;
}
static int early(int x) {
	int r = ({ if (x < 0) return -1; x * 2; });
	return r + 1;
}
int main(void) {
	int a[5] = { 10, 20, 30, 40, 50 }, i = 2, z = 0;
	long long big = 1LL << 40;
	char c;
	printf("%d %d %d\n", f(6), early(-3), early(4));
	printf("%d\n", t(1) + ({ int j, s = 0; for (j = 0; j < 4; j++) s += t(j); s; }) * t(2));
	printf("%d %d\n", z && ({ t(99); 1; }), z || ({ t(7); 0; }));
	printf("%d\n", i > 1 ? ({ int q = t(5); q * q; }) : ({ t(6); 0; }));
	printf("%d\n", a[({ int k = i; switch (k) { case 2: k = 4; break; default: k = 0; } k; })]);
	printf("%lld\n", ({ long long u = big; u + ({ int v = 3; v * v; }); }));
	c = ({ 300 + 1; });
	printf("%d\n", c);
	({ t(42); });
	({ for (i = 0; i < 3; i++) t(i); });
	printf("\n%d %d\n", ({ int x = 0; do { x += 7; } while (x < 30); x; }), i);
	return ({ int r = 0; r; });
}
`

// labelProgram: labels as values. A threaded interpreter whose opcode table
// is a static array of &&label; a table of label differences, jumped
// through as &&base + offset; label addresses held in variables, compared
// and passed around; two functions with labels of the same names.
const labelProgram = `#include <stdio.h>
static int run(const unsigned char *pc) {
	static const void *ops[] = { &&halt, &&inc, &&dbl, &&neg, &&jnz };
	int acc = 0, n = 0;
	goto *ops[*pc];
inc:	acc++; n++; goto *ops[*++pc];
dbl:	acc *= 2; n++; goto *ops[*++pc];
neg:	acc = -acc; n++; goto *ops[*++pc];
jnz:	n++; if (acc != 0 && acc > -40) { pc -= 3; } else { pc++; } goto *ops[*pc];
halt:	return acc * 1000 + n;
}
static int offsets(int i) {
	static const int off[] = { &&a - &&a, &&b - &&a, &&c - &&a };
	int r = 0;
	goto *(&&a + off[i]);
a:	r += 1;
b:	r += 10;
c:	r += 100;
	return r;
}
static int same(int k) {
	void *p = k ? &&a : &&b, *q = &&a;
	int r = (p == q) * 7;
	goto *p;
a:	return r + 1;
b:	return r + 2;
}
int main(void) {
	static const unsigned char prog[] = { 1, 2, 3, 2, 4, 0 };
	static const unsigned char prog2[] = { 1, 1, 2, 0 };
	printf("%d %d\n", run(prog), run(prog2));
	printf("%d %d %d\n", offsets(0), offsets(1), offsets(2));
	printf("%d %d\n", same(1), same(0));
	return 0;
}
`

// irreducibleProgram: loops entered at more than one block, which the
// structurer reaches through dispatch blocks: a goto into a loop, two loops
// that jump into each other, an irreducible loop inside another, a goto
// into a loop from a switch. Each path prints, so the order of the visits
// shows.
const irreducibleProgram = `#include <stdio.h>
static int into(int x) {
	int i = 0;
	if (x) goto in;
	while (i < 10) { i += 2; in: i++; }
	return i;
}
static int pingpong(int n) {
	int a = 0, b = 0;
	if (n & 1) goto B;
A:	a++; printf("A");
	if (a + b > n) return a * 10 + b;
	goto B;
B:	b++; printf("B");
	if (b % 3 == 0) goto A;
	if (a + b > n) return a * 10 + b;
	goto B;
}
static int nested(int x) {
	int i, s = 0;
	for (i = 0; i < 3; i++) {
		int j = 0;
		if ((x + i) & 1) goto mid;
	top:	j++;
	mid:	s += j;
		if (j < 3) goto top;
	}
	return s;
}
static int sw(int k) {
	int r = 0, i = 0;
	switch (k) {
	case 1: goto in1;
	case 2: goto in2;
	}
	for (; i < 4; i++) {
		r += 1;
	in1:	r += 10;
	in2:	r += 100;
	}
	return r;
}
int main(void) {
	printf("%d %d\n", into(0), into(1));
	printf("%d %d\n", pingpong(5), pingpong(8));
	printf("\n%d %d\n", nested(0), nested(1));
	printf("%d %d %d\n", sw(0), sw(1), sw(2));
	return 0;
}
`

// complexProgram: float and double _Complex. Arithmetic of two complex
// values and with a real operand, the product and quotient through
// compiler-rt with an infinity and a NaN (Annex G's recovery), parameters
// and results, a struct member, conversions both ways, __real__, __imag__,
// ~ (conjugate), == and !=, truth values, op=, static and local
// initialisers. Every part is printed with %a, so every bit shows.
const complexProgram = `#include <stdio.h>
typedef double _Complex dc;
typedef float _Complex fc;
static void show(const char *n, dc z) { printf("%s %a %a\n", n, __real__ z, __imag__ z); }
static dc add(dc a, dc b) { return a + b; }
static dc mul(dc a, dc b) { return a * b; }
static dc dv(dc a, dc b) { return a / b; }
static fc mulf(fc a, fc b) { return a * b; }
static dc fromreal(double d) { return d; }
struct s { int k; dc z; float f; };
static dc g1 = 1.5 + 2.0i;
static fc g2 = 3.0f;
int main(void) {
	dc a = 1.0 + 2.0i, b = 3.0 - 0.5i, inf = __builtin_inf() + 1.0i, nan = __builtin_nan("") + 0.0i;
	fc f = 2.0f + 1.0fi;
	double d = -2.0, n0 = -0.0;
	struct s st = { 7, 0, 1.5f };
	show("add", add(a, b));
	show("sub", a - b);
	show("mul", mul(a, b));
	show("div", dv(a, b));
	show("muli", mul(inf, a));
	show("divz", dv(a, 0.0));
	show("muln", mul(nan, a));
	show("real+", d + a);
	show("real-", d - a);
	show("-real", a - d);
	show("realx", d * a);
	show("realdiv", a / d);
	show("divreal", d / a);
	show("negz", n0 + (0.0 - 0.0i));
	show("neg", -a);
	show("conj", ~a);
	show("conv", fromreal(4.25));
	show("float", mulf(f, f));
	show("g", g1 + g2);
	st.z = a * 2.0;
	st.z += b;
	st.z *= a;
	st.z /= b;
	st.z -= 1.0;
	show("st", st.z);
	printf("%d %d %d %d %d %d\n", a == a, a != b, a == 1.0 + 2.0i, (dc)1.0 == 1.0, !(0.0 + 0.0i), a == 1.0 + 3.0i);
	if (b) printf("true\n");
	if (0.0i) printf("false\n");
	if (d * 0.0 + 2.0i) printf("imaginary\n");
	d = a;
	printf("%a %a %a %d %a\n", d, (double)b, __real__ f, st.k, (double)st.f);
	return 0;
}
`

// intComplexProgram: GNU integer complex types (int, unsigned, long long
// parts) and __real__ / __imag__ as lvalues: arithmetic, the product and
// the quotient (signed and unsigned), conjugate, parameters and results,
// assignment to a part and its address, also of a double _Complex.
const intComplexProgram = `#include <stdio.h>
typedef __complex__ int ci;
typedef __complex__ unsigned cu;
typedef __complex__ long long cl;
static ci conj(ci x) { return ~x; }
static cl mul(cl a, cl b) { return a * b; }
int main(void) {
	ci a, b, c;
	cu u;
	cl l;
	double _Complex d;
	double *pd;
	__real__ a = 7; __imag__ a = -3;
	__real__ b = 2; __imag__ b = 5;
	c = a * b;
	printf("%d %d\n", __real__ c, __imag__ c);
	c = a / b; printf("%d %d\n", __real__ c, __imag__ c);
	c = (a * b) / b; printf("%d %d\n", __real__ c, __imag__ c);
	c = a + b - 1; printf("%d %d\n", __real__ c, __imag__ c);
	c = conj(a); printf("%d %d %d %d\n", __real__ c, __imag__ c, c == conj(a), c != a);
	c = -a * 3; printf("%d %d\n", __real__ c, __imag__ c);
	__real__ u = 4000000000u; __imag__ u = 10;
	u = u / 3; printf("%u %u\n", __real__ u, __imag__ u);
	__real__ l = 1LL << 40; __imag__ l = -2;
	l = mul(l, l); printf("%lld %lld\n", __real__ l, __imag__ l);
	d = 1.5 + 2.5i;
	__imag__ d = -4.0;
	pd = &__real__ d; *pd += 1.0;
	__real__ d *= 2;
	printf("%a %a %d\n", __real__ d, __imag__ d, (int)__real__ a);
	c = 1.0 + 2.0i;
	printf("%d %d %d\n", __real__ c, __imag__ c, !(a - a));
	return 0;
}
`

// vectorProgram: GNU vectors of int, unsigned short, signed char, long
// long, float and double. Every operator with two vectors and with a
// scalar on either side, signed and unsigned division and shifts (no
// promotion: the element wraps), the unary operators (-0.0 too), the
// comparisons (-1 or 0, a NaN unordered), elements as values and lvalues,
// op=, casts between vectors and from and to a scalar, parameters and results, a
// static and a local initialiser, a compound literal, a member, ?:. No NaN
// that arithmetic produced is printed: wasm leaves its sign to the machine
// (0.0 / 0.0 computed at run time was -nan on the x86 CI runner, nan on an
// arm64 Mac; clang folds it to nan).
const vectorProgram = `#include <stdio.h>
typedef int v4si __attribute__((vector_size(16)));
typedef unsigned short v4hu __attribute__((vector_size(8)));
typedef signed char v8qi __attribute__((vector_size(8)));
typedef long long v2di __attribute__((vector_size(16)));
typedef float v4sf __attribute__((vector_size(16)));
typedef double v2df __attribute__((vector_size(16)));
typedef int v2si __attribute__((vector_size(8)));
typedef char v4qi __attribute__((vector_size(4)));
typedef short __attribute__((vector_size(8))) v4hs;
static v4si gi = {150, -100, 7, 200};
static v4hs ghs = {-7, 300};
static v2df gd = {1.5};
struct S { char c; v4si v; };
static void pi(const char *n, v4si v) { printf("%s %d %d %d %d\n", n, v[0], v[1], v[2], v[3]); }
static void phu(const char *n, v4hu v) { printf("%s %u %u %u %u\n", n, v[0], v[1], v[2], v[3]); }
static void pqi(const char *n, v8qi v) { int i; printf("%s", n); for (i = 0; i < 8; i++) printf(" %d", v[i]); printf("\n"); }
static void psf(const char *n, v4sf v) { printf("%s %a %a %a %a\n", n, v[0], v[1], v[2], v[3]); }
static void pdf(const char *n, v2df v) { printf("%s %a %a\n", n, v[0], v[1]); }
static void pdi(const char *n, v2di v) { printf("%s %lld %lld\n", n, v[0], v[1]); }
static v4si twice(v4si v) { return v + v; }
int main(void) {
	v4si j = {10, 13, -20, 30}, k;
	v4hu h = {65535, 1, 300, 40000}, h2 = {2, 3, 4, 5};
	v8qi q = {127, -128, 1, 2, 3, 4, 5, -6};
	v2di l = {1LL << 40, -3};
	v4sf f = {1.5f, -0.0f, 3.0f, 0.0f};
	v2df d = {__builtin_nan(""), 2.0};
	struct S s;
	int i;
	pi("add", gi + j); pi("sub", gi - j); pi("mul", gi * j); pi("div", gi / j); pi("rem", gi % j);
	pi("and", gi & j); pi("or", gi | j); pi("xor", gi ^ j); pi("shl", j << 2); pi("shr", gi >> 1);
	pi("scl", 2 - j); pi("scr", j - 2); pi("shv", 1 << (v4si){0, 1, 2, 3});
	pi("neg", -gi); pi("cpl", ~gi); pi("pos", +gi);
	pi("lt", gi < j); pi("eq", gi == gi); pi("ne", j != 13); pi("ge", 7 >= gi);
	phu("hadd", h + h2); phu("hmul", h * h2); phu("hdiv", h / h2); phu("hshr", h >> h2); phu("hneg", -h);
	phu("hlt", (v4hu)(h < h2)); phu("hshl", h << 1);
	pqi("qadd", q + 1); pqi("qmul", q * q); pqi("qdiv", q / 3); pqi("qshr", q >> 1); pqi("qeq", q == 3);
	pdi("ladd", l + l); pdi("lmul", l * 3); pdi("lshr", l >> 4); pdi("llt", l < 0);
	psf("fadd", f + 1.0f); psf("fmul", f * f); psf("fdiv", 1.0f / f); psf("fneg", -f); psf("fpos", +f);
	pi("flt", f < 1.0f); pi("feq", f == 0.0f);
	pdf("dadd", gd + gd); pdf("dneg", -d); pdi("deq", d == d); pdi("dne", d != d); pdi("dlt", d < 3.0);
	k = gi; k[2] = 99; k[0] += 5; pi("elem", k);
	k += j; k *= 2; k -= 1; k <<= 1; k >>= 2; k &= 255; k |= 1; k ^= 3; k /= 3; k %= 5; pi("op=", k);
	f *= 2.0f; f /= 4.0f; psf("fop=", f);
	pi("cast", (v4si)f); psf("cast2", (v4sf)(v4si){0x3f800000, 0x40000000, 0, -1});
	pdi("scast", (v2di)(v4si){1, 2, 3, 4}); pqi("lcast", (v8qi)0x0102030405060708LL);
	pi("call", twice(j)); pi("nest", twice(twice(j) - j) * 2);
	s.c = 1; s.v = j; s.v[3] = 7; pi("member", s.v);
	for (i = 0; i < 2; i++) pi("cond", i ? j : gi);
	pi("lit", (v4si){1, 2} + (v4si){3});
	pi("argl", twice((v4si){31})); q = (v8qi){31, -128, 127}; q %= (signed char)-19; pqi("crem", q);
	{ v4hs a = {1, -2, 3}, b = (v4hs){4} * a + ghs; printf("spec %d %d %d %d %zu\n", b[0], b[1], b[2], b[3], sizeof b); }
	printf("%d %d %zu %zu\n", (gi + j)[1], (gi == gi)[3], sizeof(v8qi), _Alignof(v2df));
	printf("%llx %llx %d\n", (unsigned long long)q, (long long)(v2si){5, 0x3ff00000}, (int)(v4qi){1, 2, 3, 4});
	return 0;
}
`

// aggValueProgram: statement expressions whose value is a structure or a
// union (nested, assigned, passed, selected from, and a copy: the object
// the last expression names changes afterwards, 20020206-1.c), and GNU
// casts to a union from each kind of member (960416-1.c).
const aggValueProgram = `#include <stdio.h>
struct A { unsigned a, b, c; };
union U { unsigned long long d; struct { unsigned l, h; } s; double f; char *p; struct A t; };
static struct A bar(void) { return (struct A){176, 52, 31}; }
static void show(const char *n, struct A a) { printf("%s %u %u %u\n", n, a.a, a.b, a.c); }
static void showu(const char *n, union U u) { printf("%s %llx %u %u\n", n, u.d, u.s.l, u.s.h); }
int main(void) {
	struct A d, s = {1, 2, 3};
	union U u;
	char c[] = "xyz";
	d = ({ ({ bar(); }); }); show("nest", d);
	d = ({ s; }); s.a = 9; show("copy", d);
	d = ({ struct A x = s; s.b = 7; x; }); show("local", d); show("s", s);
	show("arg", ({ int i = 2; s.c += i; s; }));
	printf("sel %u\n", ({ bar(); }).b);
	u = ({ union U v; v.d = 5; v; }); showu("union", u);
	showu("ull", (union U)0x100000000ULL);
	showu("dbl", (union U)1.5);
	u = (union U)c; printf("ptr %s\n", u.p);
	u = (union U)s; show("mem", u.t);
	printf("%u\n", ((union U)0x1234567800000009ULL).s.h);
	return 0;
}
`

// int128Program: __int128 and unsigned __int128. Every operator in both
// signednesses with carries and borrows across bit 64, division and remainder
// with negative operands, shifts by constants on both sides of 64 and by
// variables, comparisons that the high half and the low half each decide,
// conversions to and from the narrower integers, double and long double,
// ++ and --, op= with narrower and wider operands, parameters, results,
// varargs, statics with constants above 64 bits, ?:, and the truth values.
// Each value prints as its two halves.
const int128Program = `#include <stdio.h>
typedef __int128 S;
typedef unsigned __int128 U;
static U gu = ((U)0x0123456789abcdefULL << 64) | 0xfedcba9876543210ULL;
static S gs = -((S)1 << 100) + 12345;
static S garr[2] = {-1, (S)3 << 70};
static void pu(const char *n, U v) { printf("%s %016llx%016llx\n", n, (unsigned long long)(v >> 64), (unsigned long long)v); }
static void ps(const char *n, S v) { pu(n, (U)v); }
static S idS(S x) { return x; }
static U addU(U a, U b) { return a + b; }
static S mix(int a, S b, double c, U d, char e) { return b * a + (S)c + (S)d + e; }
static S vsum(int n, ...) {
	__builtin_va_list ap; S t = 0;
	__builtin_va_start(ap, n);
	while (n--) t += __builtin_va_arg(ap, S);
	__builtin_va_end(ap);
	return t;
}
int main(void) {
	volatile int sh = 70, sh2 = 5, z = 0;
	S a = ((S)0x7fffffffffffffffLL << 64) | 0xffffffffffffffffULL, b = -3, c = 1, d = -((S)1 << 90) - 7;
	U ua = ~(U)0, ub = (U)1 << 64, uc = 12345678901234567890ULL;
	pu("gu", gu); ps("gs", gs); ps("g0", garr[0]); ps("g1", garr[1]);
	ps("add", a + c); ps("add2", b + c); ps("add3", ub - 1 + (U)1);
	pu("uadd", ua + 1); pu("usub", (U)0 - 1); pu("usub2", ub - 1); pu("sub", (U)(d - b));
	ps("neg", -d); ps("neg0", -(S)0); ps("neg1", -c); pu("negmin", (U)-((S)1 << 127));
	ps("cpl", ~d); pu("ucpl", ~ub);
	ps("and", d & a); ps("or", d | c); ps("xor", d ^ a);
	pu("uand", ua & ub); pu("uor", ub | uc); pu("uxor", ua ^ uc);
	ps("mul", d * b); ps("mul2", a * a); pu("umul", ua * ua); pu("umul2", uc * uc);
	ps("div", d / b); ps("div2", d / 7); ps("div3", (-d) / (-b)); ps("mod", d % b); ps("mod2", d % -7); ps("mod3", -d % 1000);
	pu("udiv", ua / uc); pu("umod", ua % uc); pu("udiv2", ub / 3);
	ps("shl1", c << 1); ps("shl63", c << 63); ps("shl64", c << 64); ps("shl65", c << 65); ps("shl127", c << 127);
	ps("shr1", d >> 1); ps("shr63", d >> 63); ps("shr64", d >> 64); ps("shr70", d >> 70); ps("shr127", d >> 127);
	pu("ushr1", ua >> 1); pu("ushr64", ua >> 64); pu("ushr100", ua >> 100); pu("ushl3", ua << 3); pu("ushl0", ub << 0);
	ps("vshl", c << sh); ps("vshr", d >> sh); pu("vushr", ua >> sh); ps("vshl5", d << sh2); ps("vshr5", d >> sh2);
	printf("cmp %d%d%d%d%d%d\n", d < b, d <= b, d > b, d >= b, d == b, d != b);
	printf("cmp2 %d%d%d%d%d%d\n", a < d, a <= a, a > d, a >= a, a == a, a != d);
	printf("ucmp %d%d%d%d%d%d\n", ua < ub, ua <= ub, ua > ub, ua >= ub, ua == ub, ua != ub);
	printf("ucmp2 %d%d%d%d\n", ub < ub + 1, ub + 1 > ub, ub - 1 < ub, ub > ub - 1);
	printf("mixcmp %d %d %d\n", (S)-1 < (U)0, -1 < d, ua > (S)5);
	printf("sm %d %d\n", d < 0, (S)0x7fffffffffffffffLL + 1 > 0);
	printf("truth %d %d %d %d %d\n", !d, !(S)0, !!ub, d && c, z || (S)0);
	if (ub) printf("if-ub\n");
	if (!(ub >> 64 >> 1)) printf("if-zero\n");
	while (c) c >>= 1;
	printf("loop %d\n", (int)(c == 0));
	printf("conv %d %u %lld %llu %d\n", (int)d, (unsigned)ua, (long long)d, (unsigned long long)d, (char)ua);
	printf("conv2 %d %d %g %g %Lg\n", (short)ub, (_Bool)ub, (double)d, (double)ua, (long double)d);
	printf("conv3 %g %g\n", (double)(float)ua, (double)(S)-5);
	{ S i = -5; U j = -5; long long k = -5; unsigned long long m = 5; unsigned char uc2 = 200; signed char sc = -100;
	  ps("fi", i); pu("fj", j); ps("fk", k); ps("fm", m); ps("fu", uc2); ps("fc", sc); pu("fcu", (U)sc); ps("fb", (_Bool)7); }
	{ double x = -1.5e30, y = 3.0e38, w = 4.0e18; float f = -7.9f; long double q = -1.0e30L;
	  ps("dx", (S)x); ps("dy", (S)y); pu("dw", (U)w); ps("df", (S)f); ps("dq", (S)q); pu("duq", (U)(q * -1)); ps("dx1", x); }
	printf("%d%d%d\n", (S)3.9 == 3, (U)ub == ub, (S)-0.5 == 0);
	{ S x = 5; x += d; ps("pe1", x); x -= 1; x *= 3; ps("pe2", x); x /= 5; x %= 100000007; ps("pe3", x);
	  x <<= 3; x >>= 1; x |= 1; x &= ~(S)2; x ^= (S)1 << 80; ps("pe4", x);
	  x = 17; x += 1.5; ps("pe5", x); x *= -2; ps("pe6", x); }
	{ S x = 7; int n = 3; x += n; x *= (char)2; ps("ie", x); n += d; printf("narrow %d\n", n);
	  unsigned char uch = 250; uch += (S)10; printf("uch %d\n", uch); long long ll = 5; ll *= (S)1 << 40; printf("ll %lld\n", ll);
	  U ui = 3; ui *= -1; pu("ui", ui); unsigned int u32 = 4000000000u; u32 += (U)u32; printf("u32 %u\n", u32); }
	{ S x = -1; S y = x++; ps("post", y); ps("post2", x); y = ++x; ps("pre", y); y = x--; ps("postd", y); ps("postd2", x); y = --x; ps("pred", y); ps("pred2", x);
	  U u = ~(U)0 >> 64; u++; pu("carry", u); u--; pu("borrow", u); ++u; pu("c2", u); S big = (S)1 << 64; big--; ps("b2", big); big++; ps("b3", big); }
	{ S *p = &garr[1]; *p += (S)1 << 65; ps("ptr", *p); p[-1] <<= 64; ps("ptr2", garr[0]); garr[1] = garr[0] = c ? 9 : 10; ps("cp", garr[1]); }
	ps("cond", z ? a : d); ps("cond2", !z ? (S)1 : d); pu("comma", (z, ub));
	ps("id", idS(d)); pu("addU", addU(ua, ub)); pu("addU2", addU(ub, ub)); ps("mix", mix(3, d, -2.5, ub, 'a'));
	ps("va", vsum(3, d, a, (S)1 << 100)); ps("va2", vsum(2, (S)1, b));
	ps("lit", (S)0x123456789abcdefLL * 1000000007); pu("lit2", ((U)1 << 127) | ((U)1 << 3));
	ps("fold", (((S)1 << 100) / 3) % 1000003); pu("fold2", ~(U)0 / 10); ps("fold3", -(((S)1 << 120) >> 7));
	pu("kb", (U)(_Bool)((S)5 << 100)); ps("kb2", (_Bool)(U)0 + (S)2);
	pu("k1", (U)(S)-1 * 3); pu("k2", ((U)1 << 127) / 3 % 1000003); ps("k3", ((S)-1 << 100) / 7); ps("k4", -((S)1 << 100) % 1000003);
	pu("k5", ((((U)5 << 70) & ((U)7 << 68)) | ((U)1 << 100)) ^ 12345); ps("k6", +((S)3 << 90) - -((S)1 << 99)); pu("k7", ~((U)1 << 70) + (U)0);
	ps("kf", (S)(float)16777217); pu("kf2", (U)(double)(((U)1 << 64) + 1)); ps("kf3", -(int)(S)5 + (S)0);
	idS(d); -idS(d); (void)addU(ua, ub);
	{ struct { int a: 5; unsigned b: 7; long long c: 40; } bf = {3, 100, -5}; bf.a += (S)13; bf.b *= (U)3; bf.c -= ((S)1 << 70) + 9; bf.a |= (U)8; bf.b /= (S)-1;
	  printf("bf %d %u %lld\n", bf.a, bf.b, bf.c); }
	printf("sz %zu %zu %zu\n", sizeof(S), _Alignof(U), sizeof garr);
	return 0;
}
`

// ldComplexProgram: long double _Complex. Arithmetic, mixed with real
// operands, the product and quotient through __multc3 and __divtc3 with an
// infinity, conjugate and negation, ==, truth values, parameters and
// results, conversions from double _Complex and to long double and double,
// __real__ and __imag__ as values and lvalues, op=, a static initialiser.
// Each part prints with %La, so all 113 bits show.
const ldComplexProgram = `#include <stdio.h>
typedef long double _Complex lc;
static void show(const char *n, lc z) { printf("%s %La %La\n", n, __real__ z, __imag__ z); }
static lc mul(lc a, lc b) { return a * b; }
static lc dv(lc a, lc b) { return a / b; }
static lc conj(lc x) { return ~x; }
static lc g = 0.1L;
int main(void) {
	lc a, b, c;
	double _Complex d = 1.0 + 2.0i;
	long double r;
	__real__ a = 0.1L; __imag__ a = -3.0L;
	b = d;
	b += 1.0L / 3;
	show("add", a + b);
	show("sub", a - b);
	show("mul", mul(a, b));
	show("div", dv(a, b));
	show("inf", mul(__builtin_infl() + 1.0i, b));
	show("mixed", 2.0L * a);
	show("rdiv", a / 4.0L);
	show("lrdiv", 1.0L / a);
	show("neg", -a);
	show("conj", conj(a));
	show("imag", a + 2.0iL);
	show("g", g);
	c = a; c *= b; c -= 1.0L;
	show("op", c);
	r = a;
	printf("%La %f %d %d %d %d\n", r, (double)b, a == a, a != b, !(0.0L * a), a == 0.1L - 3.0iL);
	if (0.0L + 1.0iL) printf("true\n");
	show("bconj", __builtin_conjl(a));
	printf("%a %a %a %a\n", __builtin_creal(d), __builtin_cimag(d), (double)__builtin_crealf(1.5f + 2.5fi), (double)__builtin_cimagf(__builtin_conjf(1.5f + 2.5fi)));
	return 0;
}
`

const packedProgram = `#include <stdio.h>
#include <string.h>
struct __attribute__((packed)) p { char c; long long l; short s; int i; double d; };
struct m { char c; int i __attribute__((packed)); short s __attribute__((aligned(2), packed)); };
struct o { char c; struct p in; } __attribute__((packed));
union __attribute__((packed)) u { char c[3]; short s; };
static void dump(const void *v, size_t n) { const unsigned char *b = v; size_t i; for (i = 0; i < n; i++) printf("%02x", b[i]); printf("\n"); }
static long long sum(struct p q) { return q.c + q.l + q.s + q.i + (long long)q.d; }
int main(void) {
	struct p a[3]; struct m m; struct o o; union u u; int k;
	memset(a, 0xaa, sizeof a); memset(&m, 0xaa, sizeof m); memset(&o, 0xaa, sizeof o);
	for (k = 0; k < 3; k++) { a[k].c = k; a[k].l = 0x0102030405060708LL * (k + 1); a[k].s = -2 - k; a[k].i = 70000 + k; a[k].d = 1.5 * k; }
	m.c = 1; m.i = 0x11223344; m.s = 0x5566;
	o.c = 9; o.in = a[1]; o.in.l += 1;
	u.s = 0x7788;
	dump(a, sizeof a); dump(&m, sizeof m); dump(&o, sizeof o); dump(&u, sizeof u);
	printf("%zu %zu %zu %zu %lld %lld %d\n", sizeof(struct p), sizeof(struct m), sizeof(struct o), sizeof(union u), sum(a[2]), sum(o.in), a[2].s + m.s);
	return 0;
}
`

// asmProgram: asm statements with an empty template, the optimisation
// barriers of the gcc torture tests: inputs evaluated, "+" outputs
// unchanged, "=" outputs given their tied input.
const asmProgram = `#include <stdio.h>
int calls;
int *bump(int *p) { calls++; return p; }
int main(void) {
	int x = 7, y = 40, z, w = 3; long long l = 1LL << 40, m; char *p, buf[4] = "abc"; unsigned char c = 200; int i;
	asm volatile ("" : : : "memory");
	asm ("");
	asm volatile ("" : : "r" (x), "r" (*bump(&y)) : "memory");
	asm ("" : "+r" (x));
	asm ("" : "+r" (*bump(&w)));
	asm ("" : "=r" (z) : "0" (y + 2));
	asm ("" : "=r" (m) : "0" (l));
	asm ("" : "=r" (p) : "0" (buf));
	asm ("" : "=r" (i) : "0" (c));
	asm volatile ("" : "=r" (c) : "0" (300));
	asm ("" : "=r" (x), "=r" (y) : "1" (5), "0" (6));
	printf("%d %d %d %d %lld %s %d %d %d\n", x, y, z, w, m, p, i, c, calls);
	return 0;
}
`

// vlaParamProgram: parameters whose pointee is variably modified. The row
// stride is a run-time value (pr22061-1.c); the size expressions are
// evaluated once, on entry, for their effects (pr22061-2.c).
const vlaParamProgram = `#include <stdio.h>
#include <stdlib.h>
#include <string.h>
static int ticks;
static int tick(int v) { ticks++; return v; }

static void fill(int rows, int n, char a[rows][n]) {
	for (int i = 0; i < rows; i++)
		for (int j = 0; j < n; j++) a[i][j] = (char)(i * 10 + j);
}
static int sum(int rows, int n, char (*p)[n]) {
	int t = 0;
	for (int i = 0; i < rows; i++)
		for (int j = 0; j < n; j++) t += p[i][j] * (i + 1);
	return t;
}
static int third(int n, int a[][n][3]) { return a[1][n - 1][2] + (*(a + 1))[0][1]; }
static int wide(int n, short a[][n]) { return a[2][1] * 100 + (a + 1)[1][0]; }
static int pointer(int n, char (*p)[n]) { return (int)((char *)(p + 3) - (char *)p) + (int)((char *)&p[2] - (char *)p); }
static int effects(int n, char a[tick(2)][tick(n)]) { return a[1][n - 1]; }
static int inner(int n, char (*p)[tick(n)]) { return p[1][0]; }
static int outer(char a[tick(7)][3]) { return a[1][2]; }
static int callthru(int n, char a[][n]) { return sum(3, n, a) + a[2][n - 1]; }
static int store(int n, int (*p)[n], int v) { p[1][n - 1] = v; return p[1][n - 1] + p[0][0]; }

int main(void) {
	char buf[400];
	int n = 7;
	memset(buf, 0, sizeof buf);
	fill(4, n, (char (*)[7])buf);
	printf("%d %d\n", buf[0], buf[3 * 7 + 6]);
	printf("sum %d\n", sum(4, n, (void *)buf));
	int ints[90];
	for (int u = 0; u < 90; u++) ints[u] = u;
	printf("third %d\n", third(4, (void *)ints));
	short sh[60];
	for (int w = 0; w < 60; w++) sh[w] = (short)(w * 3);
	printf("wide %d\n", wide(5, (void *)sh));
	printf("pointer %d\n", pointer(9, (void *)buf));
	ticks = 0;
	printf("effects %d ticks %d\n", effects(5, (void *)buf), ticks);
	ticks = 0;
	printf("inner %d ticks %d\n", inner(6, (void *)buf), ticks);
	ticks = 0;
	printf("outer %d ticks %d\n", outer((void *)buf), ticks);
	printf("callthru %d\n", callthru(5, (void *)buf));
	printf("store %d\n", store(4, (void *)ints, 77));
	printf("ints %d %d\n", ints[7], ints[0]);
	return 0;
}
`

// vlaProgram: block-scope variable-length arrays. The loops of 100000
// passes over a 4 KB array overflow the stack unless the array is given back
// where its block ends, and at each jump that leaves it, so each loop leaves
// by one way only: the end of the block, continue, break, a goto back to a
// label before the declaration, a goto forward out of the block.
const vlaProgram = `#include <stdio.h>
struct P { int a; char c; double d; };
static void *volatile sink;
enum { N = 100000 };
static int kk = 4096; /* not a constant: the arrays of K bytes are variable-length */
#define K kk

static int sizes(int n) {
	int t = 0;
	for (int i = 1; i <= n; i++) {
		int a[i];
		for (int j = 0; j < i; j++) a[j] = j * i;
		t = t * 7 + a[i - 1] + a[0];
	}
	return t;
}

static int siblings(int n) {
	int t = 0;
	{ char a[n]; for (int i = 0; i < n; i++) a[i] = i; t += a[n - 1]; sink = a; }
	{ short b[n * 3]; for (int i = 0; i < n * 3; i++) b[i] = i * 3; t += b[n * 3 - 1]; sink = b; }
	{ long long c[2]; c[0] = 1; c[1] = n; t += (int)c[1]; }
	return t;
}

static int nested(int n) {
	int t = 0;
	int a[n];
	for (int i = 0; i < n; i++) a[i] = i + 1;
	{
		int b[n + 1];
		for (int i = 0; i <= n; i++) b[i] = a[i % n] * 10;
		{
			int c[2 * n];
			for (int i = 0; i < 2 * n; i++) c[i] = a[i % n] + b[i % (n + 1)];
			for (int m = 0; m < 2 * n; m++) t += c[m];
		}
		t = t * 3 + b[n];
	}
	return t * 5 + a[n - 1];
}

static int viaStructs(int n) {
	struct P p[n];
	double d[n];
	for (int i = 0; i < n; i++) {
		p[i].a = i;
		p[i].c = 'a' + i;
		p[i].d = i / 4.0;
	}
	for (int k = 0; k < n; k++) d[k] = p[k].d * 3 + p[n - 1 - k].a;
	double s = 0;
	for (int m = 0; m < n; m++) s += d[m] + p[m].c;
	struct P *q = p + 1;
	return (int)(s * 8) + q[0].a * 1000 + (int)(&p[n - 1] - &p[0]) + ((long)p % 16 == 0) * 100000;
}

static int alignment(int n) {
	char c[n];
	long double w[n];
	int ok = ((long)c % 16 == 0) + ((long)w % 16 == 0) * 2;
	char d[n + 1];
	return ok + ((long)d % 16 == 0) * 4;
}

static int escape(int n) {
	int t = 0;
	for (int i = 1; i < 5; i++) {
		int a[i * n];
		a[0] = i;
		a[i * n - 1] = i * 10;
		sink = a;
		t += ((int *)sink)[0] + ((int *)sink)[i * n - 1];
	}
	return t;
}

static int callee(int *p, int n) { int b[n]; b[n - 1] = p[0] + 1; return b[n - 1]; }

static int calls(int n) {
	int a[n];
	for (int i = 0; i < n; i++) a[i] = i;
	int t = 0;
	for (int m = 0; m < 100; m++) { int b[n]; b[0] = callee(a, n + m); t += b[0] + a[n - 1]; }
	return t;
}

static long endBlock(void) {
	long t = 0;
	for (int i = 0; i < N; i++) {
		char b[K];
		b[0] = i;
		b[K - 1] = i >> 3;
		sink = b;
		t += b[0] + b[K - 1];
	}
	return t;
}

static long viaContinue(void) {
	long t = 0;
	for (int i = 0; i < N; i++) {
		char b[K + i % 2];
		b[0] = i;
		sink = b;
		t += b[0];
		continue;
	}
	int w = 0;
	while (w++ < N) {
		char b[K];
		b[0] = 1;
		sink = b;
		if (w > 0) continue;
		t += 1000000;
	}
	do {
		char b[K];
		sink = b;
		if (w-- > 0) continue;
	} while (w > 0);
	return t;
}

static long viaBreak(void) {
	long t = 0;
	for (int i = 0; i < N; i++) {
		for (;;) {
			char b[K];
			b[0] = i;
			sink = b;
			t += b[0];
			break;
		}
		switch (i % 3) {
		case 0: { char s[K]; sink = s; s[0] = 2; t += s[0]; break; }
		default: { int s[K / 4]; sink = s; s[0] = 3; t += s[0]; }
		}
	}
	return t;
}

static long viaGotoBack(void) {
	long t = 0;
	int i = 0;
again:
	if (i < N) {
		char b[K];
		b[0] = i;
		sink = b;
		t += b[0];
		i++;
		goto again;
	}
	return t;
}

static long viaGotoSameBlock(void) {
	long t = 0;
	int i = 0;
	{
	lab:;
		char b[K];
		b[0] = i;
		sink = b;
		t += b[0];
		if (++i < N) goto lab;
	}
	return t;
}

static long viaGotoOut(void) {
	long t = 0;
	for (int i = 0; i < N; i++) {
		{
			int n = K - (i & 1);
			char b[n];
			b[0] = i;
			sink = b;
			t += b[0];
			goto out;
		}
		t += 1000;
	out:;
	}
	return t;
}

static int gotoInner(int n) {
	int t = 0;
	int a[n];
	a[0] = 5;
	{
		int b[n + 2];
		b[0] = 7;
		int k = 0;
	back:
		{
			int c[n + 3];
			c[0] = k;
			t += a[0] + b[0] + c[0];
			if (++k < 3) goto back;
		}
	}
	return t;
}

static int early(int n) {
	int a[n];
	a[0] = 9;
	if (n > 2) {
		int b[n];
		b[0] = a[0] + 1;
		return b[0];
	}
	return a[0];
}

int main(void) {
	printf("%d %d %d %d %d\n", sizes(1), sizes(20), siblings(5), siblings(50), nested(6));
	printf("%d %d %d %d\n", viaStructs(5), alignment(3), escape(4), calls(7));
	printf("%ld %ld %ld %ld\n", endBlock(), viaContinue(), viaBreak(), viaGotoBack());
	printf("%ld %ld %d %d %d\n", viaGotoSameBlock(), viaGotoOut(), gotoInner(2), early(1), early(5));
	return 0;
}
`

const staticInitProgram = `#include <stdio.h>
#include <string.h>
struct in { char c; int k[3]; };
struct s { short a; struct in in; int z; };
union u { struct { char pad[5]; struct in b; } d; struct { int c; struct in b; } e; };
struct s gs[3];
union u gu;
int *pk = &gs[1].in.k[2];
struct in *pin = &gs[2].in;
int *pk2 = gs[1].in.k + 1;
int *pz = &(gs + 2)->z;
char *pc = &((struct s *)gs)->in.c;
struct in *pb = &gu.e.b, *pd = &gu.d.b;
static const double dv[] = {__builtin_inf(), -__builtin_inf(), __builtin_nan(""), -__builtin_nan(""), __builtin_nan("0x1"), __builtin_nan("0X1"), __builtin_nan("017"), __builtin_nan("0b101"), __builtin_nan("0o7"), __builtin_nan("123456789"), __builtin_nan("0xfffffffffffffffffff"), __builtin_huge_val(), -2.5};
static const float fv[] = {__builtin_inff(), -__builtin_inff(), __builtin_nanf(""), -__builtin_nanf("0x1"), __builtin_nanf("0xffffffff"), __builtin_huge_valf(), 0.1f};
int main(void) {
	unsigned long long b; unsigned w; int i; double x = __builtin_nan("0x2a"); float y = __builtin_nanf("077");
	printf("%d %d %d %d %d %d %d\n", (int)((char *)pk - (char *)gs), (int)((char *)pin - (char *)gs), (int)((char *)pk2 - (char *)gs), (int)((char *)pz - (char *)gs), (int)(pc - (char *)gs), (int)((char *)pb - (char *)&gu), (int)((char *)pd - (char *)&gu));
	for (i = 0; i < (int)(sizeof dv / sizeof dv[0]); i++) { memcpy(&b, &dv[i], 8); printf("%llx ", b); }
	printf("\n");
	for (i = 0; i < (int)(sizeof fv / sizeof fv[0]); i++) { memcpy(&w, &fv[i], 4); printf("%x ", w); }
	memcpy(&b, &x, 8); memcpy(&w, &y, 4);
	printf("\n%llx %x\n", b, w);
	return 0;
}
`

// abiHeader declares, for TestABIAgainstClang, one function per way
// BasicCABI.md passes a structure or union: indirect (P, C, W, Big), as a
// singleton's scalar (L, N, U), or not at all (E, a GNU empty struct); W's
// alignment, greater than its member's, makes it indirect.
const abiHeader = `#include <stdarg.h>
struct P { int a, b; };
struct L { long long v; };
struct N { struct { int x[1]; } in; };
union U { short s; };
struct C { char a, b, c; };
struct E { };
struct W { int v __attribute__((aligned(8))); };
struct Big { int k[9]; };
struct P mkp(int a, int b);
int sump(struct P p);
struct L mkl(long long v);
long long getl(struct L l);
struct N mkn(int x);
int getn(struct N n);
union U mku(short s);
int getu(union U u);
struct C mkc(char a);
int getc3(struct C c, struct E e, int z);
struct W mkw(int v);
int getw(struct W w);
struct Big mkbig(int s);
int sumbig(struct Big b);
struct E mke(void);
long long vmix(int n, ...);
int vnone(int n, ...);
struct FV { float x; };
double fmix(float a, double b, struct FV v, int n, ...);
struct FV mkfv(float x);
`

const abiLib = `struct P mkp(int a, int b) { struct P p; p.a = a; p.b = b; return p; }
int sump(struct P p) { int r = p.a * 10 + p.b; p.a = 0; return r; }
struct L mkl(long long v) { struct L l = {v}; return l; }
long long getl(struct L l) { return l.v; }
struct N mkn(int x) { struct N n; n.in.x[0] = x; return n; }
int getn(struct N n) { return n.in.x[0]; }
union U mku(short s) { union U u; u.s = s; return u; }
int getu(union U u) { return u.s; }
struct C mkc(char a) { struct C c = {a, a + 1, a + 2}; return c; }
int getc3(struct C c, struct E e, int z) { return c.a + c.b * 3 + c.c * 5 + z; }
struct W mkw(int v) { struct W w; w.v = v; return w; }
int getw(struct W w) { return w.v; }
struct Big mkbig(int s) { struct Big b; int i; for (i = 0; i < 9; i++) b.k[i] = s + i; return b; }
int sumbig(struct Big b) { int i, t = 0; for (i = 0; i < 9; i++) t += b.k[i] * (i + 1); b.k[0] = 99; return t; }
struct E mke(void) { struct E e; return e; }
long long vmix(int n, ...) {
	va_list ap;
	long long t = 0;
	va_start(ap, n);
	while (n-- > 0) {
		switch (va_arg(ap, int)) {
		case 1: t = t * 3 + va_arg(ap, int); break;
		case 2: t = t * 3 + va_arg(ap, long long); break;
		case 3: t = t * 3 + *va_arg(ap, char *); break;
		case 4: { struct P p = va_arg(ap, struct P); t = t * 3 + p.a * 10 + p.b; break; }
		case 5: t = t * 3 + va_arg(ap, struct L).v; break;
		}
	}
	va_end(ap);
	return t;
}
int vnone(int n, ...) { return n; }
double fmix(float a, double b, struct FV v, int n, ...) {
	va_list ap;
	double t = a + b + v.x;
	va_start(ap, n);
	while (n-- > 0)
		t = t * 2 + va_arg(ap, double);
	va_end(ap);
	return t;
}
struct FV mkfv(float x) { struct FV v; v.x = x * 2; return v; }
`

const abiMain = `void abort(void);
int main(void) {
	struct P p = mkp(3, 4);
	struct Big b = mkbig(2);
	struct E e = mke();
	if (sump(p) != 34 || p.a != 3) abort();
	if (sump(mkp(1, 2)) != 12) abort();
	if (getl(mkl((1LL << 33) + 5)) != (1LL << 33) + 5) abort();
	if (getn(mkn(7)) != 7) abort();
	if (getu(mku(-3)) != -3) abort();
	if (getc3(mkc(10), e, 1) != 10 + 33 + 60 + 1) abort();
	if (getw(mkw(11)) != 11) abort();
	if (sumbig(b) != 2 * 45 + 240 || b.k[0] != 2) abort();
	{
		struct L l = {1LL << 35};
		short s = -2;
		if (vmix(6, 1, s, 2, 1LL << 40, 3, "A", 4, p, 1, (char)7, 5, l) !=
			((((-2 * 3 + (1LL << 40)) * 3 + 'A') * 3 + 34) * 3 + 7) * 3 + (1LL << 35)) abort();
		if (vnone(9) != 9) abort();
	}
	{
		float q = 4.0f;
		if (fmix(1.5f, 2.25, mkfv(0.25f), 2, 0.125, q) != ((1.5 + 2.25 + 0.5) * 2 + 0.125) * 2 + 4) abort();
	}
	return mkp(5, 6).b + mkl(9).v + mkbig(1).k[8] * 100;
}
`

// TestABIAgainstClang: structures and unions cross between our code and
// clang's, both ways, and the program exits as clang's alone does. The
// control is the single-file comparison: it cannot see an ABI that is
// wrong the same way on both sides, which the mixed links do.
func TestABIAgainstClang(t *testing.T) {
	h := newHarness(t)
	want, err := h.theirs(abiHeader + abiLib + abiMain)
	if err != nil {
		t.Fatal(err)
	}
	if want.Exit != 6+9+900 {
		t.Fatalf("clang alone: %+v", want)
	}
	h.check(abiHeader + abiLib + abiMain)
	for _, c := range []struct{ name, ours, theirs string }{
		{"our caller, clang's callee", abiHeader + abiMain, abiHeader + abiLib},
		{"clang's caller, our callee", abiHeader + abiLib, abiHeader + abiMain},
	} {
		got, err := h.mixed(c.ours, c.theirs)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !same(got, want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, want)
		}
	}
}

// TestRandomExpressions: random integer programs, free of undefined
// behaviour by construction, give the result clang's give. Each program
// folds its variables into the exit status. WASMGEN_RANDOM=N runs N
// programs instead of the default.
func TestRandomExpressions(t *testing.T) {
	h := newHarness(t)
	n := 60
	if s := os.Getenv("WASMGEN_RANDOM"); s != "" {
		var err error
		if n, err = strconv.Atoi(s); err != nil {
			t.Fatal(err)
		}
	}
	for i := range n {
		src := randomProgram(rand.New(rand.NewPCG(uint64(i), 0x5eed)))
		before := t.Failed()
		h.check(src)
		if !before && t.Failed() {
			t.Logf("program %d failed; seed (%d, 0x5eed)", i, i)
			return
		}
	}
}

type ctype struct {
	name   string
	bits   int
	signed bool
}

var ctypes = []ctype{
	{"_Bool", 1, false},
	{"signed char", 8, true}, {"unsigned char", 8, false},
	{"short", 16, true}, {"unsigned short", 16, false},
	{"int", 32, true}, {"unsigned", 32, false},
	{"long", 32, true}, {"unsigned long", 32, false},
	{"long long", 64, true}, {"unsigned long long", 64, false},
}

type gen struct {
	r      *rand.Rand
	vars   []string
	typ    map[string]ctype
	arrays []string // arrays of 4 elements, indexed by (e) & 3
	ptr    string   // a pointer to one of vars, when set
}

func (g *gen) pickType() ctype { return ctypes[g.r.IntN(len(ctypes))] }

func (g *gen) constant() string {
	switch g.r.IntN(4) {
	case 0:
		return strconv.Itoa(g.r.IntN(10))
	case 1:
		return strconv.FormatInt(int64(g.r.Int32()), 10)
	case 2:
		return strconv.FormatUint(g.r.Uint64(), 10) + "ULL"
	}
	return "-" + strconv.Itoa(g.r.IntN(300))
}

// expr returns a side-effect-free expression with no undefined behaviour:
// arithmetic that can overflow is done in an unsigned type, shift counts
// are masked, divisors are made odd (non-zero) in an unsigned type.
func (g *gen) expr(depth int) string {
	if depth <= 0 || g.r.IntN(4) == 0 {
		switch k := g.r.IntN(6); {
		case k == 0 || len(g.vars) == 0:
			return "(" + g.pickType().name + ")" + g.constant()
		case k == 1 && len(g.arrays) != 0:
			return g.element(depth)
		case k == 2 && g.ptr != "":
			return "(*" + g.ptr + ")"
		}
		return g.vars[g.r.IntN(len(g.vars))]
	}
	a, b := g.expr(depth-1), g.expr(depth-1)
	wide := "unsigned"
	if g.r.IntN(2) == 0 {
		wide = "unsigned long long"
	}
	switch g.r.IntN(17) {
	case 14:
		return "((" + a + ") && (" + b + "))"
	case 15:
		return "((" + a + ") || (" + b + "))"
	case 16:
		return "((" + a + ") ? (" + b + ") : (" + g.expr(depth-1) + "))"
	case 0:
		return "((" + wide + ")(" + a + ") + (" + wide + ")(" + b + "))"
	case 1:
		return "((" + wide + ")(" + a + ") - (" + wide + ")(" + b + "))"
	case 2:
		return "((" + wide + ")(" + a + ") * (" + wide + ")(" + b + "))"
	case 3:
		return "((" + wide + ")(" + a + ") / ((" + wide + ")(" + b + ") | 1))"
	case 4:
		return "((" + wide + ")(" + a + ") % ((" + wide + ")(" + b + ") | 1))"
	case 5:
		return "((" + a + ") & (" + b + "))"
	case 6:
		return "((" + a + ") | (" + b + "))"
	case 7:
		return "((" + a + ") ^ (" + b + "))"
	case 8:
		ops := []string{"<", ">", "<=", ">=", "==", "!="}
		return "((" + a + ") " + ops[g.r.IntN(len(ops))] + " (" + b + "))"
	case 9:
		if wide == "unsigned" {
			return "((unsigned)(" + a + ") << ((" + b + ") & 31))"
		}
		return "((unsigned long long)(" + a + ") << ((" + b + ") & 63))"
	case 10:
		if wide == "unsigned" {
			return "((int)(" + a + ") >> ((" + b + ") & 31))"
		}
		return "((long long)(" + a + ") >> ((" + b + ") & 63))"
	case 11:
		un := []string{"~", "!", "-(" + wide + ")"}
		return un[g.r.IntN(len(un))] + "(" + a + ")"
	case 12:
		return "(" + g.pickType().name + ")(" + a + ")"
	}
	return "(" + a + ", " + b + ")"
}

// element is an array element with an index that is always in bounds.
func (g *gen) element(depth int) string {
	a := g.arrays[g.r.IntN(len(g.arrays))]
	saved := g.arrays
	g.arrays = nil // no nested indexing: keeps the expressions small
	i := g.expr(depth - 1)
	g.arrays = saved
	return a + "[((int)(" + i + ")) & 3]"
}

// statement writes a random statement. Loops run a bounded number of times
// on a counter (i0, i1, i2 by nesting level) that nothing else assigns, so
// every program terminates; break and continue appear only inside loops.
func (g *gen) statement(b *strings.Builder, ind string, level int, inLoop bool) {
	r := g.r
	v := g.vars[r.IntN(len(g.vars))]
	t := g.typ[v]
	kind := r.IntN(12)
	if level >= 3 && kind >= 6 {
		kind = r.IntN(6)
	}
	switch kind {
	case 0:
		fmt.Fprintf(b, "%s%s = %s;\n", ind, v, g.expr(3))
	case 1:
		switch {
		case len(g.arrays) != 0 && r.IntN(2) == 0:
			fmt.Fprintf(b, "%s%s = %s;\n", ind, g.element(2), g.expr(3))
		case g.ptr != "":
			fmt.Fprintf(b, "%s*%s = %s;\n", ind, g.ptr, g.expr(3))
		default:
			fmt.Fprintf(b, "%s%s = %s;\n", ind, v, g.expr(3))
		}
	case 2:
		ops := []string{"&=", "|=", "^="}
		fmt.Fprintf(b, "%s%s %s %s;\n", ind, v, ops[r.IntN(len(ops))], g.expr(2))
	case 3:
		if t.signed {
			fmt.Fprintf(b, "%s%s = (%s)((unsigned long long)%s + 1);\n", ind, v, t.name, v)
		} else {
			ops := []string{"+=", "-=", "*="}
			fmt.Fprintf(b, "%s%s %s %s;\n", ind, v, ops[r.IntN(len(ops))], g.expr(2))
		}
	case 4:
		fmt.Fprintf(b, "%s%s = helper(%s, (int)(%s));\n", ind, v, g.expr(2), g.expr(1))
	case 5:
		if inLoop && r.IntN(2) == 0 {
			jumps := []string{"break", "continue"}
			fmt.Fprintf(b, "%sif (%s) %s;\n", ind, g.expr(2), jumps[r.IntN(2)])
		} else if t.signed || t.bits == 1 {
			fmt.Fprintf(b, "%s%s = !%s;\n", ind, v, v)
		} else {
			fmt.Fprintf(b, "%s%s++;\n", ind, v)
		}
	case 6, 7:
		fmt.Fprintf(b, "%sif (%s) {\n", ind, g.expr(2))
		g.block(b, ind+"\t", level+1, inLoop)
		if r.IntN(2) == 0 {
			fmt.Fprintf(b, "%s} else {\n", ind)
			g.block(b, ind+"\t", level+1, inLoop)
		}
		fmt.Fprintf(b, "%s}\n", ind)
	case 8:
		c := fmt.Sprintf("i%d", level)
		fmt.Fprintf(b, "%sfor (%s = 0; %s < %d; %s++) {\n", ind, c, c, 1+r.IntN(6), c)
		g.block(b, ind+"\t", level+1, true)
		fmt.Fprintf(b, "%s}\n", ind)
	case 9:
		c := fmt.Sprintf("i%d", level)
		fmt.Fprintf(b, "%s%s = 0;\n%swhile (%s < %d) {\n%s\t%s++;\n", ind, c, ind, c, 1+r.IntN(6), ind, c)
		g.block(b, ind+"\t", level+1, true)
		fmt.Fprintf(b, "%s}\n", ind)
	case 10:
		c := fmt.Sprintf("i%d", level)
		fmt.Fprintf(b, "%s%s = 0;\n%sdo {\n%s\t%s++;\n", ind, c, ind, ind, c)
		g.block(b, ind+"\t", level+1, true)
		fmt.Fprintf(b, "%s} while (%s < %d);\n", ind, c, 1+r.IntN(6))
	case 11:
		fmt.Fprintf(b, "%sswitch ((int)(%s) & 7) {\n", ind, g.expr(2))
		for _, l := range []string{"case 0:", "case 1:", "case 2:", "case 5:", "default:"} {
			if r.IntN(3) == 0 {
				continue
			}
			fmt.Fprintf(b, "%s%s\n", ind, l)
			g.block(b, ind+"\t", level+1, inLoop)
			if r.IntN(2) == 0 {
				fmt.Fprintf(b, "%s\tbreak;\n", ind)
			}
		}
		fmt.Fprintf(b, "%s}\n", ind)
	}
}

func (g *gen) block(b *strings.Builder, ind string, level int, inLoop bool) {
	for range 1 + g.r.IntN(3) {
		g.statement(b, ind, level, inLoop)
	}
}

func randomProgram(r *rand.Rand) string {
	g := &gen{r: r, typ: map[string]ctype{}}
	var b strings.Builder
	// globals, and a helper function computing an expression of its
	// parameters
	for i := range 3 {
		t := g.pickType()
		name := fmt.Sprintf("g%d", i)
		if r.IntN(2) == 0 {
			fmt.Fprintf(&b, "%s %s = (%s)%s;\n", t.name, name, t.name, g.constant())
		} else {
			fmt.Fprintf(&b, "%s %s;\n", t.name, name)
		}
		g.vars, g.typ[name] = append(g.vars, name), t
	}
	pt, rt := g.pickType(), g.pickType()
	saved := g.vars
	g.vars = []string{"p", "q"}
	fmt.Fprintf(&b, "static %s helper(%s p, int q) {\n\tif (%s) return %s;\n\treturn %s;\n}\n", rt.name, pt.name, g.expr(2), g.expr(3), g.expr(3))
	g.vars = saved
	at := g.pickType()
	fmt.Fprintf(&b, "%s ga[4] = {(%s)%s, (%s)%s};\n", at.name, at.name, g.constant(), at.name, g.constant())
	b.WriteString("int main(void) {\n\tint i0, i1, i2;\n")
	for i := range 5 {
		t := g.pickType()
		name := fmt.Sprintf("v%d", i)
		fmt.Fprintf(&b, "\t%s %s = (%s)(%s);\n", t.name, name, t.name, g.expr(2))
		g.vars, g.typ[name] = append(g.vars, name), t
	}
	lt := g.pickType()
	fmt.Fprintf(&b, "\t%s la[4] = {(%s)(%s)};\n", lt.name, lt.name, g.expr(2))
	target := g.vars[3+r.IntN(2)] // a local: taking its address puts it in memory
	fmt.Fprintf(&b, "\t%s *pv = &%s;\n", g.typ[target].name, target)
	g.arrays, g.ptr = []string{"ga", "la"}, "pv"
	for range 10 {
		g.statement(&b, "\t", 0, false)
	}
	b.WriteString("\tunsigned h = 0;\n")
	for _, a := range g.arrays {
		for i := range 4 {
			fmt.Fprintf(&b, "\th = h * 31u + (unsigned)%s[%d] + (unsigned)((unsigned long long)%s[%d] >> 32);\n", a, i, a, i)
		}
	}
	for _, v := range g.vars {
		fmt.Fprintf(&b, "\th = h * 31u + (unsigned)%s + (unsigned)((unsigned long long)%s >> 32);\n", v, v)
	}
	b.WriteString("\treturn (int)h;\n}\n")
	return b.String()
}
