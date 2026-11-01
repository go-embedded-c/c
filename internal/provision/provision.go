// Package provision installs the pinned oracle toolchain into a home
// directory: the LLVM 23.1.4 release (checksummed), and a wasi-libc sysroot
// built from the pinned commit with that clang.
//
// Layout of the home directory (see oracle.HomeLayout):
//
//	dl/<asset>     the downloaded release archive
//	llvm/          the extracted release (bin/clang, bin/wasm-ld, …)
//	src/wasi-libc/ the pinned wasi-libc checkout
//	build/         the cmake build tree
//	sysroot/       the installed sysroot
//
// Every step is skipped when its output exists, so a rerun repairs a home.
package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/go-embedded-c/c/internal/oracle"
)

// Asset is a release archive and the sha256 GitHub publishes for it.
type Asset struct {
	Name   string
	SHA256 string
}

// URL returns where the asset is served from.
func (a Asset) URL() string {
	return "https://github.com/llvm/llvm-project/releases/download/llvmorg-" + oracle.ClangVersion + "/" + a.Name
}

// AssetFor returns the pinned release archive for a GOOS/GOARCH.
func AssetFor(goos, goarch string) (Asset, error) {
	switch goos + "/" + goarch {
	case "darwin/arm64":
		return Asset{"LLVM-23.1.4-macOS-ARM64.tar.zst", "0f8b0db925ab54a63028d788e85b14abaa25492e9608819728c17f6786948da4"}, nil
	case "linux/amd64":
		return Asset{"LLVM-23.1.4-Linux-X64.tar.zst", "efdd1dbd089b99bfcfcde7b8eb1236163f2e88df5a981cca4f783ec9cb5e0378"}, nil
	}
	return Asset{}, fmt.Errorf("provision: no pinned LLVM %s release for %s/%s", oracle.ClangVersion, goos, goarch)
}

// Options configure Install.
type Options struct {
	Home string
	Log  io.Writer
	Jobs int // cmake --parallel (4 when zero)
	// WasiLibcURL is the git remote of wasi-libc.
	WasiLibcURL string
	// BaseURL, when set, replaces the release download URL prefix (tests).
	BaseURL string
}

