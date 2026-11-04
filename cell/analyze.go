package cell

import (
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/go-embedded-c/c/headers"
	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/cparse"
)

// Kind is what a defined name is.
type Kind int

// The kinds.
const (
	Variable Kind = iota
	Function
	Typedef
	Tag // struct, union or enum tag; the name is "struct s", "union u", "enum e"
	Enumerator
	Macro
)

var kindNames = [...]string{"variable", "function", "typedef", "tag", "enumerator", "macro"}

func (k Kind) String() string { return kindNames[k] }

// Name is a name a cell defines, at its line and column in the cell.
type Name struct {
	Name      string
	Kind      Kind
	Line, Col int
}

// Diagnostic is an error at a line and column of the cell (1-based), and at
// byte Offset of it. Line is 0 and Offset -1 when the error is not in the
// cell (a header, say); Msg then says where.
type Diagnostic struct {
	Line, Col int
	Offset    int
	Msg       string
}

func (d Diagnostic) String() string { return fmt.Sprintf("%d:%d: %s", d.Line, d.Col, d.Msg) }

// Limits bound the work of one analysis.
type Limits struct {
	MaxBytes  int // cell size; DefaultLimits.MaxBytes when 0
	MaxDepth  int // bracket nesting; MaxDepth when 0, and never more
	MaxTokens int // tokens macro replacement may produce; DefaultLimits.MaxTokens when 0
}

// DefaultLimits are used for zero fields. MaxTokens is far above what the
// standard headers and ordinary macros produce, and stops an expansion bomb
// (`#define D(a) ((a)+(a))` nested 40 deep is 2^40 tokens) in well under a
// second.
var DefaultLimits = Limits{MaxBytes: 1 << 20, MaxDepth: MaxDepth, MaxTokens: 1 << 16}

// Context is what earlier cells left for later ones: their declarations as C
// text, and the names they define.
type Context struct {
	Prelude string
	Defined map[string]Kind
}

// Extend returns the context after a cell without diagnostics; a cell with
// diagnostics leaves the context as it was.
func (c Context) Extend(a *Analysis) Context {
	if len(a.Diagnostics) != 0 {
		return c
	}
	d := make(map[string]Kind, len(c.Defined)+len(a.Defines))
	for k, v := range c.Defined {
		d[k] = v
	}
	for _, n := range a.Defines {
		d[n.Name] = n.Kind
	}
	return Context{Prelude: c.Prelude + a.prelude, Defined: d}
}

// Analysis is the result of analysing one cell.
type Analysis struct {
	Defines     []Name
	Uses        []string // names defined by earlier cells that the cell reads, sorted
	Diagnostics []Diagnostic
	Decls       []Chunk // external declarations and directives, in order
	Stmts       []Chunk // statements, run in order by the cell's function
	Hoisted     []Chunk // declarations (also in Decls) whose initialisers run in order with Stmts
	prelude     string  // what the cell adds to Context.Prelude
	hoists      map[int]hoisted
	passes      int // cc/v4 runs, for the tests
}

// Names of the synthesized sources, as they appear in cc/v4 positions.
const (
	preludeName = "<prelude>"
	cellName    = "<cell>"
	wrapperName = "<wrapper>"
)

// headerFS is the file system cc/v4 reads; a test counts its misses.
var headerFS = headers.FS

func config(lim Limits) (*cc.Config, error) {
	abi, err := cparse.ABI()
	if err != nil {
		return nil, err
	}
	return &cc.Config{
		ABI:             abi,
		FS:              headerFS(),
		Predefined:      cparse.Predefined(),
		IncludePaths:    headers.SysIncludePaths,
		SysIncludePaths: headers.SysIncludePaths,
		UnsignedEnums:   true,
		MaxExpansion:    lim.MaxTokens,
	}, nil
}

// Analyze checks one cell in the context of the cells before it, decides
// which of its chunks are declarations and which are statements, and reports
// the names it defines and uses. It never panics on hostile input.
func Analyze(src []byte, ctx Context, lim Limits) *Analysis {
	a := analyze(src, ctx, lim)
	lines := lineStarts(src)
	for i, d := range a.Diagnostics {
		a.Diagnostics[i].Offset = -1
		if d.Line > 0 {
			a.Diagnostics[i].Offset = min(offset(lines, d.Line, d.Col), len(src))
		}
	}
	return a
}

