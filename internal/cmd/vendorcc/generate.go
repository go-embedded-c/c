package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
)

// Version is the cc/v4 release internal/cc is made from.
const Version = "v4.30.5"

// A filePatch replaces exactly one occurrence of Old in File; any other count
// is an error, so that a new cc/v4 release cannot silently skip a patch.
type filePatch struct {
	File, Old, New, Why string
}

var patches = []filePatch{
	{
		File: "cc.go",
		Why:  "a budget for macro replacement (go-embedded-c/c#11)",
		Old:  "\tHasBuiltin func(name string) bool\n",
		New: "\tHasBuiltin func(name string) bool\n\n" +
			"\t// MaxExpansion, when positive, bounds the tokens macro replacement may\n" +
			"\t// produce in one translation unit. Past it the translation unit is in\n" +
			"\t// error and every further replacement is empty (go-embedded-c patch).\n" +
			"\tMaxExpansion int\n\n" +
			"\t// FunctionAlign, when positive, is __alignof__ of a function without an\n" +
			"\t// aligned attribute, the target's; cc/v4 says 1 (go-embedded-c patch).\n" +
			"\tFunctionAlign int\n",
	},
	{
		File: "cpp.go",
		Why:  "the count the budget is checked against",
		Old:  "type cpp struct {\n\tcfg         *Config\n",
		New:  "type cpp struct {\n\tcfg         *Config\n\texpanded    int // tokens produced by macro replacement (go-embedded-c patch)\n",
	},
	{
		File: "cpp.go",
		Why:  "count an object-like replacement",
		Old:  "\t\t\tsubst = c.subst(eval, m, repl, nil, nil, HS.add(src), nil)\n",
		New:  "\t\t\tsubst = c.subst(eval, m, repl, nil, nil, HS.add(src), nil)\n\t\t\tsubst = c.budget(T, subst)\n",
	},
	{
		File: "cpp.go",
		Why:  "count a function-like replacement, arguments substituted",
		Old:  "\t\t\t\tsubst = c.subst(eval, m, m.ts(), m.fp(), args, HS.cap(rparen.hs).add(src), nil)\n",
		New:  "\t\t\t\tsubst = c.subst(eval, m, m.ts(), m.fp(), args, HS.cap(rparen.hs).add(src), nil)\n\t\t\t\tsubst = c.budget(T, subst)\n",
	},
	{
		File: "cpp.go",
		Why:  "`#define M` with nothing after the name, at the end of input, indexed past the line (go-embedded-c/c#2)",
		Old:  "\tnm := ln[0]\n\tln = ln[1:]\n\tswitch {\n",
		New: "\tnm := ln[0]\n\tif nm.Ch != rune(IDENTIFIER) {\n" +
			"\t\tc.eh(\"%v: macro name must be an identifier\", nm.Position())\n\t\treturn\n\t}\n\n" +
			"\tln = ln[1:]\n\tif len(ln) == 0 {\n\t\tc.defineObjectMacro(nm, nil)\n\t\treturn\n\t}\n\n\tswitch {\n",
	},
	{
		File: "cpp.go",
		Why:  "`#define` alone at the end of input: no macro name token (go-embedded-c/c#2)",
		Old:  "func (c *cpp) define(ln controlLine) {\n\tdef := ln[1]\n",
		New:  "func (c *cpp) define(ln controlLine) {\n\tif len(ln) < 2 {\n\t\tc.eh(\"%v: missing macro name\", ln[0].Position())\n\t\treturn\n\t}\n\n\tdef := ln[1]\n",
	},
	{
		File: "cpp.go",
		Why:  "an invocation unterminated at the end of input is an error, not a panic (go-embedded-c/c#2)",
		Old:  "\t\tcase eof:\n\t\t\tpanic(todo(\"\", &t))\n",
		New:  "\t\tcase eof:\n\t\t\tc.eh(\"%v: unterminated argument list invoking a macro\", t.Position())\n\t\t\treturn nil, t, false\n",
	},
	{
		File: "cpp.go",
		Why:  "and its caller then emits the macro name as is, before the end of input it consumed",
		Old:  "\t\t\t\tif !ok {\n\t\t\t\t\tpanic(todo(\"\"))\n\t\t\t\t\t// return r\n\t\t\t\t}\n",
		New:  "\t\t\t\tif !ok {\n\t\t\t\t\tend := cppTokens{rparen}\n\t\t\t\t\tTS.prepend(&end)\n\t\t\t\t\tgoto ret\n\t\t\t\t}\n",
	},
	{
		File: "check.go",
		Why:  "a struct member that failed to type-check is no field (go-embedded-c/c#2)",
		Old:  "\tfor i, f := range fields {\n\t\tf.index = i\n",
		New:  "\tfields = nonNilFields(fields)\n\tfor i, f := range fields {\n\t\tf.index = i\n",
	},
	{
		File: "check.go",
		Why:  "a bit-field width that is not an integer constant leaves no field to type (go-embedded-c/c#2)",
		Old:  "\t\t\tn.Declarator.fixVolatileAndConst(c)\n\t\t\tr.typ.typ = n.Declarator.Type()\n",
		New:  "\t\t\tif r == nil {\n\t\t\t\treturn\n\t\t\t}\n\n\t\t\tn.Declarator.fixVolatileAndConst(c)\n\t\t\tr.typ.typ = n.Declarator.Type()\n",
	},
	{
		File: "parser.go",
		Why:  "`^` alone: the type-qualifier loop shifted the end of input forever (go-embedded-c/c#2)",
		Old:  "\t\tcase p.isTypeQualifier(ch):\n\t\t\tr = &TypeQualifiers{Case: TypeQualifiersTypeQual, TypeQualifiers: r, TypeQualifier: p.typeQualifier(acceptAttributes)}\n\t\tdefault:\n",
		New:  "\t\tcase p.isTypeQualifier(ch):\n\t\t\tr = &TypeQualifiers{Case: TypeQualifiersTypeQual, TypeQualifiers: r, TypeQualifier: p.typeQualifier(acceptAttributes)}\n\t\tcase ch == eof:\n\t\t\tp.cpp.eh(\"%v: unexpected EOF, expected type qualifier\", p.toks[0].Position())\n\t\t\treturn r\n\t\tdefault:\n",
	},
	{
		File: "parser.go",
		Why:  "any other loop that shifts the end of input without progress: count those shifts",
		Old:  "type parser struct {\n\tcpp        *cpp\n",
		New:  "type parser struct {\n\tcpp        *cpp\n\teofShifts  int // shifts of the end of input (go-embedded-c patch)\n",
	},
	{
		File: "parser.go",
		Why:  "and stop the parser past maxEOFShifts",
		Old:  "\tr = p.toks[0]\n\tif r.Ch != eof {\n",
		New:  "\tr = p.toks[0]\n\tif r.Ch == eof {\n\t\tp.noProgress(r)\n\t}\n\tif r.Ch != eof {\n",
	},
	{
		File: "cc.go",
		Why:  "the stop is a panic, recovered into an error at the parser's entry",
		Old:  "\tast, err := p.parse()\n\treturn p, ast, err\n}\n",
		New:  "\tast, err := p.parseBounded()\n\treturn p, ast, err\n}\n",
	},
	{
		File: "parser.go",
		Why:  "a panic unwinding a function definition found r nil",
		Old:  "\tdefer func() {\n\t\tr.scope = p.fnScope\n\t\tp.fnScope = nil\n\t}()\n",
		New:  "\tdefer func() {\n\t\tif r != nil {\n\t\t\tr.scope = p.fnScope\n\t\t}\n\t\tp.fnScope = nil\n\t}()\n",
	},
	{
		File: "parser.go",
		Why:  "and unwinding a compound statement",
		Old:  "\t\tdefer func() {\n\t\t\tr.gotos = p.gotos\n",
		New:  "\t\tdefer func() {\n\t\t\tif r == nil {\n\t\t\t\treturn\n\t\t\t}\n\n\t\t\tr.gotos = p.gotos\n",
	},
	{
		File: "cpp.go",
		Why:  "`#define M##`: `##` at either end of a replacement list (6.10.3.3p1) pasted with nothing and panicked (go-embedded-c/c#2)",
		Old:  "\tm, err := newMacro(nm, params, replList, minArgs, varArg, isFnLike)\n",
		New: "\tif n := len(replList); n != 0 && (replList[0].Ch == rune(PPPASTE) || replList[n-1].Ch == rune(PPPASTE)) {\n" +
			"\t\tc.eh(\"%v: '##' cannot appear at either end of a macro expansion\", nm.Position())\n\t\treturn\n\t}\n\n" +
			"\tm, err := newMacro(nm, params, replList, minArgs, varArg, isFnLike)\n",
	},
	{
		File: "cpp.go",
		Why:  "`#define M(A,...) __VA_ARGS__` then `M()`: no argument for the variadic part sliced past the arguments (go-embedded-c/c#2)",
		Old:  "\treturn c.varArgs(t, AP[i:])\n}\n",
		New:  "\tif i >= len(AP) {\n\t\treturn nil\n\t}\n\n\treturn c.varArgs(t, AP[i:])\n}\n",
	},
	{
		File: "scanner.go",
		Why:  "a string literal unterminated at the end of input: sChar shifted EOF forever (go-embedded-c/c#2)",
		Old:  "\tcase '\"', '\\n':\n\t\treturn false\n",
		New:  "\tcase '\"', '\\n', eof:\n\t\treturn false\n",
	},
	{
		File: "scanner.go",
		Why:  "and the same in a character constant",
		Old:  "\tcase '\\'', '\\n':\n\t\treturn false\n",
		New:  "\tcase '\\'', '\\n', eof:\n\t\treturn false\n",
	},
	{
		File: "parser.go",
		Why:  "`0={#,`: an initializer list whose first item failed linked the next onto nil (go-embedded-c/c#2)",
		Old:  "\t\t\til = &InitializerList{Token: p.shift(false), Initializer: p.initializer(parent)}\n\t\t}\n\t\tprev.InitializerList = il\n",
		New:  "\t\t\til = &InitializerList{Token: p.shift(false), Initializer: p.initializer(parent)}\n\t\t}\n\t\tif prev == nil {\n\t\t\tr, prev = il, il\n\t\t\tcontinue\n\t\t}\n\n\t\tprev.InitializerList = il\n",
	},
	{
		File: "scanner.go",
		Why:  "`\"\"\\`: the scanner's offset ran past the end of input and the position lookup panicked (go-embedded-c/c#2)",
		Old:  "func (s *scannerSource) pos(off uint32) token.Position {\n\treturn s.file.PositionFor(",
		New:  "func (s *scannerSource) pos(off uint32) token.Position {\n\tif off > s.len {\n\t\toff = s.len\n\t}\n\treturn s.file.PositionFor(",
	},
	{
		File: "scanner.go",
		Why:  "and the EOF token fail returns carried that offset",
		Old:  "func (s *scanner) fail() Token {\n",
		New:  "func (s *scanner) fail() Token {\n\tif s.off > s.s.len {\n\t\ts.off = s.s.len\n\t}\n",
	},
	{
		File: "etc.go",
		Why:  "`0\\n\\\\`: a token's offset past the end of the last file panicked in the position lookup (go-embedded-c/c#2)",
		Old:  "\tit = f.items[l-1]\n\treturn it.file.Position(mtoken.Pos(pos - it.off))\n",
		New:  "\tit = f.items[l-1]\n\tp := mtoken.Pos(pos - it.off)\n\tif end := it.file.Pos(it.file.Size()); p > end {\n\t\tp = end\n\t}\n\treturn it.file.Position(p)\n",
	},
	{
		File: "check.go",
		Why:  "enumeration constants have type int (C11 6.7.2.2p3) when every value fits; UnsignedEnums gave them the unsigned type of the enum, so `i >= FOO` compared unsigned (gcc torture enum-2.c)",
		Old:  "\t\tfor _, v := range list {\n\t\t\tv.typ = t\n\t\t}\n",
		New:  "\t\tfor _, v := range list {\n\t\t\tv.typ = t\n\t\t\tif ts == nil && min >= math.MinInt32 && max <= math.MaxInt32 {\n\t\t\t\tv.typ = c.intT\n\t\t\t}\n\t\t}\n",
	},
	{
		File: "check.go",
		Why:  "an unnamed parameter of an invalid type (void f(int float)): DeclarationSpecifiers.check returns nil, and .Decay() on it panicked (FuzzSource, CI run 42491474333)",
		Old:  "\t\tt := n.AbstractDeclarator.check(c, n.DeclarationSpecifiers.check(c, &isExtern, &isStatic, &isAtomic, &isThreadLocal, &isConst, &isVolatile, &isInline, &isRegister, &isAuto, &isNoreturn, &isRestrict, &alignas)).Decay()\n",
		New:  "\t\tt := n.AbstractDeclarator.check(c, n.DeclarationSpecifiers.check(c, &isExtern, &isStatic, &isAtomic, &isThreadLocal, &isConst, &isVolatile, &isInline, &isRegister, &isAuto, &isNoreturn, &isRestrict, &alignas))\n\t\tif t == nil {\n\t\t\tt = Invalid\n\t\t}\n\t\tt = t.Decay()\n",
	},
	{
		File: "check.go",
		Why:  "GNU __alignof__ of an expression does not decay it, as sizeof does not: a function aligned(256) has alignment 256, not a pointer's (gcc torture align-3.c)",
		Old:  "\t\tt := n.UnaryExpression.check(c, mode.add(decay))\n\t\tn.val, n.typ = UInt64Value(t.Align()), c.sizeT(n)\n",
		New:  "\t\tt := n.UnaryExpression.check(c, mode).Undecay()\n\t\tn.val, n.typ = UInt64Value(t.Align()), c.sizeT(n)\n",
	},
	{
		File: "type.go",
		Why:  "and FunctionType.Align returns it",
		Old:  "\t\t\treturn int(v)\n\t\t}\n\t}\n\n\treturn 1\n}\n\n// Decay implements Type.\nfunc (n *FunctionType) Decay()",
		New:  "\t\t\treturn int(v)\n\t\t}\n\t}\n\n\tif n.c != nil && n.c.cfg.FunctionAlign > 0 {\n\t\treturn n.c.cfg.FunctionAlign\n\t}\n\n\treturn 1\n}\n\n// Decay implements Type.\nfunc (n *FunctionType) Decay()",
	},
	{
		File: "scanner.go",
		Why:  "a file missing from Config.FS must not be opened on the host (go-embedded-c/c#4)",
		Old:  "\t\tf, err := os.Open(src.Name)\n",
		New: "\t\tif src.FS != nil {\n" +
			"\t\t\treturn nil, errorf(\"%s: not in the configured file system\", src.Name)\n" +
			"\t\t}\n\n" +
			"\t\tf, err := os.Open(src.Name)\n",
	},
	{
		File: "cpp.go",
		Why:  "nor probed on the host by __has_include (go-embedded-c/c#4)",
		Old:  "\tif fi == nil {\n\t\tif fi, err = os.Stat(fn); err != nil {\n",
		New:  "\tif fi == nil {\n\t\tif c.cfg.FS != nil {\n\t\t\treturn false\n\t\t}\n\n\t\tif fi, err = os.Stat(fn); err != nil {\n",
	},
}

