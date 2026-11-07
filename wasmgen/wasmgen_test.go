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
		{"int main(int c, char **v) { return 0; }", "parameter of type pointer to pointer to char"},
		{"int main(void) { int x = 1; if (x) return 1; return 0; }", "control flow (W3): StatementSelection"},
		{"int main(void) { int x = 1; return x && 2; }", "&& (W3)"},
		{"int main(void) { int x = 1; return x ? 2 : 3; }", "?: (W3)"},
		{"int main(void) { int x = 1; int *p = &x; return 0; }", "address-taken local (W4)"},
		{"int main(void) { static int n; return n; }", "static local"},
		{"double d; int main(void) { return 0; }", "file-scope object of type double"},
		{"int g(int, ...); int main(void) { return g(1, 2); }", "call of a variadic function"},
		{"int main(void) { int a[2]; return 0; }", "local of type array of 2 int"},
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
	r    *rand.Rand
	vars []string
	typ  map[string]ctype
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
		if g.r.IntN(3) == 0 || len(g.vars) == 0 {
			return "(" + g.pickType().name + ")" + g.constant()
		}
		return g.vars[g.r.IntN(len(g.vars))]
	}
	a, b := g.expr(depth-1), g.expr(depth-1)
	wide := "unsigned"
	if g.r.IntN(2) == 0 {
		wide = "unsigned long long"
	}
	switch g.r.IntN(14) {
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
	fmt.Fprintf(&b, "static %s helper(%s p, int q) { return %s; }\n", rt.name, pt.name, g.expr(3))
	g.vars = saved
	b.WriteString("int main(void) {\n")
	for i := range 5 {
		t := g.pickType()
		name := fmt.Sprintf("v%d", i)
		fmt.Fprintf(&b, "\t%s %s = (%s)(%s);\n", t.name, name, t.name, g.expr(2))
		g.vars, g.typ[name] = append(g.vars, name), t
	}
	for range 12 {
		v := g.vars[r.IntN(len(g.vars))]
		t := g.typ[v]
		switch r.IntN(6) {
		case 0, 1:
			fmt.Fprintf(&b, "\t%s = %s;\n", v, g.expr(3))
		case 2:
			ops := []string{"&=", "|=", "^="}
			fmt.Fprintf(&b, "\t%s %s %s;\n", v, ops[r.IntN(len(ops))], g.expr(2))
		case 3:
			if t.signed {
				fmt.Fprintf(&b, "\t%s = (%s)((unsigned long long)%s + 1);\n", v, t.name, v)
			} else {
				ops := []string{"+=", "-=", "*="}
				fmt.Fprintf(&b, "\t%s %s %s;\n", v, ops[r.IntN(len(ops))], g.expr(2))
			}
		case 4:
			fmt.Fprintf(&b, "\t%s = helper(%s, (int)(%s));\n", v, g.expr(2), g.expr(1))
		case 5:
			if t.signed || t.bits == 1 {
				fmt.Fprintf(&b, "\t%s = !%s;\n", v, v)
			} else if r.IntN(2) == 0 {
				fmt.Fprintf(&b, "\t%s++;\n", v)
			} else {
				fmt.Fprintf(&b, "\t--%s;\n", v)
			}
		}
	}
	b.WriteString("\tunsigned h = 0;\n")
	for _, v := range g.vars {
		fmt.Fprintf(&b, "\th = h * 31u + (unsigned)%s + (unsigned)((unsigned long long)%s >> 32);\n", v, v)
	}
	b.WriteString("\treturn (int)h;\n}\n")
	return b.String()
}
