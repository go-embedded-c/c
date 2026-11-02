// Package cell turns the text of a notebook C cell into a translation unit
// cc/v4 can check, and reports what the cell defines and uses.
//
// A cell mixes what C allows at file scope (external declarations,
// preprocessor directives) with what it does not (statements). Split cuts
// the text into top-level chunks; the analyzer then lets cc/v4 decide which
// chunks are declarations and wraps the others in a function
// (bib/c-cpp.md §2.5, go-scikit/roadmap).
package cell

import "fmt"

// ChunkKind says what a chunk is, as far as the splitter can tell without a
// parser.
type ChunkKind int

// The kinds.
const (
	Directive ChunkKind = iota // a preprocessor line, continuations included
	Code                       // a declaration or a statement, to be classified
)

// Chunk is a top-level piece of a cell: Src[Start:End] of the cell text,
// starting at Line and Col (both 1-based).
type Chunk struct {
	Kind       ChunkKind
	Start, End int
	Line, Col  int
	// Directives are the preprocessor lines inside a Code chunk (in a
	// function body, say); the splitter skips them like comments.
	Directives []Chunk
}

// SyntaxError is a cell the splitter cannot cut: an unterminated comment,
// string or bracket. Line and Col are 1-based.
type SyntaxError struct {
	Line, Col int
	Msg       string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("%d:%d: %s", e.Line, e.Col, e.Msg) }

// MaxDepth bounds bracket nesting, so that a hostile cell costs memory
// proportional to its length and no more.
const MaxDepth = 256

type scanner struct {
	src       []byte
	off       int
	line, col int
	maxDepth  int
}

func (s *scanner) peekByte(k int) byte {
	if s.off+k < len(s.src) {
		return s.src[s.off+k]
	}
	return 0
}

func (s *scanner) next() {
	if s.src[s.off] == '\n' {
		s.line++
		s.col = 1
	} else {
		s.col++
	}
	s.off++
}

func (s *scanner) errorf(line, col int, format string, args ...any) error {
	return &SyntaxError{Line: line, Col: col, Msg: fmt.Sprintf(format, args...)}
}

// skipSpace skips white space and comments; it reports whether a newline was
// crossed.
func (s *scanner) skipSpace() (newline bool, err error) {
	for s.off < len(s.src) {
		switch c := s.src[s.off]; {
		case c == '\n':
			newline = true
			s.next()
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v':
			s.next()
		case c == '\\' && s.peekByte(1) == '\n':
			s.next()
			s.next()
		case c == '/' && s.peekByte(1) == '/':
			for s.off < len(s.src) && s.src[s.off] != '\n' {
				s.next()
			}
		case c == '/' && s.peekByte(1) == '*':
			line, col := s.line, s.col
			s.next()
			s.next()
			for {
				if s.off >= len(s.src) {
					return newline, s.errorf(line, col, "unterminated comment")
				}
				if s.src[s.off] == '*' && s.peekByte(1) == '/' {
					s.next()
					s.next()
					break
				}
				if s.src[s.off] == '\n' {
					newline = true
				}
				s.next()
			}
		default:
			return newline, nil
		}
	}
	return newline, nil
}

// token is one lexical token of the splitter's coarse grammar: a word
// (identifier, keyword or number), a literal, or a single punctuator byte.
type token struct {
	word      string // identifier or keyword; empty otherwise
	punct     byte   // punctuator byte; 0 otherwise
	start     int
	line, col int
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c >= 0x80
}

func isIdentByte(c byte) bool { return isIdentStart(c) || '0' <= c && c <= '9' }

