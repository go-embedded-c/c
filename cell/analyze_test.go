package cell

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/go-embedded-c/c/headers"
)

func chunkTexts(src string, cs []Chunk) []string {
	var r []string
	for _, c := range cs {
		r = append(r, src[c.Start:c.End])
	}
	return r
}

func mustAnalyze(t *testing.T, src string, ctx Context) *Analysis {
	t.Helper()
	a := Analyze([]byte(src), ctx, Limits{})
	if len(a.Diagnostics) != 0 {
		t.Fatalf("Analyze(%q): %v", src, a.Diagnostics)
	}
	return a
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		src          string
		decls, stmts []string
	}{
		{"#include <stdio.h>\nint x = 2; printf(\"%d\\n\", x * x);",
			[]string{"#include <stdio.h>", "int x = 2;"}, []string{`printf("%d\n", x * x);`}},
		{"int y; y = 3;", []string{"int y;"}, []string{"y = 3;"}},
		{"int f(int); f(1);", []string{"int f(int);"}, []string{"f(1);"}},
		{"typedef int T; T a; a++;", []string{"typedef int T;", "T a;"}, []string{"a++;"}},
		{"int i; for (i = 0; i < 3; i++) { } if (i) i--;", []string{"int i;"}, []string{"for (i = 0; i < 3; i++) { }", "if (i) i--;"}},
		{"int sq(int v) { return v * v; }", []string{"int sq(int v) { return v * v; }"}, nil},
		{"int a; { int b = a; a = b + 1; }", []string{"int a;"}, []string{"{ int b = a; a = b + 1; }"}},
	} {
		a := mustAnalyze(t, tc.src, Context{})
		if got := chunkTexts(tc.src, a.Decls); !reflect.DeepEqual(got, tc.decls) {
			t.Errorf("%q: decls %q, want %q", tc.src, got, tc.decls)
		}
		if got := chunkTexts(tc.src, a.Stmts); !reflect.DeepEqual(got, tc.stmts) {
			t.Errorf("%q: stmts %q, want %q", tc.src, got, tc.stmts)
		}
	}
}

func TestDefinesAndUses(t *testing.T) {
	c1 := "#define N 3\nstruct pt { int x, y; };\nenum color { RED, GREEN };\ntypedef struct pt Pt;\nint grid[] = {1, 2, 3};\nstatic double scale = 2.0, off;\nint area(Pt p) { return p.x * p.y; }\n"
	a1 := mustAnalyze(t, c1, Context{})
	var got []string
	for _, n := range a1.Defines {
		got = append(got, n.Kind.String()+" "+n.Name)
	}
	want := []string{"macro N", "tag struct pt", "tag enum color", "enumerator RED", "enumerator GREEN", "typedef Pt", "variable grid", "variable scale", "variable off", "function area"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("defines\n got %q\nwant %q", got, want)
	}
	if a1.Uses != nil && len(a1.Uses) != 0 {
		t.Errorf("first cell uses %q", a1.Uses)
	}
	ctx := Context{}.Extend(a1)
	for _, w := range []string{"extern int grid[3];", "static double scale, off;", "int area(Pt p);", "#define N 3", "struct pt { int x, y; };"} {
		if !strings.Contains(ctx.Prelude, w) {
			t.Errorf("prelude lacks %q:\n%s", w, ctx.Prelude)
		}
	}

	c2 := "Pt q = { N, 2 };\nint total = 0;\ntotal = area(q) + grid[0] + sizeof grid / sizeof grid[0];\nstruct pt *pp = &q; pp->x = RED; q.y = 1;\n"
	a2 := mustAnalyze(t, c2, ctx)
	wantUses := []string{"N", "Pt", "RED", "area", "grid", "struct pt"}
	if !reflect.DeepEqual(a2.Uses, wantUses) {
		t.Errorf("uses %q, want %q", a2.Uses, wantUses)
	}
}

func TestUsesSkipsMembers(t *testing.T) {
	a1 := mustAnalyze(t, "int x; struct s { int x; } v;", Context{})
	a2 := mustAnalyze(t, "v.x = 1;", Context{}.Extend(a1))
	if want := []string{"v"}; !reflect.DeepEqual(a2.Uses, want) {
		t.Errorf("uses %q, want %q", a2.Uses, want)
	}
}

