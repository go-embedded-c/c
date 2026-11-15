package wasmgen

import (
	"maps"

	"github.com/go-embedded-c/c/internal/cc"
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
	list := items(x.CompoundStatement)
	var last cc.ExpressionNode
	if want != nil {
		n := len(list)
		if n == 0 || list[n-1].Case != cc.BlockItemStmt || list[n-1].Statement.Case != cc.StatementExpr ||
			list[n-1].Statement.ExpressionStatement.ExpressionList == nil {
			return unsupported(x, "statement expression whose last item is not an expression")
		}
		last = list[n-1].Statement.ExpressionStatement.ExpressionList
		list = list[:n-1]
	}

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
		from, err := g.rvalue(last)
		if err != nil {
			return err
		}
		g.convert(from, *want)
		res = g.local(want.vt)
		g.op(opLocalSet)
		g.uleb(uint64(res))
	}
	exit := &block{}
	g.endJump(exit)

	if !maps.Equal(labels, g.labels) {
		return unsupported(x, "label or goto in a statement expression")
	}
	inside := map[*block]bool{}
	for _, b := range g.blocks {
		inside[b] = true
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
