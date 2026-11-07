// Package cparse is the front end of the M0 smoke test: modernc.org/cc/v4
// configured by hand for wasm32, with no host C compiler involved at parse
// time (NewConfig, which execs `cc`, is not used).
//
// The wasm32 ABI is the one clang defines (clang/lib/Basic/Targets/
// WebAssembly.h): ILP32, char signed, long double IEEE quad (16 bytes),
// 8-byte alignment for double and long long. cc/v4's NewABI knows no wasm
// target, so the ABI is the linux/386 one with the differences patched in;
// the point of M1 is to upstream a real one.
package cparse

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime/debug"

	"github.com/go-embedded-c/c/internal/cc"
)

// Version is the pinned front end: internal/cc is modernc.org/cc/v4 at this
// version, patched (internal/cmd/vendorcc).
const Version = "v4.30.5"

//go:embed predefined.h
var predefined string

// Predefined returns the predefined-macro block of clang 23.1.4 for
// wasm32-wasip1 (regenerate with `coracle predefined`).
func Predefined() string { return predefined }

// GeneratePredefined runs `clang --target=wasm32-wasip1 -dM -E` on an empty
// input (clang's default language standard) and returns the macro block.
func GeneratePredefined(ctx context.Context, clang string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, clang, "--target=wasm32-wasip1", "-dM", "-E", "-x", "c", "-")
	cmd.Stdin = bytes.NewReader(nil)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("cparse: %s -dM -E: %w", clang, err)
	}
	return out, nil
}

// ABI returns the wasm32 ABI.
func ABI() (*cc.ABI, error) {
	abi, err := cc.NewABI("linux", "386")
	if err != nil {
		return nil, err
	}
	abi.SignedChar = true
	t := abi.Types
	for _, k := range []cc.Kind{cc.Double, cc.LongLong, cc.ULongLong, cc.Float32x, cc.Float64} {
		t[k] = cc.AbiType{Size: 8, Align: 8, FieldAlign: 8}
	}
	for _, k := range []cc.Kind{cc.LongDouble, cc.Float64x, cc.Float128, cc.Int128, cc.UInt128} {
		t[k] = cc.AbiType{Size: 16, Align: 16, FieldAlign: 16}
	}
	t[cc.ComplexFloat] = cc.AbiType{Size: 8, Align: 4, FieldAlign: 4}
	t[cc.ComplexDouble] = cc.AbiType{Size: 16, Align: 8, FieldAlign: 8}
	t[cc.ComplexLongDouble] = cc.AbiType{Size: 32, Align: 16, FieldAlign: 16}
	return abi, nil
}

// Front is a configured parser.
type Front struct {
	cfg *cc.Config
}

// New builds a front end whose #include <…> path is: clang's resource
// headers, then the sysroot's per-target and generic include directories.
func New(resourceInclude, sysroot string) (*Front, error) {
	abi, err := ABI()
	if err != nil {
		return nil, err
	}
	return &Front{cfg: &cc.Config{
		ABI:             abi,
		Predefined:      predefined,
		IncludePaths:    []string{"@"}, // #include "x.h" from the including file's directory
		SysIncludePaths: []string{resourceInclude, filepath.Join(sysroot, "include", "wasm32-wasip1"), filepath.Join(sysroot, "include")},
		UnsignedEnums:   true, // gcc compatibility, what the torture suite expects
	}}, nil
}

// PanicError is a panic inside cc/v4, recovered: the front end is fed
// untrusted text and must never take the process down. Stack is where it
// happened, for the fuzz test's list of known (filed) defects.
type PanicError struct {
	Value any
	Stack string
}

func (e *PanicError) Error() string { return fmt.Sprintf("cparse: panic in cc: %v", e.Value) }

// Level is how far a file was taken.
type Level int

// The levels.
const (
	ParseOnly Level = iota // preprocess and parse
	Check                  // plus the type checker
)

// File preprocesses and parses (and, at level Check, type-checks) one file,
// reading it from path. A panic inside cc is returned as an error: the front
// end is fed untrusted text.
func (f *Front) File(path string, lvl Level) error { return f.Source(path, nil, lvl) }

// Source is File over in-memory text (src != nil) or a file (src == nil).
func (f *Front) Source(name string, src []byte, lvl Level) error {
	_, err := f.run(name, src, lvl)
	return err
}

// Translate is Source at level Check, returning the checked AST: what the
// code generator compiles.
func (f *Front) Translate(name string, src []byte) (*cc.AST, error) {
	return f.run(name, src, Check)
}

func (f *Front) run(name string, src []byte, lvl Level) (ast *cc.AST, err error) {
	defer func() {
		if r := recover(); r != nil {
			ast, err = nil, &PanicError{Value: r, Stack: string(debug.Stack())}
		}
	}()
	main := cc.Source{Name: name}
	if src != nil {
		main.Value = string(src)
	}
	srcs := []cc.Source{
		{Name: "<predefined>", Value: f.cfg.Predefined},
		{Name: "<builtin>", Value: cc.Builtin},
		main,
	}
	if lvl == Check {
		return cc.Translate(f.cfg, srcs)
	}
	return cc.Parse(f.cfg, srcs)
}
