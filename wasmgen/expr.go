package wasmgen

import (
	"strings"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// intValue returns the value of a constant integer expression.
func intValue(e cc.ExpressionNode) (int64, bool) {
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
	v = convertConst(v, s)
	if s.vt == wasm.I64 {
		g.op(opI64Const)
		g.body = wasm.AppendSLEB(g.body, v)
		return
	}
	g.op(opI32Const)
	g.body = wasm.AppendSLEB(g.body, int64(int32(uint32(v))))
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
	s, ok := scalarOf(t)
	if !ok {
		return scalar{}, unsupported(e, "value of type "+typeName(t))
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
		return s, unsupported(x, "?: (W3)")
	case *cc.LogicalOrExpression:
		if x.Case == cc.LogicalOrExpressionLAnd {
			return g.rvalue(x.LogicalAndExpression)
		}
		return s, unsupported(x, "|| (W3)")
	case *cc.LogicalAndExpression:
		if x.Case == cc.LogicalAndExpressionOr {
			return g.rvalue(x.InclusiveOrExpression)
		}
		return s, unsupported(x, "&& (W3)")
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
		return s, g.binary(x.AdditiveExpression, x.MultiplicativeExpression, s, op)
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
		}
		return s, unsupported(x, "postfix "+x.Case.String()+" (W4)")
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
		if from.vt == wasm.I64 {
			g.op(opI64Eqz)
		} else {
			g.op(opI32Eqz)
		}
		return nil
	case cc.UnaryExpressionInc, cc.UnaryExpressionDec:
		return g.incdec(x, x.UnaryExpression, x.Case == cc.UnaryExpressionInc, false, true)
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
			if isVoid(x.Type()) {
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
	}
	if e.Pure() {
		return nil
	}
	if _, err := g.rvalue(e); err != nil {
		return err
	}
	g.op(opDrop)
	return nil
}

// target is an lvalue W2 can store to: a local, or a file-scope object.
type target struct {
	local  bool
	index  uint32
	global *dataSym
	s      scalar
}

func (g *fgen) lvalue(e cc.ExpressionNode) (target, error) {
	for {
		switch x := e.(type) {
		case *cc.PrimaryExpression:
			if x.Case == cc.PrimaryExpressionExpr {
				e = x.ExpressionList
				continue
			}
			if x.Case != cc.PrimaryExpressionIdent {
				break
			}
			d, ok := x.ResolvedTo().(*cc.Declarator)
			if !ok {
				break
			}
			s, ok := scalarOf(d.Type())
			if !ok {
				return target{}, unsupported(x, "object of type "+typeName(d.Type()))
			}
			if i, ok := g.vars[d]; ok {
				return target{local: true, index: i, s: s}, nil
			}
			if d.StorageDuration() != cc.Static {
				return target{}, unsupported(x, "object without a local")
			}
			ds, err := g.m.global(x, d)
			if err != nil {
				return target{}, err
			}
			return target{global: ds, s: s}, nil
		case *cc.PostfixExpression:
			if x.Case == cc.PostfixExpressionPrimary {
				e = x.PrimaryExpression
				continue
			}
		case *cc.ExpressionList:
			if x.ExpressionList == nil {
				e = x.AssignmentExpression
				continue
			}
		}
		return target{}, unsupported(e, "lvalue (W4)")
	}
}

// memarg appends the alignment and a relocated offset addressing the
// object of t (the address is the constant 0 pushed before).
func (g *fgen) memarg(t target, op byte, align uint32) {
	g.op(op)
	g.uleb(uint64(align))
	g.relocs = append(g.relocs, wasm.Reloc{Type: wasm.RelocMemoryAddrLEB, Offset: uint32(len(g.body)), Symbol: t.global.sym})
	g.body = wasm.AppendPaddedULEB(g.body, 0)
}

func (g *fgen) load(x *cc.PrimaryExpression) error {
	t, err := g.lvalue(x)
	if err != nil {
		return err
	}
	if t.local {
		g.op(opLocalGet)
		g.uleb(uint64(t.index))
		return nil
	}
	g.op(opI32Const, 0)
	op, align := t.s.load()
	g.memarg(t, op, align)
	return nil
}

// store stores the value on the stack into t, leaving a copy when want. For
// a global, the caller pushed the address (i32.const 0) before the value.
func (g *fgen) store(t target, want bool) {
	if t.local {
		g.op(pick(want, opLocalTee, opLocalSet))
		g.uleb(uint64(t.index))
		return
	}
	var tmp uint32
	if want {
		tmp = g.local(t.s.vt)
		g.op(opLocalTee)
		g.uleb(uint64(tmp))
	}
	op, align := t.s.store()
	g.memarg(t, op, align)
	if want {
		g.op(opLocalGet)
		g.uleb(uint64(tmp))
	}
}

func (g *fgen) address(t target) {
	if !t.local {
		g.op(opI32Const, 0)
	}
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

// assign compiles E1 = E2 and E1 op= E2 (C11 6.5.16).
func (g *fgen) assign(x *cc.AssignmentExpression, want bool) error {
	t, err := g.lvalue(x.UnaryExpression)
	if err != nil {
		return err
	}
	g.address(t)
	if x.Case == cc.AssignmentExpressionAssign {
		from, err := g.rvalue(x.AssignmentExpression)
		if err != nil {
			return err
		}
		g.convert(from, t.s)
		g.store(t, want)
		return nil
	}
	ops, ok := binaryOps[x.Case]
	if !ok {
		return unsupported(x, "assignment "+x.Case.String())
	}
	// E1 op= E2 is E1 = E1 op E2 with E1 evaluated once; the operation is
	// in the promoted type of E1 for shifts, in the common type otherwise.
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
	if err := g.loadTarget(t); err != nil {
		return err
	}
	g.convert(t.s, os)
	from, err := g.rvalue(x.AssignmentExpression)
	if err != nil {
		return err
	}
	g.convert(from, os)
	g.op(opFor(pick(os.signed, ops[0], ops[1]), os))
	g.convert(os, t.s)
	g.store(t, want)
	return nil
}

func (g *fgen) loadTarget(t target) error {
	if t.local {
		g.op(opLocalGet)
		g.uleb(uint64(t.index))
		return nil
	}
	g.op(opI32Const, 0)
	op, align := t.s.load()
	g.memarg(t, op, align)
	return nil
}

// incdec compiles ++E, --E, E++ and E--: E += 1 in the promoted type,
// yielding the old value for the postfix forms.
func (g *fgen) incdec(n, e cc.ExpressionNode, inc, post, want bool) error {
	t, err := g.lvalue(e)
	if err != nil {
		return err
	}
	os := t.s
	if os.bits < 32 {
		os = scalar{vt: wasm.I32, bits: 32, signed: true}
	}
	op := byte(opI32Add)
	if !inc {
		op = opI32Sub
	}
	step := func() {
		g.convert(t.s, os)
		g.constant(1, os)
		g.op(opFor(op, os))
		g.convert(os, t.s)
	}
	if !post || !want {
		g.address(t)
		if err := g.loadTarget(t); err != nil {
			return err
		}
		step()
		g.store(t, want)
		return nil
	}
	old := g.local(t.s.vt)
	if err := g.loadTarget(t); err != nil {
		return err
	}
	g.op(opLocalSet)
	g.uleb(uint64(old))
	g.address(t)
	g.op(opLocalGet)
	g.uleb(uint64(old))
	step()
	g.store(t, false)
	g.op(opLocalGet)
	g.uleb(uint64(old))
	_ = n
	return nil
}

// call compiles a direct call (C11 6.5.2.2).
func (g *fgen) call(x *cc.PostfixExpression) error {
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
	p, ok := callee.(*cc.PrimaryExpression)
	if !ok || p.Case != cc.PrimaryExpressionIdent {
		return unsupported(x, "indirect call (W4)")
	}
	d, ok := p.ResolvedTo().(*cc.Declarator)
	if !ok || d.Type().Kind() != cc.Function {
		return unsupported(x, "call through a pointer (W4)")
	}
	var args []cc.ExpressionNode
	for l := x.ArgumentExpressionList; l != nil; l = l.ArgumentExpressionList {
		args = append(args, l.AssignmentExpression)
	}
	f, err := g.calleeFor(x, d)
	if err != nil {
		return err
	}
	if len(f.params) != len(args) {
		return unsupported(x, "call with a different number of arguments than parameters")
	}
	for i, a := range args {
		from, err := g.rvalue(a)
		if err != nil {
			return err
		}
		g.convert(from, f.params[i])
	}
	g.op(opCall)
	g.m.calls = append(g.m.calls, callSite{fn: g.fn, offset: uint32(len(g.body)), callee: f})
	g.relocs = append(g.relocs, wasm.Reloc{Type: wasm.RelocFunctionIndexLEB, Offset: uint32(len(g.body)), Symbol: f.sym})
	g.body = wasm.AppendPaddedULEB(g.body, 0)
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
		if _, known := libc[lib]; !ok || !known {
			return nil, unsupported(n, "builtin "+name)
		}
		name = lib
	}
	if f := g.m.funcs[name]; f != nil {
		return f, nil
	}
	if ps, ok := libc[name]; ok {
		return g.m.importFunc(name, ps, nil), nil
	}
	return g.m.callee(n, d)
}
