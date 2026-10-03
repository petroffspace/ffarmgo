package farm

import (
	"math"

	"ffarmgo/internal/rng"
	"ffarmgo/internal/shaper"
	"ffarmgo/internal/x87"
)

// Constants from the image (bits verified with tools/listing.py f64).
var (
	inv255 = math.Float64frombits(0x3F70101010101010) // DAT_100090f0
	lumR   = math.Float64frombits(0x3F533606EC4C8E17) // DAT_100091b0 = 0.299/255
	lumG   = math.Float64frombits(0x3F62DB8FC9213194) // DAT_100091a8 = 0.587/255
	lumB   = math.Float64frombits(0x3F3D4C6706C53C05) // DAT_100091a0 = 0.114/255
	ri2cxK = math.Float64frombits(0x3FB999999999999A) // DAT_10009228 = 0.1
)

// Source is the filter's view of the image being filtered (FUN_10001010
// reads it through the host's filter record; FUN_10001000 returns its
// size). Render installs one per call.
type Source interface {
	Size() (w, h int32)
	RGB(x, y int32) (r, g, b uint8)
}

// Engine holds the plugin's process-wide state.
type Engine struct {
	perm    [1024]int32 // DAT_1000d8e4, shuffled once at load (FUN_10008300)
	dither  *rng.Rand   // DAT_1000a370, never reseeded
	PlasmaN int32       // DAT_1000a1d8: 64 for previews, 256 for the final render
	src     Source      // set by Render

	// Workers is how many goroutines evaluate a strip's pixels: 0 picks
	// GOMAXPROCS for strips of 65536 pixels or more and 1 below that.
	// The result is the same for any value (see renderParallel).
	Workers int
}

// NewEngine mirrors plugin load: the noise permutation table is shuffled
// from srand48(123456790) and the dither generator starts at its static
// seed. Reuse one engine for successive renders to model one host
// session (the dither generator keeps advancing).
func NewEngine() *Engine {
	e := &Engine{dither: rng.FromState(rng.DitherState), PlasmaN: 64}
	g := rng.Seed48(rng.NoiseSeed)
	for i := range e.perm {
		e.perm[i] = int32(i)
	}
	// FUN_10008300: for i = 0..1022, j = i + ftol(drand·(1024−i)); swap.
	for i := int32(0); i < 1023; i++ {
		j := i + x87.Ftol(g.NextDouble()*float64(1024-i))
		e.perm[i], e.perm[j] = e.perm[j], e.perm[i]
	}
	return e
}

// growCtx is the growth context: output dims plus the 48-bit generator
// at +0xc (restored from the .gdo blob).
type growCtx struct {
	dims Dims
	rng  *rng.Rand
	log  []Choice
}

// opSpec is one registry entry (FUN_10005500): child types in
// consumption order and the constructor.
type opSpec struct {
	name     string
	children []int
	ctor     func(e *Engine, g *growCtx, typ int, kids []*Node) *Node
}

// simple returns a constructor that just builds a node around fn.
func simple(name string, fn func(*Engine, *Node, *Ctx)) func(*Engine, *growCtx, int, []*Node) *Node {
	return func(e *Engine, g *growCtx, typ int, kids []*Node) *Node {
		return e.build(typ, nodeArgs(kids), fn, name, g.dims)
	}
}

func nodeArgs(kids []*Node) []arg {
	a := make([]arg, len(kids))
	for i, k := range kids {
		a[i] = arg{node: k}
	}
	return a
}

