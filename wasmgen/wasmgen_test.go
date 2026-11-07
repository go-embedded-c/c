package wasmgen_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/go-embedded-c/c/internal/cparse"
	"github.com/go-embedded-c/c/internal/oracle"
	"github.com/go-embedded-c/c/internal/oracle/oracletest"
	"github.com/go-embedded-c/c/wasmgen"
)

func compile(t *testing.T, f *cparse.Front, src string) ([]byte, error) {
	t.Helper()
	ast, err := f.Translate("t.c", []byte(src))
	if err != nil {
		t.Fatalf("%q: %v", src, err)
	}
	return wasmgen.Compile(ast, "t.c")
}

// TestUnsupported: everything past W1 is refused by name, not miscompiled.
func TestUnsupported(t *testing.T) {
	f, err := cparse.New("", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ src, what string }{
		{"int x;", "file-scope declaration"},
		{"int f(void) { return 0; }", "function other than main"},
		{"int main(int c, char **v) { return 0; }", "main with parameters"},
		{"void main(void) { }", "main not returning int"},
		{"int main(void) { int x = 1; return x; }", "function body other than one return"},
		{"int main(void) { }", "function body other than one return"},
		{"int main(void) { return; }", "function body other than one return"},
	} {
		_, err := compile(t, f, tc.src)
		var u *wasmgen.Unsupported
		if !errors.As(err, &u) || u.What != tc.what {
			t.Errorf("%q: %v, want unsupported %q", tc.src, err, tc.what)
		}
	}
	obj, err := compile(t, f, "int main(void) { return 2 * 3 + 1; }")
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

// TestAgainstClang: our object, linked by wasm-ld with wasi-libc, exits as
// clang's program does, in the same wazero. The control pairs our program
// with clang's for another value: the harness must see the difference.
func TestAgainstClang(t *testing.T) {
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
	defer r.Close(ctx)
	ours := func(src string) oracle.Result {
		obj, err := compile(t, f, src)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		mod, err := tc.Link(ctx, obj)
		if err != nil {
			t.Fatalf("%q: link: %v", src, err)
		}
		res, err := r.Run(ctx, mod, nil, nil)
		if err != nil {
			t.Fatalf("%q: run: %v", src, err)
		}
		return res
	}
	theirs := func(src string) oracle.Result {
		res, err := tc.Output(ctx, r, []byte(src), oracle.CompileOptions{})
		if err != nil {
			t.Fatalf("%q: clang: %v", src, err)
		}
		return res
	}
	same := func(a, b oracle.Result) bool {
		return a.Exit == b.Exit && a.Trap == b.Trap && bytes.Equal(a.Stdout, b.Stdout) && bytes.Equal(a.Stderr, b.Stderr)
	}
	for _, src := range []string{
		"int main(void) { return 0; }",
		"int main(void) { return 3; }",
		"int main(void) { return 2 * 21; }",
		"int main(void) { return 255; }",
		"int main(void) { return -1; }",
	} {
		if a, b := ours(src), theirs(src); !same(a, b) {
			t.Errorf("%q: ours %+v, clang %+v", src, a, b)
		}
	}
	if same(ours("int main(void) { return 3; }"), theirs("int main(void) { return 4; }")) {
		t.Error("control: the harness did not tell exit 3 from exit 4")
	}
}
