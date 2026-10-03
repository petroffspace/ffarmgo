// Package farm is the expression-tree engine of ffarm.8bf: node
// construction (FUN_10004920), the prepare pass (FUN_10004dd0), per-pixel
// evaluation (FUN_10004f30/FUN_10004f90), every operator's constructor
// and evaluation function, and the renderer (FUN_10001240).
//
// Arithmetic model: the plugin runs on the x87 FPU with the Windows
// default precision control (53-bit mantissa), so +, −, ×, ÷ round to
// double after every instruction and plain float64 reproduces them, as
// long as the Go compiler does not fuse a multiply-add — hence the
// explicit float64(a*b) conversions in sums. FYL2X, F2XM1 and MSVCRT
// pow carry extra precision into the next rounding; they come from the
// extended-precision model in internal/x87 (ext.go). FSIN/FCOS (polar
// points only, which no constructor produces) use Go's math package.
package farm

import (
	"math"

	"ffarmgo/internal/x87"
)

// Runtime type IDs, in FUN_10008030 registration order (the counter
// pre-increments, so IDs start at 1).
const (
	TNode    = 1 // DAT_1000f0d0: child reference (4 bytes)
	TScalar  = 2 // DAT_1000eff8: double
	TExpo    = 3 // DAT_1000effc: double, random leaf −ln(1−U)
	TBipolar = 4 // DAT_1000f000: double, random leaf 2U−1
	TShaped  = 5 // DAT_1000f004: double, random leaf = shaped draw
	TEfd0    = 6 // DAT_1000efd0: 8 bytes, no methods
	TMap     = 7 // DAT_1000efcc: coordinate map (u, v, w), abstract
	TPoint   = 8 // DAT_1000efd4: (a, b, polar flag)
	TImage   = 9 // DAT_1000f0cc: (r, g, b, a, hsv flag)
)

// Dependency bits in Node.Flags and Ctx.Mask (node+0x34, ctx+0x20).
const (
	DepCoord = 0x002 // reads ctx.U/V/W
	DepPixel = 0x800 // reads ctx.IX/IY; node has pixel dimensions
)

// Value is one typed value. Field use by type: scalars D[0]; map
// D[0..2]; point D[0..1] + Flag (polar); image D[0..3] RGBA + Flag (HSV).
type Value struct {
	D    [4]float64
	Flag int32
}

// Ctx is the 40-byte evaluation context (FUN_10001240's ctx at
// DAT_1000aa38): +0 u, +8 v, +0x10 w, +0x18 ix, +0x1c iy, +0x20 mask.
type Ctx struct {
	U, V, W float64
	IX, IY  int32
	Mask    uint32
}

// Dims is the growth/render dimension record (DAT_1000a930 entries):
// output width and height. The record's +8 "interpolate" flag, which
// would make pol2map bilinear, is only ever written as 0 (FUN_10001bd0),
// so pol2map is always nearest-neighbour here.
type Dims struct {
	W, H int32
}

// Node mirrors the 0x80-byte node built by FUN_10004920.
type Node struct {
	Name  string
	Type  int     // requested type (+0x14)
	Kids  []*Node // child nodes (+0x28); nil where the argument is a value
	Args  []Value // argument slots inside the value buffer (+0x24)
	Lazy  []bool  // +0x28 entry +4: parent's fn evaluates this child itself
	Flags uint32  // +0x34
	W, H  int32   // +0x38, +0x3c
	Val   Value   // result slot (start of the value buffer, +0x18)

	fn   func(e *Engine, n *Node, ctx *Ctx) // +0x1c
	prep func(e *Engine, n *Node)           // +0x78
	priv any                                // +0x30
}

// arg is one constructor argument: a child node or an immediate value.
type arg struct {
	node *Node
	val  Value
}

