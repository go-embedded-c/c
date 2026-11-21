package wasmgen

import (
	"encoding/binary"
	"math/big"
	"strconv"
	"strings"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// long double (W5b): IEEE binary128, 16 bytes aligned on 16 (BasicCABI.md).
// It is not a wasm scalar. scalarOf keeps refusing it, so that a path that
// does not know it refuses instead of treating an address as a number; the
// value lives in memory, as an aggregate does, and every operation is a call
// to compiler-rt's soft-float routines, as clang emits them (probed with
// clang 23.1.4 -S):
//
//   - a parameter is two i64, low half then high half; a result goes through
//     an sret pointer, the first parameter (passQuad, sig.sret);
//   - + - * / are __addtf3, __subtf3, __multf3, __divtf3 (dst, a, b);
//   - a comparison is __lttf2, __letf2, __gttf2, __getf2, __eqtf2 or __netf2,
//     whose int result is then compared with 0 by the operator itself
//     (compiler-rt comparetf2.c: an unordered operand makes <, <=, ==
//     false and >, >=, != as the table says through its sign);
//   - negation flips the sign bit of the high half;
//   - a value used as a truth value is __netf2(a, 0) != 0.
//
// An expression of type long double evaluates, through ldAddr, to the
// address of 16 bytes: the object itself for an lvalue, a frame temporary
// for anything else. A long double in a scalar context goes through an
// explicit conversion (operand).

// isLD reports whether t is long double.
func isLD(t cc.Type) bool { return t != nil && t.Kind() == cc.LongDouble }

const (
	quadSize = 16
	opI64Xor = 0x85
)

var (
	quadArith = []scalar{u32, u64, u64, u64, u64} // dst, a (lo, hi), b (lo, hi)
	quadCmp   = []scalar{u64, u64, u64, u64}
	quadOne   = []scalar{u64, u64}
)

// libcall calls the compiler-rt routine name, importing it once.
func (g *fgen) libcall(name string, ps []scalar, r *scalar) {
	f := g.m.funcs[name]
	if f == nil {
		f = g.m.importFunc(name, scalarSig(ps, r))
	}
	g.op(opCall)
	g.cur.calls = append(g.cur.calls, callSite{offset: uint32(len(g.cur.code)), callee: f})
	g.reloc(wasm.RelocFunctionIndexLEB, f.sym)
}

// memargOff appends a load or store with its alignment and offset.
func (g *fgen) memargOff(op byte, align uint32, off uint64) {
	g.op(op)
	g.uleb(uint64(align))
	g.uleb(off)
}

func (g *fgen) getLocal(i uint32) {
	g.op(opLocalGet)
	g.uleb(uint64(i))
}

func (g *fgen) setLocal(i uint32) {
	g.op(opLocalSet)
	g.uleb(uint64(i))
}

// ldHalves pushes the low then the high half of the long double at the
// address in local a.
func (g *fgen) ldHalves(a uint32) {
	g.getLocal(a)
	g.memargOff(opI64Load, 3, 0)
	g.getLocal(a)
	g.memargOff(opI64Load, 3, 8)
}

// ldLocal evaluates e as a long double and keeps its address in a new local.
func (g *fgen) ldLocal(e cc.ExpressionNode) (uint32, error) {
	if err := g.ldAddr(e); err != nil {
		return 0, err
	}
	a := g.local(wasm.I32)
	g.setLocal(a)
	return a, nil
}

// ldTemp allocates a frame temporary for a long double.
func (g *fgen) ldTemp() int64 { return g.frameSlot(quadSize, 16) }

// ldStoreConst writes lo and hi into the temporary at slot.
func (g *fgen) ldStoreConst(slot int64, lo, hi uint64) {
	for i, v := range [2]uint64{lo, hi} {
		g.slotAddress(slot)
		g.op(opI64Const)
		g.sleb(int64(v))
		g.memargOff(opI64Store, 3, uint64(8*i))
	}
}

// memcopy16 copies a long double from the address in src to the one in dst.
func (g *fgen) memcopy16(dst, src uint32) {
	g.getLocal(dst)
	g.getLocal(src)
	g.constant(quadSize, intScalar)
	g.op(0xfc, 10, 0, 0) // memory.copy
}

// ldAddr pushes the address of 16 bytes holding the value of e converted to
// long double: the object of an lvalue, a frame temporary otherwise. e may
// have any arithmetic type.
func (g *fgen) ldAddr(e cc.ExpressionNode) error {
	if err := g.enter(e); err != nil {
		return err
	}
	defer func() { g.depth-- }()
	e = strip(e)
	if isLDCx(e.Type()) {
		// Its real part, at the start of its 32 bytes (ldcx.go).
		return g.aggAddr(e)
	}
	if isI128(e.Type()) {
		return g.iToLD(e)
	}
	if !isLD(e.Type()) {
		return g.ldFromScalar(e)
	}
	switch x := e.(type) {
	case *cc.PrimaryExpression:
		switch x.Case {
		case cc.PrimaryExpressionFloat:
			return g.ldLiteral(x)
		case cc.PrimaryExpressionIdent:
			return g.addr(x)
		}
	case *cc.CastExpression:
		if x.Case == cc.CastExpressionCast {
			// A conversion to long double of a long double or of a scalar.
			return g.ldAddr(x.CastExpression)
		}
	case *cc.UnaryExpression:
		switch x.Case {
		case cc.UnaryExpressionPlus:
			return g.ldAddr(x.CastExpression)
		case cc.UnaryExpressionMinus:
			return g.ldNeg(x.CastExpression)
		case cc.UnaryExpressionDeref, cc.UnaryExpressionReal, cc.UnaryExpressionImag:
			return g.addr(x)
		case cc.UnaryExpressionInc, cc.UnaryExpressionDec:
			return g.ldIncDec(x.UnaryExpression, x.Case == cc.UnaryExpressionInc, false, true)
		}
	case *cc.AdditiveExpression:
		lib := "__addtf3"
		if x.Case == cc.AdditiveExpressionSub {
			lib = "__subtf3"
		}
		return g.ldArith(lib, x.AdditiveExpression, x.MultiplicativeExpression)
	case *cc.MultiplicativeExpression:
		switch x.Case {
		case cc.MultiplicativeExpressionMul:
			return g.ldArith("__multf3", x.MultiplicativeExpression, x.CastExpression)
		case cc.MultiplicativeExpressionDiv:
			return g.ldArith("__divtf3", x.MultiplicativeExpression, x.CastExpression)
		}
	case *cc.AssignmentExpression:
		if x.Case == cc.AssignmentExpressionCond {
			return g.ldAddr(x.ConditionalExpression)
		}
		return g.ldAssign(x, true)
	case *cc.ConditionalExpression:
		if x.Case == cc.ConditionalExpressionLOr {
			return g.ldAddr(x.LogicalOrExpression)
		}
		if x.ExpressionList == nil {
			return unsupported(x, "?: without a middle operand")
		}
		if err := g.condition(x.LogicalOrExpression); err != nil {
			return err
		}
		g.op(opIf, byte(wasm.I32))
		if err := g.ldAddr(x.ExpressionList); err != nil {
			return err
		}
		g.op(opElse)
		if err := g.ldAddr(x.ConditionalExpression); err != nil {
			return err
		}
		g.op(opEnd)
		return nil
	case *cc.ExpressionList:
		for l := x; l != nil; l = l.ExpressionList {
			if l.ExpressionList == nil {
				return g.ldAddr(l.AssignmentExpression)
			}
			if err := g.effect(l.AssignmentExpression); err != nil {
				return err
			}
		}
	case *cc.LogicalOrExpression:
		if x.Case == cc.LogicalOrExpressionLAnd {
			return g.ldAddr(x.LogicalAndExpression)
		}
	case *cc.PostfixExpression:
		switch x.Case {
		case cc.PostfixExpressionCall:
			if ok, err := g.ldBuiltin(x); ok {
				return err
			}
			var slot int64
			if err := g.callAgg(x, &slot); err != nil {
				return err
			}
			g.slotAddress(slot)
			return nil
		case cc.PostfixExpressionInc, cc.PostfixExpressionDec:
			return g.ldIncDec(x.PostfixExpression, x.Case == cc.PostfixExpressionInc, true, true)
		case cc.PostfixExpressionIndex, cc.PostfixExpressionSelect, cc.PostfixExpressionPSelect, cc.PostfixExpressionComplit:
			return g.addr(x)
		}
	}
	return unsupported(e, "long double expression")
}

// ldLiteral materialises a constant long double in a temporary.
func (g *fgen) ldLiteral(e cc.ExpressionNode) error {
	lo, hi, ok := ldConst(e)
	if !ok {
		return unsupported(e, "long double constant "+constText(e))
	}
	tmp := g.ldTemp()
	g.ldStoreConst(tmp, lo, hi)
	g.slotAddress(tmp)
	return nil
}

func constText(e cc.ExpressionNode) string {
	if p, ok := strip(e).(*cc.PrimaryExpression); ok && p.Case == cc.PrimaryExpressionFloat {
		return p.Token.SrcStr()
	}
	return "expression"
}

// ldArith pushes the address of a lib b, computed into a temporary.
func (g *fgen) ldArith(lib string, a, b cc.ExpressionNode) error {
	la, err := g.ldLocal(a)
	if err != nil {
		return err
	}
	lb, err := g.ldLocal(b)
	if err != nil {
		return err
	}
	tmp := g.ldTemp()
	g.slotAddress(tmp)
	g.ldHalves(la)
	g.ldHalves(lb)
	g.libcall(lib, quadArith, nil)
	g.slotAddress(tmp)
	return nil
}

// ldNeg pushes the address of -a: the high half with its sign bit flipped.
func (g *fgen) ldNeg(a cc.ExpressionNode) error {
	la, err := g.ldLocal(a)
	if err != nil {
		return err
	}
	tmp := g.ldTemp()
	g.slotAddress(tmp)
	g.getLocal(la)
	g.memargOff(opI64Load, 3, 0)
	g.memargOff(opI64Store, 3, 0)
	g.slotAddress(tmp)
	g.getLocal(la)
	g.memargOff(opI64Load, 3, 8)
	g.op(opI64Const)
	g.sleb(-1 << 63)
	g.op(opI64Xor)
	g.memargOff(opI64Store, 3, 8)
	g.slotAddress(tmp)
	return nil
}

// ldFromScalar pushes the address of the long double equal to the scalar
// expression e.
func (g *fgen) ldFromScalar(e cc.ExpressionNode) error {
	if _, ok := scalarOf(e.Type()); !ok || isAggregate(e.Type()) {
		return unsupported(e, "conversion of "+typeName(e.Type())+" to long double")
	}
	from, err := g.rvalue(e)
	if err != nil {
		return err
	}
	g.ldFromStack(from)
	return nil
}

// ldFromStack converts the scalar on the stack to a long double and pushes
// its address.
func (g *fgen) ldFromStack(from scalar) {
	v := g.local(from.vt)
	g.setLocal(v)
	tmp := g.ldTemp()
	g.slotAddress(tmp)
	g.getLocal(v)
	switch {
	case from.vt == wasm.F32:
		g.libcall("__extendsftf2", []scalar{u32, f32Scalar}, nil)
	case from.vt == wasm.F64:
		g.libcall("__extenddftf2", []scalar{u32, f64Scalar}, nil)
	case from.vt == wasm.I64 && from.signed:
		g.libcall("__floatditf", []scalar{u32, u64}, nil)
	case from.vt == wasm.I64:
		g.libcall("__floatunditf", []scalar{u32, u64}, nil)
	case from.bits == 32 && !from.signed:
		g.libcall("__floatunsitf", []scalar{u32, u32}, nil)
	default:
		g.libcall("__floatsitf", []scalar{u32, u32}, nil)
	}
	g.slotAddress(tmp)
}

// operand pushes the value of e converted to to; e may be a long double.
func (g *fgen) operand(e cc.ExpressionNode, to scalar) error {
	if isLD(e.Type()) || isLDCx(e.Type()) {
		// A long double _Complex in a scalar context: its real part.
		return g.ldToScalar(e, to)
	}
	if isI128(e.Type()) {
		return g.iToScalar(e, to)
	}
	from, err := g.rvalue(e)
	if err != nil {
		return err
	}
	g.convert(from, to)
	return nil
}

// ldToScalar pushes the long double e converted to the scalar to.
func (g *fgen) ldToScalar(e cc.ExpressionNode, to scalar) error {
	a, err := g.ldLocal(e)
	if err != nil {
		return err
	}
	g.ldToScalarAt(a, to)
	return nil
}

// ldToScalarAt converts the long double at the address in local a.
func (g *fgen) ldToScalarAt(a uint32, to scalar) {
	if to.isBool {
		g.ldIsNonZero(a)
		return
	}
	g.ldHalves(a)
	switch {
	case to.vt == wasm.F32:
		g.libcall("__trunctfsf2", quadOne, &f32Scalar)
	case to.vt == wasm.F64:
		g.libcall("__trunctfdf2", quadOne, &f64Scalar)
	case to.vt == wasm.I64 && to.signed:
		g.libcall("__fixtfdi", quadOne, &s64)
	case to.vt == wasm.I64:
		g.libcall("__fixunstfdi", quadOne, &s64)
	case to.signed:
		g.libcall("__fixtfsi", quadOne, &intScalar)
		g.normalize(to)
	default:
		g.libcall("__fixunstfsi", quadOne, &intScalar)
		g.normalize(to)
	}
}

// ldIsNonZero pushes an i32 that is non-zero when the long double at the
// address in local a compares unequal to 0 (true for NaN): __netf2(a, 0).
func (g *fgen) ldIsNonZero(a uint32) {
	g.ldHalves(a)
	g.op(opI64Const, 0, opI64Const, 0)
	g.libcall("__netf2", quadCmp, &intScalar)
	g.constant(0, intScalar)
	g.op(opI32Ne)
}

// ldCondition pushes an i32 that is non-zero when e is true.
func (g *fgen) ldCondition(e cc.ExpressionNode) error {
	a, err := g.ldLocal(e)
	if err != nil {
		return err
	}
	g.ldIsNonZero(a)
	return nil
}

// ldCompare pushes the int result of a op b, the operands converted to long
// double; op is one of the i32 comparison opcodes, applied by the caller's
// convention to the result of the library routine and 0.
func (g *fgen) ldCompare(a, b cc.ExpressionNode, op byte) error {
	var lib string
	switch op {
	case opI32Eq:
		lib = "__eqtf2"
	case opI32Ne:
		lib = "__netf2"
	case opI32LtS, opI32LtU:
		lib, op = "__lttf2", opI32LtS
	case opI32GtS, opI32GtU:
		lib, op = "__gttf2", opI32GtS
	case opI32LeS, opI32LeU:
		lib, op = "__letf2", opI32LeS
	default:
		lib, op = "__getf2", opI32GeS
	}
	la, err := g.ldLocal(a)
	if err != nil {
		return err
	}
	lb, err := g.ldLocal(b)
	if err != nil {
		return err
	}
	g.ldHalves(la)
	g.ldHalves(lb)
	g.libcall(lib, quadCmp, &intScalar)
	g.constant(0, intScalar)
	g.op(op)
	return nil
}

// ldAssign compiles E1 = E2 and E1 op= E2 for a long double E1: E2 first,
// as for scalars, then E1's address; the result of an operation goes through
// a temporary. With want it leaves E1's address.
func (g *fgen) ldAssign(x *cc.AssignmentExpression, want bool) error {
	var lib string
	switch x.Case {
	case cc.AssignmentExpressionAssign:
	case cc.AssignmentExpressionAdd:
		lib = "__addtf3"
	case cc.AssignmentExpressionSub:
		lib = "__subtf3"
	case cc.AssignmentExpressionMul:
		lib = "__multf3"
	case cc.AssignmentExpressionDiv:
		lib = "__divtf3"
	default:
		return unsupported(x, "assignment "+x.Case.String()+" of a long double")
	}
	b, err := g.ldLocal(x.AssignmentExpression)
	if err != nil {
		return err
	}
	if err := g.addr(x.UnaryExpression); err != nil {
		return err
	}
	d := g.local(wasm.I32)
	g.setLocal(d)
	if lib == "" {
		g.memcopy16(d, b)
	} else {
		tmp := g.ldTemp()
		g.slotAddress(tmp)
		g.ldHalves(d)
		g.ldHalves(b)
		g.libcall(lib, quadArith, nil)
		g.getLocal(d)
		g.slotAddress(tmp)
		g.constant(quadSize, intScalar)
		g.op(0xfc, 10, 0, 0)
	}
	if want {
		g.getLocal(d)
	}
	return nil
}

// ldScalarAssign compiles E1 op= E2 for a scalar E1 and a long double E2:
// the operation is in long double (C11 6.5.16.2p3, 6.3.1.8), the result
// converted back to E1's type.
func (g *fgen) ldScalarAssign(x *cc.AssignmentExpression, t *target, want bool) error {
	var lib string
	switch x.Case {
	case cc.AssignmentExpressionAdd:
		lib = "__addtf3"
	case cc.AssignmentExpressionSub:
		lib = "__subtf3"
	case cc.AssignmentExpressionMul:
		lib = "__multf3"
	case cc.AssignmentExpressionDiv:
		lib = "__divtf3"
	default:
		return unsupported(x, "compound assignment of a scalar by a long double")
	}
	if t.s.ptr || t.bf {
		return unsupported(x, "compound assignment of a scalar by a long double")
	}
	b, err := g.ldLocal(x.AssignmentExpression)
	if err != nil {
		return err
	}
	if err := g.address(t); err != nil {
		return err
	}
	g.loadTarget(t)
	g.ldFromStack(t.s)
	l := g.local(wasm.I32)
	g.setLocal(l)
	tmp := g.ldTemp()
	g.slotAddress(tmp)
	g.ldHalves(l)
	g.ldHalves(b)
	g.libcall(lib, quadArith, nil)
	r := g.local(wasm.I32)
	g.slotAddress(tmp)
	g.setLocal(r)
	g.ldToScalarAt(r, t.s)
	g.store(t, want)
	return nil
}

// ldIncDec compiles ++E, --E, E++ and E-- for a long double E; with want it
// pushes the address of the value (the old one, in a copy, for the postfix
// forms).
func (g *fgen) ldIncDec(e cc.ExpressionNode, inc, post, want bool) error {
	if err := g.addr(e); err != nil {
		return err
	}
	d := g.local(wasm.I32)
	g.setLocal(d)
	one := g.ldTemp()
	g.ldStoreConst(one, 0, 0x3fff<<48)
	oneL := g.local(wasm.I32)
	g.slotAddress(one)
	g.setLocal(oneL)
	var old int64
	var oldL uint32
	if post && want {
		old = g.ldTemp()
		oldL = g.local(wasm.I32)
		g.slotAddress(old)
		g.setLocal(oldL)
		g.memcopy16(oldL, d)
	}
	lib := "__addtf3"
	if !inc {
		lib = "__subtf3"
	}
	tmp := g.ldTemp()
	g.slotAddress(tmp)
	g.ldHalves(d)
	g.ldHalves(oneL)
	g.libcall(lib, quadArith, nil)
	g.getLocal(d)
	g.slotAddress(tmp)
	g.constant(quadSize, intScalar)
	g.op(0xfc, 10, 0, 0)
	switch {
	case post && want:
		g.slotAddress(old)
	case want:
		g.getLocal(d)
	}
	return nil
}

// Constants. A long double constant is computed here exactly, as clang's
// APFloat does: a literal parsed once to binary128, operations rounded to
// 113 bits each. cc/v4 folds in 256 bits, which is not the same, so its
// folded value is never used.

const quadPrec = 113

// ldValue returns the value of a constant expression of arithmetic type
// that is a literal, an integer constant, or + - * / of such, rounded to
// binary128 as it goes; false for anything else.
func ldValue(e cc.ExpressionNode) (v *big.Float, ok bool) {
	// big.Float panics on Inf - Inf and the like: refused, not crashed.
	defer func() {
		if recover() != nil {
			v, ok = nil, false
		}
	}()
	v, ok = ldValue1(e)
	if ok && v.Sign() != 0 && !v.IsInf() {
		var m big.Float
		if v.MantExp(&m) > 16384 {
			// 2**16384 and above round to infinity (IEEE 754 round to
			// nearest even), as in 960405-1.c: X + X.
			v = new(big.Float).SetPrec(quadPrec).SetInf(v.Signbit())
		}
	}
	return v, ok
}

func ldValue1(e cc.ExpressionNode) (*big.Float, bool) {
	e = strip(e)
	newF := func() *big.Float { return new(big.Float).SetPrec(quadPrec).SetMode(big.ToNearestEven) }
	switch x := e.(type) {
	case *cc.PrimaryExpression:
		if x.Case == cc.PrimaryExpressionFloat {
			return parseFloatLiteral(x.Token.SrcStr())
		}
	case *cc.UnaryExpression:
		switch x.Case {
		case cc.UnaryExpressionPlus:
			return ldValue(x.CastExpression)
		case cc.UnaryExpressionMinus:
			v, ok := ldValue(x.CastExpression)
			if !ok {
				return nil, false
			}
			return newF().Neg(v), true
		}
	case *cc.CastExpression:
		if x.Case == cc.CastExpressionCast && isLD(x.Type()) {
			// To long double from a long double, an integer or a floating
			// literal: exact, or rounded as the operand already is.
			return ldValue(x.CastExpression)
		}
	case *cc.AdditiveExpression:
		if !isLD(x.Type()) {
			break // an integer expression: its folded value below
		}
		a, ok1 := ldValue(x.AdditiveExpression)
		b, ok2 := ldValue(x.MultiplicativeExpression)
		if !ok1 || !ok2 {
			return nil, false
		}
		if x.Case == cc.AdditiveExpressionSub {
			return newF().Sub(a, b), true
		}
		return newF().Add(a, b), true
	case *cc.MultiplicativeExpression:
		if !isLD(x.Type()) {
			break
		}
		a, ok1 := ldValue(x.MultiplicativeExpression)
		b, ok2 := ldValue(x.CastExpression)
		if !ok1 || !ok2 {
			return nil, false
		}
		switch x.Case {
		case cc.MultiplicativeExpressionMul:
			return newF().Mul(a, b), true
		case cc.MultiplicativeExpressionDiv:
			if b.Sign() == 0 {
				return nil, false
			}
			return newF().Quo(a, b), true
		}
		return nil, false
	}
	if t := e.Type(); t != nil && cc.IsIntegerType(t) && !isLD(t) {
		switch v := e.Value().(type) {
		case cc.Int64Value:
			return newF().SetInt64(int64(v)), true
		case cc.UInt64Value:
			return newF().SetUint64(uint64(v)), true
		}
	}
	return nil, false
}

// parseFloatLiteral reads a floating constant as C types it: the suffix L
// binary128, f binary32, none binary64, each rounded once from the text.
func parseFloatLiteral(text string) (*big.Float, bool) {
	newF := func() *big.Float { return new(big.Float).SetPrec(quadPrec).SetMode(big.ToNearestEven) }
	if len(text) > 400 {
		return nil, false
	}
	bits := 64
	quad := false
	switch {
	case strings.HasSuffix(text, "f"), strings.HasSuffix(text, "F"):
		bits = 32
		text = text[:len(text)-1]
	case strings.HasSuffix(text, "l"), strings.HasSuffix(text, "L"):
		quad = true
		text = text[:len(text)-1]
	}
	if !quad {
		v, err := strconv.ParseFloat(text, bits)
		if err != nil {
			return nil, false
		}
		return newF().SetFloat64(v), true
	}
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, "0x") {
		f, _, err := big.ParseFloat(text, 0, quadPrec, big.ToNearestEven)
		if err != nil || f.IsInf() {
			return nil, false
		}
		return f, true
	}
	// A decimal text is exact as a rational; its rounding to 113 bits is the
	// one correctly rounded division. An exponent far outside binary128's
	// range is refused before it is expanded.
	if i := strings.IndexByte(lower, 'e'); i >= 0 {
		n, err := strconv.Atoi(lower[i+1:])
		if err != nil || n > 6000 || n < -6000 {
			return nil, false
		}
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, false
	}
	return newF().SetRat(r), true
}

