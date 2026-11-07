// Package wasmgen compiles a checked C translation unit to a relocatable
// wasm32 object (tool-conventions Linking.md), which wasm-ld links with
// wasi-libc. The design and its order of work are in docs/design-m2.md.
//
// Increment W2: functions with integer parameters and results, integer
// locals, integer globals (initialised, tentative, static, extern), integer
// expressions, direct calls to functions defined here or elsewhere, and
// return. Control flow other than return comes with W3, memory with W4.
// Anything else is refused with an *Unsupported error that names the
// construct, so a corpus run counts refusals by cause.
package wasmgen

import (
	"encoding/binary"
	"fmt"
	"strings"

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

// Limits bound the work of one compilation; the AST may come from an
// untrusted notebook cell.
type Limits struct {
	MaxFunctionBytes int // the largest function body (1 MiB when zero)
	MaxDepth         int // the deepest expression nesting (1000 when zero)
}

func (l Limits) withDefaults() Limits {
	if l.MaxFunctionBytes == 0 {
		l.MaxFunctionBytes = 1 << 20
	}
	if l.MaxDepth == 0 {
		l.MaxDepth = 1000
	}
	return l
}

// module is the object being built.
type module struct {
	o     *wasm.Object
	lim   Limits
	types map[string]uint32 // signature -> type index

	// Function symbols by name. A defined function has its position in
	// o.Funcs; an undefined one its position in o.Imports.
	funcs map[string]*funcSym
	// Data symbols by name (file scope objects).
	data map[string]*dataSym

	calls []callSite
}

type funcSym struct {
	sym     uint32 // symbol index
	defined bool
	pos     int // in o.Funcs or o.Imports
	params  []scalar
	result  *scalar // nil for void
}

// signature returns the parameter and result representations of ft.
func signature(n cc.Node, ft *cc.FunctionType) ([]scalar, *scalar, error) {
	var ps []scalar
	for _, p := range params(ft) {
		s, ok := scalarOf(p.Type())
		if !ok {
			return nil, nil, unsupported(n, "parameter of type "+typeName(p.Type()))
		}
		ps = append(ps, s)
	}
	if r := ft.Result(); !isVoid(r) {
		s, ok := scalarOf(r)
		if !ok {
			return nil, nil, unsupported(n, "result of type "+typeName(r))
		}
		return ps, &s, nil
	}
	return ps, nil, nil
}

// wasmType returns the function type of a signature, sharing type indices.
func (m *module) wasmType(ps []scalar, r *scalar) uint32 {
	var t wasm.FuncType
	for _, p := range ps {
		t.Params = append(t.Params, p.vt)
	}
	if r != nil {
		t.Results = []wasm.ValType{r.vt}
	}
	key := fmt.Sprint(t.Params, t.Results)
	if i, ok := m.types[key]; ok {
		return i
	}
	m.o.Types = append(m.o.Types, t)
	m.types[key] = uint32(len(m.o.Types) - 1)
	return m.types[key]
}

type dataSym struct {
	sym     uint32
	defined bool
	seg     int // segment index when defined
	s       scalar
}

// callSite is a call immediate to patch once the function indices are
// known: the imports come first in the index space, and they are
// discovered while compiling the bodies.
type callSite struct {
	fn     int // in o.Funcs
	offset uint32
	callee *funcSym
}

// Compile compiles the external declarations of ast that come from the file
// named file; the predefined and builtin sources, and headers, only declare.
func Compile(ast *cc.AST, file string) ([]byte, error) {
	return CompileLimits(ast, file, Limits{})
}

// InternalError is a panic inside the generator or in a cc/v4 helper it
// called, recovered: the AST may come from untrusted text, and a defect
// must not take the process down. Unlike *Unsupported, it is a bug.
type InternalError struct {
	Value any
}

func (e *InternalError) Error() string { return fmt.Sprintf("wasmgen: internal error: %v", e.Value) }

// CompileLimits is Compile with explicit limits.
func CompileLimits(ast *cc.AST, file string, lim Limits) (obj []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			obj, err = nil, &InternalError{Value: r}
		}
	}()
	m := &module{o: &wasm.Object{}, lim: lim.withDefaults(), types: map[string]uint32{}, funcs: map[string]*funcSym{}, data: map[string]*dataSym{}}
	var defs []*cc.FunctionDefinition
	for l := ast.TranslationUnit; l != nil; l = l.TranslationUnit {
		ed := l.ExternalDeclaration
		if !fromUnit(ed.Position().Filename, file) {
			continue
		}
		switch ed.Case {
		case cc.ExternalDeclarationFuncDef:
			if err := m.declareFunction(ed.FunctionDefinition); err != nil {
				return nil, err
			}
			defs = append(defs, ed.FunctionDefinition)
		case cc.ExternalDeclarationDecl:
			if err := m.declaration(ed.Declaration); err != nil {
				return nil, err
			}
		default:
			return nil, unsupported(ed, "file-scope "+ed.Case.String())
		}
	}
	for _, fd := range defs {
		if err := m.function(fd); err != nil {
			return nil, err
		}
	}
	m.finish()
	return m.o.Encode(), nil
}

