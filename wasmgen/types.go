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
	// isFloat: float (f32) or double (f64); signed is set, so that the
	// integer paths that pick an operation by signedness pick the
	// signed, IEEE-compatible one where they do not know floats.
	isFloat bool
	ptr     bool  // a pointer: an unsigned 32-bit address
	elem    int64 // for a pointer, the size of what it points to (1 for void, as GNU C)
}

// scalarOf returns the representation of t, or false when the generator
// does not compile values of that type yet (floats come with W5).
func scalarOf(t cc.Type) (scalar, bool) {
	if t == nil {
		return scalar{}, false
	}
	if p, ok := t.(*cc.PointerType); ok {
		elem := undecayed(p.Elem()).Size()
		if elem <= 0 || isVoid(p.Elem()) {
			elem = 1
		}
		return scalar{vt: wasm.I32, bits: 32, ptr: true, elem: elem}, true
	}
	if t.VectorSize() > 0 {
		// A GNU vector (vector_size): cc/v4 keeps the element's kind, so
		// float __attribute__((vector_size(16))) would read as a float
		// (gcc torture 20060420-1.c). Not a scalar.
		return scalar{}, false
	}
	if s, ok := floatScalarOf(t); ok {
		return s, true
	}
	if !cc.IsIntegerType(t) {
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

// undecayed returns the array a type cc/v4 already decayed stands for, or t.
// cc/v4 types an array operand as the pointer it decays to, even under &:
// &a[0][0] of char a[2][2][5] is typed as a pointer to char *, not to
// char[5], and steps by 4 instead of 5 (gcc torture strlen-4.c). The
// decayed pointer keeps the array as its Undecay; any other pointer's
// Undecay is itself.
func undecayed(t cc.Type) cc.Type {
	if p, ok := t.(*cc.PointerType); ok {
		if a, ok := p.Undecay().(*cc.ArrayType); ok {
			return a
		}
	}
	return t
}

// hasVLA reports whether the structure or union t has a variable-length
// array member, a GNU extension clang refuses ("will never be supported");
// its layout is not static, so the generator refuses it too.
func hasVLA(t cc.Type) bool {
	var n int
	var field func(int) *cc.Field
	switch x := t.(type) {
	case *cc.StructType:
		n, field = x.NumFields(), x.FieldByIndex
	case *cc.UnionType:
		n, field = x.NumFields(), x.FieldByIndex
	case *cc.ArrayType:
		return x.IsVLA() || hasVLA(x.Elem())
	default:
		return false
	}
	for i := range n {
		if f := field(i); f != nil && hasVLA(f.Type()) {
			return true
		}
	}
	return false
}

// isAggregate reports whether t is a struct or union.
func isAggregate(t cc.Type) bool {
	if t == nil {
		return false
	}
	k := t.Kind()
	return k == cc.Struct || k == cc.Union
}

// passing is how BasicCABI.md passes a value of a type, as a parameter or
// a result: scalars and singleton aggregates directly, an empty aggregate
// not at all, any other aggregate through a pointer (indirect).
type passing int

const (
	passDirect passing = iota
	passIgnored
	passIndirect
)

// abiOf classifies t; s is the wasm scalar for passDirect.
func abiOf(t cc.Type) (passing, scalar, bool) {
	if !isAggregate(t) {
		s, ok := scalarOf(t)
		return passDirect, s, ok
	}
	if t.IsIncomplete() || hasVLA(t) {
		return 0, scalar{}, false
	}
	if t.Size() == 0 {
		return passIgnored, scalar{}, true
	}
	if s, ok := singleton(t); ok {
		return passDirect, s, true
	}
	return passIndirect, scalar{vt: wasm.I32, bits: 32, ptr: true, elem: t.Size()}, true
}

// singleton is BasicCABI.md's "struct or union that recursively (including
// through nested structs, unions, and arrays) contains just a single
// scalar value and is not specified to have greater than natural
// alignment": it is passed as that scalar.
func singleton(t cc.Type) (scalar, bool) {
	switch x := t.(type) {
	case *cc.ArrayType:
		if x.Len() != 1 {
			return scalar{}, false
		}
		return singleton(x.Elem())
	case *cc.StructType, *cc.UnionType:
		var fields []*cc.Field
		var n int
		if st, ok := x.(*cc.StructType); ok {
			n = st.NumFields()
			for i := range n {
				fields = append(fields, st.FieldByIndex(i))
			}
		} else {
			ut := x.(*cc.UnionType)
			n = ut.NumFields()
			for i := range n {
				fields = append(fields, ut.FieldByIndex(i))
			}
		}
		var only *cc.Field
		for _, f := range fields {
			if f == nil || f.Type().Size() == 0 {
				continue
			}
			if only != nil || f.IsBitfield() {
				return scalar{}, false
			}
			only = f
		}
		if only == nil || t.Align() > only.Type().Align() || t.Size() != only.Type().Size() {
			return scalar{}, false
		}
		return singleton(only.Type())
	}
	return scalarOf(t)
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
	opGlobalGet   = 0x23
	opGlobalSet   = 0x24
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
	case s.vt == wasm.F32:
		return opF32Load, 2
	case s.vt == wasm.F64:
		return opF64Load, 3
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
	case s.vt == wasm.F32:
		return opF32Store, 2
	case s.vt == wasm.F64:
		return opF64Store, 3
	case s.vt == wasm.I64:
		return opI64Store, 3
	case s.bits == 8:
		return opI32Store8, 0
	case s.bits == 16:
		return opI32Store16, 1
	}
	return opI32Store, 2
}
