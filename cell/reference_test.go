package cell

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-embedded-c/c/internal/oracle"
)

// benchTU is benchCell as clang sees it: the statements wrapped in a
// function, as Analyze does.
var benchTU = func() string {
	head, tail, _ := strings.Cut(benchCell, "double total = 0;\n")
	return head + "double total;\nvoid __cell(void) {\ntotal = 0;\n" + tail + "}\n"
}()

// BenchmarkClangSyntaxOnly is the reference for BenchmarkAnalyze: clang
// 23.1.4 checking the same translation unit for wasm32-wasip1, process start
// included (it is what a host compiler costs per cell). Skipped without the
// oracle toolchain.
func BenchmarkClangSyntaxOnly(b *testing.B) {
	tc, err := oracle.Locate()
	if errors.Is(err, oracle.ErrNoToolchain) {
		b.Skip(err)
	}
	if err != nil {
		b.Fatal(err)
	}
	args := []string{"--target=" + oracle.Target, "--sysroot=" + tc.Sysroot, "-isystem", filepath.Join(tc.Sysroot, "include", oracle.Target), "-fsyntax-only", "-x", "c", "-"}
	for b.Loop() {
		cmd := exec.Command(tc.Clang, args...)
		cmd.Stdin = strings.NewReader(benchTU)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			b.Fatalf("%v: %s", err, stderr.String())
		}
	}
}
