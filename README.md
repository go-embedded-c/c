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
| M2 code generator | package `wasmgen`, cc/v4 AST → relocatable wasm32 object; design in [docs/design-m2.md](docs/design-m2.md). W1–W3, W4a–W4d and W5–W7 are done: object writer, the `wasm-ld` + wazero harness against clang, integer expressions, locals, globals, calls, control flow structured after Ramsey (2022), memory (pointers, arrays, the linear-stack frame, string literals, static addresses), structures and unions with the BasicCABI passing rules, variadic functions, static locals and `__func__`, `main(argc, argv)`, `float`, `double` and `long double` (binary128), `float` and `double _Complex`, bit-fields, compound literals, `goto`, function pointers and the computing GCC builtins (`alloca`, `*_overflow`, the quiet comparisons, bit counts, `fabs`/`copysign`/`inf`/`nan`, `isinf`/`signbit`, `ffs`, `bswap`), address constants of members (`&s.m`, `&p->m`), `asm` statements with an empty template, the `mode` and `packed` attributes, clang's bit-field layout and promotion, statement expressions, labels as values, and irreducible loops. 1605 gcc torture files run as under clang | **in progress** |
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

W4c adds variadic functions:

- A call stores the arguments matched by `...` in a buffer in the
  caller's frame and passes its address as the last parameter. This is
  BasicCABI.md's rule, laid out the way LLVM's WebAssembly backend lays it
  out: each argument after the default argument promotions, at its
  alignment.
- `va_start`, `va_arg`, `va_end` and `va_copy` read that buffer as clang
  does: 4-byte slots, aligned up for a wider type, and a pointer for a
  structure passed indirectly.
- An undeclared `printf` calls wasi-libc's `printf`, as under clang. So
  do `puts`, `putchar`, `malloc`, `free`, and the `mem*` and `str*`
  functions of `<string.h>` that cc/v4 declares as builtins.
- Compound literals (C11 6.5.2.5):
  - In a function, a literal gets a frame slot and is initialised each
    time it is evaluated; what its list does not name is zero.
  - At file scope, it gets a segment of its own (`.compoundliteral.N`).
  - A literal of incomplete array type, `(int[]){…}`, is refused.
- A function defined as `f()` and called with arguments gets them
  evaluated and dropped. That is what clang's wrapper does
  (`WebAssemblyFixFunctionBitcasts`).
- A `float` argument is promoted to `double`.

W4d adds:

- Static locals. Each gets a segment and a local symbol named
  `function.name`, as clang names it. It is initialised once, in the data.
- `int main(int argc, char *argv[])`, defined as `__main_argc_argv`
  (BasicCABI.md, "User entrypoint"), which wasi-libc calls with the
  arguments.

W5 adds `float` and `double`, as wasm's `f32` and `f64`. Every operation
is the one clang emits at `-O0` (checked with `llvm-objdump`):

- A conversion to an integer type saturates (`trunc_sat`).
- A conversion to `_Bool`, `!x` and a controlling expression compare with
  zero, so NaN is true.
- Negation is `f.neg`, so `-(+0.0)` is `-0.0`.
- A floating literal is rounded once, from its text, to its own type.
- GNU vector types are refused: they were read as their element type
  before. `long double` (binary128) is described below.

W6 adds bit-fields:

- A read loads the field's storage unit and extracts the bits. It
  sign-extends them when the declared type is signed; a plain `int`
  bit-field is signed, as in clang.
- A write loads the unit, clears the field's bits, ORs the new ones in
  and stores the unit back. The neighbouring fields keep their values.
- Initialisers set the bits, in the data for a static object and in the
  frame for a local one. The value of an assignment is the stored value,
  truncated to the width.
