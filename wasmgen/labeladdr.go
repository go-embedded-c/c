package wasmgen

import (
	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// Labels as values, GNU C's &&label and goto *p (GCC manual, Labels as
// Values). wasm has no code addresses, so &&label is a number: one per label
// whose address is taken, distinct across the module and never 0, so that it
// compares unequal to a null pointer and to the address of another label.
// goto *p stores p in a local and jumps to the function's one dispatch
// block, which compares it with the number of each label whose address the
// function takes and jumps there; a value that is none of them (a label of
// another function, which GCC leaves undefined) traps.
//
// One dispatch block for all the goto * of a function keeps the usual
// interpreter loop reducible: the dispatcher is the loop's header and
// dominates every label it reaches.

type labelKey struct{ fn, label string }

// labelID is the number &&label evaluates to in the function being
// compiled, given on first use.
func (m *module) labelID(label string) int64 {
	k := labelKey{m.fn, label}
	if id, ok := m.labelIDs[k]; ok {
		return id
	}
	if m.labelIDs == nil {
		m.labelIDs = map[labelKey]int64{}
		m.takenLabels = map[string][]string{}
	}
	id := int64(len(m.labelIDs) + 1)
	m.labelIDs[k] = id
	m.takenLabels[m.fn] = append(m.takenLabels[m.fn], label)
	return id
}

// labelAddr returns the label of e when e is &&label.
func labelAddr(e cc.ExpressionNode) (string, bool) {
	x, ok := strip(e).(*cc.UnaryExpression)
	if !ok || x.Case != cc.UnaryExpressionLabelAddr {
		return "", false
	}
	return x.Token2.SrcStr(), true
}

// labelConst evaluates, for a static initializer, &&label, &&a - &&b, or
// either cast: the number, or the difference of the numbers, which goto
// *(&&b + d) turns back into &&a.
func (m *module) labelConst(e cc.ExpressionNode) (int64, bool) {
	e = stripPointerCasts(e)
	if l, ok := labelAddr(e); ok {
		return m.labelID(l), true
	}
	if c, ok := strip(e).(*cc.CastExpression); ok && c.Case == cc.CastExpressionCast {
		return m.labelConst(c.CastExpression)
	}
	if x, ok := strip(e).(*cc.AdditiveExpression); ok && x.Case == cc.AdditiveExpressionSub {
		a, okA := labelAddr(stripPointerCasts(x.AdditiveExpression))
		b, okB := labelAddr(stripPointerCasts(x.MultiplicativeExpression))
		if okA && okB {
			return m.labelID(a) - m.labelID(b), true
		}
	}
	return 0, false
}

// gotoExpr compiles goto *e.
func (g *fgen) gotoExpr(j *cc.JumpStatement) error {
	if len(g.vlas) > 0 {
		return unsupported(j, "computed goto in the scope of a variable-length array")
	}
	from, err := g.rvalue(j.ExpressionList)
	if err != nil {
		return err
	}
	if !from.ptr && from.vt != wasm.I32 {
		return unsupported(j, "computed goto of type "+typeName(j.ExpressionList.Type()))
	}
	if g.dispatch == nil {
		g.dispatch = g.newBlock()
		g.target = g.local(wasm.I32)
	}
	g.op(opLocalSet)
	g.uleb(uint64(g.target))
	g.endJump(g.dispatch)
	g.dead()
	return nil
}

// dispatchBlocks fills the dispatch block, when the function has a goto *,
// with the comparisons against each label whose address it takes.
func (g *fgen) dispatchBlocks() {
	if g.dispatch == nil {
		return
	}
	g.cur = g.dispatch
	for _, name := range g.m.takenLabels[g.name] {
		g.op(opLocalGet)
		g.uleb(uint64(g.target))
		g.constant(g.m.labelIDs[labelKey{g.name, name}], scalar{vt: wasm.I32, bits: 32})
		g.op(opI32Eq)
		next := g.newBlock()
		g.branch(g.labelBlock(name), next)
		g.cur = next
	}
	g.op(opUnreachable)
	g.cur.term = termReturn
}
