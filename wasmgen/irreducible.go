package wasmgen

import (
	"sort"

	"github.com/go-embedded-c/c/internal/wasm"
)

// Irreducible control flow: a loop with more than one entry, which C makes
// with a goto into a loop (or a goto * whose dispatcher a label is also
// reached from elsewhere). Ramsey's structuring needs every loop to have one
// header that dominates it. As LLVM's WebAssemblyFixIrreducibleControlFlow
// does, each such loop gets a dispatch block: every edge to one of its
// entries, from outside the loop or from inside it, sets a label variable
// to the entry's number and jumps to the dispatch block, which branches to
// the entry the label names. The dispatch block is then the loop's single
// entry. No code is copied, unlike node splitting (Ramsey, appendix A), so a
// hostile function cannot make the module grow by duplication; the number of
// fixes and the blocks they add are bounded (structure).

const maxIrreducibleFixes = 64

// fixIrreducible makes the loop of b, entered at more than one block, a
// loop with a single entry. order is analyze's reverse postorder, which b
// is in. It reports false when it found no loop with several entries, which
// analyze's verdict excludes; structure then refuses the graph.
func (g *fgen) fixIrreducible(order []*block, exit *block, b *block) bool {
	reach := map[*block]bool{}
	for _, x := range order {
		reach[x] = true
	}
	region := reach
	for {
		scc := sccOf(b, region, exit)
		if len(scc) == 0 {
			return false
		}
		if scc[order[0]] {
			// The entry block has no predecessor (labels and loops open
			// blocks of their own), and holds the prologue, which a loop
			// must not run again.
			return false
		}
		var entries []*block
		for x := range scc {
			for _, p := range x.preds {
				if reach[p] && !scc[p] {
					entries = append(entries, x)
					break
				}
			}
		}
		if len(entries) >= 2 {
			sort.Slice(entries, func(i, j int) bool { return entries[i].rpo < entries[j].rpo })
			g.dispatchEntries(order, entries)
			return true
		}
		if len(entries) == 0 || entries[0] == b {
			// One entry h dominates its loop; the edge to b that analyze
			// found is inside the loop without h, a loop of its own.
			return false
		}
		delete(scc, entries[0])
		region = scc
	}
}

// sccOf is the strongly connected component of b in the subgraph of region:
// the blocks b reaches and that reach b, without leaving region.
func sccOf(b *block, region map[*block]bool, exit *block) map[*block]bool {
	fwd := map[*block]bool{b: true}
	work := []*block{b}
	for len(work) != 0 {
		x := work[len(work)-1]
		work = work[:len(work)-1]
		for _, s := range x.succ {
			if s != exit && region[s] && !fwd[s] {
				fwd[s] = true
				work = append(work, s)
			}
		}
	}
	scc := map[*block]bool{b: true}
	work = []*block{b}
	for len(work) != 0 {
		x := work[len(work)-1]
		work = work[:len(work)-1]
		for _, p := range x.preds {
			if region[p] && fwd[p] && !scc[p] {
				scc[p] = true
				work = append(work, p)
			}
		}
	}
	return scc
}

// dispatchEntries adds the dispatch block of the loop entered at entries
// and redirects to it every edge to one of them.
func (g *fgen) dispatchEntries(order []*block, entries []*block) {
	if !g.hasIrrLabel {
		g.irrLabel, g.hasIrrLabel = g.local(wasm.I32), true
	}
	index := map[*block]int64{}
	for i, e := range entries {
		index[e] = int64(i)
	}
	cur := g.cur
	defer func() { g.cur = cur }()

	dispatch := g.newBlock()
	g.cur = dispatch
	for i, e := range entries[:len(entries)-1] {
		g.op(opLocalGet)
		g.uleb(uint64(g.irrLabel))
		g.constant(int64(i), scalar{vt: wasm.I32, bits: 32})
		g.op(opI32Eq)
		next := g.newBlock()
		g.branch(e, next)
		g.cur = next
	}
	g.endJump(entries[len(entries)-1])

	// The edges, each through a block of its own that sets the label: an
	// edge of a conditional block leaves its condition on the stack, which
	// the code at the end of that block must not disturb.
	for _, x := range order {
		for j, s := range x.succ {
			k, ok := index[s]
			if !ok {
				continue
			}
			set := g.newBlock()
			g.cur = set
			g.setLabel(k)
			g.endJump(dispatch)
			x.succ[j] = set
		}
	}
}

func (g *fgen) setLabel(k int64) {
	g.constant(k, scalar{vt: wasm.I32, bits: 32})
	g.op(opLocalSet)
	g.uleb(uint64(g.irrLabel))
}