- cc/v4 laid bit-fields out by its own rule, which is not clang's. For
  example, `struct { unsigned long long f:44; _Bool g:1; short h; }` had
  `h` at 8 and size 16, where clang has 6 and 8. The front end now uses
  the rule of the Itanium ABI, which gcc and clang follow on every target
  but Microsoft's (go-embedded-c/c#29). Its constant `sizeof`, `_Alignof`
  and `offsetof` agree with clang's.
  - A bit-field goes at the next free bit, unless it would straddle a
    boundary of its type's alignment.
  - A zero-width bit-field moves the next member to its type's boundary.
  - An unnamed bit-field does not align the structure.
  - A packed bit-field goes at the next free bit. Its storage unit is the
    bytes it spans, rounded to 1, 2, 4 or 8, and may be misaligned. One
    that spans 9 bytes is refused.
- A bit-field's type in expressions follows clang's promotion
  (`isPromotableBitField`, as gcc does), whatever its declared type:
  - narrower than `int`: `int`;
  - as wide as `int`: `int` or `unsigned int`, by the declared type's
    signedness;
  - wider: the declared type.

  So `long long x:24` is an `int`. It is read in its 64-bit unit and then
  converted.

Statement expressions, GNU C's `({ … })`, take the value of their last
expression statement; when the last item is not an expression, they are
`void`.

- The body may hold declarations, loops, a `switch`, `break` and
  `continue` inside them, and `return`.
- The body is compiled as a graph of its own and structured like a function
  body, wrapped in a wasm `block` that is its only exit. It is emitted inline
  in the enclosing expression, so values already on the stack stay under it.
  The value goes out through a local.
- Jumping into a statement expression is not permitted (GCC manual). Jumping
  out of one with `break`, `continue` or `goto` is permitted by GCC and
  refused here, as are a label and a `goto` inside one.

`asm` statements with an empty template, the optimisation barriers of the
gcc torture tests (`asm volatile ("" : : : "memory")`, `asm ("" : "+r" (x))`,
`asm ("" : "=r" (x) : "0" (y))`), compile to what their operands say, since
no instruction runs:

- an input is evaluated;
- a `+` output keeps its value, its lvalue evaluated once;
- a `=` output tied to an input by its number receives that input, as
  their shared register carries it through;
- an untied `=m` output keeps its value;
- clobbers do nothing, since the generator keeps no value in a register
  across statements.

Refused: a template that is not empty, a named operand, `asm goto`, and an
untied `=` output that is not memory, whose value would be whatever the
register held.

cc/v4 ignored two GNU attributes that change the layout of data, and the
front end (patches in `internal/cmd/vendorcc`) now implements them:

- `mode` (`unsigned int __attribute__((mode(QI)))`) gives the declaration
  the type of the mode's width: `QI`, `HI`, `SI`, `DI`, `byte`, `word`,
  `pointer`, and `SF`/`DF` for floating types. An integer keeps the signedness
  of the declared type, and an enumeration that of its underlying type, as in
  clang. cc/v4 had left the type its declared size (4 bytes where clang makes
  it 1).
- `packed` on the definition of a struct or union packs each member. On a
  member, it packs that member. A packed member has alignment 1 unless an
  `aligned` attribute asks for more. cc/v4 had kept the unpacked layout
  (`sizeof` 24 where clang gives 11).

