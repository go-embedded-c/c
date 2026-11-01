# c — C for go-notebook cells, in pure Go

`github.com/go-embedded-c/c` is the start of a C implementation written in Go
(`CGO_ENABLED=0`), meant to be embedded in Go programs and to run in the
browser through WebAssembly — it will back the ` ```c ` cells of
[go-notebook](https://github.com/go-notebook) the way
[rbgo](https://github.com/go-embedded-ruby/ruby) backs its Ruby cells and
[go-embedded-julia/julia](https://github.com/go-embedded-julia/julia) its Julia
cells.

The plan is [go-scikit/roadmap `bib/c-cpp.md`](https://github.com/go-scikit/roadmap/blob/main/bib/c-cpp.md):
parse with [modernc.org/cc/v4](https://pkg.go.dev/modernc.org/cc/v4), write a
C → wasm32 code generator and a cell runtime sharing one memory and one
function table, run on [wazero](https://github.com/tetratelabs/wazero) (desktop)
and the browser's engine (playground), with wasi-libc as the libc. Every stage
is checked against a reference, never against expectations written by hand.

## Status

| milestone | contents | state |
|---|---|---|
| M0 oracle harness | `internal/oracle`, `cmd/coracle`, golden file, corpus runner, cc/v4 parse smoke test | **done** |
| M1 front end | wasm32 `ABI` for cc/v4, embedded wasi-libc headers, cell splitter and prelude | planned |
| M2 code generator | cc/v4 AST → wasm32 | planned |
| M3 cell runtime | shared memory/table session, cell linker, WASI host, limits | planned |
| M4 – M5 | notebook bridge, conformance and performance | planned |

There is no compiler here yet: nothing in this repository executes C except the
oracle's clang.

## The oracle

The reference is **clang 23.1.4** (`llvmorg-23.1.4`) compiling to
`wasm32-wasip1` against a **wasi-libc** sysroot (commit `d9ad2d11991c`), and the
resulting module run under **wazero v1.12.0** — the engine our kernel will use.
Same ABI, same libc, same engine: a difference between the oracle and our
compiler is a compiler difference. "Correct" means the same exit status and the
same stdout/stderr bytes.

```
go install github.com/go-embedded-c/c/cmd/coracle@latest
export CORACLE_HOME=$HOME/coracle-home
coracle toolchain          # downloads LLVM 23.1.4 (sha256-checked), builds the sysroot
coracle version
coracle run prog.c         # compile with the oracle, run, relay stdout/stderr/status
```

`coracle toolchain` needs `zstd`, `tar`, `git` and `cmake`; it is idempotent,
and pinned for macOS arm64 and Linux amd64. Tests that need the toolchain skip
cleanly without it (`CORACLE_REQUIRE=1` turns the skip into a failure; CI sets
it). `CORACLE_CLANG` and `CORACLE_SYSROOT` point at an existing toolchain
instead; the clang version is checked and anything but 23.1.4 is refused.

Runs are bounded: a wall-clock timeout, a memory limit in wasm pages (a guest
`malloc` past it returns NULL), a cap on stdout and stderr, and a deterministic
random source and clock.

| command | what it does |
|---|---|
| `coracle golden` | regenerates `testdata/programs.golden` from the 13 programs in `testdata/programs/` |
| `coracle corpus --dir D` | compiles and runs every `.c` below D with the oracle; one line per file |
| `coracle parse --dir D [--check]` | runs cc/v4 over every `.c` below D |
| `coracle predefined` | regenerates `internal/cparse/predefined.h` from the oracle clang |

### Corpora

Neither is committed: the gcc testsuite is GPL (used as test input only) and
c-testsuite licenses each test in its own `.otag` file. CI fetches them by
commit.

| corpus | revision | files |
|---|---|---|
| gcc `gcc.c-torture/execute` | gcc 15.2.0, `5115c7e447fc` | 1 890 |
| c-testsuite `tests/single-exec` | `5c7275656d75` | 220 |

Oracle over the corpora (`-O0`, `-std=gnu89` for gcc, `gnu17` for c-testsuite):

| corpus | pass | clang refuses | trap | other |
|---|---|---|---|---|
| gcc torture execute | 1 627 | 177 | 86 | 0 |
| c-testsuite | 219 | 0 | 1 (`00187`) | 0 |

### cc/v4 smoke test

`internal/cparse` configures modernc.org/cc/v4 v4.30.5 by hand — a wasm32 `ABI`
literal, the predefined macros of the oracle clang, include paths into the
sysroot, no host compiler (`NewConfig` is not used). Files taken:

| corpus | preprocess + parse | + type check |
|---|---|---|
| gcc torture execute | 1 877 / 1 890 | 1 877 / 1 890 |
| c-testsuite | 218 / 220 | 217 / 220 |

Tests hold these as floors. cc/v4 panics on some malformed input (three
reproducers are kept in `knownPanics` and the fuzz corpus); `cparse` recovers
every panic into a `*PanicError`.

## Not yet

- csmith differential fuzzing (oracle against our kernel) comes with the code
  generator.
- The gcc `gcc.c-torture/compile` and `gcc.dg/torture` directories, and
  ccorpus2, are not run yet.
- The org landing page and the docs site wait for M4; this README is the only
  user-facing page at M0.

Licence: BSD 3-Clause (see `LICENSE`, and `NOTICE` for what is only run).
