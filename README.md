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
| M1 front end | package `cell`: splitter, declaration/statement classification, hoisting, embedded headers, defines/uses with `Def`, prelude; go-notebook's C language is built on it (v0.2.1). The wasm32 `ABI` is not yet upstreamed. | **done** |
| M2 code generator | package `wasmgen`, cc/v4 AST → relocatable wasm32 object; design in [docs/design-m2.md](docs/design-m2.md). W1–W3, W4a and W4b are done: object writer, the `wasm-ld` + wazero harness against clang, integer expressions, locals, globals, calls, control flow structured after Ramsey (2022), memory (pointers, arrays, the linear-stack frame, string literals, static addresses), and structures and unions with the BasicCABI passing rules. 952 gcc torture files run as under clang | **in progress** |
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

`internal/cparse` configures the front end by hand — a wasm32 `ABI`
literal, the predefined macros of the oracle clang, include paths into the
sysroot, no host compiler (`NewConfig` is not used). Files taken:

| corpus | preprocess + parse | + type check |
|---|---|---|
| gcc torture execute | 1 881 / 1 890 | 1 881 / 1 890 |
| c-testsuite | 218 / 220 | 217 / 220 |

Of the nine gcc refusals, eight are wasi-libc's own `#error` (`signal.h`,
`setjmp.h`, `sys/mman.h`) and one is C23 `f(...)`. Tests hold these as floors.
`cparse` still recovers any panic into a `*PanicError`, but `FuzzSource` now
fails on one: the panics and hangs it found in cc/v4 v4.30.5 are patched (below).

### The front end: `internal/cc`

The front end is modernc.org/cc/v4 v4.30.5 (BSD-3-Clause, `internal/cc/LICENSE`),
copied and patched by `go run ./internal/cmd/vendorcc --cc <module dir>`. Each
patch must match exactly once, so a new cc/v4 release cannot skip one, and
`TestInternalCCIsGenerated` checks the copy against the generator. The patches:

- `Config.MaxExpansion` bounds the tokens macro replacement produces (#11).
  Without it, `#define D(a) ((a)+(a))` nested 40 deep, 154 bytes, asks for
  2^40 tokens and never returns.
- A file missing from `Config.FS` is not opened, nor probed by
  `__has_include`, on the host file system (#4).
- The inputs that `FuzzSource` found panicking or hanging are now errors
  (#2). Each is in `formerPanics`, or `TestEmptyVariadicArguments`, with a
  valid control:
  - `#define` with no macro name;
  - `##` at either end of a replacement list (C11 6.10.3.3p1);
  - an argument list unterminated at the end of input;
  - `M()` for `M(A,...)`;
  - a non-constant bit-field width;
  - `0={#,`;
  - the end of input in a string, a character constant or a line splice;
  - `^` alone, which looped forever.

  A parser that keeps shifting the end of input is stopped after 4096 shifts.
- Three places where cc/v4 disagreed with clang on a gcc torture file:
  - enumeration constants have type `int` (C11 6.7.2.2p3), even when
    `UnsignedEnums` makes the enum type unsigned (`enum-2.c`);
  - GNU `__alignof__` of an expression does not decay it (`align-3.c`);
  - `Config.FunctionAlign` sets the alignment of a function without an
    `aligned` attribute; clang gives 4 on wasm32.

Upgrading cc/v4 means bumping `vendorcc.Version` and rerunning it.

## Cells (M1, in progress)

Package `cell` analyses a notebook C cell without a host compiler or a file
system, so it runs unchanged in the wasm playground:

```go
a := cell.Analyze(src, ctx, cell.Limits{}) // zero fields take DefaultLimits
// a.Diagnostics: line:col and byte offset in the cell
// a.Defines:     file-scope names, with their kind (function, variable, typedef, tag, enumerator, macro)
//                and Def: Definition, Tentative (`int x;`) or Declaration (a prototype, `extern int x;`, `struct s;`)
// a.Uses:        names defined by earlier cells that this cell reads
// a.Decls, a.Stmts: the cell's chunks, classified
ctx = ctx.Extend(a)    // what later cells see
```

A cell may mix declarations and statements: `int x = 2; printf("%d\n", x * x);`
is a cell. Each top-level chunk that cc/v4 parses as a declaration with a type
is one; the others (including `x = 3;`, which C89 would read as an implicit
`int` declaration) are statements, run in order by the cell's function. Later
cells see earlier declarations: functions as prototypes, objects without their
initialiser (bib/c-cpp.md §2.5).

Initialised objects are hoisted, as cling does: the declaration stays at file
scope and the initialisation runs as an assignment in source order, so
`int i = 1; i++; int j = i;` leaves `j == 2`. Aggregates (`= {…}`), arrays and
`const` objects keep their static initialiser, which C then requires to be
constant.

The headers a cell can `#include` are embedded (`headers`): wasi-libc's for
wasm32-wasip1 and clang's own C headers, at the oracle's pinned revisions.
`setjmp.h` and `signal.h` stop with wasi-libc's `#error`, as under clang.

**Security.** A cell is untrusted text. `Limits` bound it: 1 MiB of source,
256 levels of brackets, 65 536 tokens of macro expansion. The last limit stops
expansion bombs in well under a second (`TestExpansionBombsRefused`). A cell
cannot read the host file system, by two independent layers:

- `cell` refuses before the front end runs: only literal includes of embedded
  headers, no `#line`, `#include_next` or `__has_include`, and no `#define` of
  a word that occurs in an include path.
- `internal/cc` does not fall back to the host. `TestHostFilesUnreachable`
  checks both layers, against a control that reads the host file.

## Code generator (M2, in progress)

`wasmgen.Compile(ast, file)` turns a checked translation unit into a
relocatable wasm32 object in the tool-conventions `Linking.md` format: the
format clang writes and wasi-libc's `libc.a` is made of. Until the cell
linker of M3 exists, `oracle.Link` links the object with the pinned
`wasm-ld`, `crt1.o` and `libc.a`. Tests then run the program in the same
wazero as clang's, and compare exit status and output (`TestAgainstClang`).

W2 compiles the following:

- functions with integer parameters and results;
- integer locals and file-scope objects: initialised, tentative, `static`
  and `extern`;
- every integer operator except `&&`, `||` and `?:`, with C's conversions
  at each width from `_Bool` to `long long`;
- assignment, compound assignment, `++`/`--` and the comma operator;
- direct calls, to functions defined in the unit or imported. A K&R call of
  an undeclared `exit` or `abort` gets the library's signature, as under
  clang. `abs`, `labs` and `llabs` are computed inline, as clang does.

W3 adds control flow:

- `if`/`else`, `while`, `do`, `for`, `break`, `continue`, `return`, and
  `switch` with fall-through and `default`. These are lowered to a graph of
  basic blocks, then structured into wasm `block`, `loop`, `if` and `br` by
  Ramsey's dominator-tree translation ("Beyond Relooper", ICFP 2022).
- `&&`, `||` and `?:` become wasm `if`s with a result inside the
  expression.

W4a adds memory:

- Pointers, with arithmetic, differences and unsigned comparisons.
- Arrays, multi-dimensional too, and string literals.
- Objects whose address is taken, in a frame on the linear stack. The
  frame is reached through `__stack_pointer`, as BasicCABI.md and clang
  lay it out. The prologue copies address-taken parameters into the frame,
  and every return restores the stack pointer.
- Static initialisers that hold addresses, written as
  `R_WASM_MEMORY_ADDR_I32` data relocations.
- Like clang, an assignment evaluates its right operand before the left
  one. `x[0] |= f()` can observe the difference when `f` changes `x[0]`.

W4b adds structures and unions:

- Members through `.` and `->`, anonymous members, nested aggregates,
  arrays of them, and their initialisers, static or not.
- Assignment, `?:` and the comma operator on whole structures. Each copies
  the bytes with `memory.copy`.
- Passing and returning them as BasicCABI.md specifies:
  - a structure or union holding a single scalar travels as that scalar;
  - an empty one is not passed at all;
  - any other is passed through a pointer to a copy the caller makes, and
    is returned through a pointer the caller passes first.
- Bit-fields are still refused, in members and in initialisers (W6).

The generator refuses anything else with an `*Unsupported` error that names
the construct.

Four tests check the generator:

- `TestRandomExpressions` generates random programs without undefined
  behaviour. Each must exit exactly as clang's does: 60 programs in CI, 600
  checked locally. A deliberately miscompiled sign extension makes it
  fail.
- `TestABIAgainstClang` links our object with clang's, in both directions,
  and calls across them with every way of passing a structure. A
  single-file comparison cannot catch an ABI that both sides get wrong in
  the same way; these mixed links do. Treating no structure as a
  singleton makes only the mixed links fail.
- `TestCorpusGCCTorture` counts what happens to each torture file. On
  2026-11-08, after W4b, 952 files ran the same as under clang (762 after
  W4a, 454 after W3, 10 after W2). The test holds that count as a floor
  and logs the refusals by cause. Bit-fields (W6) and variadic functions
  are now the most common.
  - No other file differs. The one known difference, `shiftdi-2.c`, comes
    from the oracle compiling with `-std=gnu89`, where a large decimal
    constant has another type (#20).
  - A known difference that disappears makes the test fail, so the list
    stays true.
- `FuzzCompile` checks that the generator never panics and that every
  object it emits is a valid module.

## Not yet

- csmith differential fuzzing (oracle against our kernel) comes with the code
  generator.
- The gcc `gcc.c-torture/compile` and `gcc.dg/torture` directories, and
  ccorpus2, are not run yet.
- The org landing page and the docs site wait for M4; this README is the only
  user-facing page at M0.

Licence: BSD 3-Clause (see `LICENSE`, and `NOTICE` for what is only run).
