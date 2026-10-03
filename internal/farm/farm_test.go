package farm

import (
	"bytes"
	"image"
	"image/color"
	"math"
	"os"
	"testing"

	"ffarmgo/internal/gdo"
	"ffarmgo/internal/rng"
)

// gradient is a deterministic synthetic source.
type gradient struct{ w, h int32 }

func (g gradient) Size() (int32, int32) { return g.w, g.h }
func (g gradient) RGB(x, y int32) (uint8, uint8, uint8) {
	return uint8(x * 255 / (g.w - 1)), uint8(y * 255 / (g.h - 1)), uint8((x + y) * 7)
}

func withSource(src Source) *Engine {
	e := NewEngine()
	e.src = src
	return e
}

func loadRender(t *testing.T) *gdo.File {
	t.Helper()
	data, err := os.ReadFile("testdata/render.gdo")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	f, err := gdo.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f
}

// Growth replay of render.gdo (seed 0xE88C5F62A74A): 35 draws and the
// final generator state, a regression baseline pinned since the first
// port of the selector. The plasma ctors draw from a copy of the state
// and source leaves draw nothing, so only selections and ctors count.
func TestGrowDrawScheduleMatchesReplay(t *testing.T) {
	f := loadRender(t)
	e := withSource(gradient{131, 71})
	tr := e.Grow(f, Dims{131, 71})
	if tr.Rng.Draws() != 35 || tr.Rng.State() != 0x9E3D5BEADB2F {
		t.Fatalf("growth consumed %d draws, final state %012X; want 35, 9E3D5BEADB2F",
			tr.Rng.Draws(), tr.Rng.State())
	}
	if tr.Root.Name != "trfmap" {
		t.Fatalf("root = %q, want trfmap", tr.Root.Name)
	}
}

func TestPermutationTable(t *testing.T) {
	e := withSource(gradient{2, 2})
	var seen [1024]bool
	for _, v := range e.perm {
		if v < 0 || v >= 1024 || seen[v] {
			t.Fatalf("perm is not a permutation (value %d)", v)
		}
		seen[v] = true
	}
	// Pinned to the table the plugin's own FUN_10008300 builds, executed
	// under tools/emu.py (head and a checksum over all entries).
	if head := [16]int32{945, 747, 858, 923, 326, 42, 62, 657, 769, 286, 160, 36, 142, 415, 358, 561}; [16]int32(e.perm[:16]) != head {
		t.Fatalf("perm head %v, plugin builds %v", e.perm[:16], head)
	}
	sum := 0
	for i, v := range e.perm {
		sum += i * int(v)
	}
	if sum != 268583922 {
		t.Fatalf("perm checksum %d, plugin's %d", sum, 268583922)
	}
	if e.perm == withSource(gradient{2, 2}).perm {
		return
	}
	t.Fatal("perm shuffle is not deterministic")
}

func TestScalarOps(t *testing.T) {
	cases := []struct {
		op   int
		args []float64
		want float64
	}{
		{2, []float64{0.25}, 0.75},       // minpe
		{3, []float64{0.5, 0.5}, 0.75},   // peplus: a+b-ab
		{4, []float64{0.5, 0.25}, 0.125}, // petimes
		{5, []float64{0.25, 0.5}, 0.5},   // pepow: a^b
		{6, []float64{0.4, 1, 2}, 1},     // switch: a<0.5 → b
		{6, []float64{0.5, 1, 2}, 2},     //         else c
		{7, []float64{0.25, 1, 0}, 0.25}, // mix: (1-a)c + ab
		{8, []float64{0.3, 0.5}, 0.3},    // bias(x, 0.5) = x
		{8, []float64{0.5, 0.75}, 0.375}, // bias: s=0.5 → x(xs+1-s)
		{8, []float64{0.5, 1}, 0.25},     // bias: s=1 → x^2
		{9, []float64{0.3, 0.5}, 0.3},    // gain(x, 0.5) = x
		{9, []float64{0.25, 1}, 0.125},   // gain: t=0.5, bias(.5,1)=.25 → .125
	}
	e := withSource(gradient{2, 2})
	for _, c := range cases {
		kids := make([]*Node, len(c.args))
		for i, a := range c.args {
			kids[i] = e.build(TScalar, []arg{{val: Value{D: [4]float64{a}}}}, func(_ *Engine, n *Node, _ *Ctx) { n.Val = n.Args[0] }, "c", Dims{})
		}
		n := ops[c.op].ctor(e, &growCtx{rng: rng.FromState(1)}, TScalar, kids)
		ctx := &Ctx{Mask: DepPixel | DepCoord}
		e.prepare(n, ctx)
		if got := n.Val.D[0]; math.Abs(got-c.want) > 1e-15 {
			t.Errorf("%s%v = %v, want %v", ops[c.op].name, c.args, got, c.want)
		}
	}
}

