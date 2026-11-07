package wasm

// Section ids (binary/modules.html#sections).
const (
	secCustom   = 0
	secType     = 1
	secFunction = 3
	secCode     = 10
)

// Linking.md: the linking section's version and the symbol table subsection.
const (
	linkingVersion = 2
	subSymbolTable = 8
)

// AppendULEB appends v as an unsigned LEB128.
func AppendULEB(b []byte, v uint64) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

// AppendSLEB appends v as a signed LEB128.
func AppendSLEB(b []byte, v int64) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v == 0 && c&0x40 == 0 || v == -1 && c&0x40 != 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

// AppendPaddedULEB appends v as a 5-byte unsigned LEB128, the form a
// relocated immediate takes (Linking.md: "encoded as a 5-byte varuint32").
func AppendPaddedULEB(b []byte, v uint32) []byte {
	for i := 0; i < 4; i++ {
		b = append(b, byte(v&0x7f)|0x80)
		v >>= 7
	}
	return append(b, byte(v&0x7f))
}

// AppendPaddedSLEB appends v as a 5-byte signed LEB128.
func AppendPaddedSLEB(b []byte, v int32) []byte {
	for i := 0; i < 4; i++ {
		b = append(b, byte(v&0x7f)|0x80)
		v >>= 7
	}
	return append(b, byte(v&0x7f))
}

func appendName(b []byte, s string) []byte {
	b = AppendULEB(b, uint64(len(s)))
	return append(b, s...)
}

func appendSection(b []byte, id byte, content []byte) []byte {
	b = append(b, id)
	b = AppendULEB(b, uint64(len(content)))
	return append(b, content...)
}

func appendCustom(b []byte, name string, content []byte) []byte {
	return appendSection(b, secCustom, append(appendName(nil, name), content...))
}

// Encode returns the object as a module: magic, version, the type, function
// and code sections, then "linking" and, when there are relocations,
// "reloc.CODE".
func (o *Object) Encode() []byte {
	b := []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00}
	section := 0 // index of the next section, as reloc.* counts them

	var types []byte
	types = AppendULEB(types, uint64(len(o.Types)))
	for _, t := range o.Types {
		types = append(types, 0x60)
		types = AppendULEB(types, uint64(len(t.Params)))
		for _, v := range t.Params {
			types = append(types, byte(v))
		}
		types = AppendULEB(types, uint64(len(t.Results)))
		for _, v := range t.Results {
			types = append(types, byte(v))
		}
	}
	b = appendSection(b, secType, types)
	section++

	var funcs []byte
	funcs = AppendULEB(funcs, uint64(len(o.Funcs)))
	for _, f := range o.Funcs {
		funcs = AppendULEB(funcs, uint64(f.Type))
	}
	b = appendSection(b, secFunction, funcs)
	section++

	var code []byte
	var relocs []Reloc // offsets relative to the code section's contents
	code = AppendULEB(code, uint64(len(o.Funcs)))
	for _, f := range o.Funcs {
		var entry []byte
		entry = appendLocals(entry, f.Locals)
		bodyAt := len(entry)
		entry = append(entry, f.Body...)
		entry = append(entry, 0x0b) // end
		code = AppendULEB(code, uint64(len(entry)))
		base := uint32(len(code) + bodyAt)
		code = append(code, entry...)
		for _, r := range f.Relocs {
			r.Offset += base
			relocs = append(relocs, r)
		}
	}
	codeSection := section
	b = appendSection(b, secCode, code)

	var linking []byte
	linking = AppendULEB(linking, linkingVersion)
	var syms []byte
	syms = AppendULEB(syms, uint64(len(o.Symbols)))
	for _, s := range o.Symbols {
		syms = append(syms, byte(s.Kind))
		syms = AppendULEB(syms, uint64(s.Flags))
		syms = AppendULEB(syms, uint64(s.Index))
		syms = appendName(syms, s.Name)
	}
	linking = append(linking, subSymbolTable)
	linking = AppendULEB(linking, uint64(len(syms)))
	linking = append(linking, syms...)
	b = appendCustom(b, "linking", linking)

	if len(relocs) != 0 {
		var rel []byte
		rel = AppendULEB(rel, uint64(codeSection))
		rel = AppendULEB(rel, uint64(len(relocs)))
		for _, r := range relocs {
			rel = append(rel, byte(r.Type))
			rel = AppendULEB(rel, uint64(r.Offset))
			rel = AppendULEB(rel, uint64(r.Symbol))
			if r.Type.hasAddend() {
				rel = AppendSLEB(rel, int64(r.Addend))
			}
		}
		b = appendCustom(b, "reloc.CODE", rel)
	}
	return b
}

// appendLocals appends locals as runs of equal types (binary/modules.html#code-section).
func appendLocals(b []byte, locals []ValType) []byte {
	var runs [][2]uint32
	for _, v := range locals {
		if n := len(runs); n != 0 && runs[n-1][1] == uint32(v) {
			runs[n-1][0]++
			continue
		}
		runs = append(runs, [2]uint32{1, uint32(v)})
	}
	b = AppendULEB(b, uint64(len(runs)))
	for _, r := range runs {
		b = AppendULEB(b, uint64(r[0]))
		b = append(b, byte(r[1]))
	}
	return b
}
