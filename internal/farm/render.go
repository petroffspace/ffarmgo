package farm

import (
	"runtime"
	"sync"
	"sync/atomic"

	"ffarmgo/internal/gdo"
	"ffarmgo/internal/rng"
	"ffarmgo/internal/x87"
)

// Tree is a grown expression tree plus the generator state after growth.
type Tree struct {
	Root     *Node
	RootType int
	Rng      *rng.Rand
	Log      []Choice // roulette choices, in selection order
}

// Choice is one entry of a record's choice log (FUN_100055e0, 0x14-byte
// entries at record +0x40): the pool a roulette selection was made from
// and the operator it picked. Depth −1 is the root pool.
type Choice struct {
	Parent, Pos, Depth int
	Op                 int
}

// Grow mirrors FUN_10006540 → FUN_10005850: restore the generator from
// the file's context blob and grow the tree for an output of size d,
// reading the current source (the leaves take its size).
func (e *Engine) Grow(f *gdo.File, d Dims) *Tree {
	return e.growFrom(f, d, rng.FromState(f.Context.RNG()))
}

// growFrom grows with an explicit generator (the Farm dialog grows new
// cells from the cell's live dims record, not from a saved blob).
func (e *Engine) growFrom(f *gdo.File, d Dims, r *rng.Rand) *Tree {
	g := &growCtx{dims: d, rng: r}
	root := e.grow(f, g, int(f.TagB), 0, 0, -1)
	return &Tree{Root: root, RootType: int(f.TagB), Rng: g.rng, Log: g.log}
}

// grow mirrors FUN_10005850. Selection is type-blind; children are grown
// before the operator's ctor runs. At depth 4, image and scalar children
// become source leaves, types with a random constructor become constant
// leaves, and abstract types (the map type) are re-selected at the SAME
// depth.
func (e *Engine) grow(f *gdo.File, g *growCtx, typ, parent, pos, depth int) *Node {
	n, logged := selectEntry(f, g.rng, parent, pos, depth)
	op := int(n.ID)
	if logged {
		g.log = append(g.log, Choice{Parent: parent, Pos: pos, Depth: depth, Op: op})
	}
	if op < 0 || op >= len(ops) {
		panic("farm: pool entry names an unregistered operator")
	}
	spec := ops[op]
	kids := make([]*Node, len(spec.children))
	for i, ct := range spec.children {
		switch {
		case depth+1 < 4:
			kids[i] = e.grow(f, g, ct, op, i, depth+1)
		case ct == TImage:
			kids[i] = e.imageLeaf(ct, g.dims)
		case ct == TScalar:
			kids[i] = e.lumaLeaf(ct, g.dims)
		case typeCtors[ct] != nil:
			kids[i] = e.constLeaf(ct, typeCtors[ct], g)
		default:
			kids[i] = e.grow(f, g, ct, op, i, depth)
		}
	}
	return spec.ctor(e, g, typ, kids)
}

// Frame is an 8-bit interleaved image as the host hands it to the
// filter: Planes bytes per pixel, the first Channels of which are colour
// (FUN_10003c00 maps the host's image mode to Channels: grayscale and
// duotone 1, RGB and Lab 3, CMYK 4); any further planes are
// transparency.
type Frame struct {
	W, H     int
	Planes   int
	Channels int
	Pix      []byte
}

func (f *Frame) at(x, y int) []byte {
	i := (y*f.W + x) * f.Planes
	return f.Pix[i : i+f.Planes]
}

// rectSource is the filter's view of the source: the filter rectangle
// only (inRect = filterRect), read as FUN_10001010 does — fewer than
// three planes means gray, replicated to R, G and B.
type rectSource struct {
	f      *Frame
	x0, y0 int
	w, h   int32
}

func (s rectSource) Size() (int32, int32) { return s.w, s.h }

func (s rectSource) RGB(x, y int32) (uint8, uint8, uint8) {
	p := s.f.at(s.x0+int(x), s.y0+int(y))
	if s.f.Planes < 3 {
		return p[0], p[0], p[0]
	}
	return p[0], p[1], p[2]
}