// fromUnit reports whether a declaration at a position in name belongs to
// the unit being compiled: the predefined and builtin sources and the
// headers only declare. The name is not always file: a #line directive
// renames the rest of the source (gcc torture 20010206-1.c).
func fromUnit(name, file string) bool {
	if name == file {
		return true
	}
	return !strings.HasPrefix(name, "<") && !strings.HasSuffix(name, ".h")
}

// params returns the parameters of ft; cc/v4 gives `(void)` one parameter
// of type void.
func params(ft *cc.FunctionType) []*cc.Parameter {
	ps := ft.Parameters()
	if len(ps) == 1 && isVoid(ps[0].Type()) {
		return nil
	}
	return ps
}

func (m *module) symbol(s wasm.Symbol) uint32 {
	m.o.Symbols = append(m.o.Symbols, s)
	return uint32(len(m.o.Symbols) - 1)
}

func linkFlags(d *cc.Declarator) uint32 {
	if d.Linkage() == cc.Internal {
		return wasm.SymBindingLocal
	}
	return wasm.SymHidden // clang's default visibility for a static link
}

// declareFunction gives a definition its place and symbol before any body
// is compiled, so calls may come before the definition.
func (m *module) declareFunction(fd *cc.FunctionDefinition) error {
	d := fd.Declarator
	ft, ok := d.Type().(*cc.FunctionType)
	if !ok {
		// cc/v4 accepts `A{}` as a definition of A with the type int.
		return unsupported(d, "function definition of type "+typeName(d.Type()))
	}
	if ft.IsVariadic() {
		return unsupported(d, "variadic function definition")
	}
	ps, r, err := signature(d, ft)
	if err != nil {
		return err
	}
	name := d.Name()
	if f := m.funcs[name]; f != nil && f.defined {
		return unsupported(d, "function defined twice")
	}
	if name == "main" && len(ps) != 0 {
		return unsupported(d, "main with parameters")
	}
	m.o.Funcs = append(m.o.Funcs, wasm.Func{Type: m.wasmType(ps, r)})
	f := &funcSym{defined: true, pos: len(m.o.Funcs) - 1, params: ps, result: r}
	f.sym = m.symbol(wasm.Symbol{Kind: wasm.SymFunction, Flags: linkFlags(d), Name: name})
	m.funcs[name] = f
	if name == "main" {
		// wasi-libc's crt1.o calls __main_void for a main that takes no
		// arguments; clang defines it on the same function.
		alias := *f
		alias.sym = m.symbol(wasm.Symbol{Kind: wasm.SymFunction, Flags: wasm.SymHidden, Name: "__main_void"})
		m.funcs["__main_void"] = &alias
	}
	return nil
}

// callee returns the symbol for a call of the function declared by d,
// importing it when it is not defined in this unit.
func (m *module) callee(n cc.Node, d *cc.Declarator) (*funcSym, error) {
	if f := m.funcs[d.Name()]; f != nil {
		return f, nil
	}
	ft, ok := d.Type().(*cc.FunctionType)
	if !ok {
		return nil, unsupported(n, "call of a non-function")
	}
	if ft.IsVariadic() {
		return nil, unsupported(n, "call of a variadic function")
	}
	ps, r, err := signature(n, ft)
	if err != nil {
		return nil, err
	}
	return m.importFunc(d.Name(), ps, r), nil
}

// importFunc imports env.name with the given signature.
func (m *module) importFunc(name string, ps []scalar, r *scalar) *funcSym {
	m.o.Imports = append(m.o.Imports, wasm.Import{Name: name, Type: m.wasmType(ps, r)})
	f := &funcSym{pos: len(m.o.Imports) - 1, params: ps, result: r}
	f.sym = m.symbol(wasm.Symbol{Kind: wasm.SymFunction, Flags: wasm.SymUndefined})
	m.funcs[name] = f
	return f
}

