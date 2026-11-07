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

// TestUnsupported: everything past W2 is refused by name, not miscompiled.
func TestUnsupported(t *testing.T) {
	f, err := cparse.New("", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ src, what string }{
		{"int main(int c, char **v) { return 0; }", "main with parameters"},
		{"int main(void) { goto l; l: return 0; }", "jump JumpStatementGoto (W7)"},
		{"int main(void) { int x = 1; switch (x) { case 1 ... 3: return 1; } return 0; }", "case range"},
		{"int main(void) { int x = 1; return x ?: 2; }", "?: without a middle operand"},
		{"int main(void) { static int n; return n; }", "static local"},
		{"double d; int main(void) { return 0; }", "file-scope object of type double"},
		{"int g(int, ...); int main(void) { return g(1, 2); }", "call of a variadic function"},
		{"struct S { int a; }; int main(void) { struct S s; return 0; }", "local of type struct S {a int}"},
		{"int f(void); int (*fp)(void) = f; int main(void) { return 0; }", "address of a function (W4c)"},
		{"int f(int n) { int a[n]; return 0; } int main(void) { return f(2); }", "variable-length array"},
		{"int main(void) { return __builtin_clz(1); }", "builtin __builtin_clz"},
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

// FuzzCompile: the generator is fed the AST of untrusted text. It may
// refuse anything; it must not panic, and what it accepts is a valid module.
func FuzzCompile(f *testing.F) {
	front, err := cparse.New("", "")
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{
		"int main(void) { return 0; }",
		"int main(void) { return 1 << 31; }",
		"int main(void) { return (char)300; }",
		"int main() { return sizeof(long long); }",
		"int x; int main(void) { return x; }",
		"unsigned char c = 200; long long f(short a, unsigned b) { return a * (long long)b; } int main(void) { c += f(-3, 7); return c++ + --c; }",
		"void abort(void); static int s = -2; int main(void) { s <<= 3; exit(s); abort(); }",
	} {
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
