package farm

import (
	"math"

	"ffarmgo/internal/rng"
	"ffarmgo/internal/x87"
)

// noiseState is a noise node's private block (0x70 bytes): the 31-bit
// seed drawn at construction (+0x68). The cached cell and corners
// (+0, +8) are a pure function of (cell, seed, mode), so they are
// recomputed rather than cached.
type noiseState struct {
	seed  int32
	deriv bool // e8e8: track derivatives, output a point (dopdnoise)
	bipol bool // e8e4: bipolar corner values (dopbnoise)
}

// noiseCtor mirrors LAB_10008370 / LAB_100087f0 / LAB_10008860 and
// FUN_100083b0: one u32 draw for the seed; the node depends on (u, v).
func noiseCtor(name string, deriv, bipol bool) func(*Engine, *growCtx, int, []*Node) *Node {
	return func(e *Engine, g *growCtx, typ int, kids []*Node) *Node {
		n := e.build(typ, nodeArgs(kids), evalNoise, name, g.dims)
		n.priv = &noiseState{seed: int32(g.rng.NextUint31()), deriv: deriv, bipol: bipol}
		n.Flags = DepCoord
		return n
	}
}

// evalNoise mirrors FUN_10008410: sample at (u/s, v/s), s = child value.
func evalNoise(e *Engine, n *Node, c *Ctx) {
	st := n.priv.(*noiseState)
	s := n.Args[0].D[0]
	r := e.noise([2]float64{c.U / s, c.V / s}, st)
	if st.deriv {
		n.Val.D[0], n.Val.D[1], n.Val.Flag = r[0], r[1], 0
	} else {
		n.Val.D[0] = r[2]
	}
}

// noise mirrors FUN_100084d0: Hermite interpolation of four lattice
// corners, each carrying (gradient x, gradient y, value).
func (e *Engine) noise(pos [2]float64, st *noiseState) [3]float64 {
	var cell [2]int32
	for i := range pos {
		cell[i] = x87.Ftol(math.Floor(pos[i]))
		pos[i] = pos[i] - float64(cell[i])
	}
	var c [4][3]float64 // corner order (0,0), (0,1), (1,0), (1,1)
	for dx := int32(0); dx < 2; dx++ {
		for dy := int32(0); dy < 2; dy++ {
			c[dx*2+dy] = e.corner([2]int32{cell[0] + dx, cell[1] + dy}, st)
		}
	}
	hermite(&c[0], &c[1], 1, pos[1], st.deriv)
	hermite(&c[2], &c[3], 1, pos[1], st.deriv)
	hermite(&c[0], &c[2], 0, pos[0], st.deriv)
	return c[0]
}

// corner mirrors FUN_100086f0: hash the cell through the permutation
// table, seed a private drand48 stream with the hash, and draw the value
// and gradients.
func (e *Engine) corner(cell [2]int32, st *noiseState) [3]float64 {
	h := e.perm[st.seed&0x3ff]
	for _, k := range cell {
		h = e.perm[(h^k)&0x3ff]
	}
	r := rng.Seed48(uint32(h))
	v := r.NextDouble()
	val := v
	scale := v - math.Floor(v+0.5)
	switch {
	case st.bipol:
		val = (v + v) - 1
		scale = scale * 8 // DAT_10009200
	case !st.deriv:
		scale = scale * 4 // DAT_100091f8
	}
	var out [3]float64
	for i := 0; i < 2; i++ {
		d := r.NextDouble()
		out[i] = ((d + d) - 1) * scale
	}
	out[2] = val
	return out
}

// hermite mirrors FUN_10008610: cubic Hermite interpolation of the value
// (index 2) between a and b along axis, with slopes a[axis], b[axis];
// the result replaces a. With deriv, a[axis] becomes the interpolated
// derivative and the other gradient component is lerped; without, only
// components below axis are lerped.
func hermite(a, b *[3]float64, axis int, t float64, deriv bool) {
	v0, v1 := a[2], b[2]
	d0, d1 := a[axis], b[axis]
	A := ((d1 - v1) - v1) + (v0 + v0) + d0
	B := ((v1 - A) - d0) - v0
	a[2] = float64(float64(float64(float64(float64(A*t)+B)*t)+d0)*t) + v0
	if deriv {
		a[axis] = float64(float64(float64(float64(A*3)*t)+(B+B))*t) + d0
		for k := 0; k < 2; k++ {
			if k != axis {
				a[k] = float64((b[k]-a[k])*t) + a[k]
			}
		}
		return
	}
	for k := 0; k < axis; k++ {
		a[k] = float64((b[k]-a[k])*t) + a[k]
	}
}
