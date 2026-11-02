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

// TestHostFilesUnreachable: a cell that names a host file is refused twice
// over. The directive guard refuses it, and internal/cc, given the cell
// without the guard, no longer falls back to os.Open for a file missing from
// Config.FS (go-embedded-c/c#4). The control is the same parse with FS nil,
// cc/v4's host mode, which does read the file: the test can see the defect.
func TestHostFilesUnreachable(t *testing.T) {
	rel := hostHeader(t)
	cfg, err := config(DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	// The search directories are "include/all" and "include/wasi": two more
	// ".." reach the working directory.
	cell := "#include \"../../" + rel + ".h\"\nint *p = &host_secret;\n"

	host := *cfg
	host.FS = nil
	host.IncludePaths = []string{"."}
	host.SysIncludePaths = []string{"."}
	if _, err := parse(&host, "#include \""+rel+".h\"\nint *p = &host_secret;\n", true); err != nil {
		t.Fatalf("control: with FS nil cc did not read the host file: %v", firstLine(err.Error()))
	}
	_, err = parse(cfg, cell, true)
	if err == nil || !strings.Contains(err.Error(), "include file not found") {
		t.Fatalf("internal/cc reached the host file system: %v", err)
	}
	a := Analyze([]byte(cell), Context{}, Limits{})
	if len(a.Diagnostics) == 0 || strings.Contains(a.Diagnostics[0].Msg, "undefined") {
		t.Fatalf("Analyze did not refuse the include itself: %v", a.Diagnostics)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
