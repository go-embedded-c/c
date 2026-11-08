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
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/go-embedded-c/c/internal/oracle"
)

// Asset is a release archive, the sha256 GitHub publishes for it, and its
// REST API URL (the second address of the same file).
type Asset struct {
	Name   string
	SHA256 string
	APIURL string
}

// URL returns where the asset is served from.
func (a Asset) URL() string {
	return "https://github.com/llvm/llvm-project/releases/download/llvmorg-" + oracle.ClangVersion + "/" + a.Name
}

// AssetFor returns the pinned release archive for a GOOS/GOARCH.
func AssetFor(goos, goarch string) (Asset, error) {
	switch goos + "/" + goarch {
	case "darwin/arm64":
		return Asset{"LLVM-23.1.4-macOS-ARM64.tar.zst", "0f8b0db925ab54a63028d788e85b14abaa25492e9608819728c17f6786948da4",
			"https://api.github.com/repos/llvm/llvm-project/releases/assets/659320554"}, nil
	case "linux/amd64":
		return Asset{"LLVM-23.1.4-Linux-X64.tar.zst", "efdd1dbd089b99bfcfcde7b8eb1236163f2e88df5a981cca4f783ec9cb5e0378",
			"https://api.github.com/repos/llvm/llvm-project/releases/assets/659252374"}, nil
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

// Download parameters. GitHub's release CDN has answered a whole-file GET
// of the LLVM archive with 504 for hours while serving byte ranges of the
// same file (2026-11-08), so the archive is fetched in ranges, each retried.
// The sha256 check at the end is what guarantees the bytes.
var (
	chunkSize int64 = 64 << 20
	// The CDN also fails some ranges, for a while (run 42470580768: five
	// attempts over 31 s were not enough): the wait doubles from retryWait
	// up to maxWait, about four minutes over the attempts of one range.
	attempts         = 10
	retryWait        = time.Second
	maxWait          = 30 * time.Second
	maxArchive int64 = 8 << 30 // the LLVM archives are under 2 GiB
)

// DownloadFrom is Download from the first of urls that delivers the file.
// GitHub serves a release asset at two addresses: github.com/…/releases/
// download/…, whose front end answered 504 for hours on 2026-11-08 (for
// wasi-sdk's asset even on 1 KiB ranges), and the REST API's asset URL,
// which served the same file at the same time.
func DownloadFrom(ctx context.Context, urls []string, dst, want string) error {
	var errs []error
	for _, u := range urls {
		err := Download(ctx, u, dst, want)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	err = fetch(ctx, url, f)
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

// fetch writes the body of url into f, range by range.
func fetch(ctx context.Context, url string, f *os.File) error {
	total := int64(-1)
	for off := int64(0); total < 0 || off < total; {
		n, size, err := fetchRange(ctx, url, f, off)
		if err != nil {
			return err
		}
		if size < 0 {
			return nil // the server sent the whole file
		}
		if total >= 0 && size != total {
			return fmt.Errorf("provision: GET %s: the size changed from %d to %d", url, total, size)
		}
		total = size
		off += n
	}
	return nil
}

// errPermanent marks a failure a retry cannot fix.
type errPermanent struct{ error }

// fetchRange writes the range of url starting at off into f, retrying a
// server error or a broken transfer. It returns the bytes written and the
// file's total size, or -1 when the server ignored the range and sent the
// whole file from offset 0.
func fetchRange(ctx context.Context, url string, f *os.File, off int64) (int64, int64, error) {
	var err error
	wait := retryWait
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				return 0, 0, ctx.Err()
			case <-time.After(wait):
			}
			wait = min(2*wait, maxWait)
		}
		var n, size int64
		n, size, err = fetchRangeOnce(ctx, url, f, off)
		if err == nil {
			return n, size, nil
		}
		if p, ok := err.(errPermanent); ok {
			return 0, 0, p.error
		}
	}
	return 0, 0, fmt.Errorf("%w (offset %d, %d attempts)", err, off, attempts)
}

func fetchRangeOnce(ctx context.Context, url string, f *os.File, off int64) (int64, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, errPermanent{err}
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+chunkSize-1))
	// The REST API's asset URL serves the bytes only when asked for them;
	// the download URL ignores it.
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, err // the network: retried
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK && off == 0:
		// No range support: the whole file, bounded.
		n, err := io.Copy(io.NewOffsetWriter(f, 0), io.LimitReader(resp.Body, maxArchive+1))
		if err == nil && n > maxArchive {
			return 0, 0, errPermanent{fmt.Errorf("provision: GET %s: larger than %d bytes", url, maxArchive)}
		}
		return n, -1, err
	case resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode >= 500:
		return 0, 0, fmt.Errorf("provision: GET %s: %s", url, resp.Status)
	default:
		return 0, 0, errPermanent{fmt.Errorf("provision: GET %s: %s", url, resp.Status)}
	}
	first, last, size, err := parseContentRange(resp.Header.Get("Content-Range"))
	if err != nil || first != off || size > maxArchive {
		return 0, 0, errPermanent{fmt.Errorf("provision: GET %s: Content-Range %q for offset %d", url, resp.Header.Get("Content-Range"), off)}
	}
	want := last - first + 1
	n, err := io.Copy(io.NewOffsetWriter(f, off), io.LimitReader(resp.Body, want))
	if err == nil && n != want {
		err = fmt.Errorf("provision: GET %s: %d bytes of %d at offset %d", url, n, want, off)
	}
	return n, size, err
}