// dither quantizes to multiples of 1/(4d+1) and advances its own global
// generator once per evaluation, never the growth generator.
func TestDither(t *testing.T) {
	e := withSource(gradient{2, 2})
	n := &Node{Args: []Value{{D: [4]float64{0.4}}, {D: [4]float64{1}}}}
	levels := map[float64]int{}
	for i := 0; i < 2000; i++ {
		evalDither(e, n, nil)
		levels[math.Round(n.Val.D[0]*5*1e9)/1e9]++
	}
	if e.dither.Draws() != 2000 {
		t.Fatalf("dither drew %d times, want 2000", e.dither.Draws())
	}
	for l := range levels {
		if l != math.Trunc(l) || l < 0 || l > 5 {
			t.Fatalf("dither produced %v/5, not a level", l)
		}
	}
	if len(levels) < 2 {
		t.Fatalf("dither is not random: %v", levels)
	}
}

func TestMirrorTiling(t *testing.T) {
	cases := map[int32]int32{0: 0, 4: 4, 5: 4, 9: 0, 10: 0, -1: 0, -5: 4, -6: 4}
	for x, want := range cases {
		if got := mirror(x, 5); got != want {
			t.Errorf("mirror(%d, 5) = %d, want %d", x, got, want)
		}
	}
}

// Hermite noise interpolates exactly through the lattice values and is
// continuous across cell boundaries.
func TestNoiseLattice(t *testing.T) {
	e := withSource(gradient{2, 2})
	st := &noiseState{seed: 12345}
	for _, cell := range [][2]int32{{0, 0}, {3, -2}, {-7, 5}} {
		got := e.noise([2]float64{float64(cell[0]), float64(cell[1])}, st)[2]
		if want := e.corner(cell, st)[2]; got != want {
			t.Errorf("noise at lattice %v = %v, corner value %v", cell, got, want)
		}
	}
	a := e.noise([2]float64{2 - 1e-9, 0.3}, st)[2]
	b := e.noise([2]float64{2, 0.3}, st)[2]
	if math.Abs(a-b) > 1e-6 {
		t.Errorf("noise discontinuous at x=2: %v vs %v", a, b)
	}
}

// The plasma is deterministic, spans the full byte range (min→0,
// max→255), and depends on the generator state.
func TestPlasma(t *testing.T) {
	e := withSource(gradient{2, 2})
	a := e.plasma(64, 64, 0, nil, rng.FromState(42), Dims{64, 64})
	b := e.plasma(64, 64, 0, nil, rng.FromState(42), Dims{64, 64})
	c := e.plasma(64, 64, 0, nil, rng.FromState(43), Dims{64, 64})
	if !bytes.Equal(a, b) {
		t.Fatal("plasma not deterministic")
	}
	if bytes.Equal(a, c) {
		t.Fatal("plasma ignores its generator")
	}
	lo, hi := a[0], a[0]
	for _, v := range a {
		lo, hi = min(lo, v), max(hi, v)
	}
	if lo != 0 || hi != 255 {
		t.Fatalf("plasma range [%d, %d], want [0, 255]", lo, hi)
	}
	if s := e.plasma(64, 64, 1, nil, rng.FromState(42), Dims{64, 64}); len(s) != 2*64*64 {
		t.Fatalf("slope plasma has %d bytes", len(s))
	}
}

// gradientFrame builds an RGB (or replicated-gray) test frame.
func gradientFrame(w, h, planes, channels int) *Frame {
	f := &Frame{W: w, H: h, Planes: planes, Channels: channels}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b := uint8(x*255/(w-1)), uint8(y*255/(h-1)), uint8((x+y)*7)
			px := []byte{r, g, b}
			if channels == 1 {
				px = []byte{r}
			}
			f.Pix = append(f.Pix, px...)
			if planes > channels {
				f.Pix = append(f.Pix, 77)
			}
		}
	}
	return f
}

func TestRenderDeterministic(t *testing.T) {
	t.Parallel()
	f := loadRender(t)
	src := gradientFrame(48, 32, 3, 3)
	a := NewEngine().Render(f, src, nil)
	b := NewEngine().Render(f, src, nil)
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Fatal("render not deterministic")
	}
	distinct := map[[3]byte]bool{}
	for i := 0; i < len(a.Pix); i += 3 {
		distinct[[3]byte(a.Pix[i:i+3])] = true
	}
	if len(distinct) < 16 {
		t.Fatalf("render has only %d distinct colours", len(distinct))
	}
}

