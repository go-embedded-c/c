// Package wasm encodes WebAssembly modules in the binary format of the core
// specification (document/core/binary), and relocatable objects in the
// format of tool-conventions Linking.md: the "linking" section (version 2)
// and "reloc.*" sections that wasm-ld and wasi-libc's libc.a use.
//
// It encodes only; it does not validate. Tests check its output with
// wazero's validator and by linking it with the oracle's wasm-ld.
package wasm

// ValType is a value type (binary/types.html#value-types).
type ValType byte

// The number types.
const (
	I32 ValType = 0x7f
	I64 ValType = 0x7e
	F32 ValType = 0x7d
	F64 ValType = 0x7c
)

// FuncType is a function type: 0x60 params results.
type FuncType struct {
	Params, Results []ValType
}

// Func is a defined function. Body is its instructions without the final
// `end`, which Encode appends. Relocs are offsets into Body.
type Func struct {
	Type   uint32
	Locals []ValType
	Body   []byte
	Relocs []Reloc
}

// SymKind is a symbol kind (Linking.md, symbol table subsection).
type SymKind byte

// The symbol kinds.
const (
	SymFunction SymKind = 0
	SymData     SymKind = 1
	SymGlobal   SymKind = 2
	SymSection  SymKind = 3
	SymEvent    SymKind = 4
	SymTable    SymKind = 5
)

// Symbol flags (Linking.md).
const (
	SymBindingWeak  = 0x1
	SymBindingLocal = 0x2
	SymHidden       = 0x4
	SymUndefined    = 0x10
	SymExported     = 0x20
	SymExplicitName = 0x40
	SymNoStrip      = 0x80
)

// Symbol is an entry of the symbol table.
//
// For a function, Index is in the function index space (imports first); an
// undefined function (SymUndefined) refers to an import and its name is the
// import's. For data, Index is the segment, and Offset and Size place the
// object in it; an undefined data symbol has only its name.
type Symbol struct {
	Kind         SymKind
	Flags        uint32
	Index        uint32
	Name         string
	Offset, Size uint32
}

// Import is a function import: module "env", field Name, of type Type.
type Import struct {
	Name string
	Type uint32
}

// Segment is a data segment: Init bytes placed at Offset in memory 0, with
// a name and alignment (log2) for the segment info subsection.
type Segment struct {
	Name   string
	Align  uint32
	Offset uint32
	Init   []byte
}

// RelocType is a relocation type (Linking.md, relocation sections).
type RelocType byte

// The relocation types used so far.
const (
	RelocFunctionIndexLEB RelocType = 0
	RelocMemoryAddrLEB    RelocType = 3
	RelocMemoryAddrSLEB   RelocType = 4
	RelocGlobalIndexLEB   RelocType = 7
)

// hasAddend reports whether entries of type t carry an addend.
func (t RelocType) hasAddend() bool {
	switch t {
	case 3, 4, 5, 8, 9, 14, 15, 16, 23:
		return true
	}
	return false
}

// Reloc is a relocation of the 5-byte LEB at Offset in a function body,
// against symbol Symbol.
type Reloc struct {
	Type   RelocType
	Offset uint32
	Symbol uint32
	Addend int32
}

// Object is a relocatable object: the sections of one translation unit.
// Function indices count Imports first, then Funcs. Memory is imported as
// env.__linear_memory, as clang's objects do, when Memory is set or there
// are Segments.
type Object struct {
	Types    []FuncType
	Imports  []Import
	Memory   bool
	Funcs    []Func
	Segments []Segment
	Symbols  []Symbol
}
