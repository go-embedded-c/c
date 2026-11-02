package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// clangResourceHeaders is the allowlist of clang resource headers copied
// into headers.ClangDir: the C library headers clang provides itself, and the
// wasm SIMD intrinsics. Every other file of the resource directory is an
// intrinsic header of another target, a GPU or offload wrapper, or a module
// map. tgmath.h is left out on purpose: clang's is built on
// __attribute__((overloadable)), which cc/v4 does not parse; libc's (musl's,
// on __typeof__ and _Generic-free conditionals) is found instead.
var clangResourceHeaders = []string{
	"__float_float.h", "__float_header_macro.h", "__float_infinity_nan.h",
	"__stdarg___gnuc_va_list.h", "__stdarg___va_copy.h", "__stdarg_header_macro.h",
	"__stdarg_va_arg.h", "__stdarg_va_copy.h", "__stdarg_va_list.h",
	"__stddef_header_macro.h", "__stddef_max_align_t.h", "__stddef_null.h",
	"__stddef_nullptr_t.h", "__stddef_offsetof.h", "__stddef_ptrdiff_t.h",
	"__stddef_rsize_t.h", "__stddef_size_t.h", "__stddef_unreachable.h",
	"__stddef_wchar_t.h", "__stddef_wint_t.h",
	"float.h", "inttypes.h", "iso646.h", "limits.h", "stdalign.h", "stdarg.h",
	"stdatomic.h", "stdbool.h", "stdckdint.h", "stdcountof.h", "stddef.h",
	"stdint.h", "stdnoreturn.h", "varargs.h", "wasm_simd128.h",
}

// Sources locates what Generate copies.
type Sources struct {
	ClangResourceInclude string // clang -print-resource-dir + /include
	WasiInclude          string // <sysroot>/include/wasm32-wasip1
	LLVMLicense          string // llvm-project clang/LICENSE.TXT
	WasiLicenses         string // directory holding wasi-libc's LICENSE* files
}

// clangPatches are the only changes made to the copied clang headers. Each
// replaces a __has_include_next probe whose answer is constant for
// wasm32-wasip1: cc/v4 evaluates the probe even behind a false `&&`, and
// a probe for a file missing from its fs.FS becomes a host os.Stat (24 % of
// the analysis time, and a host access). The results are the ones clang
// gets with the unpatched headers: __MVS__ is z/OS only, and wasi-libc has
// no stdatomic.h.
var clangPatches = map[string][2]string{
	"stddef.h":    {"#if defined(__MVS__) && __has_include_next(<stddef.h>)", "#if 0 /* defined(__MVS__) && __has_include_next(<stddef.h>): z/OS only (genheaders) */"},
	"stdarg.h":    {"#if defined(__MVS__) && __has_include_next(<stdarg.h>)", "#if 0 /* defined(__MVS__) && __has_include_next(<stdarg.h>): z/OS only (genheaders) */"},
	"stdatomic.h": {"__has_include_next(<stdatomic.h>) &&", "0 /* __has_include_next(<stdatomic.h>): wasi-libc has none (genheaders) */ &&"},
}

// Generate replaces the tree under dst (the include directory of this
// package) with a copy of the sources. It refuses a symbolic link or a
// non-regular file in the sysroot: the copy must hold exactly the bytes the
// oracle compiles against, clangPatches aside.
func Generate(dst string, src Sources) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	for _, name := range clangResourceHeaders {
		to := filepath.Join(dst, "clang", name)
		if err := copyFile(filepath.Join(src.ClangResourceInclude, name), to); err != nil {
			return err
		}
		if p, ok := clangPatches[name]; ok {
			if err := patch(to, p[0], p[1]); err != nil {
				return err
			}
		}
	}
	err := filepath.WalkDir(src.WasiInclude, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("headers: %s: not a regular file", p)
		}
		rel, err := filepath.Rel(src.WasiInclude, p)
		if err != nil {
			return err
		}
		return copyFile(p, filepath.Join(dst, "wasi", rel))
	})
	if err != nil {
		return err
	}
	if err := copyFile(src.LLVMLicense, filepath.Join(dst, "LICENSE-llvm.txt")); err != nil {
		return err
	}
	entries, err := os.ReadDir(src.WasiLicenses)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "LICENSE") && e.Type().IsRegular() {
			if err := copyFile(filepath.Join(src.WasiLicenses, e.Name()), filepath.Join(dst, "wasi-libc-"+e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// patch replaces exactly one occurrence of old in the file; any other count
// is an error, so that a new clang release cannot silently skip a patch.
func patch(file, old, new string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if n := strings.Count(string(b), old); n != 1 {
		return fmt.Errorf("genheaders: %s: %q occurs %d times, want 1", file, old, n)
	}
	return os.WriteFile(file, []byte(strings.Replace(string(b), old, new, 1)), 0o644)
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o644)
}