// ops is the registry in slot order (tools/listing.py ops). Node names
// are the strings each ctor passes to FUN_10004920.
var ops = [22]opSpec{
	{"mu", nil, func(e *Engine, g *growCtx, typ int, kids []*Node) *Node {
		n := e.build(typ, nil, func(_ *Engine, n *Node, c *Ctx) { n.Val.D[0] = c.U }, "m", g.dims)
		n.Flags = DepCoord
		return n
	}},
	{"mv", nil, func(e *Engine, g *growCtx, typ int, kids []*Node) *Node {
		n := e.build(typ, nil, func(_ *Engine, n *Node, c *Ctx) { n.Val.D[0] = c.V }, "m", g.dims)
		n.Flags = DepCoord
		return n
	}},
	{"minpe", []int{TScalar}, simple("minpe", func(_ *Engine, n *Node, _ *Ctx) {
		n.Val.D[0] = 1 - n.Args[0].D[0]
	})},
	{"peplus", []int{TScalar, TScalar}, simple("peplus", func(_ *Engine, n *Node, _ *Ctx) {
		a, b := n.Args[0].D[0], n.Args[1].D[0]
		n.Val.D[0] = (b + a) - float64(b*a)
	})},
	{"petimes", []int{TScalar, TScalar}, simple("petimes", func(_ *Engine, n *Node, _ *Ctx) {
		n.Val.D[0] = n.Args[1].D[0] * n.Args[0].D[0]
	})},
	{"pepow", []int{TScalar, TScalar}, simple("pepow", func(_ *Engine, n *Node, _ *Ctx) {
		n.Val.D[0] = x87.Pow(n.Args[0].D[0], n.Args[1].D[0])
	})},
	{"switch", []int{TScalar, TScalar, TScalar}, simple("gswitch", func(_ *Engine, n *Node, _ *Ctx) {
		if below(n.Args[0].D[0], 0.5) {
			n.Val.D[0] = n.Args[1].D[0]
		} else {
			n.Val.D[0] = n.Args[2].D[0]
		}
	})},
	{"mix", []int{TScalar, TScalar, TScalar}, simple("g", func(_ *Engine, n *Node, _ *Ctx) {
		a := n.Args[0].D[0]
		n.Val.D[0] = float64((1-a)*n.Args[2].D[0]) + float64(a*n.Args[1].D[0])
	})},
	{"bias", []int{TScalar, TScalar}, simple("gbias", func(_ *Engine, n *Node, _ *Ctx) {
		n.Val.D[0] = bias(n.Args[0].D[0], n.Args[1].D[0])
	})},
	{"gain", []int{TScalar, TScalar}, simple("ggain", func(_ *Engine, n *Node, _ *Ctx) {
		n.Val.D[0] = gain(n.Args[0].D[0], n.Args[1].D[0])
	})},
	{"ri2cx", []int{TPoint, TScalar}, simple("ri2cx", evalRi2cx)},
	{"dopnoise", []int{TScalar}, noiseCtor("dopnoise2", false, false)},
	{"dopbnoise", []int{TScalar}, noiseCtor("dopbnoise2", false, true)},
	{"dopdnoise", []int{TScalar}, noiseCtor("dopdnoise2", true, false)},
	{"rfract", []int{TScalar}, fractCtor("rfract", 0)},
	{"bfract", []int{TScalar}, fractCtor("bfract", 1)},
	{"dither", []int{TScalar, TScalar}, simple("dither", evalDither)},
	{"makrgb", []int{TScalar, TScalar, TScalar}, simple("makrgb", func(_ *Engine, n *Node, _ *Ctx) {
		n.Val = Value{D: [4]float64{n.Args[0].D[0], n.Args[1].D[0], n.Args[2].D[0], 1}}
	})},
	{"gswitch", []int{TScalar, TImage, TImage}, simple("gswitch", func(_ *Engine, n *Node, _ *Ctx) {
		if below(n.Args[0].D[0], 0.5) {
			n.Val = n.Args[1]
		} else {
			n.Val = n.Args[2]
		}
	})},
	{"gmix", []int{TScalar, TImage, TImage}, simple("g", func(_ *Engine, n *Node, _ *Ctx) {
		a := n.Args[0].D[0]
		for i := 0; i < 4; i++ { // the flag word is left untouched
			n.Val.D[i] = float64((1-a)*n.Args[2].D[i]) + float64(a*n.Args[1].D[i])
		}
	})},
	{"trfmap", []int{TImage, TMap}, trfmapCtor},
	{"trfmap", []int{TScalar, TMap}, trfmapCtor},
}

