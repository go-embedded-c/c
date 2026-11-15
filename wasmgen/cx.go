package wasmgen

import (
	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// Complex types (C11 6.2.5p11, Annex G), float _Complex and double _Complex.
// BasicCABI.md represents them as a struct of two fields of the
// corresponding floating type, real then imaginary: they are aggregates
// here (isAggregate), passed through a pointer and returned through sret
// like any struct of two members, copied, stored and laid out as one.
// long double _Complex is refused.
//
// An expression of complex type evaluates, through aggAddr, to the address
// of its two parts: the object of an lvalue, a frame temporary for a
// computed value. What clang emits (probed with clang 23.1.4 -O1 -S):
//
//   - + and - are part by part; with a real operand, clang does not convert
//     it to complex but leaves the other operand's imaginary part as it is
//     (so -0.0 stays -0.0), and so does this;
//   - * of two complex values is __mulsc3 / __muldc3 (sret, a, b, c, d),
//     which is the textbook product with Annex G's recovery of infinities
//     from NaN results; with a real operand it is part by part;
//   - / is __divsc3 / __divdc3, of a real dividend too (as a + 0i); a
//     real divisor divides each part;
//   - -z negates both parts, ~z (GNU conjugate) the imaginary one;
//   - == and != compare both parts;
//   - a complex value in a scalar context is its real part (C11 6.3.1.7p2),
//     and as a truth value it is true when either part is non-zero.

// isCx reports whether t is a complex type the generator compiles: float
// and double _Complex, and GNU's integer complex types of int, unsigned,
// long and long long parts. long double _Complex and the char and short
// integer ones are refused.
func isCx(t cc.Type) bool {
	if t == nil {
		return false
	}
	switch t.Kind() {
	case cc.ComplexFloat, cc.ComplexDouble, cc.ComplexInt, cc.ComplexUInt, cc.ComplexLong, cc.ComplexLongLong:
		return true
	}
	return false
}

// cxElem is the representation of each part of the complex type t.
func cxElem(t cc.Type) scalar {
	switch t.Kind() {
	case cc.ComplexFloat:
		return scalar{vt: wasm.F32, bits: 32, signed: true, isFloat: true}
	case cc.ComplexDouble:
		return scalar{vt: wasm.F64, bits: 64, signed: true, isFloat: true}
	}
	bits := int(t.Size()) * 4 // half the size, in bits
	s := scalar{vt: wasm.I32, bits: bits, signed: t.Kind() != cc.ComplexUInt}
	if bits == 64 {
		s.vt = wasm.I64
	}
	return s
}

// cxConstPart pushes v as a part of representation el.
func (g *fgen) cxConstPart(v float64, el scalar) {
	if el.isFloat {
		g.constF(v, el)
		return
	}
	g.constant(int64(v), el)
}

// cxNeg negates the part in local l.
func (g *fgen) cxNeg(l uint32, el scalar) {
	if el.isFloat {
		g.getLocal(l)
		g.op(pick(el.vt == wasm.F32, opF32Neg, opF64Neg))
	} else {
		g.zero(el)
		g.getLocal(l)
		g.op(opFor(opI32Sub, el))
	}
	g.setLocal(l)
}

// cxRel pushes the comparison op (opI32Eq or opI32Ne) of the two parts on
// the stack.
func (g *fgen) cxRel(op byte, el scalar) {
	switch {
	case el.isFloat:
		g.op(floatRel(op, el))
	case el.vt == wasm.I64:
		g.op(op + i64CompareDelta)
	default:
		g.op(op)
	}
}

// cxPartAddr pushes the address of __real__ e or __imag__ e of the complex
// lvalue e.
func (g *fgen) cxPartAddr(e cc.ExpressionNode, imag bool) error {
	if err := g.aggAddr(e); err != nil {
		return err
	}
	if imag {
		g.constant(int64(cxElem(e.Type()).bits/8), intScalar)
		g.op(opI32Add)
	}
	return nil
}

// cxParts holds the two parts of a value in locals.
type cxParts struct {
	re, im uint32
	el     scalar
}

// parts evaluates e, of any arithmetic type, as a complex number whose
// parts have the representation el, into two new locals. real reports that
// e was not complex: its imaginary part is then a zero no operation should
// see (see the + and - rule above).
func (g *fgen) parts(e cc.ExpressionNode, el scalar) (p cxParts, real bool, err error) {
	p = cxParts{re: g.local(el.vt), im: g.local(el.vt), el: el}
	if !isCx(e.Type()) {
		if err := g.operand(e, el); err != nil {
			return p, true, err
		}
		g.setLocal(p.re)
		g.zero(el)
		g.setLocal(p.im)
		return p, true, nil
	}
	if err := g.aggAddr(e); err != nil {
		return p, false, err
	}
	a := g.local(wasm.I32)
	g.setLocal(a)
	from := cxElem(e.Type())
	for i, l := range []uint32{p.re, p.im} {
		g.getLocal(a)
		op, align := from.load()
		g.memargOff(op, align, uint64(i)*uint64(from.bits/8))
		g.convert(from, el)
		g.setLocal(l)
	}
	return p, false, nil
}

// store writes the parts into a new frame temporary of type t and pushes its
// address.
func (g *fgen) cxStore(t cc.Type, re, im uint32) {
	el := cxElem(t)
	slot := g.frameSlot(t.Size(), t.Align())
	for i, l := range []uint32{re, im} {
		g.slotAddress(slot)
		g.getLocal(l)
		op, align := el.store()
		g.memargOff(op, align, uint64(i)*uint64(el.bits/8))
	}
	g.slotAddress(slot)
}

// aggAddrAs pushes the address of the value of e converted to t, an
// aggregate: a complex t takes e of any arithmetic type.
func (g *fgen) aggAddrAs(e cc.ExpressionNode, t cc.Type) error {
	if isCx(t) && !(isCx(e.Type()) && e.Type().Kind() == t.Kind()) {
		p, _, err := g.parts(e, cxElem(t))
		if err != nil {
			return err
		}
		g.cxStore(t, p.re, p.im)
		return nil
	}
	return g.aggAddr(e)
}

// cxComputed compiles a complex value that is not an object: arithmetic,
// a cast, ?:, an assignment with an operator, a constant. It reports false,
// with nothing emitted, for the others, which aggAddr takes as aggregates.
func (g *fgen) cxComputed(e cc.ExpressionNode) (bool, error) {
	t := e.Type()
	if v, ok := cxConst(e); ok {
		el := cxElem(t)
		re, im := g.local(el.vt), g.local(el.vt)
		g.cxConstPart(real(v), el)
		g.setLocal(re)
		g.cxConstPart(imag(v), el)
		g.setLocal(im)
		g.cxStore(t, re, im)
		return true, nil
	}
	switch x := strip(e).(type) {
	case *cc.AdditiveExpression:
		switch x.Case {
		case cc.AdditiveExpressionAdd:
			return true, g.cxArith(t, x.AdditiveExpression, x.MultiplicativeExpression, opI32Add)
		case cc.AdditiveExpressionSub:
			return true, g.cxArith(t, x.AdditiveExpression, x.MultiplicativeExpression, opI32Sub)
		}
	case *cc.MultiplicativeExpression:
		switch x.Case {
		case cc.MultiplicativeExpressionMul:
			return true, g.cxArith(t, x.MultiplicativeExpression, x.CastExpression, opI32Mul)
		case cc.MultiplicativeExpressionDiv:
			return true, g.cxArith(t, x.MultiplicativeExpression, x.CastExpression, opI32DivS)
		}
	case *cc.UnaryExpression:
		switch x.Case {
		case cc.UnaryExpressionPlus, cc.UnaryExpressionMinus, cc.UnaryExpressionCpl:
			p, _, err := g.parts(x.CastExpression, cxElem(t))
			if err != nil {
				return true, err
			}
			if x.Case == cc.UnaryExpressionMinus {
				g.cxNeg(p.re, p.el)
			}
			if x.Case != cc.UnaryExpressionPlus {
				g.cxNeg(p.im, p.el)
			}
			g.cxStore(t, p.re, p.im)
			return true, nil
		}
	case *cc.CastExpression:
		if x.Case == cc.CastExpressionCast {
			return true, g.aggAddrAs(x.CastExpression, t)
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
		if err := g.aggAddrAs(x.ExpressionList, t); err != nil {
			return true, err
		}
		g.op(opElse)
		if err := g.aggAddrAs(x.ConditionalExpression, t); err != nil {
			return true, err
		}
		g.op(opEnd)
		return true, nil
	case *cc.AssignmentExpression:
		var op byte
		switch x.Case {
		case cc.AssignmentExpressionAssign:
			return true, g.aggAssign(x, true)
		case cc.AssignmentExpressionAdd:
			op = opI32Add
		case cc.AssignmentExpressionSub:
			op = opI32Sub
		case cc.AssignmentExpressionMul:
			op = opI32Mul
		case cc.AssignmentExpressionDiv:
			op = opI32DivS
		default:
			return false, nil
		}
		return true, g.cxCompound(x, op)
	}
	return false, nil
}

// cxConst returns the value of a constant expression of complex type: an
// imaginary constant (GNU 2.0i), a real one, and + - * and unary - of such.
// It folds as the code computes, a real operand by the rules above, which
// is what clang does too: 0.0 - 0.0i is -0 imaginary (measured, clang
// 23.1.4), where converting 0.0 to complex first would give +0.
func cxConst(e cc.ExpressionNode) (complex128, bool) {
	v, _, ok := cxFold(e)
	return v, ok
}

// cxFold is cxConst, and whether the value came from a real operand.
func cxFold(e cc.ExpressionNode) (v complex128, isReal, ok bool) {
	switch v := e.Value().(type) {
	case cc.Complex128Value:
		return complex128(v), false, true
	case cc.Complex64Value:
		return complex128(v), false, true
	}
	if !isCx(e.Type()) {
		if f, ok := floatConst(e); ok {
			return complex(f, 0), true, true
		}
		return 0, false, false
	}
	switch x := strip(e).(type) {
	case *cc.AdditiveExpression:
		a, ar, oka := cxFold(x.AdditiveExpression)
		b, br, okb := cxFold(x.MultiplicativeExpression)
		if !oka || !okb || x.Case != cc.AdditiveExpressionAdd && x.Case != cc.AdditiveExpressionSub {
			break
		}
		sub := x.Case == cc.AdditiveExpressionSub
		re := real(a) + real(b)
		if sub {
			re = real(a) - real(b)
		}
		var im float64
		switch {
		case br:
			im = imag(a)
		case ar && !sub:
			im = imag(b)
		case ar:
			im = -imag(b)
		case sub:
			im = imag(a) - imag(b)
		default:
			im = imag(a) + imag(b)
		}
		return complex(re, im), false, true
	case *cc.MultiplicativeExpression:
		a, ar, oka := cxFold(x.MultiplicativeExpression)
		b, br, okb := cxFold(x.CastExpression)
		if !oka || !okb || x.Case != cc.MultiplicativeExpressionMul {
			break
		}
		switch {
		case ar:
			return complex(real(a)*real(b), real(a)*imag(b)), false, true
		case br:
			return complex(real(a)*real(b), imag(a)*real(b)), false, true
		}
		return complex(real(a)*real(b)-imag(a)*imag(b), real(a)*imag(b)+imag(a)*real(b)), false, true
	case *cc.UnaryExpression:
		if x.Case == cc.UnaryExpressionMinus {
			if a, _, ok := cxFold(x.CastExpression); ok {
				return complex(-real(a), -imag(a)), false, true
			}
		}
	case *cc.CastExpression:
		if x.Case == cc.CastExpressionCast {
			v, _, ok := cxFold(x.CastExpression)
			return v, false, ok
		}
	}
	return 0, false, false
}

// cxArith pushes the address of a op b of complex type t.
func (g *fgen) cxArith(t cc.Type, a, b cc.ExpressionNode, op byte) error {
	el := cxElem(t)
	p, aReal, err := g.parts(a, el)
	if err != nil {
		return err
	}
	q, bReal, err := g.parts(b, el)
	if err != nil {
		return err
	}
	return g.cxCombine(t, p, aReal, q, bReal, op)
}

// cxCombine pushes the address of p op q, of complex type t; aReal and
// bReal tell which came from a real operand.
func (g *fgen) cxCombine(t cc.Type, p cxParts, aReal bool, q cxParts, bReal bool, op byte) error {
	el := p.el
	re, im := g.local(el.vt), g.local(el.vt)
	part := func(x, y uint32, o byte) {
		g.getLocal(x)
		g.getLocal(y)
		if o == opI32DivS && !el.isFloat && !el.signed {
			o = opI32DivU
		}
		g.op(opFor(o, el))
	}
	switch {
	case op == opI32Add || op == opI32Sub:
		part(p.re, q.re, op)
		g.setLocal(re)
		switch {
		case bReal:
			g.getLocal(p.im)
		case aReal && op == opI32Add:
			g.getLocal(q.im)
		case aReal:
			g.getLocal(q.im)
			g.setLocal(im)
			g.cxNeg(im, el)
			g.getLocal(im)
		default:
			part(p.im, q.im, op)
		}
		g.setLocal(im)
	case el.isFloat && (op == opI32Mul && (aReal || bReal) || op == opI32DivS && bReal):
		// A real operand scales each part of the other. Not for integer
		// parts: clang divides an integer complex by a real operand made
		// complex, which overflows differently (measured: (4e9 + 10i) / 3u).
		x, y, k := p.re, p.im, q.re
		if aReal {
			x, y, k = q.re, q.im, p.re
		}
		if op == opI32Mul && aReal {
			part(k, x, op)
			g.setLocal(re)
			part(k, y, op)
		} else {
			part(x, k, op)
			g.setLocal(re)
			part(y, k, op)
		}
		g.setLocal(im)
	case !el.isFloat:
		// GNU integer complex: the textbook product, and the quotient
		// ((ac + bd) / (cc + dd), (bc - ad) / (cc + dd)) as clang
		// computes it (CGExprComplex, EmitBinDiv for integers).
		if op == opI32Mul {
			part(p.re, q.re, opI32Mul)
			part(p.im, q.im, opI32Mul)
			g.op(opFor(opI32Sub, el))
			g.setLocal(re)
			part(p.re, q.im, opI32Mul)
			part(p.im, q.re, opI32Mul)
			g.op(opFor(opI32Add, el))
			g.setLocal(im)
			break
		}
		den := g.local(el.vt)
		part(q.re, q.re, opI32Mul)
		part(q.im, q.im, opI32Mul)
		g.op(opFor(opI32Add, el))
		g.setLocal(den)
		part(p.re, q.re, opI32Mul)
		part(p.im, q.im, opI32Mul)
		g.op(opFor(opI32Add, el))
		g.setLocal(re)
		part(re, den, opI32DivS)
		g.setLocal(re)
		part(p.im, q.re, opI32Mul)
		part(p.re, q.im, opI32Mul)
		g.op(opFor(opI32Sub, el))
		g.setLocal(im)
		part(im, den, opI32DivS)
		g.setLocal(im)
	default:
		name := map[bool]string{true: "__mul", false: "__div"}[op == opI32Mul]
		name += map[bool]string{true: "sc3", false: "dc3"}[el.vt == wasm.F32]
		slot := g.frameSlot(t.Size(), t.Align())
		g.slotAddress(slot)
		for _, l := range []uint32{p.re, p.im, q.re, q.im} {
			g.getLocal(l)
		}
		g.libcall(name, []scalar{intScalar, el, el, el, el}, nil)
		g.slotAddress(slot)
		return nil
	}
	g.cxStore(t, re, im)
	return nil
}

// cxCompound compiles E1 op= E2 for a complex E1: E2 first, then E1's
// address once, then E1 = E1 op E2; it pushes E1's address.
func (g *fgen) cxCompound(x *cc.AssignmentExpression, op byte) error {
	t := x.UnaryExpression.Type()
	el := cxElem(t)
	q, bReal, err := g.parts(x.AssignmentExpression, el)
	if err != nil {
		return err
	}
	if err := g.addr(x.UnaryExpression); err != nil {
		return err
	}
	dst := g.local(wasm.I32)
	g.setLocal(dst)
	p := cxParts{re: g.local(el.vt), im: g.local(el.vt), el: el}
	for i, l := range []uint32{p.re, p.im} {
		g.getLocal(dst)
		ld, align := el.load()
		g.memargOff(ld, align, uint64(i)*uint64(el.bits/8))
		g.setLocal(l)
	}
	if err := g.cxCombine(t, p, false, q, bReal, op); err != nil {
		return err
	}
	src := g.local(wasm.I32)
	g.setLocal(src)
	g.getLocal(dst)
	g.getLocal(src)
	g.constant(t.Size(), intScalar)
	g.op(0xfc, 10, 0, 0) // memory.copy
	g.getLocal(dst)
	return nil
}

// cxReal pushes the real part of the complex e, converted to its own
// representation, for a scalar context (C11 6.3.1.7p2).
func (g *fgen) cxReal(e cc.ExpressionNode) (scalar, error) {
	if err := g.aggAddr(e); err != nil {
		return scalar{}, err
	}
	el := cxElem(e.Type())
	op, align := el.load()
	g.memarg(op, align)
	return el, nil
}

// cxPart pushes __real__ e or __imag__ e: of a real e, e and 0 (GNU).
func (g *fgen) cxPart(e cc.ExpressionNode, imag bool, s scalar) error {
	if !isCx(e.Type()) {
		if imag {
			if err := g.effect(e); err != nil {
				return err
			}
			g.zero(s)
			return nil
		}
		return g.operand(e, s)
	}
	if err := g.aggAddr(e); err != nil {
		return err
	}
	el := cxElem(e.Type())
	op, align := el.load()
	off := uint64(0)
	if imag {
		off = uint64(el.bits / 8)
	}
	g.memargOff(op, align, off)
	g.convert(el, s)
	return nil
}

// cxTruth pushes an i32 that is non-zero when the complex e is: either part
// non-zero, a NaN included.
func (g *fgen) cxTruth(e cc.ExpressionNode) error {
	p, _, err := g.parts(e, cxElem(e.Type()))
	if err != nil {
		return err
	}
	for _, l := range []uint32{p.re, p.im} {
		g.getLocal(l)
		g.zero(p.el)
		g.cxRel(opI32Ne, p.el)
	}
	g.op(opI32Or)
	return nil
}

// cxEqual pushes a == b (eq) or a != b of operands at least one of which is
// complex, compared as complex numbers in their common type.
func (g *fgen) cxEqual(a, b cc.ExpressionNode, eq bool) error {
	t := cc.UsualArithmeticConversions(a.Type(), b.Type())
	if !isCx(t) {
		return unsupported(a, "comparison of "+typeName(a.Type())+" and "+typeName(b.Type()))
	}
	el := cxElem(t)
	p, _, err := g.parts(a, el)
	if err != nil {
		return err
	}
	q, _, err := g.parts(b, el)
	if err != nil {
		return err
	}
	for _, pair := range [][2]uint32{{p.re, q.re}, {p.im, q.im}} {
		g.getLocal(pair[0])
		g.getLocal(pair[1])
		g.cxRel(opI32Eq, el)
	}
	g.op(opI32And)
	if !eq {
		g.op(opI32Eqz)
	}
	return nil
}