// quadBits returns the binary128 encoding of f, a value of at most 113 bits
// of precision: low and high halves. Zero and the infinities are encoded; a
// subnormal or an overflow not yet clamped is refused.
func quadBits(f *big.Float) (lo, hi uint64, ok bool) {
	var sign uint64
	if f.Signbit() {
		sign = 1 << 63
	}
	if f.Sign() == 0 {
		return 0, sign, true
	}
	if f.IsInf() {
		return 0, sign | 0x7fff<<48, true
	}
	if f.Prec() > quadPrec {
		return 0, 0, false
	}
	var m big.Float
	exp := f.MantExp(&m) // f = m * 2**exp, 0.5 <= |m| < 1
	e := exp - 1 + 16383
	if e <= 0 || e >= 0x7fff {
		return 0, 0, false
	}
	m.Abs(&m)
	m.SetMantExp(&m, quadPrec) // an integer in [2**112, 2**113)
	mi, acc := m.Int(nil)
	if acc != big.Exact {
		return 0, 0, false
	}
	mi.Sub(mi, new(big.Int).Lsh(big.NewInt(1), 112))
	lo = new(big.Int).And(mi, new(big.Int).SetUint64(^uint64(0))).Uint64()
	hi = new(big.Int).Rsh(mi, 64).Uint64() | uint64(e)<<48 | sign
	return lo, hi, true
}

