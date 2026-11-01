package cell

import (
	"strings"

	"modernc.org/cc/v4"
)

// hoisted is a file-scope object declaration split in two, as cling does
// (DeclExtractor.h:64-78): the declaration without its initialisers stays at
// file scope, and the initialisations become an assignment statement run in
// source order by the cell's function, so that `int i = 1; i++; int j = i;`
// leaves j == 2. Both texts are the chunk with bytes blanked out, never
// moved, so every position in them is the cell's.
type hoisted struct {
	decl string // `int j      ;`
	stmt string // `    j = i;`
}

// hoistable returns the split of a declaration chunk, or false when its
// objects cannot be initialised by assignment: an aggregate initialiser
// (`{…}`), an array, a const object, a typedef or extern declaration, or no
// initialiser at all.
func hoistable(src []byte, lines []int, c Chunk, d *cc.Declaration) (hoisted, bool) {
	if d == nil || d.Case != cc.DeclarationDecl || d.DeclarationSpecifiers == nil || d.InitDeclaratorList == nil {
		return hoisted{}, false
	}
	// at returns a node's offset when it lies inside the chunk, else -1: the
	// AST's positions are trusted only once checked (FuzzAnalyze found
	// `typedef T;T A=0;`, whose positions run past the chunk).
	at := func(n cc.Node) int {
		p, ok := cellPosition(n)
		if !ok {
			return -1
		}
		if o := offset(lines, p.Line, p.Column); o >= c.Start && o < c.End {
			return o
		}
		return -1
	}
	constant := false
	for ds := d.DeclarationSpecifiers; ds != nil; ds = ds.DeclarationSpecifiers {
		if s := ds.StorageClassSpecifier; s != nil {
			switch s.Token.SrcStr() {
			case "typedef", "extern", "_Thread_local", "__thread":
				return hoisted{}, false
			}
		}
		if q := ds.TypeQualifier; q != nil && q.Token.SrcStr() == "const" {
			constant = true
		}
	}
	var ids []*cc.InitDeclarator
	any := false
	for il := d.InitDeclaratorList; il != nil; il = il.InitDeclaratorList {
		ids = append(ids, il.InitDeclarator)
		any = any || il.InitDeclarator.Case == cc.InitDeclaratorInit
	}
	if !any {
		return hoisted{}, false
	}
	// end[k] is where declarator k's text stops: the comma before the next
	// declarator, or the chunk's final ';'.
	end := make([]int, len(ids))
	starts := make([]int, len(ids))
	for k, id := range ids {
		if starts[k] = at(id); starts[k] < c.Start {
			return hoisted{}, false
		}
	}
	for k := range ids {
		if k+1 < len(ids) {
			e := starts[k+1] - 1
			for e > starts[k] && src[e] != ',' {
				e--
			}
			end[k] = e
		} else {
			e := c.End - 1
			for e > starts[k] && src[e] != ';' {
				e--
			}
			if src[e] != ';' {
				return hoisted{}, false
			}
			end[k] = e
		}
	}
	decl := []byte(string(src[c.Start:c.End]))
	stmt := []byte(string(src[c.Start:c.End]))
	blank := func(b []byte, from, to int) {
		for i := from; i < to; i++ {
			if b[i-c.Start] != '\n' {
				b[i-c.Start] = ' '
			}
		}
	}
	blank(stmt, c.Start, starts[0])
	lastComma := -1 // the last comma kept in stmt
	for k, id := range ids {
		nameTok := id.Declarator.NameTok()
		name := at(nameTok)
		if name < starts[k] || name >= end[k] {
			return hoisted{}, false
		}
		if id.Case != cc.InitDeclaratorInit {
			blank(stmt, starts[k], end[k]+1) // the declarator and its comma
			if k+1 == len(ids) {
				stmt[end[k]-c.Start] = ';'
				if lastComma >= 0 {
					stmt[lastComma-c.Start] = ' '
				}
			}
			continue
		}
		eq, init := at(id.Token), at(id.Initializer)
		if eq < name || init < eq || init >= end[k] || name+len(nameTok.SrcStr()) > eq {
			return hoisted{}, false
		}
		declarator := string(src[starts[k]:eq])
		if src[init] == '{' || strings.Contains(declarator, "[") || constant && !strings.Contains(declarator, "*") {
			return hoisted{}, false
		}
		blank(decl, eq, end[k])
		blank(stmt, starts[k], name)
		blank(stmt, name+len(nameTok.SrcStr()), eq)
		if k+1 < len(ids) {
			lastComma = end[k]
		}
	}
	return hoisted{decl: string(decl), stmt: string(stmt)}, true
}