Both are refused where nothing applies them: in a type name, before a member,
on an `enum`. They are also refused on what is not implemented yet: `TI`,
and a mode on a vector type (go-embedded-c/c#42). Packed bit-fields follow
the bit-field rule above.

The same patches fix a parser defect that dropped the middle values of an
attribute list of three or more, and panicked on a `,` after a refused first
value.

Function pointers:

- A function's address is its index in the indirect function table,
  which wasm-ld builds from the `R_WASM_TABLE_INDEX_*` relocations.
- A call through a pointer is `call_indirect` on the imported
  `__indirect_function_table`, as clang emits it. The pointer is
  evaluated first, then the arguments.
- Function pointers work in static initialisers, in structures, as
  arguments and in comparisons.
- A call through a pointer to a function without a prototype is refused.

GCC builtins that compute rather than call a library function compile to
what clang 23.1.4 emits for wasm32:

- `__builtin_alloca` moves `__stack_pointer` down by the size rounded up
  to 16. The epilogue restores the value it had on entry, which frees the
  memory when the function returns.
- `__builtin_{add,sub,mul}_overflow`, at any operand and result width:
  - the exact value is computed in 128 bits;
  - the product of 64-bit operands goes through compiler-rt's
    `__multi3`, as clang's does.

  The `_p` forms are refused; clang does not know them.
- `isunordered`, `isgreater` and the other quiet comparisons are computed
  in the operands' common real type.
- `clz`, `ctz` and `popcount` (plain, `l` and `ll`), `expect`, `prefetch`,
  `unreachable` and `trap` are also supported.
- Floating-point builtins, for `float` and `double` (the `long double`
  forms are in the `long double` paragraph below):
  - `fabs` and `copysign`;
  - `inf` and `huge_val`;
  - `nan(s)` with the payload clang gives it: `s` read as an integer
    (`0x`, `0b`, `0o`, a leading `0` for octal, else decimal), its low
    significand bits kept, and the quiet bit set (`TryEvaluateBuiltinNaN`
    and `IEEEFloat::makeNaN`, LLVM 23.1.4). A string clang cannot read is
    refused;
  - all of these also in static initializers, negated or not, written as
    bits;
  - `isinf`, `isfinite`, `isnan` and `signbit`.
- Byte builtins: `ffs` (plain, `l` and `ll`) and `bswap16/32/64`.
- Library functions called by their `__builtin_` name go to wasi-libc:
  - `fprintf`, `sprintf` and `snprintf`;
  - `memmove`, `mempcpy`, `stpcpy`, `bzero`, `index` and `rindex`, with
    the library's signature, since cc/v4 does not declare them.

  `__builtin_fputs` is refused, because clang does not know it.
  `__builtin_imaxabs` is refused too: cc/v4 would type the undeclared
  call `int` where clang keeps `intmax_t`.
- A pointer to a variable-length array is refused. Its step is a size
  known only at run time, and cc/v4's is not that size.

`long double` is IEEE binary128, 16 bytes aligned to 16 (BasicCABI.md). It
is not a wasm scalar: it is a 16-byte memory object, and every operation
calls compiler-rt's soft-float routines, as clang 23.1.4 emits them
(probed with `-S`):

- A parameter is two `i64`, low half first, spilled to a 16-byte frame
  slot; a result goes through the `sret` pointer, as a structure's does. A
  value of type `long double` evaluates to the address of its object, or of
  a frame temporary.
- `+ - * /` are `__addtf3`, `__subtf3`, `__multf3` and `__divtf3`;
  comparisons are `__lttf2`, `__letf2`, `__gttf2`, `__getf2`, `__eqtf2` and
  `__netf2` compared with 0, so a NaN makes `<`, `<=`, `>`, `>=` and `==`
  false and `!=` true. A truth value, `!x` and a condition are
  `__netf2(x, 0)`; negation flips the sign bit of the high half.
- Conversions go through `__extendsftf2`, `__extenddftf2`, `__floatsitf`,
  `__floatunsitf`, `__floatditf` and `__floatunditf` in, and `__trunctfsf2`,
  `__trunctfdf2`, `__fixtfsi`, `__fixunstfsi`, `__fixtfdi` and `__fixunstfdi`
  out. A conversion to `_Bool` compares with zero.
- Locals, globals, members, arrays, compound assignment, `++` and `--`, `?:`,
  calls, and variadic arguments (16 bytes at a 16-byte boundary of the
  buffer) and `va_arg` work.
- A static initialiser writes the 16 bytes of the value, computed exactly
  with `math/big`: a literal is parsed once to 113 bits, each operation is
  rounded to 113 bits, and an overflow becomes infinity, as clang's APFloat
  does. cc/v4's own folded value (256 bits) is not used. A subnormal
  result is refused.
- Builtins: `fabsl`, `copysignl`, `infl`, `huge_vall`, `nanl` (the low 112
  bits of the payload and bit 111 set; a string longer than 18 characters
  is refused), `isinf`, `isfinite`, `isnan`, `signbit` and `signbitl`, and
  the quiet comparisons, on long doubles.
- Refused by name: `struct { long double x; }` as a parameter or a result
  (clang passes it as two `i64`, which is not what the other structures
  do), `_Complex long double`, a compound assignment such as `k %= ld` of a
  scalar, and `__builtin_isinfl` and `__builtin_isnanl`, which clang does
  not know either.

`float _Complex` and `double _Complex` (C11 Annex G):

- BasicCABI.md represents a complex value as a struct of its two parts, real
  then imaginary. So it is an aggregate here: passed through a pointer,
  returned through `sret`, copied, stored and laid out like a struct of two
  members.
- An expression of complex type evaluates to the address of its two parts:
  the object of an lvalue, or a frame temporary.
- What clang emits (probed with `-S`):
  - `+` and `-` work part by part. With a real operand, clang does not
    convert it to complex but keeps the other operand's imaginary part as
    it is, so `-0.0` stays `-0.0`. It folds constants the same way, and so
    does this generator.
  - `*` of two complex values is `__mulsc3`/`__muldc3`, which applies Annex
    G's recovery of infinities. With a real operand, `*` works part by part.
  - `/` is `__divsc3`/`__divdc3`, a real dividend included; a real divisor
    divides each part.
  - `-z` negates both parts, and `~z` (GNU conjugate) the imaginary part.
  - `==` and `!=` compare both parts.
  - In a scalar context, a complex value is its real part. As a truth value
    it is true when either part is non-zero.
- `__real__`, `__imag__`, compound assignment, static and local initialisers
  work.
- Refused: `long double _Complex`, and `*=` or `/=` of a scalar by a complex
  value, whose real part is not the scalar times the real part.

W7 adds `goto`:

- A label is a block of the graph and a `goto` is an edge to it. The
  Ramsey structuring that handles loops and `switch` turns the edge into a
  `br`.
- A `goto` into a loop from outside makes the graph irreducible: the loop
  has more than one entry. As in LLVM's WebAssemblyFixIrreducibleControlFlow,
  each such loop gets a dispatch block.
  - Every edge to one of the loop's entries sets a label variable to that
    entry, then jumps to the dispatch block, which branches to it.
  - The dispatch block becomes the loop's single entry.
  - No code is copied, unlike node splitting (Ramsey, appendix A).
  - A function may need at most 64 such blocks. Past that it is refused, so
    a cell full of `goto`s cannot grow the module without bound.
- Still refused: GNU's block-local `__label__`.
- Labels as values (GNU `&&label`, `goto *p`):
  - wasm has no code addresses, so `&&label` is a number. It is distinct
    for each label whose address is taken, across the module, and never 0.
    It can go in static data, alone or as the difference of two labels.
  - `goto *p` stores `p` and jumps to the function's single dispatch block,
    which compares it with each of those numbers. A value that matches none
    of them traps. GCC leaves undefined a jump to a label of another
    function.
  - One dispatcher per function keeps the usual threaded interpreter
    reducible, because the dispatcher dominates every label it reaches.
    clang refuses some computed gotos on wasm32 (990208-1.c); we compile
    them.
- Also refused: a function-scope `typedef` of a variable-length type,
  and `sizeof` of one. cc/v4 folds such a size, which C computes at run
  time, into a constant.

The generator refuses anything else with an `*Unsupported` error that names
the construct.

Five tests check the generator (`TestQuadABIAgainstClang`, which does for
`long double` parameters, results and varargs what `TestABIAgainstClang`
does for structures, is the fifth):

- `TestRandomExpressions` generates random programs without undefined
  behaviour. Each must exit exactly as clang's does: 60 programs in CI, 600
  checked locally. A deliberately miscompiled sign extension makes it
  fail.
- `TestABIAgainstClang` links our object with clang's, in both directions,
  and calls across them with every way of passing a structure and with
  variadic calls. A single-file comparison cannot catch an ABI that both
  sides get wrong in the same way; these mixed links do. Treating no
  structure as a singleton makes only the mixed links fail. So does
  misaligning a `long long` in the varargs buffer, on the side that is
  ours.
- `TestCorpusGCCTorture` counts what happens to each torture file. On
  2026-11-15, with `float` and `double _Complex`, 1605 files ran the same
  as under clang, 6 gained and none lost (1599 with `__func__`, 4 gained
  and none lost, 1595 with irreducible loops, 4 gained and none
  lost, 1591 with labels as values, 9 gained and
  none lost, 1582 with statement expressions, 5 gained
  and none lost, 1577 with `long long` bit-fields as narrow
  as `int`, 10 gained and none lost, 1567 with the Itanium
  bit-field layout, 12 gained and none lost, 1555 with the `mode` and `packed`
  attributes, eight gained and five lost to the then refusal of packed
  bit-fields, 1552 with the empty-template `asm` statements, 1502 with
  `long double`, 1477 with static member addresses and the floating-point builtins in
  static initializers, 1472 with the floating-point and byte builtins, 1440 with the computing builtins,
  1404 with function pointers, 1381 after W7, 1350 with compound literals,
  1327 with the library builtins, 1250 after W6, 1160 after W5, 1049 after
  W4d, 1005 after W4c, 952 after W4b, 762 after W4a, 454 after W3, 10 after
  W2). The test holds that count as a floor and logs the refusals by cause,
  with three files for each. `asm` statements, `__builtin_longjmp` and
  nested functions are now the most common. It also names each file clang
  refuses that we compile.
  - The files that ran the same are also listed, one per line, in
    `wasmgen/testdata/gcc-torture-same.txt`. The test fails for each listed
    file that stops running the same, and for each file that starts to run
    the same without being listed. `CORACLE_WRITE_SAME=1` rewrites the list.
    A count can rise while one file is lost: the builtins wave lost
    `pr22061-2.c` while it gained 37 others, and nothing failed. Checked
    this way, the floating-point and byte builtins gained 32 files and lost
    none, the static member addresses gained 5 and lost none, and `long
    double` gained 25 and lost none. The `long double` files that still do
    not count are refused by clang (`pr39228.c` uses `__builtin_isinfl`;
    `fp-cmp-4l.c` and `fp-cmp-8l.c` include a file from a directory clang
    is not given) or use `asm` or `__builtin_va_arg_pack`.
  - No other file differs. There are three known differences:
    - `shiftdi-2.c` comes from the oracle compiling with `-std=gnu89`,
      where a large decimal constant has another type (#20);
    - `return-addr.c` prints addresses of locals, which depend on the
      frame layout;
    - `20031003-1.c` converts an out-of-range `float` to `int`, which is
      undefined. We saturate, as the test expects; clang folds the
      constant to another value.
  - A known difference that disappears makes the test fail, so the list
    stays true.
- `FuzzCompile` checks that the generator never panics and that every
  object it emits is a valid module. `TestCompileSeeds` makes sure that
  each of its seeds still compiles; the fuzz target skips what is
  refused, so a seed that stopped compiling would be skipped without
  anyone noticing.

## Not yet

- csmith differential fuzzing (oracle against our kernel) comes with the code
  generator.
- The gcc `gcc.c-torture/compile` and `gcc.dg/torture` directories, and
  ccorpus2, are not run yet.
- The org landing page and the docs site wait for M4; this README is the only
  user-facing page at M0.

Licence: BSD 3-Clause (see `LICENSE`, and `NOTICE` for what is only run).