// ldConst returns the binary128 encoding of a constant expression.
func ldConst(e cc.ExpressionNode) (lo, hi uint64, ok bool) {
	if p, isP := strip(e).(*cc.PostfixExpression); isP && p.Case == cc.PostfixExpressionCall {
		return ldBuiltinConst(p)
	}
	if u, isU := strip(e).(*cc.UnaryExpression); isU && u.Case == cc.UnaryExpressionMinus {
		if p, isP := strip(u.CastExpression).(*cc.PostfixExpression); isP && p.Case == cc.PostfixExpressionCall {
			lo, hi, ok = ldBuiltinConst(p)
			return lo, hi ^ 1<<63, ok
		}
	}
	v, ok := ldValue(e)
	if !ok {
		return 0, 0, false
	}
	return quadBits(v)
}

// ldStatic writes the 16 bytes of the constant e at off in b.
func ldStatic(e cc.ExpressionNode, b []byte) bool {
	lo, hi, ok := ldConst(e)
	if !ok || len(b) < quadSize {
		return false
	}
	binary.LittleEndian.PutUint64(b, lo)
	binary.LittleEndian.PutUint64(b[8:], hi)
	return true
}

// ldClassify compiles isinf, isfinite, isnan and signbit of a long double
// from the bits of its high half (sign, 15-bit exponent, top of the
// significand) and its low half; isnan is __unordtf2(x, x) as clang emits.
func (g *fgen) ldClassify(x *cc.PostfixExpression, b string, a cc.ExpressionNode) error {
	la, err := g.ldLocal(a)
	if err != nil {
		return err
	}
	const inf, mag = 0x7fff << 48, 1<<63 - 1
	hi := func() {
		g.getLocal(la)
		g.memargOff(opI64Load, 3, 8)
	}
	switch b {
	case "isinf":
		// ((hi & mag) ^ inf | lo) == 0
		hi()
		g.op(opI64Const)
		g.sleb(mag)
		g.op(opI64And, opI64Const)
		g.sleb(inf)
		g.op(opI64Xor)
		g.getLocal(la)
		g.memargOff(opI64Load, 3, 0)
		g.op(opI64Or, opI64Eqz)
	case "isfinite":
		// (hi & inf) != inf
		hi()
		g.op(opI64Const)
		g.sleb(inf)
		g.op(opI64And, opI64Const)
		g.sleb(inf)
		g.op(0x52) // i64.ne
	case "isnan":
		g.ldHalves(la)
		g.ldHalves(la)
		g.libcall("__unordtf2", quadCmp, &intScalar)
		g.constant(0, intScalar)
		g.op(opI32Ne)
	case "signbit":
		hi()
		g.constant(63, s64)
		g.op(opI64ShrU, opI32WrapI64)
	default:
		return unsupported(x, "__builtin_"+b+" of long double")
	}
	return g.builtinResult(x, intScalar)
}

