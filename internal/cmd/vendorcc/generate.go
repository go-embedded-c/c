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
	{
		File: "check.go",
		Why:  "__attribute__((mode(QI))) was ignored: `unsigned int __attribute__((mode(QI)))` stayed 4 bytes where clang makes it 1, and memclr.c and misalign.c wrote past their packed members; the attribute is recorded (mode.go, go-embedded-c/c#40)",
		Old:  "\t\tcase\n\t\t\t\"__aligned__\",\n\t\t\t\"aligned\":\n",
		New: "\t\tcase\n\t\t\t\"__mode__\",\n\t\t\t\"mode\":\n\n" +
			"\t\t\tc.checkMode(n, attr)\n" +
			"\t\tcase\n\t\t\t\"__aligned__\",\n\t\t\t\"aligned\":\n",
	},
	{
		File: "type.go",
		Why:  "in the attributes, where it travels with the others",
		Old:  "\tisVolatile   bool\n\tweak         bool\n}\n",
		New: "\tisVolatile   bool\n\tweak         bool\n\n" +
			"\tmode   []*AttributeValue // __attribute__((mode(…))), see Declarator.applyMode (go-embedded-c patch)\n" +
			"\tpacked []*AttributeValue // __attribute__((packed)), see packed.go (go-embedded-c patch)\n}\n",
	},
	{
		File: "type.go",
		Why:  "and through their merges",
		Old:  "\tvar ok bool\n\tr.customAttributes, ok = mergeCustomAttributes(n.customAttributes, m.customAttributes)\n",
		New:  "\tr.mode = append(append([]*AttributeValue(nil), n.mode...), m.mode...)\n\tr.packed = append(append([]*AttributeValue(nil), n.packed...), m.packed...)\n\n\tvar ok bool\n\tr.customAttributes, ok = mergeCustomAttributes(n.customAttributes, m.customAttributes)\n",
	},
	{
		File: "check.go",
		Why:  "then it replaces the type of the declarator: in the declaration specifiers,",
		Old:  "\tif n.IsParam() {\n\t\tn.typ = r.Decay()\n\t}\n\treturn n.Type()\n}\n",
		New:  "\tif n.applyMode(c) {\n\t\tr = n.Type()\n\t}\n\tif n.IsParam() {\n\t\tn.typ = r.Decay()\n\t}\n\treturn n.Type()\n}\n",
	},
	{
		File: "check.go",
		Why:  "after the declarator,",
		Old:  "\t\tif n.Declarator.typ, err = mergeAttr(n.Declarator.Type(), attr); err != nil {\n\t\t\tc.errors.add(errorf(\"%v\", err))\n\t\t}\n",
		New:  "\t\tif n.Declarator.typ, err = mergeAttr(n.Declarator.Type(), attr); err != nil {\n\t\t\tc.errors.add(errorf(\"%v\", err))\n\t\t}\n\t\tif n.Declarator.applyMode(c) {\n\t\t\tt = n.Declarator.Type()\n\t\t}\n",
	},
	{
		File: "check.go",
		Why:  "or after the declaration",
		Old:  "\t\t\t\t\td.Type().Attributes().setVisibilityDecl(d)\n\t\t\t\t}\n",
		New:  "\t\t\t\t\td.Type().Attributes().setVisibilityDecl(d)\n\t\t\t\t}\n\t\t\t\td.applyMode(c)\n",
	},
	{
		File: "check.go",
		Why:  "and one written where no declarator takes it is refused, not ignored",
		Old:  "\tc.fixSyntheticDeclarators(n.Scope)\n",
		New:  "\tc.unapplied(c.modes, \"mode\")\n\tc.unapplied(c.packed, \"packed\")\n\tc.fixSyntheticDeclarators(n.Scope)\n",
	},
	{
		File: "check.go",
		Why:  "the mode and packed attributes seen, and whether something took each",
		Old:  "\tcheckingSizeof bool\n\tusesVectors    bool\n}\n",
		New:  "\tcheckingSizeof bool\n\tusesVectors    bool\n\n\tmodes  map[*AttributeValue]bool // go-embedded-c patch, see mode.go\n\tpacked map[*AttributeValue]bool // go-embedded-c patch, see packed.go\n}\n",
	},
	{
		File: "check.go",
		Why:  "__attribute__((packed)) was ignored: a packed struct kept the layout of an unpacked one (clang 23.1.4: sizeof 11, cc/v4: 24 for char, long long, short), and gcc torture misalign.c wrote past its packed member; the attribute is recorded (packed.go, go-embedded-c/c#42)",
		Old:  "\t\tcase \"__gnu_inline__\":\n\t\t\tattr.setGNUInline()\n\t\t}\n",
		New:  "\t\tcase \"__gnu_inline__\":\n\t\t\tattr.setGNUInline()\n\t\tcase \"__packed__\", \"packed\":\n\t\t\tc.checkPacked(n, attr)\n\t\t}\n",
	},
	{
		File: "check.go",
		Why:  "taken by the definition of a struct or union, before the tag or after the closing brace",
		Old:  "\t\tvar defAlign int\n\t\tif attr != nil {\n\t\t\tdefAlign = int(attr.Aligned())\n",
		New:  "\t\tpacked := c.takePacked(attr)\n\t\tvar defAlign int\n\t\tif attr != nil {\n\t\t\tdefAlign = int(attr.Aligned())\n",
	},
	{
		File: "check.go",
		Why:  "which marks the union",
		Old:  "\t\t\tt := c.newUnionType(n.LexicalScope(), tag, nil, -1, 1, attr)\n\t\t\tt.defAlign = defAlign\n",
		New:  "\t\t\tt := c.newUnionType(n.LexicalScope(), tag, nil, -1, 1, attr)\n\t\t\tt.defAlign = defAlign\n\t\t\tt.packed = packed\n",
	},
	{
		File: "check.go",
		Why:  "or the struct",
		Old:  "\t\t\tt := c.newStructType(n.LexicalScope(), tag, nil, -1, 1, attr)\n\t\t\tt.defAlign = defAlign\n",
		New:  "\t\t\tt := c.newStructType(n.LexicalScope(), tag, nil, -1, 1, attr)\n\t\t\tt.defAlign = defAlign\n\t\t\tt.packed = packed\n",
	},
	{
		File: "type.go",
		Why:  "packed",
		Old:  "\tdefAlign      int // From an aligned attribute of the definition, see StructOrUnionSpecifier.check.\n",
		New:  "\tdefAlign      int // From an aligned attribute of the definition, see StructOrUnionSpecifier.check.\n\tpacked        *AttributeValue // go-embedded-c patch, see packed.go\n",
	},
	{
		File: "check.go",
		Why:  "whose members are then packed",
		Old:  "\ta := &fieldAllocator{align: 1, fields: fields, list: n, bigEndian: c.ast.ABI.isBigEndian()}\n",
		New:  "\tc.packFields(s.typ, fields)\n\ta := &fieldAllocator{align: 1, fields: fields, list: n, bigEndian: c.ast.ABI.isBigEndian()}\n",
	},
	{
		File: "check.go",
		Why:  "or taken by the member it follows",
		Old:  "\t\t\tif attr := n.AttributeSpecifierList.check(c); attr != nil && attr.Aligned() > 0 && len(r) != 0 {\n",
		New:  "\t\t\tattr := n.AttributeSpecifierList.check(c)\n\t\t\tif attr != nil && len(attr.packed) != 0 && len(r) != 0 && r[len(r)-1] != nil {\n\t\t\t\tc.packMember(attr.packed, r[len(r)-1])\n\t\t\t}\n\t\t\tif attr != nil && attr.Aligned() > 0 && len(r) != 0 {\n",
	},
	{
		File: "parser.go",
		Why:  "an attribute list linked each value to the first, so the middle ones of three were dropped (`__attribute__((unused, packed, deprecated))` lost packed), and a ',' after a refused first value panicked (FuzzSource)",
		Old:  "\tprev := r\n\tfor p.rune(false) == ',' {\n\t\tavl := &AttributeValueList{Token: p.shift(false), AttributeValue: p.attributeValue()}\n\t\tprev.AttributeValueList = avl\n\t}\n",
		New:  "\tprev := r\n\tfor p.rune(false) == ',' {\n\t\tavl := &AttributeValueList{Token: p.shift(false), AttributeValue: p.attributeValue()}\n\t\tif prev == nil {\n\t\t\tr = avl\n\t\t} else {\n\t\t\tprev.AttributeValueList = avl\n\t\t}\n\t\tprev = avl\n\t}\n",
	},
	{
		File: "type.go",
		Why:  "a packed member",
		Old:  "\tisFlexibleArrayMember bool\n}\n",
		New:  "\tisFlexibleArrayMember bool\n\tpacked                bool // go-embedded-c patch, see packed.go\n}\n",
	},
	{
		File: "type.go",
		Why:  "has alignment 1, or what its aligned attribute asks",
		Old:  "func (n *Field) alignment() int { return max(n.Type().Align(), n.align) }\n",
		New:  "func (n *Field) alignment() int {\n\tif n.packed {\n\t\treturn max(1, n.align)\n\t}\n\n\treturn max(n.Type().Align(), n.align)\n}\n",
	},
	{
		File: "check.go",
		Why:  "bit-fields were laid out in groups, not by the Itanium rule gcc and clang follow: `struct { unsigned long long f:44; _Bool g:1; short h; }` had h at 8 and size 16, clang 6 and 8 (bitfield.go, go-embedded-c/c#29)",
		Old:  "\t\tn.checkStruct(c, a, fields)\n",
		New:  "\t\tif c.itaniumLayout() && hasBitfield(fields) {\n\t\t\tc.layoutStruct(a, fields)\n\t\t} else {\n\t\t\tn.checkStruct(c, a, fields)\n\t\t}\n",
	},
	{
		File: "check.go",
		Why:  "a bit-field as wide as int promotes to int or unsigned int by its declared type's signedness, whatever that type (clang ASTContext::isPromotableBitField, matching gcc): `long long x:32` is an int, not a long long (go-embedded-c/c#29)",
		Old:  "\t\t\tif f.ValueBits() < c.intT.Size()*8 {\n\t\t\t\tx = c.intT.(*PredefinedType)\n\t\t\t}\n",
		New: "\t\t\tswitch w := c.intT.Size() * 8; {\n" +
			"\t\t\tcase f.ValueBits() < w:\n\t\t\t\tx = c.intT.(*PredefinedType)\n" +
			"\t\t\tcase f.ValueBits() == w && IsSignedInteger(x):\n\t\t\t\tx = c.intT.(*PredefinedType)\n" +
			"\t\t\tcase f.ValueBits() == w:\n\t\t\t\tx = c.ast.kinds[UInt].(*PredefinedType)\n" +
			"\t\t\t}\n",
	},
	{
		File: "type.go",
		Why:  "the usual arithmetic conversions lost the complex domain when the other operand's real type ranked higher: long double + double _Complex was long double, double + float _Complex double, dropping the imaginary part; C11 6.3.1.8 converts \"without change of type domain\", and the result is complex when either operand is; and they dropped the vector of a vector and a scalar: v + 2, of a vector of short, was an int, while the scalar converts to the element type and the result is the vector (GCC manual, Vector Extensions; vector.go)",
		Old:  "func UsualArithmeticConversions(a, b Type) (r Type) {\n",
		New: "func UsualArithmeticConversions(a, b Type) (r Type) {\n" +
			"\tif v := vectorOperand(a, b); v != nil {\n\t\treturn v\n\t}\n\n" +
			"\tr = usualArithmeticConversions(a, b)\n" +
			"\tif complexKinds[r.Kind()] || !complexKinds[a.Kind()] && !complexKinds[b.Kind()] {\n" +
			"\t\treturn r\n\t}\n\n" +
			"\tp, ok := r.(*PredefinedType)\n" +
			"\tif !ok {\n\t\treturn r\n\t}\n\n" +
			"\tfor ck, rk := range correspondingRealKinds {\n" +
			"\t\tif complexKinds[ck] && rk == r.Kind() {\n" +
			"\t\t\tif t := p.c.ast.kinds[Kind(ck)]; t != nil {\n\t\t\t\treturn t\n\t\t\t}\n\t\t}\n\t}\n" +
			"\treturn r\n}\n\n" +
			"func usualArithmeticConversions(a, b Type) (r Type) {\n",
	},
	{
		File: "cc.go",
		Why:  "the complex builtins were not declared, so __builtin_conj(z) was an implicit int function and its argument went unconverted",
		Old:  "long double __builtin_fabsl(long double x);\n",
		New: "long double __builtin_fabsl(long double x);\n" +
			"double _Complex __builtin_conj(double _Complex z);\n" +
			"float _Complex __builtin_conjf(float _Complex z);\n" +
			"long double _Complex __builtin_conjl(long double _Complex z);\n" +
			"double __builtin_creal(double _Complex z);\n" +
			"float __builtin_crealf(float _Complex z);\n" +
			"long double __builtin_creall(long double _Complex z);\n" +
			"double __builtin_cimag(double _Complex z);\n" +
			"float __builtin_cimagf(float _Complex z);\n" +
			"long double __builtin_cimagl(long double _Complex z);\n",
	},
	{
		File: "check.go",
		Why:  "an imaginary constant with the L suffix (2.0iL) was typed _Complex long, an integer complex type, while its value is a long double one",
		Old:  "return &ComplexLongDoubleValue{Im: (*LongDoubleValue)(bf)}, c.ast.kinds[ComplexLong]",
		New:  "return &ComplexLongDoubleValue{Im: (*LongDoubleValue)(bf)}, c.ast.kinds[ComplexLongDouble]",
	},
	{
		File: "check.go",
		Why:  "and in unions, where an unnamed bit-field aligned the union",
		Old:  "\t\tn.checkUnion(c, a, fields)\n",
		New:  "\t\tif c.itaniumLayout() && hasBitfield(fields) {\n\t\t\tc.layoutUnion(a, fields)\n\t\t} else {\n\t\t\tn.checkUnion(c, a, fields)\n\t\t}\n",
	},
	{
		File: "type.go",
		Why:  "a GNU vector is aligned on its size, as gcc and clang align it (clang 23.1.4 --target=wasm32-wasip1: _Alignof of a 16-byte vector of int is 16, of an 8-byte one 8), not on its element's (vector.go)",
		Old:  "\tif n.attributer.p != nil {\n\t\tif v := n.attributer.p.Aligned(); v > 0 {\n\t\t\treturn int(v)\n\t\t}\n\t}\n\n\tif x, ok := n.c.ast.ABI.Types[n.kind]; ok {\n\t\treturn x.Align\n\t}\n",
		New:  "\tif n.attributer.p != nil {\n\t\tif v := n.attributer.p.Aligned(); v > 0 {\n\t\t\treturn int(v)\n\t\t}\n\t}\n\n\tif v := n.VectorSize(); v > 0 {\n\t\treturn int(v)\n\t}\n\n\tif x, ok := n.c.ast.ABI.Types[n.kind]; ok {\n\t\treturn x.Align\n\t}\n",
	},
	{
		File: "type.go",
		Why:  "and so is it as a member",
		Old:  "\tif x, ok := n.c.ast.ABI.Types[n.kind]; ok {\n\t\treturn x.FieldAlign\n\t}\n",
		New:  "\tif v := n.VectorSize(); v > 0 {\n\t\treturn int(v)\n\t}\n\n\tif x, ok := n.c.ast.ABI.Types[n.kind]; ok {\n\t\treturn x.FieldAlign\n\t}\n",
	},
	{
		File: "type.go",
		Why:  "the array a vector is initialised as counted bytes, not elements: a 16-byte vector of int took 16 initialisers",
		Old:  "\t\tvec = n.c.newArrayType(n, sz, nil)\n",
		New:  "\t\tvec = n.c.newArrayType(n, vectorElems(n, sz), nil)\n",
	},
	{
		File: "type.go",
		Why:  "and the integer promotions turned a vector of short into an int: integer vector operations do not promote (GCC manual, Vector Extensions)",
		Old:  "func IntegerPromotion(t Type) Type {\n\tswitch t.Kind() {\n",
		New:  "func IntegerPromotion(t Type) Type {\n\tif isVectorType(t) {\n\t\treturn t\n\t}\n\n\tswitch t.Kind() {\n",
	},
	{
		File: "check.go",
		Why:  "a shift of a scalar by a vector, 1 << v, was typed as the scalar: it is the vector (GCC manual, Vector Extensions)",
		Old:  "\t\t\tn.typ = IntegerPromotion(a)\n\t\t\tif b := a.BitField(); b != nil {\n",
		New:  "\t\t\tn.typ = IntegerPromotion(a)\n\t\t\tif v := vectorOperand(a, b); v != nil {\n\t\t\t\tn.typ = v\n\t\t\t}\n\t\t\tif b := a.BitField(); b != nil {\n",
	},
	{
		File: "check.go",
		Why:  "an element of a vector, v[i], was typed as the whole vector",
		Old:  "\t\tcase isVectorType(t1) && IsIntegerType(t2):\n\t\t\tn.typ = t1\n\t\tcase isVectorType(t2) && IsIntegerType(t1):\n\t\t\tn.typ = t2\n",
		New:  "\t\tcase isVectorType(t1) && IsIntegerType(t2):\n\t\t\tn.typ = vectorElem(t1)\n\t\tcase isVectorType(t2) && IsIntegerType(t1):\n\t\t\tn.typ = vectorElem(t2)\n",
	},
	{
		File: "check.go",
		Why:  "an equality of vectors is a vector of signed integers of the same width, each -1 or 0, not an int (GCC manual, Vector Extensions)",
		Old:  "\t\t\tn.typ = c.intT\n\t\tdefault:\n\t\t\tc.errors.add(errorf(\"%v: invalid operands: %s (%v) and %s (%v)\"",
		New:  "\t\t\tn.typ = vectorCompare(a, b, c.intT)\n\t\tdefault:\n\t\t\tc.errors.add(errorf(\"%v: invalid operands: %s (%v) and %s (%v)\"",
	},
	{
		File: "check.go",
		Why:  "and so is a relation of vectors",
		Old:  "\t\t\t// ok\n\t\tdefault:\n\t\t\tc.errors.add(errorf(\"%v: invalid operands: %s and %s\", n.Token.Position(), a, b))\n\t\t}\n\t\tn.setPure(n.RelationalExpression.Pure() && n.ShiftExpression.Pure())\n",
		New:  "\t\t\tn.typ = vectorCompare(a, b, c.intT)\n\t\tdefault:\n\t\t\tc.errors.add(errorf(\"%v: invalid operands: %s and %s\", n.Token.Position(), a, b))\n\t\t}\n\t\tn.setPure(n.RelationalExpression.Pure() && n.ShiftExpression.Pure())\n",
	},
}