// declaration handles a file-scope declaration: typedefs and prototypes
// emit nothing; an object definition, tentative or not, gets a segment.
func (m *module) declaration(decl *cc.Declaration) error {
	if decl.Case != cc.DeclarationDecl {
		return nil // _Static_assert
	}
	for il := decl.InitDeclaratorList; il != nil; il = il.InitDeclaratorList {
		id := il.InitDeclarator
		d := id.Declarator
		switch {
		case d.IsTypename():
			continue
		case d.Type().Kind() == cc.Function:
			continue
		case d.IsExtern() && id.Case != cc.InitDeclaratorInit:
			continue // defined elsewhere; referenced lazily
		}
		if err := m.object(d, id); err != nil {
			return err
		}
	}
	return nil
}

// object defines a file-scope integer object in its own segment, named as
// clang names them (.data.x, .bss.x).
func (m *module) object(d *cc.Declarator, id *cc.InitDeclarator) error {
	s, ok := scalarOf(d.Type())
	if !ok {
		return unsupported(d, "file-scope object of type "+d.Type().String())
	}
	var init []byte
	if id.Case == cc.InitDeclaratorInit {
		if id.Initializer.AssignmentExpression == nil {
			return unsupported(id.Initializer, "braced initializer")
		}
		v, ok := intValue(id.Initializer.AssignmentExpression)
		if !ok {
			return unsupported(id.Initializer, "non-constant initializer")
		}
		init = binary.LittleEndian.AppendUint64(nil, uint64(convertConst(v, s)))[:s.bits/8]
	}
	name := d.Name()
	if prev := m.data[name]; prev != nil {
		if !prev.defined {
			return unsupported(d, "object declared extern then defined")
		}
		if init != nil {
			copy(m.o.Segments[prev.seg].Init, init)
		}
		return nil
	}
	// The alignment includes an aligned attribute or _Alignas on d.
	align := max(s.bits/8, d.Type().Align(), d.Alignas())
	seg := wasm.Segment{Name: ".bss." + name, Align: alignLog2(align), Init: make([]byte, s.bits/8)}
	if init != nil {
		seg.Name, seg.Init = ".data."+name, init
	}
	var end uint32
	if n := len(m.o.Segments); n != 0 {
		last := m.o.Segments[n-1]
		end = last.Offset + uint32(len(last.Init))
	}
	a := uint32(1) << seg.Align
	seg.Offset = (end + a - 1) &^ (a - 1)
	m.o.Segments = append(m.o.Segments, seg)
	ds := &dataSym{defined: true, seg: len(m.o.Segments) - 1, s: s}
	ds.sym = m.symbol(wasm.Symbol{Kind: wasm.SymData, Flags: linkFlags(d), Name: name, Index: uint32(ds.seg), Size: uint32(len(seg.Init))})
	m.data[name] = ds
	return nil
}

func alignLog2(n int) uint32 {
	var l uint32
	for 1<<l < n {
		l++
	}
	return l
}

// global returns the data symbol of the file-scope object declared by d,
// an undefined symbol when it is defined elsewhere.
func (m *module) global(n cc.Node, d *cc.Declarator) (*dataSym, error) {
	if ds := m.data[d.Name()]; ds != nil {
		return ds, nil
	}
	s, ok := scalarOf(d.Type())
	if !ok {
		return nil, unsupported(n, "object of type "+d.Type().String())
	}
	ds := &dataSym{s: s}
	ds.sym = m.symbol(wasm.Symbol{Kind: wasm.SymData, Flags: wasm.SymUndefined, Name: d.Name()})
	m.data[d.Name()] = ds
	return ds, nil
}

// finish fixes the function indices: imports first, then definitions.
func (m *module) finish() {
	imports := uint32(len(m.o.Imports))
	index := func(f *funcSym) uint32 {
		if f.defined {
			return imports + uint32(f.pos)
		}
		return uint32(f.pos)
	}
	for _, f := range m.funcs {
		m.o.Symbols[f.sym].Index = index(f)
	}
	for _, c := range m.calls {
		wasm.PutPaddedULEB(m.o.Funcs[c.fn].Body[c.offset:], index(c.callee))
	}
	m.o.Memory = len(m.data) != 0
}
