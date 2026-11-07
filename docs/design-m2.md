# M2 design: `wasmgen`, from the cc/v4 AST to wasm32

M2 of [go-scikit/roadmap `bib/c-cpp.md`](https://github.com/go-scikit/roadmap/blob/main/bib/c-cpp.md) §5. It is the milestone the plan sizes **L**, larger than all the others. This file records what was read, the decisions taken, and the order of work. Each increment is measured by the same yardstick: how many gcc torture execute files we compile, link and run with the same output as clang.

## Sources read

| Source | Revision | What it fixes |
|---|---|---|
| WebAssembly tool-conventions `BasicCABI.md` | `25aceba5` (2026-09-25) | sizes and alignments; ILP32; `long double` = binary128 at 16/16; arrays of 16 bytes or more aligned to 16; how arguments and results are passed (scalars direct; singleton structs direct; other structs and unions indirect; an indirect result is a prepended pointer); varargs in a caller buffer passed as the last parameter; linear stack growing down, `SP` aligned to 16, frame pointer at the frame's bottom; `main` vs `__main_argc_argv` |
| tool-conventions `Linking.md` | same | the relocatable object format that `wasm-ld` and wasi-libc's `libc.a` use: `linking` and `reloc.*` custom sections, the symbol table, segment info, init functions |
| tool-conventions `SetjmpLongjmp.md` | same | `setjmp`/`longjmp` through the exception-handling proposal (the `__WasmLongjmpArgs` layout, the exception tag) |
| WebAssembly core spec `document/core` | `f3268fa9` (2026-11-06) | the instruction set and binary encoding we emit |
| N. Ramsey, "Beyond Relooper", PACMPL 6 ICFP, art. 90 (2022), doi:10.1145/3547621 | — | the structuring algorithm (§4–§7) and node splitting for irreducible graphs (appendix A) |
| LLVM `llvm/lib/Target/WebAssembly/WebAssemblyFixIrreducibleControlFlow.cpp` | the bibliography clone | the dispatch-block alternative to node splitting |
| `cell` and `internal/cc` (this repository) | v0.2 | the AST and types we start from; `cparse.ABI()` |

The full paper text is in `_bibliography/c-cpp/papers/`, not in this repository; it is CC BY-ND 4.0.

## Decisions

### Output: a relocatable object, linked by `wasm-ld` until M3

`wasmgen` writes one relocatable wasm object per translation unit, in the `Linking.md` format: the same format clang writes and that wasi-libc's `libc.a` is made of.

- **During M2**, the oracle's `wasm-ld` links our object with `crt1.o` and `libc.a` from the pinned sysroot. This means M2 is tested on whole C programs from its first increment, without first writing a linker.
- **In M3**, the cell linker replaces `wasm-ld` and reads the same format (a `dylink.0` subset for cells).

The test that matters always runs both builds in the same wazero:

- our object, linked by `wasm-ld`;
- clang's object, linked by `wasm-ld`.

It compares their exit status and output bytes. This isolates our code generator from the engine (plan §2.3).

### Pipeline

1. **AST to IR.** The IR is a per-function control-flow graph of basic blocks.
   - Each instruction works on typed virtual values (`i32`, `i64`, `f32`, `f64`), which become wasm locals.
   - An object whose address is taken, or that is an aggregate, lives in the function's frame on the linear stack.
   - Every function is lowered to a CFG, even when the source is structured. This keeps one path through the back end, so `goto`, `switch` fall-through and short-circuit operators need no special case.
2. **Structuring**, Ramsey §5: `doTree`, `nodeWithin` and `doBranch` over the dominator tree, with children ordered by reverse postorder. The three GHC improvements of §7 come in the same increment:
   - no redundant `br`;
   - no `block` wrapped around an `if`;
   - `switch` becomes `br_table`.
3. **Emission** of wasm instructions, with relocations for function indices, globals, data addresses and table slots.
4. **Encoding** of the module and its `linking` and `reloc.CODE`/`reloc.DATA` sections.

The dominator tree is computed by Lengauer–Tarjan, the algorithm GHC uses (Ramsey §7.5).

### Irreducible control flow is bounded

C produces irreducible graphs with `goto` into a loop. Ramsey's appendix A turns such a graph into a reducible one by node splitting, and the naive greedy strategy can duplicate a lot of code. A notebook cell is untrusted input, so a cell of `goto`s must not be able to blow up compile time or module size.

- Splitting is bounded: the number of duplicated IR instructions is limited to a fraction of the function's size.
- Past the bound, a Diagnostic refuses the function, so the failure is a refusal and not a hang.
- A dispatch loop is the planned fallback if the corpora show the bound is too tight. LLVM's `WebAssemblyFixIrreducibleControlFlow.cpp:23-26` does this: it adds "a new block that can dispatch to each of the loop entries". It costs a little run time but duplicates no code.

### ABI

The ABI is `BasicCABI.md` version 1, which clang follows:

- the stack pointer is the mutable global `__stack_pointer`, imported as `wasm-ld` expects;
- frames are 16-aligned, and the red zone is not used at first;
- structs follow the singleton-or-indirect rule;
- 8- and 16-bit scalar arguments are sign- or zero-extended;
- the varargs buffer follows the table in `BasicCABI.md`.

`long double` and `__int128` arithmetic calls the compiler-rt builtins (`__addtf3` and the like), as clang does. They are in the pinned toolchain's resource directory, `lib/clang/23/lib/wasm32-unknown-wasip1`, not in the wasi-libc sysroot. M3 has to embed them alongside `libc.a`.

## Order of work

Each increment ends with a merged PR. Each PR also adds a test that holds the gcc torture execute pass count as a floor, the way `cparse` holds its parse floors.

| | Content | Done when |
|---|---|---|
| W1 | module encoder, object writer (`linking`, `reloc.*`), `int main(void) { return 0; }`, the run harness (`wasm-ld` + wazero against clang) | the harness passes on `return 0` and refuses a wrong exit code |
| W2 | integer expressions, locals, globals, calls, `return`, string literals in data, a `printf` call through libc | torture files with no float, no aggregate and no `goto` |
| W3 | CFG, dominators, Ramsey structuring; `if`, loops, `break`/`continue`, `&&`/`\|\|`/`?:`, `switch` → `br_table` | every torture file whose functions are reducible and use only W2 features |
| W4 | memory: pointers, arrays, structs, unions, the linear-stack frame, aggregate copy, struct arguments and results | the pass count rises; refusals are counted by cause |
| W5 | `float`/`double`, conversions, comparisons | — |
| W6 | varargs, bit-fields, compound literals, initialisers of every kind | — |
| W7 | `goto`, irreducible graphs, the splitting bound | — |
| W8 | `long double`, `_Complex`, `setjmp`/`longjmp` (exnref, behind `CoreFeaturesExceptionHandling` in wazero) | the M5 gate: at least ccgo's pass rate on the same files |

## Security and performance

Every increment applies both checks below.

**Security**

- The generator is fed the AST of untrusted cells, so it is bounded: function size, nesting, the splitting budget, data size.
- It is fuzzed from `FuzzAnalyze`'s corpus. Every refusal is a Diagnostic, never a panic.
- The object writer is checked by reading its own output back, and `wasm-ld` must accept it.

**Performance**

- From W3 on, each measured file is timed under the same wazero in two builds: our object, and clang's at `-O0` and at `-O2`. Measurements are interleaved on one core.
- The first target is parity with clang `-O0`. The gap to `-O2` stays an open defect, closed by an optimiser after correctness.