// budgetGo is added to the package: the error and the check the patches call.
const budgetGo = `package cc

// budget counts the tokens of the replacement of the macro at t and returns
// it, or nothing once Config.MaxExpansion is exceeded: the error is reported
// once, at the use that crossed the budget, and the expansions still pending
// shrink to nothing, so the work stops near the budget. It does not panic:
// the parser's deferred scope handling cannot unwind (go-embedded-c patch).
func (c *cpp) budget(t Token, r cppTokens) cppTokens {
	if c.cfg.MaxExpansion <= 0 {
		return r
	}
	if c.expanded > c.cfg.MaxExpansion {
		return nil
	}
	if c.expanded += len(r); c.expanded > c.cfg.MaxExpansion {
		c.eh("%v: macro expansion exceeds %d tokens", t.Position(), c.cfg.MaxExpansion)
		return nil
	}
	return r
}

// maxEOFShifts bounds how often the parser may shift the end of input: an
// error-recovery loop that does so without progress would never end.
const maxEOFShifts = 1 << 12

type noProgressError struct{ pos string }

func (e *noProgressError) Error() string {
	return e.pos + ": unexpected EOF (the parser made no progress)"
}

func (p *parser) noProgress(t Token) {
	if p.eofShifts++; p.eofShifts > maxEOFShifts {
		panic(&noProgressError{t.Position().String()})
	}
}

// parseBounded is parse, with a stop for no progress turned into an error
// (go-embedded-c patch).
func (p *parser) parseBounded() (ast *AST, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(*noProgressError)
			if !ok {
				panic(r)
			}
			ast, err = nil, e
		}
	}()
	return p.parse()
}

// nonNilFields drops the members whose check failed and returned no field;
// the error is already reported (go-embedded-c patch).
func nonNilFields(fs []*Field) []*Field {
	r := fs[:0]
	for _, f := range fs {
		if f != nil {
			r = append(r, f)
		}
	}
	return r
}
`