// vectorGo is added to the package: the typing of GNU vectors.
const vectorGo = `package cc

// GNU vectors, __attribute__((vector_size(N))) (GCC manual, Vector
// Extensions). cc/v4 keeps a vector as its element's predefined type with
// the attribute, so its Kind is the element's; these functions give the
// operators the types gcc and clang give them (go-embedded-c patch).

// vectorElems is how many elements of type elem a vector of size bytes has.
// elem may carry the vector attribute already, which makes its Size the
// vector's: the element's size is its kind's.
func vectorElems(elem Type, size int64) int64 {
	s := elem.Size()
	if p, ok := elem.(*PredefinedType); ok {
		if x, ok := p.c.ast.ABI.Types[p.kind]; ok {
			s = x.Size
		}
	}
	if s > 0 {
		return size / s
	}

	return size
}

// vectorElem is the element type of the vector t.
func vectorElem(t Type) Type {
	if p, ok := t.(*PredefinedType); ok {
		if e := p.c.ast.kinds[p.kind]; e != nil {
			return e
		}
	}

	return t
}

// VectorElem returns the element type of the GNU vector type t and how many
// elements it has; ok is false when t is not a vector.
func VectorElem(t Type) (elem Type, n int64, ok bool) {
	if t == nil || !isVectorType(t) {
		return nil, 0, false
	}

	elem = vectorElem(t)
	return elem, vectorElems(elem, t.Size()), true
}

// vectorOperand is the type of a binary operation of a and b when one is a
// vector: that vector, the scalar converted to its element type. Nil when
// neither is.
func vectorOperand(a, b Type) Type {
	switch {
	case isVectorType(a):
		return a
	case isVectorType(b):
		return b
	}

	return nil
}

// vectorCompare is the type of a comparison of a and b: when one is a
// vector, a vector of signed integers as wide as its elements and as many
// (clang: char, short, long or long long elements, -1 or 0), else def.
func vectorCompare(a, b, def Type) Type {
	v := vectorOperand(a, b)
	if v == nil {
		return def
	}

	p, ok := v.(*PredefinedType)
	if !ok {
		return def
	}

	var k Kind
	switch vectorElem(v).Size() {
	case 1:
		k = SChar
	case 2:
		k = Short
	case 4:
		k = Int
	case 8:
		k = LongLong
	default:
		return def
	}
	e, ok := p.c.ast.kinds[k].(*PredefinedType)
	if !ok {
		return def
	}

	attr := newAttributes()
	attr.setVectorSize(v.Size())
	return e.setAttr(attr)
}
`

