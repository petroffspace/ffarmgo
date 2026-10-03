package farm

import (
	"math"

	"ffarmgo/internal/rng"
	"ffarmgo/internal/shaper"
	"ffarmgo/internal/x87"
)

// passDecay is DAT_100091e0, the per-pass roughness factor (≈ √½, but
// not the correctly rounded √½: 0x3FE6A09E5C65884E).
var passDecay = math.Float64frombits(0x3FE6A09E5C65884E)

// fractCtor mirrors LAB_10006fd0 (rfract, mode 0) and LAB_10007130
// (bfract, mode 1). The child is not attached to the node: it modulates
// the plasma generated here, once, at construction. The ctor draws one
// u32, then the plasma draws from a COPY of the advanced growth state,
// so it never advances the growth generator itself.
func fractCtor(name string, mode int) func(*Engine, *growCtx, int, []*Node) *Node {
	return func(e *Engine, g *growCtx, typ int, kids []*Node) *Node {
		N := e.PlasmaN
		fn := evalRfract
		if mode == 1 {
			fn = evalBfract
		}
		n := e.build(typ, nil, fn, name, g.dims)
		n.W, n.H = N, N
		n.Flags = DepPixel
		g.rng.NextUint31()
		var child *Node
		if len(kids) > 0 {
			child = kids[0]
		}
		n.priv = e.plasma(N, N, mode, child, g.rng.Clone(), g.dims)
		return n
	}
}

func evalRfract(_ *Engine, n *Node, c *Ctx) {
	grid := n.priv.([]byte)
	n.Val.D[0] = float64(grid[wrapIndex(n, c)]) * inv255
}

func evalBfract(_ *Engine, n *Node, c *Ctx) {
	grid := n.priv.([]byte)
	i := 2 * wrapIndex(n, c)
	b0 := float64(grid[i])*inv255 - 0.5
	b1 := float64(grid[i+1])*inv255 - 0.5
	n.Val.D[0], n.Val.D[1], n.Val.Flag = b0+b0, b1+b1, 0
}

// wrapIndex: (ix mod W) + (iy mod H)·W with non-negative remainders.
func wrapIndex(n *Node, c *Ctx) int {
	x, y := c.IX%n.W, c.IY%n.H
	if x < 0 {
		x += n.W
	}
	if y < 0 {
		y += n.H
	}
	return int(y*n.W + x)
}

// plasmaDesc mirrors the descriptor FUN_100065b0 works on.
type plasmaDesc struct {
	W, H                   int32
	mode                   int // 0: values (W·H bytes), 1: slopes (2·W·H bytes)
	grid                   []float32
	rng                    *rng.Rand // desc+0xc
	vMin, vMax, dMin, dMax float64   // +0x18, +0x20, +0x28, +0x30
	rough                  float64   // +0x58
	child                  *Node     // +0x48
	pix, coord             bool      // +0x50, +0x4c
	ctx                    Ctx       // the ctor's stack context
}

// plasma mirrors FUN_100065b0: diamond-square on a torus with random
// displacement roughness^h · shape(u31), h = child(x, y).
func (e *Engine) plasma(W, H int32, mode int, child *Node, r *rng.Rand, dims Dims) []byte {
	p := &plasmaDesc{W: W, H: H, mode: mode, grid: make([]float32, W*H), rng: r, rough: 1}

	// Pins (065db–06651), raw coordinates.
	switch {
	case W == H:
		p.grid[0] = 0
	case W < H:
		for y := int32(0); y < H; y += W {
			p.grid[y*W] = 0
		}
	default:
		for x := int32(0); x < W; x += H {
			p.grid[x] = 0
		}
	}
	extent := min(W, H) / 2

	if child != nil {
		if child.Flags&DepPixel != 0 && (child.W != W || child.H != H) {
			child = e.pol2map(child, dims)
		}
		p.child = child
		p.pix = child.Flags&DepPixel != 0
		p.coord = child.Flags&DepCoord != 0
		p.ctx.Mask = DepPixel | DepCoord
		e.prepare(child, &p.ctx)
	}

	for extent > 0 {
		p.diamond(e, extent)
		p.rough *= passDecay
		p.square(e, extent)
		p.rough *= passDecay
		extent /= 2
	}
	return p.output()
}