// FileSHA256 returns the hex sha256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Download fetches url into dst and checks its sha256; a file already there
// with the right sum is kept. A wrong sum deletes the file and is an error.
func Download(ctx context.Context, url, dst, want string) error {
	if got, err := FileSHA256(dst); err == nil && got == want {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provision: GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	got, err := FileSHA256(dst)
	if err != nil {
		return err
	}
	if got != want {
		os.Remove(dst)
		return fmt.Errorf("provision: %s has sha256 %s, want %s", url, got, want)
	}
	return nil
}

func run(ctx context.Context, log io.Writer, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("provision: %s %v: %w", name, args, err)
	}
	return nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// extract unpacks a .tar.zst into dir, stripping the top directory. The zstd
// window of the LLVM archives is 1 GiB, hence --long=31.
func extract(ctx context.Context, archive, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	zs := exec.CommandContext(ctx, "zstd", "-d", "-q", "--long=31", "-c", archive)
	tar := exec.CommandContext(ctx, "tar", "-x", "--strip-components=1", "-C", dir)
	pr, pw := io.Pipe()
	zs.Stdout, tar.Stdin = pw, pr
	var zerr, terr error
	zs.Stderr, tar.Stderr = os.Stderr, os.Stderr
	if terr = tar.Start(); terr != nil {
		return terr
	}
	if zerr = zs.Start(); zerr != nil {
		pw.Close()
		tar.Wait()
		return zerr
	}
	zerr = zs.Wait()
	pw.Close()
	terr = tar.Wait()
	pr.Close()
	if zerr != nil {
		return fmt.Errorf("provision: zstd: %w", zerr)
	}
	if terr != nil {
		return fmt.Errorf("provision: tar: %w", terr)
	}
	return nil
}

// Install provisions o.Home.
func Install(ctx context.Context, o Options) error {
	log := o.Log
	if log == nil {
		log = io.Discard
	}
	home, err := filepath.Abs(o.Home)
	if err != nil {
		return err
	}
	o.Home = home
	jobs := o.Jobs
	if jobs == 0 {
		jobs = 4
	}
	remote := o.WasiLibcURL
	if remote == "" {
		remote = "https://github.com/WebAssembly/wasi-libc"
	}
	clang, sysroot := oracle.HomeLayout(o.Home)
	if exists(clang) && exists(filepath.Join(sysroot, "lib", oracle.Target, "libc.a")) {
		fmt.Fprintln(log, "provision: already installed")
		return nil
	}
	asset, err := AssetFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	archive := filepath.Join(o.Home, "dl", asset.Name)
	url := asset.URL()
	if o.BaseURL != "" {
		url = o.BaseURL + "/" + asset.Name
	}
	llvm := filepath.Join(o.Home, "llvm")
	if !exists(clang) {
		fmt.Fprintf(log, "provision: downloading %s\n", url)
		if err := Download(ctx, url, archive, asset.SHA256); err != nil {
			return err
		}
		fmt.Fprintln(log, "provision: extracting")
		if err := extract(ctx, archive, llvm); err != nil {
			return err
		}
	}
	src := filepath.Join(o.Home, "src", "wasi-libc")
	if !exists(filepath.Join(src, "CMakeLists.txt")) {
		fmt.Fprintf(log, "provision: fetching wasi-libc %s\n", oracle.WasiLibcCommit)
		if err := os.MkdirAll(src, 0o755); err != nil {
			return err
		}
		for _, a := range [][]string{
			{"init", "-q"},
			{"fetch", "-q", "--depth", "1", remote, oracle.WasiLibcCommit},
			{"checkout", "-q", "FETCH_HEAD"},
		} {
			if err := run(ctx, log, src, "git", a...); err != nil {
				return err
			}
		}
	}
	build := filepath.Join(o.Home, "build")
	bin := filepath.Join(llvm, "bin")
	fmt.Fprintln(log, "provision: building the sysroot")
	if err := run(ctx, log, o.Home, "cmake", "-S", src, "-B", build,
		"-DCMAKE_C_COMPILER="+clang,
		"-DCMAKE_AR="+filepath.Join(bin, "llvm-ar"),
		"-DCMAKE_NM="+filepath.Join(bin, "llvm-nm"),
		"-DCMAKE_RANLIB="+filepath.Join(bin, "llvm-ranlib"),
		"-DTARGET_TRIPLE="+oracle.Target,
		"-DCMAKE_INSTALL_PREFIX="+sysroot); err != nil {
		return err
	}
	if err := run(ctx, log, o.Home, "cmake", "--build", build, "--parallel", fmt.Sprint(jobs)); err != nil {
		return err
	}
	if err := run(ctx, log, o.Home, "cmake", "--install", build); err != nil {
		return err
	}
	return installBuiltins(o.Home, llvm)
}

// installBuiltins copies the compiler-rt builtins that the wasi-libc build
// fetched (the release has none for wasm32) into the clang resource dir, so
// that linking a program finds libclang_rt.builtins.a.
func installBuiltins(home, llvm string) error {
	const rel = "lib/wasm32-unknown-wasip1/libclang_rt.builtins.a"
	from := filepath.Join(home, "build", "temp-resource-dir", filepath.FromSlash(rel))
	majors, err := filepath.Glob(filepath.Join(llvm, "lib", "clang", "*"))
	if err != nil || len(majors) != 1 {
		return fmt.Errorf("provision: expected one resource dir under %s/lib/clang, found %v", llvm, majors)
	}
	to := filepath.Join(majors[0], filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("provision: builtins: %w", err)
	}
	return os.WriteFile(to, data, 0o644)
}