// build mirrors FUN_10004920. Pixel-dimensioned children (DepPixel,
// both sides > 1) agree on a size, or else the node takes d's size and
// every child that differs from d in BOTH width and height is wrapped
// in pol2map. Flags are the OR of the (possibly wrapped) children's.
func (e *Engine) build(typ int, args []arg, fn func(*Engine, *Node, *Ctx), name string, d Dims) *Node {
	n := &Node{
		Name: name, Type: typ, fn: fn, prep: prepDims,
		Kids: make([]*Node, len(args)), Args: make([]Value, len(args)), Lazy: make([]bool, len(args)),
		W: 1, H: 1,
	}
	seen, conflict := false, false
	for _, a := range args {
		k := a.node
		if k == nil || k.Flags&DepPixel == 0 || k.W <= 1 || k.H <= 1 {
			continue
		}
		if !seen {
			n.W, n.H, seen = k.W, k.H, true
		} else if conflict || k.W != n.W || k.H != n.H {
			conflict = true
		}
	}
	if conflict {
		n.W, n.H = d.W, d.H
	}
	for i, a := range args {
		k := a.node
		if k == nil {
			n.Args[i] = a.val
			continue
		}
		if k.Flags&DepPixel != 0 && conflict && k.W > 1 && k.H > 1 && k.W != d.W && k.H != d.H {
			k = e.pol2map(k, d)
		}
		n.Kids[i] = k
		n.Flags |= k.Flags
	}
	return n
}

// prepDims is the default prepare method (LAB_100048c0): a pixel node
// takes the size of its LAST pixel-dimensioned child, or 1×1.
func prepDims(e *Engine, n *Node) {
	if len(n.Kids) == 0 {
		return
	}
	w, h := int32(1), int32(1)
	for _, k := range n.Kids {
		if k != nil && k.Flags&DepPixel != 0 && k.W > 1 && k.H > 1 {
			w, h = k.W, k.H
		}
	}
	if n.Flags&DepPixel != 0 {
		n.W, n.H = w, h
	}
}

// pol2map mirrors FUN_10005140: wrap a pixel-dimensioned node so it is
// sampled through (u, v) instead of (ix, iy).
func (e *Engine) pol2map(child *Node, d Dims) *Node {
	n := e.build(child.Type, []arg{{node: child}}, evalPol2map, "pol2map", d)
	n.Lazy[0] = true
	n.Flags = child.Flags&^DepPixel | DepCoord
	return n
}

// evalPol2map mirrors FUN_100051c0 (nearest-neighbour path).
func evalPol2map(e *Engine, n *Node, ctx *Ctx) {
	k := n.Kids[0]
	ix := x87.Ftol(math.Floor(float64(k.W) * ctx.U))
	iy := x87.Ftol(math.Floor(float64(k.H) * ctx.V))
	mask, sx, sy := ctx.Mask, ctx.IX, ctx.IY
	ctx.Mask |= DepPixel
	ctx.IX, ctx.IY = ix, iy
	e.eval(k, ctx)
	ctx.Mask, ctx.IX, ctx.IY = mask, sx, sy
	n.Val = k.Val
}

// eval mirrors FUN_10004f30/FUN_10004f90 for unshared nodes (every node
// of a grown tree: the +0x70 share marker stays 0, so the snapshot cache
// at +0x48 is never consulted). A node independent of the changing
// context keeps the value computed in the prepare pass.
func (e *Engine) eval(n *Node, ctx *Ctx) {
	if ctx.Mask&n.Flags == 0 {
		return
	}
	for i, k := range n.Kids {
		if k != nil && !n.Lazy[i] {
			e.eval(k, ctx)
			n.Args[i] = k.Val
		}
	}
	n.fn(e, n, ctx)
}

// prepare mirrors FUN_10004dd0 for unshared nodes: post-order, copy each
// constant child's value into its slot, run the prepare method, and
// evaluate the node once if it is constant under ctx.Mask.
func (e *Engine) prepare(n *Node, ctx *Ctx) bool {
	for i, k := range n.Kids {
		if k != nil && e.prepare(k, ctx) {
			n.Args[i] = k.Val
		}
	}
	if n.prep != nil {
		n.prep(e, n)
	}
	if ctx.Mask&n.Flags != 0 {
		return false
	}
	n.fn(e, n, ctx)
	return true
}
