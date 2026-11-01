package cell

import (
	"io/fs"
	"path"
	"strings"
	"sync"

	"github.com/go-embedded-c/c/headers"
)

// cc/v4 v4.30.5 opens a file it does not find in Config.FS with os.Open
// (scanner.go:268-279). On a desktop kernel a cell could therefore read host
// files with `#include "../../x"`; TestGuardIsNotVacuous reproduces it. cc/v4
// also macro-expands the operand of #include, the <…> form included
// (cpp.go:1801), so redefining a word of a path an embedded header includes
// (`#define stdio ../../x`) could redirect that include; this second path was
// not reproduced (the test logs why), and is guarded anyway. The guard runs
// before cc/v4 sees the cell:
//
//   - only the directives below are accepted;
//   - #include takes a literal path, relative, without "..", present in the
//     embedded tree;
//   - #define and #undef refuse every identifier that occurs in an #include
//     path, the embedded headers' and the cell's;
//   - __has_include is refused (it would stat host paths).
var allowedDirectives = map[string]bool{
	"": true, "define": true, "undef": true, "include": true,
	"if": true, "ifdef": true, "ifndef": true, "elif": true, "elifdef": true, "elifndef": true,
	"else": true, "endif": true, "error": true, "warning": true, "pragma": true,
}

// pathWords is the set of identifiers occurring in the #include paths of the
// embedded headers.
var pathWords = sync.OnceValue(func() map[string]bool {
	words := map[string]bool{}
	_ = fs.WalkDir(headers.FS(), "include", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".h") {
			return err
		}
		b, err := fs.ReadFile(headers.FS(), p)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			name, rest := directiveName(line)
			if name == "include" || name == "include_next" {
				addWords(words, includeOperand(rest))
			}
		}
		return nil
	})
	return words
})

func addWords(words map[string]bool, s string) {
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return r >= 0x80 || !isIdentByte(byte(r)) }) {
		if isIdentStart(w[0]) {
			words[w] = true
		}
	}
}

// includeOperand returns the <…> or "…" operand at the start of an #include
// line's rest, without what follows it (a comment, say), or the whole rest
// when it is not literal.
func includeOperand(rest string) string {
	rest = strings.TrimSpace(stripComments(rest))
	if rest == "" {
		return rest
	}
	end := map[byte]byte{'<': '>', '"': '"'}[rest[0]]
	if end == 0 {
		return rest
	}
	if i := strings.IndexByte(rest[1:], end); i >= 0 {
		return rest[:i+2]
	}
	return rest
}

// directiveName splits a preprocessor line into its name and the rest; name
// is "-" when the line is not a directive.
func directiveName(line string) (name, rest string) {
	line = strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(line, "#") {
		return "-", ""
	}
	line = strings.TrimLeft(line[1:], " \t")
	i := 0
	for i < len(line) && isIdentByte(line[i]) {
		i++
	}
	return line[:i], strings.TrimSpace(line[i:])
}

// includeFile reports whether an #include operand names a file of the
// embedded tree, and returns the path inside the quotes or brackets.
func includeFile(operand string) (string, bool) {
	if len(operand) < 2 {
		return "", false
	}
	if !(operand[0] == '<' && operand[len(operand)-1] == '>' || operand[0] == '"' && operand[len(operand)-1] == '"') {
		return "", false
	}
	p := operand[1 : len(operand)-1]
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || path.Clean(p) != p || strings.HasPrefix(p, "..") {
		return p, false
	}
	for _, dir := range headers.SysIncludePaths {
		if fi, err := fs.Stat(headers.FS(), dir+"/"+p); err == nil && fi.Mode().IsRegular() {
			return p, true
		}
	}
	return p, false
}

// vet checks the directives of a cell, returning one diagnostic per refused
// line.
func vet(src []byte, chunks []Chunk) []Diagnostic {
	var all []Chunk
	for _, c := range chunks {
		if c.Kind == Directive {
			all = append(all, c)
		}
		all = append(all, c.Directives...)
	}
	cellWords := map[string]bool{}
	type directive struct {
		c          Chunk
		name, rest string
	}
	var ds []directive
	for _, c := range all {
		text := strings.ReplaceAll(string(src[c.Start:c.End]), "\\\n", " ")
		name, rest := directiveName(stripComments(text))
		if name == "include" {
			rest = includeOperand(rest)
			if rest != "" && (rest[0] == '<' || rest[0] == '"') {
				addWords(cellWords, rest)
			}
		}
		ds = append(ds, directive{c, name, rest})
	}
	var diags []Diagnostic
	refuse := func(c Chunk, msg string) { diags = append(diags, Diagnostic{Line: c.Line, Col: c.Col, Msg: msg}) }
	for _, d := range ds {
		switch {
		case !allowedDirectives[d.name]:
			refuse(d.c, "#"+d.name+" is not allowed in a notebook cell")
		case strings.Contains(d.rest, "__has_include"):
			refuse(d.c, "__has_include is not allowed in a notebook cell")
		case d.name == "include":
			if p, ok := includeFile(d.rest); !ok {
				if p == "" {
					refuse(d.c, "#include needs a literal <file> or \"file\" in a notebook cell")
				} else {
					refuse(d.c, "#include: "+p+": no such header (cells can include the C library and clang's headers)")
				}
			}
		case d.name == "define" || d.name == "undef":
			name := d.rest
			for i := 0; i < len(name); i++ {
				if !isIdentByte(name[i]) {
					name = name[:i]
					break
				}
			}
			if pathWords()[name] || cellWords[name] {
				refuse(d.c, "#"+d.name+" "+name+": the name occurs in an #include path, so a cell cannot define it")
			}
		}
	}
	return diags
}

// stripComments replaces the comments of one directive with a space.
func stripComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '/' && i+1 < len(s) && s[i+1] == '*':
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return b.String()
			}
			b.WriteByte(' ')
			i += j + 3
		case s[i] == '/' && i+1 < len(s) && s[i+1] == '/':
			return b.String()
		case s[i] == '"' || s[i] == '\'':
			q := s[i]
			b.WriteByte(q)
			for i++; i < len(s) && s[i] != q; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					b.WriteByte(s[i])
					i++
				}
				b.WriteByte(s[i])
			}
			if i < len(s) {
				b.WriteByte(q)
			}
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
