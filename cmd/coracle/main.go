// Command coracle drives the reference C compiler (clang → wasm32-wasip1,
// run under wazero) and the cc/v4 parse smoke test.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/go-embedded-c/c/internal/corpus"
	"github.com/go-embedded-c/c/internal/cparse"
	"github.com/go-embedded-c/c/internal/golden"
	"github.com/go-embedded-c/c/internal/oracle"
	"github.com/go-embedded-c/c/internal/provision"
)

// osExit is replaced by tests.
var osExit = os.Exit

func main() {
	err := RootCmd().Execute()
	var ec exitCode
	switch {
	case errors.As(err, &ec):
		osExit(int(ec))
	case err != nil:
		osExit(1)
	}
}

// exitCode is returned by `run` to carry the program's exit status.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// RootCmd builds the command tree.
func RootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "coracle",
		Short:         "C oracle: clang to wasm32-wasip1 under wazero, and the cc/v4 parse smoke test",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(versionCmd(), toolchainCmd(), runCmd(), goldenCmd(), corpusCmd(), parseCmd(), predefinedCmd())
	return root
}

func locate(ctx context.Context) (*oracle.Toolchain, error) {
	tc, err := oracle.Locate()
	if err != nil {
		return nil, err
	}
	return tc, tc.Verify(ctx)
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "print the pinned versions and the toolchain found", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "clang %s (target %s)\nwasi-libc %s\ncc %s\n", oracle.ClangVersion, oracle.Target, oracle.WasiLibcCommit, cparse.Version)
			if _, err := locate(cmd.Context()); err != nil {
				fmt.Fprintf(w, "toolchain: %v\n", err)
			} else {
				fmt.Fprintln(w, "toolchain: ok")
			}
			return nil
		},
	}
}

func toolchainCmd() *cobra.Command {
	var home string
	var jobs int
	c := &cobra.Command{
		Use: "toolchain", Short: "download and build the pinned oracle toolchain into --home", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if home == "" {
				return errors.New("--home is required")
			}
			return provision.Install(cmd.Context(), provision.Options{Home: home, Log: cmd.ErrOrStderr(), Jobs: jobs})
		},
	}
	c.Flags().StringVar(&home, "home", os.Getenv(oracle.EnvHome), "directory to install into (default $"+oracle.EnvHome+")")
	c.Flags().IntVar(&jobs, "jobs", 4, "parallel build jobs")
	return c
}

func compileFlags(c *cobra.Command, co *oracle.CompileOptions) {
	c.Flags().StringVar(&co.Opt, "opt", "-O0", "optimisation flag")
	// The gcc torture suite is K&R-era code; our own programs are C17.
	std := "gnu17"
	if c.Name() == "corpus" {
		std = "gnu89"
	}
	c.Flags().StringVar(&co.Std, "std", std, "language standard")
}

func runCmd() *cobra.Command {
	var co oracle.CompileOptions
	c := &cobra.Command{
		Use: "run FILE.c", Short: "compile FILE.c with the oracle, run it, and relay stdout, stderr and exit status", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			tc, err := locate(ctx)
			if err != nil {
				return err
			}
			src, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			rn, err := oracle.NewRunner(ctx, oracle.Limits{})
			if err != nil {
				return err
			}
			defer rn.Close(ctx)
			res, err := tc.Output(ctx, rn, src, co)
			if err != nil {
				return err
			}
			cmd.OutOrStdout().Write(res.Stdout)
			cmd.ErrOrStderr().Write(res.Stderr)
			if res.Trap != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "trap: %s\n", res.Trap)
				return exitCode(134)
			}
			if res.TimedOut {
				return errors.New("timed out")
			}
			if res.Exit != 0 {
				return exitCode(res.Exit)
			}
			return nil
		},
	}
	compileFlags(c, &co)
	return c
}

func goldenCmd() *cobra.Command {
	var dir, out string
	var co oracle.CompileOptions
	c := &cobra.Command{
		Use: "golden", Short: "regenerate the golden file from the committed programs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			tc, err := locate(ctx)
			if err != nil {
				return err
			}
			rn, err := oracle.NewRunner(ctx, oracle.Limits{})
			if err != nil {
				return err
			}
			defer rn.Close(ctx)
			recs, err := corpus.Programs(ctx, tc, rn, dir, co)
			if err != nil {
				return err
			}
			f, err := os.Create(out)
			if err != nil {
				return err
			}
			if err := golden.Encode(f, recs); err != nil {
				f.Close()
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d programs -> %s\n", len(recs), out)
			return f.Close()
		},
	}
	c.Flags().StringVar(&dir, "dir", "testdata/programs", "directory of programs")
	c.Flags().StringVar(&out, "out", "testdata/programs.golden", "golden file to write")
	compileFlags(c, &co)
	return c
}

