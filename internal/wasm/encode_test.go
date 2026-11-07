package wasm

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/tetratelabs/wazero"
)

func TestLEB(t *testing.T) {
	for _, tc := range []struct {
		got  []byte
		want string
	}{
		{AppendULEB(nil, 0), "00"},
		{AppendULEB(nil, 127), "7f"},
		{AppendULEB(nil, 128), "8001"},
		{AppendULEB(nil, 624485), "e58e26"}, // the example of the LEB128 article the spec cites
		{AppendSLEB(nil, 0), "00"},
		{AppendSLEB(nil, -1), "7f"},
		{AppendSLEB(nil, 63), "3f"},
		{AppendSLEB(nil, 64), "c000"},
		{AppendSLEB(nil, -64), "40"},
		{AppendSLEB(nil, -65), "bf7f"},
		{AppendSLEB(nil, -123456), "c0bb78"},
		{AppendPaddedULEB(nil, 0), "8080808000"},
		{AppendPaddedULEB(nil, 3), "8380808000"},
		{AppendPaddedULEB(nil, 0xffffffff), "ffffffff0f"},
		{AppendPaddedSLEB(nil, -1), "ffffffff7f"},
		{AppendPaddedSLEB(nil, 16), "9080808000"},
	} {
		if g := hex.EncodeToString(tc.got); g != tc.want {
			t.Errorf("got %s, want %s", g, tc.want)
		}
	}
}

// returns builds an object with one function () -> i32 returning v, named
// by the given symbols.
func returns(v int32, names ...string) *Object {
	o := &Object{
		Types: []FuncType{{Results: []ValType{I32}}},
		Funcs: []Func{{Type: 0, Body: AppendSLEB([]byte{0x41}, int64(v))}},
	}
	for _, n := range names {
		o.Symbols = append(o.Symbols, Symbol{Kind: SymFunction, Name: n})
	}
	return o
}

// TestEncodeValidates: wazero's validator accepts the module and runs it
// (custom sections are ignored by the core spec).
func TestEncodeValidates(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	o := returns(42, "f")
	b := o.Encode()
	compiled, err := rt.CompileModule(ctx, b)
	if err != nil {
		t.Fatalf("invalid module %x: %v", b, err)
	}
	_ = compiled.Close(ctx)

	want := "0061736d01000000" + // magic, version
		"010501600001" + "7f" + // type section: () -> i32
		"03020100" + // function section: one function of type 0
		"0a" + "06" + "01" + "04" + "00" + "412a" + "0b" // code: 1 entry, size 4, no locals, i32.const 42, end
	if got := hex.EncodeToString(b); !bytes.HasPrefix([]byte(got), []byte(want)) {
		t.Errorf("module\n got %s\nwant prefix %s", got, want)
	}
}

// sections splits a module into its custom sections, by name, and its other
// sections, by id.
func sections(t *testing.T, b []byte) (custom map[string][]byte, byID map[byte][]byte) {
	t.Helper()
	custom, byID = map[string][]byte{}, map[byte][]byte{}
	p := b[8:]
	for len(p) != 0 {
		id := p[0]
		n, k := uleb(p[1:])
		content := p[1+k : 1+k+int(n)]
		p = p[1+k+int(n):]
		if id == secCustom {
			l, k := uleb(content)
			custom[string(content[k:k+int(l)])] = content[k+int(l):]
			continue
		}
		byID[id] = content
	}
	return custom, byID
}

func uleb(b []byte) (uint64, int) {
	var v uint64
	for i, c := range b {
		v |= uint64(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// TestRelocOffsets: a relocation's offset, relative to the code section's
// contents, points at the 5-byte immediate it relocates.
func TestRelocOffsets(t *testing.T) {
	callee := returns(1, "g")
	caller := Func{Type: 0, Locals: []ValType{I32, I32, I64}}
	caller.Body = append(caller.Body, 0x10) // call
	at := uint32(len(caller.Body))
	caller.Body = AppendPaddedULEB(caller.Body, 0)
	caller.Relocs = []Reloc{{Type: RelocFunctionIndexLEB, Offset: at, Symbol: 1}}
	o := &Object{
		Types:   callee.Types,
		Funcs:   []Func{callee.Funcs[0], caller},
		Symbols: []Symbol{{Kind: SymFunction, Index: 0, Name: "g"}, {Kind: SymFunction, Index: 1, Name: "f"}},
	}
	custom, byID := sections(t, o.Encode())
	rel, ok := custom["reloc.CODE"]
	if !ok {
		t.Fatal("no reloc.CODE section")
	}
	target, k := uleb(rel)
	if target != 2 {
		t.Errorf("reloc.CODE targets section %d, want 2 (type, function, code)", target)
	}
	rel = rel[k:]
	count, k := uleb(rel)
	rel = rel[k:]
	if count != 1 || rel[0] != byte(RelocFunctionIndexLEB) {
		t.Fatalf("relocations % x", rel)
	}
	off, _ := uleb(rel[1:])
	code := byID[secCode]
	if got := code[off-1 : off+5]; !bytes.Equal(got, []byte{0x10, 0x80, 0x80, 0x80, 0x80, 0x00}) {
		t.Errorf("at offset %d: % x, want the call and its padded index", off, got)
	}
	if _, ok := custom["linking"]; !ok {
		t.Error("no linking section")
	}
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	if _, err := rt.CompileModule(ctx, o.Encode()); err != nil {
		t.Errorf("invalid module: %v", err)
	}
}
