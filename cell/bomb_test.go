package cell

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// macroBomb is a cell whose last macro expands to 2^levels tokens, in a few
// hundred bytes: the preprocessor's "billion laughs".
func macroBomb(levels int) []byte {
	var b strings.Builder
	b.WriteString("#define m0 1+\n")
	for i := 1; i <= levels; i++ {
		fmt.Fprintf(&b, "#define m%d m%d m%d\n", i, i-1, i-1)
	}
	fmt.Fprintf(&b, "int x = m%d 1;\n", levels)
	return []byte(b.String())
}

// argumentBomb nests a macro that uses its argument twice: 2^depth tokens
// with one #define and brackets well under MaxDepth.
func argumentBomb(depth int) []byte {
	return []byte("#define D(a) ((a)+(a))\nint y = " + strings.Repeat("D(", depth) + "1" + strings.Repeat(")", depth) + ";\n")
}

// TestExpansionBombsRefused: each bomb is refused with a diagnostic at the
// macro use in the cell, and the refusal allocates a bounded amount
// (go-embedded-c/c#11). The control expands the same shapes small enough to
// pass, so the refusal is the budget's and not a parse error.
func TestExpansionBombsRefused(t *testing.T) {
	for _, tc := range []struct {
		name        string
		bomb, small []byte
		line        int
	}{
		{"chained", macroBomb(40), macroBomb(8), 42},
		{"argument", argumentBomb(40), argumentBomb(8), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if a := Analyze(tc.small, Context{}, Limits{}); len(a.Diagnostics) != 0 {
				t.Fatalf("control refused: %v", a.Diagnostics)
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			a := Analyze(tc.bomb, Context{}, Limits{})
			runtime.ReadMemStats(&after)
			if len(a.Diagnostics) == 0 {
				t.Fatal("a 2^40-token expansion was accepted")
			}
			d := a.Diagnostics[0]
			if d.Line != tc.line || !strings.Contains(d.Msg, "macro expansion exceeds") {
				t.Fatalf("refused with %v, want line %d and the budget", d, tc.line)
			}
			if got := after.TotalAlloc - before.TotalAlloc; got > 512<<20 {
				t.Errorf("the refusal allocated %d bytes", got)
			}
		})
	}
}