// Render mirrors the final render (FUN_100010d0 → FUN_10001240) for a
// host with enough memory to take the whole filter rectangle as one
// strip (every modern host). See RenderStrips.
func (e *Engine) Render(f *gdo.File, src *Frame, mask []byte) *Frame {
	return e.RenderStrips(f, src, mask, 0)
}

// StripRows mirrors FUN_10001890: the rows per strip for a rectangle of
// width w and height h. A row costs (2·planes + 1)·w bytes (input,
// output and mask); the strip grows one row at a time until it reaches
// maxSpace or the rectangle's height, so the last row added may push it
// past maxSpace. maxSpace ≤ 0 means unlimited.
func StripRows(w, h, planes int, maxSpace int64) int {
	if maxSpace <= 0 {
		return max(h, 1)
	}
	row := int64(2*planes+1) * int64(w)
	n := 1
	if row >= maxSpace {
		return n
	}
	for n < h {
		n++
		if int64(n)*row >= maxSpace {
			break
		}
	}
	return n
}

// RenderStrips mirrors the final render (FUN_100010d0 → FUN_10001240)
// and returns a new frame; src is not modified.
//
// mask, when non-nil, is the selection (one byte per pixel, W·H): the
// filter rectangle becomes the bounding box of the non-zero bytes,
// pixels with mask 0 are copied unevaluated, and the rest are blended
// with the source by mask/255. Pixels outside the rectangle are
// untouched.
//
// The rectangle is processed in horizontal strips of StripRows rows
// (maxSpace is the host's FilterRecord.maxSpace; ≤ 0 means one strip).
// FUN_10001240 runs once per strip and treats the strip as the whole
// image: the tree is regrown for the strip's size, u and v span the
// strip, and the source leaves see (and mirror-tile) only the strip.
// The dither generator and the evaluation context carry over between
// strips.
//
// Assumption: FUN_100010d0 requests the last strip as top + rows even
// when that runs past the rectangle; the host is assumed to clip it to
// the filter rectangle.
func (e *Engine) RenderStrips(f *gdo.File, src *Frame, mask []byte, maxSpace int64) *Frame {
	out := &Frame{W: src.W, H: src.H, Planes: src.Planes, Channels: src.Channels,
		Pix: append([]byte(nil), src.Pix...)}
	x0, y0, x1, y1 := 0, 0, src.W, src.H
	if mask != nil {
		var ok bool
		if x0, y0, x1, y1, ok = bounds(mask, src.W, src.H); !ok {
			return out
		}
	}
	rows := StripRows(x1-x0, y1-y0, src.Planes, maxSpace)
	ctx := &Ctx{} // DAT_1000aa38: a static, so it persists across strips
	for top := y0; top < y1; top += rows {
		e.PlasmaN = 256 // FUN_100065a0(0x100)
		e.renderStrip(f, src, out, mask, x0, top, x1, min(top+rows, y1), ctx)
	}
	return out
}

// Preview renders a cell the way the Farm dialog's grid sizes its
// plasmas (DAT_1000a1d8 keeps its load-time 64; only the final render
// sets 256), over the whole of src in one pass. It is a port aid for
// previews: how the dialog's preview thread samples the image is not
// modelled, so callers pass a source already scaled to the cell.
func (e *Engine) Preview(f *gdo.File, src *Frame) *Frame {
	out := &Frame{W: src.W, H: src.H, Planes: src.Planes, Channels: src.Channels,
		Pix: append([]byte(nil), src.Pix...)}
	e.PlasmaN = 64
	e.renderStrip(f, src, out, nil, 0, 0, src.W, src.H, &Ctx{})
	return out
}

