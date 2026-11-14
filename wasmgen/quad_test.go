package wasmgen_test

import "testing"

// quadProgram prints long doubles exactly, as the 16 bytes of binary128, so
// that a difference of one bit, a NaN or a signed zero shows in the stdout
// clang's program gives. It covers storage (locals, globals, members,
// arrays, static initialisers), parameters and results, each operation, each
// comparison NaN included, each conversion in both directions, and varargs.
const quadProgram = `#include <stdio.h>
#include <string.h>
#include <stdarg.h>
static void pr(const char *n, long double x) {
	unsigned long long w[2];
	memcpy(w, &x, 16);
	printf("%s %016llx %016llx\n", n, w[1], w[0]);
}
long double gld = 1.5L, gneg = -3.25L, gz = -0.0L, ghex = 0x1.8p-3L;
long double gpi = 3.14159265358979323846264338327950288L, gint = 7, gexpr = 1.0L / 3 + 2.5L * 4;
long double tab[3] = {1.0L, 0.1L, 1e4000L};
long double ginf = __builtin_infl(), gnan = __builtin_nanl("0x123");
double gd = 1.1L; int gi = 7.9L; float gf = 0.1L;
struct S { int tag; long double v; char c; } gs = {1, 2.5L, 'x'};
union U { long double ld; unsigned long long w[2]; } gu = {-1.25L};
long double id(long double x) { return x; }
long double add3(long double a, int b, long double c) { return a + b + c; }
long double mix(float f, long double a, double d, long double b, long long l) { return a * f - b / d + l; }
struct S mks(long double v, int t) { struct S s; s.tag = t; s.v = v; s.c = 'q'; return s; }
long double getv(struct S s) { s.v += 1; return s.v * s.tag; }
long double second(int a, long double x, long double y) { return y; }
int cmp(long double a, long double b) {
	return (a < b) | (a <= b) << 1 | (a > b) << 2 | (a >= b) << 3 | (a == b) << 4 | (a != b) << 5;
}
long double vsum(int n, ...) {
	va_list ap;
	long double t = 0;
	va_start(ap, n);
	while (n-- > 0) {
		t += va_arg(ap, long double);
		t += va_arg(ap, int);
		t += va_arg(ap, double);
	}
	va_end(ap);
	return t;
}
long double counter(void) { static long double c = 0.5L; c *= 2; return c; }
int main(void) {
	long double a = 1.0L, b = 3.0L, z = 0.0L, c = a / b, n = z / z, inf = 1 / z;
	pr("div", c); pr("add", a + b); pr("sub", a - b); pr("mul", c * b);
	pr("neg", -c); pr("negz", -z); pr("negneg", -(-c)); pr("plus", +c);
	pr("gld", gld); pr("gneg", gneg); pr("gz", gz); pr("ghex", ghex); pr("gpi", gpi); pr("gint", gint); pr("gexpr", gexpr);
	pr("tab0", tab[0]); pr("tab1", tab[1]); pr("tab2", tab[2]); pr("ginf", ginf); pr("gnan", gnan);
	pr("gs", gs.v); pr("gu", gu.ld);
	printf("%a %d %a %d\n", gd, gi, gf, gs.tag);
	pr("n", n); pr("inf", inf); pr("-inf", -inf);
	{
		int i = -5; unsigned u = 4000000000u; long long ll = -(1LL << 62); unsigned long long ull = ~0ULL;
		float f = 1.1f; double d = 1.1; signed char sc = -7; unsigned char uc = 250; _Bool bb = 1; short sh = -3; unsigned short us = 65535;
		pr("i", i); pr("u", u); pr("ll", ll); pr("ull", ull); pr("f", f); pr("d", d);
		pr("sc", sc); pr("uc", uc); pr("bb", bb); pr("sh", sh); pr("us", us);
		pr("cast", (long double)3 / 2); pr("c2", (long double)(double)1.1);
	}
	{
		long double x = 123456789.75L, y = -2.5L, big = 1e30L, s = 200.75L;
		printf("%d %u %lld %llu %d %d %d\n", (int)x, (unsigned)x, (long long)y, (unsigned long long)x, (signed char)y, (unsigned char)s, (short)y);
		printf("%a %a %a %a\n", (double)x, (double)big, (double)(float)x, (double)(float)big);
		printf("%d %d %d %d\n", (_Bool)z, (_Bool)n, (_Bool)a, (_Bool)-z);
		printf("%lld %llu %u\n", (long long)-big, (unsigned long long)s, (unsigned)s);
	}
	printf("%d %d %d %d\n", cmp(a, b), cmp(b, a), cmp(a, a), cmp(n, a));
	printf("%d %d %d %d\n", cmp(n, n), cmp(inf, a), cmp(-inf, inf), cmp(z, -z));
	printf("%d %d %d %d %d\n", !z, !n, !a, n ? 1 : 2, z ? 3 : 4);
	if (n) puts("nan true");
	if (z) puts("z true");
	while (z) { }
	for (; a; ) break;
	printf("%d %d %d\n", a && n, z || z, !(a && z));
	printf("%d %d %d %d %d\n", a < 2, 3 > a, a == 1, a != 1, 1 <= a);
	printf("%d %d %d\n", a < 2.5, 1.5f >= a, b == 3u);
	pr("id", id(c)); pr("add3", add3(a, 2, b)); pr("mix", mix(1.5f, a, 3.0, b, -7LL)); pr("idint", id(5)); pr("second", second(1, a, b));
	{
		long double x = 2;
		int k = 10; double dd = 1; float ff = 2;
		x += 1; x -= 0.5; x *= 3; x /= 7; pr("compound", x);
		x = x + 1; x++; ++x; x--; pr("incdec", x); pr("post", x++); pr("pre", ++x); pr("postd", x--); pr("pred", --x); pr("x", x);
		k += 2.5L; k *= 1.5L; dd /= 3.0L; ff -= 0.25L;
		printf("%d %a %a\n", k, dd, ff);
		long double *p = &x;
		*p = 9; pr("p", *p); pr("pa", p[0]); *p += 0.5L; pr("p+", x);
		struct S s = {1, 2, 3};
		s.v += 4; pr("s", s.v); s.v = s.tag; pr("s2", s.v);
		struct S t = mks(6.25L, 4); pr("mks", t.v); pr("getv", getv(t)); pr("getv2", getv(mks(1, 2)));
		long double arr[3] = {1, 2, 3};
		arr[1] *= arr[2]; pr("arr", arr[1]);
		pr("comma", (k++, x)); pr("cond", k > 1 ? x : 7); pr("cond2", k < 1 ? x : 7);
		(void)x; x; (x = 4); x = x = 5; pr("chain", x);
		pr("fabs", __builtin_fabsl(-c)); pr("copysign", __builtin_copysignl(a, -z)); pr("inf", __builtin_infl()); pr("huge", __builtin_huge_vall());
		pr("nan", __builtin_nanl("")); pr("nan2", __builtin_nanl("0x123"));
	}
	pr("vsum", vsum(3, 1.5L, 1, 0.25, 2.5L, 2, 0.5, 3.5L, 3, 0.125));
	printf("%La %Lf %d %Lg\n", c, 2.5L, 7, gneg);
	pr("c1", counter()); pr("c2", counter());
	return 0;
}
`

