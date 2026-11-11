package wasmgen

import (
	"encoding/binary"
	"math"
	"strings"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// The floating-point and byte builtins (GCC manual, "Other Builtins"),
// each what clang 23.1.4 emits for wasm32 at -O0.

const (
	opF32Copysign    = 0x98
	opF64Copysign    = 0xa6
	opI32ReinterpF32 = 0xbc
	opI64ReinterpF64 = 0xbd
	opI64Shl         = 0x86
	opI64ShrU        = 0x88
)

// floatOf returns the floating type a builtin works in from its suffix:
// float for f, double for none; long double is refused.
func floatOf(x *cc.PostfixExpression, b, base string) (scalar, error) {
	switch strings.TrimPrefix(b, base) {
	case "":
		return f64Scalar, nil
	case "f":
		return f32Scalar, nil
	}
	return scalar{}, unsupported(x, "__builtin_"+b)
}

// fabs and copysign: f64.abs and f64.copysign, or their f32 forms.
func (g *fgen) fabsCopysign(x *cc.PostfixExpression, b string, args []cc.ExpressionNode) error {
	base, n, op := "fabs", 1, byte(opF64Abs)
	if strings.HasPrefix(b, "copysign") {
		base, n, op = "copysign", 2, opF64Copysign
	}
	s, err := floatOf(x, b, base)
	if err != nil {
		return err
	}
	if len(args) != n {
		return unsupported(x, "__builtin_"+b+" with another number of arguments")
	}
	if s.vt == wasm.F32 {
		op = map[byte]byte{opF64Abs: opF32Abs, opF64Copysign: opF32Copysign}[op]
	}
	for _, a := range args {
		from, err := g.rvalue(a)
		if err != nil {
			return err
		}
		g.convert(from, s)
	}
	g.op(op)
	return g.builtinResult(x, s)
}

// infNaN compiles inf, huge_val and nan(""): constants. nan with another
// string (a payload) is refused.
func (g *fgen) infNaN(x *cc.PostfixExpression, b string, args []cc.ExpressionNode) error {
	base := "inf"
	switch {
	case strings.HasPrefix(b, "huge_val"):
		base = "huge_val"
	case strings.HasPrefix(b, "nan"):
		base = "nan"
	}
	s, err := floatOf(x, b, base)
	if err != nil {
		return err
	}
	if base != "nan" {
		if len(args) != 0 {
			return unsupported(x, "__builtin_"+b+" with arguments")
		}
		g.constF(math.Inf(1), s)
		return g.builtinResult(x, s)
	}
	if len(args) != 1 {
		return unsupported(x, "__builtin_"+b+" with another number of arguments")
	}
	p, ok := strip(args[0]).(*cc.PrimaryExpression)
	if !ok || p.Case != cc.PrimaryExpressionString || p.Token.SrcStr() != `""` {
		return unsupported(x, "__builtin_"+b+" of a string other than \"\"")
	}
	// The default quiet NaN, positive, without payload (IEEE 754 6.2.1).
	if s.vt == wasm.F32 {
		g.op(opF32Const)
		g.cur.code = binary.LittleEndian.AppendUint32(g.cur.code, 0x7fc00000)
	} else {
		g.op(opF64Const)
		g.cur.code = binary.LittleEndian.AppendUint64(g.cur.code, 0x7ff8000000000000)
	}
	return g.builtinResult(x, s)
}

// classify compiles isinf, isfinite, isnan and signbit of a float or a
// double, in its own type: |x| == inf, |x| < inf, x != x, and the sign
// bit of its representation.
func (g *fgen) classify(x *cc.PostfixExpression, b string, a cc.ExpressionNode) error {
	from, err := g.rvalue(a)
	if err != nil {
		return err
	}
	if !from.isFloat {
		return unsupported(x, "__builtin_"+b+" of "+typeName(a.Type()))
	}
	s := f32Scalar
	if from.vt == wasm.F64 {
		s = f64Scalar
	}
	g.convert(from, s)
	abs := byte(opF32Abs)
	if s.vt == wasm.F64 {
		abs = opF64Abs
	}
	switch b {
	case "isinf":
		g.op(abs)
		g.constF(math.Inf(1), s)
		g.op(floatRel(opI32Eq, s))
	case "isfinite":
		g.op(abs)
		g.constF(math.Inf(1), s)
		g.op(floatRel(opI32LtS, s))
	case "isnan":
		t := g.local(s.vt)
		g.op(opLocalTee)
		g.uleb(uint64(t))
		g.op(opLocalGet)
		g.uleb(uint64(t))
		g.op(floatRel(opI32Ne, s))
	case "signbit":
		if s.vt == wasm.F64 {
			g.op(opI64ReinterpF64)
			g.constant(63, s64)
			g.op(opI64ShrU, opI32WrapI64)
		} else {
			g.op(opI32ReinterpF32)
			g.constant(31, intScalar)
			g.op(opI32ShrU)
		}
	}
	return g.builtinResult(x, intScalar)
}

// ffs: 0 for 0, else one plus the index of the least significant 1 bit:
// select(ctz(x) + 1, 0, x != 0).
func (g *fgen) ffs(x *cc.PostfixExpression, b string, a cc.ExpressionNode) error {
	from, err := g.rvalue(a)
	if err != nil {
		return err
	}
	if from.isFloat {
		return unsupported(x, "__builtin_"+b+" of a floating value")
	}
	s, ctz := u32, byte(opI32Clz+1)
	if b == "ffsll" {
		s, ctz = u64, opI64Clz+1
	}
	g.convert(from, s)
	t := g.local(s.vt)
	g.op(opLocalTee)
	g.uleb(uint64(t))
	g.op(ctz)
	if s.vt == wasm.I64 {
		g.op(opI32WrapI64)
	}
	g.constant(1, intScalar)
	g.op(opI32Add)
	g.constant(0, intScalar)
	g.op(opLocalGet)
	g.uleb(uint64(t))
	if s.vt == wasm.I64 {
		g.op(opI64Eqz)
	} else {
		g.op(opI32Eqz)
	}
	g.op(opI32Eqz, opSelect)
	return g.builtinResult(x, intScalar)
}

// bswap reverses the bytes of a 16-, 32- or 64-bit value with shifts and
// masks; wasm has no instruction for it.
func (g *fgen) bswap(x *cc.PostfixExpression, b string, a cc.ExpressionNode) error {
	from, err := g.rvalue(a)
	if err != nil {
		return err
	}
	if from.isFloat {
		return unsupported(x, "__builtin_"+b+" of a floating value")
	}
	switch b {
	case "bswap16":
		g.convert(from, u32)
		t := g.swapInput()
		g.bytePart(t, 0, 8)
		g.bytePart(t, 1, 0)
		g.op(opI32Or)
		return g.builtinResult(x, scalar{vt: wasm.I32, bits: 16})
	case "bswap32":
		g.convert(from, u32)
		g.bswap32(g.swapInput())
		return g.builtinResult(x, u32)
	}
	// bswap64: the swapped low half becomes the high half.
	g.convert(from, u64)
	v := g.local(wasm.I64)
	g.op(opLocalSet)
	g.uleb(uint64(v))
	half := func(shift int64) {
		g.op(opLocalGet)
		g.uleb(uint64(v))
		if shift != 0 {
			g.constant(shift, s64)
			g.op(opI64ShrU)
		}
		g.op(opI32WrapI64)
		g.bswap32(g.swapInput())
		g.op(opI64ExtendI32U)
	}
	half(0)
	g.constant(32, s64)
	g.op(opI64Shl)
	half(32)
	g.op(opI64Or)
	return g.builtinResult(x, u64)
}

// swapInput keeps the i32 on the stack in a new local.
func (g *fgen) swapInput() uint32 {
	t := g.local(wasm.I32)
	g.op(opLocalSet)
	g.uleb(uint64(t))
	return t
}

// bytePart pushes byte i of the i32 local t, moved to bit to.
func (g *fgen) bytePart(t uint32, i, to int) {
	g.op(opLocalGet)
	g.uleb(uint64(t))
	if i != 0 {
		g.constant(int64(8*i), intScalar)
		g.op(opI32ShrU)
	}
	g.constant(0xff, intScalar)
	g.op(opI32And)
	if to != 0 {
		g.constant(int64(to), intScalar)
		g.op(opI32Shl)
	}
}

// bswap32 pushes the i32 local t with its bytes reversed.
func (g *fgen) bswap32(t uint32) {
	for i := range 4 {
		g.bytePart(t, i, 24-8*i)
		if i > 0 {
			g.op(opI32Or)
		}
	}
}
