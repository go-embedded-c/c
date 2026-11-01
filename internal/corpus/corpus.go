// Package corpus runs a directory of C programs through the oracle.
//
// Two corpora are read, neither of them ever committed (the gcc testsuite is
// GPL, c-testsuite licenses each test in its own .otag file): the gcc
// torture tests (exit status 0 = pass) and c-testsuite's single-exec tests
// (exit status 0 and stdout equal to the file named <test>.c.expected).
package corpus

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/go-embedded-c/c/internal/oracle"
)

// Pinned revisions of the corpora (bib/c-cpp.md §0 of go-scikit/roadmap).
const (
	GCCTag         = "releases/gcc-15.2.0"
	GCCCommit      = "5115c7e447fc07457443df874bf57840e8316d5f"
	GCCSubdir      = "gcc/testsuite/gcc.c-torture/execute"
	CTestsuiteRepo = "https://github.com/c-testsuite/c-testsuite"
	CTestsuiteRev  = "5c7275656d751de0e68b2d340a95b5681858ed07"
	CTestsuiteDir  = "tests/single-exec"
)

// Outcome classifies one program.
type Outcome string

// The outcomes. Only Pass is success.
const (
	Pass        Outcome = "pass"
	CompileFail Outcome = "compile-fail"
	Fail        Outcome = "fail"    // nonzero exit, or stdout differs from .expected
	Trap        Outcome = "trap"    // the guest trapped
	Timeout     Outcome = "timeout" // the run exceeded its bound
	Harness     Outcome = "harness" // the harness itself failed (invalid module, compiler crash)
)

// Entry is one program of a corpus.
type Entry struct {
	Path     string // slash-separated, relative to the corpus root
	Source   []byte
	Expected []byte // stdout wanted, when Path+".expected" exists
	HasExp   bool
}

// Load reads every .c file below root (sorted by path), with its .expected
// file when there is one.
func Load(root string) ([]Entry, error) {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && strings.HasSuffix(p, ".c") {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	es := make([]Entry, 0, len(paths))
	for _, p := range paths {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			return nil, err
		}
		e := Entry{Path: p, Source: src}
		exp, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)) + ".expected")
		if err == nil {
			e.Expected, e.HasExp = exp, true
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		es = append(es, e)
	}
	return es, nil
}

// Rec is the verdict on one entry.
type Rec struct {
	Path    string
	Outcome Outcome
	Exit    uint32
	Detail  string // trap text, or the first compiler diagnostic line
}

// Judge classifies a run of e that produced res.
func Judge(e Entry, res oracle.Result) Rec {
	r := Rec{Path: e.Path, Exit: res.Exit}
	switch {
	case res.TimedOut:
		r.Outcome = Timeout
	case res.Trap != "":
		r.Outcome, r.Detail = Trap, res.Trap
	case res.Exit != 0:
		r.Outcome = Fail
	case e.HasExp && !bytes.Equal(res.Stdout, e.Expected):
		r.Outcome, r.Detail = Fail, "stdout differs from .expected"
	default:
		r.Outcome = Pass
	}
	return r
}

// Run compiles and runs every entry with the oracle, workers at a time, and
// returns the records in entry order.
func Run(ctx context.Context, tc *oracle.Toolchain, rn *oracle.Runner, es []Entry, co oracle.CompileOptions, workers int) []Rec {
	if workers < 1 {
		workers = 1
	}
	recs := make([]Rec, len(es))
	var wg sync.WaitGroup
	idx := make(chan int)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				recs[i] = one(ctx, tc, rn, es[i], co)
			}
		}()
	}
	for i := range es {
		idx <- i
	}
	close(idx)
	wg.Wait()
	return recs
}

func one(ctx context.Context, tc *oracle.Toolchain, rn *oracle.Runner, e Entry, co oracle.CompileOptions) Rec {
	wasm, err := tc.Compile(ctx, e.Source, co)
	if err != nil {
		if ce, ok := err.(*oracle.CompileError); ok {
			first, _, _ := strings.Cut(strings.TrimSpace(ce.Diagnostics), "\n")
			return Rec{Path: e.Path, Outcome: CompileFail, Detail: first}
		}
		return Rec{Path: e.Path, Outcome: Harness, Detail: err.Error()}
	}
	res, err := rn.Run(ctx, wasm, nil, nil)
	if err != nil {
		return Rec{Path: e.Path, Outcome: Harness, Detail: err.Error()}
	}
	return Judge(e, res)
}

// Summary counts records by outcome.
func Summary(recs []Rec) map[Outcome]int {
	m := map[Outcome]int{}
	for _, r := range recs {
		m[r.Outcome]++
	}
	return m
}

// Report writes one line per record (path, outcome, exit, detail) and a
// summary; per-record, not only totals, so two runs can be diffed.
func Report(w io.Writer, recs []Rec) {
	for _, r := range recs {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", r.Path, r.Outcome, r.Exit, strings.ReplaceAll(r.Detail, "\n", " "))
	}
	sum := Summary(recs)
	fmt.Fprintf(w, "# total %d", len(recs))
	for _, o := range []Outcome{Pass, CompileFail, Fail, Trap, Timeout, Harness} {
		fmt.Fprintf(w, " %s=%d", o, sum[o])
	}
	fmt.Fprintln(w)
}