// In a one-channel mode the leaves read gray replicated to R, G, B, and
// an image root's channel 0 is written. So a grayscale render equals the
// red plane of the RGB render of the same gray image replicated.
func TestRenderGrayscale(t *testing.T) {
	t.Parallel()
	f := loadRender(t)
	gray := gradientFrame(48, 32, 1, 1)
	rgb := &Frame{W: 48, H: 32, Planes: 3, Channels: 3}
	for _, v := range gray.Pix {
		rgb.Pix = append(rgb.Pix, v, v, v)
	}
	g := NewEngine().Render(f, gray, nil)
	c := NewEngine().Render(f, rgb, nil)
	if len(g.Pix) != 48*32 {
		t.Fatalf("gray output has %d bytes", len(g.Pix))
	}
	for i, v := range g.Pix {
		if v != c.Pix[3*i] {
			t.Fatalf("pixel %d: gray %d, RGB red %d", i, v, c.Pix[3*i])
		}
	}
}

// Transparency planes of evaluated pixels become 255; colour is
// unaffected by the extra plane.
func TestRenderAlphaPlane(t *testing.T) {
	t.Parallel()
	f := loadRender(t)
	a := NewEngine().Render(f, gradientFrame(48, 32, 4, 3), nil)
	b := NewEngine().Render(f, gradientFrame(48, 32, 3, 3), nil)
	for i := 0; i < 48*32; i++ {
		if a.Pix[4*i+3] != 255 || !bytes.Equal(a.Pix[4*i:4*i+3], b.Pix[3*i:3*i+3]) {
			t.Fatalf("pixel %d: %v vs %v", i, a.Pix[4*i:4*i+4], b.Pix[3*i:3*i+3])
		}
	}
}

// A selection covering a sub-rectangle renders that rectangle as if it
// were the whole image (filterRect = bounding box), blends by m/255
// (≈ identity at 255), and leaves every other pixel untouched.
func TestRenderMask(t *testing.T) {
	t.Parallel()
	f := loadRender(t)
	src := gradientFrame(48, 32, 3, 3)
	mask := make([]byte, 48*32)
	for y := 8; y < 24; y++ {
		for x := 10; x < 40; x++ {
			mask[y*48+x] = 255
		}
	}
	mask[20*48+20] = 0 // a hole: copied, not evaluated
	got := NewEngine().Render(f, src, mask)

	crop := &Frame{W: 30, H: 16, Planes: 3, Channels: 3}
	for y := 8; y < 24; y++ {
		crop.Pix = append(crop.Pix, src.Pix[(y*48+10)*3:(y*48+40)*3]...)
	}
	want := NewEngine().Render(f, crop, nil)

	for y := 0; y < 32; y++ {
		for x := 0; x < 48; x++ {
			g := got.at(x, y)
			inside := x >= 10 && x < 40 && y >= 8 && y < 24 && mask[y*48+x] != 0
			if !inside {
				if !bytes.Equal(g, src.at(x, y)) {
					t.Fatalf("(%d,%d) outside the selection changed", x, y)
				}
				continue
			}
			w := want.at(x-10, y-8)
			for c := 0; c < 3; c++ {
				if d := int(g[c]) - int(w[c]); d < -1 || d > 1 {
					t.Fatalf("(%d,%d) ch%d = %d, cropped render %d", x, y, c, g[c], w[c])
				}
			}
		}
	}
}

// Partial selection blends toward the source.
func TestRenderMaskBlend(t *testing.T) {
	t.Parallel()
	f := loadRender(t)
	src := gradientFrame(16, 16, 1, 1)
	full := make([]byte, 256)
	half := make([]byte, 256)
	for i := range full {
		full[i], half[i] = 255, 128
	}
	a := NewEngine().Render(f, src, full)
	b := NewEngine().Render(f, src, half)
	checked := 0
	for i := range b.Pix {
		if a.Pix[i] == 0 || a.Pix[i] == 255 {
			continue // clamped: the blend sees the unclamped value
		}
		checked++
		want := float64(a.Pix[i])*128/255 + float64(src.Pix[i])*127/255
		if d := float64(b.Pix[i]) - want; d < -1.5 || d > 1.5 {
			t.Fatalf("pixel %d: half-selected %d, expected ≈ %.1f", i, b.Pix[i], want)
		}
	}
	if checked < 20 {
		t.Fatalf("only %d unclamped pixels to check", checked)
	}
}

