package wasmgen

import (
	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// A function is lowered to a control-flow graph of basic blocks; each
// block holds straight-line wasm code (its "actions") and ends in a jump, a
// conditional on the i32 its code leaves on the stack, or nothing (its code
// returned). structure.go turns the graph into structured wasm.
type block struct {
	code   []byte
	relocs []wasm.Reloc
	calls  []callSite // offsets in code
	term   termKind
	succ   []*block // jump: [t]; cond: [true, false]

	// set by the structurer
	rpo   int // reverse postorder number, 0 for the entry; -1 if unreachable
	preds []*block
	idom  *block
	kids  []*block
}

type termKind int

const (
	termNone termKind = iota // not terminated yet
	termJump
	termCond
	termReturn // the code ends in return (or unreachable)
)

// fgen compiles one function body.
type fgen struct {
	m       *module
	fn      int // in m.o.Funcs
	result  scalar
	void    bool
	blocks  []*block
	cur     *block
	nparams uint32
	locals  []wasm.ValType // after the parameters
	vars    map[*cc.Declarator]uint32
	depth   int

	breaks    []*block // innermost last
	continues []*block
	cases     map[*cc.LabeledStatement]*block

	// The linear-stack frame (mem.go), when the function needs one.
	frame     bool
	fp, top   uint32 // locals: the frame's base, and __stack_pointer on entry
	frameSize int64
	slots     map[*cc.Declarator]int64 // objects in the frame, by offset
	spilled   []spilledParam
}

func (g *fgen) op(b ...byte) { g.cur.code = append(g.cur.code, b...) }

func (g *fgen) uleb(v uint64) { g.cur.code = wasm.AppendULEB(g.cur.code, v) }

func (g *fgen) sleb(v int64) { g.cur.code = wasm.AppendSLEB(g.cur.code, v) }

// reloc appends a 5-byte immediate relocated against sym.
func (g *fgen) reloc(t wasm.RelocType, sym uint32) {
	g.cur.relocs = append(g.cur.relocs, wasm.Reloc{Type: t, Offset: uint32(len(g.cur.code)), Symbol: sym})
	g.cur.code = wasm.AppendPaddedULEB(g.cur.code, 0)
}

// local allocates a local of type vt.
func (g *fgen) local(vt wasm.ValType) uint32 {
	g.locals = append(g.locals, vt)
	return g.nparams + uint32(len(g.locals)-1)
}

func (g *fgen) newBlock() *block {
	b := &block{}
	g.blocks = append(g.blocks, b)
	return b
}

// jump ends the current block with a jump to to and continues in to.
func (g *fgen) jump(to *block) {
	g.endJump(to)
	g.cur = to
}

func (g *fgen) endJump(to *block) {
	if g.cur.term == termNone {
		g.cur.term, g.cur.succ = termJump, []*block{to}
	}
}

// branch ends the current block on the i32 its code pushed.
func (g *fgen) branch(t, f *block) {
	g.cur.term, g.cur.succ = termCond, []*block{t, f}
}

// dead continues after a return, break, continue: what follows is
// unreachable until a jump target, and lands in a block nothing enters.
func (g *fgen) dead() { g.cur = g.newBlock() }

func (m *module) function(fd *cc.FunctionDefinition) error {
	d := fd.Declarator
	f := m.funcs[d.Name()]
	ft, ok := d.Type().(*cc.FunctionType)
	if !ok || f == nil {
		return unsupported(d, "function definition of type "+typeName(d.Type()))
	}
	g := &fgen{m: m, fn: f.pos, vars: map[*cc.Declarator]uint32{}, void: isVoid(ft.Result()),
		cases: map[*cc.LabeledStatement]*block{}, slots: map[*cc.Declarator]int64{}}
	if !g.void {
		g.result, _ = scalarOf(ft.Result()) // signature checked it
	}
	g.nparams = uint32(len(params(ft)))
	if g.frame = needsFrame(ft, fd.CompoundStatement); g.frame {
		g.fp, g.top = g.local(wasm.I32), g.local(wasm.I32)
	}
	for i, p := range params(ft) {
		if p.Declarator == nil {
			return unsupported(d, "unnamed parameter")
		}
		if p.Declarator.AddressTaken() {
			s := f.params[i]
			slot := g.frameSlot(int64(s.bits/8), s.bits/8)
			g.slots[p.Declarator] = slot
			g.spilled = append(g.spilled, spilledParam{param: uint32(i), slot: slot, s: s})
			continue
		}
		g.vars[p.Declarator] = uint32(i)
	}
	g.cur = g.newBlock()
	// The size expressions of a parameter declared as an array (int
	// b[a++]) are evaluated on entry (C11 6.9.1p10), for their effects:
	// the parameter itself is adjusted to a pointer.
	for _, p := range params(ft) {
		for dd := p.Declarator.DirectDeclarator; dd != nil; dd = dd.DirectDeclarator {
			switch dd.Case {
			case cc.DirectDeclaratorArr, cc.DirectDeclaratorStaticArr, cc.DirectDeclaratorArrStatic:
				if e := dd.AssignmentExpression; e != nil && !e.Pure() {
					if err := g.effect(e); err != nil {
						return err
					}
				}
			}
		}
	}
	if err := g.compound(fd.CompoundStatement); err != nil {
		return err
	}
	// Falling off the end: main returns 0 (C11 5.1.2.2.3); for another
	// function the value is unusable, and any value will do.
	g.ret()
	if g.frame {
		// The frame size is known now: put the prologue before the entry
		// block's code, moving its relocations and call sites.
		p, entry := g.prologue(), g.blocks[0]
		shift := uint32(len(p.code))
		for i := range entry.relocs {
			entry.relocs[i].Offset += shift
		}
		for i := range entry.calls {
			entry.calls[i].offset += shift
		}
		entry.code = append(p.code, entry.code...)
		entry.relocs = append(p.relocs, entry.relocs...)
	}
	body, calls, err := g.structure(d)
	if err != nil {
		return err
	}
	if len(body.code) > m.lim.MaxFunctionBytes {
		return unsupported(d, "function larger than the limit")
	}
	for _, c := range calls {
		c.fn = f.pos
		m.calls = append(m.calls, c)
	}
	m.o.Funcs[f.pos].Body = body.code
	m.o.Funcs[f.pos].Locals = g.locals
	m.o.Funcs[f.pos].Relocs = body.relocs
	return nil
}

// ret ends the current block with a return of nothing or zero.
func (g *fgen) ret() {
	if g.cur.term != termNone {
		return
	}
	if !g.void {
		g.zero(g.result)
	}
	g.epilogue()
	g.op(opReturn)
	g.cur.term = termReturn
}

func (g *fgen) zero(s scalar) {
	if s.vt == wasm.I64 {
		g.op(opI64Const, 0)
		return
	}
	g.op(opI32Const, 0)
}

// items returns the block items of a compound statement, without the
// declaration cc/v4 injects at the top of every function body,
// `static const char __func__[] = "name";` (parser.go:623); a use of
// __func__ is refused as a static local.
func items(cs *cc.CompoundStatement) []*cc.BlockItem {
	var r []*cc.BlockItem
	for l := cs.BlockItemList; l != nil; l = l.BlockItemList {
		if bi := l.BlockItem; !isFuncName(bi) {
			r = append(r, bi)
		}
	}
	return r
}

func isFuncName(bi *cc.BlockItem) bool {
	if bi.Case != cc.BlockItemDecl || bi.Declaration == nil {
		return false
	}
	il := bi.Declaration.InitDeclaratorList
	return il != nil && il.InitDeclaratorList == nil && il.InitDeclarator.Declarator.Name() == "__func__"
}

func (g *fgen) compound(cs *cc.CompoundStatement) error {
	for _, bi := range items(cs) {
		switch bi.Case {
		case cc.BlockItemDecl:
			if err := g.localDeclaration(bi.Declaration); err != nil {
				return err
			}
		case cc.BlockItemStmt:
			if err := g.statement(bi.Statement); err != nil {
				return err
			}
		default:
			return unsupported(bi, "block item "+bi.Case.String())
		}
	}
	return nil
}

// condition evaluates e as a controlling expression: it pushes an i32
// that is non-zero when e compares unequal to 0 (C11 6.8.4.1).
func (g *fgen) condition(e cc.ExpressionNode) error {
	from, err := g.rvalue(e)
	if err != nil {
		return err
	}
	if from.vt == wasm.I64 {
		g.op(opI64Eqz, opI32Eqz)
	}
	return nil
}

func (g *fgen) statement(s *cc.Statement) error {
	switch s.Case {
	case cc.StatementCompound:
		return g.compound(s.CompoundStatement)
	case cc.StatementExpr:
		if e := s.ExpressionStatement.ExpressionList; e != nil {
			return g.effect(e)
		}
		return nil
	case cc.StatementSelection:
		return g.selection(s.SelectionStatement)
	case cc.StatementIteration:
		return g.iteration(s.IterationStatement)
	case cc.StatementLabeled:
		return g.labeled(s.LabeledStatement)
	case cc.StatementJump:
		return g.jumpStatement(s.JumpStatement)
	}
	return unsupported(s, "statement "+s.Case.String())
}

func (g *fgen) jumpStatement(j *cc.JumpStatement) error {
	switch j.Case {
	case cc.JumpStatementReturn:
		if j.ExpressionList != nil {
			if g.void {
				if err := g.effect(j.ExpressionList); err != nil {
					return err
				}
			} else {
				from, err := g.rvalue(j.ExpressionList)
				if err != nil {
					return err
				}
				g.convert(from, g.result)
				g.epilogue()
				g.op(opReturn)
				g.cur.term = termReturn
				g.dead()
				return nil
			}
		}
		// `return;` in a function with a result is allowed when the caller
		// does not use the value (C89 3.6.6.4); wasm needs one.
		g.ret()
		g.dead()
		return nil
	case cc.JumpStatementBreak:
		if len(g.breaks) == 0 {
			return unsupported(j, "break outside a loop or switch")
		}
		g.endJump(g.breaks[len(g.breaks)-1])
		g.dead()
		return nil
	case cc.JumpStatementContinue:
		if len(g.continues) == 0 {
			return unsupported(j, "continue outside a loop")
		}
		g.endJump(g.continues[len(g.continues)-1])
		g.dead()
		return nil
	}
	return unsupported(j, "jump "+j.Case.String()+" (W7)")
}

func (g *fgen) selection(s *cc.SelectionStatement) error {
	switch s.Case {
	case cc.SelectionStatementIf, cc.SelectionStatementIfElse:
		if err := g.condition(s.ExpressionList); err != nil {
			return err
		}
		then, join := g.newBlock(), g.newBlock()
		els := join
		if s.Case == cc.SelectionStatementIfElse {
			els = g.newBlock()
		}
		g.branch(then, els)
		g.cur = then
		if err := g.statement(s.Statement); err != nil {
			return err
		}
		g.endJump(join)
		if s.Case == cc.SelectionStatementIfElse {
			g.cur = els
			if err := g.statement(s.Statement2); err != nil {
				return err
			}
			g.endJump(join)
		}
		g.cur = join
		return nil
	case cc.SelectionStatementSwitch:
		return g.switchStatement(s)
	}
	return unsupported(s, "selection "+s.Case.String())
}

// switchStatement compiles a switch as a chain of comparisons of the
// promoted controlling value with each case value (C11 6.8.4.2), then the
// body, whose case labels are the chain's targets.
func (g *fgen) switchStatement(s *cc.SelectionStatement) error {
	st, ok := scalarOf(cc.IntegerPromotion(s.ExpressionList.Type()))
	if !ok {
		return unsupported(s, "switch on "+typeName(s.ExpressionList.Type()))
	}
	from, err := g.rvalue(s.ExpressionList)
	if err != nil {
		return err
	}
	g.convert(from, st)
	tmp := g.local(st.vt)
	g.op(opLocalSet)
	g.uleb(uint64(tmp))
	exit := g.newBlock()
	target := exit
	for _, l := range s.LabeledStatements() {
		b := g.newBlock()
		g.cases[l] = b
		switch l.Case {
		case cc.LabeledStatementDefault:
			target = b
			continue
		case cc.LabeledStatementCaseLabel:
		default:
			return unsupported(l, "case range")
		}
		v, ok := intValue(l.ConstantExpression)
		if !ok {
			return unsupported(l, "non-constant case")
		}
		g.op(opLocalGet)
		g.uleb(uint64(tmp))
		g.constant(v, st)
		eq := byte(opI32Eq)
		if st.vt == wasm.I64 {
			eq += i64CompareDelta
		}
		g.op(eq)
		next := g.newBlock()
		g.branch(b, next)
		g.cur = next
	}
	g.endJump(target)
	g.dead()
	g.breaks = append(g.breaks, exit)
	err = g.statement(s.Statement)
	g.breaks = g.breaks[:len(g.breaks)-1]
	if err != nil {
		return err
	}
	g.jump(exit)
	return nil
}

func (g *fgen) labeled(l *cc.LabeledStatement) error {
	b, ok := g.cases[l]
	if !ok {
		return unsupported(l, "label (W7)")
	}
	g.jump(b) // falling through from the previous case
	return g.statement(l.Statement)
}

func (g *fgen) iteration(s *cc.IterationStatement) error {
	switch s.Case {
	case cc.IterationStatementWhile:
		head, body, exit := g.newBlock(), g.newBlock(), g.newBlock()
		g.jump(head)
		if err := g.condition(s.ExpressionList); err != nil {
			return err
		}
		g.branch(body, exit)
		g.cur = body
		if err := g.loopBody(s.Statement, exit, head); err != nil {
			return err
		}
		g.endJump(head)
		g.cur = exit
		return nil
	case cc.IterationStatementDo:
		body, test, exit := g.newBlock(), g.newBlock(), g.newBlock()
		g.jump(body)
		if err := g.loopBody(s.Statement, exit, test); err != nil {
			return err
		}
		g.jump(test)
		if err := g.condition(s.ExpressionList); err != nil {
			return err
		}
		g.branch(body, exit)
		g.cur = exit
		return nil
	case cc.IterationStatementFor, cc.IterationStatementForDecl:
		if s.Case == cc.IterationStatementForDecl {
			if err := g.localDeclaration(s.Declaration); err != nil {
				return err
			}
		} else if s.ExpressionList != nil {
			if err := g.effect(s.ExpressionList); err != nil {
				return err
			}
		}
		cond, step := s.ExpressionList2, s.ExpressionList3
		if s.Case == cc.IterationStatementForDecl {
			cond, step = s.ExpressionList, s.ExpressionList2
		}
		head, body, next, exit := g.newBlock(), g.newBlock(), g.newBlock(), g.newBlock()
		g.jump(head)
		if cond != nil {
			if err := g.condition(cond); err != nil {
				return err
			}
			g.branch(body, exit)
		} else {
			g.endJump(body)
		}
		g.cur = body
		if err := g.loopBody(s.Statement, exit, next); err != nil {
			return err
		}
		g.jump(next)
		if step != nil {
			if err := g.effect(step); err != nil {
				return err
			}
		}
		g.endJump(head)
		g.cur = exit
		return nil
	}
	return unsupported(s, "iteration "+s.Case.String())
}

func (g *fgen) loopBody(s *cc.Statement, brk, cont *block) error {
	g.breaks = append(g.breaks, brk)
	g.continues = append(g.continues, cont)
	err := g.statement(s)
	g.breaks = g.breaks[:len(g.breaks)-1]
	g.continues = g.continues[:len(g.continues)-1]
	return err
}

// localDeclaration gives each block-scope integer object a wasm local.
func (g *fgen) localDeclaration(decl *cc.Declaration) error {
	if decl.Case != cc.DeclarationDecl {
		return nil
	}
	for il := decl.InitDeclaratorList; il != nil; il = il.InitDeclaratorList {
		id := il.InitDeclarator
		d := id.Declarator
		switch {
		case d.IsTypename(), d.Type().Kind() == cc.Function, d.IsExtern():
			continue
		case d.IsStatic():
			return unsupported(d, "static local")
		}
		s, ok := scalarOf(d.Type())
		if !ok || d.AddressTaken() {
			if err := g.memoryLocal(d, id); err != nil {
				return err
			}
			continue
		}
		idx, ok := g.vars[d]
		if !ok {
			idx = g.local(s.vt)
			g.vars[d] = idx
		}
		if id.Case != cc.InitDeclaratorInit {
			continue
		}
		if id.Initializer.AssignmentExpression == nil {
			return unsupported(id.Initializer, "braced initializer")
		}
		from, err := g.rvalue(id.Initializer.AssignmentExpression)
		if err != nil {
			return err
		}
		g.convert(from, s)
		g.op(opLocalSet)
		g.uleb(uint64(idx))
	}
	return nil
}
