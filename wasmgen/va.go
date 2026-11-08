package wasmgen

import (
	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// Variable arguments (BasicCABI.md, "Varargs are placed in a buffer by the
// caller and the last parameter to the function is a pointer to that
// buffer"). The layout is the one LLVM's WebAssembly backend gives the
// buffer (WebAssemblyISelLowering.cpp, LowerCall: each argument at the
// next offset aligned to its type, the buffer 16-aligned in the caller's
// frame, a null pointer when there is none), and va_arg reads it as clang
// does (Targets/WebAssembly.cpp, EmitVAArg → emitVoidPtrVAArg: 4-byte
// slots, aligned up for a wider type, a pointer for an indirect aggregate).

// vararg is one argument matched by `...`, once promoted (C11 6.5.2.2p7).
type vararg struct {
	e    cc.ExpressionNode
	pass passing // passDirect: s; passIndirect: a pointer to a copy of t
	s    scalar
	t    cc.Type
	off  int64
}

// varargs stores the arguments args into a buffer in the frame and pushes
// its address, the last wasm argument of a variadic call.
func (g *fgen) varargs(args []cc.ExpressionNode) error {
	var vs []vararg
	var size int64
	for _, a := range args {
		v := vararg{e: a, t: a.Type()}
		switch {
		case isAggregate(v.t):
			pass, s, ok := abiOf(v.t)
			if !ok {
				return unsupported(a, "variadic argument of type "+typeName(v.t))
			}
			if pass == passIgnored {
				continue // an empty structure takes no room (clang: ignored)
			}
			if pass == passDirect && s.bits < 32 {
				return unsupported(a, "variadic argument of a narrow singleton type")
			}
			v.pass, v.s = pass, s
			if pass == passIndirect {
				v.s = scalar{vt: wasm.I32, bits: 32, ptr: true}
			}
		default:
			s, ok := promoted(a)
			if !ok {
				return unsupported(a, "variadic argument of type "+typeName(v.t))
			}
			v.s = s
		}
		align := int64(max(4, v.s.bits/8))
		v.off = (size + align - 1) &^ (align - 1)
		size = v.off + int64(v.s.bits/8)
		vs = append(vs, v)
	}
	if len(vs) == 0 {
		g.constant(0, intScalar)
		return nil
	}
	buf := g.frameSlot((size+15)&^15, 16)
	for _, v := range vs {
		var tmp int64
		if v.pass == passIndirect {
			// A copy the callee may change (BasicCABI.md).
			tmp = g.frameSlot(v.t.Size(), v.t.Align())
			g.slotAddress(tmp)
			if err := g.aggAddr(v.e); err != nil {
				return err
			}
			g.constant(v.t.Size(), intScalar)
			g.op(0xfc, 10, 0, 0) // memory.copy
		}
		g.slotAddress(buf + v.off)
		switch {
		case v.pass == passIndirect:
			g.slotAddress(tmp)
		case isAggregate(v.t):
			if err := g.aggAddr(v.e); err != nil {
				return err
			}
			op, align := v.s.load()
			g.memarg(op, align)
		default:
			from, err := g.rvalue(v.e)
			if err != nil {
				return err
			}
			g.convert(from, v.s)
		}
		op, align := v.s.store()
		g.memarg(op, align)
	}
	g.slotAddress(buf)
	return nil
}

// unprototyped reports whether ft is declared without a prototype, `f()`:
// cc/v4 gives it no parameters and no maximum, without `...`.
func unprototyped(ft *cc.FunctionType) bool {
	return !ft.IsVariadic() && ft.MaxArgs() < 0
}

// promoted returns the representation of e after the default argument
// promotions (C11 6.5.2.2p6), for a scalar; floats come with W5.
func promoted(e cc.ExpressionNode) (scalar, bool) {
	t := e.Type()
	if t.Kind() == cc.Array || designatesArray(e) {
		return scalarOf(t.Decay())
	}
	if cc.IsIntegerType(t) {
		t = cc.IntegerPromotion(t)
	}
	return scalarOf(t)
}

// vaIntrinsic compiles __builtin_va_start and __builtin_va_end, which
// cc/v4 leaves as calls; it reports whether name is one of them.
func (g *fgen) vaIntrinsic(x *cc.PostfixExpression, name string, args []cc.ExpressionNode) (bool, error) {
	switch name {
	case "__builtin_va_start":
		if !g.variadic {
			return true, unsupported(x, "va_start outside a variadic function")
		}
		if len(args) != 2 {
			return true, unsupported(x, "va_start without two arguments")
		}
		t, err := g.lvalue(args[0])
		if err != nil {
			return true, err
		}
		if err := g.address(t); err != nil {
			return true, err
		}
		g.op(opLocalGet)
		g.uleb(uint64(g.va))
		g.store(t, false)
	case "__builtin_va_end":
		if len(args) != 1 {
			return true, unsupported(x, "va_end without one argument")
		}
		if err := g.effect(args[0]); err != nil {
			return true, err
		}
	default:
		return false, nil
	}
	// Undeclared, they are `int f()` to cc/v4; a value may be expected.
	if !isVoid(x.Type()) {
		s, ok := scalarOf(x.Type())
		if !ok {
			return true, unsupported(x, "call of type "+typeName(x.Type()))
		}
		g.zero(s)
	}
	return true, nil
}

// vaArgOf recognises cc/v4's expansion of va_arg(ap, T),
// `*(T*)__builtin_va_arg_impl(ap)`, in the operand of a `*`.
func vaArgOf(operand cc.ExpressionNode) (ap cc.ExpressionNode, t cc.Type, ok bool) {
	c, ok := strip(operand).(*cc.CastExpression)
	if !ok || c.Case != cc.CastExpressionCast {
		return nil, nil, false
	}
	call, ok := strip(c.CastExpression).(*cc.PostfixExpression)
	if !ok || call.Case != cc.PostfixExpressionCall {
		return nil, nil, false
	}
	p, ok := strip(call.PostfixExpression).(*cc.PrimaryExpression)
	if !ok || p.Case != cc.PrimaryExpressionIdent || p.Token.SrcStr() != "__builtin_va_arg_impl" {
		return nil, nil, false
	}
	l := call.ArgumentExpressionList
	if l == nil || l.ArgumentExpressionList != nil {
		return nil, nil, false
	}
	pt, ok := c.Type().(*cc.PointerType)
	if !ok {
		return nil, nil, false
	}
	return l.AssignmentExpression, pt.Elem(), true
}

// vaArg pushes the address of the next argument of type t in the buffer ap
// points into, and advances ap past it.
func (g *fgen) vaArg(n cc.Node, ap cc.ExpressionNode, t cc.Type) error {
	if t.IsIncomplete() || t.Size() < 0 {
		return unsupported(n, "va_arg of an incomplete type")
	}
	size, align := t.Size(), int64(t.Align())
	indirect := false
	if isAggregate(t) {
		pass, _, ok := abiOf(t)
		if !ok {
			return unsupported(n, "va_arg of type "+typeName(t))
		}
		if pass == passIndirect {
			indirect, size, align = true, 4, 4
		}
	}
	tg, err := g.lvalue(ap)
	if err != nil {
		return err
	}
	if !tg.s.ptr {
		return unsupported(n, "va_arg of a va_list that is not a pointer")
	}
	if err := g.address(tg); err != nil {
		return err
	}
	if err := g.loadTarget(tg); err != nil {
		return err
	}
	if align > 4 {
		g.constant(align-1, intScalar)
		g.op(opI32Add)
		g.constant(-align, intScalar)
		g.op(opI32And)
	}
	p := g.local(wasm.I32)
	g.op(opLocalTee)
	g.uleb(uint64(p))
	g.constant((size+3)&^3, intScalar)
	g.op(opI32Add)
	g.store(tg, false)
	g.op(opLocalGet)
	g.uleb(uint64(p))
	if indirect {
		g.memarg(opI32Load, 2)
	}
	return nil
}