// below is FCOMP + TEST AH,1: true when a < b or unordered (NaN).
func below(a, b float64) bool { return !(a >= b) }

// bias mirrors FUN_10007bc0(x, b).
func bias(x, b float64) float64 {
	s := (b + b) - 1
	if s == 0 || math.IsNaN(s) { // FCOM 0 / TEST AH,0x40: equal or unordered
		return x
	}
	a := math.Abs(s)
	if a < 1 {
		return (float64(x*s) + 1 - s) * x
	}
	p := x87.Pow(2, a)
	if s < 0 {
		return 1 - x87.Pow(1-x, p)
	}
	return x87.Pow(x, p)
}

// gain mirrors FUN_10007cd0(x, g).
func gain(x, g float64) float64 {
	f := math.Floor(x+0.5) * 2
	t := (x + x) - f
	if t > 0 {
		return (bias(t, g) + f) * 0.5
	}
	return (f - bias(-t, g)) * 0.5
}

// cartesian mirrors FUN_10008a10: a polar point (flag ≠ 0) becomes
// (r·cos θ, r·sin θ). No constructor sets the flag, so this is the
// identity in practice.
func cartesian(p Value) (x, y float64) {
	if p.Flag == 0 {
		return p.D[0], p.D[1]
	}
	return math.Cos(p.D[1]) * p.D[0], math.Sin(p.D[1]) * p.D[0]
}

// evalRi2cx mirrors LAB_10008bf0: map = (u + x·s·0.1, v + y·s·0.1).
// The map's third component is never written.
func evalRi2cx(_ *Engine, n *Node, c *Ctx) {
	x, y := cartesian(n.Args[0])
	s := n.Args[1].D[0]
	n.Val.D[0] = float64(float64(x*s)*ri2cxK) + c.U
	n.Val.D[1] = float64(float64(y*s)*ri2cxK) + c.V
}

// trfmapCtor mirrors LAB_10008130: the first child is evaluated lazily
// at the mapped coordinates; a pixel-dimensioned first child is first
// wrapped in pol2map. The node's pixel dependence comes only from the
// map child.
func trfmapCtor(e *Engine, g *growCtx, typ int, kids []*Node) *Node {
	n := e.build(typ, nodeArgs(kids), evalTrfmap, "trfmap", g.dims)
	if n.Kids[0].Flags&DepPixel != 0 {
		n.Kids[0] = e.pol2map(n.Kids[0], g.dims)
	}
	n.Lazy[0] = true
	n.Flags = n.Kids[0].Flags&^DepPixel | n.Kids[1].Flags
	return n
}

// evalTrfmap mirrors LAB_100081a0.
func evalTrfmap(e *Engine, n *Node, c *Ctx) {
	k := n.Kids[0]
	if k == nil {
		n.Val = n.Args[0]
		return
	}
	u, v, w, mask := c.U, c.V, c.W, c.Mask
	c.Mask |= DepCoord
	m := n.Args[1]
	c.U, c.V, c.W = m.D[0], m.D[1], m.D[2]
	e.eval(k, c)
	c.U, c.V, c.W, c.Mask = u, v, w, mask
	n.Val = k.Val
}

// evalDither mirrors FUN_10008d30: quantize x to n = 4d+1 levels with a
// random choice among four neighbouring levels, drawn from the
// process-global dither generator.
func evalDither(e *Engine, n *Node, _ *Ctx) {
	x, d := n.Args[0].D[0], n.Args[1].D[0]
	lv := d*4 + 1
	inv := 1 / lv
	s := lv * x
	i := x87.Ftol(math.Floor(s))
	f := s - float64(i)
	a := (f + 1) * 0.5
	ia := 1 - a
	b := f * 0.5
	ib := 1 - b
	r := e.dither.NextDouble()
	c1 := float64(ia*ia) * 0.5
	switch {
	case below(r, c1):
		i--
	case below(r, float64(float64(ib*ib)*0.5)+float64(ia*a)+c1):
	case below(r, float64(ib*b)+float64(float64(a*a)*0.5)+(float64(float64(ib*ib)*0.5)+float64(ia*a)+c1)):
		i++
	default:
		i += 2
	}
	n.Val.D[0] = float64(i) * inv
}