func corpusCmd() *cobra.Command {
	var dir, out string
	var jobs int
	var co oracle.CompileOptions
	c := &cobra.Command{
		Use: "corpus", Short: "run every .c file below --dir through the oracle (gcc torture execute, c-testsuite single-exec)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" {
				return errors.New("--dir is required")
			}
			ctx := cmd.Context()
			tc, err := locate(ctx)
			if err != nil {
				return err
			}
			es, err := corpus.Load(dir)
			if err != nil {
				return err
			}
			rn, err := oracle.NewRunner(ctx, oracle.Limits{Timeout: 20 * time.Second})
			if err != nil {
				return err
			}
			defer rn.Close(ctx)
			recs := corpus.Run(ctx, tc, rn, es, co, jobs)
			w := cmd.OutOrStdout()
			if out != "" {
				f, err := os.Create(out)
				if err != nil {
					return err
				}
				defer f.Close()
				corpus.Report(f, recs)
			}
			sum := corpus.Summary(recs)
			fmt.Fprintf(w, "%d files: pass=%d compile-fail=%d fail=%d trap=%d timeout=%d harness=%d\n", len(recs),
				sum[corpus.Pass], sum[corpus.CompileFail], sum[corpus.Fail], sum[corpus.Trap], sum[corpus.Timeout], sum[corpus.Harness])
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "corpus root")
	c.Flags().StringVar(&out, "out", "", "write one line per file here")
	c.Flags().IntVar(&jobs, "jobs", max(1, runtime.NumCPU()/2), "parallel compilations")
	compileFlags(c, &co)
	return c
}

func parseCmd() *cobra.Command {
	var dir, out string
	var check bool
	var jobs int
	c := &cobra.Command{
		Use: "parse", Short: "run modernc.org/cc/v4 over every .c file below --dir (wasm32 ABI, no host compiler)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" {
				return errors.New("--dir is required")
			}
			ctx := cmd.Context()
			tc, err := locate(ctx)
			if err != nil {
				return err
			}
			inc, err := tc.ResourceInclude(ctx)
			if err != nil {
				return err
			}
			es, err := corpus.Load(dir)
			if err != nil {
				return err
			}
			lvl := cparse.ParseOnly
			if check {
				lvl = cparse.Check
			}
			errs := make([]error, len(es))
			var wg sync.WaitGroup
			idx := make(chan int)
			for range max(1, jobs) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					fr, _ := cparse.New(inc, tc.Sysroot)
					for i := range idx {
						errs[i] = fr.Source(filepath.Join(dir, filepath.FromSlash(es[i].Path)), nil, lvl)
					}
				}()
			}
			for i := range es {
				idx <- i
			}
			close(idx)
			wg.Wait()
			ok := 0
			var f *os.File
			if out != "" {
				if f, err = os.Create(out); err != nil {
					return err
				}
				defer f.Close()
			}
			for i, e := range errs {
				if e == nil {
					ok++
				} else if f != nil {
					fmt.Fprintf(f, "%s\t%s\n", es[i].Path, firstLine(e.Error()))
				}
			}
			level := "parse"
			if check {
				level = "check"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cc %s (%s): %d / %d files\n", cparse.Version, level, ok, len(es))
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "corpus root")
	c.Flags().StringVar(&out, "out", "", "write the failing files and their first error here")
	c.Flags().BoolVar(&check, "check", false, "also run the type checker")
	c.Flags().IntVar(&jobs, "jobs", max(1, runtime.NumCPU()/2), "parallel parsers")
	return c
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

func predefinedCmd() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use: "predefined", Short: "regenerate internal/cparse/predefined.h from the oracle clang", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tc, err := locate(cmd.Context())
			if err != nil {
				return err
			}
			b, err := cparse.GeneratePredefined(cmd.Context(), tc.Clang)
			if err != nil {
				return err
			}
			return os.WriteFile(out, b, 0o644)
		},
	}
	c.Flags().StringVar(&out, "out", "internal/cparse/predefined.h", "file to write")
	return c
}