// renderStrip mirrors one call of FUN_10001240 on the rectangle
// [x0, x1) × [y0, y1).
func (e *Engine) renderStrip(f *gdo.File, src, out *Frame, mask []byte, x0, y0, x1, y1 int, ctx *Ctx) {
	W, H := int32(x1-x0), int32(y1-y0)
	e.src = rectSource{f: src, x0: x0, y0: y0, w: W, h: H}
	root, rootType := e.growStrip(f, Dims{W: W, H: H}, ctx)
	p := &stripPass{e: e, src: src, out: out, mask: mask, x0: x0, y0: y0, w: W, h: H,
		root: root, rootType: rootType}
	if n := e.workers(int(W) * int(H)); n > 1 && H > 1 && e.renderParallel(p, ctx, n) {
		return
	}
	p.rows(e.dither, ctx, 0, H, -1)
}

// growStrip grows the tree for one strip and runs the prepare pass.
func (e *Engine) growStrip(f *gdo.File, d Dims, ctx *Ctx) (*Node, int) {
	t := e.Grow(f, d)
	// FUN_10005370: a pixel-dimensioned root whose size differs from the
	// context's (ix, iy) — zero on a first render, the previous strip's
	// last pixel otherwise — is wrapped in pol2map.
	root := t.Root
	if root.Flags&DepPixel != 0 && (root.W != ctx.IX || root.H != ctx.IY) {
		root = e.pol2map(root, d)
	}
	ctx.Mask = DepPixel | DepCoord
	e.prepare(root, ctx)
	return root, t.RootType
}

// stripPass is one strip's pixel loop: its tree, source and output.
type stripPass struct {
	e        *Engine
	src, out *Frame
	mask     []byte
	x0, y0   int
	w, h     int32
	root     *Node
	rootType int
}

func (p *stripPass) maskAt(x, y int32) byte {
	if p.mask == nil {
		return 0xff
	}
	return p.mask[(int(y)+p.y0)*p.src.W+int(x)+p.x0]
}

// rows evaluates rows [ya, yb) drawing dither values from g. With k ≥ 0
// it checks that every pixel draws exactly k values and reports false
// as soon as one does not.
func (p *stripPass) rows(g *rng.Rand, ctx *Ctx, ya, yb int32, k int) bool {
	e, src, root := p.e, p.src, p.root
	e.dither = g
	// DAT_1000a3ac: the output reads an image (one value per channel)
	// only in three-channel modes; otherwise every channel gets D[0].
	imageOut := src.Channels == 3
	colour := min(src.Planes, src.Channels)
	// FUN_10001240's stack buffer persists across pixels, but only D[0]
	// is ever written for a scalar root, so a fresh buffer per band holds
	// the same values.
	var v Value
	for y := ya; y < yb; y++ {
		for x := int32(0); x < p.w; x++ {
			var m byte
			if p.mask != nil {
				m = p.maskAt(x, y)
				if m == 0 {
					continue // copied unevaluated
				}
			}
			ctx.IX, ctx.IY = x, y
			ctx.U = float64(x) / float64(p.w) // FUN_10005410
			ctx.V = float64(y) / float64(p.h)
			before := g.Draws()
			e.eval(root, ctx)
			if k >= 0 && g.Draws()-before != k {
				return false
			}
			// FUN_10004f30 copies only the root type's size.
			if isScalarType(p.rootType) {
				v.D[0] = root.Val.D[0]
			} else {
				v = root.Val
			}
			// FUN_10004060 would convert HSV here when the flag is set;
			// no operator ever sets it.
			o := p.out.at(int(x)+p.x0, int(y)+p.y0)
			s := src.at(int(x)+p.x0, int(y)+p.y0)
			for c := src.Planes - 1; c >= src.Channels; c-- {
				o[c] = 0xff
			}
			mm := float64(m) * inv255
			im := 1 - mm
			for c := colour - 1; c >= 0; c-- {
				val := v.D[0]
				if imageOut {
					val = v.D[c]
				}
				if m != 0 {
					val = float64(val*mm) + float64(float64(float64(s[c])*im)*inv255)
				}
				o[c] = x87.Encode(val)
			}
		}
	}
	return true
}

// workers is how many goroutines render a strip of n pixels.
func (e *Engine) workers(n int) int {
	if e.Workers > 0 {
		return e.Workers
	}
	if n < 1<<16 {
		return 1
	}
	return runtime.GOMAXPROCS(0)
}

