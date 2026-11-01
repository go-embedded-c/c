package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-embedded-c/c/internal/oracle"
	"github.com/go-embedded-c/c/internal/oracle/oracletest"
)

func execute(args ...string) (string, string, error) {
	var out, errb bytes.Buffer
	root := RootCmd()
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errb)
	err := root.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func noToolchain(t *testing.T) {
	t.Setenv(oracle.EnvClang, "")
	t.Setenv(oracle.EnvSysroot, "")
	t.Setenv(oracle.EnvHome, "")
}

func TestVersionWithoutToolchain(t *testing.T) {
	noToolchain(t)
	out, _, err := execute("version")
	if err != nil || !strings.Contains(out, "clang 23.1.4") || !strings.Contains(out, "toolchain: oracle toolchain not available") {
		t.Fatalf("%q, %v", out, err)
	}
}

func TestVersionWithToolchain(t *testing.T) {
	oracletest.Require(t)
	out, _, err := execute("version")
	if err != nil || !strings.Contains(out, "toolchain: ok") {
		t.Fatalf("%q, %v", out, err)
	}
}

func TestCommandsNeedAToolchain(t *testing.T) {
	noToolchain(t)
	for _, args := range [][]string{
		{"run", "x.c"}, {"golden"}, {"corpus", "--dir", "."}, {"parse", "--dir", "."}, {"predefined"},
	} {
		if _, _, err := execute(args...); err == nil || !strings.Contains(err.Error(), "toolchain") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestFlagErrors(t *testing.T) {
	noToolchain(t)
	for _, args := range [][]string{{"toolchain", "--home", ""}, {"corpus"}, {"parse"}, {"run"}, {"nonsense"}} {
		if _, _, err := execute(args...); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}

func TestRunRelaysStatus(t *testing.T) {
	oracletest.Require(t)
	dir := t.TempDir()
	write := func(name, src string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	out, _, err := execute("run", write("ok.c", "#include <stdio.h>\nint main(void){puts(\"hi\");return 0;}"))
	if err != nil || out != "hi\n" {
		t.Fatalf("%q, %v", out, err)
	}
	_, _, err = execute("run", write("seven.c", "int main(void){return 7;}"))
	if ec, ok := err.(exitCode); !ok || int(ec) != 7 || ec.Error() != "exit status 7" {
		t.Fatalf("%v", err)
	}
	_, e, err := execute("run", write("trap.c", "#include <stdlib.h>\nint main(void){abort();}"))
	if ec, ok := err.(exitCode); !ok || int(ec) != 134 || !strings.Contains(e, "trap:") {
		t.Fatalf("%q, %v", e, err)
	}
	if _, _, err := execute("run", write("bad.c", "int main(void){return z;}")); err == nil {
		t.Fatal("a compile error must be an error")
	}
	if _, _, err := execute("run", filepath.Join(dir, "missing.c")); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

func TestGoldenAndCorpusAndParse(t *testing.T) {
	tc := oracletest.Require(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.c"), []byte("#include <stdio.h>\nint main(void){puts(\"a\");return 0;}"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := filepath.Join(t.TempDir(), "out.golden")
	if out, _, err := execute("golden", "--dir", dir, "--out", g); err != nil || !strings.Contains(out, "1 programs") {
		t.Fatalf("%q, %v", out, err)
	}
	if b, err := os.ReadFile(g); err != nil || !strings.Contains(string(b), "stdout \"a\\n\"") {
		t.Fatalf("%q, %v", b, err)
	}
	rep := filepath.Join(t.TempDir(), "rep.tsv")
	if out, _, err := execute("corpus", "--dir", dir, "--out", rep, "--jobs", "1"); err != nil || !strings.Contains(out, "1 files: pass=1") {
		t.Fatalf("%q, %v", out, err)
	}
	if b, _ := os.ReadFile(rep); !strings.HasPrefix(string(b), "a.c\tpass\t0\t") {
		t.Fatalf("%q", b)
	}
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "b.c"), []byte("int f( {"), 0o644); err != nil {
		t.Fatal(err)
	}
	fails := filepath.Join(t.TempDir(), "fails.tsv")
	for _, check := range []string{"", "--check"} {
		args := []string{"parse", "--dir", dir, "--jobs", "1"}
		if check != "" {
			args = append(args, check)
		}
		if out, _, err := execute(args...); err != nil || !strings.Contains(out, "1 / 1 files") {
			t.Fatalf("%q, %v", out, err)
		}
	}
	if out, _, err := execute("parse", "--dir", bad, "--out", fails); err != nil || !strings.Contains(out, "0 / 1 files") {
		t.Fatalf("%q, %v", out, err)
	}
	if b, _ := os.ReadFile(fails); !strings.HasPrefix(string(b), "b.c\t") {
		t.Fatalf("%q", b)
	}
	pre := filepath.Join(t.TempDir(), "p.h")
	if _, _, err := execute("predefined", "--out", pre); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(pre); !strings.Contains(string(b), "__wasm32__") {
		t.Fatal("predefined block lacks __wasm32__")
	}
	_ = tc
}

func TestFirstLine(t *testing.T) {
	if firstLine("a\nb") != "a" || firstLine("ab") != "ab" {
		t.Fatal("firstLine")
	}
}

func TestMainExitCodes(t *testing.T) {
	var got []int
	osExit = func(c int) { got = append(got, c) }
	defer func() { osExit = os.Exit }()
	old := os.Args
	defer func() { os.Args = old }()
	for _, args := range [][]string{{"coracle", "version"}, {"coracle", "nonsense"}} {
		os.Args = args
		main()
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("exit codes %v", got)
	}
}
