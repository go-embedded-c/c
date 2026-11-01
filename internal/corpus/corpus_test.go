package corpus_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-embedded-c/c/internal/corpus"
	"github.com/go-embedded-c/c/internal/golden"
	"github.com/go-embedded-c/c/internal/oracle"
	"github.com/go-embedded-c/c/internal/oracle/oracletest"
)

func TestJudge(t *testing.T) {
	e := corpus.Entry{Path: "p.c"}
	ex := corpus.Entry{Path: "q.c", Expected: []byte("ok\n"), HasExp: true}
	for _, c := range []struct {
		e    corpus.Entry
		res  oracle.Result
		want corpus.Outcome
	}{
		{e, oracle.Result{}, corpus.Pass},
		{e, oracle.Result{Exit: 1}, corpus.Fail},
		{e, oracle.Result{Trap: "unreachable"}, corpus.Trap},
		{e, oracle.Result{TimedOut: true}, corpus.Timeout},
		{ex, oracle.Result{Stdout: []byte("ok\n")}, corpus.Pass},
		{ex, oracle.Result{Stdout: []byte("no\n")}, corpus.Fail},
	} {
		if got := corpus.Judge(c.e, c.res); got.Outcome != c.want {
			t.Errorf("%+v: %s, want %s", c.res, got.Outcome, c.want)
		}
	}
}

func TestLoadAndReport(t *testing.T) {
	dir := t.TempDir()
	must := func(p, s string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("b.c", "int b;")
	must("a.c", "int a;")
	must("a.c.expected", "x")
	must("sub/c.c", "int c;")
	must("notes.txt", "ignored")
	es, err := corpus.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Path)
	}
	if got := strings.Join(names, ","); got != "a.c,b.c,sub/c.c" {
		t.Fatalf("entries %s", got)
	}
	if !es[0].HasExp || string(es[0].Expected) != "x" || es[1].HasExp {
		t.Fatalf("expected files: %+v", es)
	}
	if _, err := corpus.Load(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing root must be an error")
	}
	recs := []corpus.Rec{{Path: "a.c", Outcome: corpus.Pass}, {Path: "b.c", Outcome: corpus.CompileFail, Detail: "l1\nl2"}}
	var buf bytes.Buffer
	corpus.Report(&buf, recs)
	want := "a.c\tpass\t0\t\nb.c\tcompile-fail\t0\tl1 l2\n# total 2 pass=1 compile-fail=1 fail=0 trap=0 timeout=0 harness=0\n"
	if buf.String() != want {
		t.Fatalf("report:\n%s", buf.String())
	}
}

func TestRunLive(t *testing.T) {
	tc := oracletest.Require(t)
	rn, err := oracle.NewRunner(context.Background(), oracle.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rn.Close(context.Background())
	es := []corpus.Entry{
		{Path: "ok.c", Source: []byte("int main(void){return 0;}")},
		{Path: "bad.c", Source: []byte("int main(void){return y;}")},
		{Path: "ret3.c", Source: []byte("int main(void){return 3;}")},
		{Path: "out.c", Source: []byte("#include <stdio.h>\nint main(void){puts(\"ok\");return 0;}"), Expected: []byte("ok\n"), HasExp: true},
	}
	recs := corpus.Run(context.Background(), tc, rn, es, oracle.CompileOptions{}, 2)
	for i, want := range []corpus.Outcome{corpus.Pass, corpus.CompileFail, corpus.Fail, corpus.Pass} {
		if recs[i].Outcome != want {
			t.Errorf("%s: %s (%s), want %s", recs[i].Path, recs[i].Outcome, recs[i].Detail, want)
		}
	}
	if recs[1].Detail == "" {
		t.Error("a compile failure must carry the first diagnostic")
	}
	if sum := corpus.Summary(recs); sum[corpus.Pass] != 2 {
		t.Errorf("summary %v", sum)
	}
}

// TestGoldenPrograms: the committed golden file decodes and covers exactly
// the committed programs (no toolchain needed), and, with the toolchain,
// the oracle still produces every record byte for byte.
func TestGoldenPrograms(t *testing.T) {
	f, err := os.Open("../../testdata/programs.golden")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want, err := golden.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	srcs, _ := filepath.Glob("../../testdata/programs/*.c")
	if len(srcs) != len(want) || len(want) < 10 {
		t.Fatalf("%d programs, %d golden records", len(srcs), len(want))
	}
	for i, p := range srcs {
		if n := strings.TrimSuffix(filepath.Base(p), ".c"); want[i].Name != n {
			t.Fatalf("record %d is %q, program is %q", i, want[i].Name, n)
		}
	}
	tc := oracletest.Require(t)
	rn, err := oracle.NewRunner(context.Background(), oracle.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rn.Close(context.Background())
	got, err := corpus.Programs(context.Background(), tc, rn, "../../testdata/programs", oracle.CompileOptions{Std: "gnu17"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("%s: oracle gave %+v, golden has %+v", want[i].Name, got[i], want[i])
		}
	}
}

func TestProgramsErrors(t *testing.T) {
	tc := oracletest.Require(t)
	rn, _ := oracle.NewRunner(context.Background(), oracle.Limits{})
	defer rn.Close(context.Background())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.c"), []byte("int main(void){return y;}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.Programs(context.Background(), tc, rn, dir, oracle.CompileOptions{}); err == nil {
		t.Fatal("a program that does not compile must be an error")
	}
}

// Corpus runs, on the real corpora, when their directories are given.

func TestCTestsuite(t *testing.T) {
	dir := oracletest.Dir(t, "CORACLE_CTESTSUITE")
	tc := oracletest.Require(t)
	es, err := corpus.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 220 {
		t.Fatalf("%d tests, the pinned revision has 220", len(es))
	}
	rn, _ := oracle.NewRunner(context.Background(), oracle.Limits{})
	defer rn.Close(context.Background())
	recs := corpus.Run(context.Background(), tc, rn, es, oracle.CompileOptions{Std: "gnu17"}, 4)
	sum := corpus.Summary(recs)
	t.Logf("c-testsuite: %v", sum)
	// The oracle must agree with the suite's own expectations, bar the
	// recorded exception (00187 calls a function through a wrong type).
	for _, r := range recs {
		if r.Outcome != corpus.Pass && r.Path != "00187.c" {
			t.Errorf("%s: %s %s", r.Path, r.Outcome, r.Detail)
		}
	}
}
