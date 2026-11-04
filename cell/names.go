package cell

import (
	"fmt"
	"slices"
	"strings"

	"github.com/go-embedded-c/c/internal/cc"
)

// defines lists the file-scope names the cell's declarations and #defines
// introduce, in source order. inits holds the cell positions of the
// declarators written with an initializer: ast is the hoisted translation
// unit, where those initializers have become statements.
func defines(src []byte, ast *cc.AST, inits map[position]bool, chunks []Chunk) []Name {
	var r []Name
	add := func(name string, k Kind, def Def, n cc.Node) {
		if p, ok := cellPosition(n); ok && name != "" {
			r = append(r, Name{Name: name, Kind: k, Def: def, Line: p.Line, Col: p.Column})
		}
	}
	for l := ast.TranslationUnit; l != nil; l = l.TranslationUnit {
		ed := l.ExternalDeclaration
		if _, ok := cellPosition(ed); !ok {
			continue
		}
		switch ed.Case {
		case cc.ExternalDeclarationFuncDef:
			fd := ed.FunctionDefinition
			specifierNames(fd.DeclarationSpecifiers, add)
			add(fd.Declarator.Name(), Function, Definition, fd.Declarator)
		case cc.ExternalDeclarationDecl:
			d := ed.Declaration
			if d.Case != cc.DeclarationDecl {
				continue
			}
			specifierNames(d.DeclarationSpecifiers, add)
			if d.InitDeclaratorList == nil {
				forwardTag(d.DeclarationSpecifiers, add)
			}
			extern := storageClass(d, cc.StorageClassSpecifierExtern)
			for il := d.InitDeclaratorList; il != nil; il = il.InitDeclaratorList {
				dr := il.InitDeclarator.Declarator
				p, _ := cellPosition(dr)
				switch {
				case dr.IsTypename():
					add(dr.Name(), Typedef, Definition, dr)
				case isFunction(dr.Type()):
					add(dr.Name(), Function, Declaration, dr)
				case inits[p]:
					add(dr.Name(), Variable, Definition, dr) // 6.9.2p1, extern included
				case extern:
					add(dr.Name(), Variable, Declaration, dr)
				default:
					add(dr.Name(), Variable, Tentative, dr) // 6.9.2p2
				}
			}
		}
	}
	r = append(r, macroNames(src, chunks)...)
	slices.SortStableFunc(r, func(a, b Name) int {
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return a.Col - b.Col
	})
	return r
}

// macroNames lists the macros the cell's #define lines introduce, top-level
// and inside code chunks.
func macroNames(src []byte, chunks []Chunk) []Name {
	var r []Name
	for _, c := range chunks {
		ds := c.Directives
		if c.Kind == Directive {
			ds = []Chunk{c}
		}
		for _, d := range ds {
			name, rest := directiveName(stripComments(string(src[d.Start:d.End])))
			if name != "define" {
				continue
			}
			i := 0
			for i < len(rest) && isIdentByte(rest[i]) {
				i++
			}
			if i > 0 {
				r = append(r, Name{Name: rest[:i], Kind: Macro, Def: Definition, Line: d.Line, Col: d.Col})
			}
		}
	}
	return r
}

func isFunction(t cc.Type) bool {
	_, ok := t.(*cc.FunctionType)
	return ok
}

// initialised returns the cell positions of the declarators written with an
// initializer, read from the translation unit before hoisting.
func initialised(ast *cc.AST) map[position]bool {
	r := map[position]bool{}
	for l := ast.TranslationUnit; l != nil; l = l.TranslationUnit {
		ed := l.ExternalDeclaration
		if ed.Case != cc.ExternalDeclarationDecl || ed.Declaration.Case != cc.DeclarationDecl {
			continue
		}
		for il := ed.Declaration.InitDeclaratorList; il != nil; il = il.InitDeclaratorList {
			if il.InitDeclarator.Case == cc.InitDeclaratorInit {
				if p, ok := cellPosition(il.InitDeclarator.Declarator); ok {
					r[p] = true
				}
			}
		}
	}
	return r
}

func storageClass(d *cc.Declaration, c cc.StorageClassSpecifierCase) bool {
	for ds := d.DeclarationSpecifiers; ds != nil; ds = ds.DeclarationSpecifiers {
		if ds.StorageClassSpecifier != nil && ds.StorageClassSpecifier.Case == c {
			return true
		}
	}
	return false
}

