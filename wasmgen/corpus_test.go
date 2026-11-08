package wasmgen_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-embedded-c/c/internal/oracle/oracletest"
	"github.com/go-embedded-c/c/wasmgen"
)

// gccTortureFloor is how many gcc 15.2.0 gcc.c-torture/execute files we
// compile, link and run with clang's exit status and output. A change of
// the generator must not lower it.
const gccTortureFloor = 1003 // W4c, measured 2026-11-08: 1003 same, 0 different, 2 known (W4b: 952, W4a: 762, W3: 454, W2: 10)

// knownDifferent are files whose result differs from clang's for a reason
// that is not the generator's, each examined. A file listed here that
// starts to agree is an error, so that the list stays true.
var knownDifferent = map[string]string{
	// The oracle compiles with -std=gnu89 (internal/oracle), where the
	// decimal constant 2810079215, too large for a 32-bit long, is an
	// unsigned long, so -2810079215 in expected_b[28] is 1484888081; cc/v4
	// and the test follow C99, where it is a long long. clang reports the
	// b >> 28 mismatch and aborts; our program passes. go-embedded-c/c#20.
	"shiftdi-2.c": "C89 vs C99 type of a decimal constant",
	// The test prints four addresses of locals with %p. Our frame is laid
	// out differently from clang's, so the addresses differ (measured
	// 2026-11-08: four times 0xfff8f against four times 0xfffa5); both
	// programs exit 0, and the four values agree within each program.
	"return-addr.c": "prints the addresses of stack objects",
}

// TestCorpusGCCTorture runs the generator over the gcc torture suite and
// counts each file's fate: the same as clang, refused (by cause), or
// different. A file clang itself refuses does not count.
func TestCorpusGCCTorture(t *testing.T) {
	dir := oracletest.Dir(t, "CORACLE_GCC_TORTURE")
	h := newHarness(t)
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".c") {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	pass, frontRefused, differ := 0, 0, []string{}
	causes := map[string]int{}
	for _, p := range files {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		ast, err := h.front.Translate(p, src)
		if err != nil {
			frontRefused++
			continue
		}
		obj, err := wasmgen.Compile(ast, p)
		var u *wasmgen.Unsupported
		if errors.As(err, &u) {
			causes[u.What]++
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		mod, err := h.tc.Link(context.Background(), obj)
		if err != nil {
			causes["link: "+firstLine(err.Error())]++
			continue
		}
		a, err := h.r.Run(context.Background(), mod, nil, nil)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		b, err := h.theirs(string(src))
		if err != nil {
			// We compiled it and clang did not: name it, so the count can be
			// followed back to the files.
			t.Logf("refused by clang, compiled by us: %s: %s", filepath.Base(p), firstLine(err.Error()))
			frontRefused++
			continue
		}
		name := filepath.Base(p)
		_, known := knownDifferent[name]
		switch {
		case same(a, b) && known:
			t.Errorf("%s agrees with clang now: drop it from knownDifferent", name)
		case same(a, b):
			pass++
		case known:
			t.Logf("known difference: %s: %s", name, knownDifferent[name])
		default:
			differ = append(differ, name)
			t.Logf("%s:\nours  exit %d trap %q stdout %q\nclang exit %d trap %q stdout %q",
				name, a.Exit, a.Trap, a.Stdout, b.Exit, b.Trap, b.Stdout)
		}
	}
	type kv struct {
		k string
		v int
	}
	var top []kv
	for k, v := range causes {
		top = append(top, kv{k, v})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].v > top[j].v || top[i].v == top[j].v && top[i].k < top[j].k })
	for i, c := range top {
		if i == 15 {
			break
		}
		t.Logf("refused %4d  %s", c.v, c.k)
	}
	t.Logf("gcc torture: %d same as clang, %d different, %d refused by wasmgen, %d refused by the front end, of %d",
		pass, len(differ), len(files)-pass-len(differ)-frontRefused, frontRefused, len(files))
	for _, d := range differ {
		t.Errorf("different from clang: %s", d)
	}
	if pass < gccTortureFloor {
		t.Errorf("%d files the same as clang, the floor is %d", pass, gccTortureFloor)
	}
}

// firstLine is the first line of a linker error without the temporary
// path, a stable label for counting.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if i := strings.LastIndex(line, ".o: "); i >= 0 {
		line = line[i+len(".o: "):]
	}
	return line
}
