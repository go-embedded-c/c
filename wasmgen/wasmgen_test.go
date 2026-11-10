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
		{"int main(int c, char **v, char **e) { return 0; }", "main with parameters other than (int, char **)"},
		{"int main(void) { return __func__[0]; }", "static local __func__"},
		{"int main(void) { void *p = 0; goto *p; return 0; }", "computed goto"},
		{"int f(int i) { typedef struct { int c[i + 2]; } c; return sizeof(c) * 3; } int main(void) { return f(1); }", "variable-length type"},
		{"int f(int i) { return sizeof(int[i]); } int main(void) { return f(1); }", "sizeof of a variable-length type"},
		{"int f(int x) { int i = 0; if (x) goto in; while (i < 10) { i += 2; in: i++; } return i; } int main(void) { return f(1); }", "irreducible control flow (W7)"},
		{"int main(void) { int x = 1; switch (x) { case 1 ... 3: return 1; } return 0; }", "case range"},
		{"int main(void) { int x = 1; return x ?: 2; }", "?: without a middle operand"},
		{"long double d; int main(void) { return 0; }", "file-scope object of type long double"},
		{"int main(void) { double d = 1.5L; return d; }", "value of type long double"},
		{"double d = 1.5L; int main(void) { return 0; }", "non-constant initializer"},
		{"int g(int, ...); int main(void) { return g(1, 1.0L); }", "variadic argument of type long double"},
		{"typedef float v4 __attribute__((vector_size(16))); float f(v4 *p) { v4 t = *p; return 0; } int main(void) { return 0; }", "local of type v4"},
		{"typedef int v4 __attribute__((vector_size(16))); int main(void) { v4 a, b; return 0; }", "local of type v4"},
		{"void f(int n) { struct S { int i[n]; int k; } s; } int main(void) { f(1); return 0; }", "local of type struct S {i array of int; k int}"},
		{"int f(int n, void *p) { struct S { int i[n]; int k; } *s = p; return s->k; } int main(void) { return 0; }", "local of type pointer to struct S"},
		{"int g(int n) { __builtin_va_start(n, n); return 0; } int main(void) { return g(1); }", "va_start outside a variadic function"},
		{"struct S { long long a: 24; }; int main(void) { struct S s; s.a = 1; return s.a; }", "bit-field of a type wider than int, narrower than int"},
		{"int x; struct S { int a: 3; } s = {x}; int main(void) { return 0; }", "non-constant bit-field initializer"},
		{"struct S { unsigned long long f:44; _Bool g:1; short h; }; int main(void) { struct S s; s.h = 1; return s.h; }", "bit-field layout that cc/v4 computes otherwise than clang"},
		{"struct S { long double d; }; int main(void) { struct S s = {1}; return 0; }", "initializer of type long double"},
		{"int f(); int g(int (*p)()) { return p(1); } int main(void) { return 0; }", "call through a pointer to a function without a prototype"},
		{"int f(int n) { int a[n]; return 0; } int main(void) { return f(2); }", "variable-length array"},
		{"int main(void) { return __builtin_bswap32(1); }", "builtin __builtin_bswap32"},
		{"int main(void) { return __builtin_mul_overflow_p(1, 2, 0); }", "builtin __builtin_mul_overflow_p"},
		{"int n = 2; void bar(char a[2][n]) { a[1][0] = 1; } int main(void) { return 0; }", "parameter of type pointer to array of char"},
		{"int main(void) { return (int)__builtin_return_address(0); }", "builtin __builtin_return_address"},
		{"int f(); int main(void) { return f(1); }", "call with arguments of a function declared without them in another unit"},
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