// renderParallel splits a strip's pixel loop across n goroutines and
// reproduces the sequential loop exactly, or reports false with the
// engine and ctx as they were after the prepare pass, so the caller can
// run the loop sequentially.
//
// Pixels are independent except for the dither generator, which every
// dither evaluation advances by one draw. Each worker evaluates its own
// copy of the prepared tree (node values are per-tree; plasma grids and
// noise seeds are read-only and shared) and takes chunks of rows; a
// chunk's generator is the post-prepare state jumped ahead by k draws
// per evaluated pixel before it. That assumes every pixel draws the
// same k values, which each worker checks pixel by pixel. On success
// the generator and ctx are left where the sequential loop leaves them.
func (e *Engine) renderParallel(p *stripPass, ctx *Ctx, n int) bool {
	base := e.dither.State()

	// Evaluated pixels before each row, and the last evaluated pixel.
	before := make([]int, p.h+1)
	lastX, lastY := int32(-1), int32(-1)
	for y := int32(0); y < p.h; y++ {
		c := 0
		for x := int32(0); x < p.w; x++ {
			if p.maskAt(x, y) != 0 {
				c++
				lastX, lastY = x, y
			}
		}
		before[y+1] = before[y] + c
	}
	if before[p.h] == 0 {
		return false
	}

	ws := make([]stripPass, n)
	for i := range ws {
		ws[i] = *p
		ws[i].e = &Engine{perm: e.perm, PlasmaN: e.PlasmaN, src: e.src, Workers: 1}
		ws[i].root = cloneTree(p.root, map[*Node]*Node{})
	}

	// k: draws per pixel, measured on the first row with evaluated pixels.
	y := int32(0)
	for before[y+1] == before[y] {
		y++
	}
	g := rng.FromState(base)
	c := *ctx
	ws[0].rows(g, &c, y, y+1, -1)
	cnt := before[y+1] - before[y]
	if g.Draws()%cnt != 0 {
		return false
	}
	k := g.Draws() / cnt

	const chunk = 4
	var next atomic.Int32
	var failed atomic.Bool
	var wg sync.WaitGroup
	for i := range ws {
		wg.Add(1)
		go func(w *stripPass) {
			defer wg.Done()
			for !failed.Load() {
				ya := next.Add(chunk) - chunk
				if ya >= p.h {
					return
				}
				g := rng.FromState(base)
				g.Skip(uint64(before[ya]) * uint64(k))
				c := *ctx
				if !w.rows(g, &c, ya, min(ya+chunk, p.h), k) {
					failed.Store(true)
				}
			}
		}(&ws[i])
	}
	wg.Wait()
	if failed.Load() {
		return false
	}
	e.dither.Skip(uint64(before[p.h]) * uint64(k))
	ctx.IX, ctx.IY = lastX, lastY
	ctx.U = float64(lastX) / float64(p.w)
	ctx.V = float64(lastY) / float64(p.h)
	return true
}

// cloneTree copies a prepared tree: fresh nodes, values and argument
// slots; shared methods and private blocks (read-only during eval).
func cloneTree(n *Node, seen map[*Node]*Node) *Node {
	if n == nil {
		return nil
	}
	if c, ok := seen[n]; ok {
		return c
	}
	c := *n
	seen[n] = &c
	c.Args = append([]Value(nil), n.Args...)
	c.Kids = make([]*Node, len(n.Kids))
	for i, k := range n.Kids {
		c.Kids[i] = cloneTree(k, seen)
	}
	return &c
}

func isScalarType(t int) bool { return t >= TScalar && t <= TEfd0 }

// bounds is the bounding box [x0, x1) × [y0, y1) of the non-zero mask
// bytes (Photoshop's filterRect for a selection).
func bounds(mask []byte, w, h int) (x0, y0, x1, y1 int, ok bool) {
	x0, y0, x1, y1 = w, h, 0, 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if mask[y*w+x] != 0 {
				x0, y0 = min(x0, x), min(y0, y)
				x1, y1 = max(x1, x+1), max(y1, y+1)
			}
		}
	}
	return x0, y0, x1, y1, x1 > x0
}