// packedGo is added to the package: the packed attribute (go-embedded-c/c#42).
const packedGo = `package cc

// __attribute__((packed)) on the definition of a struct or union is packed on
// each member; a packed member has alignment 1 unless an aligned attribute
// asks more (GCC manual, Common Attributes, packed). cc/v4 ignored it
// (go-embedded-c patch). A packed bit-field is laid out by bitfield.go; a
// packed enum is refused until implemented.

// checkPacked records the packed attribute n in attr.
func (c *ctx) checkPacked(n *AttributeValue, attr *Attributes) {
	if c.packed == nil {
		c.packed = map[*AttributeValue]bool{}
	}
	if _, ok := c.packed[n]; !ok {
		c.packed[n] = false
	}
	attr.packed = append(attr.packed, n)
	attr.isNonZero = true
}

// takePacked marks the packed attributes of a definition taken and returns
// the first, nil if none.
func (c *ctx) takePacked(a *Attributes) *AttributeValue {
	if a == nil || len(a.packed) == 0 {
		return nil
	}

	for _, n := range a.packed {
		c.packed[n] = true
	}
	return a.packed[0]
}

// packFields packs the members of t when its definition is packed.
func (c *ctx) packFields(t Type, fields []*Field) {
	var p *AttributeValue
	switch x := t.(type) {
	case *StructType:
		p = x.packed
	case *UnionType:
		p = x.packed
	}
	if p == nil {
		return
	}

	for _, f := range fields {
		c.packField(p, f)
	}
}

// packMember packs f, the member the attributes l follow.
func (c *ctx) packMember(l []*AttributeValue, f *Field) {
	for _, n := range l {
		c.packed[n] = true
	}
	c.packField(l[0], f)
}

// packField packs f. A packed bit-field is laid out by layoutStruct, on the
// targets that follow the Itanium rule; elsewhere it is refused.
func (c *ctx) packField(n *AttributeValue, f *Field) {
	if f.IsBitfield() && !c.itaniumLayout() {
		c.errors.add(errorf("%v: attribute packed on a bit-field is not supported for this target", n.Position()))
		return
	}

	f.packed = true
}
`

