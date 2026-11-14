package wasmgen

import (
	"strconv"
	"strings"

	"github.com/go-embedded-c/c/internal/cc"
)

// GNU asm statements with an empty template (GCC manual, "Extended Asm").
// The gcc torture tests use them as optimisation barriers:
//
//	asm volatile ("" : : : "memory");
//	asm ("" : "+r" (x));
//	asm ("" : "=r" (x) : "0" (y));
//
// An empty template runs no instruction, so the statement does only what
// its operands say:
//   - an input is evaluated;
//   - a "+" output (read and written) keeps its value;
//   - a "=" output tied to an input by its number ("0") receives that input,
//     as the register they share carries it through;
//   - a "=m" output untied keeps its value: the asm writes no memory;
//   - clobbers are nothing to a generator that keeps no value in a
//     register across statements.
//
// A template that is not empty, a named operand, asm goto and a "=" output
// that no input ties and that is not memory are refused: what the output
// holds afterwards is whatever the register held.

// asmOperand is one "constraint" (expression) of an asm statement.
type asmOperand struct {
	constraint string
	e          cc.ExpressionNode
}

// asmStatement compiles s, an asm statement.
func (g *fgen) asmStatement(s *cc.AsmStatement) error {
	a := s.Asm
	if a == nil {
		return unsupported(s, "asm")
	}
	if a.Token3.SrcStr() != `""` {
		return unsupported(s, "asm with a template")
	}
	var sections [][]asmOperand
	for l := a.AsmArgList; l != nil; l = l.AsmArgList {
		var ops []asmOperand
		for el := l.AsmExpressionList; el != nil; el = el.AsmExpressionList {
			if el.AsmIndex != nil {
				return unsupported(s, "asm with a named operand")
			}
			if el.AssignmentExpression == nil {
				continue // an empty section, ": :"
			}
			op, ok := asmOperandOf(el.AssignmentExpression)
			if !ok {
				// A clobber is a string alone.
				if _, isString := asmString(el.AssignmentExpression); !isString {
					return unsupported(s, "asm operand")
				}
			}
			ops = append(ops, op)
		}
		// cc/v4 links its lists in source order, as for the arguments of
		// a call.
		sections = append(sections, ops)
	}
	if len(sections) > 3 {
		return unsupported(s, "asm goto")
	}
	var outputs, inputs []asmOperand
	if len(sections) > 0 {
		outputs = sections[0]
	}
	if len(sections) > 1 {
		inputs = sections[1]
	}
	for _, o := range outputs {
		if o.e == nil {
			return unsupported(s, "asm operand")
		}
	}
	tied := map[int]cc.ExpressionNode{}
	for _, in := range inputs {
		if in.e == nil {
			return unsupported(s, "asm operand")
		}
		c := strings.TrimLeft(in.constraint, "%")
		if n, err := strconv.Atoi(c); err == nil {
			if n < 0 || n >= len(outputs) {
				return unsupported(s, "asm input tied to no output")
			}
			if _, dup := tied[n]; dup {
				return unsupported(s, "asm output tied twice")
			}
			tied[n] = in.e
			continue
		}
		// An untied input: evaluated, nothing else.
		if !in.e.Pure() {
			if err := g.effect(in.e); err != nil {
				return err
			}
		}
	}
	for i, o := range outputs {
		c := o.constraint
		switch {
		case strings.HasPrefix(c, "+"):
			if _, ok := tied[i]; ok {
				return unsupported(s, "asm read-write output tied to an input")
			}
			// Read and written back unchanged.
			if !o.e.Pure() {
				if err := g.effect(o.e); err != nil {
					return err
				}
			}
		case strings.HasPrefix(c, "="):
			if in, ok := tied[i]; ok {
				if err := g.assignFrom(s, o.e, in); err != nil {
					return err
				}
				continue
			}
			if strings.TrimLeft(c, "=&") != "m" {
				return unsupported(s, "asm output that no input gives a value")
			}
			if !o.e.Pure() {
				if err := g.effect(o.e); err != nil {
					return err
				}
			}
		default:
			return unsupported(s, "asm output constraint "+strconv.Quote(c))
		}
	}
	return nil
}

// asmOperandOf reads "constraint" (e), which cc/v4 parses as a call of the
// string literal.
func asmOperandOf(e cc.ExpressionNode) (asmOperand, bool) {
	x, ok := strip(e).(*cc.PostfixExpression)
	if !ok || x.Case != cc.PostfixExpressionCall {
		return asmOperand{}, false
	}
	c, ok := asmString(x.PostfixExpression)
	if !ok {
		return asmOperand{}, false
	}
	l := x.ArgumentExpressionList
	if l == nil || l.ArgumentExpressionList != nil {
		return asmOperand{}, false
	}
	return asmOperand{constraint: c, e: l.AssignmentExpression}, true
}

// asmString reads a string literal of an asm statement from its token:
// cc/v4 does not type-check asm operands, so the literal has no value.
func asmString(e cc.ExpressionNode) (string, bool) {
	p, ok := strip(e).(*cc.PrimaryExpression)
	if !ok || p.Case != cc.PrimaryExpressionString {
		return "", false
	}
	s, err := strconv.Unquote(p.Token.SrcStr())
	return s, err == nil
}

// assignFrom compiles lhs = rhs for two scalars, rhs converted to lhs's
// type as by simple assignment (C11 6.5.16.1).
func (g *fgen) assignFrom(n cc.Node, lhs, rhs cc.ExpressionNode) error {
	if _, ok := scalarOf(lhs.Type()); !ok {
		return unsupported(n, "asm output of type "+typeName(lhs.Type()))
	}
	t, err := g.lvalue(lhs)
	if err != nil {
		return err
	}
	if err := g.operand(rhs, t.s); err != nil {
		return err
	}
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
	g.store(t, false)
	return nil
}
