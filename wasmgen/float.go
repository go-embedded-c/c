package wasmgen

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// Floating types (W5): float is binary32 and double binary64, wasm's f32
// and f64 (BasicCABI.md); long double, binary128, is refused. Every
// operation is the IEEE one wasm defines, as clang emits it at -O0
// (checked with llvm-objdump on clang 23.1.4): a conversion to an integer
// type saturates (trunc_sat, the nontrapping-fptoint feature clang
// enables), a conversion to _Bool compares with zero, negation is f.neg
// and not 0 - x, which would give +0 for -(+0).

var (
	f32Scalar = scalar{vt: wasm.F32, bits: 32, signed: true, isFloat: true}
	f64Scalar = scalar{vt: wasm.F64, bits: 64, signed: true, isFloat: true}
)

// floatScalarOf returns the representation of a floating type.
func floatScalarOf(t cc.Type) (scalar, bool) {
	switch t.Kind() {
	case cc.Float, cc.Float32:
		return f32Scalar, true
	case cc.Double, cc.Float64:
		return f64Scalar, true
	}
	return scalar{}, false
}

// Opcodes (core spec, binary/instructions; wasm-3.0 5.3-binary.instructions).
const (
	opF32Const = 0x43
	opF64Const = 0x44
	opF32Load  = 0x2a
	opF64Load  = 0x2b
	opF32Store = 0x38
	opF64Store = 0x39

	opF32Eq  = 0x5b // ne lt gt le ge follow; f64 at +6
	opF32Neg = 0x8c
	opF64Neg = 0x9a
	opF32Add = 0x92 // sub mul div follow; f64 at +0x0e
	opF32Abs = 0x8b
	opF64Abs = 0x99

	opF32DemoteF64  = 0xb6
	opF64PromoteF32 = 0xbb
)

// floatOp maps an i32 arithmetic opcode (add, sub, mul, div_s, div_u) to
// the floating one of s.
func floatOp(op byte, s scalar) byte {
	var i byte
	switch op {
	case opI32Add:
		i = 0
	case opI32Sub:
		i = 1
	case opI32Mul:
		i = 2
	case opI32DivS, opI32DivU:
		i = 3
	default:
		// cc/v4 refuses %, bitwise operators and shifts on floating
		// operands (C11 6.5.5p2, 6.5.10–12).
		panic("wasmgen: integer-only operator on a floating type")
	}
	if s.vt == wasm.F64 {
		return opF32Add + 0x0e + i
	}
	return opF32Add + i
}

// floatRel maps an i32 comparison (eq, ne, lt_s, gt_s, le_s, ge_s, or their
// unsigned forms) to the floating one of s.
func floatRel(op byte, s scalar) byte {
	var i byte
	switch op {
	case opI32Eq:
		i = 0
	case opI32Ne:
		i = 1
	case opI32LtS, opI32LtU:
		i = 2
	case opI32GtS, opI32GtU:
		i = 3
	case opI32LeS, opI32LeU:
		i = 4
	case opI32GeS, opI32GeU:
		i = 5
	default:
		panic("wasmgen: not a comparison")
	}
	if s.vt == wasm.F64 {
		return opF32Eq + 6 + i
	}
	return opF32Eq + i
}

// constF pushes v as a constant of the floating type s.
func (g *fgen) constF(v float64, s scalar) {
	if s.vt == wasm.F32 {
		g.op(opF32Const)
		g.cur.code = binary.LittleEndian.AppendUint32(g.cur.code, math.Float32bits(float32(v)))
		return
	}
	g.op(opF64Const)
	g.cur.code = binary.LittleEndian.AppendUint64(g.cur.code, math.Float64bits(v))
}

// convertFloat converts the value on the stack when from or to is
// floating; it reports whether it did.
func (g *fgen) convertFloat(from, to scalar) bool {
	switch {
	case !from.isFloat && !to.isFloat:
		return false
	case to.isBool:
		// x != 0, so that NaN is true (C11 6.3.1.2).
		g.constF(0, from)
		g.op(floatRel(opI32Ne, from))
	case from.isFloat && to.isFloat:
		switch {
		case from.vt == wasm.F64 && to.vt == wasm.F32:
			g.op(opF32DemoteF64)
		case from.vt == wasm.F32 && to.vt == wasm.F64:
			g.op(opF64PromoteF32)
		}
	case from.isFloat:
		// Toward zero (C11 6.3.1.4p1); out of range is undefined, and
		// saturates as clang's trunc_sat does. A narrow type goes through
		// i32 and is then normalised.
		var sub byte
		if !(to.signed || to.bits < 32) {
			sub = 1
		}
		if from.vt == wasm.F64 {
			sub += 2
		}
		if to.vt == wasm.I64 {
			sub += 4
		}
		g.op(0xfc, sub) // i32/i64.trunc_sat_f32/f64_s/u
		g.normalize(to)
	default:
		// From an integer: i32 or i64, signed or not, to f32 or f64.
		op := byte(0xb2) // f32.convert_i32_s
		if to.vt == wasm.F64 {
			op = 0xb7
		}
		if from.vt == wasm.I64 {
			op += 2
		}
		if !from.signed {
			op++
		}
		g.op(op)
	}
	return true
}

// floatLiteral returns the value of a floating constant, rounded once from
// its decimal or hexadecimal text to its own type (C11 6.4.4.2): through
// cc/v4's float64 value, a 1.1f would be rounded twice. A double literal
// converted to float (float f = 1.1) is then rounded again by the caller,
// as C says. A long double literal (suffix L) is not one we know.
func floatLiteral(e cc.ExpressionNode) (float64, bool) {
	p, ok := strip(e).(*cc.PrimaryExpression)
	if !ok || p.Case != cc.PrimaryExpressionFloat {
		return 0, false
	}
	text := p.Token.SrcStr()
	bits := 64
	switch {
	case strings.HasSuffix(text, "f"), strings.HasSuffix(text, "F"):
		bits = 32
		text = text[:len(text)-1]
	case strings.HasSuffix(text, "l"), strings.HasSuffix(text, "L"):
		return 0, false
	}
	v, err := strconv.ParseFloat(text, bits)
	if err != nil {
		// Out of range: ParseFloat returns ±Inf with ErrRange, which is
		// the value C gives too; anything else is not a literal we know.
		if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
			return 0, false
		}
	}
	return v, true
}

// floatConst returns the value of a constant arithmetic expression for an
// initialiser: a floating literal, or cc/v4's folded value. A folded
// float expression may have been computed in double (cc/v4 evaluates in
// float64), which can differ from clang in the last bit.
func floatConst(e cc.ExpressionNode) (float64, bool) {
	if v, ok := floatLiteral(e); ok {
		return v, true
	}
	switch v := e.Value().(type) {
	case cc.Float64Value:
		return float64(v), true
	case cc.Int64Value:
		return float64(v), true
	case cc.UInt64Value:
		return float64(v), true
	}
	return 0, false
}

// floatBytes returns v as the little-endian bytes of s.
func floatBytes(v float64, s scalar) []byte {
	if s.vt == wasm.F32 {
		return binary.LittleEndian.AppendUint32(nil, math.Float32bits(float32(v)))
	}
	return binary.LittleEndian.AppendUint64(nil, math.Float64bits(v))
}