// parseContentRange parses "bytes first-last/size" (RFC 9110 §14.4), with
// first <= last < size; the unknown size "*" is refused.
func parseContentRange(s string) (first, last, size int64, err error) {
	rest, ok := strings.CutPrefix(s, "bytes ")
	if !ok {
		return 0, 0, 0, fmt.Errorf("content-range %q: no bytes unit", s)
	}
	span, total, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, 0, 0, fmt.Errorf("content-range %q: no size", s)
	}
	a, b, ok := strings.Cut(span, "-")
	if !ok {
		return 0, 0, 0, fmt.Errorf("content-range %q: no range", s)
	}
	for _, p := range []struct {
		s string
		v *int64
	}{{a, &first}, {b, &last}, {total, &size}} {
		v, err := strconv.ParseInt(p.s, 10, 64)
		if err != nil || v < 0 {
			return 0, 0, 0, fmt.Errorf("content-range %q: bad number %q", s, p.s)
		}
		*p.v = v
	}
	if first > last || last >= size {
		return 0, 0, 0, fmt.Errorf("content-range %q: inconsistent", s)
	}
	return first, last, size, nil
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
	urls := []string{asset.URL(), asset.APIURL}
	if o.BaseURL != "" {
		urls = []string{o.BaseURL + "/" + asset.Name}
	}
	llvm := filepath.Join(o.Home, "llvm")
	if !exists(clang) {
		fmt.Fprintf(log, "provision: downloading %s\n", asset.Name)
		if err := DownloadFrom(ctx, urls, archive, asset.SHA256); err != nil {
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
	// The compiler-rt builtins wasi-libc links against. Its cmake would
	// download them itself, without a checksum and in one GET; fetch them
	// here, pinned, through ranges.
	builtins := filepath.Join(o.Home, "dl", "libclang_rt.builtins.a")
	if !exists(builtins) {
		burls := []string{builtinsAsset.URL, builtinsAsset.APIURL}
		if o.BaseURL != "" {
			burls = []string{o.BaseURL + "/" + builtinsAsset.Name}
		}
		tgz := filepath.Join(o.Home, "dl", builtinsAsset.Name)
		fmt.Fprintf(log, "provision: downloading %s\n", builtinsAsset.Name)
		if err := DownloadFrom(ctx, burls, tgz, builtinsAsset.SHA256); err != nil {
			return err
		}
		if err := extractMember(tgz, builtinsAsset.Member, builtins); err != nil {
			return err
		}
	}
	build := filepath.Join(o.Home, "build")
	bin := filepath.Join(llvm, "bin")
	fmt.Fprintln(log, "provision: building the sysroot")
	if err := run(ctx, log, o.Home, "cmake", "-S", src, "-B", build,
		"-DBUILTINS_LIB="+builtins,
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

// builtinsAsset is the archive wasi-libc's cmake/builtins.cmake (at
// oracle.WasiLibcCommit) downloads when BUILTINS_LIB is not set, and the
// member it uses for a target without the libcall thread context (our
// case: HAVE_WASM_LIBCALL_THREAD_CONTEXT is empty). The sha256 is GitHub's
// digest of the release asset, equal to the copy the earlier builds
// fetched (checked 2026-11-08).
var builtinsAsset = struct{ Name, URL, APIURL, SHA256, Member string }{
	Name:   "libclang_rt-34.0-rc.1.tar.gz",
	URL:    "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-34-rc.1/libclang_rt-34.0-rc.1.tar.gz",
	APIURL: "https://api.github.com/repos/WebAssembly/wasi-sdk/releases/assets/463648626",
	SHA256: "84da92d09e34bac7d39eb7f8075697b4f00bbccd0cfb6ce9654565ad6a7e2dfe",
	Member: "wasm32-unknown-wasip2/libclang_rt.builtins.a",
}

// maxMember bounds the member extractMember writes.
var maxMember int64 = 64 << 20

// extractMember writes the regular file of a .tar.gz whose path ends with
// member (after any top directory) to dst.
func extractMember(archive, member, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("provision: %s: %w", archive, err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("provision: %s has no %s", archive, member)
		}
		if err != nil {
			return fmt.Errorf("provision: %s: %w", archive, err)
		}
		name := path.Clean(h.Name)
		if h.Typeflag != tar.TypeReg || name != member && !strings.HasSuffix(name, "/"+member) {
			continue
		}
		if h.Size < 0 || h.Size > maxMember {
			return fmt.Errorf("provision: %s: %s is %d bytes, over %d", archive, member, h.Size, maxMember)
		}
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		_, err = io.CopyN(out, tr, h.Size)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
		return err
	}
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