// forwardTag reports the tag of `struct s;` or `union u;`, a declaration with
// no declarator and no body (6.7.2.3p7): it declares the tag, defines nothing.
func forwardTag(ds *cc.DeclarationSpecifiers, add func(string, Kind, Def, cc.Node)) {
	for ; ds != nil; ds = ds.DeclarationSpecifiers {
		if ts := ds.TypeSpecifier; ts != nil && ts.Case == cc.TypeSpecifierStructOrUnion {
			if s := ts.StructOrUnionSpecifier; s.Case == cc.StructOrUnionSpecifierTag {
				add(s.StructOrUnion.Token.SrcStr()+" "+s.Token.SrcStr(), Tag, Declaration, s)
			}
		}
	}
}

// specifierNames reports the tags and enumerators a declaration's specifiers
// define (a struct, union or enum with a body).
func specifierNames(ds *cc.DeclarationSpecifiers, add func(string, Kind, Def, cc.Node)) {
	for ; ds != nil; ds = ds.DeclarationSpecifiers {
		ts := ds.TypeSpecifier
		if ts == nil {
			continue
		}
		switch ts.Case {
		case cc.TypeSpecifierStructOrUnion:
			s := ts.StructOrUnionSpecifier
			if s.Case == cc.StructOrUnionSpecifierDef && s.Token.SrcStr() != "" {
				add(s.StructOrUnion.Token.SrcStr()+" "+s.Token.SrcStr(), Tag, Definition, s)
			}
		case cc.TypeSpecifierEnum:
			e := ts.EnumSpecifier
			if e.Case != cc.EnumSpecifierDef {
				continue
			}
			if e.Token2.SrcStr() != "" {
				add("enum "+e.Token2.SrcStr(), Tag, Definition, e)
			}
			for el := e.EnumeratorList; el != nil; el = el.EnumeratorList {
				add(el.Enumerator.Token.SrcStr(), Enumerator, Definition, el.Enumerator)
			}
		}
	}
}

// uses returns the names defined by earlier cells that occur in the cell,
// sorted. It reads the cell's tokens, not its AST: a name used only inside a
// macro expansion is a use of the macro, which is itself a name. A member
// name after '.' or '->' is not a use; `struct s` is a use of the tag.
func uses(src []byte, chunks []Chunk, ctx Context, defs []Name) []string {
	own := map[string]bool{}
	for _, d := range defs {
		own[d.Name] = true
	}
	seen := map[string]bool{}
	for _, c := range chunks {
		s := &scanner{src: src[:c.End], off: c.Start, line: c.Line, col: c.Col}
		prev, prevWord := byte(0), ""
		for {
			if _, err := s.skipSpace(); err != nil || s.off >= c.End {
				break
			}
			if c.Kind == Code && s.src[s.off] == '#' && s.atLineStart() {
				_, _ = s.directive()
				continue
			}
			t, err := s.lex()
			if err != nil {
				break
			}
			if t.word != "" {
				name := t.word
				if prevWord == "struct" || prevWord == "union" || prevWord == "enum" {
					name = prevWord + " " + t.word
				}
				if _, ok := ctx.Defined[name]; ok && prev != '.' && prev != '>' && !own[name] {
					seen[name] = true
				}
				prev, prevWord = 0, t.word
				continue
			}
			prev, prevWord = t.punct, ""
			if t.punct == '>' && t.start > 0 && src[t.start-1] == '-' {
				prev = '>'
			} else if t.punct == '>' {
				prev = 0
			}
		}
	}
	r := make([]string, 0, len(seen))
	for n := range seen {
		r = append(r, n)
	}
	slices.Sort(r)
	return r
}

// atLineStart reports whether only blanks precede s.off on its line.
func (s *scanner) atLineStart() bool {
	for i := s.off - 1; i >= 0; i-- {
		switch s.src[i] {
		case '\n':
			return true
		case ' ', '\t':
		default:
			return false
		}
	}
	return true
}

