package wasmgen

import (
	"maps"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// Statement expressions, GNU C's ({ ... }) (GCC manual, Statements and
// Declarations in Expressions): the compound statement runs, and the value
// of its last expression statement is the value of the construct; when the
// last item is not an expression the construct is void.
//
// An expression is emitted into the current block, with its operands on the
// wasm stack, so the body cannot become blocks of the function's graph: a
// value pushed before it would be left on the stack across a block boundary.
// The body is compiled as a graph of its own, structured like a function body
// but wrapped in a wasm block whose end is its only exit, and the result is
// emitted inline. Values already on the stack stay under it: the blocks,
// loops and branches inside it do not reach them. The value goes out through
// a local.
//
// Jumping into a statement expression is not permitted (GCC manual). Jumping
// out of one is, with break, continue or goto; that is refused here, as are a
// label and a goto inside one. return is allowed: it leaves the function
// from anywhere.

// stmtExpr compiles x. With want nil the value, if any, is discarded;
// otherwise it is converted to want and pushed.
func (g *fgen) stmtExpr(x *cc.PrimaryExpression, want *scalar) error {
	if want == nil {
		return g.stmtExprBody(x, nil)
	}
	return g.stmtExprBody(x, func(last cc.ExpressionNode) (wasm.ValType, error) {
		from, err := g.rvalue(last)
		if err != nil {
			return 0, err
		}
		g.convert(from, *want)
		return want.vt, nil
	})
}

// stmtExprAgg compiles x, of structure, union or vector type, and pushes
// the address of a frame temporary holding its value: a copy, as the value
// of an assignment is, so that the object the last expression designates
// may change afterwards without changing it (20020206-1.c assigns it).
func (g *fgen) stmtExprAgg(x *cc.PrimaryExpression) error {
	t := x.Type()
	return g.stmtExprBody(x, func(last cc.ExpressionNode) (wasm.ValType, error) {
		slot := g.frameSlot(t.Size(), t.Align())
		g.slotAddress(slot)
		if err := g.aggAddrAs(last, t); err != nil {
			return 0, err
		}
		g.constant(t.Size(), intScalar)
		g.op(0xfc, 10, 0, 0) // memory.copy
		g.slotAddress(slot)
		return wasm.I32, nil
	})
}

// stmtExprBody compiles the body of x. With value nil the value, if any, is
// discarded; otherwise value pushes it, from the last expression, and says
// of what type.
func (g *fgen) stmtExprBody(x *cc.PrimaryExpression, value func(last cc.ExpressionNode) (wasm.ValType, error)) error {
	list := items(x.CompoundStatement)
	var last cc.ExpressionNode
	if value != nil {
		n := len(list)
		if n == 0 || list[n-1].Case != cc.BlockItemStmt || list[n-1].Statement.Case != cc.StatementExpr ||
			list[n-1].Statement.ExpressionStatement.ExpressionList == nil {
			return unsupported(x, "statement expression whose last item is not an expression")
		}
		last = list[n-1].Statement.ExpressionStatement.ExpressionList
		list = list[:n-1]
	}

	depth := len(g.vlas)
	blocks, cur, breaks, continues := g.blocks, g.cur, g.breaks, g.continues
	labels := maps.Clone(g.labels)
	defer func() { g.blocks, g.cur, g.breaks, g.continues = blocks, cur, breaks, continues }()

	// break and continue that are not inside a loop or switch of the body
	// would leave it: they jump to escape, which the check below refuses.
	escape := &block{}
	g.breaks = append(append([]*block(nil), breaks...), escape)
	g.continues = append(append([]*block(nil), continues...), escape)
	g.blocks = nil
	g.cur = g.newBlock()
	for _, bi := range list {
		var err error
		switch bi.Case {
		case cc.BlockItemLabel:
			g.localLabels(bi)
		case cc.BlockItemDecl:
			err = g.localDeclaration(bi.Declaration)
		case cc.BlockItemStmt:
			err = g.statement(bi.Statement)
		default:
			err = unsupported(bi, "block item "+bi.Case.String())
		}
		if err != nil {
			return err
		}
	}
	var res uint32
	if last != nil {
		vt, err := value(last)
		if err != nil {
			return err
		}
		res = g.local(vt)
		g.op(opLocalSet)
		g.uleb(uint64(res))
	}
	exit := &block{}
	g.endJump(exit)

	inside := map[*block]bool{}
	for _, b := range g.blocks {
		inside[b] = true
	}
	// A label the body declares and defines, `__label__ top; top: …`, lives
	// in it: the jumps to it come from inside. A label of the function that
	// the body mentions, or one that is only jumped to, is not.
	for name, b := range g.labels {
		if old, had := labels[name]; had && old == b {
			continue
		}
		if !inside[b] || !g.declared[name] {
			return unsupported(x, "label or goto in a statement expression")
		}
		delete(g.labels, name)
	}
	for _, b := range g.blocks {
		for _, s := range b.succ {
			if s != exit && !inside[s] {
				return unsupported(x, "jump out of a statement expression")
			}
		}
	}
	region, _, err := g.structure(x, exit)
	if err != nil {
		return err
	}

	g.blocks, g.cur = blocks, cur
	base := uint32(len(g.cur.code))
	g.cur.code = append(g.cur.code, region.code...)
	for _, r := range region.relocs {
		r.Offset += base
		g.cur.relocs = append(g.cur.relocs, r)
	}
	for _, c := range region.calls {
		c.offset += base
		g.cur.calls = append(g.cur.calls, c)
	}
	g.endVLAScope(depth)
	if last != nil {
		g.op(opLocalGet)
		g.uleb(uint64(res))
	}
	return nil
}

// isStmtExpr reports whether e is a statement expression.
func isStmtExpr(e cc.ExpressionNode) (*cc.PrimaryExpression, bool) {
	x, ok := strip(e).(*cc.PrimaryExpression)
	return x, ok && x.Case == cc.PrimaryExpressionStmt
}
