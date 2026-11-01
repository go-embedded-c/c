// Package oracle drives the reference C compiler as a differential oracle.
//
// The oracle of go-embedded-c is clang compiling to wasm32-wasip1 against a
// wasi-libc sysroot, the result executed under wazero: the same ABI, the same
// libc and the same engine as the kernel this repository will grow into, so a
// difference between the two is a compiler difference (bib/c-cpp.md §2.1 of
// go-scikit/roadmap). The versions are pinned below; Verify refuses any other.
package oracle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ClangVersion is the pinned LLVM/clang release (llvmorg-23.1.4, latest
// release on 2026-11-01).
const ClangVersion = "23.1.4"

// WasiLibcCommit is the pinned commit of github.com/WebAssembly/wasi-libc.
const WasiLibcCommit = "d9ad2d11991c51475d024b86757310f515fec704"

// Target is the clang target triple of the oracle.
const Target = "wasm32-wasip1"

// Environment variables that locate the toolchain.
const (
	EnvClang   = "CORACLE_CLANG"   // the clang executable
	EnvSysroot = "CORACLE_SYSROOT" // the wasi-libc sysroot
	EnvHome    = "CORACLE_HOME"    // a directory made by `coracle toolchain`
)

// ErrNoToolchain is wrapped by every error that means "the oracle toolchain is
// not installed here"; tests skip on it, they never fail.
var ErrNoToolchain = errors.New("oracle toolchain not available")

// Toolchain is a located oracle: a clang and a wasi-libc sysroot.
type Toolchain struct {
	Clang   string
	Sysroot string
}

// HomeLayout returns the clang and sysroot paths below a home directory made
// by the provision package.
func HomeLayout(home string) (clang, sysroot string) {
	return filepath.Join(home, "llvm", "bin", "clang"), filepath.Join(home, "sysroot")
}

// Locate finds the toolchain from the environment: $CORACLE_CLANG and
// $CORACLE_SYSROOT, or else $CORACLE_HOME. It does not run anything.
func Locate() (*Toolchain, error) {
	clang, sysroot := os.Getenv(EnvClang), os.Getenv(EnvSysroot)
	if home := os.Getenv(EnvHome); home != "" && clang == "" && sysroot == "" {
		clang, sysroot = HomeLayout(home)
	}
	if clang == "" || sysroot == "" {
		return nil, fmt.Errorf("%w: set %s, or %s and %s", ErrNoToolchain, EnvHome, EnvClang, EnvSysroot)
	}
	return &Toolchain{Clang: clang, Sysroot: sysroot}, nil
}

// Verify checks that the clang is the pinned release and that the sysroot
// holds libc for the target. Anything else is an error: a golden produced by
// another clang would be a different oracle.
func (t *Toolchain) Verify(ctx context.Context) error {
	if _, err := os.Stat(t.Clang); err != nil {
		return fmt.Errorf("%w: %v", ErrNoToolchain, err)
	}
	libc := filepath.Join(t.Sysroot, "lib", Target, "libc.a")
	if _, err := os.Stat(libc); err != nil {
		return fmt.Errorf("%w: %v", ErrNoToolchain, err)
	}
	got, err := t.Version(ctx)
	if err != nil {
		return err
	}
	if got != ClangVersion {
		return fmt.Errorf("oracle: clang %s is installed, the pinned release is %s", got, ClangVersion)
	}
	return nil
}

// Version returns the clang version, from `clang --version`.
func (t *Toolchain) Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, t.Clang, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("oracle: %s --version: %w", t.Clang, err)
	}
	return ParseVersion(out)
}

// ParseVersion extracts "23.1.4" from the first line of `clang --version`
// ("clang version 23.1.4 (…)"; Apple's and Homebrew's builds say the same).
func ParseVersion(out []byte) (string, error) {
	const key = "clang version "
	i := bytes.Index(out, []byte(key))
	if i < 0 {
		return "", fmt.Errorf("oracle: no %q in %q", key, out)
	}
	rest := out[i+len(key):]
	end := bytes.IndexAny(rest, " \r\n")
	if end < 0 {
		end = len(rest)
	}
	if end == 0 {
		return "", fmt.Errorf("oracle: empty version in %q", out)
	}
	return string(rest[:end]), nil
}

// ResourceInclude returns clang's own header directory (stddef.h, stdarg.h…).
func (t *Toolchain) ResourceInclude(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, t.Clang, "-print-resource-dir").Output()
	if err != nil {
		return "", fmt.Errorf("oracle: -print-resource-dir: %w", err)
	}
	return filepath.Join(string(bytes.TrimSpace(out)), "include"), nil
}

// CompileOptions are the flags of one compilation.
type CompileOptions struct {
	// Opt is the optimisation flag ("-O0" when empty).
	Opt string
	// Std is the language standard ("gnu89" when empty: the gcc torture
	// suite is K&R-era code).
	Std string
	// Extra flags are appended last.
	Extra []string
	// Timeout bounds the compile (30 s when zero).
	Timeout time.Duration
}

// StackSize is the wasm stack, in bytes. clang's default is 64 KiB, too small
// for the corpora; wasm-ld places the stack first so growth is a clean trap.
const StackSize = 1 << 20

// Args returns the clang arguments for compiling stdin ("-x c -") to out.
func (t *Toolchain) Args(o CompileOptions, out string) []string {
	if o.Opt == "" {
		o.Opt = "-O0"
	}
	if o.Std == "" {
		o.Std = "gnu89"
	}
	args := []string{
		"--target=" + Target, "--sysroot=" + t.Sysroot,
		o.Opt, "-std=" + o.Std, "-w",
		fmt.Sprintf("-Wl,-z,stack-size=%d", StackSize),
		// wasi-libc leaves long double formatting out unless asked.
		"-lc-printscan-long-double",
	}
	args = append(args, o.Extra...)
	return append(args, "-x", "c", "-", "-o", out)
}

// CompileError is the compiler's refusal of a program (as opposed to a
// failure to run the compiler at all).
type CompileError struct{ Diagnostics string }

func (e *CompileError) Error() string { return "compile: " + e.Diagnostics }

// Compile compiles one C translation unit to a wasm module. A *CompileError
// means clang rejected the source.
func (t *Toolchain) Compile(ctx context.Context, src []byte, o CompileOptions) ([]byte, error) {
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "coracle-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "a.wasm")
	cmd := exec.CommandContext(ctx, t.Clang, t.Args(o, out)...)
	cmd.Stdin = bytes.NewReader(src)
	cmd.Env = []string{"LC_ALL=C"}
	var diag bytes.Buffer
	cmd.Stderr, cmd.Stdout = &diag, &diag
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("oracle: compile: %w", ctx.Err())
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, &CompileError{Diagnostics: diag.String()}
		}
		return nil, fmt.Errorf("oracle: running %s: %w", t.Clang, err)
	}
	return os.ReadFile(out)
}