func TestDiagnostics(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{"int x = ;", "1:9: unexpected ';'"},
		{"\n\n  printf(1;", "3:9: unclosed '('"},
		{"int f(void) { return }", "1:22:"},
		{"if (x) y(;", "1:9: unclosed '('"},
		{"if (x) y(1;);", "1:"},
		{"undeclared_name = 1;", "1:1: undefined: undeclared_name"},
		{"#include \"../../etc/passwd\"", "1:1: #include: ../../etc/passwd: no such header"},
		{"#include </etc/passwd>", "1:1: #include: /etc/passwd: no such header"},
		{"#include <nosuch.h>", "1:1: #include: nosuch.h: no such header"},
		{"#define F <stdio.h>\n#include F", "2:1: #include needs a literal"},
		{"#define stdio ../../x\n#include <stdio.h>", "1:1: #define stdio: the name occurs in an #include path"},
		{"int f(void) {\n#line 1 \"x\"\n}", "2:1: #line is not allowed"},
		{"#include_next <stdio.h>", "1:1: #include_next is not allowed"},
		{"#if __has_include(\"/etc/passwd\")\n#endif", "1:1: __has_include is not allowed"},
		{strings.Repeat("x", DefaultLimits.MaxBytes+1), "1:1: cell is 1048577 bytes"},
	} {
		a := Analyze([]byte(tc.src), Context{}, Limits{})
		if len(a.Diagnostics) == 0 || !strings.HasPrefix(a.Diagnostics[0].String(), tc.want) {
			t.Errorf("Analyze(%.40q) = %v, want prefix %q", tc.src, a.Diagnostics, tc.want)
		}
	}
}

func TestEveryHeaderIncludeIsLiteral(t *testing.T) {
	// The guard relies on the embedded headers including only literal paths
	// (a computed #include could be steered by a cell's macro).
	n := 0
	err := fs.WalkDir(headers.FS(), "include", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".h") {
			return err
		}
		b, err := fs.ReadFile(headers.FS(), p)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			name, rest := directiveName(line)
			if name != "include" && name != "include_next" {
				continue
			}
			n++
			rest = strings.TrimSpace(stripComments(rest))
			if rest == "" || rest[0] != '<' && rest[0] != '"' {
				t.Errorf("%s:%d: computed include %q", p, i+1, line)
			}
		}
		return nil
	})
	if err != nil || n < 100 {
		t.Fatalf("walked %d includes: %v", n, err)
	}
	if w := pathWords(); !w["stdio"] || !w["h"] {
		t.Errorf("pathWords lacks stdio or h")
	}
}

func TestStandardHeaders(t *testing.T) {
	src := ""
	// Not here: setjmp.h and signal.h, which wasi-libc refuses with #error
	// unless the program opts in (exception handling, signal emulation), as
	// clang does; tgmath.h, see TestTgmath.
	for _, h := range []string{"assert", "ctype", "errno", "float", "inttypes", "limits", "locale", "math", "stdalign", "stdarg", "stdatomic", "stdbool", "stddef", "stdint", "stdio", "stdlib", "stdnoreturn", "string", "time", "uchar", "wchar", "wctype", "complex", "iso646", "fenv"} {
		src += "#include <" + h + ".h>\n"
	}
	mustAnalyze(t, src+"int main(void) { printf(\"%zu\\n\", strlen(\"abc\")); return 0; }\n", Context{})
}

func TestTgmath(t *testing.T) {
	mustAnalyze(t, "#include <tgmath.h>\ndouble r; float f; r = sqrt(2.0); f = sqrt(2.0f);", Context{})
}

func TestHostileInputDoesNotPanic(t *testing.T) {
	for _, src := range []string{"#define", "#define A(x) x\nA(", "struct { int a : f(); } s;"} {
		a := Analyze([]byte(src), Context{}, Limits{})
		if len(a.Diagnostics) == 0 {
			t.Errorf("Analyze(%q): no diagnostic", src)
		}
	}
}

// benchCell is a typical notebook cell. The reference for its timing is
// `clang --target=wasm32-wasip1 -fsyntax-only` on the same text with the
// statements wrapped in a function.
const benchCell = "#include <stdio.h>\n#include <stdlib.h>\n#include <string.h>\n#include <math.h>\n" +
	"struct pt { double x, y; };\n" +
	"double dist(struct pt a, struct pt b) { return sqrt((a.x-b.x)*(a.x-b.x) + (a.y-b.y)*(a.y-b.y)); }\n" +
	"struct pt ps[3] = {{0, 0}, {3, 4}, {6, 8}};\n" +
	"double total = 0;\n" +
	"for (int i = 1; i < 3; i++) total += dist(ps[i-1], ps[i]);\n" +
	"printf(\"%g\\n\", total);\n"

func BenchmarkAnalyze(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if a := Analyze([]byte(benchCell), Context{}, Limits{}); len(a.Diagnostics) != 0 {
			b.Fatal(a.Diagnostics)
		}
	}
}

func FuzzAnalyze(f *testing.F) {
	for _, s := range []string{
		"#include <stdio.h>\nint x = 2; printf(\"%d\\n\", x * x);",
		"struct s { int a; } v; v.a = 1; if (v.a) { } else v.a--;",
		"#define A(x) x\nA(",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		a := Analyze(src, Context{}, Limits{MaxBytes: 4096})
		ctx := Context{}.Extend(a)
		Analyze(src, ctx, Limits{MaxBytes: 4096})
	})
}