func analyze(src []byte, ctx Context, lim Limits) *Analysis {
	if lim.MaxBytes == 0 {
		lim.MaxBytes = DefaultLimits.MaxBytes
	}
	if lim.MaxDepth <= 0 || lim.MaxDepth > MaxDepth {
		lim.MaxDepth = MaxDepth
	}
	if lim.MaxTokens == 0 {
		lim.MaxTokens = DefaultLimits.MaxTokens
	}
	a := &Analysis{}
	if len(src) > lim.MaxBytes {
		a.Diagnostics = []Diagnostic{{Line: 1, Col: 1, Msg: fmt.Sprintf("cell is %d bytes, the limit is %d", len(src), lim.MaxBytes)}}
		return a
	}
	chunks, err := split(src, lim.MaxDepth)
	if err != nil {
		se := err.(*SyntaxError)
		a.Diagnostics = []Diagnostic{{Line: se.Line, Col: se.Col, Msg: se.Msg}}
		return a
	}
	if a.Diagnostics = vet(src, chunks); len(a.Diagnostics) != 0 {
		return a
	}
	cfg, err := config(lim)
	if err != nil {
		a.Diagnostics = []Diagnostic{{Msg: err.Error()}}
		return a
	}
	lines := lineStarts(src)
	macros := map[string]bool{}
	for name, k := range ctx.Defined {
		if k == Macro {
			macros[name] = true
		}
	}
	for _, n := range macroNames(src, chunks) {
		macros[n.Name] = true
	}
	stmt := make([]bool, len(chunks))
	for i, c := range chunks {
		stmt[i] = c.Kind == Code && certainStatement(src, c, macros)
	}
	asDecl := make([][]rawDiagnostic, len(chunks)) // why a chunk failed as a declaration
	var ast *cc.AST
	// Classify: every other Code chunk starts as a declaration; one cc/v4
	// cannot parse as one, or that parses only as an implicit-int declaration
	// (`x = 3;`, `f(x);`), becomes a statement. Each round moves at least one
	// chunk, so there are at most len(chunks)+1 rounds. The rounds only parse:
	// a type error must not move a chunk, or `int j = i;` would become a local
	// variable instead of a hoisted global.
	for range len(chunks) + 1 {
		var perr error
		a.passes++
		ast, perr = parse(cfg, assemble(src, ctx, chunks, stmt, nil), false)
		if perr == nil {
			moved := false
			for l := ast.TranslationUnit; l != nil; l = l.TranslationUnit {
				ed := l.ExternalDeclaration
				if ed.Case == cc.ExternalDeclarationDecl && ed.Declaration.Case == cc.DeclarationDecl && ed.Declaration.DeclarationSpecifiers == nil {
					if p, ok := cellPosition(ed); ok {
						if i := chunkAt(chunks, lines, p); i >= 0 && !stmt[i] {
							stmt[i], moved = true, true
						}
					}
				}
			}
			if !moved {
				break
			}
			continue
		}
		d := diagnostics(perr)
		i := -1
		if len(d) != 0 && d[0].cell {
			i = chunkAt(chunks, lines, d[0].pos)
		}
		if i >= 0 && stmt[i] && asDecl[i] != nil && !startsStatement(src, chunks[i]) {
			// The chunk fails as a statement too, and does not look like
			// one: the declaration reading has the better message.
			d = asDecl[i]
		}
		if i < 0 || stmt[i] || chunks[i].Kind != Code {
			a.Diagnostics = cellDiagnostics(d)
			return a
		}
		stmt[i], asDecl[i] = true, d
	}
	hoists := map[int]hoisted{}
	for l := ast.TranslationUnit; l != nil; l = l.TranslationUnit {
		ed := l.ExternalDeclaration
		if ed.Case != cc.ExternalDeclarationDecl {
			continue
		}
		if p, ok := cellPosition(ed); ok {
			if i := chunkAt(chunks, lines, p); i >= 0 && !stmt[i] {
				if h, ok := hoistable(src, lines, chunks[i], ed.Declaration); ok {
					hoists[i] = h
				}
			}
		}
	}
	a.hoists = hoists
	for i, c := range chunks {
		switch _, h := hoists[i]; {
		case stmt[i]:
			a.Stmts = append(a.Stmts, c)
		case h:
			a.Hoisted = append(a.Hoisted, c)
			a.Decls = append(a.Decls, c)
		default:
			a.Decls = append(a.Decls, c)
		}
	}
	a.passes++
	ast, err = parse(cfg, assemble(src, ctx, chunks, stmt, hoists), true)
	if err != nil {
		a.Diagnostics = cellDiagnostics(diagnostics(err))
		return a
	}
	a.Defines = defines(src, ast, chunks)
	a.Uses = uses(src, chunks, ctx, a.Defines)
	a.prelude = preludeOf(src, lines, ast, chunks, stmt, hoists)
	return a
}

