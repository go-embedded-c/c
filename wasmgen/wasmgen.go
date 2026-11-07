// Package wasmgen compiles a checked C translation unit to a relocatable
// wasm32 object (tool-conventions Linking.md), which wasm-ld links with
// wasi-libc. The design and its order of work are in docs/design-m2.md.
//
// This is increment W1: a `main` taking no arguments whose body returns an
// integer constant. Anything else is refused with an *Unsupported error that
// names the construct, so a corpus run counts refusals by cause.
package wasmgen

import (
	"fmt"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// Unsupported is a construct the generator does not compile yet.
type Unsupported struct {
	Pos  string // file:line:col
	What string // the construct, a stable label for counting
}

func (e *Unsupported) Error() string { return fmt.Sprintf("%s: not supported yet: %s", e.Pos, e.What) }

func unsupported(n cc.Node, what string) error {
	return &Unsupported{Pos: n.Position().String(), What: what}
}

// Compile compiles the external declarations of ast that come from the file
// named file; the predefined and builtin sources, and headers, only declare.
func Compile(ast *cc.AST, file string) ([]byte, error) {
	o := &wasm.Object{}
	for l := ast.TranslationUnit; l != nil; l = l.TranslationUnit {
		ed := l.ExternalDeclaration
		if ed.Position().Filename != file {
			continue
		}
		switch ed.Case {
		case cc.ExternalDeclarationFuncDef:
			if err := function(o, ed.FunctionDefinition); err != nil {
				return nil, err
			}
		default:
			return nil, unsupported(ed, "file-scope declaration")
		}
	}
	return o.Encode(), nil
}

// function compiles a definition of `int main(void)` returning a constant.
func function(o *wasm.Object, fd *cc.FunctionDefinition) error {
	d := fd.Declarator
	ft, ok := d.Type().(*cc.FunctionType)
	if !ok || d.Name() != "main" {
		return unsupported(d, "function other than main")
	}
	// cc/v4 gives `(void)` one parameter of type void.
	if ps := ft.Parameters(); len(ps) > 1 || len(ps) == 1 && ps[0].Type().Kind() != cc.Void {
		return unsupported(d, "main with parameters")
	}
	if ft.Result().Kind() != cc.Int {
		return unsupported(d, "main not returning int")
	}
	body := items(fd.CompoundStatement)
	if len(body) != 1 {
		return unsupported(fd.CompoundStatement, "function body other than one return")
	}
	bi := body[0]
	if bi.Case != cc.BlockItemStmt || bi.Statement.Case != cc.StatementJump ||
		bi.Statement.JumpStatement.Case != cc.JumpStatementReturn || bi.Statement.JumpStatement.ExpressionList == nil {
		return unsupported(bi, "function body other than one return")
	}
	e := bi.Statement.JumpStatement.ExpressionList
	v, ok := e.Value().(cc.Int64Value)
	if !ok {
		return unsupported(e, "non-constant expression")
	}
	code := wasm.AppendSLEB([]byte{0x41}, int64(int32(v))) // i32.const
	o.Types = append(o.Types, wasm.FuncType{Results: []wasm.ValType{wasm.I32}})
	o.Funcs = append(o.Funcs, wasm.Func{Type: uint32(len(o.Types) - 1), Body: code})
	idx := uint32(len(o.Funcs) - 1)
	// wasi-libc's crt1.o calls __main_void for a main that takes no
	// arguments; clang defines both names on the same function, hidden.
	for _, n := range []string{"main", "__main_void"} {
		o.Symbols = append(o.Symbols, wasm.Symbol{Kind: wasm.SymFunction, Flags: wasm.SymHidden, Index: idx, Name: n})
	}
	return nil
}

// items returns the block items of a compound statement, without the
// declaration cc/v4 injects at the top of every function body,
// `static const char __func__[] = "name";` (parser.go:623).
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
