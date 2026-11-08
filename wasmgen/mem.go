package wasmgen

import (
	"encoding/binary"
	"strings"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// Memory (W4a): pointers, arrays, objects whose address is taken, string
// literals, and the static initialisers that hold addresses.
//
// A local whose address is taken, or an array, lives in the function's
// frame on the linear stack (BasicCABI.md): the prologue moves
// __stack_pointer down by the frame size, keeping its old value in a local
// (top) and the new one in another (fp); every return restores top. An
// object's address is fp plus its offset.

// staticInit writes the initialiser of a static object into seg: constants
// as bytes, addresses as R_WASM_MEMORY_ADDR_I32 relocations. cc/v4 gives
// every leaf its offset from the start of the object.
func (m *module) staticInit(in *cc.Initializer, seg *wasm.Segment) error {
	if in.AssignmentExpression == nil {
		for l := in.InitializerList; l != nil; l = l.InitializerList {
			if err := m.staticInit(l.Initializer, seg); err != nil {
				return err
			}
		}
		return nil
	}
	if bitfieldLeaf(in) {
		return unsupported(in, "bit-field initializer (W6)")
	}
	t, e, off := in.Type(), in.AssignmentExpression, in.Offset()
	for i := range in.Len() {
		at := off + i*t.Size()
		if at < 0 || at+t.Size() > int64(len(seg.Init)) {
			return unsupported(in, "initializer outside its object")
		}
		if err := m.staticValue(e, t, seg, at); err != nil {
			return err
		}
	}
	return nil
}

// bitfieldLeaf reports whether the initializer leaf in initializes a
// bit-field: its bits share bytes with its neighbours, so a store of its
// type would overwrite them (gcc torture 20230630-1.c).
func bitfieldLeaf(in *cc.Initializer) bool {
	f := in.Field()
	return f != nil && f.IsBitfield()
}

// staticValue writes the value of e, of type t, at off in seg.
func (m *module) staticValue(e cc.ExpressionNode, t cc.Type, seg *wasm.Segment, off int64) error {
	if a, ok := t.(*cc.ArrayType); ok {
		// char s[N] = "…": the literal's bytes, then zeros.
		v, ok := stringValue(e)
		if !ok || a.Elem().Size() != 1 {
			return unsupported(e, "array initializer")
		}
		copy(seg.Init[off:off+t.Size()], v)
		return nil
	}
	s, ok := scalarOf(t)
	if !ok {
		return unsupported(e, "initializer of type "+typeName(t))
	}
	if v, ok := intValue(e); ok {
		b := binary.LittleEndian.AppendUint64(nil, uint64(convertConst(v, s)))
		copy(seg.Init[off:], b[:s.bits/8])
		return nil
	}
	if s.bits != 32 {
		return unsupported(e, "non-constant initializer")
	}
	sym, addend, err := m.staticAddr(e)
	if err != nil {
		return err
	}
	seg.Relocs = append(seg.Relocs, wasm.Reloc{Type: wasm.RelocMemoryAddrI32, Offset: uint32(off), Symbol: sym, Addend: int32(addend)})
	return nil
}

// stringValue returns the bytes of a string literal expression.
func stringValue(e cc.ExpressionNode) (string, bool) {
	e = strip(e)
	if p, ok := e.(*cc.PrimaryExpression); ok && p.Case == cc.PrimaryExpressionString {
		if v, ok := p.Value().(cc.StringValue); ok {
			return string(v), true
		}
	}
	return "", false
}

// literalSize is the size of a string literal's array: its bytes and the
// terminating NUL. cc/v4 types the literal already decayed to a pointer,
// so the type does not give it.
func literalSize(v cc.StringValue) int64 {
	if strings.HasSuffix(string(v), "\x00") {
		return int64(len(v))
	}
	return int64(len(v)) + 1
}

// strip removes parentheses and single-element comma lists.
func strip(e cc.ExpressionNode) cc.ExpressionNode {
	for {
		switch x := e.(type) {
		case *cc.PrimaryExpression:
			if x.Case == cc.PrimaryExpressionExpr {
				e = x.ExpressionList
				continue
			}
		case *cc.ExpressionList:
			if x.ExpressionList == nil {
				e = x.AssignmentExpression
				continue
			}
		case *cc.PostfixExpression:
			if x.Case == cc.PostfixExpressionPrimary {
				e = x.PrimaryExpression
				continue
			}
		case *cc.CastExpression:
			if x.Case == cc.CastExpressionUnary {
				e = x.UnaryExpression
				continue
			}
		case *cc.UnaryExpression:
			if x.Case == cc.UnaryExpressionPostfix {
				e = x.PostfixExpression
				continue
			}
		}
		return e
	}
}

// staticAddr evaluates an address constant (C11 6.6p9): the address of a
// static object, an array that decays, or a string literal, plus or minus
// an integer constant. It returns the data symbol and the byte addend.
func (m *module) staticAddr(e cc.ExpressionNode) (uint32, int64, error) {
	e = strip(e)
	switch x := e.(type) {
	case *cc.PrimaryExpression:
		switch x.Case {
		case cc.PrimaryExpressionString:
			v, ok := x.Value().(cc.StringValue)
			if !ok {
				return 0, 0, unsupported(x, "wide string literal")
			}
			return m.stringLiteral(string(v), literalSize(v)), 0, nil
		case cc.PrimaryExpressionIdent:
			d, ok := x.ResolvedTo().(*cc.Declarator)
			if ok && (d.Type().Kind() == cc.Array || d.Type().Kind() == cc.Function) && d.StorageDuration() == cc.Static && !d.IsParam() {
				return m.staticObject(x, d)
			}
		}
	case *cc.CastExpression:
		if s, ok := scalarOf(x.Type()); ok && s.bits == 32 {
			return m.staticAddr(x.CastExpression)
		}
	case *cc.UnaryExpression:
		if x.Case == cc.UnaryExpressionAddrof {
			return m.staticLvalue(x.CastExpression)
		}
	case *cc.AdditiveExpression:
		if s, ok := scalarOf(x.Type()); ok && s.ptr {
			a, b := x.AdditiveExpression, x.MultiplicativeExpression
			if _, ok := scalarOf(a.Type()); !ok || !isPtrOrArray(a.Type()) {
				a, b = b, a
			}
			k, ok := intValue(b)
			if !ok {
				break
			}
			if x.Case == cc.AdditiveExpressionSub {
				k = -k
			}
			sym, addend, err := m.staticAddr(a)
			return sym, addend + k*s.elem, err
		}
	}
	return 0, 0, unsupported(e, "non-constant initializer")
}

func isPtrOrArray(t cc.Type) bool { return t.Kind() == cc.Ptr || t.Kind() == cc.Array }

// staticLvalue is the address of a static lvalue: x, a[k], *(&…).
func (m *module) staticLvalue(e cc.ExpressionNode) (uint32, int64, error) {
	e = strip(e)
	switch x := e.(type) {
	case *cc.PrimaryExpression:
		if x.Case == cc.PrimaryExpressionString {
			return m.staticAddr(x)
		}
		if x.Case == cc.PrimaryExpressionIdent {
			if d, ok := x.ResolvedTo().(*cc.Declarator); ok && d.StorageDuration() == cc.Static && !d.IsParam() {
				return m.staticObject(x, d)
			}
		}
	case *cc.PostfixExpression:
		if x.Case == cc.PostfixExpressionIndex {
			k, ok := intValue(x.ExpressionList)
			if ok {
				sym, addend, err := m.staticAddr(x.PostfixExpression)
				return sym, addend + k*x.Type().Size(), err
			}
		}
	case *cc.UnaryExpression:
		if x.Case == cc.UnaryExpressionDeref {
			return m.staticAddr(x.CastExpression)
		}
	}
	return 0, 0, unsupported(e, "non-constant initializer")
}

// staticObject returns the symbol of a static object, file-scope or not.
func (m *module) staticObject(n cc.Node, d *cc.Declarator) (uint32, int64, error) {
	if d.Type().Kind() == cc.Function {
		return 0, 0, unsupported(n, "address of a function (W4c)")
	}
	if d.Linkage() == cc.None {
		return 0, 0, unsupported(n, "address of a static local")
	}
	ds, err := m.global(n, d)
	if err != nil {
		return 0, 0, err
	}
	return ds.sym, 0, nil
}

// frameSlot allocates size bytes at align in the frame.
func (g *fgen) frameSlot(size int64, align int) int64 {
	a := int64(max(align, 1))
	off := (g.frameSize + a - 1) &^ (a - 1)
	g.frameSize = off + size
	return off
}

// prologue returns the code that sets the frame up, to put before the
// entry block: top = __stack_pointer; fp = top - size; __stack_pointer = fp;
// then the address-taken parameters are copied to their slots.
func (g *fgen) prologue() *block {
	b := &block{}
	saved := g.cur
	g.cur = b
	size := (g.frameSize + 15) &^ 15 // the stack pointer stays 16-aligned
	sp := g.m.stackPointer()
	g.op(opGlobalGet)
	g.reloc(wasm.RelocGlobalIndexLEB, sp)
	g.op(opLocalTee)
	g.uleb(uint64(g.top))
	g.constant(size, intScalar)
	g.op(opI32Sub, opLocalTee)
	g.uleb(uint64(g.fp))
	g.op(opGlobalSet)
	g.reloc(wasm.RelocGlobalIndexLEB, sp)
	for _, p := range g.spilled {
		g.slotAddress(p.slot)
		g.op(opLocalGet)
		g.uleb(uint64(p.param))
		op, align := p.s.store()
		g.op(op)
		g.uleb(uint64(align))
		g.uleb(0)
	}
	g.cur = saved
	return b
}

// epilogue restores the stack pointer before a return.
func (g *fgen) epilogue() {
	if !g.frame {
		return
	}
	g.op(opLocalGet)
	g.uleb(uint64(g.top))
	g.op(opGlobalSet)
	g.reloc(wasm.RelocGlobalIndexLEB, g.m.stackPointer())
}

// slotAddress pushes fp + off.
func (g *fgen) slotAddress(off int64) {
	g.op(opLocalGet)
	g.uleb(uint64(g.fp))
	if off != 0 {
		g.constant(off, intScalar)
		g.op(opI32Add)
	}
}

// addr pushes the address of the lvalue e, or of the array or string
// literal it designates.
func (g *fgen) addr(e cc.ExpressionNode) error {
	if err := g.enter(e); err != nil {
		return err
	}
	defer func() { g.depth-- }()
	e = strip(e)
	switch x := e.(type) {
	case *cc.PrimaryExpression:
		switch x.Case {
		case cc.PrimaryExpressionString:
			v, ok := x.Value().(cc.StringValue)
			if !ok {
				return unsupported(x, "wide string literal")
			}
			g.op(opI32Const)
			g.reloc(wasm.RelocMemoryAddrSLEB, g.m.stringLiteral(string(v), literalSize(v)))
			return nil
		case cc.PrimaryExpressionIdent:
			d, ok := x.ResolvedTo().(*cc.Declarator)
			if !ok {
				break
			}
			if off, ok := g.slots[d]; ok {
				g.slotAddress(off)
				return nil
			}
			if i, ok := g.refs[d]; ok {
				g.op(opLocalGet)
				g.uleb(uint64(i))
				return nil
			}
			if _, ok := g.vars[d]; ok {
				return unsupported(x, "address of a register local")
			}
			if d.StorageDuration() != cc.Static || d.IsParam() {
				return unsupported(x, "object without storage")
			}
			sym, _, err := g.m.staticObject(x, d)
			if err != nil {
				return err
			}
			g.op(opI32Const)
			g.reloc(wasm.RelocMemoryAddrSLEB, sym)
			return nil
		}
	case *cc.UnaryExpression:
		if x.Case == cc.UnaryExpressionDeref {
			return g.ptrValue(x.CastExpression)
		}
	case *cc.PostfixExpression:
		switch x.Case {
		case cc.PostfixExpressionSelect, cc.PostfixExpressionPSelect:
			// E.m and E->m (C11 6.5.2.3): the member's offset from the
			// address of the structure or union.
			f := x.Field()
			if f == nil {
				return unsupported(x, "member")
			}
			if f.IsBitfield() {
				return unsupported(x, "bit-field member (W6)")
			}
			var err error
			if x.Case == cc.PostfixExpressionSelect {
				err = g.aggAddr(x.PostfixExpression)
			} else {
				err = g.ptrValue(x.PostfixExpression)
			}
			if err != nil {
				return err
			}
			if off := f.Offset(); off != 0 {
				g.constant(off, intScalar)
				g.op(opI32Add)
			}
			return nil
		}
		if x.Case == cc.PostfixExpressionIndex {
			// E1[E2] is *(E1 + E2); either operand may be the pointer.
			p, i := x.PostfixExpression, x.ExpressionList
			if !isPtrOrArray(p.Type()) {
				p, i = i, p
			}
			if err := g.ptrValue(p); err != nil {
				return err
			}
			// The element's size comes from what p points to: cc/v4 types
			// m[i] of int m[3][4] as the decayed int *, not as int[4].
			if err := g.offset(i, elemOf(p.Type()).Size()); err != nil {
				return err
			}
			g.op(opI32Add)
			return nil
		}
	}
	return unsupported(e, "lvalue")
}

// aggAddr pushes the address of a structure or union value: an lvalue's
// object, or a frame temporary holding the result of a call, an assignment,
// E1 ? E2 : E3 or a comma expression.
func (g *fgen) aggAddr(e cc.ExpressionNode) error {
	if err := g.enter(e); err != nil {
		return err
	}
	defer func() { g.depth-- }()
	e = strip(e)
	switch x := e.(type) {
	case *cc.PostfixExpression:
		if x.Case == cc.PostfixExpressionCall {
			var slot int64
			if err := g.callAgg(x, &slot); err != nil {
				return err
			}
			g.slotAddress(slot)
			return nil
		}
	case *cc.AssignmentExpression:
		switch x.Case {
		case cc.AssignmentExpressionCond:
			return g.aggAddr(x.ConditionalExpression)
		case cc.AssignmentExpressionAssign:
			return g.aggAssign(x, true)
		}
		return unsupported(x, "assignment "+x.Case.String()+" of a structure")
	case *cc.ConditionalExpression:
		if x.Case == cc.ConditionalExpressionLOr {
			break
		}
		if x.ExpressionList == nil {
			return unsupported(x, "?: without a middle operand")
		}
		if err := g.condition(x.LogicalOrExpression); err != nil {
			return err
		}
		g.op(opIf, byte(wasm.I32))
		if err := g.aggAddr(x.ExpressionList); err != nil {
			return err
		}
		g.op(opElse)
		if err := g.aggAddr(x.ConditionalExpression); err != nil {
			return err
		}
		g.op(opEnd)
		return nil
	case *cc.ExpressionList:
		for l := x; l != nil; l = l.ExpressionList {
			if l.ExpressionList == nil {
				return g.aggAddr(l.AssignmentExpression)
			}
			if err := g.effect(l.AssignmentExpression); err != nil {
				return err
			}
		}
	}
	return g.addr(e)
}

// aggAssign compiles E1 = E2 for structures and unions: E2 first, as for
// scalars, then a copy of its bytes over E1's. With want, it leaves E1's
// address.
func (g *fgen) aggAssign(x *cc.AssignmentExpression, want bool) error {
	t := x.UnaryExpression.Type()
	if t.IsIncomplete() {
		return unsupported(x, "assignment of an incomplete type")
	}
	if err := g.aggAddr(x.AssignmentExpression); err != nil {
		return err
	}
	src := g.local(wasm.I32)
	g.op(opLocalSet)
	g.uleb(uint64(src))
	if err := g.addr(x.UnaryExpression); err != nil {
		return err
	}
	var dst uint32
	if want {
		dst = g.local(wasm.I32)
		g.op(opLocalTee)
		g.uleb(uint64(dst))
	}
	g.op(opLocalGet)
	g.uleb(uint64(src))
	g.constant(t.Size(), intScalar)
	g.op(0xfc, 10, 0, 0) // memory.copy
	if want {
		g.op(opLocalGet)
		g.uleb(uint64(dst))
	}
	return nil
}

// elemOf is the type a pointer or array type has elements of.
func elemOf(t cc.Type) cc.Type {
	switch x := t.(type) {
	case *cc.PointerType:
		return x.Elem()
	case *cc.ArrayType:
		return x.Elem()
	}
	return t
}

// designatesArray reports whether e designates an array object, which
// converts to its address (C11 6.3.2.1p3). cc/v4 often types such an
// operand already decayed, so the object is found from the declarator, or
// from what the indexed or dereferenced pointer points to.
func designatesArray(e cc.ExpressionNode) bool {
	if e.Type().Kind() == cc.Array {
		return true
	}
	e = strip(e)
	switch x := e.(type) {
	case *cc.PrimaryExpression:
		switch x.Case {
		case cc.PrimaryExpressionString:
			return true
		case cc.PrimaryExpressionIdent:
			d, ok := x.ResolvedTo().(*cc.Declarator)
			return ok && d.Type().Kind() == cc.Array
		}
	case *cc.PostfixExpression:
		if (x.Case == cc.PostfixExpressionSelect || x.Case == cc.PostfixExpressionPSelect) && x.Field() != nil {
			return x.Field().Type().Kind() == cc.Array
		}
		if x.Case == cc.PostfixExpressionIndex {
			p := x.PostfixExpression
			if !isPtrOrArray(p.Type()) {
				p = x.ExpressionList
			}
			return elemOf(p.Type()).Kind() == cc.Array
		}
	case *cc.UnaryExpression:
		if x.Case == cc.UnaryExpressionDeref {
			return elemOf(x.CastExpression.Type()).Kind() == cc.Array
		}
	}
	return false
}

// ptrValue pushes the value of an expression of pointer type, or the
// address of one that designates an array (C11 6.3.2.1p3).
func (g *fgen) ptrValue(e cc.ExpressionNode) error {
	if designatesArray(e) {
		return g.addr(e)
	}
	from, err := g.rvalue(e)
	if err != nil {
		return err
	}
	if !from.ptr {
		g.convert(from, scalar{vt: wasm.I32, bits: 32})
	}
	return nil
}

// offset pushes the integer e times size, as a 32-bit address offset.
func (g *fgen) offset(e cc.ExpressionNode, size int64) error {
	from, err := g.rvalue(e)
	if err != nil {
		return err
	}
	g.convert(from, intScalar)
	if size != 1 {
		g.constant(size, intScalar)
		g.op(opI32Mul)
	}
	return nil
}

// memoryLocal gives a local that lives in memory its frame slot and runs
// its initialiser: an aggregate is zeroed (memory.fill), then each leaf is
// stored, as C11 6.7.9p21 leaves the rest of the object zero.
func (g *fgen) memoryLocal(d *cc.Declarator, id *cc.InitDeclarator) error {
	t := d.Type()
	if a, ok := t.(*cc.ArrayType); ok && a.IsVLA() {
		return unsupported(d, "variable-length array")
	}
	size, err := objectSize(d, id)
	if err != nil {
		return err
	}
	if !storable(t) {
		return unsupported(d, "local of type "+typeName(t))
	}
	if size > maxObject {
		return unsupported(d, "local larger than the limit")
	}
	slot, ok := g.slots[d]
	if !ok {
		slot = g.frameSlot(size, max(t.Align(), d.Alignas()))
		g.slots[d] = slot
	}
	if id.Case != cc.InitDeclaratorInit {
		return nil
	}
	if _, scalar := scalarOf(t); !scalar {
		g.slotAddress(slot)
		g.constant(0, intScalar)
		g.constant(size, intScalar)
		g.op(0xfc, 11, 0) // memory.fill
	}
	return g.localInit(id.Initializer, slot)
}

// objectSize is the size of the object d defines. An array of unknown size
// takes it from its initialiser (C11 6.7.9p22): a string literal's length
// and NUL for char s[] = "…", which cc/v4 types as the decayed pointer, or
// the braced list's type; GNU C's `int d[][8] = {}` has size 0.
func objectSize(d *cc.Declarator, id *cc.InitDeclarator) (int64, error) {
	t := d.Type()
	if size := t.Size(); size >= 0 && !t.IsIncomplete() {
		return size, nil
	}
	if id.Case != cc.InitDeclaratorInit {
		return 0, unsupported(d, "object of incomplete type")
	}
	in := id.Initializer
	if v, ok := stringValue(in.AssignmentExpression); ok && in.AssignmentExpression != nil {
		return literalSize(cc.StringValue(v)) * elemOf(t).Size(), nil
	}
	if size := in.Type().Size(); size > 0 && !in.Type().IsIncomplete() {
		return size, nil
	}
	if in.InitializerList == nil {
		return 0, nil
	}
	return 0, unsupported(d, "object whose size the initializer does not give")
}

// localInit stores the leaves of an initialiser into the object at slot.
func (g *fgen) localInit(in *cc.Initializer, slot int64) error {
	if in.AssignmentExpression == nil {
		for l := in.InitializerList; l != nil; l = l.InitializerList {
			if err := g.localInit(l.Initializer, slot); err != nil {
				return err
			}
		}
		return nil
	}
	if bitfieldLeaf(in) {
		return unsupported(in, "bit-field initializer (W6)")
	}
	t, e := in.Type(), in.AssignmentExpression
	for i := range in.Len() {
		at := slot + in.Offset() + i*t.Size()
		if a, ok := t.(*cc.ArrayType); ok {
			// char s[N] = "…": copy the literal, at most N bytes.
			v, ok := stringValue(e)
			if !ok || a.Elem().Size() != 1 {
				return unsupported(e, "array initializer")
			}
			g.slotAddress(at)
			if err := g.addr(e); err != nil {
				return err
			}
			g.constant(min(t.Size(), literalSize(cc.StringValue(v))), intScalar)
			g.op(0xfc, 10, 0, 0) // memory.copy
			continue
		}
		if isAggregate(t) {
			// struct S b = a: a copy of the value's bytes.
			g.slotAddress(at)
			if err := g.aggAddr(e); err != nil {
				return err
			}
			g.constant(t.Size(), intScalar)
			g.op(0xfc, 10, 0, 0) // memory.copy
			continue
		}
		s, ok := scalarOf(t)
		if !ok {
			return unsupported(e, "initializer of type "+typeName(t))
		}
		g.slotAddress(at)
		from, err := g.rvalue(e)
		if err != nil {
			return err
		}
		g.convert(from, s)
		op, align := s.store()
		g.memarg(op, align)
	}
	return nil
}

// spilledParam is an address-taken parameter copied to the frame.
type spilledParam struct {
	param uint32
	slot  int64
	s     scalar
}
