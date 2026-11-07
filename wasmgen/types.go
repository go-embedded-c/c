package wasmgen

import (
	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// scalar is how a C scalar type lives in wasm (BasicCABI.md, scalar types):
// the value type, the width in bits of the C type, and its signedness. A
// value narrower than its wasm type is kept normalised: sign- or
// zero-extended from its width, so comparisons and widening need no work.
type scalar struct {
	vt     wasm.ValType
	bits   int
	signed bool
	isBool bool
}

// scalarOf returns the representation of t, or false when W2 does not
// compile values of that type (pointers come with W4, floats with W5).
func scalarOf(t cc.Type) (scalar, bool) {
	if t == nil || !cc.IsIntegerType(t) {
		return scalar{}, false
	}
	bits := int(t.Size()) * 8
	s := scalar{bits: bits, signed: cc.IsSignedInteger(t), isBool: t.Kind() == cc.Bool}
	switch {
	case bits <= 32:
		s.vt = wasm.I32
	case bits == 64:
		s.vt = wasm.I64
	default:
		return scalar{}, false // __int128
	}
	return s, true
}

// isVoid reports whether t is void.
func isVoid(t cc.Type) bool { return t != nil && t.Kind() == cc.Void }

// Opcodes (core spec, binary/instructions.html).
const (
	opUnreachable = 0x00
	opDrop        = 0x1a
	opSelect      = 0x1b
	opReturn      = 0x0f
	opCall        = 0x10
	opLocalGet    = 0x20
	opLocalSet    = 0x21
	opLocalTee    = 0x22
	opI32Const    = 0x41
	opI64Const    = 0x42

	opI32Eqz = 0x45
	opI32Eq  = 0x46
	opI32Ne  = 0x47
	opI32LtS = 0x48
	opI32LtU = 0x49
	opI32GtS = 0x4a
	opI32GtU = 0x4b
	opI32LeS = 0x4c
	opI32LeU = 0x4d
	opI32GeS = 0x4e
	opI32GeU = 0x4f
	opI64Eqz = 0x50

	opI32Add  = 0x6a
	opI32Sub  = 0x6b
	opI32Mul  = 0x6c
	opI32DivS = 0x6d
	opI32DivU = 0x6e
	opI32RemS = 0x6f
	opI32RemU = 0x70
	opI32And  = 0x71
	opI32Or   = 0x72
	opI32Xor  = 0x73
	opI32Shl  = 0x74
	opI32ShrS = 0x75
	opI32ShrU = 0x76

	opI32WrapI64    = 0xa7
	opI64ExtendI32S = 0xac
	opI64ExtendI32U = 0xad
	opI32Extend8S   = 0xc0
	opI32Extend16S  = 0xc1
	i64Delta        = 0x7c - 0x6a // i64.add - i32.add, the same for every binary op from add to shr_u
	i64CompareDelta = 0x51 - 0x46 // i64.eq - i32.eq, the same for eq..ge_u
	opI32Load       = 0x28
	opI64Load       = 0x29
	opI32Load8S     = 0x2c
	opI32Load8U     = 0x2d
	opI32Load16S    = 0x2e
	opI32Load16U    = 0x2f
	opI32Store      = 0x36
	opI64Store      = 0x37
	opI32Store8     = 0x3a
	opI32Store16    = 0x3b
)

// load returns the load opcode and alignment (log2) for s.
func (s scalar) load() (op byte, align uint32) {
	switch {
	case s.vt == wasm.I64:
		return opI64Load, 3
	case s.bits == 8 && s.signed:
		return opI32Load8S, 0
	case s.bits == 8:
		return opI32Load8U, 0
	case s.bits == 16 && s.signed:
		return opI32Load16S, 1
	case s.bits == 16:
		return opI32Load16U, 1
	}
	return opI32Load, 2
}

// store returns the store opcode and alignment (log2) for s.
func (s scalar) store() (op byte, align uint32) {
	switch {
	case s.vt == wasm.I64:
		return opI64Store, 3
	case s.bits == 8:
		return opI32Store8, 0
	case s.bits == 16:
		return opI32Store16, 1
	}
	return opI32Store, 2
}
