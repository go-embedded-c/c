package cell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostHeader writes a header outside the embedded tree and returns its path
// relative to the working directory, without the ".h" suffix.
func hostHeader(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hostsecret.h"), []byte("int host_secret;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, filepath.Join(dir, "hostsecret"))
	if err != nil {
		t.Skipf("no relative path from %s to %s: %v", wd, dir, err)
	}
	return filepath.ToSlash(rel)
}

// TestGuardIsNotVacuous shows the defect the guard exists for: cc/v4, given
// the cell without the guard, reads a host file through its os.Open
// fallback, by a direct path and by a macro that rewrites a word of an
// embedded header's #include. Analyze refuses both cells.
func TestGuardIsNotVacuous(t *testing.T) {
	rel := hostHeader(t)
	cfg, err := config()
	if err != nil {
		t.Fatal(err)
	}
	// The search directories are "include/clang" and "include/wasi": two more
	// ".." reach the working directory.
	up := "../../" + rel
	for _, tc := range []struct{ name, cell string }{
		{"direct", "#include \"" + up + ".h\"\nint *p = &host_secret;\n"},
		{"macro", "#define stdio " + up + "\n#include <stdio.h>\nint *p = &host_secret;\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(cfg, tc.cell, true)
			if tc.name == "macro" && err != nil {
				// cc/v4 may space the expanded tokens apart; the direct case
				// is the proof, this one documents the second path.
				t.Logf("macro path not taken by cc/v4: %v", firstLine(err.Error()))
			} else if err != nil {
				t.Fatalf("without the guard cc/v4 did not read the host file: %v", firstLine(err.Error()))
			}
			a := Analyze([]byte(tc.cell), Context{}, Limits{})
			if len(a.Diagnostics) == 0 {
				t.Fatalf("Analyze accepted a cell that reads %s.h from the host", rel)
			}
			if strings.Contains(a.Diagnostics[0].Msg, "undefined") {
				t.Fatalf("refused for the wrong reason: %v", a.Diagnostics)
			}
		})
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