const header = "// Code generated by vendorcc from modernc.org/cc/v4 " + Version + "; DO NOT EDIT.\n// The patches are listed in internal/cmd/vendorcc/generate.go.\n\n"

// Generate writes internal/cc into out (replaced): the non-test Go files of
// the cc/v4 module directory src, patched, plus budget.go and the licence.
func Generate(out, src string) error {
	if base := filepath.Base(src); base != "v4@"+Version {
		return fmt.Errorf("vendorcc: %s is not modernc.org/cc/v4@%s", src, Version)
	}
	mod, err := os.ReadFile(filepath.Join(src, "go.mod"))
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(mod, []byte("module modernc.org/cc/v4\n")) {
		return fmt.Errorf("vendorcc: %s/go.mod is not modernc.org/cc/v4", src)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	files := map[string]string{"budget.go": budgetGo}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, n))
		if err != nil {
			return err
		}
		files[n] = string(b)
	}
	for _, p := range patches {
		s, ok := files[p.File]
		if !ok {
			return fmt.Errorf("vendorcc: %s: no such file", p.File)
		}
		if n := strings.Count(s, p.Old); n != 1 {
			return fmt.Errorf("vendorcc: %s: %q occurs %d times, want 1 (%s)", p.File, p.Old, n, p.Why)
		}
		files[p.File] = strings.Replace(s, p.Old, p.New, 1)
	}
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for n, s := range files {
		b, err := format.Source([]byte(header + s))
		if err != nil {
			return fmt.Errorf("vendorcc: %s: %v", n, err)
		}
		if err := os.WriteFile(filepath.Join(out, n), b, 0o644); err != nil {
			return err
		}
	}
	lic, err := os.ReadFile(filepath.Join(src, "LICENSE"))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "LICENSE"), lic, 0o644)
}
