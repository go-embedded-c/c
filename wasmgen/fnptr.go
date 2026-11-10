package wasmgen

import (
	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// Function pointers. A function's address is its index in the indirect
// function table, which wasm-ld builds from the R_WASM_TABLE_INDEX_*
// relocations (Linking.md): i32.const with R_WASM_TABLE_INDEX_SLEB in code,
// R_WASM_TABLE_INDEX_I32 in data. A call through a pointer is
// call_indirect, its type immediate relocated with R_WASM_TYPE_INDEX_LEB and
// its table with R_WASM_TABLE_NUMBER_LEB against the undefined table symbol
// __indirect_function_table, imported from env. That is what clang 23.1.4
// emits (llvm-objdump -d -r of a probe).

// functionDesignator returns the function e names, when e is the
// identifier of a function (C11 6.3.2.1p4), possibly in parentheses.
func functionDesignator(e cc.ExpressionNode) *cc.Declarator {
	p, ok := strip(e).(*cc.PrimaryExpression)
	if !ok || p.Case != cc.PrimaryExpressionIdent {
		return nil
	}
	d, ok := p.ResolvedTo().(*cc.Declarator)
	if !ok || d.Type().Kind() != cc.Function {
		return nil
	}
	return d
}

// addressedFunction returns the function whose address e is: f or &f.
func addressedFunction(e cc.ExpressionNode) *cc.Declarator {
	e = strip(e)
	if u, ok := e.(*cc.UnaryExpression); ok && u.Case == cc.UnaryExpressionAddrof {
		e = u.CastExpression
	}
	return functionDesignator(e)
}

// functionType returns the function type a pointer type points to.
func functionType(t cc.Type) (*cc.FunctionType, bool) {
	p, ok := t.(*cc.PointerType)
	if !ok {
		return nil, false
	}
	if ft, ok := p.Elem().(*cc.FunctionType); ok {
		return ft, true
	}
	ft, ok := p.Elem().Undecay().(*cc.FunctionType)
	return ft, ok
}

// table returns the symbol of the indirect function table, importing it.
func (m *module) table() uint32 {
	if !m.o.Table {
		m.o.Table = true
		m.tbl = m.symbol(wasm.Symbol{Kind: wasm.SymTable, Flags: wasm.SymUndefined | wasm.SymNoStrip})
	}
	return m.tbl
}

// funcAddr pushes the table index of the function d.
func (g *fgen) funcAddr(n cc.Node, d *cc.Declarator) error {
	f, err := g.calleeFor(n, d)
	if err != nil {
		return err
	}
	g.op(opI32Const)
	g.reloc(wasm.RelocTableIndexSLEB, f.sym)
	return nil
}

// staticFunc returns the symbol of the function d for a static initializer.
func (m *module) staticFunc(n cc.Node, d *cc.Declarator) (uint32, error) {
	if f := m.funcs[d.Name()]; f != nil {
		return f.sym, nil
	}
	f, err := m.callee(n, d.Name(), d)
	if err != nil {
		return 0, err
	}
	return f.sym, nil
}

// indirectCall compiles a call through a pointer, (*p)(…) or p(…).
func (g *fgen) indirectCall(x *cc.PostfixExpression, callee cc.ExpressionNode, args []cc.ExpressionNode, slot *int64) error {
	e := strip(callee)
	for {
		u, ok := e.(*cc.UnaryExpression)
		if !ok || u.Case != cc.UnaryExpressionDeref {
			break
		}
		// *p designates the function p points to, which converts back to
		// p (C11 6.5.3.2p4, 6.3.2.1p4): (*p)(…) and (**p)(…) are p(…).
		e = strip(u.CastExpression)
	}
	if functionDesignator(e) != nil {
		return unsupported(x, "call through * of a function")
	}
	ft, ok := functionType(e.Type())
	if !ok {
		return unsupported(x, "call of "+typeName(e.Type()))
	}
	if unprototyped(ft) && len(args) > 0 {
		return unsupported(x, "call through a pointer to a function without a prototype")
	}
	sg, err := signature(x, ft)
	if err != nil {
		return err
	}
	// The pointer first, as clang evaluates it, then the arguments.
	from, err := g.rvalue(e)
	if err != nil {
		return err
	}
	g.convert(from, scalar{vt: wasm.I32, bits: 32, ptr: true})
	fp := g.local(wasm.I32)
	g.op(opLocalSet)
	g.uleb(uint64(fp))
	typ := g.m.wasmType(sg)
	tbl := g.m.table()
	return g.emitCall(x, &sg, args, nil, slot, func() {
		g.op(opLocalGet)
		g.uleb(uint64(fp))
		g.op(opCallIndirect)
		g.reloc(wasm.RelocTypeIndexLEB, typ)
		g.reloc(wasm.RelocTableNumberLEB, tbl)
	})
}
