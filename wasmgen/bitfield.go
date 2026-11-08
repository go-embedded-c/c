package wasmgen

import (
	"encoding/binary"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// Bit-fields (W6). cc/v4 lays a bit-field out as AccessBytes bytes at
// Offset() (its storage unit), the value in ValueBits bits from OffsetBits,
// little-endian. A read loads the unit and extracts the bits, sign-extended
// when the declared type is signed (a plain int bit-field is signed, as in
// clang); a write loads the unit, clears the field's bits, ors the new ones
// in and stores the unit back, which leaves the neighbours as they were.
// The layout itself is cc/v4's; the gcc torture census compares it with
// clang's.

// bitunit is a bit-field's storage unit and place in it.
type bitunit struct {
	s      scalar // the unit as an unsigned integer of 8, 16, 32 or 64 bits
	bytes  int64
	off    int64 // the field's first bit in the unit
	bits   int64 // the field's width
	signed bool  // the declared type's signedness
}

// bitfieldOf returns the field of e when e is a bit-field member.
func bitfieldOf(e cc.ExpressionNode) (*cc.PostfixExpression, *cc.Field) {
	x, ok := strip(e).(*cc.PostfixExpression)
	if !ok || x.Case != cc.PostfixExpressionSelect && x.Case != cc.PostfixExpressionPSelect {
		return nil, nil
	}
	if f := x.Field(); f != nil && f.IsBitfield() {
		return x, f
	}
	return nil, nil
}

// unitOf describes f's storage unit.
func unitOf(n cc.Node, f *cc.Field) (bitunit, error) {
	u := bitunit{bytes: f.AccessBytes(), off: int64(f.OffsetBits()), bits: f.ValueBits(), signed: cc.IsSignedInteger(f.Type())}
	switch u.bytes {
	case 1, 2, 4:
		u.s = scalar{vt: wasm.I32, bits: int(u.bytes) * 8}
	case 8:
		u.s = scalar{vt: wasm.I64, bits: 64}
	default:
		return u, unsupported(n, "bit-field in a storage unit of odd size")
	}
	if u.bits <= 0 || u.off < 0 || u.off+u.bits > u.bytes*8 {
		return u, unsupported(n, "bit-field outside its storage unit")
	}
	if f.Type().Size() > 4 && u.bits < 32 {
		// cc/v4 types every bit-field narrower than int as int
		// (check.go, fieldType); clang keeps long long for a long long
		// x:24, so the arithmetic on it and the argument it makes to `...`
		// would differ.
		return u, unsupported(n, "bit-field of a type wider than int, narrower than int")
	}
	return u, nil
}

// width is the unit's wasm type in bits.
func (u bitunit) width() int64 {
	if u.s.vt == wasm.I64 {
		return 64
	}
	return 32
}

// mask is the field's bits in the unit.
func (u bitunit) mask() uint64 {
	return (uint64(1)<<u.bits - 1) << u.off
}

// field is the representation of the extracted value: the unit's wasm type,
// the field's width and signedness, extended to the whole type.
func (u bitunit) field() scalar {
	return scalar{vt: u.s.vt, bits: int(u.width()), signed: u.signed}
}

// loadUnit loads the unit at the address on the stack, zero-extended.
func (g *fgen) loadUnit(u bitunit) {
	switch u.bytes {
	case 1:
		g.memarg(opI32Load8U, 0)
	case 2:
		g.memarg(opI32Load16U, 1)
	case 4:
		g.memarg(opI32Load, 2)
	default:
		g.memarg(opI64Load, 3)
	}
}

// storeUnit stores the unit on the stack at the address under it.
func (g *fgen) storeUnit(u bitunit) {
	switch u.bytes {
	case 1:
		g.memarg(opI32Store8, 0)
	case 2:
		g.memarg(opI32Store16, 1)
	case 4:
		g.memarg(opI32Store, 2)
	default:
		g.memarg(opI64Store, 3)
	}
}

// extract turns the unit on the stack into the field's value, in
// u.field(): shifted to bit 0 and sign- or zero-extended from its width.
func (g *fgen) extract(u bitunit) {
	w := u.width()
	s := u.field()
	if u.signed {
		if sh := w - u.off - u.bits; sh != 0 {
			g.constant(sh, s)
			g.op(opFor(opI32Shl, s))
		}
		if sh := w - u.bits; sh != 0 {
			g.constant(sh, s)
			g.op(opFor(opI32ShrS, s))
		}
		return
	}
	if u.off != 0 {
		g.constant(u.off, s)
		g.op(opFor(opI32ShrU, s))
	}
	if u.bits < w {
		g.constant(int64(uint64(1)<<u.bits-1), s)
		g.op(opFor(opI32And, s))
	}
}

// insert stores the value in local v (of u.field()'s wasm type) into the
// field whose unit is at the address in local a.
func (g *fgen) insert(u bitunit, a, v uint32) {
	s := u.field()
	g.op(opLocalGet)
	g.uleb(uint64(a))
	g.op(opLocalGet)
	g.uleb(uint64(a))
	g.loadUnit(u)
	g.constant(int64(^u.mask()), s)
	g.op(opFor(opI32And, s))
	g.op(opLocalGet)
	g.uleb(uint64(v))
	if u.off != 0 {
		g.constant(u.off, s)
		g.op(opFor(opI32Shl, s))
	}
	g.constant(int64(u.mask()), s)
	g.op(opFor(opI32And, s))
	g.op(opFor(opI32Or, s))
	g.storeUnit(u)
}

// staticBitfield writes the constant v into the field at byte off of seg.
func staticBitfield(seg []byte, off int64, u bitunit, v int64) {
	b := make([]byte, 8)
	copy(b, seg[off:off+u.bytes])
	unit := binary.LittleEndian.Uint64(b)
	unit = unit&^u.mask() | uint64(v)<<u.off&u.mask()
	binary.LittleEndian.PutUint64(b, unit)
	copy(seg[off:off+u.bytes], b)
}