// ldUnordered compiles the quiet comparisons of C11 7.12.14 on long doubles:
// the routines of the ordinary operators, whose result for an unordered
// pair makes each of < <= > >= false (compiler-rt comparetf2.c), and
// __unordtf2.
func (g *fgen) ldUnordered(x *cc.PostfixExpression, b string, args []cc.ExpressionNode) error {
	la, err := g.ldLocal(args[0])
	if err != nil {
		return err
	}
	lb, err := g.ldLocal(args[1])
	if err != nil {
		return err
	}
	rel := func(lib string, op byte) {
		g.ldHalves(la)
		g.ldHalves(lb)
		g.libcall(lib, quadCmp, &intScalar)
		g.constant(0, intScalar)
		g.op(op)
	}
	switch b {
	case "isunordered":
		rel("__unordtf2", opI32Ne)
	case "isgreater":
		rel("__gttf2", opI32GtS)
	case "isgreaterequal":
		rel("__getf2", opI32GeS)
	case "isless":
		rel("__lttf2", opI32LtS)
	case "islessequal":
		rel("__letf2", opI32LeS)
	case "islessgreater":
		rel("__lttf2", opI32LtS)
		rel("__gttf2", opI32GtS)
		g.op(opI32Or)
	default:
		return unsupported(x, "__builtin_"+b+" of long double")
	}
	return g.builtinResult(x, intScalar)
}