// bitfieldGo is added to the package: bit-fields laid out by the Itanium rule
// (go-embedded-c/c#29).
const bitfieldGo = `package cc

// cc/v4 laid bit-fields out in "groups" sized by the declared type
// (fieldAllocator), which is not the rule gcc and clang follow for every
// target but Microsoft's, the Itanium C++ ABI's for C (clang
// RecordLayoutBuilder, ItaniumRecordLayoutBuilder::LayoutBitField):
//
//   - a bit-field goes at the next free bit, unless it would straddle a
//     boundary of its declared type's alignment, and then at that boundary;
//     a packed bit-field goes at the next free bit (GCC manual, packed: "one
//     bit for a bit-field");
//   - a zero-width bit-field moves the next member to its type's boundary,
//     packed or not;
//   - an unnamed bit-field does not align the struct;
//   - the next ordinary member goes at the first byte after the bits used,
//     at its alignment.
//
// struct { unsigned long long f:44; _Bool g:1; short h; } has h at 6 and size
// 8 under clang, at 8 and 16 under the group allocator (go-embedded-c patch).

// itaniumLayout reports whether the target lays bit-fields out by the
// Itanium rule: little-endian, not Microsoft's. The other targets keep
// cc/v4's allocator.
func (c *ctx) itaniumLayout() bool {
	return !c.ast.ABI.isBigEndian() && c.ast.ABI.goos != "windows"
}

func hasBitfield(fields []*Field) bool {
	for _, f := range fields {
		if f.IsBitfield() {
			return true
		}
	}
	return false
}

// bitfieldAlign is the alignment that bounds a bit-field and that a named one
// gives the struct: its declared type's, 1 when packed.
func bitfieldAlign(f *Field) int64 {
	if f.packed {
		return 1
	}

	return int64(max(f.Type().Align(), 1))
}

// placeBitfield records that f's value is at bit of the struct. Its storage
// unit, which a read loads and a write stores back whole, is the aligned unit
// of its declared type, inside which the rule keeps it; for a packed
// bit-field, which may straddle any boundary, the bytes it spans rounded up
// to 1, 2, 4 or 8 (more is not a unit, and is refused where it is accessed).
func placeBitfield(f *Field, bit int64) {
	w := f.ValueBits()
	var start, unit int64
	switch {
	case f.packed:
		start = bit / 8
		unit = 1
		for unit*8 < bit-start*8+w {
			unit *= 2
		}
	default:
		unit = bitfieldAlign(f)
		start = bit / (unit * 8) * unit
	}
	f.offsetBytes = start
	f.outerGroupOffsetBytes = start
	f.offsetBits = int(bit - start*8)
	f.accessBytes = unit
	f.groupSize = int(unit)
	f.inOverlapGroup = false
	f.mask = 0
	if w > 0 && w <= 64 {
		f.mask = (^uint64(0) >> (64 - w)) << f.offsetBits
	}
}

// layoutStruct lays out the members of a struct with bit-fields by the
// Itanium rule, and leaves in a the end of the last member, in bytes, and the
// struct's alignment.
func (c *ctx) layoutStruct(a *fieldAllocator, fields []*Field) {
	var bit int64 // the next free bit
	align := int64(1)
	for i, f := range fields {
		t := f.Type()
		if !f.IsBitfield() {
			if (t.IsIncomplete() || t.Size() == 0) && t.Kind() == Array && i == len(fields)-1 {
				f.isFlexibleArrayMember = true
				if x, ok := t.(*ArrayType); ok {
					f.isFlexibleArrayMember = !x.IsVLA()
				}
			}
			sz := t.Size()
			if f.isFlexibleArrayMember || isEmpty(t) || sz < 0 {
				sz = 0
			}
			fa := int64(f.alignment())
			off := roundup((bit+7)/8, fa)
			f.offsetBytes = off
			f.outerGroupOffsetBytes = off
			f.accessBytes = sz
			f.groupSize = int(sz)
			bit = (off + sz) * 8
			align = max(align, fa)
			continue
		}

		w := f.ValueBits()
		if w == 0 {
			// Packed or not.
			bit = roundup(bit, int64(max(t.Align(), 1))*8)
			placeBitfield(f, bit)
			continue
		}

		ba := bitfieldAlign(f)
		if unit := ba * 8; !f.packed && bit/unit != (bit+w-1)/unit {
			bit = roundup(bit, unit)
		}
		placeBitfield(f, bit)
		bit += w
		if f.Name() != "" {
			align = max(align, ba)
		}
	}
	a.brkBytes = (bit + 7) / 8
	a.align = int(align)
}

// layoutUnion lays out the members of a union with bit-fields: each at
// offset 0; a bit-field takes the bytes of its width, and only a named one
// aligns the union.
func (c *ctx) layoutUnion(a *fieldAllocator, fields []*Field) {
	var size int64
	align := int64(1)
	for _, f := range fields {
		t := f.Type()
		if !f.IsBitfield() {
			sz := t.Size()
			if isEmpty(t) || sz < 0 {
				sz = 0
			}
			f.offsetBytes, f.outerGroupOffsetBytes, f.accessBytes, f.groupSize = 0, 0, sz, int(sz)
			size = max(size, sz)
			align = max(align, int64(f.alignment()))
			continue
		}

		placeBitfield(f, 0)
		w := f.ValueBits()
		size = max(size, (w+7)/8)
		if f.Name() != "" && w != 0 {
			align = max(align, bitfieldAlign(f))
		}
	}
	a.brkBytes = size
	a.align = int(align)
}
`

