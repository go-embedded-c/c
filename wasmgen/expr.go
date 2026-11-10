package wasmgen

import (
	"strings"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// intValue returns the value of a constant integer expression.
//
// For an expression of pointer type, cc/v4's value cannot be trusted: its
// eval caches one value per node for two meanings, and &"X"[0] reports 88,
// the value of "X"[0] (value.go, PostfixExpression.eval). A pointer
// constant is taken only from an integer expression cast to the pointer
// type, as in (char *)0.
func intValue(e cc.ExpressionNode) (int64, bool) {
	if e.Type() != nil && e.Type().Kind() == cc.Ptr {
		x, ok := strip(e).(*cc.CastExpression)
		if !ok || x.Case != cc.CastExpressionCast {
			return 0, false
		}
		return intValue(x.CastExpression)
	}
	switch v := e.Value().(type) {
	case cc.Int64Value:
		return int64(v), true
	case cc.UInt64Value:
		return int64(v), true
	}
	return 0, false
}

// convertConst returns v converted to s, normalised as values of s are kept
// (C11 6.3.1.2, 6.3.1.3; the implementation-defined case wraps, as clang).
func convertConst(v int64, s scalar) int64 {
	switch {
	case s.isBool:
		if v != 0 {
			return 1
		}
		return 0
	case s.bits == 64:
		return v
	case s.signed:
		sh := 64 - s.bits
		return v << sh >> sh
	}
	return int64(uint64(v) & (1<<s.bits - 1))
}

func (g *fgen) constant(v int64, s scalar) {
	if s.isFloat {
		g.constF(float64(v), s)
		return
	}
	v = convertConst(v, s)
	if s.vt == wasm.I64 {
		g.op(opI64Const)
		g.sleb(v)
		return
	}
	g.op(opI32Const)
	g.sleb(int64(int32(uint32(v))))
}

// normalize re-establishes the representation of s, narrower than its
// wasm type, on the value on the stack.
func (g *fgen) normalize(s scalar) {
	switch {
	case s.isBool:
		g.op(opI32Eqz, opI32Eqz)
	case s.bits >= 32:
	case s.signed && s.bits == 8:
		g.op(opI32Extend8S)
	case s.signed && s.bits == 16:
		g.op(opI32Extend16S)
	default:
		g.constant(int64(1)<<s.bits-1, scalar{vt: wasm.I32, bits: 32, signed: true})
		g.op(opI32And)
	}
}

// convert converts the value on the stack from from to to.
func (g *fgen) convert(from, to scalar) {
	if g.convertFloat(from, to) {
		return
	}
	switch {
	case to.isBool:
		if from.isBool {
			return
		}
		if from.vt == wasm.I64 {
			g.op(opI64Eqz, opI32Eqz)
			return
		}
		g.op(opI32Eqz, opI32Eqz)
	case from.vt == wasm.I64 && to.vt == wasm.I32:
		g.op(opI32WrapI64)
		g.normalize(to)
	case from.vt == wasm.I32 && to.vt == wasm.I64:
		if from.signed {
			g.op(opI64ExtendI32S)
		} else {
			g.op(opI64ExtendI32U)
		}
	case to.vt == wasm.I32 && to.bits < 32 && (to.bits < from.bits || to.signed != from.signed || from.isBool):
		g.normalize(to)
	}
}

func (g *fgen) enter(n cc.Node) error {
	if g.depth++; g.depth > g.m.lim.MaxDepth {
		return unsupported(n, "expression nested deeper than the limit")
	}
	return nil
}

// rvalue pushes the value of e, of e.Type(), and returns its representation.
func (g *fgen) rvalue(e cc.ExpressionNode) (scalar, error) {
	if err := g.enter(e); err != nil {
		return scalar{}, err
	}
	defer func() { g.depth-- }()
	t := e.Type()
	if a, ok := t.(*cc.ArrayType); ok {
		// An array is converted to a pointer to its first element.
		ps, _ := scalarOf(a.Decay())
		return ps, g.addr(e)
	}
	if designatesArray(e) {
		ps, _ := scalarOf(t) // already typed as the decayed pointer
		return ps, g.addr(e)
	}
	if d := addressedFunction(e); d != nil {
		// f or &f: a pointer to the function, its table index.
		return scalar{vt: wasm.I32, bits: 32, ptr: true, elem: 1}, g.funcAddr(e, d)
	}
	s, ok := scalarOf(t)
	if !ok {
		return scalar{}, unsupported(e, "value of type "+typeName(t))
	}
	if u, ok := e.(*cc.UnaryExpression); ok && (u.Case == cc.UnaryExpressionSizeofExpr || u.Case == cc.UnaryExpressionSizeofType) {
		// sizeof of a variable-length type is computed at run time (C11
		// 6.5.3.4p2); cc/v4 folds it to a constant (20040423-1.c).
		var t cc.Type
		if u.Case == cc.UnaryExpressionSizeofType {
			t = u.TypeName.Type()
		} else {
			t = undecayed(u.UnaryExpression.Type())
		}
		if hasVLA(t) {
			return s, unsupported(u, "sizeof of a variable-length type")
		}
	}
	if v, ok := intValue(e); ok && e.Pure() {
		g.constant(v, s)
		return s, nil
	}
	switch x := e.(type) {
	case *cc.PrimaryExpression:
		switch x.Case {
		case cc.PrimaryExpressionExpr:
			from, err := g.rvalue(x.ExpressionList)
			if err != nil {
				return s, err
			}
			g.convert(from, s)
			return s, nil
		case cc.PrimaryExpressionIdent:
			return s, g.load(x)
		case cc.PrimaryExpressionFloat:
			v, ok := floatLiteral(x)
			if !ok || !s.isFloat {
				return s, unsupported(x, "floating constant "+x.Token.SrcStr())
			}
			g.constF(v, s)
			return s, nil
		}
		return s, unsupported(x, "primary expression "+x.Case.String())
	case *cc.ExpressionList:
		for l := x; l != nil; l = l.ExpressionList {
			if l.ExpressionList == nil {
				from, err := g.rvalue(l.AssignmentExpression)
				if err != nil {
					return s, err
				}
				g.convert(from, s)
				return s, nil
			}
			if err := g.effect(l.AssignmentExpression); err != nil {
				return s, err
			}
		}
	case *cc.AssignmentExpression:
		if x.Case == cc.AssignmentExpressionCond {
			return g.rvalue(x.ConditionalExpression)
		}
		return s, g.assign(x, true)
	case *cc.ConditionalExpression:
		if x.Case == cc.ConditionalExpressionLOr {
			return g.rvalue(x.LogicalOrExpression)
		}
		if x.ExpressionList == nil {
			return s, unsupported(x, "?: without a middle operand")
		}
		// E1 ? E2 : E3 (C11 6.5.15): an if with a result, so that values
		// already pushed by the enclosing expression stay below it.
		if err := g.condition(x.LogicalOrExpression); err != nil {
			return s, err
		}
		g.op(opIf, byte(s.vt))
		for i, e := range []cc.ExpressionNode{x.ExpressionList, x.ConditionalExpression} {
			if i == 1 {
				g.op(opElse)
			}
			from, err := g.rvalue(e)
			if err != nil {
				return s, err
			}
			g.convert(from, s)
		}
		g.op(opEnd)
		return s, nil
	case *cc.LogicalOrExpression:
		if x.Case == cc.LogicalOrExpressionLAnd {
			return g.rvalue(x.LogicalAndExpression)
		}
		// E1 || E2 (C11 6.5.14): 1 if E1 is non-zero, else E2 != 0.
		if err := g.condition(x.LogicalOrExpression); err != nil {
			return s, err
		}
		g.op(opIf, byte(wasm.I32), opI32Const, 1, opElse)
		if err := g.condition(x.LogicalAndExpression); err != nil {
			return s, err
		}
		g.op(opI32Eqz, opI32Eqz, opEnd)
		return s, nil
	case *cc.LogicalAndExpression:
		if x.Case == cc.LogicalAndExpressionOr {
			return g.rvalue(x.InclusiveOrExpression)
		}
		// E1 && E2 (C11 6.5.13): E2 != 0 if E1 is non-zero, else 0.
		if err := g.condition(x.LogicalAndExpression); err != nil {
			return s, err
		}
		g.op(opIf, byte(wasm.I32))
		if err := g.condition(x.InclusiveOrExpression); err != nil {
			return s, err
		}
		g.op(opI32Eqz, opI32Eqz, opElse, opI32Const, 0, opEnd)
		return s, nil
	case *cc.InclusiveOrExpression:
		return s, g.binary(x.InclusiveOrExpression, x.ExclusiveOrExpression, s, opI32Or)
	case *cc.ExclusiveOrExpression:
		return s, g.binary(x.ExclusiveOrExpression, x.AndExpression, s, opI32Xor)
	case *cc.AndExpression:
		return s, g.binary(x.AndExpression, x.EqualityExpression, s, opI32And)
	case *cc.EqualityExpression:
		op := byte(opI32Eq)
		if x.Case == cc.EqualityExpressionNeq {
			op = opI32Ne
		}
		return s, g.compare(x.EqualityExpression, x.RelationalExpression, op, op)
	case *cc.RelationalExpression:
		var ss, uu byte
		switch x.Case {
		case cc.RelationalExpressionLt:
			ss, uu = opI32LtS, opI32LtU
		case cc.RelationalExpressionGt:
			ss, uu = opI32GtS, opI32GtU
		case cc.RelationalExpressionLeq:
			ss, uu = opI32LeS, opI32LeU
		default:
			ss, uu = opI32GeS, opI32GeU
		}
		return s, g.compare(x.RelationalExpression, x.ShiftExpression, ss, uu)
	case *cc.ShiftExpression:
		op := byte(opI32Shl)
		if x.Case == cc.ShiftExpressionRsh {
			op = opI32ShrU
			if s.signed {
				op = opI32ShrS
			}
		}
		return s, g.binary(x.ShiftExpression, x.AdditiveExpression, s, op)
	case *cc.AdditiveExpression:
		op := byte(opI32Add)
		if x.Case == cc.AdditiveExpressionSub {
			op = opI32Sub
		}
		a, b := x.AdditiveExpression, x.MultiplicativeExpression
		pa, pb := isPtrOrArray(a.Type()), isPtrOrArray(b.Type())
		switch {
		case pa && pb:
			// p - q: the number of elements between them (C11 6.5.6p9).
			ps, _ := scalarOf(a.Type().Decay())
			if err := g.ptrValue(a); err != nil {
				return s, err
			}
			if err := g.ptrValue(b); err != nil {
				return s, err
			}
			g.op(opI32Sub)
			if ps.elem != 1 {
				g.constant(ps.elem, intScalar)
				g.op(opI32DivS)
			}
			g.convert(intScalar, s)
			return s, nil
		case pa || pb:
			// p ± n, n + p: n scaled by the size of the element.
			p, n := a, b
			if pb {
				p, n = b, a
			}
			if err := g.ptrValue(p); err != nil {
				return s, err
			}
			if err := g.offset(n, s.elem); err != nil {
				return s, err
			}
			g.op(op)
			return s, nil
		}
		return s, g.binary(a, b, s, op)
	case *cc.MultiplicativeExpression:
		var op byte
		switch x.Case {
		case cc.MultiplicativeExpressionMul:
			op = opI32Mul
		case cc.MultiplicativeExpressionDiv:
			op = pick(s.signed, opI32DivS, opI32DivU)
		default:
			op = pick(s.signed, opI32RemS, opI32RemU)
		}
		return s, g.binary(x.MultiplicativeExpression, x.CastExpression, s, op)
	case *cc.CastExpression:
		if x.Case == cc.CastExpressionUnary {
			return g.rvalue(x.UnaryExpression)
		}
		from, err := g.rvalue(x.CastExpression)
		if err != nil {
			return s, err
		}
		g.convert(from, s)
		return s, nil
	case *cc.UnaryExpression:
		return s, g.unary(x, s)
	case *cc.PostfixExpression:
		switch x.Case {
		case cc.PostfixExpressionPrimary:
			return g.rvalue(x.PrimaryExpression)
		case cc.PostfixExpressionCall:
			return s, g.call(x)
		case cc.PostfixExpressionInc, cc.PostfixExpressionDec:
			return s, g.incdec(x, x.PostfixExpression, x.Case == cc.PostfixExpressionInc, true, true)
		case cc.PostfixExpressionIndex, cc.PostfixExpressionSelect, cc.PostfixExpressionPSelect:
			return s, g.load(x)
		}
		return s, unsupported(x, "postfix "+x.Case.String())
	}
	return s, unsupported(e, "expression")
}

func pick(signed bool, s, u byte) byte {
	if signed {
		return s
	}
	return u
}

func typeName(t cc.Type) string {
	if t == nil {
		return "<nil>"
	}
	return t.String()
}

// i64 maps an i32 binary opcode (add .. shr_u) to its i64 counterpart.
func opFor(op byte, s scalar) byte {
	if s.isFloat {
		return floatOp(op, s)
	}
	if s.vt == wasm.I64 {
		return op + i64Delta
	}
	return op
}

// binary pushes a op b computed in s, the type of the expression: after
// the usual arithmetic conversions (or, for shifts, the promoted left
// operand) both operands are converted to it.
func (g *fgen) binary(a, b cc.ExpressionNode, s scalar, op byte) error {
	for _, e := range []cc.ExpressionNode{a, b} {
		from, err := g.rvalue(e)
		if err != nil {
			return err
		}
		g.convert(from, s)
	}
	g.op(opFor(op, s))
	g.normalize(s)
	return nil
}

// compare pushes the int result of comparing a and b in their common type.
func (g *fgen) compare(a, b cc.ExpressionNode, signed, unsigned byte) error {
	if isPtrOrArray(a.Type()) || isPtrOrArray(b.Type()) || addressedFunction(a) != nil || addressedFunction(b) != nil {
		// Pointers compare as unsigned addresses; a null pointer constant
		// or another integer converts to an address (C11 6.5.8, 6.5.9).
		for _, e := range []cc.ExpressionNode{a, b} {
			if err := g.ptrValue(e); err != nil {
				return err
			}
		}
		g.op(unsigned)
		return nil
	}
	if _, ok := scalarOf(a.Type()); !ok {
		return unsupported(a, "comparison of "+typeName(a.Type()))
	}
	if _, ok := scalarOf(b.Type()); !ok {
		return unsupported(b, "comparison of "+typeName(b.Type()))
	}
	common, ok := scalarOf(cc.UsualArithmeticConversions(a.Type(), b.Type()))
	if !ok {
		return unsupported(a, "comparison of "+typeName(a.Type())+" and "+typeName(b.Type()))
	}
	for _, e := range []cc.ExpressionNode{a, b} {
		from, err := g.rvalue(e)
		if err != nil {
			return err
		}
		g.convert(from, common)
	}
	op := pick(common.signed, signed, unsigned)
	if common.isFloat {
		g.op(floatRel(op, common))
		return nil
	}
	if common.vt == wasm.I64 {
		op += i64CompareDelta
	}
	g.op(op)
	return nil
}

func (g *fgen) unary(x *cc.UnaryExpression, s scalar) error {
	switch x.Case {
	case cc.UnaryExpressionPostfix:
		_, err := g.rvalue(x.PostfixExpression)
		return err
	case cc.UnaryExpressionPlus:
		from, err := g.rvalue(x.CastExpression)
		if err != nil {
			return err
		}
		g.convert(from, s)
		return nil
	case cc.UnaryExpressionMinus:
		if s.isFloat {
			// f.neg flips the sign: -(+0.0) is -0.0, which 0 - x is not.
			from, err := g.rvalue(x.CastExpression)
			if err != nil {
				return err
			}
			g.convert(from, s)
			g.op(pick(s.vt == wasm.F32, opF32Neg, opF64Neg))
			return nil
		}
		g.zero(s)
		from, err := g.rvalue(x.CastExpression)
		if err != nil {
			return err
		}
		g.convert(from, s)
		g.op(opFor(opI32Sub, s))
		g.normalize(s)
		return nil
	case cc.UnaryExpressionCpl:
		from, err := g.rvalue(x.CastExpression)
		if err != nil {
			return err
		}
		g.convert(from, s)
		g.constant(-1, s)
		g.op(opFor(opI32Xor, s))
		g.normalize(s)
		return nil
	case cc.UnaryExpressionNot:
		from, err := g.rvalue(x.CastExpression)
		if err != nil {
			return err
		}
		switch {
		case from.isFloat:
			g.constF(0, from) // !x is x == 0 (C11 6.5.3.3p5): false for NaN
			g.op(floatRel(opI32Eq, from))
		case from.vt == wasm.I64:
			g.op(opI64Eqz)
		default:
			g.op(opI32Eqz)
		}
		return nil
	case cc.UnaryExpressionInc, cc.UnaryExpressionDec:
		return g.incdec(x, x.UnaryExpression, x.Case == cc.UnaryExpressionInc, false, true)
	case cc.UnaryExpressionAddrof:
		if d := functionDesignator(x.CastExpression); d != nil {
			return g.funcAddr(x, d)
		}
		return g.addr(x.CastExpression)
	case cc.UnaryExpressionDeref:
		return g.load(x)
	}
	return unsupported(x, "unary "+x.Case.String())
}

// effect evaluates e for its side effects only.
func (g *fgen) effect(e cc.ExpressionNode) error {
	if err := g.enter(e); err != nil {
		return err
	}
	defer func() { g.depth-- }()
	switch x := e.(type) {
	case *cc.ExpressionList:
		for l := x; l != nil; l = l.ExpressionList {
			if err := g.effect(l.AssignmentExpression); err != nil {
				return err
			}
		}
		return nil
	case *cc.AssignmentExpression:
		if x.Case != cc.AssignmentExpressionCond {
			return g.assign(x, false)
		}
	case *cc.PostfixExpression:
		switch x.Case {
		case cc.PostfixExpressionInc, cc.PostfixExpressionDec:
			return g.incdec(x, x.PostfixExpression, x.Case == cc.PostfixExpressionInc, true, false)
		case cc.PostfixExpressionCall:
			if isVoid(x.Type()) || isAggregate(x.Type()) {
				return g.call(x)
			}
			if err := g.call(x); err != nil {
				return err
			}
			g.op(opDrop)
			return nil
		}
	case *cc.UnaryExpression:
		if x.Case == cc.UnaryExpressionInc || x.Case == cc.UnaryExpressionDec {
			return g.incdec(x, x.UnaryExpression, x.Case == cc.UnaryExpressionInc, false, false)
		}
	case *cc.CastExpression:
		if x.Case == cc.CastExpressionCast && isVoid(x.Type()) {
			return g.effect(x.CastExpression)
		}
	case *cc.PrimaryExpression:
		if x.Case == cc.PrimaryExpressionExpr {
			return g.effect(x.ExpressionList)
		}
	case *cc.LogicalAndExpression, *cc.LogicalOrExpression:
		// `a && f();`: evaluate the right operand only for its effect.
		var a, b cc.ExpressionNode
		negate := false
		if and, ok := x.(*cc.LogicalAndExpression); ok && and.Case == cc.LogicalAndExpressionLAnd {
			a, b = and.LogicalAndExpression, and.InclusiveOrExpression
		} else if or, ok := x.(*cc.LogicalOrExpression); ok && or.Case == cc.LogicalOrExpressionLOr {
			a, b, negate = or.LogicalOrExpression, or.LogicalAndExpression, true
		}
		if a != nil {
			if err := g.condition(a); err != nil {
				return err
			}
			if negate {
				g.op(opI32Eqz)
			}
			g.op(opIf, voidBT)
			if err := g.effect(b); err != nil {
				return err
			}
			g.op(opEnd)
			return nil
		}
	case *cc.ConditionalExpression:
		if x.Case == cc.ConditionalExpressionCond && x.ExpressionList != nil {
			if err := g.condition(x.LogicalOrExpression); err != nil {
				return err
			}
			g.op(opIf, voidBT)
			if err := g.effect(x.ExpressionList); err != nil {
				return err
			}
			g.op(opElse)
			if err := g.effect(x.ConditionalExpression); err != nil {
				return err
			}
			g.op(opEnd)
			return nil
		}
	}
	if e.Pure() {
		return nil
	}
	if isAggregate(e.Type()) {
		if err := g.aggAddr(e); err != nil {
			return err
		}
		g.op(opDrop)
		return nil
	}
	if _, err := g.rvalue(e); err != nil {
		return err
	}
	g.op(opDrop)
	return nil
}

// target is a scalar lvalue: a register local (a wasm local), or an object
// in memory, whose address e designates.
type target struct {
	local bool
	index uint32
	e     cc.ExpressionNode // the lvalue, for memory
	addr  uint32            // a local holding its address, once address ran
	s     scalar
	// A bit-field: e is the member expression, addr holds its unit's
	// address.
	bf     bool
	unit   bitunit
	member *cc.PostfixExpression
}

func (g *fgen) lvalue(e cc.ExpressionNode) (*target, error) {
	e = strip(e)
	s, ok := scalarOf(e.Type())
	if !ok {
		return nil, unsupported(e, "lvalue of type "+typeName(e.Type()))
	}
	if x, f := bitfieldOf(e); f != nil {
		u, err := unitOf(x, f)
		if err != nil {
			return nil, err
		}
		return &target{e: e, s: s, bf: true, unit: u, member: x}, nil
	}
	if x, ok := e.(*cc.PrimaryExpression); ok && x.Case == cc.PrimaryExpressionIdent {
		if d, ok := x.ResolvedTo().(*cc.Declarator); ok {
			if i, ok := g.vars[d]; ok {
				return &target{local: true, index: i, s: s}, nil
			}
		}
	}
	return &target{e: e, s: s}, nil
}

// memarg appends a load or store with its alignment and offset 0.
func (g *fgen) memarg(op byte, align uint32) {
	g.op(op)
	g.uleb(uint64(align))
	g.uleb(0)
}

// address pushes the address of t when it is in memory, keeping a copy for
// loadTarget; a register local has none.
func (g *fgen) address(t *target) error {
	if t.local {
		return nil
	}
	var err error
	if t.bf {
		err = g.memberAddr(t.member)
	} else {
		err = g.addr(t.e)
	}
	if err != nil {
		return err
	}
	t.addr = g.local(wasm.I32)
	g.op(opLocalTee)
	g.uleb(uint64(t.addr))
	return nil
}

// loadTarget pushes the value of t; address ran before for memory.
func (g *fgen) loadTarget(t *target) error {
	if t.local {
		g.op(opLocalGet)
		g.uleb(uint64(t.index))
		return nil
	}
	g.op(opLocalGet)
	g.uleb(uint64(t.addr))
	if t.bf {
		g.loadUnit(t.unit)
		g.extract(t.unit)
		g.convert(t.unit.field(), t.s)
		return nil
	}
	op, align := t.s.load()
	g.memarg(op, align)
	return nil
}

// store stores the value on the stack into t, leaving a copy when want. For
// memory, address pushed the address before the value.
func (g *fgen) store(t *target, want bool) {
	if t.local {
		g.op(pick(want, opLocalTee, opLocalSet))
		g.uleb(uint64(t.index))
		return
	}
	if t.bf {
		// The value, in the unit's type; the address under it is in
		// t.addr already.
		v := g.local(t.unit.s.vt)
		g.convert(t.s, t.unit.field())
		g.op(opLocalSet)
		g.uleb(uint64(v))
		g.op(opDrop)
		g.insert(t.unit, t.addr, v)
		if want {
			// The value of the assignment is the field's, after it
			// (C11 6.5.16p3): what was stored, truncated to the width.
			g.op(opLocalGet)
			g.uleb(uint64(t.addr))
			g.loadUnit(t.unit)
			g.extract(t.unit)
			g.convert(t.unit.field(), t.s)
		}
		return
	}
	var tmp uint32
	if want {
		tmp = g.local(t.s.vt)
		g.op(opLocalTee)
		g.uleb(uint64(tmp))
	}
	op, align := t.s.store()
	g.memarg(op, align)
	if want {
		g.op(opLocalGet)
		g.uleb(uint64(tmp))
	}
}

// load pushes the value of the lvalue e.
func (g *fgen) load(e cc.ExpressionNode) error {
	t, err := g.lvalue(e)
	if err != nil {
		return err
	}
	if t.local {
		return g.loadTarget(t)
	}
	if t.bf {
		if err := g.address(t); err != nil {
			return err
		}
		g.op(opDrop)
		return g.loadTarget(t)
	}
	if err := g.addr(t.e); err != nil {
		return err
	}
	op, align := t.s.load()
	g.memarg(op, align)
	return nil
}

var binaryOps = map[cc.AssignmentExpressionCase][2]byte{ // signed, unsigned
	cc.AssignmentExpressionMul: {opI32Mul, opI32Mul},
	cc.AssignmentExpressionDiv: {opI32DivS, opI32DivU},
	cc.AssignmentExpressionMod: {opI32RemS, opI32RemU},
	cc.AssignmentExpressionAdd: {opI32Add, opI32Add},
	cc.AssignmentExpressionSub: {opI32Sub, opI32Sub},
	cc.AssignmentExpressionLsh: {opI32Shl, opI32Shl},
	cc.AssignmentExpressionRsh: {opI32ShrS, opI32ShrU},
	cc.AssignmentExpressionAnd: {opI32And, opI32And},
	cc.AssignmentExpressionXor: {opI32Xor, opI32Xor},
	cc.AssignmentExpressionOr:  {opI32Or, opI32Or},
}

// assign compiles E1 = E2 and E1 op= E2 (C11 6.5.16). The two operands
// are unsequenced in C; like clang (CGExprScalar.cpp, VisitBinAssign and
// EmitCompoundAssignLValue: "Emit the RHS first") E2 is evaluated before
// E1's address and value, which a call in E2 can observe (gcc torture
// pr58943.c: x[0] |= foo(), where foo changes x[0]).
func (g *fgen) assign(x *cc.AssignmentExpression, want bool) error {
	if isAggregate(x.UnaryExpression.Type()) {
		// Only effect gets here with a structure: rvalue refuses its type,
		// aggAddr takes the value, and cc/v4 refuses op= on structures.
		return g.aggAssign(x, false)
	}
	t, err := g.lvalue(x.UnaryExpression)
	if err != nil {
		return err
	}
	if x.Case == cc.AssignmentExpressionAssign {
		from, err := g.rvalue(x.AssignmentExpression)
		if err != nil {
			return err
		}
		g.convert(from, t.s)
		if !t.local {
			v := g.local(t.s.vt)
			g.op(opLocalSet)
			g.uleb(uint64(v))
			if err := g.address(t); err != nil {
				return err
			}
			g.op(opLocalGet)
			g.uleb(uint64(v))
		}
		g.store(t, want)
		return nil
	}
	ops, ok := binaryOps[x.Case]
	if !ok {
		return unsupported(x, "assignment "+x.Case.String())
	}
	// E1 op= E2 is E1 = E1 op E2 with E1 evaluated once; the operation is
	// in the promoted type of E1 for shifts, in the common type otherwise.
	rs, ok := scalarOf(x.AssignmentExpression.Type())
	if !ok {
		return unsupported(x, "compound assignment of "+typeName(x.AssignmentExpression.Type()))
	}
	if t.s.ptr {
		// p += n, p -= n: n scaled by the size of what p points to.
		if x.Case != cc.AssignmentExpressionAdd && x.Case != cc.AssignmentExpressionSub || rs.ptr {
			return unsupported(x, "compound assignment of a pointer")
		}
		if err := g.offset(x.AssignmentExpression, t.s.elem); err != nil {
			return err
		}
		r := g.local(wasm.I32)
		g.op(opLocalSet)
		g.uleb(uint64(r))
		if err := g.address(t); err != nil {
			return err
		}
		if err := g.loadTarget(t); err != nil {
			return err
		}
		g.op(opLocalGet)
		g.uleb(uint64(r))
		g.op(pick(x.Case == cc.AssignmentExpressionAdd, opI32Add, opI32Sub))
		g.store(t, want)
		return nil
	}
	if rs.ptr {
		return unsupported(x, "compound assignment of a pointer")
	}
	var opT cc.Type
	if x.Case == cc.AssignmentExpressionLsh || x.Case == cc.AssignmentExpressionRsh {
		opT = cc.IntegerPromotion(x.UnaryExpression.Type())
	} else {
		opT = cc.UsualArithmeticConversions(x.UnaryExpression.Type(), x.AssignmentExpression.Type())
	}
	os, ok := scalarOf(opT)
	if !ok {
		return unsupported(x, "compound assignment in "+typeName(opT))
	}
	from, err := g.rvalue(x.AssignmentExpression)
	if err != nil {
		return err
	}
	g.convert(from, os)
	r := g.local(os.vt)
	g.op(opLocalSet)
	g.uleb(uint64(r))
	if err := g.address(t); err != nil {
		return err
	}
	if err := g.loadTarget(t); err != nil {
		return err
	}
	g.convert(t.s, os)
	g.op(opLocalGet)
	g.uleb(uint64(r))
	g.op(opFor(pick(os.signed, ops[0], ops[1]), os))
	g.convert(os, t.s)
	g.store(t, want)
	return nil
}

// incdec compiles ++E, --E, E++ and E--: E += 1 in the promoted type (by
// the size of the object pointed to, for a pointer), yielding the old value
// for the postfix forms.
func (g *fgen) incdec(n, e cc.ExpressionNode, inc, post, want bool) error {
	t, err := g.lvalue(e)
	if err != nil {
		return err
	}
	os, one := t.s, int64(1)
	switch {
	case t.s.ptr:
		os, one = scalar{vt: wasm.I32, bits: 32}, t.s.elem
	case os.bits < 32:
		os = intScalar
	}
	op := byte(opI32Add)
	if !inc {
		op = opI32Sub
	}
	if err := g.address(t); err != nil {
		return err
	}
	if err := g.loadTarget(t); err != nil {
		return err
	}
	var old uint32
	if post && want {
		old = g.local(t.s.vt)
		g.op(opLocalTee)
		g.uleb(uint64(old))
	}
	g.convert(t.s, os)
	g.constant(one, os)
	g.op(opFor(op, os))
	g.convert(os, t.s)
	g.store(t, want && !post)
	if post && want {
		g.op(opLocalGet)
		g.uleb(uint64(old))
	}
	_ = n
	return nil
}

// call compiles a direct call (C11 6.5.2.2) whose result, if any, is
// scalar; an aggregate result is discarded.
func (g *fgen) call(x *cc.PostfixExpression) error { return g.callAgg(x, nil) }

// callAgg compiles a direct call. With slot non-nil, the call's aggregate
// result is left in the frame at *slot.
func (g *fgen) callAgg(x *cc.PostfixExpression, slot *int64) error {
	callee := x.PostfixExpression
	for {
		if p, ok := callee.(*cc.PrimaryExpression); ok && p.Case == cc.PrimaryExpressionExpr {
			callee = p.ExpressionList
			continue
		}
		if p, ok := callee.(*cc.PostfixExpression); ok && p.Case == cc.PostfixExpressionPrimary {
			callee = p.PrimaryExpression
			continue
		}
		break
	}
	var args []cc.ExpressionNode
	for l := x.ArgumentExpressionList; l != nil; l = l.ArgumentExpressionList {
		args = append(args, l.AssignmentExpression)
	}
	d := functionDesignator(callee)
	if d == nil {
		return g.indirectCall(x, callee, args, slot)
	}
	if s, ok := absBuiltins[strings.TrimPrefix(d.Name(), "__builtin_")]; ok && len(args) == 1 {
		return g.abs(args[0], s)
	}
	if ok, err := g.vaIntrinsic(x, d.Name(), args); ok {
		return err
	}
	f, err := g.calleeFor(x, d)
	if err != nil {
		return err
	}
	var extra []cc.ExpressionNode // evaluated for their effects only
	if ft, ok := d.Type().(*cc.FunctionType); ok && unprototyped(ft) && len(f.params) == 0 && len(args) > 0 {
		// `x(){…}` called as x(0): C leaves it undefined; clang calls a
		// wrapper that drops the extra arguments (WebAssemblyFixFunction-
		// Bitcasts.cpp, createWrapper). cc/v4 gives a K&R definition
		// `f(a) int a;` its parameters, so only `()` gets here. For a
		// function of another unit, the call's arguments are its signature,
		// which is not done yet.
		if !f.defined {
			return unsupported(x, "call with arguments of a function declared without them in another unit")
		}
		args, extra = nil, args
	}
	return g.emitCall(x, &f.sig, args, extra, slot, func() {
		g.op(opCall)
		g.cur.calls = append(g.cur.calls, callSite{offset: uint32(len(g.cur.code)), callee: f})
		g.reloc(wasm.RelocFunctionIndexLEB, f.sym)
	})
}

// emitCall compiles the arguments of a call of signature f, the call itself
// (emit), and the use of its result. extra arguments are evaluated for their
// effects only.
func (g *fgen) emitCall(x *cc.PostfixExpression, f *sig, args, extra []cc.ExpressionNode, slot *int64, emit func()) error {
	if len(args) < len(f.params) || !f.variadic && len(args) != len(f.params) {
		return unsupported(x, "call with a different number of arguments than parameters")
	}
	var result int64
	if f.sret != nil {
		// The caller provides the result's memory, its address first.
		result = g.frameSlot(f.sret.Size(), f.sret.Align())
		g.slotAddress(result)
	}
	for i, a := range args[:len(f.params)] {
		p := f.params[i]
		switch {
		case p.pass == passIgnored:
			if err := g.effect(a); err != nil {
				return err
			}
		case p.pass == passIndirect:
			// A copy the callee may change (BasicCABI.md).
			tmp := g.frameSlot(p.t.Size(), p.t.Align())
			g.slotAddress(tmp)
			if err := g.aggAddr(a); err != nil {
				return err
			}
			g.constant(p.t.Size(), intScalar)
			g.op(0xfc, 10, 0, 0) // memory.copy
			g.slotAddress(tmp)
		case isAggregate(p.t):
			// A singleton: its one scalar, loaded.
			if err := g.aggAddr(a); err != nil {
				return err
			}
			op, align := p.s.load()
			g.memarg(op, align)
		default:
			from, err := g.rvalue(a)
			if err != nil {
				return err
			}
			g.convert(from, p.s)
		}
	}
	if f.variadic {
		if err := g.varargs(args[len(f.params):]); err != nil {
			return err
		}
	}
	for _, a := range extra {
		if err := g.effect(a); err != nil {
			return err
		}
	}
	emit()
	if isAggregate(f.rtype) {
		if f.result != nil {
			// A singleton result: store it to memory, to give it an address.
			v := g.local(f.result.vt)
			g.op(opLocalSet)
			g.uleb(uint64(v))
			result = g.frameSlot(f.rtype.Size(), f.rtype.Align())
			g.slotAddress(result)
			g.op(opLocalGet)
			g.uleb(uint64(v))
			op, align := f.result.store()
			g.memarg(op, align)
		}
		if slot != nil {
			*slot = result
		}
		return nil
	}
	switch want := x.Type(); {
	case f.result == nil && !isVoid(want):
		// An implicit `int abort()` used for its value: the library
		// function returns nothing; give the expression one.
		s, ok := scalarOf(want)
		if !ok {
			return unsupported(x, "call of type "+typeName(want))
		}
		g.zero(s)
	case f.result != nil && isVoid(want):
		g.op(opDrop)
	case f.result != nil:
		s, ok := scalarOf(want)
		if !ok {
			return unsupported(x, "call of type "+typeName(want))
		}
		g.convert(*f.result, s)
	}
	return nil
}

// libc are the signatures clang gives the library functions a K&R program
// calls without declaring them (clang: "implicitly declaring library
// function 'exit' with type 'void (int)'"). A header's declaration gives
// the same signature. Without them, the implicit `int f()` would not
// match wasi-libc's definition at link time.
var libc = map[string][]scalar{
	"abort": nil,
	"exit":  {intScalar},
	"_exit": {intScalar},
	"_Exit": {intScalar},
}

var intScalar = scalar{vt: wasm.I32, bits: 32, signed: true}

// absBuiltins are the library functions clang computes itself, even when
// the unit defines a function of the same name (gcc torture 20021127-1.c:
// a user llabs that aborts is never called).
var absBuiltins = map[string]scalar{
	"abs":   intScalar,
	"labs":  intScalar, // long is 32 bits on wasm32
	"llabs": {vt: wasm.I64, bits: 64, signed: true},
}

// abs pushes |a| in s: select(-t, t, t < 0).
func (g *fgen) abs(a cc.ExpressionNode, s scalar) error {
	from, err := g.rvalue(a)
	if err != nil {
		return err
	}
	g.convert(from, s)
	t := g.local(s.vt)
	g.op(opLocalSet)
	g.uleb(uint64(t))
	g.zero(s)
	g.op(opLocalGet)
	g.uleb(uint64(t))
	g.op(opFor(opI32Sub, s), opLocalGet)
	g.uleb(uint64(t))
	g.op(opLocalGet)
	g.uleb(uint64(t))
	g.zero(s)
	lt := byte(opI32LtS)
	if s.vt == wasm.I64 {
		lt += i64CompareDelta
	}
	g.op(lt, opSelect)
	return nil
}

// calleeFor returns the function a call goes to: one defined or already
// imported here, a library function with its library signature, or else an
// import with the signature of d's declaration.
func (g *fgen) calleeFor(n cc.Node, d *cc.Declarator) (*funcSym, error) {
	name := d.Name()
	if d.Position().Filename == "<builtin>" {
		// An undeclared library function resolves to cc/v4's builtin
		// declaration (`exit` to `__builtin_exit`); clang calls the
		// library function. Other builtins are intrinsics, not calls.
		lib, ok := strings.CutPrefix(name, "__builtin_")
		_, known := libc[lib]
		if !ok || !known && !libcAsDeclared[lib] {
			return nil, unsupported(n, "builtin "+name)
		}
		name = lib
	} else if strings.HasPrefix(name, "__builtin_") {
		// A builtin cc/v4 does not declare (__builtin_return_address) is
		// implicitly `int f()`; it is an intrinsic, not a library call.
		return nil, unsupported(n, "builtin "+name)
	}
	if f := g.m.funcs[name]; f != nil {
		return f, nil
	}
	if ps, ok := libc[name]; ok {
		return g.m.importFunc(name, scalarSig(ps, nil)), nil
	}
	return g.m.callee(n, name, d)
}

// libcAsDeclared are the library functions whose cc/v4 builtin declaration
// has the signature of wasi-libc's definition, so that an undeclared call
// goes to the library, as clang's does.
var libcAsDeclared = map[string]bool{
	// internal/cc/cc.go declares each with the C standard's prototype,
	// which is wasi-libc's (size_t is __SIZE_TYPE__, unsigned long, i32).
	// clang may expand a memcpy or memset of constant size inline; the
	// call has the same effect.
	"printf": true, "puts": true, "putchar": true,
	"malloc": true, "free": true,
	"memcpy": true, "memset": true, "memcmp": true, "memchr": true,
	"strlen": true, "strcmp": true, "strncmp": true, "strcpy": true, "strncpy": true,
	"strcat": true, "strncat": true, "strchr": true, "strrchr": true, "strstr": true,
	"strpbrk": true, "strspn": true, "strcspn": true,
}