// quadLib and quadMain are the two halves of an ABI test: long double
// parameters, results and varargs cross between our code and clang's.
const quadHeader = `#include <stdarg.h>
void abort(void);
struct QS { int a; long double v; };
long double qadd(long double a, long double b);
long double qmix(int i, long double a, double d, long double b, float f);
long double qvs(int n, ...);
struct QS qmk(long double v, int a);
long double qget(struct QS s);
int qcmp(long double a, long double b);
long double qdef(long double x);
`

const quadLib = `long double qadd(long double a, long double b) { return a + b; }
long double qmix(int i, long double a, double d, long double b, float f) { return a * i + b / d - f; }
long double qvs(int n, ...) {
	va_list ap;
	long double t = 0;
	va_start(ap, n);
	while (n-- > 0) {
		switch (va_arg(ap, int)) {
		case 1: t = t * 2 + va_arg(ap, int); break;
		case 2: t = t * 2 + va_arg(ap, long double); break;
		default: t = t * 2 + va_arg(ap, double); break;
		}
	}
	va_end(ap);
	return t;
}
struct QS qmk(long double v, int a) { struct QS s; s.a = a; s.v = v; return s; }
long double qget(struct QS s) { return s.v * s.a; }
int qcmp(long double a, long double b) { return a < b ? -1 : a > b ? 1 : 0; }
long double qdef(long double x) { return x - 1; }
`

