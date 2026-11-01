package corpus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-embedded-c/c/internal/golden"
	"github.com/go-embedded-c/c/internal/oracle"
)

// Programs compiles and runs every .c file directly under dir (the committed
// golden programs, written for this repository) and returns the oracle's
// answer for each, sorted by name. A program that does not compile is an
// error: a golden program must be valid C.
func Programs(ctx context.Context, tc *oracle.Toolchain, rn *oracle.Runner, dir string, co oracle.CompileOptions) ([]golden.Record, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.c"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	recs := make([]golden.Record, 0, len(names))
	for _, p := range names {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		wasm, err := tc.Compile(ctx, src, co)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		res, err := rn.Run(ctx, wasm, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		if res.TimedOut || res.Truncated {
			return nil, fmt.Errorf("%s: timed out or produced too much output", filepath.Base(p))
		}
		recs = append(recs, golden.Record{
			Name: strings.TrimSuffix(filepath.Base(p), ".c"), Exit: res.Exit, Trap: res.Trap,
			Stdout: res.Stdout, Stderr: res.Stderr,
		})
	}
	return recs, nil
}
