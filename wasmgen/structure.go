package wasmgen

import (
	"sort"

	"github.com/go-embedded-c/c/internal/cc"
	"github.com/go-embedded-c/c/internal/wasm"
)

// Structuring follows N. Ramsey, "Beyond Relooper: Recursive Translation
// of Unstructured Control Flow to Structured Control Flow", PACMPL 6 ICFP
// (2022), section 5: doTree, nodeWithin and doBranch recurse over the
// dominator tree, with children ordered by reverse postorder (RPO). A node
// with two or more forward in-edges is a merge node and is placed after a
// block that its predecessors exit; a node with a back in-edge is a loop
// header and is wrapped in a loop. It needs a reducible graph: C without
// goto always gives one (W7 adds goto and node splitting).

// Opcodes of control (wasm 3.0, 5.3-binary.instructions.spectec).
const (
	opBlock = 0x02
	opLoop  = 0x03
	opIf    = 0x04
	opElse  = 0x05
	opEnd   = 0x0b
	opBr    = 0x0c
	voidBT  = 0x40 // the empty block type
)

type frameKind int

const (
	frameIf frameKind = iota
	frameLoop
	frameBlock
)

// frame is a syntactic form enclosing the code being emitted; a loop is
// labelled by its header, a block by the node that follows it.
type frame struct {
	kind  frameKind
	label *block
}

// out is the structured function body being assembled.
type out struct {
	code   []byte
	relocs []wasm.Reloc
	calls  []callSite
}

type structurer struct {
	g     *fgen
	n     cc.Node
	out   out
	merge map[*block]bool
	loop  map[*block]bool
	err   error
}

// structure numbers the reachable blocks, builds the dominator tree and
// emits the structured body.
func (g *fgen) structure(n cc.Node) (out, []callSite, error) {
	entry := g.blocks[0]
	for _, b := range g.blocks {
		b.rpo = -1
	}
	// Postorder by an explicit DFS: a hostile function cannot exhaust the
	// goroutine stack through recursion here.
	var post []*block
	visited := map[*block]bool{entry: true}
	type item struct {
		b    *block
		next int
	}
	stack := []item{{b: entry}}
	for len(stack) != 0 {
		top := &stack[len(stack)-1]
		if top.next < len(top.b.succ) {
			s := top.b.succ[top.next]
			top.next++
			if !visited[s] {
				visited[s] = true
				stack = append(stack, item{b: s})
			}
			continue
		}
		post = append(post, top.b)
		stack = stack[:len(stack)-1]
	}
	order := make([]*block, len(post))
	for i, b := range post {
		b.rpo = len(post) - 1 - i
		order[b.rpo] = b
	}
	for _, b := range order {
		for _, s := range b.succ {
			s.preds = append(s.preds, b)
		}
	}

	// Dominators: K. Cooper, T. Harvey, K. Kennedy, "A Simple, Fast
	// Dominance Algorithm" (2001), iterating over RPO.
	entry.idom = entry
	intersect := func(a, b *block) *block {
		for a != b {
			for a.rpo > b.rpo {
				a = a.idom
			}
			for b.rpo > a.rpo {
				b = b.idom
			}
		}
		return a
	}
	for changed := true; changed; {
		changed = false
		for _, b := range order[1:] {
			var nd *block
			for _, p := range b.preds {
				if p.idom == nil {
					continue
				}
				if nd == nil {
					nd = p
				} else {
					nd = intersect(p, nd)
				}
			}
			if nd != b.idom {
				b.idom, changed = nd, true
			}
		}
	}
	dominates := func(a, b *block) bool {
		for {
			if a == b {
				return true
			}
			if b == entry {
				return false
			}
			b = b.idom
		}
	}

	s := &structurer{g: g, n: n, merge: map[*block]bool{}, loop: map[*block]bool{}}
	for _, b := range order {
		if b != entry {
			b.idom.kids = append(b.idom.kids, b)
		}
		forward := 0
		for _, p := range b.preds {
			if p.rpo < b.rpo {
				forward++
				continue
			}
			// A back edge p -> b: reducible iff b dominates p.
			if !dominates(b, p) {
				return out{}, nil, unsupported(n, "irreducible control flow (W7)")
			}
			s.loop[b] = true
		}
		s.merge[b] = forward >= 2
	}
	s.doTree(entry, nil)
	if s.err != nil {
		return out{}, nil, s.err
	}
	if !g.void {
		// Every path returned; the end of the body is unreachable, and
		// unreachable makes the missing result valid.
		s.emit(opUnreachable)
	}
	return s.out, s.out.calls, nil
}

func (s *structurer) emit(b ...byte) { s.out.code = append(s.out.code, b...) }

func push(ctx []frame, f frame) []frame {
	r := make([]frame, len(ctx)+1)
	copy(r, ctx)
	r[len(ctx)] = f
	return r
}

// doTree emits x and the nodes it dominates (Ramsey, lines 1-7).
func (s *structurer) doTree(x *block, ctx []frame) {
	var ys []*block
	for _, k := range x.kids {
		if s.merge[k] {
			ys = append(ys, k)
		}
	}
	sort.Slice(ys, func(i, j int) bool { return ys[i].rpo > ys[j].rpo })
	if s.loop[x] {
		s.emit(opLoop, voidBT)
		s.nodeWithin(x, ys, push(ctx, frame{frameLoop, x}))
		s.emit(opEnd)
		return
	}
	s.nodeWithin(x, ys, ctx)
}

// nodeWithin places x inside one block per merge child, the child with the
// highest RPO outermost (lines 8-21).
func (s *structurer) nodeWithin(x *block, ys []*block, ctx []frame) {
	if len(ys) != 0 {
		y := ys[0]
		s.emit(opBlock, voidBT)
		s.nodeWithin(x, ys[1:], push(ctx, frame{frameBlock, y}))
		s.emit(opEnd)
		s.doTree(y, ctx)
		return
	}
	s.actions(x)
	switch x.term {
	case termJump:
		s.doBranch(x, x.succ[0], ctx)
	case termCond:
		s.emit(opIf, voidBT)
		inner := push(ctx, frame{kind: frameIf})
		s.doBranch(x, x.succ[0], inner)
		s.emit(opElse)
		s.doBranch(x, x.succ[1], inner)
		s.emit(opEnd)
	}
}

// doBranch emits the transfer from x to y: a br to an enclosing loop or
// block, or y itself when x is its only forward predecessor (lines 22-26).
func (s *structurer) doBranch(x, y *block, ctx []frame) {
	if y.rpo <= x.rpo || s.merge[y] {
		for i := len(ctx) - 1; i >= 0; i-- {
			if f := ctx[i]; f.kind != frameIf && f.label == y {
				s.emit(opBr)
				s.out.code = wasm.AppendULEB(s.out.code, uint64(len(ctx)-1-i))
				return
			}
		}
		if s.err == nil {
			s.err = unsupported(s.n, "control flow the structurer could not place")
		}
		return
	}
	s.doTree(y, ctx)
}

// actions appends x's code, moving its relocations and call sites.
func (s *structurer) actions(x *block) {
	base := uint32(len(s.out.code))
	s.out.code = append(s.out.code, x.code...)
	for _, r := range x.relocs {
		r.Offset += base
		s.out.relocs = append(s.out.relocs, r)
	}
	for _, c := range x.calls {
		c.offset += base
		s.out.calls = append(s.out.calls, c)
	}
}