// lex reads the token at s.off, which is not white space.
func (s *scanner) lex() (token, error) {
	t := token{start: s.off, line: s.line, col: s.col}
	c := s.src[s.off]
	switch {
	case isIdentStart(c):
		for s.off < len(s.src) && isIdentByte(s.src[s.off]) {
			s.next()
		}
		t.word = string(s.src[t.start:s.off])
		if (t.word == "L" || t.word == "u" || t.word == "U" || t.word == "u8") && s.off < len(s.src) && (s.src[s.off] == '"' || s.src[s.off] == '\'') {
			t.word = ""
			return t, s.literal()
		}
	case '0' <= c && c <= '9' || c == '.' && '0' <= s.peekByte(1) && s.peekByte(1) <= '9':
		// A pp-number: digits, letters, '.', and a sign after an exponent.
		for s.off < len(s.src) {
			d := s.src[s.off]
			if (d == '+' || d == '-') && (s.src[s.off-1]|0x20 == 'e' || s.src[s.off-1]|0x20 == 'p') {
				s.next()
				continue
			}
			if !isIdentByte(d) && d != '.' && d != '\'' {
				break
			}
			s.next()
		}
	case c == '"' || c == '\'':
		return t, s.literal()
	case s.peekByte(1) == '=' && (c == '+' || c == '-' || c == '*' || c == '/' || c == '%' || c == '&' || c == '|' || c == '^' || c == '<' || c == '>' || c == '!' || c == '='):
		// Two-byte operators ending in '=': a compound assignment, or a
		// comparison, never a plain '='.
		t.punct = opCompare
		if c != '<' && c != '>' && c != '!' && c != '=' {
			t.punct = opAssign
		}
		s.next()
		s.next()
	default:
		t.punct = c
		s.next()
	}
	return t, nil
}

// Punctuator codes for the two-byte operators the splitter distinguishes;
// both are bytes no C punctuator uses.
const (
	opAssign  = 'A' // += -= *= /= %= &= |= ^=
	opCompare = 'C' // == != <= >=
)

// literal consumes a string or character literal starting at s.off.
func (s *scanner) literal() error {
	q, line, col := s.src[s.off], s.line, s.col
	s.next()
	for {
		if s.off >= len(s.src) || s.src[s.off] == '\n' {
			return s.errorf(line, col, "unterminated %s literal", map[byte]string{'"': "string", '\'': "character"}[q])
		}
		switch s.src[s.off] {
		case '\\':
			s.next()
			if s.off < len(s.src) {
				s.next()
			}
		case q:
			s.next()
			return nil
		default:
			s.next()
		}
	}
}

// Split cuts a cell into top-level chunks. A Code chunk ends at a ';' or a
// '}' outside any bracket, and runs on past it when C does:
//
//   - a '{' opened after struct, union or enum belongs to a declaration that
//     ends at the next ';' (`struct s {…} v;`), and so does any '{' after an
//     assignment (`int a[] = {…};`, `p = (struct s){…};`);
//   - `else` after a statement continues the if;
//   - `while` after the body of a `do` continues the do.
//
// Any other '}' that closes the outermost bracket ends the chunk: a function
// definition, a compound statement, the body of if/for/while/switch.
func Split(src []byte) ([]Chunk, error) { return split(src, MaxDepth) }

// split is Split with brackets nested at most depth deep.
func split(src []byte, depth int) ([]Chunk, error) {
	s := &scanner{src: src, line: 1, col: 1, maxDepth: depth}
	var chunks []Chunk
	atLineStart := true
	for {
		nl, err := s.skipSpace()
		if err != nil {
			return nil, err
		}
		atLineStart = atLineStart || nl
		if s.off >= len(src) {
			return chunks, nil
		}
		if atLineStart && src[s.off] == '#' {
			c, err := s.directive()
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, c)
			atLineStart = true
			continue
		}
		c, err := s.code()
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
		atLineStart = false
	}
}

// directive consumes a preprocessor line, with its backslash continuations
// and any comment that spans lines.
func (s *scanner) directive() (Chunk, error) {
	c := Chunk{Kind: Directive, Start: s.off, Line: s.line, Col: s.col}
	for s.off < len(s.src) {
		switch ch := s.src[s.off]; {
		case ch == '\n':
			c.End = s.off
			return c, nil
		case ch == '\\' && s.peekByte(1) == '\n':
			s.next()
			s.next()
		case ch == '/' && (s.peekByte(1) == '*' || s.peekByte(1) == '/'):
			if _, err := s.skipComment(); err != nil {
				return c, err
			}
		case ch == '"' || ch == '\'':
			if err := s.literal(); err != nil {
				return c, err
			}
		default:
			s.next()
		}
	}
	c.End = s.off
	return c, nil
}

