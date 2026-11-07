package wasmgen

import (
	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// fgen compiles one function body.
type fgen struct {
	m       *module
	fn      int // in m.o.Funcs
	result  scalar
	void    bool
	body    []byte
	relocs  []wasm.Reloc
	nparams uint32
	locals  []wasm.ValType // after the parameters
	vars    map[*cc.Declarator]uint32
	depth   int
}

func (g *fgen) op(b ...byte) { g.body = append(g.body, b...) }

func (g *fgen) uleb(v uint64) { g.body = wasm.AppendULEB(g.body, v) }

// local allocates a local of type vt.
func (g *fgen) local(vt wasm.ValType) uint32 {
	g.locals = append(g.locals, vt)
	return g.nparams + uint32(len(g.locals)-1)
}

func (m *module) function(fd *cc.FunctionDefinition) error {
	d := fd.Declarator
	f := m.funcs[d.Name()]
	ft, ok := d.Type().(*cc.FunctionType)
	if !ok || f == nil {
		return unsupported(d, "function definition of type "+typeName(d.Type()))
	}
	g := &fgen{m: m, fn: f.pos, vars: map[*cc.Declarator]uint32{}, void: isVoid(ft.Result())}
	if !g.void {
		g.result, _ = scalarOf(ft.Result()) // typeIndex checked it
	}
	for i, p := range params(ft) {
		if p.Declarator == nil {
			return unsupported(d, "unnamed parameter")
		}
		if p.Declarator.AddressTaken() {
			return unsupported(p.Declarator, "address-taken parameter")
		}
		g.vars[p.Declarator] = uint32(i)
	}
	g.nparams = uint32(len(params(ft)))
	if err := g.compound(fd.CompoundStatement); err != nil {
		return err
	}
	// Falling off the end: main returns 0 (C11 5.1.2.2.3); for another
	// function the value is unusable, and any value will do.
	if !g.void {
		g.zero(g.result)
	}
	if len(g.body) > m.lim.MaxFunctionBytes {
		return unsupported(d, "function larger than the limit")
	}
	m.o.Funcs[f.pos].Body = g.body
	m.o.Funcs[f.pos].Locals = g.locals
	m.o.Funcs[f.pos].Relocs = g.relocs
	return nil
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

func (g *fgen) statement(s *cc.Statement) error {
	switch s.Case {
	case cc.StatementCompound:
		return g.compound(s.CompoundStatement)
	case cc.StatementExpr:
		if e := s.ExpressionStatement.ExpressionList; e != nil {
			return g.effect(e)
		}
		return nil
	case cc.StatementJump:
		j := s.JumpStatement
		if j.Case != cc.JumpStatementReturn {
			return unsupported(j, "control flow (W3): "+j.Case.String())
		}
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
			}
		} else if !g.void {
			// `return;` in a function with a result: allowed when the
			// caller does not use the value (C89 3.6.6.4); wasm needs one.
			g.zero(g.result)
		}
		g.op(opReturn)
		return nil
	}
	return unsupported(s, "control flow (W3): "+s.Case.String())
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
		case d.AddressTaken():
			return unsupported(d, "address-taken local (W4)")
		}
		s, ok := scalarOf(d.Type())
		if !ok {
			return unsupported(d, "local of type "+d.Type().String())
		}
		idx := g.local(s.vt)
		g.vars[d] = idx
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