// preludeOf is what the cell leaves for later cells: its directives, and its
// declarations reduced to what declares without defining again. A function
// definition becomes its prototype; an initialised object loses its
// initialiser (and becomes extern when it has no storage class), keeping an
// array's length.
func preludeOf(src []byte, lines []int, ast *cc.AST, chunks []Chunk, stmt []bool, hoists map[int]hoisted) string {
	byChunk := map[int]*cc.ExternalDeclaration{}
	for l := ast.TranslationUnit; l != nil; l = l.TranslationUnit {
		if p, ok := cellPosition(l.ExternalDeclaration); ok {
			if i := chunkAt(chunks, lines, p); i >= 0 {
				byChunk[i] = l.ExternalDeclaration
			}
		}
	}
	var b strings.Builder
	for i, c := range chunks {
		if stmt[i] {
			continue
		}
		// at returns a node's offset inside this chunk, or -1 (see hoistable).
		at := func(n cc.Node) int {
			if p, ok := cellPosition(n); ok {
				if o := offset(lines, p.Line, p.Column); o >= c.Start && o <= c.End {
					return o
				}
			}
			return -1
		}
		text := string(src[c.Start:c.End])
		if h, ok := hoists[i]; ok {
			// The blanked initialisers leave runs of spaces; squeeze them.
			text = strings.Join(strings.Fields(h.decl), " ")
			text = strings.NewReplacer(" ,", ",", " ;", ";").Replace(text)
			if ed := byChunk[i]; ed != nil && !hasStorageClass(ed.Declaration) {
				text = "extern " + text
			}
		} else if ed := byChunk[i]; ed != nil {
			switch ed.Case {
			case cc.ExternalDeclarationFuncDef:
				fd := ed.FunctionDefinition
				if fd.DeclarationList != nil {
					continue // K&R definition: no prototype to write
				}
				if o := at(fd.CompoundStatement); o >= 0 {
					text = strings.TrimSpace(string(src[c.Start:o])) + ";"
				}
			case cc.ExternalDeclarationDecl:
				text = declarationOnly(src, c, ed.Declaration, at)
			}
		}
		fmt.Fprintf(&b, "%s\n", text)
	}
	return b.String()
}

func hasStorageClass(d *cc.Declaration) bool {
	if d == nil {
		return false
	}
	for ds := d.DeclarationSpecifiers; ds != nil; ds = ds.DeclarationSpecifiers {
		if ds.StorageClassSpecifier != nil {
			return true
		}
	}
	return false
}

func declarationOnly(src []byte, c Chunk, d *cc.Declaration, at func(cc.Node) int) string {
	text := string(src[c.Start:c.End])
	if d.Case != cc.DeclarationDecl || d.InitDeclaratorList == nil {
		return text
	}
	var ids []*cc.InitDeclarator
	initialised := false
	for il := d.InitDeclaratorList; il != nil; il = il.InitDeclaratorList {
		ids = append(ids, il.InitDeclarator)
		initialised = initialised || il.InitDeclarator.Case == cc.InitDeclaratorInit
	}
	if !initialised {
		return text
	}
	// Every cut must lie in the chunk, in order; otherwise keep the text.
	starts, ends := make([]int, len(ids)), make([]int, len(ids))
	prev := c.Start
	for k, id := range ids {
		starts[k], ends[k] = at(id), c.End
		switch {
		case id.Case == cc.InitDeclaratorInit:
			ends[k] = at(id.Token) // '='
		case k+1 < len(ids):
			ends[k] = at(ids[k+1])
		}
		if starts[k] < prev || ends[k] < starts[k] {
			return text
		}
		prev = ends[k]
	}
	var b strings.Builder
	if !hasStorageClass(d) {
		b.WriteString("extern ")
	}
	b.WriteString(strings.TrimSpace(string(src[c.Start:starts[0]])))
	for k, id := range ids {
		dt := strings.TrimRight(strings.TrimSpace(string(src[starts[k]:ends[k]])), ",;")
		if at, ok := id.Declarator.Type().(*cc.ArrayType); ok && at.Len() >= 0 {
			dt = strings.Replace(dt, "[]", fmt.Sprintf("[%d]", at.Len()), 1)
		}
		if k > 0 {
			b.WriteString(",")
		}
		b.WriteString(" ")
		b.WriteString(strings.TrimSpace(dt))
	}
	b.WriteString(";")
	return b.String()
}