// --- depth-4 leaves ---------------------------------------------------------

// imageLeaf mirrors FUN_10004430 ("phtshp"): the source pixel at
// (ix, iy), mirror-tiled, as an RGBA image value.
func (e *Engine) imageLeaf(typ int, d Dims) *Node {
	n := e.build(typ, nil, func(e *Engine, n *Node, c *Ctx) {
		r, g, b := e.srcPixel(c.IX, c.IY)
		n.Val = Value{D: [4]float64{float64(r) * inv255, float64(g) * inv255, float64(b) * inv255, 1}}
	}, "phtshp", d)
	e.markSource(n)
	return n
}

// lumaLeaf mirrors FUN_10004590 ("vphtshp"): source luminance.
func (e *Engine) lumaLeaf(typ int, d Dims) *Node {
	n := e.build(typ, nil, func(e *Engine, n *Node, c *Ctx) {
		r, g, b := e.srcPixel(c.IX, c.IY)
		n.Val.D[0] = float64(float64(r)*lumR) + float64(float64(g)*lumG) + float64(float64(b)*lumB)
	}, "vphtshp", d)
	e.markSource(n)
	return n
}

func (e *Engine) markSource(n *Node) {
	n.W, n.H = e.src.Size()
	n.Flags = DepPixel
	n.prep = func(e *Engine, n *Node) { n.W, n.H = e.src.Size() } // LAB_10004300
}

// srcPixel mirrors the tiling in LAB_10004310/LAB_10004480: odd tiles
// are mirrored, so the source repeats as a seamless reflection.
func (e *Engine) srcPixel(x, y int32) (r, g, b uint8) {
	w, h := e.src.Size()
	return e.src.RGB(mirror(x, w), mirror(y, h))
}

func mirror(x, w int32) int32 {
	t := x87.Ftol(math.Floor(float64(x) / float64(w)))
	m := x % w
	if m < 0 {
		m += w
	}
	if t&1 != 0 {
		m = w - m - 1
	}
	return m
}

// typeCtors are the types' random-value constructors (third field of
// the FUN_10008030 table), used for depth-4 constant leaves.
var typeCtors = map[int]func(r *rng.Rand) Value{
	TScalar: func(r *rng.Rand) Value { return Value{D: [4]float64{r.NextDouble()}} }, // LAB_10007680
	TExpo: func(r *rng.Rand) Value {
		return Value{D: [4]float64{-x87.FYL2X(1-r.NextDouble(), x87.LN2_80).Round53()}}
	}, // LAB_100076f0
	TBipolar: func(r *rng.Rand) Value { return Value{D: [4]float64{r.NextDouble()*2 - 1}} }, // LAB_100076a0
	TShaped:  func(r *rng.Rand) Value { return Value{D: [4]float64{shaped(r, 0, 1)}} },      // LAB_100076c0
	TPoint: func(r *rng.Rand) Value { // LAB_10008b30
		a := shaped(r, 0, 1)
		return Value{D: [4]float64{a, shaped(r, 0, 1)}}
	},
	TImage: func(r *rng.Rand) Value { // LAB_10004670
		a := r.NextDouble()
		b := r.NextDouble()
		return Value{D: [4]float64{a, b, r.NextDouble(), 1}}
	},
}

func shaped(r *rng.Rand, offset, scale float64) float64 {
	return shaper.ShapedDraw(r.NextUint31, offset, scale)
}

// constLeaf mirrors FUN_100059c0: draw a random value and wrap it in a
// constant node "c" (LAB_10005a10 copies its argument).
func (e *Engine) constLeaf(typ int, ctor func(*rng.Rand) Value, g *growCtx) *Node {
	v := ctor(g.rng)
	return e.build(typ, []arg{{val: v}}, func(_ *Engine, n *Node, _ *Ctx) { n.Val = n.Args[0] }, "c", g.dims)
}
