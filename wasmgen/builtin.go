package wasmgen

import (
	"strconv"
	"strings"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// GCC builtins that are computations, not library calls (GCC manual,
// "Other Builtins", "Integer Overflow Builtins"). Each compiles to what
// clang 23.1.4 emits for wasm32 at -O0 (clang -S of a probe).

// More opcodes (core spec, binary/instructions.html).
const (
	opI32Clz   = 0x67 // ctz popcnt follow
	opI64Clz   = 0x79 // ctz popcnt follow
	opI64Ne    = 0x52
	opI64LtU   = 0x54
	opI64Add   = 0x7c
	opI64Sub   = 0x7d
	opI64Mul   = 0x7e
	opI64And   = 0x83
	opI64Or    = 0x84
	opI64ShrS  = 0x87
	opI64Ext8S = 0xc2 // extend16_s extend32_s follow
)

var (
	s64 = scalar{vt: wasm.I64, bits: 64, signed: true}
	u32 = scalar{vt: wasm.I32, bits: 32}
	u64 = scalar{vt: wasm.I64, bits: 64}
)

// builtin compiles the call x of the builtin name, when it is one compiled
// here; ok is false otherwise.
func (g *fgen) builtin(x *cc.PostfixExpression, name string, args []cc.ExpressionNode) (ok bool, err error) {
	b, isBuiltin := strings.CutPrefix(name, "__builtin_")
	if !isBuiltin {
		return false, nil
	}
	arity := func(n int) error {
		if len(args) != n {
			return unsupported(x, name+" with "+strconv.Itoa(len(args))+" arguments")
		}
		return nil
	}
	switch b {
	case "expect":
		// The value of its first argument (GCC manual).
		if err := arity(2); err != nil {
			return true, err
		}
		from, err := g.rvalue(args[0])
		if err != nil {
			return true, err
		}
		g.convert(from, intScalar) // long, 32 bits on wasm32
		if err := g.effect(args[1]); err != nil {
			return true, err
		}
		return true, g.builtinResult(x, intScalar)
	case "prefetch":
		// A hint: its arguments are evaluated, nothing else is done.
		for _, a := range args {
			if err := g.effect(a); err != nil {
				return true, err
			}
		}
		return true, g.builtinResult(x, scalar{})
	case "unreachable", "trap":
		g.op(opUnreachable)
		return true, g.builtinResult(x, scalar{})
	case "alloca":
		if err := arity(1); err != nil {
			return true, err
		}
		return true, g.alloca(x, args[0])
	case "clz", "ctz", "popcount", "clzl", "ctzl", "popcountl", "clzll", "ctzll", "popcountll":
		if err := arity(1); err != nil {
			return true, err
		}
		return true, g.bitCount(x, b, args[0])
	case "isunordered", "isgreater", "isgreaterequal", "isless", "islessequal", "islessgreater":
		if err := arity(2); err != nil {
			return true, err
		}
		return true, g.unorderedCompare(x, b, args)
	case "add_overflow", "sub_overflow", "mul_overflow":
		// Not the _p forms: clang 23.1.4 does not know them.
		if err := arity(3); err != nil {
			return true, err
		}
		return true, g.overflow(x, b, args)
	}
	return false, nil
}

// builtinResult gives the call x its value from the value of s on the stack, or
// none when s is the zero scalar (nothing on the stack).
func (g *fgen) builtinResult(x *cc.PostfixExpression, s scalar) error {
	want := x.Type()
	switch {
	case isVoid(want) && s.vt == 0:
	case isVoid(want):
		g.op(opDrop)
	case s.vt == 0:
		// `int __builtin_trap()`, implicitly declared, after unreachable.
		ws, ok := scalarOf(want)
		if !ok {
			return unsupported(x, "call of type "+typeName(want))
		}
		g.zero(ws)
	default:
		ws, ok := scalarOf(want)
		if !ok {
			return unsupported(x, "call of type "+typeName(want))
		}
		g.convert(s, ws)
	}
	return nil
}

// alloca moves __stack_pointer down by size rounded up to 16, as clang
// does, and returns it. The epilogue restores __stack_pointer from the
// value it had on entry, which frees the memory when the function returns;
// the frame is addressed from its own base, not from __stack_pointer.
func (g *fgen) alloca(x *cc.PostfixExpression, size cc.ExpressionNode) error {
	from, err := g.rvalue(size)
	if err != nil {
		return err
	}
	g.convert(from, u32)
	n := g.local(wasm.I32)
	g.op(opLocalSet)
	g.uleb(uint64(n))
	sp := g.m.stackPointer()
	g.op(opGlobalGet)
	g.reloc(wasm.RelocGlobalIndexLEB, sp)
	g.op(opLocalGet)
	g.uleb(uint64(n))
	g.constant(15, intScalar)
	g.op(opI32Add)
	g.constant(-16, intScalar)
	g.op(opI32And, opI32Sub)
	p := g.local(wasm.I32)
	g.op(opLocalTee)
	g.uleb(uint64(p))
	g.op(opGlobalSet)
	g.reloc(wasm.RelocGlobalIndexLEB, sp)
	g.op(opLocalGet)
	g.uleb(uint64(p))
	return g.builtinResult(x, scalar{vt: wasm.I32, bits: 32, ptr: true, elem: 1})
}

// bitCount compiles clz, ctz and popcount: i32.clz and its neighbours, or
// the i64 ones for the ll forms. clz(0) and ctz(0) are undefined; wasm
// gives the width, as clang's code does.
func (g *fgen) bitCount(x *cc.PostfixExpression, b string, a cc.ExpressionNode) error {
	from, err := g.rvalue(a)
	if err != nil {
		return err
	}
	if from.isFloat {
		return unsupported(x, "__builtin_"+b+" of a floating value")
	}
	kind := byte(0)
	switch {
	case strings.HasPrefix(b, "ctz"):
		kind = 1
	case strings.HasPrefix(b, "popcount"):
		kind = 2
	}
	if strings.HasSuffix(b, "ll") {
		g.convert(from, u64)
		g.op(opI64Clz+kind, opI32WrapI64)
	} else {
		g.convert(from, u32)
		g.op(opI32Clz + kind)
	}
	return g.builtinResult(x, intScalar)
}

// unorderedCompare compiles the quiet comparisons of C11 7.12.14 in the
// common real type of the operands (C11 6.3.1.8). wasm's comparisons do
// not raise on a NaN; isunordered is a != a | b != b.
func (g *fgen) unorderedCompare(x *cc.PostfixExpression, b string, args []cc.ExpressionNode) error {
	var ops [2]uint32
	var froms [2]scalar
	common := scalar{vt: wasm.F32, bits: 32, signed: true, isFloat: true}
	for i, a := range args {
		s, ok := scalarOf(a.Type())
		if !ok {
			return unsupported(x, "__builtin_"+b+" of "+typeName(a.Type()))
		}
		froms[i] = s
		if s.vt == wasm.F64 || !s.isFloat && s.bits == 64 {
			common = f64Scalar
		}
	}
	if !froms[0].isFloat && !froms[1].isFloat {
		return unsupported(x, "__builtin_"+b+" of two integers")
	}
	for i, a := range args {
		from, err := g.rvalue(a)
		if err != nil {
			return err
		}
		g.convert(from, common)
		ops[i] = g.local(common.vt)
		g.op(opLocalSet)
		g.uleb(uint64(ops[i]))
	}
	delta := byte(0)
	if common.vt == wasm.F64 {
		delta = 6
	}
	get := func(i int) {
		g.op(opLocalGet)
		g.uleb(uint64(ops[i]))
	}
	rel := func(i, j int, op byte) {
		get(i)
		get(j)
		g.op(op + delta)
	}
	const ne, lt, gt, le, ge = opF32Eq + 1, opF32Eq + 2, opF32Eq + 3, opF32Eq + 4, opF32Eq + 5
	switch b {
	case "isunordered":
		rel(0, 0, ne)
		rel(1, 1, ne)
		g.op(opI32Or)
	case "isgreater":
		rel(0, 1, gt)
	case "isgreaterequal":
		rel(0, 1, ge)
	case "isless":
		rel(0, 1, lt)
	case "islessequal":
		rel(0, 1, le)
	case "islessgreater":
		rel(0, 1, lt)
		rel(0, 1, gt)
		g.op(opI32Or)
	}
	return g.builtinResult(x, intScalar)
}

// overflow compiles __builtin_{add,sub,mul}_overflow(a, b, r): the
// operation in infinite precision, its value converted to *r's type and
// stored, and whether the two differ (GCC manual, "Integer Overflow
// Builtins").
//
// The exact value is computed in 128 bits, hi:lo, from the operands
// extended by their own signedness. A sum or difference of two 64-bit
// integers fits, and so does a product, as a signed value, except
// unsigned × unsigned, which fits as an unsigned one; the test below is
// right in both readings, since a high half of such a product is never
// all ones. The product of two 64-bit operands is compiler-rt's __multi3,
// as clang's is; that of two narrower ones is one i64.mul.
func (g *fgen) overflow(x *cc.PostfixExpression, b string, args []cc.ExpressionNode) error {
	op := strings.TrimSuffix(b, "_overflow")
	p, ok := args[2].Type().(*cc.PointerType)
	if !ok {
		return unsupported(x, "__builtin_"+b+" whose third argument is not a pointer")
	}
	rt := p.Elem()
	rs, ok := scalarOf(rt)
	if !ok || rs.isFloat || rs.ptr || rs.isBool || rt.Kind() == cc.Enum {
		return unsupported(x, "__builtin_"+b+" of a result of type "+typeName(rt))
	}
	var lo, hi [2]uint32
	var unsigned [2]bool
	wide := false
	for i, a := range args[:2] {
		from, err := g.rvalue(a)
		if err != nil {
			return err
		}
		if from.isFloat || from.ptr {
			return unsupported(x, "__builtin_"+b+" of "+typeName(a.Type()))
		}
		wide = wide || from.vt == wasm.I64
		unsigned[i] = !from.signed || from.isBool
		g.convert(from, scalar{vt: wasm.I64, bits: 64, signed: from.signed && !from.isBool})
		lo[i], hi[i] = g.local(wasm.I64), g.local(wasm.I64)
		g.op(opLocalTee)
		g.uleb(uint64(lo[i]))
		if unsigned[i] {
			g.op(opDrop)
			g.constant(0, s64)
		} else {
			g.constant(63, s64)
			g.op(opI64ShrS)
		}
		g.op(opLocalSet)
		g.uleb(uint64(hi[i]))
	}
	from, err := g.rvalue(args[2])
	if err != nil {
		return err
	}
	g.convert(from, u32)
	ptr := g.local(wasm.I32)
	g.op(opLocalSet)
	g.uleb(uint64(ptr))
	get := func(l uint32) {
		g.op(opLocalGet)
		g.uleb(uint64(l))
	}
	set := func(l uint32) {
		g.op(opLocalSet)
		g.uleb(uint64(l))
	}
	rlo, rhi := g.local(wasm.I64), g.local(wasm.I64)
	switch {
	case op == "add" || op == "sub":
		arith := byte(opI64Add)
		if op == "sub" {
			arith = opI64Sub
		}
		get(lo[0])
		get(lo[1])
		g.op(arith)
		set(rlo)
		// The carry out of the low half: lo < a.lo for a sum, a.lo < b.lo
		// (the borrow) for a difference.
		get(hi[0])
		get(hi[1])
		g.op(arith)
		if op == "add" {
			get(rlo)
			get(lo[0])
		} else {
			get(lo[0])
			get(lo[1])
		}
		g.op(opI64LtU, opI64ExtendI32U, arith)
		set(rhi)
	case !wide:
		get(lo[0])
		get(lo[1])
		g.op(opI64Mul)
		g.op(opLocalTee)
		g.uleb(uint64(rlo))
		if unsigned[0] && unsigned[1] {
			g.op(opDrop)
			g.constant(0, s64)
		} else {
			g.constant(63, s64)
			g.op(opI64ShrS)
		}
		set(rhi)
	default:
		// __multi3(&r, a.lo, a.hi, b.lo, b.hi) (BasicCABI.md: a 128-bit
		// integer is two i64, low first; its result is returned through
		// memory).
		slot := g.frameSlot(16, 16)
		g.slotAddress(slot)
		for i := range 2 {
			get(lo[i])
			get(hi[i])
		}
		f := g.m.funcs["__multi3"]
		if f == nil {
			f = g.m.importFunc("__multi3", scalarSig([]scalar{u32, u64, u64, u64, u64}, nil))
		}
		g.op(opCall)
		g.cur.calls = append(g.cur.calls, callSite{offset: uint32(len(g.cur.code)), callee: f})
		g.reloc(wasm.RelocFunctionIndexLEB, f.sym)
		g.slotAddress(slot)
		g.memarg(opI64Load, 3)
		set(rlo)
		g.slotAddress(slot + 8)
		g.memarg(opI64Load, 3)
		set(rhi)
	}
	// r: the low rs.bits of the result, extended as rs is.
	r := g.local(wasm.I64)
	get(rlo)
	switch {
	case rs.bits == 64:
	case rs.signed && rs.bits == 8:
		g.op(opI64Ext8S)
	case rs.signed && rs.bits == 16:
		g.op(opI64Ext8S + 1)
	case rs.signed:
		g.op(opI64Ext8S + 2)
	default:
		g.constant(int64(1)<<rs.bits-1, s64)
		g.op(opI64And)
	}
	set(r)
	get(ptr)
	get(r)
	g.convert(s64, rs)
	st, align := rs.store()
	g.memarg(st, align)
	// Overflow: hi:lo differs from r extended to 128 bits.
	get(rhi)
	if rs.signed {
		get(r)
		g.constant(63, s64)
		g.op(opI64ShrS)
	} else {
		g.constant(0, s64)
	}
	g.op(opI64Ne)
	get(rlo)
	get(r)
	g.op(opI64Ne, opI32Or)
	return g.builtinResult(x, intScalar)
}