// parse runs cc/v4 on a synthesized translation unit; a panic inside cc/v4
// becomes an error.
func parse(cfg *cc.Config, tu string, check bool) (ast *cc.AST, err error) {
	defer func() {
		if r := recover(); r != nil {
			ast, err = nil, fmt.Errorf("%s:1:1: internal error in the C front end: %v\n%s", wrapperName, r, debug.Stack())
		}
	}()
	srcs := []cc.Source{
		{Name: "<predefined>", Value: cfg.Predefined},
		{Name: "<builtin>", Value: cc.Builtin},
		{Name: preludeName, Value: tu},
	}
	if check {
		return cc.Translate(cfg, srcs)
	}
	return cc.Parse(cfg, srcs)
}

// assemble builds the translation unit: the prelude, the directives and
// declarations at their cell positions, then the statements (hoisted
// initialisations included, in source order) wrapped in a function.
func assemble(src []byte, ctx Context, chunks []Chunk, stmt []bool, hoists map[int]hoisted) string {
	var b strings.Builder
	b.WriteString(ctx.Prelude)
	emit := func(c Chunk, text string) {
		fmt.Fprintf(&b, "\n#line %d \"%s\"\n%s%s\n", c.Line, cellName, strings.Repeat(" ", c.Col-1), text)
	}
	for i, c := range chunks {
		if h, ok := hoists[i]; ok {
			emit(c, h.decl)
		} else if !stmt[i] {
			emit(c, string(src[c.Start:c.End]))
		}
	}
	fmt.Fprintf(&b, "\n#line 1 \"%s\"\nvoid __cell(void) {\n", wrapperName)
	for i, c := range chunks {
		if h, ok := hoists[i]; ok {
			emit(c, h.stmt)
		} else if stmt[i] {
			emit(c, string(src[c.Start:c.End]))
		}
	}
	fmt.Fprintf(&b, "\n#line 2 \"%s\"\n}\n", wrapperName)
	return b.String()
}

type position struct{ Line, Column int }

type rawDiagnostic struct {
	cell bool
	pos  position
	msg  string // the whole line when not in the cell
}

// diagnostics splits a cc/v4 error, one "file:line:col: message" per line.
func diagnostics(err error) []rawDiagnostic {
	var r []rawDiagnostic
	for _, line := range strings.Split(err.Error(), "\n") {
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, cellName+":"); ok {
			parts := strings.SplitN(rest, ":", 3)
			if len(parts) == 3 {
				l, err1 := strconv.Atoi(parts[0])
				c, err2 := strconv.Atoi(parts[1])
				if err1 == nil && err2 == nil {
					r = append(r, rawDiagnostic{cell: true, pos: position{l, c}, msg: strings.TrimSpace(parts[2])})
					continue
				}
			}
		}
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "goroutine ") {
			continue // the stack of a recovered panic
		}
		r = append(r, rawDiagnostic{msg: line})
	}
	return r
}

func cellDiagnostics(raw []rawDiagnostic) []Diagnostic {
	var r []Diagnostic
	for _, d := range raw {
		if d.cell {
			r = append(r, Diagnostic{Line: d.pos.Line, Col: d.pos.Column, Msg: d.msg})
		} else {
			r = append(r, Diagnostic{Msg: d.msg})
		}
	}
	if len(r) == 0 {
		r = []Diagnostic{{Msg: "the C front end failed without a message"}}
	}
	return r
}