// modeGo is added to the package: the mode attribute (go-embedded-c/c#40).
const modeGo = `package cc

import (
	"fmt"
	"sort"
	"strings"
)

// gcc and clang treat __attribute__((mode(M))) as an attribute of the
// declaration, not of a type: it replaces the declared type by the integer or
// floating type of M's width, and an integer keeps the signedness of the
// declared type, an enumeration that of its underlying type (clang
// Sema::AddModeAttr). cc/v4 ignored it (go-embedded-c patch).

// checkMode records the mode attribute n in attr.
func (c *ctx) checkMode(n *AttributeValue, attr *Attributes) {
	l := n.ArgumentExpressionList
	if l == nil || l.ArgumentExpressionList != nil || isName(l.AssignmentExpression) == "" {
		c.errors.add(errorf("%v: attribute mode expects one identifier", n.Position()))
		return
	}

	if c.modes == nil {
		c.modes = map[*AttributeValue]bool{}
	}
	if _, ok := c.modes[n]; !ok {
		c.modes[n] = false
	}
	attr.mode = append(attr.mode, n)
	attr.isNonZero = true
}

// modeName is the machine mode n names, without the __ __ of its other
// spelling.
func modeName(n *AttributeValue) string {
	s := isName(n.ArgumentExpressionList.AssignmentExpression)
	if len(s) >= 4 && strings.HasPrefix(s, "__") && strings.HasSuffix(s, "__") {
		s = s[2 : len(s)-2]
	}
	return s
}

// applyMode gives d the type its mode attribute names and reports whether it
// had one. The new type does not carry the attribute: a typedef passes on its
// width, not the attribute, so that in
//
//	typedef unsigned int __attribute__((mode(QI))) u8;
//	u8 *p;
//
// p is a pointer to an 8-bit integer, not a pointer given a mode.
func (d *Declarator) applyMode(c *ctx) bool {
	t := d.Type()
	if t == nil {
		return false
	}

	a := t.Attributes()
	if a == nil || len(a.mode) == 0 {
		return false
	}

	for _, m := range a.mode {
		c.modes[m] = true
	}
	name := modeName(a.mode[0])
	for _, m := range a.mode[1:] {
		if s := modeName(m); s != name {
			c.errors.add(errorf("%v: conflicting mode attributes: %s and %s", m.Position(), name, s))
			return false
		}
	}

	k, err := c.modeKind(name, t)
	if err != nil {
		c.errors.add(errorf("%v: %v", a.mode[0].Position(), err))
		return false
	}

	var r Type = c.newPredefinedType(k)
	if b := a.withoutMode(); b != nil {
		r = r.setAttr(b)
	}
	if d.isTypename {
		r = r.setName(d)
	}
	d.typ = r
	return true
}

// modeKind is the kind mode name gives a declaration of type t. On a
// vector, the mode is its element's, and the vector keeps its size (clang
// Sema::AddModeAttr, "Base type can also be a vector type": the element
// count becomes the size over the new element's).
func (c *ctx) modeKind(name string, t Type) (k Kind, err error) {
	if v := t.VectorSize(); v > 0 {
		defer func() {
			if err == nil && (c.ast.ABI.Types[k].Size <= 0 || v%c.ast.ABI.Types[k].Size != 0) {
				k, err = 0, fmt.Errorf("attribute mode %s on a vector of %d bytes", name, v)
			}
		}()
	}

	abi := c.ast.ABI
	var bits int64
	float := false
	switch name {
	case "QI":
		bits = 8
	case "HI":
		bits = 16
	case "SI":
		bits = 32
	case "DI":
		bits = 64
	case "TI":
		bits = 128
	case "SF":
		bits, float = 32, true
	case "DF":
		bits, float = 64, true
	case "byte":
		bits = 8 * abi.Types[Char].Size
	case "word", "pointer", "unwind_word":
		// gcc's word_mode is the width of a register, which the ABI does not
		// give; for the targets go-embedded-c compiles to it is the width of a
		// pointer (clang 23.1.4, wasm32: 4 bytes for all three, measured).
		bits = 8 * abi.Types[Ptr].Size
	default:
		return 0, fmt.Errorf("attribute mode %s is not supported", name)
	}

	var kinds []Kind
	switch k := t.Kind(); {
	case float && (k == Float || k == Double || k == LongDouble):
		kinds = []Kind{Float, Double, LongDouble}
	case float:
		return 0, fmt.Errorf("attribute mode %s on a type that is not floating", name)
	case !IsIntegerType(t):
		return 0, fmt.Errorf("attribute mode %s on a type that is not an integer", name)
	case IsSignedInteger(t):
		kinds = []Kind{SChar, Short, Int, Long, LongLong}
	default:
		kinds = []Kind{UChar, UShort, UInt, ULong, ULongLong}
	}
	for _, k := range kinds {
		if abi.Types[k].Size*8 == bits {
			return k, nil
		}
	}

	return 0, fmt.Errorf("attribute mode %s is not supported: no type of %d bits", name, bits)
}

// withoutMode returns a without its mode attribute, or nil when nothing else
// is left.
func (a *Attributes) withoutMode() *Attributes {
	m := *a
	m.mode = nil
	m.customAttributes = withoutCustom(a.customAttributes, "mode", "__mode__")
	if m.nothingLeft() {
		return nil
	}

	return &m
}

// withoutCustom returns m without the keys names, nil if none is left.
func withoutCustom(m map[string][]Value, names ...string) (r map[string][]Value) {
	for k, v := range m {
		drop := false
		for _, s := range names {
			drop = drop || k == s
		}
		if drop {
			continue
		}

		if r == nil {
			r = map[string][]Value{}
		}
		r[k] = v
	}
	return r
}

// nothingLeft reports whether a holds no attribute.
func (a *Attributes) nothingLeft() bool {
	return a.alias == "" && a.aliasDecl == nil && a.aligned <= 0 && a.vectorSize <= 0 && a.visibility == "" && a.visibilityDecl == nil &&
		len(a.customAttributes) == 0 && !a.alwaysInline && !a.gnuInline && !a.isConst && !a.isVolatile && !a.weak &&
		len(a.mode) == 0 && len(a.packed) == 0
}

// unapplied refuses the attributes of m that nothing took, as a mode in a
// type name or a packed in front of a member: ignored, they would leave a
// type its declared width or a member its alignment.
func (c *ctx) unapplied(m map[*AttributeValue]bool, name string) {
	var l []*AttributeValue
	for n, applied := range m {
		if !applied {
			l = append(l, n)
		}
	}
	sort.Slice(l, func(i, j int) bool { return l[i].Token.seq < l[j].Token.seq })
	for _, n := range l {
		c.errors.add(errorf("%v: attribute %s is not supported here", n.Position(), name))
	}
}
`

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
// the cc/v4 module directory src, patched, plus budget.go, mode.go, packed.go,
// bitfield.go and the licence.
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
	files := map[string]string{"budget.go": budgetGo, "mode.go": modeGo, "packed.go": packedGo, "bitfield.go": bitfieldGo, "vector.go": vectorGo}
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
