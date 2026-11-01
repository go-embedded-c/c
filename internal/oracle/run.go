package oracle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// Limits bound one execution. A guest is untrusted: nothing it does may
// outgrow these.
type Limits struct {
	// Timeout is the wall-clock bound (10 s when zero).
	Timeout time.Duration
	// MemoryPages is the linear-memory bound, in 64 KiB pages (1024 = 64 MiB
	// when zero).
	MemoryPages uint32
	// MaxOutput is the bound on each of stdout and stderr (1 MiB when zero).
	MaxOutput int
}

func (l Limits) withDefaults() Limits {
	if l.Timeout == 0 {
		l.Timeout = 10 * time.Second
	}
	if l.MemoryPages == 0 {
		l.MemoryPages = 1024
	}
	if l.MaxOutput == 0 {
		l.MaxOutput = 1 << 20
	}
	return l
}

// Result is what one run of a module produced.
type Result struct {
	Stdout, Stderr []byte
	// Exit is the exit status (proc_exit, or return from main).
	Exit uint32
	// Trap is non-empty when the guest ended by a trap (unreachable, out of
	// bounds, stack overflow…) rather than by exiting; it is the trap's
	// first line, stable across runs.
	Trap string
	// TimedOut reports that the Timeout expired.
	TimedOut bool
	// Truncated reports that an output stream hit MaxOutput.
	Truncated bool
}

// Runner executes wasm modules under wazero. It is safe for concurrent use.
type Runner struct {
	rt     wazero.Runtime
	limits Limits
}

// NewRunner creates a runner with the memory bound of lim; the compiler
// engine is used where wazero has one.
func NewRunner(ctx context.Context, lim Limits) (*Runner, error) {
	lim = lim.withDefaults()
	cfg := wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithMemoryLimitPages(lim.MemoryPages)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	return &Runner{rt: rt, limits: lim}, nil
}

// Close releases the runtime.
func (r *Runner) Close(ctx context.Context) error { return r.rt.Close(ctx) }

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// capped is a writer that keeps at most max bytes and fails past them.
type capped struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

var errFull = errors.New("output limit reached")

func (c *capped) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return room, errFull
	}
	return c.buf.Write(p)
}

// Run instantiates wasm (a WASI command) and runs its _start with args and
// stdin. The returned error is for failures of the harness (an invalid
// module); anything the guest does, including trapping, is in the Result.
func (r *Runner) Run(ctx context.Context, wasm []byte, args []string, stdin []byte) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, r.limits.Timeout)
	defer cancel()
	compiled, err := r.rt.CompileModule(ctx, wasm)
	if err != nil {
		return Result{}, fmt.Errorf("oracle: invalid module: %w", err)
	}
	defer compiled.Close(ctx)
	out, errw := &capped{max: r.limits.MaxOutput}, &capped{max: r.limits.MaxOutput}
	cfg := wazero.NewModuleConfig().
		WithName("").
		WithArgs(append([]string{"prog"}, args...)...).
		WithStdin(bytes.NewReader(stdin)).
		WithStdout(out).WithStderr(errw).
		WithRandSource(zeroReader{})
	mod, err := r.rt.InstantiateModule(ctx, compiled, cfg)
	if mod != nil {
		mod.Close(ctx)
	}
	res := Result{Stdout: out.buf.Bytes(), Stderr: errw.buf.Bytes(), Truncated: out.truncated || errw.truncated}
	if err == nil {
		return res, nil
	}
	var ee *sys.ExitError
	switch {
	case errors.As(err, &ee):
		res.Exit = ee.ExitCode()
		if ee.ExitCode() == sys.ExitCodeDeadlineExceeded || ctx.Err() != nil {
			res.Exit, res.TimedOut = 0, true
		}
	default:
		res.Trap = firstLine(err.Error())
	}
	return res, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// Output is a convenience for tests: compile src and run it with no
// arguments or input.
func (t *Toolchain) Output(ctx context.Context, r *Runner, src []byte, o CompileOptions) (Result, error) {
	wasm, err := t.Compile(ctx, src, o)
	if err != nil {
		return Result{}, err
	}
	return r.Run(ctx, wasm, nil, nil)
}

var _ io.Writer = (*capped)(nil)
