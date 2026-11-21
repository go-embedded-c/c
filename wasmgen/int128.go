package wasmgen

import (
	"encoding/binary"
	"math/big"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// __int128 and unsigned __int128 (M2). Like long double (quad.go), a 128-bit
// integer is not a wasm scalar: scalarOf keeps refusing it, so that a path
// that does not know it refuses instead of treating an address as a number.
// The value lives in memory, 16 bytes aligned on 16, low half first, and an
// expression of that type evaluates, through iAddr, to the address of its 16
// bytes: the object itself for an lvalue, a frame temporary otherwise.
//
// The ABI and the choice between inline code and a compiler-rt call are what
// clang 23.1.4 emits for wasm32-wasip1 at -O0 (probed with -S):
//
//   - a parameter is two i64, low half then high half; a result goes through
//     an sret pointer, the first parameter (the passing of long double,
//     passQuad);
//   - inline, as i64 pairs: + - with the carry or borrow, unary -, ~, & | ^,
//     == != < <= > >=, the truth value (lo | hi) != 0, a shift by a constant,
//     the conversions to and from the narrower integer types;
//   - calls: * __multi3, / __divti3 (__udivti3), % __modti3 (__umodti3), a
//     shift by a variable __ashlti3, __lshrti3, __ashrti3 (sret, lo, hi,
//     i32 count), floating conversions __floattidf, __floattisf,
//     __floatuntidf, __floatuntisf, __fixdfti, __fixsfti, __fixunsdfti,
//     __fixunssfti, and with long double __floattitf, __floatuntitf,
//     __fixtfti, __fixunstfti.

// isI128 reports whether t is __int128 or unsigned __int128.
func isI128(t cc.Type) bool {
	return t != nil && (t.Kind() == cc.Int128 || t.Kind() == cc.UInt128)
}

func i128Signed(t cc.Type) bool { return t.Kind() == cc.Int128 }

const (
	opI64Eq  = 0x51
	opI64LtS = 0x53
)

var quadShift = []scalar{u32, u64, u64, u32} // dst, a (lo, hi), count

func pickS(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// quadAddr pushes the address of 16 bytes holding e converted to t, a long
// double or an __int128: the two types that live in memory and pass as two
// i64.
func (g *fgen) quadAddr(e cc.ExpressionNode, t cc.Type) error {
	if isI128(t) {
		return g.iAddr(e, t)
	}
	return g.ldAddr(e)
}

// quadLocal is quadAddr into a new local.
func (g *fgen) quadLocal(e cc.ExpressionNode, t cc.Type) (uint32, error) {
	if err := g.quadAddr(e, t); err != nil {
		return 0, err
	}
	a := g.local(wasm.I32)
	g.setLocal(a)
	return a, nil
}

func (g *fgen) iLocal(e cc.ExpressionNode, to cc.Type) (uint32, error) {
	return g.quadLocal(e, to)
}

func (g *fgen) iLo(a uint32) {
	g.getLocal(a)
	g.memargOff(opI64Load, 3, 0)
}

func (g *fgen) iHi(a uint32) {
	g.getLocal(a)
	g.memargOff(opI64Load, 3, 8)
}

func (g *fgen) teeLocal(i uint32) {
	g.op(opLocalTee)
	g.uleb(uint64(i))
}

func (g *fgen) i64Const(v int64) {
	g.op(opI64Const)
	g.sleb(v)
}

// iAddr pushes the address of 16 bytes holding e converted to the __int128
// type to: the object of an lvalue, a frame temporary otherwise. e may have
// any arithmetic type.
func (g *fgen) iAddr(e cc.ExpressionNode, to cc.Type) error {
	if err := g.enter(e); err != nil {
		return err
	}
	defer func() { g.depth-- }()
	e = strip(e)
	if !isI128(e.Type()) {
		return g.iFromOther(e, to)
	}
	if lo, hi, ok := i128Const(e); ok {
		tmp := g.ldTemp()
		g.ldStoreConst(tmp, lo, hi)
		g.slotAddress(tmp)
		return nil
	}
	t := e.Type()
	switch x := e.(type) {
	case *cc.PrimaryExpression:
		if x.Case == cc.PrimaryExpressionIdent {
			return g.addr(x)
		}
	case *cc.CastExpression:
		if x.Case == cc.CastExpressionCast {
			return g.iAddr(x.CastExpression, t)
		}
	case *cc.UnaryExpression:
		switch x.Case {
		case cc.UnaryExpressionPlus:
			return g.iAddr(x.CastExpression, t)
		case cc.UnaryExpressionMinus:
			return g.iNeg(x.CastExpression, t)
		case cc.UnaryExpressionCpl:
			return g.iCpl(x.CastExpression, t)
		case cc.UnaryExpressionDeref:
			return g.addr(x)
		case cc.UnaryExpressionInc, cc.UnaryExpressionDec:
			return g.iIncDec(x.UnaryExpression, x.Case == cc.UnaryExpressionInc, false, true)
		}
	case *cc.AdditiveExpression:
		op := iAdd
		if x.Case == cc.AdditiveExpressionSub {
			op = iSub
		}
		return g.iBinary(op, x.AdditiveExpression, x.MultiplicativeExpression, t)
	case *cc.MultiplicativeExpression:
		op := iMul
		switch x.Case {
		case cc.MultiplicativeExpressionDiv:
			op = iDiv
		case cc.MultiplicativeExpressionMod:
			op = iMod
		}
		return g.iBinary(op, x.MultiplicativeExpression, x.CastExpression, t)
	case *cc.AndExpression:
		return g.iBinary(iAnd, x.AndExpression, x.EqualityExpression, t)
	case *cc.ExclusiveOrExpression:
		return g.iBinary(iXor, x.ExclusiveOrExpression, x.AndExpression, t)
	case *cc.InclusiveOrExpression:
		return g.iBinary(iOr, x.InclusiveOrExpression, x.ExclusiveOrExpression, t)
	case *cc.ShiftExpression:
		return g.iShiftExpr(x.ShiftExpression, x.AdditiveExpression, x.Case == cc.ShiftExpressionLsh, t)
	// cc/v4 elides chain productions: an AssignmentExpression is always an
	// assignment, a ConditionalExpression always a ?: (and the || and &&
	// of an __int128 have type int, never reaching here).
	case *cc.AssignmentExpression:
		return g.iAssign(x, true)
	case *cc.ConditionalExpression:
		if x.ExpressionList == nil {
			return unsupported(x, "?: without a middle operand")
		}
		if err := g.condition(x.LogicalOrExpression); err != nil {
			return err
		}
		g.op(opIf, byte(wasm.I32))
		if err := g.iAddr(x.ExpressionList, t); err != nil {
			return err
		}
		g.op(opElse)
		if err := g.iAddr(x.ConditionalExpression, t); err != nil {
			return err
		}
		g.op(opEnd)
		return nil
	case *cc.ExpressionList:
		for l := x; l != nil; l = l.ExpressionList {
			if l.ExpressionList == nil {
				return g.iAddr(l.AssignmentExpression, t)
			}
			if err := g.effect(l.AssignmentExpression); err != nil {
				return err
			}
		}
	case *cc.PostfixExpression:
		switch x.Case {
		case cc.PostfixExpressionCall:
			var slot int64
			if err := g.callAgg(x, &slot); err != nil {
				return err
			}
			g.slotAddress(slot)
			return nil
		case cc.PostfixExpressionInc, cc.PostfixExpressionDec:
			return g.iIncDec(x.PostfixExpression, x.Case == cc.PostfixExpressionInc, true, true)
		case cc.PostfixExpressionIndex, cc.PostfixExpressionSelect, cc.PostfixExpressionPSelect, cc.PostfixExpressionComplit:
			return g.addr(x)
		}
	}
	return unsupported(e, "__int128 expression")
}

// iFromOther pushes the address of the __int128 of type to equal to e, an
// expression of another arithmetic type.
func (g *fgen) iFromOther(e cc.ExpressionNode, to cc.Type) error {
	signed := i128Signed(to)
	if isLD(e.Type()) {
		a, err := g.ldLocal(e)
		if err != nil {
			return err
		}
		tmp := g.ldTemp()
		g.slotAddress(tmp)
		g.ldHalves(a)
		g.libcall(pickS(signed, "__fixtfti", "__fixunstfti"), quadArith[:3], nil)
		g.slotAddress(tmp)
		return nil
	}
	if _, ok := scalarOf(e.Type()); !ok || isAggregate(e.Type()) {
		return unsupported(e, "conversion of "+typeName(e.Type())+" to __int128")
	}
	from, err := g.rvalue(e)
	if err != nil {
		return err
	}
	g.iFromStack(from, signed)
	return nil
}

// iFromStack converts the scalar on the stack to an __int128 and pushes its
// address; signed is the signedness of the target, which only a floating
// source cares about (an integer is extended by its own).
func (g *fgen) iFromStack(from scalar, signed bool) {
	tmp := g.ldTemp()
	if from.isFloat {
		v := g.local(from.vt)
		g.setLocal(v)
		g.slotAddress(tmp)
		g.getLocal(v)
		name := pickS(from.vt == wasm.F64, pickS(signed, "__fixdfti", "__fixunsdfti"), pickS(signed, "__fixsfti", "__fixunssfti"))
		g.libcall(name, []scalar{u32, from}, nil)
		g.slotAddress(tmp)
		return
	}
	// An integer, bool or pointer: the low half is the value extended to 64
	// bits, the high half its sign (or 0).
	if from.vt == wasm.I32 {
		g.op(pick(from.signed, opI64ExtendI32S, opI64ExtendI32U))
	}
	v := g.local(wasm.I64)
	g.setLocal(v)
	g.slotAddress(tmp)
	g.getLocal(v)
	g.memargOff(opI64Store, 3, 0)
	g.slotAddress(tmp)
	if from.signed {
		g.getLocal(v)
		g.i64Const(63)
		g.op(opI64ShrS)
	} else {
		g.i64Const(0)
	}
	g.memargOff(opI64Store, 3, 8)
	g.slotAddress(tmp)
}

// iToLD pushes the address of the long double equal to the __int128 e.
func (g *fgen) iToLD(e cc.ExpressionNode) error {
	a, err := g.iLocal(e, e.Type())
	if err != nil {
		return err
	}
	tmp := g.ldTemp()
	g.slotAddress(tmp)
	g.ldHalves(a)
	g.libcall(pickS(i128Signed(e.Type()), "__floattitf", "__floatuntitf"), quadArith[:3], nil)
	g.slotAddress(tmp)
	return nil
}

// iToScalar pushes the __int128 e (of its own type) converted to the scalar
// to.
func (g *fgen) iToScalar(e cc.ExpressionNode, to scalar) error {
	a, err := g.iLocal(e, e.Type())
	if err != nil {
		return err
	}
	g.iToScalarAt(a, to, i128Signed(e.Type()))
	return nil
}

// iToScalarAt converts the __int128 at the address in local a; signed is its
// signedness.
func (g *fgen) iToScalarAt(a uint32, to scalar, signed bool) {
	switch {
	case to.isBool:
		g.iNonZero(a)
	case to.isFloat:
		g.ldHalves(a)
		if to.vt == wasm.F64 {
			g.libcall(pickS(signed, "__floattidf", "__floatuntidf"), quadOne, &f64Scalar)
		} else {
			g.libcall(pickS(signed, "__floattisf", "__floatuntisf"), quadOne, &f32Scalar)
		}
	default:
		g.iLo(a)
		if to.vt == wasm.I32 {
			g.op(opI32WrapI64)
			g.normalize(to)
		}
	}
}

// iNonZero pushes an i32 that is non-zero when the __int128 at the address
// in local a is: (lo | hi) != 0.
func (g *fgen) iNonZero(a uint32) {
	g.iLo(a)
	g.iHi(a)
	g.op(opI64Or, opI64Eqz, opI32Eqz)
}

// iCondition pushes an i32 that is non-zero when e is true.
func (g *fgen) iCondition(e cc.ExpressionNode) error {
	a, err := g.iLocal(e, e.Type())
	if err != nil {
		return err
	}
	g.iNonZero(a)
	return nil
}

// iop is an operation of two __int128 operands.
type iop int

const (
	iAdd iop = iota
	iSub
	iAnd
	iOr
	iXor
	iMul
	iDiv
	iMod
)

// iBinary pushes the address of a op b computed in t.
func (g *fgen) iBinary(op iop, a, b cc.ExpressionNode, t cc.Type) error {
	la, err := g.iLocal(a, t)
	if err != nil {
		return err
	}
	lb, err := g.iLocal(b, t)
	if err != nil {
		return err
	}
	g.iOp(op, la, lb, i128Signed(t))
	return nil
}

// iOp pushes the address of a temporary holding (la) op (lb), the addresses
// being in locals.
func (g *fgen) iOp(op iop, la, lb uint32, signed bool) {
	tmp := g.ldTemp()
	switch op {
	case iMul, iDiv, iMod:
		name := "__multi3"
		switch op {
		case iDiv:
			name = pickS(signed, "__divti3", "__udivti3")
		case iMod:
			name = pickS(signed, "__modti3", "__umodti3")
		}
		g.slotAddress(tmp)
		g.ldHalves(la)
		g.ldHalves(lb)
		g.libcall(name, quadArith, nil)
	case iAdd, iSub:
		// lo = alo ± blo; hi = ahi ± bhi ± carry, the carry being lo <u alo
		// for +, alo <u blo for -.
		add := op == iAdd
		l := g.local(wasm.I64)
		g.slotAddress(tmp)
		g.iLo(la)
		g.iLo(lb)
		g.op(pick(add, opI64Add, opI64Sub))
		g.teeLocal(l)
		g.memargOff(opI64Store, 3, 0)
		g.slotAddress(tmp)
		g.iHi(la)
		g.iHi(lb)
		g.op(pick(add, opI64Add, opI64Sub))
		if add {
			g.getLocal(l)
			g.iLo(la)
		} else {
			g.iLo(la)
			g.iLo(lb)
		}
		g.op(opI64LtU, opI64ExtendI32U, pick(add, opI64Add, opI64Sub))
		g.memargOff(opI64Store, 3, 8)
	default:
		o := map[iop]byte{iAnd: opI64And, iOr: opI64Or, iXor: opI64Xor}[op]
		for half := range 2 {
			g.slotAddress(tmp)
			g.getLocal(la)
			g.memargOff(opI64Load, 3, uint64(8*half))
			g.getLocal(lb)
			g.memargOff(opI64Load, 3, uint64(8*half))
			g.op(o)
			g.memargOff(opI64Store, 3, uint64(8*half))
		}
	}
	g.slotAddress(tmp)
}

// iNeg pushes the address of -a: 0 - a.
func (g *fgen) iNeg(a cc.ExpressionNode, t cc.Type) error {
	la, err := g.iLocal(a, t)
	if err != nil {
		return err
	}
	tmp := g.ldTemp()
	g.slotAddress(tmp)
	g.i64Const(0)
	g.iLo(la)
	g.op(opI64Sub)
	g.memargOff(opI64Store, 3, 0)
	g.slotAddress(tmp)
	g.i64Const(0)
	g.iHi(la)
	g.op(opI64Sub)
	g.iLo(la)
	g.i64Const(0)
	g.op(opI64Ne, opI64ExtendI32U, opI64Sub)
	g.memargOff(opI64Store, 3, 8)
	g.slotAddress(tmp)
	return nil
}

// iCpl pushes the address of ~a.
func (g *fgen) iCpl(a cc.ExpressionNode, t cc.Type) error {
	la, err := g.iLocal(a, t)
	if err != nil {
		return err
	}
	tmp := g.ldTemp()
	for half := range 2 {
		g.slotAddress(tmp)
		g.getLocal(la)
		g.memargOff(opI64Load, 3, uint64(8*half))
		g.i64Const(-1)
		g.op(opI64Xor)
		g.memargOff(opI64Store, 3, uint64(8*half))
	}
	g.slotAddress(tmp)
	return nil
}

// A shift count: a constant in [0, 128) is compiled inline, anything else is
// an i32 in a local for the library routine.
type shiftCount struct {
	n     int
	isN   bool
	local uint32
}

// iCount evaluates the count operand b of a shift.
func (g *fgen) iCount(b cc.ExpressionNode) (shiftCount, error) {
	if !isI128(b.Type()) {
		if v, ok := intValue(b); ok && v >= 0 && v < 128 {
			return shiftCount{n: int(v), isN: true}, nil
		}
	}
	l := g.local(wasm.I32)
	if err := g.operand(b, u32); err != nil {
		return shiftCount{}, err
	}
	g.setLocal(l)
	return shiftCount{local: l}, nil
}

// iShiftExpr pushes the address of a << b or a >> b, in t.
func (g *fgen) iShiftExpr(a, b cc.ExpressionNode, left bool, t cc.Type) error {
	la, err := g.iLocal(a, t)
	if err != nil {
		return err
	}
	c, err := g.iCount(b)
	if err != nil {
		return err
	}
	g.iShift(la, c, left, i128Signed(t))
	return nil
}

// iShift pushes the address of the shift of the value at the address in la.
func (g *fgen) iShift(la uint32, c shiftCount, left, signed bool) {
	tmp := g.ldTemp()
	if !c.isN {
		name := "__ashlti3"
		if !left {
			name = pickS(signed, "__ashrti3", "__lshrti3")
		}
		g.slotAddress(tmp)
		g.ldHalves(la)
		g.getLocal(c.local)
		g.libcall(name, quadShift, nil)
		g.slotAddress(tmp)
		return
	}
	n := int64(c.n)
	shr := pick(signed, opI64ShrS, opI64ShrU)
	switch {
	case n == 0:
		for half := range 2 {
			g.slotAddress(tmp)
			g.getLocal(la)
			g.memargOff(opI64Load, 3, uint64(8*half))
			g.memargOff(opI64Store, 3, uint64(8*half))
		}
	case left && n < 64:
		g.slotAddress(tmp)
		g.iLo(la)
		g.i64Const(n)
		g.op(opI64Shl)
		g.memargOff(opI64Store, 3, 0)
		g.slotAddress(tmp)
		g.iHi(la)
		g.i64Const(n)
		g.op(opI64Shl)
		g.iLo(la)
		g.i64Const(64 - n)
		g.op(opI64ShrU, opI64Or)
		g.memargOff(opI64Store, 3, 8)
	case left:
		g.slotAddress(tmp)
		g.i64Const(0)
		g.memargOff(opI64Store, 3, 0)
		g.slotAddress(tmp)
		g.iLo(la)
		if n > 64 {
			g.i64Const(n - 64)
			g.op(opI64Shl)
		}
		g.memargOff(opI64Store, 3, 8)
	case n < 64:
		g.slotAddress(tmp)
		g.iLo(la)
		g.i64Const(n)
		g.op(opI64ShrU)
		g.iHi(la)
		g.i64Const(64 - n)
		g.op(opI64Shl, opI64Or)
		g.memargOff(opI64Store, 3, 0)
		g.slotAddress(tmp)
		g.iHi(la)
		g.i64Const(n)
		g.op(shr)
		g.memargOff(opI64Store, 3, 8)
	default:
		g.slotAddress(tmp)
		g.iHi(la)
		if n > 64 {
			g.i64Const(n - 64)
			g.op(shr)
		}
		g.memargOff(opI64Store, 3, 0)
		g.slotAddress(tmp)
		if signed {
			g.iHi(la)
			g.i64Const(63)
			g.op(opI64ShrS)
		} else {
			g.i64Const(0)
		}
		g.memargOff(opI64Store, 3, 8)
	}
	g.slotAddress(tmp)
}

// iLess pushes a < b as an i32: the high halves decide, the low halves
// (unsigned) break a tie.
func (g *fgen) iLess(a, b uint32, signed bool) {
	g.iHi(a)
	g.iHi(b)
	g.op(pick(signed, opI64LtS, opI64LtU))
	g.iHi(a)
	g.iHi(b)
	g.op(opI64Eq)
	g.iLo(a)
	g.iLo(b)
	g.op(opI64LtU, opI32And, opI32Or)
}

// iCompare pushes the int result of comparing a and b in ct, an __int128
// type; op is the i32 comparison opcode of the operator, the signedness
// being ct's.
func (g *fgen) iCompare(a, b cc.ExpressionNode, ct cc.Type, op byte) error {
	la, err := g.iLocal(a, ct)
	if err != nil {
		return err
	}
	lb, err := g.iLocal(b, ct)
	if err != nil {
		return err
	}
	signed := i128Signed(ct)
	switch op {
	case opI32Eq, opI32Ne:
		g.iLo(la)
		g.iLo(lb)
		g.op(opI64Xor)
		g.iHi(la)
		g.iHi(lb)
		g.op(opI64Xor, opI64Or, opI64Eqz)
		if op == opI32Ne {
			g.op(opI32Eqz)
		}
	case opI32LtS, opI32LtU:
		g.iLess(la, lb, signed)
	case opI32GtS, opI32GtU:
		g.iLess(lb, la, signed)
	case opI32LeS, opI32LeU:
		g.iLess(lb, la, signed)
		g.op(opI32Eqz)
	default:
		g.iLess(la, lb, signed)
		g.op(opI32Eqz)
	}
	return nil
}

// iIncDec compiles ++E, --E, E++ and E-- for an __int128 E; with want it
// pushes the address of the value (the old one, in a copy, for the postfix
// forms).
func (g *fgen) iIncDec(e cc.ExpressionNode, inc, post, want bool) error {
	if err := g.addr(e); err != nil {
		return err
	}
	d := g.local(wasm.I32)
	g.setLocal(d)
	var old int64
	if post && want {
		old = g.ldTemp()
		oldL := g.local(wasm.I32)
		g.slotAddress(old)
		g.setLocal(oldL)
		g.memcopy16(oldL, d)
	}
	tmp := g.ldTemp()
	// lo ± 1; the high half moves when the low half became 0 (+) or was 0 (-).
	l := g.local(wasm.I64)
	g.slotAddress(tmp)
	g.iLo(d)
	g.i64Const(1)
	g.op(pick(inc, opI64Add, opI64Sub))
	g.teeLocal(l)
	g.memargOff(opI64Store, 3, 0)
	g.slotAddress(tmp)
	g.iHi(d)
	if inc {
		g.getLocal(l)
	} else {
		g.iLo(d)
	}
	g.op(opI64Eqz, opI64ExtendI32U, pick(inc, opI64Add, opI64Sub))
	g.memargOff(opI64Store, 3, 8)
	g.getLocal(d)
	g.slotAddress(tmp)
	g.constant(quadSize, intScalar)
	g.op(0xfc, 10, 0, 0) // memory.copy
	switch {
	case post && want:
		g.slotAddress(old)
	case want:
		g.getLocal(d)
	}
	return nil
}

// iAssign compiles E1 = E2 and E1 op= E2 for an __int128 E1: E2 first, as for
// scalars, then E1's address; the result of an operation goes through a
// temporary. With want it leaves E1's address.
func (g *fgen) iAssign(x *cc.AssignmentExpression, want bool) error {
	lt := x.UnaryExpression.Type()
	var op iop
	shift, left := false, false
	switch x.Case {
	case cc.AssignmentExpressionAssign:
	case cc.AssignmentExpressionAdd:
		op = iAdd
	case cc.AssignmentExpressionSub:
		op = iSub
	case cc.AssignmentExpressionMul:
		op = iMul
	case cc.AssignmentExpressionDiv:
		op = iDiv
	case cc.AssignmentExpressionMod:
		op = iMod
	case cc.AssignmentExpressionAnd:
		op = iAnd
	case cc.AssignmentExpressionOr:
		op = iOr
	case cc.AssignmentExpressionXor:
		op = iXor
	case cc.AssignmentExpressionLsh:
		shift, left = true, true
	case cc.AssignmentExpressionRsh:
		shift = true
	default:
		return unsupported(x, "assignment "+x.Case.String()+" of an __int128")
	}
	rt := x.AssignmentExpression.Type()
	ct := lt // the type the operation is in
	if x.Case != cc.AssignmentExpressionAssign && !shift {
		ct = cc.UsualArithmeticConversions(lt, rt)
		if !isI128(ct) {
			return g.iFloatAssign(x, ct, want)
		}
	}
	var b uint32
	var c shiftCount
	var err error
	if shift {
		c, err = g.iCount(x.AssignmentExpression)
	} else {
		b, err = g.iLocal(x.AssignmentExpression, ct)
	}
	if err != nil {
		return err
	}
	if err := g.addr(x.UnaryExpression); err != nil {
		return err
	}
	d := g.local(wasm.I32)
	g.setLocal(d)
	if x.Case == cc.AssignmentExpressionAssign {
		g.memcopy16(d, b)
	} else {
		if shift {
			g.iShift(d, c, left, i128Signed(lt))
		} else {
			g.iOp(op, d, b, i128Signed(ct))
		}
		r := g.local(wasm.I32)
		g.setLocal(r)
		g.memcopy16(d, r)
	}
	if want {
		g.getLocal(d)
	}
	return nil
}

// iFloatAssign compiles E1 op= E2 for an __int128 E1 and a floating E2: the
// operation is in the floating type ct (C11 6.5.16.2p3, 6.3.1.8), the result
// converted back to the __int128.
func (g *fgen) iFloatAssign(x *cc.AssignmentExpression, ct cc.Type, want bool) error {
	cs, ok := scalarOf(ct)
	ops, okOp := binaryOps[x.Case]
	if !ok || !cs.isFloat || !okOp {
		return unsupported(x, "compound assignment of an __int128 by "+typeName(x.AssignmentExpression.Type()))
	}
	signed := i128Signed(x.UnaryExpression.Type())
	if err := g.operand(x.AssignmentExpression, cs); err != nil {
		return err
	}
	r := g.local(cs.vt)
	g.setLocal(r)
	if err := g.addr(x.UnaryExpression); err != nil {
		return err
	}
	d := g.local(wasm.I32)
	g.setLocal(d)
	g.iToScalarAt(d, cs, signed)
	g.op(opLocalGet)
	g.uleb(uint64(r))
	g.op(opFor(pick(cs.signed, ops[0], ops[1]), cs))
	g.iFromStack(cs, signed)
	res := g.local(wasm.I32)
	g.setLocal(res)
	g.memcopy16(d, res)
	if want {
		g.getLocal(d)
	}
	return nil
}

// iScalarAssign compiles E1 op= E2 for a scalar E1 and an __int128 E2, the
// operation being in the common type ct (an __int128 type), the result
// converted back to E1's type.
func (g *fgen) iScalarAssign(x *cc.AssignmentExpression, t *target, ct cc.Type, want bool) error {
	var op iop
	switch x.Case {
	case cc.AssignmentExpressionAdd:
		op = iAdd
	case cc.AssignmentExpressionSub:
		op = iSub
	case cc.AssignmentExpressionMul:
		op = iMul
	case cc.AssignmentExpressionDiv:
		op = iDiv
	case cc.AssignmentExpressionMod:
		op = iMod
	case cc.AssignmentExpressionAnd:
		op = iAnd
	case cc.AssignmentExpressionOr:
		op = iOr
	case cc.AssignmentExpressionXor:
		op = iXor
	default:
		return unsupported(x, "compound assignment of a scalar by an __int128")
	}
	// assign refused a pointer target, and a floating one makes ct floating:
	// t is an integer, a bit-field included (address, loadTarget and store
	// handle one).
	b, err := g.iLocal(x.AssignmentExpression, ct)
	if err != nil {
		return err
	}
	if err := g.address(t); err != nil {
		return err
	}
	g.loadTarget(t)
	g.iFromStack(t.s, i128Signed(ct))
	l := g.local(wasm.I32)
	g.setLocal(l)
	g.iOp(op, l, b, i128Signed(ct))
	r := g.local(wasm.I32)
	g.setLocal(r)
	g.iToScalarAt(r, t.s, i128Signed(ct))
	g.store(t, want)
	return nil
}

// Constants. cc/v4 has no value wider than 64 bits and gives an expression of
// an __int128 type no value at all, so the constants are computed here, in
// math/big, and wrapped to the type at every step.

// i128Wrap returns v converted to the integer type t (C11 6.3.1.3: modulo
// 2^N, the signed case wrapping as clang does).
func i128Wrap(v *big.Int, t cc.Type) *big.Int {
	if t.Kind() == cc.Bool {
		if v.Sign() != 0 {
			return big.NewInt(1)
		}
		return big.NewInt(0)
	}
	bits := uint(t.Size() * 8)
	m := new(big.Int).Lsh(big.NewInt(1), bits)
	r := new(big.Int).Mod(v, m) // in [0, m)
	if cc.IsSignedInteger(t) && r.Bit(int(bits)-1) == 1 {
		r.Sub(r, m)
	}
	return r
}

// i128Eval returns the value of the integer constant expression e, in the
// range of its own type.
func i128Eval(e cc.ExpressionNode) (*big.Int, bool) {
	e = strip(e)
	t := e.Type()
	if t == nil || !cc.IsIntegerType(t) || t.Size() > 16 {
		return nil, false
	}
	switch v := e.Value().(type) {
	case cc.Int64Value:
		return big.NewInt(int64(v)), true
	case cc.UInt64Value:
		return new(big.Int).SetUint64(uint64(v)), true
	}
	bin := func(a, b cc.ExpressionNode) (x, y *big.Int, ok bool) {
		x, ok1 := i128Eval(a)
		y, ok2 := i128Eval(b)
		return x, y, ok1 && ok2
	}
	var r *big.Int
	switch x := e.(type) {
	case *cc.CastExpression: // a cast: strip took the other case
		v, ok := i128Eval(x.CastExpression)
		if !ok {
			return nil, false
		}
		r = v
	case *cc.UnaryExpression:
		switch x.Case {
		case cc.UnaryExpressionPlus, cc.UnaryExpressionMinus, cc.UnaryExpressionCpl:
		default:
			return nil, false
		}
		v, ok := i128Eval(x.CastExpression)
		if !ok {
			return nil, false
		}
		switch x.Case {
		case cc.UnaryExpressionPlus:
			r = v
		case cc.UnaryExpressionMinus:
			r = new(big.Int).Neg(v)
		default:
			r = new(big.Int).Not(i128Wrap(v, t))
		}
	case *cc.AdditiveExpression:
		a, b, ok := bin(x.AdditiveExpression, x.MultiplicativeExpression)
		if !ok {
			return nil, false
		}
		if x.Case == cc.AdditiveExpressionSub {
			r = new(big.Int).Sub(a, b)
		} else {
			r = new(big.Int).Add(a, b)
		}
	case *cc.MultiplicativeExpression:
		a, b, ok := bin(x.MultiplicativeExpression, x.CastExpression)
		if !ok {
			return nil, false
		}
		// Operands are converted to the type of the expression first.
		a, b = i128Wrap(a, t), i128Wrap(b, t)
		switch x.Case {
		case cc.MultiplicativeExpressionMul:
			r = new(big.Int).Mul(a, b)
		default:
			if b.Sign() == 0 {
				return nil, false
			}
			q, m := new(big.Int).QuoRem(a, b, new(big.Int))
			if x.Case == cc.MultiplicativeExpressionDiv {
				r = q
			} else {
				r = m
			}
		}
	case *cc.AndExpression:
		a, b, ok := bin(x.AndExpression, x.EqualityExpression)
		if !ok {
			return nil, false
		}
		r = new(big.Int).And(i128Wrap(a, t), i128Wrap(b, t))
	case *cc.ExclusiveOrExpression:
		a, b, ok := bin(x.ExclusiveOrExpression, x.AndExpression)
		if !ok {
			return nil, false
		}
		r = new(big.Int).Xor(i128Wrap(a, t), i128Wrap(b, t))
	case *cc.InclusiveOrExpression:
		a, b, ok := bin(x.InclusiveOrExpression, x.ExclusiveOrExpression)
		if !ok {
			return nil, false
		}
		r = new(big.Int).Or(i128Wrap(a, t), i128Wrap(b, t))
	case *cc.ShiftExpression:
		a, b, ok := bin(x.ShiftExpression, x.AdditiveExpression)
		if !ok || b.Sign() < 0 || b.Cmp(big.NewInt(int64(t.Size()*8))) >= 0 {
			return nil, false // a count out of range is undefined: not folded
		}
		a = i128Wrap(a, t)
		if x.Case == cc.ShiftExpressionLsh {
			r = new(big.Int).Lsh(a, uint(b.Uint64()))
		} else {
			r = new(big.Int).Rsh(a, uint(b.Uint64()))
		}
	default:
		return nil, false
	}
	return i128Wrap(r, t), true
}

var two128 = new(big.Int).Lsh(big.NewInt(1), 128)

// i128Halves splits v, taken modulo 2^128, into its two halves.
func i128Halves(v *big.Int) (lo, hi uint64) {
	u := new(big.Int).Mod(v, two128)
	mask := new(big.Int).SetUint64(^uint64(0))
	return new(big.Int).And(u, mask).Uint64(), new(big.Int).Rsh(u, 64).Uint64()
}

// i128Const returns the halves of the constant __int128 expression e.
func i128Const(e cc.ExpressionNode) (lo, hi uint64, ok bool) {
	v, ok := i128Eval(e)
	if !ok {
		return 0, 0, false
	}
	lo, hi = i128Halves(v)
	return lo, hi, true
}

// i128Static writes the 16 bytes of the constant e, converted to the
// __int128 type t, into b, its 16 bytes.
func i128Static(e cc.ExpressionNode, t cc.Type, b []byte) bool {
	v, ok := i128Eval(e)
	if !ok {
		return false
	}
	lo, hi := i128Halves(i128Wrap(v, t))
	binary.LittleEndian.PutUint64(b, lo)
	binary.LittleEndian.PutUint64(b[8:], hi)
	return true
}
