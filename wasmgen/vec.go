package wasmgen

import (
	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// GNU vectors, __attribute__((vector_size(N))) (GCC manual, Vector
// Extensions): N bytes of elements of an integer or floating type, aligned
// on N. They are aggregates here (isAggregate), laid out as an array of
// their elements, copied and stored as bytes. BasicCABI.md leaves the ABI of
// vector types unspecified ("not stable"): a vector is passed through a
// pointer, like a structure, and returned through sret, as clang returns it
// (clang 23.1.4 passes one as a scalar per element; no code compiled by
// clang exchanges vectors with ours).
//
// An expression of vector type evaluates, through aggAddr, to the address
// of its elements: the object of an lvalue, a frame temporary for a
// computed value. Each operator applies element by element, in the
// element's type, without the integer promotions (the store narrows):
//
//   - + - * / % & | ^ << >> on two vectors, or a vector and a scalar
//     converted to the element type;
//   - unary + - ~;
//   - == != < > <= >= give a vector of signed integers as wide as the
//     elements, -1 where the relation holds and 0 elsewhere;
//   - a cast between a vector and a vector or scalar of the same size keeps
//     the bytes;
//   - v[i] designates element i.

// isVec reports whether t is a GNU vector type.
func isVec(t cc.Type) bool { return t != nil && t.VectorSize() > 0 }

// vecElem is the representation of an element of the vector type t, the
// element's size and how many elements t has.
func vecElem(t cc.Type) (s scalar, size, n int64, ok bool) {
	elem, n, ok := cc.VectorElem(t)
	if !ok {
		return scalar{}, 0, 0, false
	}
	s, ok = scalarOf(elem)
	if !ok || n <= 0 {
		return scalar{}, 0, 0, false
	}
	return s, elem.Size(), n, true
}

// vecOperand is an operand of an element-wise operation: the address of a
// vector in a local, or a scalar already converted to the element type.
type vecOperand struct {
	vec  bool
	l    uint32
	el   scalar // a vector's element
	size int64  // a vector's element size
}

// vecOperandOf evaluates e, as a vector or as a scalar converted to el.
func (g *fgen) vecOperandOf(e cc.ExpressionNode, el scalar) (vecOperand, error) {
	if isVec(e.Type()) {
		s, size, _, ok := vecElem(e.Type())
		if !ok {
			return vecOperand{}, unsupported(e, "vector of "+typeName(e.Type()))
		}
		if err := g.aggAddr(e); err != nil {
			return vecOperand{}, err
		}
		l := g.local(wasm.I32)
		g.setLocal(l)
		return vecOperand{vec: true, l: l, el: s, size: size}, nil
	}
	if isLD(e.Type()) || isAggregate(e.Type()) {
		return vecOperand{}, unsupported(e, "vector operand of type "+typeName(e.Type()))
	}
	if err := g.operand(e, el); err != nil {
		return vecOperand{}, err
	}
	l := g.local(el.vt)
	g.setLocal(l)
	return vecOperand{l: l}, nil
}

// push pushes element i of o, converted to el.
func (g *fgen) vecPush(o vecOperand, i int64, el scalar) {
	g.getLocal(o.l)
	if !o.vec {
		return
	}
	op, align := o.el.load()
	g.memargOff(op, align, uint64(i*o.size))
	g.convert(o.el, el)
}

// vecResult is where an element-wise operation writes: a frame temporary
// or, for an assignment with an operator, the left operand.
func (g *fgen) vecResult(t cc.Type) uint32 {
	slot := g.frameSlot(t.Size(), t.Align())
	g.slotAddress(slot)
	l := g.local(wasm.I32)
	g.setLocal(l)
	return l
}

// vecOp computes a op b element by element into the vector at dst, of type
// t. For a comparison (rel), signed and unsigned are the relation's i32
// opcodes and the result is -1 or 0 in t's elements; otherwise they are the
// operator's i32 opcodes.
func (g *fgen) vecOp(at cc.Node, t cc.Type, dst uint32, a, b vecOperand, opnd scalar, signed, unsigned byte, rel bool) error {
	res, size, n, ok := vecElem(t)
	if !ok {
		return unsupported(at, "vector of "+typeName(t))
	}
	op := pick(opnd.signed, signed, unsigned)
	for i := int64(0); i < n; i++ {
		g.getLocal(dst)
		g.vecPush(a, i, opnd)
		g.vecPush(b, i, opnd)
		switch {
		case rel && opnd.isFloat:
			g.op(floatRel(op, opnd))
		case rel && opnd.vt == wasm.I64:
			g.op(op + i64CompareDelta)
		default:
			g.op(opFor(op, opnd))
		}
		if rel {
			// 1 where the relation holds becomes -1 (all bits set).
			g.constant(-1, intScalar)
			g.op(opI32Mul)
			g.convert(intScalar, res)
		}
		st, align := res.store()
		g.memargOff(st, align, uint64(i*size))
	}
	return nil
}

// vecArith computes a op b, of vector type t, into a new temporary and
// pushes its address. ops holds the signed and unsigned i32 opcodes; rel
// marks a comparison, whose operands have the type of the vector operand.
func (g *fgen) vecArith(t cc.Type, a, b cc.ExpressionNode, signed, unsigned byte, rel bool) error {
	ot := t
	if rel {
		ot = a.Type()
		if !isVec(ot) {
			ot = b.Type()
		}
	}
	opnd, _, _, ok := vecElem(ot)
	if !ok {
		return unsupported(a, "vector of "+typeName(ot))
	}
	if opnd.isFloat && (signed == opI32RemS || signed == opI32And || signed == opI32Or || signed == opI32Xor || signed == opI32Shl || signed == opI32ShrS) {
		return unsupported(a, "integer operator on a floating vector")
	}
	x, err := g.vecOperandOf(a, opnd)
	if err != nil {
		return err
	}
	y, err := g.vecOperandOf(b, opnd)
	if err != nil {
		return err
	}
	dst := g.vecResult(t)
	if err := g.vecOp(a, t, dst, x, y, opnd, signed, unsigned, rel); err != nil {
		return err
	}
	g.getLocal(dst)
	return nil
}

// vecOpcodes maps a binary operator expression to its signed and unsigned
// i32 opcodes, its operands and whether it is a comparison.
func vecOpcodes(e cc.ExpressionNode) (a, b cc.ExpressionNode, signed, unsigned byte, rel, ok bool) {
	switch x := e.(type) {
	case *cc.AdditiveExpression:
		switch x.Case {
		case cc.AdditiveExpressionAdd:
			return x.AdditiveExpression, x.MultiplicativeExpression, opI32Add, opI32Add, false, true
		case cc.AdditiveExpressionSub:
			return x.AdditiveExpression, x.MultiplicativeExpression, opI32Sub, opI32Sub, false, true
		}
	case *cc.MultiplicativeExpression:
		switch x.Case {
		case cc.MultiplicativeExpressionMul:
			return x.MultiplicativeExpression, x.CastExpression, opI32Mul, opI32Mul, false, true
		case cc.MultiplicativeExpressionDiv:
			return x.MultiplicativeExpression, x.CastExpression, opI32DivS, opI32DivU, false, true
		case cc.MultiplicativeExpressionMod:
			return x.MultiplicativeExpression, x.CastExpression, opI32RemS, opI32RemU, false, true
		}
	case *cc.ShiftExpression:
		switch x.Case {
		case cc.ShiftExpressionLsh:
			return x.ShiftExpression, x.AdditiveExpression, opI32Shl, opI32Shl, false, true
		case cc.ShiftExpressionRsh:
			return x.ShiftExpression, x.AdditiveExpression, opI32ShrS, opI32ShrU, false, true
		}
	case *cc.AndExpression:
		if x.Case == cc.AndExpressionAnd {
			return x.AndExpression, x.EqualityExpression, opI32And, opI32And, false, true
		}
	case *cc.InclusiveOrExpression:
		if x.Case == cc.InclusiveOrExpressionOr {
			return x.InclusiveOrExpression, x.ExclusiveOrExpression, opI32Or, opI32Or, false, true
		}
	case *cc.ExclusiveOrExpression:
		if x.Case == cc.ExclusiveOrExpressionXor {
			return x.ExclusiveOrExpression, x.AndExpression, opI32Xor, opI32Xor, false, true
		}
	case *cc.EqualityExpression:
		switch x.Case {
		case cc.EqualityExpressionEq:
			return x.EqualityExpression, x.RelationalExpression, opI32Eq, opI32Eq, true, true
		case cc.EqualityExpressionNeq:
			return x.EqualityExpression, x.RelationalExpression, opI32Ne, opI32Ne, true, true
		}
	case *cc.RelationalExpression:
		switch x.Case {
		case cc.RelationalExpressionLt:
			return x.RelationalExpression, x.ShiftExpression, opI32LtS, opI32LtU, true, true
		case cc.RelationalExpressionGt:
			return x.RelationalExpression, x.ShiftExpression, opI32GtS, opI32GtU, true, true
		case cc.RelationalExpressionLeq:
			return x.RelationalExpression, x.ShiftExpression, opI32LeS, opI32LeU, true, true
		case cc.RelationalExpressionGeq:
			return x.RelationalExpression, x.ShiftExpression, opI32GeS, opI32GeU, true, true
		}
	}
	return nil, nil, 0, 0, false, false
}

// vecComputed compiles a vector value that is not an object: an operator,
// a cast, an assignment with an operator. It reports false, with nothing
// emitted, for the others, which aggAddr takes as aggregates.
func (g *fgen) vecComputed(e cc.ExpressionNode) (bool, error) {
	t := e.Type()
	e = strip(e)
	if a, b, s, u, rel, ok := vecOpcodes(e); ok {
		return true, g.vecArith(t, a, b, s, u, rel)
	}
	switch x := e.(type) {
	case *cc.UnaryExpression:
		switch x.Case {
		case cc.UnaryExpressionPlus, cc.UnaryExpressionMinus, cc.UnaryExpressionCpl:
			return true, g.vecUnary(t, x)
		}
	case *cc.CastExpression:
		if x.Case == cc.CastExpressionCast {
			return true, g.vecCast(t, x.CastExpression)
		}
	case *cc.AssignmentExpression:
		return g.vecCompound(x)
	}
	return false, nil
}

// vecUnary computes +v, -v or ~v.
func (g *fgen) vecUnary(t cc.Type, x *cc.UnaryExpression) error {
	el, _, _, ok := vecElem(t)
	if !ok {
		return unsupported(x, "vector of "+typeName(t))
	}
	if el.isFloat && x.Case == cc.UnaryExpressionCpl {
		return unsupported(x, "~ of a floating vector")
	}
	v, err := g.vecOperandOf(x.CastExpression, el)
	if err != nil {
		return err
	}
	zero := g.local(el.vt)
	switch {
	case x.Case == cc.UnaryExpressionPlus:
		// v + 0 would turn -0.0 into +0.0: copy instead.
		dst := g.vecResult(t)
		g.getLocal(dst)
		g.getLocal(v.l)
		g.constant(t.Size(), intScalar)
		g.op(0xfc, 10, 0, 0) // memory.copy
		g.getLocal(dst)
		return nil
	case x.Case == cc.UnaryExpressionCpl:
		// ~x is x ^ -1.
		g.constant(-1, el)
		g.setLocal(zero)
		dst := g.vecResult(t)
		if err := g.vecOp(x, t, dst, v, vecOperand{l: zero}, el, opI32Xor, opI32Xor, false); err != nil {
			return err
		}
		g.getLocal(dst)
		return nil
	case el.isFloat:
		// -x flips the sign, of a zero and a NaN too.
		_, size, n, _ := vecElem(t)
		dst := g.vecResult(t)
		for i := int64(0); i < n; i++ {
			g.getLocal(dst)
			g.vecPush(v, i, el)
			g.op(pick(el.vt == wasm.F32, opF32Neg, opF64Neg))
			st, align := el.store()
			g.memargOff(st, align, uint64(i*size))
		}
		g.getLocal(dst)
		return nil
	}
	// -x is 0 - x.
	g.zero(el)
	g.setLocal(zero)
	dst := g.vecResult(t)
	if err := g.vecOp(x, t, dst, vecOperand{l: zero}, v, el, opI32Sub, opI32Sub, false); err != nil {
		return err
	}
	g.getLocal(dst)
	return nil
}

// vecCast converts e to the vector type t, keeping its bytes: a vector of
// the same size, or a scalar of the same size (GCC manual, Vector
// Extensions).
func (g *fgen) vecCast(t cc.Type, e cc.ExpressionNode) error {
	from := e.Type()
	if from.Size() != t.Size() {
		return unsupported(e, "cast of "+typeName(from)+" to a vector of another size")
	}
	if isVec(from) {
		if err := g.aggAddr(e); err != nil {
			return err
		}
		src := g.local(wasm.I32)
		g.setLocal(src)
		dst := g.vecResult(t)
		g.getLocal(dst)
		g.getLocal(src)
		g.constant(t.Size(), intScalar)
		g.op(0xfc, 10, 0, 0) // memory.copy
		g.getLocal(dst)
		return nil
	}
	s, ok := scalarOf(from)
	if !ok || s.ptr {
		return unsupported(e, "cast of "+typeName(from)+" to a vector")
	}
	dst := g.vecResult(t)
	g.getLocal(dst)
	if err := g.operand(e, s); err != nil {
		return err
	}
	st, align := s.store()
	g.memargOff(st, align, 0)
	g.getLocal(dst)
	return nil
}

// vecCompound compiles E1 op= E2 on a vector E1: E2 first, then the
// elements of E1 read, combined and written back. It pushes E1's address.
func (g *fgen) vecCompound(x *cc.AssignmentExpression) (bool, error) {
	var signed, unsigned byte
	switch x.Case {
	case cc.AssignmentExpressionMul:
		signed, unsigned = opI32Mul, opI32Mul
	case cc.AssignmentExpressionDiv:
		signed, unsigned = opI32DivS, opI32DivU
	case cc.AssignmentExpressionMod:
		signed, unsigned = opI32RemS, opI32RemU
	case cc.AssignmentExpressionAdd:
		signed, unsigned = opI32Add, opI32Add
	case cc.AssignmentExpressionSub:
		signed, unsigned = opI32Sub, opI32Sub
	case cc.AssignmentExpressionLsh:
		signed, unsigned = opI32Shl, opI32Shl
	case cc.AssignmentExpressionRsh:
		signed, unsigned = opI32ShrS, opI32ShrU
	case cc.AssignmentExpressionAnd:
		signed, unsigned = opI32And, opI32And
	case cc.AssignmentExpressionXor:
		signed, unsigned = opI32Xor, opI32Xor
	case cc.AssignmentExpressionOr:
		signed, unsigned = opI32Or, opI32Or
	default:
		return false, nil
	}
	t := x.UnaryExpression.Type()
	if !isVec(t) {
		return true, unsupported(x, "assignment of a vector to "+typeName(t))
	}
	el, size, _, ok := vecElem(t)
	if !ok {
		return true, unsupported(x, "vector of "+typeName(t))
	}
	if el.isFloat && signed != opI32Mul && signed != opI32DivS && signed != opI32Add && signed != opI32Sub {
		return true, unsupported(x, "integer operator on a floating vector")
	}
	if err := g.addr(x.UnaryExpression); err != nil {
		return true, err
	}
	dst := g.local(wasm.I32)
	g.setLocal(dst)
	y, err := g.vecOperandOf(x.AssignmentExpression, el)
	if err != nil {
		return true, err
	}
	lhs := vecOperand{vec: true, l: dst, el: el, size: size}
	if err := g.vecOp(x, t, dst, lhs, y, el, signed, unsigned, false); err != nil {
		return true, err
	}
	g.getLocal(dst)
	return true, nil
}
