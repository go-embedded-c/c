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

// infNaN compiles inf, huge_val and nan: constants.
func (g *fgen) infNaN(x *cc.PostfixExpression, b string, args []cc.ExpressionNode) error {
	bits, s, err := infNaNBits(x, b, args)
	if err != nil {
		return err
	}
	if s.vt == wasm.F32 {
		g.op(opF32Const)
		g.cur.code = binary.LittleEndian.AppendUint32(g.cur.code, uint32(bits))
	} else {
		g.op(opF64Const)
		g.cur.code = binary.LittleEndian.AppendUint64(g.cur.code, bits)
	}
	return g.builtinResult(x, s)
}

// infNaNBits returns the representation of __builtin_b(args), b one of
// inf, huge_val and nan with an f suffix or none, and its type.
//
// nan(s) is what clang folds it to (ExprConstant.cpp, TryEvaluateBuiltinNaN,
// at llvmorg-23.1.4): s read whole by StringRef::getAsInteger with radix
// 0 (0x, 0b, 0o, a leading 0 for octal, else decimal), "" as 0; then
// IEEEFloat::makeNaN keeps the low precision-1 bits of that value as the
// significand and sets the quiet bit, precision-2. A string clang cannot
// read is not folded (it calls the library's nan); it is refused here.
func infNaNBits(x *cc.PostfixExpression, b string, args []cc.ExpressionNode) (uint64, scalar, error) {
	base := "inf"
	switch {
	case strings.HasPrefix(b, "huge_val"):
		base = "huge_val"
	case strings.HasPrefix(b, "nan"):
		base = "nan"
	}
	s, err := floatOf(x, b, base)
	if err != nil {
		return 0, s, err
	}
	if base != "nan" {
		if len(args) != 0 {
			return 0, s, unsupported(x, "__builtin_"+b+" with arguments")
		}
		if s.vt == wasm.F32 {
			return uint64(math.Float32bits(float32(math.Inf(1)))), s, nil
		}
		return math.Float64bits(math.Inf(1)), s, nil
	}
	if len(args) != 1 {
		return 0, s, unsupported(x, "__builtin_"+b+" with another number of arguments")
	}
	str, ok := stringValue(args[0])
	if !ok {
		return 0, s, unsupported(x, "__builtin_"+b+" of something other than a string literal")
	}
	fill, ok := nanPayload(strings.TrimSuffix(str, "\x00"))
	if !ok {
		return 0, s, unsupported(x, "__builtin_"+b+" of a string clang does not read as a number")
	}
	if s.vt == wasm.F32 {
		return 0x7f800000 | fill&(1<<23-1) | 1<<22, s, nil
	}
	return 0x7ff0000000000000 | fill&(1<<52-1) | 1<<51, s, nil
}

// nanPayload reads s as StringRef::getAsInteger(0, …) does, returning the
// value modulo 2^64: only its low bits are kept, and multiplying and adding
// modulo 2^64 gives the low bits of the exact value.
func nanPayload(s string) (uint64, bool) {
	if s == "" {
		return 0, true
	}
	radix := uint64(10)
	switch {
	case len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X'):
		radix, s = 16, s[2:]
	case len(s) >= 2 && s[0] == '0' && (s[1] == 'b' || s[1] == 'B'):
		radix, s = 2, s[2:]
	case len(s) >= 2 && s[0] == '0' && s[1] == 'o':
		radix, s = 8, s[2:]
	case len(s) >= 2 && s[0] == '0' && s[1] >= '0' && s[1] <= '9':
		radix, s = 8, s[1:]
	}
	if s == "" {
		return 0, false
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d uint64
		switch {
		case c >= '0' && c <= '9':
			d = uint64(c - '0')
		case c >= 'a' && c <= 'z':
			d = uint64(c-'a') + 10
		case c >= 'A' && c <= 'Z':
			d = uint64(c-'A') + 10
		default:
			return 0, false
		}
		if d >= radix {
			return 0, false
		}
		v = v*radix + d
	}
	return v, true
}

// staticFloatBits returns the representation in s of the constant
// floating initializer e: a literal or a folded expression, or inf,
// huge_val or nan, possibly negated. The builtins are read as bits, not
// through a float64: converting a NaN to float32 need not keep its
// payload.
func staticFloatBits(e cc.ExpressionNode, s scalar) (uint64, bool) {
	e = strip(e)
	sign := uint64(1) << 63
	if s.vt == wasm.F32 {
		sign = 1 << 31
	}
	if u, ok := e.(*cc.UnaryExpression); ok && u.Case == cc.UnaryExpressionMinus {
		if fs, ok := floatScalarOf(u.CastExpression.Type()); ok && fs.vt == s.vt {
			if v, ok := staticFloatBits(u.CastExpression, s); ok {
				return v ^ sign, true
			}
		}
	}
	if x, ok := e.(*cc.PostfixExpression); ok && x.Case == cc.PostfixExpressionCall {
		if d := functionDesignator(x.PostfixExpression); d != nil {
			if b, ok := strings.CutPrefix(d.Name(), "__builtin_"); ok {
				var args []cc.ExpressionNode
				for l := x.ArgumentExpressionList; l != nil; l = l.ArgumentExpressionList {
					args = append(args, l.AssignmentExpression)
				}
				switch b {
				case "inf", "inff", "huge_val", "huge_valf", "nan", "nanf":
					bits, bs, err := infNaNBits(x, b, args)
					if err == nil && bs.vt == s.vt {
						return bits, true
					}
					return 0, false
				}
			}
		}
	}
	v, ok := floatConst(e)
	if !ok {
		return 0, false
	}
	if s.vt == wasm.F32 {
		return uint64(math.Float32bits(float32(v))), true
	}
	return math.Float64bits(v), true
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