func (p *plasmaDesc) at(x, y int32) float64 {
	for x < 0 {
		x += p.W
	}
	for x >= p.W {
		x -= p.W
	}
	for y < 0 {
		y += p.H
	}
	for y >= p.H {
		y -= p.H
	}
	return float64(p.grid[y*p.W+x])
}

// cell finishes one cell (shared tail of 068f0 and 06b40): average,
// displacement, value trackers. Returns the double value.
func (p *plasmaDesc) cell(e *Engine, x, y int32, sum float64) float64 {
	tmp := sum * 0.25 // DAT_100091e8
	scale := x87.Pow(p.rough, p.height(e, x, y))
	v := shaper.ShapedDraw(p.rng.NextUint31, 0, scale) + tmp
	if v > p.vMax {
		p.vMax = v
	} else if below(v, p.vMin) {
		p.vMin = v
	}
	return v
}

// height mirrors FUN_10006a80's child evaluation.
func (p *plasmaDesc) height(e *Engine, x, y int32) float64 {
	if p.child == nil {
		return 0 // the DLL reads an uninitialized stack double here
	}
	if p.pix {
		p.ctx.IX, p.ctx.IY = x, y
	}
	if p.coord {
		p.ctx.U = float64(x) / float64(p.W)
		p.ctx.V = float64(y) / float64(p.H)
	}
	e.eval(p.child, &p.ctx)
	return p.child.Val.D[0]
}

// diamond mirrors FUN_100068f0: centers at odd multiples of e, x outer.
func (p *plasmaDesc) diamond(e *Engine, ext int32) {
	for x := ext; x < p.W; x += 2 * ext {
		for y := ext; y < p.H; y += 2 * ext {
			s := p.at(x+ext, y+ext)
			s = p.at(x-ext, y+ext) + s
			s = p.at(x+ext, y-ext) + s
			s = p.at(x-ext, y-ext) + s
			p.grid[y*p.W+x] = float32(p.cell(e, x, y, s))
		}
	}
}

// square mirrors FUN_10006b40: phase 1 (even x, odd y), phase 2 (odd x,
// even y), x outer; slope trackers inline at the finest extent.
func (p *plasmaDesc) square(e *Engine, ext int32) {
	for phase := 0; phase < 2; phase++ {
		x0, y0 := int32(0), ext
		if phase == 1 {
			x0, y0 = ext, 0
		}
		for x := x0; x < p.W; x += 2 * ext {
			for y := y0; y < p.H; y += 2 * ext {
				s := p.at(x, y+ext)
				s = p.at(x, y-ext) + s
				s = p.at(x+ext, y) + s
				s = p.at(x-ext, y) + s
				v := p.cell(e, x, y, s)
				if p.mode != 0 && ext == 1 {
					p.trackDiff(p.at(x+1, y) - v)
					p.trackDiff(p.at(x, y+1) - v)
				}
				p.grid[y*p.W+x] = float32(v)
			}
		}
	}
}

func (p *plasmaDesc) trackDiff(d float64) {
	if d > p.dMax {
		p.dMax = d
	} else if below(d, p.dMin) {
		p.dMin = d
	}
}

// output mirrors 0672c–0689a.
func (p *plasmaDesc) output() []byte {
	W, H := p.W, p.H
	if p.mode == 0 {
		out := make([]byte, W*H)
		recip := 1 / (p.vMax - p.vMin)
		negOff := -(recip * p.vMin)
		for i := range out {
			out[i] = x87.Encode(float64(float64(p.grid[i])*recip) + negOff)
		}
		return out
	}
	out := make([]byte, 2*W*H)
	rangeD := p.dMax - p.dMin
	for y := int32(0); y < H; y++ { // channel 0: horizontal backward differences
		prev := p.at(W-1, y)
		for x := int32(0); x < W; x++ {
			cur := p.at(x, y)
			out[2*(y*W+x)] = x87.Encode(((cur - prev) - p.dMin) / rangeD)
			prev = cur
		}
	}
	for x := int32(0); x < W; x++ { // channel 1: vertical backward differences
		prev := p.at(x, H-1)
		for y := int32(0); y < H; y++ {
			cur := p.at(x, y)
			out[2*(y*W+x)+1] = x87.Encode(((cur - prev) - p.dMin) / rangeD)
			prev = cur
		}
	}
	return out
}