// CMYK mode (4 channels): the leaves read the first three planes as
// R, G, B, and every colour channel receives the value's first
// component, so all four planes come out equal.
func TestRenderCMYK(t *testing.T) {
	t.Parallel()
	f := loadRender(t)
	src := &Frame{W: 48, H: 32, Planes: 4, Channels: 4}
	rgb := gradientFrame(48, 32, 3, 3)
	for i := 0; i < 48*32; i++ {
		src.Pix = append(src.Pix, rgb.Pix[3*i:3*i+3]...)
		src.Pix = append(src.Pix, 200) // K plane: not read by the leaves
	}
	got := NewEngine().Render(f, src, nil)
	ref := NewEngine().Render(f, rgb, nil)
	for i := 0; i < 48*32; i++ {
		p := got.Pix[4*i : 4*i+4]
		if p[0] != p[1] || p[0] != p[2] || p[0] != p[3] {
			t.Fatalf("pixel %d: CMYK planes differ: %v", i, p)
		}
		if p[0] != ref.Pix[3*i] { // same leaves → same tree values → channel 0
			t.Fatalf("pixel %d: CMYK %d, RGB red %d", i, p[0], ref.Pix[3*i])
		}
	}
}

// Photoshop hands CMYK planes inverted (255 = no ink); image.CMYK holds
// ink. The conversion must round-trip.
func TestFrameCMYKRoundTrip(t *testing.T) {
	img := image.NewCMYK(image.Rect(0, 0, 2, 1))
	img.SetCMYK(0, 0, color.CMYK{C: 10, M: 20, Y: 30, K: 40})
	img.SetCMYK(1, 0, color.CMYK{C: 250, M: 0, Y: 128, K: 255})
	f := FrameFromImage(img)
	if f.Channels != 4 || f.Planes != 4 || f.Pix[0] != 245 || f.Pix[3] != 215 {
		t.Fatalf("CMYK frame = %+v", f)
	}
	back := f.Image().(*image.CMYK)
	if !bytes.Equal(back.Pix, img.Pix) {
		t.Fatalf("round trip %v != %v", back.Pix, img.Pix)
	}
}

// FUN_10001890: rows grow one at a time until rows·(2·planes+1)·width
// reaches maxSpace (the crossing row is included) or the height is hit.
func TestStripRows(t *testing.T) {
	cases := []struct {
		w, h, planes int
		maxSpace     int64
		want         int
	}{
		{40, 30, 3, 0, 30},          // unlimited
		{40, 30, 3, 1 << 30, 30},    // plenty
		{40, 30, 3, 280, 1},         // one row costs 7·40 = 280 ≥ maxSpace
		{40, 30, 3, 281, 2},         // two rows (560) cross 281
		{40, 30, 3, 560, 2},         // exactly two rows
		{40, 30, 3, 561, 3},         // the crossing row is included
		{40, 30, 1, 120 * 5, 5},     // one plane: 3·40 = 120 per row
		{40, 30, 3, 280*30 - 1, 30}, // capped at the height
	}
	for _, c := range cases {
		if got := StripRows(c.w, c.h, c.planes, c.maxSpace); got != c.want {
			t.Errorf("StripRows(%d, %d, %d, %d) = %d, want %d", c.w, c.h, c.planes, c.maxSpace, got, c.want)
		}
	}
}

// With strips, each strip is rendered as an image of its own: the result
// equals rendering every strip separately as a cropped source, with the
// dither generator and context carried over (no dither in render.gdo).
func TestRenderStripsAsCrops(t *testing.T) {
	t.Parallel()
	f := loadRender(t)
	src := gradientFrame(48, 32, 3, 3)
	rows := StripRows(48, 32, 3, 7*48*10) // 10 rows per strip: 10, 10, 10, 2
	if rows != 10 {
		t.Fatalf("rows = %d", rows)
	}
	got := NewEngine().RenderStrips(f, src, nil, 7*48*10)
	for top := 0; top < 32; top += rows {
		h := min(rows, 32-top)
		crop := &Frame{W: 48, H: h, Planes: 3, Channels: 3, Pix: src.Pix[top*48*3 : (top+h)*48*3]}
		want := NewEngine().Render(f, crop, nil)
		if !bytes.Equal(got.Pix[top*48*3:(top+h)*48*3], want.Pix) {
			t.Fatalf("strip at row %d differs from rendering it as its own image", top)
		}
	}
	if bytes.Equal(got.Pix, NewEngine().Render(f, src, nil).Pix) {
		t.Fatal("strip rendering should differ from a single strip")
	}
}
