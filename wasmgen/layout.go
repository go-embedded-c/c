package wasmgen

import (
	"github.com/go-embedded-c/c/internal/cc"
)

// cc/v4 lays bit-fields out in "groups" sized by the declared type
// (check.go, fieldAllocator), which is not the rule clang follows for
// wasm32, the Itanium/SysV one (clang's RecordLayoutBuilder for a non-MS
// target): a bit-field goes at the next free bit unless it would straddle
// a boundary of its type's alignment, and then at that boundary; the next
// ordinary member goes at the first byte after the bits used, at its
// alignment. struct { unsigned long long f:44; _Bool g:1; short h; } has h
// at 6 in clang, at 8 in cc/v4. A structure with bit-fields whose layout
// cc/v4 computes otherwise than that rule is refused: every offset the
// generator uses comes from cc/v4.

const layoutRefusal = "bit-field layout that cc/v4 computes otherwise than clang"

// layoutOK reports whether cc/v4's layout of t agrees with clang's. A type
// without bit-fields is cc/v4's alone (W4b's census checked it); the
// verdict is kept per type.
func (m *module) layoutOK(t cc.Type) bool {
	if ok, seen := m.layouts[t]; seen {
		return ok
	}
	ok := true
	switch x := t.(type) {
	case *cc.ArrayType:
		ok = m.layoutOK(x.Elem())
	case *cc.StructType:
		ok = m.fieldsOK(x.NumFields(), x.FieldByIndex) && structLayoutOK(x)
	case *cc.UnionType:
		ok = m.fieldsOK(x.NumFields(), x.FieldByIndex) && unionLayoutOK(x)
	}
	m.layouts[t] = ok
	return ok
}

func (m *module) fieldsOK(n int, field func(int) *cc.Field) bool {
	for i := range n {
		if f := field(i); f != nil && !m.layoutOK(f.Type()) {
			return false
		}
	}
	return true
}

func hasBitfields(n int, field func(int) *cc.Field) bool {
	for i := range n {
		if f := field(i); f != nil && f.IsBitfield() {
			return true
		}
	}
	return false
}

func roundUp(n, a int64) int64 {
	if a <= 1 {
		return n
	}
	return (n + a - 1) / a * a
}

// fieldAlign is a member's alignment: its type's, or more by _Alignas.
func fieldAlign(f *cc.Field) int64 {
	a := int64(f.Type().Align())
	if d := f.Declarator(); d != nil {
		a = max(a, int64(d.Alignas()))
	}
	return max(a, 1)
}

// structLayoutOK computes the Itanium layout of a structure with bit-fields
// and compares every member's bit position, the size and the alignment.
func structLayoutOK(t *cc.StructType) bool {
	if !hasBitfields(t.NumFields(), t.FieldByIndex) {
		return true
	}
	var bit int64 // the next free bit
	align := int64(1)
	for i := range t.NumFields() {
		f := t.FieldByIndex(i)
		if f == nil {
			continue
		}
		fa := fieldAlign(f)
		if !f.IsBitfield() {
			off := roundUp((bit+7)/8, fa)
			if off != f.Offset() {
				return false
			}
			size := f.Type().Size()
			if f.IsFlexibleArrayMember() || size < 0 {
				size = 0
			}
			bit = (off + size) * 8
			align = max(align, fa)
			continue
		}
		w := f.ValueBits()
		unit := fa * 8
		if w == 0 {
			// An unnamed zero-width bit-field ends the unit: the next
			// field starts at its type's boundary. It does not align the
			// structure.
			bit = roundUp(bit, unit)
			continue
		}
		if bit/unit != (bit+w-1)/unit {
			bit = roundUp(bit, unit)
		}
		if f.Offset()*8+int64(f.OffsetBits()) != bit {
			return false
		}
		bit += w
		if f.Name() != "" {
			align = max(align, fa)
		}
	}
	size := roundUp((bit+7)/8, align)
	return size == t.Size() && align == int64(t.Align())
}

// unionLayoutOK checks a union with bit-fields: every member at bit 0, the
// size the largest member's rounded up to the alignment.
func unionLayoutOK(t *cc.UnionType) bool {
	if !hasBitfields(t.NumFields(), t.FieldByIndex) {
		return true
	}
	var size int64
	align := int64(1)
	for i := range t.NumFields() {
		f := t.FieldByIndex(i)
		if f == nil {
			continue
		}
		if f.Offset() != 0 || f.IsBitfield() && f.OffsetBits() != 0 {
			return false
		}
		fa := fieldAlign(f)
		if f.IsBitfield() {
			size = max(size, (f.ValueBits()+7)/8)
			if f.Name() != "" && f.ValueBits() != 0 {
				align = max(align, fa)
			}
			continue
		}
		size = max(size, f.Type().Size())
		align = max(align, fa)
	}
	return roundUp(size, align) == t.Size() && align == int64(t.Align())
}
