package wasmgen

import (
	"math/big"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// long double _Complex: two binary128 parts, real then imaginary, 32 bytes
// aligned on 16 (BasicCABI.md: a struct of two long doubles), passed through
// a pointer and returned through sret like the other complex types (cx.go).
// The rules are cx.go's for floating parts; each part being 16 bytes in
// memory, as a long double is (quad.go), the parts are handled by address,
// and every operation is a compiler-rt routine, as clang emits them (probed
// with clang 23.1.4 -O1 -S): __addtf3, __subtf3, __multf3, __divtf3 on
// parts, __multc3 and __divtc3 (sret, a, b, c, d, each part as two i64)
// on whole values, __eqtf2 and __netf2 for == != and truth values.

// isLDCx reports whether t is long double _Complex.
func isLDCx(t cc.Type) bool { return t != nil && t.Kind() == cc.ComplexLongDouble }

var (
	quadCx    = []scalar{u32, u64, u64, u64, u64, u64, u64, u64, u64} // dst, a, b, c, d
	quadZeroL = [2]uint64{0, 0}
)

// ldParts holds the addresses of the two parts of a value, in locals.
type ldParts struct{ re, im uint32 }

// ldcxParts evaluates e, of any arithmetic type, as a long double complex
// number. real reports that e was not complex.
func (g *fgen) ldcxParts(e cc.ExpressionNode) (p ldParts, real bool, err error) {
	p = ldParts{re: g.local(wasm.I32), im: g.local(wasm.I32)}
	switch {
	case isLDCx(e.Type()):
		if err := g.aggAddr(e); err != nil {
			return p, false, err
		}
		g.op(opLocalTee)
		g.uleb(uint64(p.re))
		g.constant(quadSize, intScalar)
		g.op(opI32Add)
		g.setLocal(p.im)
		return p, false, nil
	case isCx(e.Type()):
		if err := g.aggAddr(e); err != nil {
			return p, false, err
		}
		a := g.local(wasm.I32)
		g.setLocal(a)
		el := cxElem(e.Type())
		for i, l := range []uint32{p.re, p.im} {
			g.getLocal(a)
			op, align := el.load()
			g.memargOff(op, align, uint64(i)*uint64(el.bits/8))
			g.ldFromStack(el)
			g.setLocal(l)
		}
		return p, false, nil
	}
	if err := g.ldAddr(e); err != nil {
		return p, true, err
	}
	g.setLocal(p.re)
	slot := g.ldTemp()
	g.ldStoreConst(slot, quadZeroL[0], quadZeroL[1])
	g.slotAddress(slot)
	g.setLocal(p.im)
	return p, true, nil
}

// ldcxStore copies the parts into a new frame temporary and pushes its
// address.
func (g *fgen) ldcxStore(re, im uint32) {
	slot := g.frameSlot(2*quadSize, quadSize)
	dst := g.local(wasm.I32)
	g.slotAddress(slot)
	g.setLocal(dst)
	g.memcopy16(dst, re)
	g.getLocal(dst)
	g.constant(quadSize, intScalar)
	g.op(opI32Add)
	d2 := g.local(wasm.I32)
	g.setLocal(d2)
	g.memcopy16(d2, im)
	g.getLocal(dst)
}

// ldPartOp computes lib(x, y) for parts at the addresses in x and y into a
// new temporary, whose address it leaves in a new local.
func (g *fgen) ldPartOp(lib string, x, y uint32) uint32 {
	slot := g.ldTemp()
	g.slotAddress(slot)
	g.ldHalves(x)
	g.ldHalves(y)
	g.libcall(lib, quadArith, nil)
	r := g.local(wasm.I32)
	g.slotAddress(slot)
	g.setLocal(r)
	return r
}

// ldPartNeg leaves in a new local the address of -x.
func (g *fgen) ldPartNeg(x uint32) uint32 {
	slot := g.ldTemp()
	g.slotAddress(slot)
	g.getLocal(x)
	g.memargOff(opI64Load, 3, 0)
	g.memargOff(opI64Store, 3, 0)
	g.slotAddress(slot)
	g.getLocal(x)
	g.memargOff(opI64Load, 3, 8)
	g.op(opI64Const)
	g.sleb(-1 << 63)
	g.op(opI64Xor)
	g.memargOff(opI64Store, 3, 8)
	r := g.local(wasm.I32)
	g.slotAddress(slot)
	g.setLocal(r)
	return r
}

// ldcxComputed is cxComputed for long double _Complex.
func (g *fgen) ldcxComputed(e cc.ExpressionNode) (bool, error) {
	switch x := strip(e).(type) {
	case *cc.PostfixExpression:
		if arg, ok := conjArg(x); ok {
			p, _, err := g.ldcxParts(arg)
			if err != nil {
				return true, err
			}
			g.ldcxStore(p.re, g.ldPartNeg(p.im))
			return true, nil
		}
	case *cc.PrimaryExpression:
		if v, ok := x.Value().(*cc.ComplexLongDoubleValue); ok {
			// An imaginary constant 2.0iL.
			p := ldParts{re: g.local(wasm.I32), im: g.local(wasm.I32)}
			for _, part := range []struct {
				v *cc.LongDoubleValue
				l uint32
			}{{v.Re, p.re}, {v.Im, p.im}} {
				lo, hi := uint64(0), uint64(0)
				if part.v != nil {
					// cc/v4 parses in 256 bits; rounded once to binary128.
					f := new(big.Float).SetPrec(113).SetMode(big.ToNearestEven).Set((*big.Float)(part.v))
					var ok bool
					if lo, hi, ok = quadBits(f); !ok {
						return true, unsupported(x, "long double constant out of range")
					}
				}
				slot := g.ldTemp()
				g.ldStoreConst(slot, lo, hi)
				g.slotAddress(slot)
				g.setLocal(part.l)
			}
			g.ldcxStore(p.re, p.im)
			return true, nil
		}
	case *cc.AdditiveExpression:
		switch x.Case {
		case cc.AdditiveExpressionAdd, cc.AdditiveExpressionSub:
			return true, g.ldcxArith(x.AdditiveExpression, x.MultiplicativeExpression, x.Case == cc.AdditiveExpressionSub, opI32Add)
		}
	case *cc.MultiplicativeExpression:
		switch x.Case {
		case cc.MultiplicativeExpressionMul:
			return true, g.ldcxArith(x.MultiplicativeExpression, x.CastExpression, false, opI32Mul)
		case cc.MultiplicativeExpressionDiv:
			return true, g.ldcxArith(x.MultiplicativeExpression, x.CastExpression, false, opI32DivS)
		}
	case *cc.UnaryExpression:
		switch x.Case {
		case cc.UnaryExpressionPlus, cc.UnaryExpressionMinus, cc.UnaryExpressionCpl:
			p, _, err := g.ldcxParts(x.CastExpression)
			if err != nil {
				return true, err
			}
			re, im := p.re, p.im
			if x.Case == cc.UnaryExpressionMinus {
				re = g.ldPartNeg(re)
			}
			if x.Case != cc.UnaryExpressionPlus {
				im = g.ldPartNeg(im)
			}
			g.ldcxStore(re, im)
			return true, nil
		}
	case *cc.CastExpression:
		if x.Case == cc.CastExpressionCast {
			return true, g.aggAddrAs(x.CastExpression, e.Type())
		}
	case *cc.ConditionalExpression:
		if x.Case != cc.ConditionalExpressionCond {
			break
		}
		if x.ExpressionList == nil {
			return true, unsupported(x, "?: without a middle operand")
		}
		if err := g.condition(x.LogicalOrExpression); err != nil {
			return true, err
		}
		g.op(opIf, byte(wasm.I32))
		if err := g.aggAddrAs(x.ExpressionList, e.Type()); err != nil {
			return true, err
		}
		g.op(opElse)
		if err := g.aggAddrAs(x.ConditionalExpression, e.Type()); err != nil {
			return true, err
		}
		g.op(opEnd)
		return true, nil
	case *cc.AssignmentExpression:
		switch x.Case {
		case cc.AssignmentExpressionAssign:
			return true, g.aggAssign(x, true)
		case cc.AssignmentExpressionAdd, cc.AssignmentExpressionSub, cc.AssignmentExpressionMul, cc.AssignmentExpressionDiv:
			return true, g.ldcxCompound(x)
		}
	}
	return false, nil
}

// ldcxArith pushes the address of a op b (op: add, sub when sub, mul, div).
func (g *fgen) ldcxArith(a, b cc.ExpressionNode, sub bool, op byte) error {
	p, aReal, err := g.ldcxParts(a)
	if err != nil {
		return err
	}
	q, bReal, err := g.ldcxParts(b)
	if err != nil {
		return err
	}
	return g.ldcxCombine(p, aReal, q, bReal, sub, op)
}

// ldcxCombine pushes the address of p op q, as cxCombine does.
func (g *fgen) ldcxCombine(p ldParts, aReal bool, q ldParts, bReal, sub bool, op byte) error {
	switch {
	case op == opI32Add:
		lib := "__addtf3"
		if sub {
			lib = "__subtf3"
		}
		re := g.ldPartOp(lib, p.re, q.re)
		var im uint32
		switch {
		case bReal:
			im = p.im
		case aReal && !sub:
			im = q.im
		case aReal:
			im = g.ldPartNeg(q.im)
		default:
			im = g.ldPartOp(lib, p.im, q.im)
		}
		g.ldcxStore(re, im)
	case op == opI32Mul && (aReal || bReal), op == opI32DivS && bReal:
		lib := "__multf3"
		if op == opI32DivS {
			lib = "__divtf3"
		}
		x, y, k := p.re, p.im, q.re
		if aReal {
			x, y, k = q.re, q.im, p.re
		}
		var re, im uint32
		if op == opI32Mul && aReal {
			re, im = g.ldPartOp(lib, k, x), g.ldPartOp(lib, k, y)
		} else {
			re, im = g.ldPartOp(lib, x, k), g.ldPartOp(lib, y, k)
		}
		g.ldcxStore(re, im)
	default:
		lib := "__multc3"
		if op == opI32DivS {
			lib = "__divtc3"
		}
		slot := g.frameSlot(2*quadSize, quadSize)
		g.slotAddress(slot)
		for _, l := range []uint32{p.re, p.im, q.re, q.im} {
			g.ldHalves(l)
		}
		g.libcall(lib, quadCx, nil)
		g.slotAddress(slot)
	}
	return nil
}

// ldcxCompound compiles E1 op= E2 for a long double _Complex E1, as
// cxCompound does, and pushes E1's address.
func (g *fgen) ldcxCompound(x *cc.AssignmentExpression) error {
	q, bReal, err := g.ldcxParts(x.AssignmentExpression)
	if err != nil {
		return err
	}
	if err := g.addr(x.UnaryExpression); err != nil {
		return err
	}
	dst := g.local(wasm.I32)
	g.setLocal(dst)
	p := ldParts{re: dst, im: g.local(wasm.I32)}
	g.getLocal(dst)
	g.constant(quadSize, intScalar)
	g.op(opI32Add)
	g.setLocal(p.im)
	var op byte
	sub := false
	switch x.Case {
	case cc.AssignmentExpressionAdd:
		op = opI32Add
	case cc.AssignmentExpressionSub:
		op, sub = opI32Add, true
	case cc.AssignmentExpressionMul:
		op = opI32Mul
	default:
		op = opI32DivS
	}
	if err := g.ldcxCombine(p, false, q, bReal, sub, op); err != nil {
		return err
	}
	src := g.local(wasm.I32)
	g.setLocal(src)
	g.getLocal(dst)
	g.getLocal(src)
	g.constant(2*quadSize, intScalar)
	g.op(0xfc, 10, 0, 0) // memory.copy
	g.getLocal(dst)
	return nil
}

// ldcxTruth pushes an i32 that is non-zero when either part of e is.
func (g *fgen) ldcxTruth(e cc.ExpressionNode) error {
	p, _, err := g.ldcxParts(e)
	if err != nil {
		return err
	}
	g.ldIsNonZero(p.re)
	g.ldIsNonZero(p.im)
	g.op(opI32Or)
	return nil
}

// ldcxEqual pushes a == b (eq) or a != b, at least one a long double
// _Complex: __eqtf2 of each pair of parts is 0 when they are equal.
func (g *fgen) ldcxEqual(a, b cc.ExpressionNode, eq bool) error {
	p, _, err := g.ldcxParts(a)
	if err != nil {
		return err
	}
	q, _, err := g.ldcxParts(b)
	if err != nil {
		return err
	}
	for _, pair := range [][2]uint32{{p.re, q.re}, {p.im, q.im}} {
		g.ldHalves(pair[0])
		g.ldHalves(pair[1])
		g.libcall("__eqtf2", quadCmp, &intScalar)
		g.op(opI32Eqz)
	}
	g.op(opI32And)
	if !eq {
		g.op(opI32Eqz)
	}
	return nil
}