// skipComment consumes one comment at s.off; a line comment stops before its
// newline.
func (s *scanner) skipComment() (bool, error) {
	if s.peekByte(1) == '/' {
		for s.off < len(s.src) && s.src[s.off] != '\n' {
			s.next()
		}
		return false, nil
	}
	line, col := s.line, s.col
	s.next()
	s.next()
	for s.off < len(s.src) {
		if s.src[s.off] == '*' && s.peekByte(1) == '/' {
			s.next()
			s.next()
			return true, nil
		}
		s.next()
	}
	return false, s.errorf(line, col, "unterminated comment")
}

var closer = map[byte]byte{'(': ')', '[': ']', '{': '}'}

// code consumes one Code chunk.
func (s *scanner) code() (Chunk, error) {
	c := Chunk{Kind: Code, Start: s.off, Line: s.line, Col: s.col}
	type open struct {
		b         byte
		line, col int
	}
	var stack []open
	declBrace := false // the outermost '{' belongs to a declaration
	tagged := false    // struct/union/enum seen, no punctuator since (outside brackets)
	assigned := false  // an assignment outside brackets: braces after it are initializers or compound literals
	pendingDo := 0     // `do` statements still waiting for their `while`
	for {
		nl, err := s.skipSpace()
		if err != nil {
			return c, err
		}
		if nl && s.off < len(s.src) && s.src[s.off] == '#' {
			d, err := s.directive()
			if err != nil {
				return c, err
			}
			c.Directives = append(c.Directives, d)
			continue
		}
		if s.off >= len(s.src) {
			if len(stack) != 0 {
				o := stack[len(stack)-1]
				return c, s.errorf(o.line, o.col, "unclosed %q", o.b)
			}
			c.End = s.off
			return c, nil
		}
		t, err := s.lex()
		if err != nil {
			return c, err
		}
		outside := len(stack) == 0
		switch t.punct {
		case '(', '[', '{':
			if len(stack) == s.maxDepth {
				return c, s.errorf(t.line, t.col, "brackets nested deeper than %d", s.maxDepth)
			}
			if t.punct == '{' && outside {
				declBrace = assigned || tagged
			}
			stack = append(stack, open{t.punct, t.line, t.col})
		case ')', ']', '}':
			if outside || closer[stack[len(stack)-1].b] != t.punct {
				return c, s.errorf(t.line, t.col, "unexpected %q", t.punct)
			}
			stack = stack[:len(stack)-1]
			if t.punct == '}' && len(stack) == 0 && !declBrace && !s.continues(&pendingDo) {
				c.End = s.off
				return c, nil
			}
		case ';':
			if outside && !s.continues(&pendingDo) {
				c.End = s.off
				return c, nil
			}
		}
		if outside {
			switch {
			case t.word == "do":
				pendingDo++
			case t.word == "struct" || t.word == "union" || t.word == "enum":
				tagged = true
			case t.punct != 0 && t.punct != '{':
				tagged = false
			}
			assigned = assigned || t.punct == '=' || t.punct == opAssign
		}
	}
}

// continues reports whether the statement that just ended at a top-level ';'
// or '}' goes on: an `else`, or the `while` of a pending `do`. It consumes
// nothing.
func (s *scanner) continues(pendingDo *int) bool {
	save := *s
	defer func() { *s = save }()
	if _, err := s.skipSpace(); err != nil || s.off >= len(s.src) || !isIdentStart(s.src[s.off]) {
		return false
	}
	t, err := s.lex()
	if err != nil {
		return false
	}
	switch {
	case t.word == "else":
		return true
	case t.word == "while" && *pendingDo > 0:
		*pendingDo--
		return true
	}
	return false
}
