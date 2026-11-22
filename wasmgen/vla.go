package wasmgen

import (
	"slices"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// Block-scope variable-length arrays (C11 6.7.6.2p4), the way clang
// compiles them for wasm32: stacksave/stackrestore on __stack_pointer.
//
// At the declaration the length is evaluated, n * sizeof(T) is computed in
// 32 bits and rounded up to 16, __stack_pointer is saved in a local and
// moved down by that size, and the new value is the array's address. The
// frame is addressed from its own base (mem.go), not from __stack_pointer,
// so moving it disturbs no other local, and a callee's frame lands below the
// array. The epilogue restores __stack_pointer from its value on entry,
// which frees whatever is left when the function returns.
//
// The array's lifetime ends with its block (C11 6.2.4p7), and a jump back
// over the declaration ends it too (6.2.4p7: "if the scope is entered
// recursively, a new instance is created each time"): a loop that declares
// one must give the storage back at each pass or the stack runs out. C11
// 6.8.6.1p1 forbids jumping into the scope of a variable-length type, so the
// arrays live at a jump's target are a prefix of those live at the jump, and
// the ones that die are a suffix of the stack: restoring __stack_pointer to
// the save of the first dying array frees them all.

// vla is a live variable-length array: its declarator, the local holding
// __stack_pointer from before it, and the local holding its address.
type vla struct {
	d          *cc.Declarator
	save, base uint32
}

// isVLALocal reports whether d declares a block-scope variable-length array
// that lives on the stack.
func isVLALocal(d *cc.Declarator) bool {
	if d.IsTypename() || d.IsStatic() || d.IsExtern() {
		return false
	}
	a, ok := d.Type().(*cc.ArrayType)
	return ok && a.IsVLA()
}

// vlaLength finds the length expression of the outermost dimension of d,
// the innermost array declarator of its syntax (a[n][4] is an array of n
// arrays of 4).
func vlaLength(d *cc.Declarator) cc.ExpressionNode {
	var e cc.ExpressionNode
	for dd := d.DirectDeclarator; dd != nil; dd = dd.DirectDeclarator {
		if dd.Case == cc.DirectDeclaratorArr {
			e = dd.AssignmentExpression
		}
	}
	return e
}

// vlaLocal declares the variable-length array d.
func (g *fgen) vlaLocal(d *cc.Declarator, id *cc.InitDeclarator, a *cc.ArrayType) error {
	// cc/v4 refuses an initialiser (C11 6.7.9p3).
	if e, ok := a.Elem().(*cc.ArrayType); ok && e.IsVLA() {
		return unsupported(d, "variable-length array with a variable inner dimension")
	}
	if max(a.Align(), d.Alignas()) > 16 {
		return unsupported(d, "over-aligned variable-length array")
	}
	length := vlaLength(d)
	if length == nil {
		return unsupported(d, "variable-length array in a parenthesized declarator")
	}
	from, err := g.rvalue(length)
	if err != nil {
		return err
	}
	g.convert(from, u32)
	if size := a.Elem().Size(); size != 1 {
		g.constant(size, intScalar)
		g.op(opI32Mul)
	}
	g.constant(15, intScalar)
	g.op(opI32Add)
	g.constant(-16, intScalar)
	g.op(opI32And)
	size := g.local(wasm.I32)
	g.op(opLocalSet)
	g.uleb(uint64(size))
	save, base := g.local(wasm.I32), g.local(wasm.I32)
	sp := g.m.stackPointer()
	g.op(opGlobalGet)
	g.reloc(wasm.RelocGlobalIndexLEB, sp)
	g.op(opLocalTee)
	g.uleb(uint64(save))
	g.op(opLocalGet)
	g.uleb(uint64(size))
	g.op(opI32Sub, opLocalTee)
	g.uleb(uint64(base))
	g.op(opGlobalSet)
	g.reloc(wasm.RelocGlobalIndexLEB, sp)
	if g.vlaBase == nil {
		g.vlaBase = map[*cc.Declarator]uint32{}
	}
	g.vlaBase[d] = base
	g.vlas = append(g.vlas, vla{d, save, base})
	return nil
}

// restoreVLAs gives back the arrays above depth in the live stack, without
// popping them: a jump leaves them live for the code that follows it.
func (g *fgen) restoreVLAs(depth int) {
	if len(g.vlas) <= depth {
		return
	}
	g.op(opLocalGet)
	g.uleb(uint64(g.vlas[depth].save))
	g.op(opGlobalSet)
	g.reloc(wasm.RelocGlobalIndexLEB, g.m.stackPointer())
}

// endVLAScope ends a scope that began with depth arrays live.
func (g *fgen) endVLAScope(depth int) {
	g.restoreVLAs(depth)
	g.vlas = g.vlas[:depth]
}

// markDepth records how many arrays are live where a break or continue
// target is entered.
func (g *fgen) markDepth(target *block) {
	if g.scopeDepth == nil {
		g.scopeDepth = map[*block]int{}
	}
	g.scopeDepth[target] = len(g.vlas)
}

// gotoVLAs frees the arrays a goto to the label name leaves, and refuses a
// goto into the scope of one (C11 6.8.6.1p1).
func (g *fgen) gotoVLAs(j *cc.JumpStatement, name string) error {
	want := g.labelVLAs[name]
	live := make([]*cc.Declarator, len(g.vlas))
	for i, v := range g.vlas {
		live[i] = v.d
	}
	if len(want) > len(live) || !slices.Equal(want, live[:len(want)]) {
		return unsupported(j, "jump into the scope of a variable-length array")
	}
	g.restoreVLAs(len(want))
	return nil
}

// vlaLabels computes, for each label of a function body, the variable-length
// arrays in scope at it, outermost first: what a goto needs to tell the
// arrays it leaves from the ones it stays inside.
func vlaLabels(body *cc.CompoundStatement) map[string][]*cc.Declarator {
	w := &vlaWalk{out: map[string][]*cc.Declarator{}}
	w.compound(body)
	return w.out
}

type vlaWalk struct {
	live []*cc.Declarator
	out  map[string][]*cc.Declarator
}

func (w *vlaWalk) compound(cs *cc.CompoundStatement) {
	n := len(w.live)
	for _, bi := range items(cs) {
		switch bi.Case {
		case cc.BlockItemDecl:
			if bi.Declaration.Case != cc.DeclarationDecl {
				continue
			}
			for il := bi.Declaration.InitDeclaratorList; il != nil; il = il.InitDeclaratorList {
				if d := il.InitDeclarator.Declarator; isVLALocal(d) {
					w.live = append(w.live, d)
				}
			}
		case cc.BlockItemStmt:
			w.statement(bi.Statement)
		}
	}
	w.live = w.live[:n]
}

func (w *vlaWalk) statement(s *cc.Statement) {
	switch s.Case {
	case cc.StatementCompound:
		w.compound(s.CompoundStatement)
	case cc.StatementSelection:
		w.statement(s.SelectionStatement.Statement)
		if s.SelectionStatement.Case == cc.SelectionStatementIfElse {
			w.statement(s.SelectionStatement.Statement2)
		}
	case cc.StatementIteration:
		w.statement(s.IterationStatement.Statement)
	case cc.StatementLabeled:
		l := s.LabeledStatement
		if l.Case == cc.LabeledStatementLabel {
			w.out[l.Token.SrcStr()] = slices.Clone(w.live)
		}
		w.statement(l.Statement)
	}
}
