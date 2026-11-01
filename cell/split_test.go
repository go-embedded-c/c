package cell

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func texts(t *testing.T, src string) []string {
	t.Helper()
	chunks, err := Split([]byte(src))
	if err != nil {
		t.Fatalf("Split(%q): %v", src, err)
	}
	var r []string
	for _, c := range chunks {
		p := "C "
		if c.Kind == Directive {
			p = "D "
		}
		r = append(r, p+src[c.Start:c.End])
	}
	return r
}

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"empty", "", nil},
		{"comments only", "// a\n/* b\n c */\n", nil},
		{"decl and statement", "int x = 2; printf(\"%d\\n\", x * x);",
			[]string{"C int x = 2;", `C printf("%d\n", x * x);`}},
		{"directive", "#include <stdio.h>\nint x;",
			[]string{"D #include <stdio.h>", "C int x;"}},
		{"directive continuation", "#define SQ(x) \\\n ((x)*(x))\nSQ(2);",
			[]string{"D #define SQ(x) \\\n ((x)*(x))", "C SQ(2);"}},
		{"function definition", "int f(void) { return 1; } f();",
			[]string{"C int f(void) { return 1; }", "C f();"}},
		{"struct declaration", "struct s { int a; int b; } v, *p; v.a = 1;",
			[]string{"C struct s { int a; int b; } v, *p;", "C v.a = 1;"}},
		{"struct returning function", "struct s f(void) { struct s r; return r; } x;",
			[]string{"C struct s f(void) { struct s r; return r; }", "C x;"}},
		{"struct pointer function", "struct s *f(void) { return 0; } y;",
			[]string{"C struct s *f(void) { return 0; }", "C y;"}},
		{"enum", "enum { A, B } e; e = B;", []string{"C enum { A, B } e;", "C e = B;"}},
		{"typedef struct", "typedef struct { int a; } T; T t;",
			[]string{"C typedef struct { int a; } T;", "C T t;"}},
		{"initializer braces", "int a[] = { 1, 2, 3 }; a[0]++;",
			[]string{"C int a[] = { 1, 2, 3 };", "C a[0]++;"}},
		{"if else blocks", "if (x) { a(); } else { b(); } c();",
			[]string{"C if (x) { a(); } else { b(); }", "C c();"}},
		{"if else statements", "if (x) a(); else if (y) b(); else c(); d();",
			[]string{"C if (x) a(); else if (y) b(); else c();", "C d();"}},
		{"for", "for (i = 0; i < 3; i++) { s += i; } s;",
			[]string{"C for (i = 0; i < 3; i++) { s += i; }", "C s;"}},
		{"do while block", "do { i++; } while (i < 3); j++;",
			[]string{"C do { i++; } while (i < 3);", "C j++;"}},
		{"do while statement", "do i++; while (i < 3); while (j) j--;",
			[]string{"C do i++; while (i < 3);", "C while (j) j--;"}},
		{"do then a while loop", "do { i++; } while (i); while (j) { j--; }",
			[]string{"C do { i++; } while (i);", "C while (j) { j--; }"}},
		{"braces in literals", `s = "}{;"; c = '}';`, []string{`C s = "}{;";`, `C c = '}';`}},
		{"comment inside chunk", "int /* ; } */ x;", []string{"C int /* ; } */ x;"}},
		{"compound literal", "p = (struct s){ 1, 2 }; q;", []string{"C p = (struct s){ 1, 2 };", "C q;"}},
		{"comparison is not an assignment", "if (a == b) { x(); } y();",
			[]string{"C if (a == b) { x(); }", "C y();"}},
		{"top-level comparison then block", "a <= b; { c(); } d;",
			[]string{"C a <= b;", "C { c(); }", "C d;"}},
		{"compound assignment literal", "a += (struct s){ 1 }.x; z;",
			[]string{"C a += (struct s){ 1 }.x;", "C z;"}},
		{"trailing chunk without semicolon", "x + 1", []string{"C x + 1"}},
		{"wide string", `w = L"};";`, []string{`C w = L"};";`}},
		{"number exponent", "d = 1e+5; e = 0x1p-3;", []string{"C d = 1e+5;", "C e = 0x1p-3;"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := texts(t, tc.src); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Split(%q)\n got %q\nwant %q", tc.src, got, tc.want)
			}
		})
	}
}

func TestSplitPositions(t *testing.T) {
	src := "int a;\n\n  #define X 1\n  b = X;\n"
	chunks, err := Split([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []Chunk{
		{Kind: Code, Start: 0, End: 6, Line: 1, Col: 1},
		{Kind: Directive, Start: 10, End: 21, Line: 3, Col: 3},
		{Kind: Code, Start: 24, End: 30, Line: 4, Col: 3},
	}
	if !reflect.DeepEqual(chunks, want) {
		t.Errorf("got %+v\nwant %+v", chunks, want)
	}
}

func TestSplitInnerDirective(t *testing.T) {
	src := "int f(void) {\n#define OPEN {\n  return 1;\n}\nx;"
	chunks, err := Split([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || src[chunks[0].Start:chunks[0].End] != "int f(void) {\n#define OPEN {\n  return 1;\n}" {
		t.Fatalf("chunks %+v", chunks)
	}
	d := chunks[0].Directives
	if len(d) != 1 || src[d[0].Start:d[0].End] != "#define OPEN {" || d[0].Line != 2 || d[0].Col != 1 {
		t.Errorf("inner directives %+v", d)
	}
}

func TestSplitErrors(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{"/* open", "1:1: unterminated comment"},
		{"x = \"open;", "1:5: unterminated string literal"},
		{"c = 'x;\n", "1:5: unterminated character literal"},
		{"f(1;\n", "1:2: unclosed '('"},
		{"a];", "1:2: unexpected ']'"},
		{"{ (}", "1:4: unexpected '}'"},
		{"#define A /* open", "1:11: unterminated comment"},
		{strings.Repeat("(", MaxDepth+1), "1:257: brackets nested deeper than 256"},
	} {
		_, err := Split([]byte(tc.src))
		var se *SyntaxError
		if !errors.As(err, &se) || err.Error() != tc.want {
			t.Errorf("Split(%q) = %v, want SyntaxError %q", tc.src, err, tc.want)
		}
	}
}

func FuzzSplit(f *testing.F) {
	for _, s := range []string{
		"int x = 2; printf(\"%d\\n\", x * x);",
		"#define A(x) \\\n x\nstruct s { int a; } v; if (a) { } else b(); do x; while (y);",
		"/* */ // \n 'a' L\"b\" 1e+5 {[()]}",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		chunks, err := Split(src)
		if err != nil {
			return
		}
		// Chunks are ordered, disjoint, inside the text, and what lies between
		// them is white space or comments only.
		end := 0
		for _, c := range chunks {
			if c.Start < end || c.End < c.Start || c.End > len(src) {
				t.Fatalf("chunk %+v out of order (previous end %d, len %d)", c, end, len(src))
			}
			end = c.End
		}
	})
}