// lineStarts returns the byte offset of each line of src.
func lineStarts(src []byte) []int {
	r := []int{0}
	for i, b := range src {
		if b == '\n' {
			r = append(r, i+1)
		}
	}
	return r
}

// offset converts a cell line and column to a byte offset, or -1.
func offset(lines []int, line, col int) int {
	if line < 1 || line > len(lines) || col < 1 {
		return -1
	}
	return lines[line-1] + col - 1
}

// chunkAt returns the index of the chunk holding a cell position, or -1.
func chunkAt(chunks []Chunk, lines []int, p position) int {
	off := offset(lines, p.Line, p.Column)
	if off < 0 {
		return -1
	}
	for i, ch := range chunks {
		if off >= ch.Start && off <= ch.End {
			return i
		}
	}
	return -1
}

// cellPosition returns the cell position of a node, if it is in the cell.
func cellPosition(n cc.Node) (position, bool) {
	p := n.Position()
	return position{p.Line, p.Column}, p.Filename == cellName
}

// certainStatement reports whether a chunk can only be a statement, judged
// from its first tokens, so that classification need not spend a cc/v4 round
// on it. No declaration starts with a statement keyword or '{'; none has an
// identifier followed by an assignment, '++', '--', '.', '->' or '['; and in
// `id ( x`, a declarator cannot start with a literal or a unary operator.
// Anything else is left to cc/v4, and so is a chunk starting with a macro the
// cell or an earlier one defines (`ALIGNED(16) int x;` may expand to a
// declaration).
func certainStatement(src []byte, c Chunk, macros map[string]bool) bool {
	if startsStatement(src, c) {
		return true
	}
	s := &scanner{src: src[:c.End], off: c.Start, line: c.Line, col: c.Col}
	var toks []token
	for len(toks) < 3 {
		if _, err := s.skipSpace(); err != nil || s.off >= c.End {
			break
		}
		t, err := s.lex()
		if err != nil {
			break
		}
		toks = append(toks, t)
	}
	if len(toks) < 2 || toks[0].word == "" || isDeclKeyword(toks[0].word) || macros[toks[0].word] {
		return false
	}
	switch p := toks[1].punct; {
	case p == '=' || p == opAssign || p == '.' || p == '[':
		return true
	case (p == '+' || p == '-') && len(toks) > 2 && toks[2].punct == p && toks[2].start == toks[1].start+1:
		return true // ++ or --
	case p == '-' && len(toks) > 2 && toks[2].punct == '>' && toks[2].start == toks[1].start+1:
		return true // ->
	case p == '(' && len(toks) > 2:
		t := toks[2]
		if t.word == "" && t.punct == 0 {
			return true // a string, character or number literal
		}
		switch t.punct {
		case '&', '-', '+', '!', '~':
			return true
		}
	}
	return false
}

// isDeclKeyword reports whether a word can start a declaration.
func isDeclKeyword(w string) bool {
	switch w {
	case "typedef", "extern", "static", "auto", "register", "_Thread_local", "__thread",
		"void", "char", "short", "int", "long", "float", "double", "signed", "unsigned", "_Bool", "bool",
		"_Complex", "struct", "union", "enum", "const", "volatile", "restrict", "_Atomic",
		"inline", "_Noreturn", "_Alignas", "alignas", "_Static_assert", "static_assert", "typeof", "__typeof__",
		"__attribute__", "__extension__", "__auto_type", "__int128":
		return true
	}
	return false
}

// startsStatement reports whether a chunk begins with a statement keyword or
// a brace.
func startsStatement(src []byte, c Chunk) bool {
	if src[c.Start] == '{' {
		return true
	}
	w := c.Start
	for w < c.End && isIdentByte(src[w]) {
		w++
	}
	switch string(src[c.Start:w]) {
	case "if", "for", "while", "do", "switch", "return", "goto", "break", "continue", "case", "default":
		return true
	}
	return false
}