const quadMain = `int main(void) {
	struct QS s = qmk(2.5L, 3);
	if (qadd(1.5L, 2.25L) != 3.75L) abort();
	if (qadd(1.0L / 3, 0) != 1.0L / 3) abort();
	if (qmix(2, 1.5L, 4.0, 8.0L, 0.5f) != 2.0L + 3 - 0.5f + 1 - 1) abort();
	if (qvs(4, 1, 7, 2, 2.5L, 3, 0.5, 2, 1.0L / 3) != ((((7 * 2 + 2.5L) * 2 + 0.5) * 2) + 1.0L / 3)) abort();
	if (s.a != 3 || s.v != 2.5L || qget(s) != 7.5L) abort();
	if (qcmp(1, 2) != -1 || qcmp(2, 1) != 1 || qcmp(2, 2) != 0) abort();
	if (qdef(qadd(1, 1)) != 1) abort();
	{
		long double x = 6.5L;
		if (qadd(x, -x) != 0 || qmk(x, 2).v != x) abort();
	}
	return qget(qmk(1.5L, 4)) + qcmp(qadd(1, 2), 3) * 10;
}
`

// TestQuadABIAgainstClang: long doubles cross between our code and clang's,
// both ways, and the program exits as clang's alone does.
func TestQuadABIAgainstClang(t *testing.T) {
	h := newHarness(t)
	want, err := h.theirs(quadHeader + quadLib + quadMain)
	if err != nil {
		t.Fatal(err)
	}
	if want.Exit != 6 {
		t.Fatalf("clang alone: %+v", want)
	}
	h.check(quadHeader + quadLib + quadMain)
	for _, c := range []struct{ name, ours, theirs string }{
		{"our caller, clang's callee", quadHeader + quadMain, quadHeader + quadLib},
		{"clang's caller, our callee", quadHeader + quadLib, quadHeader + quadMain},
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

// quadProgram2: the classification and quiet-comparison builtins on every
// kind of long double, and constants that overflow to infinity at compile
// time (gcc torture 960405-1.c).
const quadProgram2 = `#include <stdio.h>
#include <string.h>
static void pr(const char *n, long double x) {
	unsigned long long w[2];
	memcpy(w, &x, 16);
	printf("%s %016llx %016llx\n", n, w[1], w[0]);
}
#define X 5.9486574767861588254287966331400356538172e4931L
long double gx = X + X, gy = 2.0L * X, gm = -X * 2, gr = 1.0L / 3 * 3, gs = 0.1L + 0.2L, gt = (long double)1 / 3 - 1e-20L;
long double gu = 1.0L - 1, gv = -(1.0L - 1), gw = (long double)0.1f;
int cls(long double x) {
	return __builtin_isinf(x) | __builtin_isnan(x) << 1 | __builtin_isfinite(x) << 2 | __builtin_signbit(x) << 3 |
		__builtin_signbitl(x) << 6;
}
int uo(long double x, long double y) {
	return __builtin_isunordered(x, y) | __builtin_isgreater(x, y) << 1 | __builtin_isgreaterequal(x, y) << 2 |
		__builtin_isless(x, y) << 3 | __builtin_islessequal(x, y) << 4 | __builtin_islessgreater(x, y) << 5;
}
int main(void) {
	long double z = 0, n = z / z, inf = 1 / z, v[7] = {0.0L, -0.0L, 1.5L, -2.5L, n, inf, -inf};
	int i, j;
	for (i = 0; i < 7; i++) {
		printf("%d:", cls(v[i]));
		for (j = 0; j < 7; j++)
			printf(" %d", uo(v[i], v[j]));
		printf(" %d %d\n", uo(v[i], 2), uo(2.5f, v[i]));
	}
	pr("gx", gx); pr("gy", gy); pr("gm", gm); pr("gr", gr); pr("gs", gs); pr("gt", gt);
	pr("gu", gu); pr("gv", gv); pr("gw", gw);
	printf("%d %d %d\n", gx == gy, __builtin_isinf(gx), __builtin_signbit(gm));
	return 0;
}
`
