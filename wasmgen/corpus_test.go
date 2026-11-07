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
const gccTortureFloor = 10 // W2, measured 2026-11-07: 10 same, 0 different

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
			frontRefused++ // clang refuses it too
			continue
		}
		if same(a, b) {
			pass++
		} else {
			differ = append(differ, filepath.Base(p))
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
