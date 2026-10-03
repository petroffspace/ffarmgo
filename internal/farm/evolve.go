package farm

import (
	"math"

	"ffarmgo/internal/gdo"
	"ffarmgo/internal/rng"
	"ffarmgo/internal/x87"
)

// Evolution: the Farm dialog's grammar learning and cell breeding.
//
// The plugin keeps ONE global grammar in RAM (pools hanging off the
// registry entries at DAT_1000b350, root pools at DAT_1000d738/d73c). It
// is built at plugin load (FUN_10005ad0), reshaped by the user's ratings
// (FUN_10005f20), and copied into every newly created cell
// (FUN_10005730). It is never saved: a .gds holds only the cells' copies.

// opResult is each registry slot's result type (spec >> 24).
var opResult = [len(ops)]int{
	TScalar, TScalar, TScalar, TScalar, TScalar, TScalar, TScalar, TScalar, TScalar, TScalar,
	TMap, TScalar, TBipolar, TPoint, TScalar, TPoint, TScalar,
	TImage, TImage, TImage, TImage, TScalar,
}

// initialWeight is DAT_1000a168: [arity][depth], favouring leaf
// operators deep in the tree and wide ones near the root.
var initialWeight = [4][4]float32{
	{0.1, 0.3, 0.7, 0.9},
	{0.2, 0.4, 0.4, 0.2},
	{0.8, 0.6, 0.4, 0.1},
	{0.9, 0.7, 0.3, 0.0},
}

// Entry is one weighted operator in a grammar pool.
type Entry struct {
	Op int
	W  float32
}

// Pool is a weighted list kept sorted by operator slot, as the plugin's
// insert routines (FUN_10005a40, FUN_10005e00) keep their linked lists.
type Pool []Entry

// find returns the entry for op, inserting it with weight 1.0 when it
// is missing (FUN_10005a40 / FUN_10005e00).
func (p *Pool) find(op int) *Entry {
	i := 0
	for i < len(*p) && (*p)[i].Op < op {
		i++
	}
	if i == len(*p) || (*p)[i].Op != op {
		*p = append(*p, Entry{})
		copy((*p)[i+1:], (*p)[i:])
		(*p)[i] = Entry{Op: op, W: 1}
	}
	return &(*p)[i]
}

// normalize mirrors FUN_10005e80: weights /= sum, the sum accumulated in
// an FPU register (53-bit) and each quotient stored as float32. A pool
// summing to zero is left alone.
func (p Pool) normalize() {
	sum := 0.0
	for _, e := range p {
		sum += float64(e.W)
	}
	if sum == 0 || math.IsNaN(sum) {
		return
	}
	for i := range p {
		p[i].W = float32(float64(p[i].W) / sum)
	}
}

// Grammar is the global grammar: child pools per (parent op, child
// position, depth) and root pools per root type.
type Grammar struct {
	Pools [len(ops)][3][4]Pool
	Root  map[int]*Pool // TScalar → DAT_1000d738, TImage → DAT_1000d73c
}

func (g *Grammar) root(typ int) *Pool {
	if typ != TImage {
		typ = TScalar // FUN_10005e00: anything but the image type uses d738
	}
	return g.Root[typ]
}

// excluded mirrors FUN_10005ec0: the noises' scale input may not be mu,
// mv or minpe, and minpe may not feed minpe.
func excluded(parent, pos, op int) bool {
	noise := parent == 11 || parent == 12 || parent == 13
	if noise && pos == 0 && (op == 0 || op == 1 || op == 2) {
		return true
	}
	return parent == 2 && op == 2
}

// DefaultGrammar mirrors FUN_10005ad0, run once at plugin load: every
// operator whose result type fits a slot gets the table weight for its
// arity and the slot's depth (root pools use depth 0), excluded pairs
// get 0, and every pool is normalized.
func DefaultGrammar() *Grammar {
	g := &Grammar{Root: map[int]*Pool{TScalar: {}, TImage: {}}}
	for _, typ := range []int{TScalar, TImage} {
		for op := range ops {
			if opResult[op] == typ {
				if e := g.root(typ).find(op); e.W != 0 {
					e.W = initialWeight[len(ops[op].children)][0]
				}
			}
		}
	}
	g.Root[TScalar].normalize()
	g.Root[TImage].normalize()
	for parent := range ops {
		for pos, ct := range ops[parent].children {
			for op := range ops {
				if opResult[op] != ct {
					continue
				}
				for d := 0; d < 4; d++ {
					e := g.Pools[parent][pos][d].find(op)
					if excluded(parent, pos, op) {
						e.W = 0
					}
					if e.W != 0 {
						e.W = initialWeight[len(ops[op].children)][d]
					}
				}
			}
			for d := 0; d < 4; d++ {
				g.Pools[parent][pos][d].normalize()
			}
		}
	}
	return g
}

// EncodeWeight mirrors FUN_100061c0: a float weight as the .gdo halfword,
// floor(w·65536) via the mantissa of float32(w + 1); 0xFFFF from 1 up.
func EncodeWeight(w float32) uint16 {
	if float64(w) >= 1 {
		return 0xFFFF
	}
	return uint16(math.Float32bits(float32(float64(w)+1)) >> 7)
}

func toList(p Pool) *gdo.ParamList {
	l := &gdo.ParamList{}
	link := &l.Head
	for _, e := range p {
		n := &gdo.Node{ID: uint16(e.Op), Weight: e.W, RawWeight: EncodeWeight(e.W)}
		*link = n
		link = &n.Next
	}
	return l
}

// NewCell mirrors FUN_10006500 / FUN_10005730: a record of the given
// root type whose pools are a copy of the grammar, whose blob is the
// cell's dims record (seed included), and which has no tree yet.
func (g *Grammar) NewCell(typ int, d Dims, seed uint64) *gdo.File {
	f := &gdo.File{TagA: 1000, TagB: uint16(typ), FirstList: toList(*g.root(typ)),
		Objects: make([]gdo.Object, len(ops))}
	f.Context.W, f.Context.H = uint32(d.W), uint32(d.H)
	f.Context.Limbs = [3]uint16{uint16(seed), uint16(seed >> 16), uint16(seed >> 32)}
	for op := range ops {
		for pos := 0; pos < 3; pos++ {
			for depth := 0; depth < 4; depth++ {
				f.Objects[op].Slots[pos*4+depth] = toList(g.Pools[op][pos][depth])
			}
		}
	}
	return f
}

// Rating mirrors FUN_100021a0: a slider position 0..100 (50 = neutral)
// as the learning strength (100 − slider)·0.01f, stored as float32.
func Rating(slider int) float32 {
	return float32(float64(100-slider) * float64(float32(0.01)))
}

// Learn mirrors FUN_10005f20: with g = 4·(1 − 2·rating)³, every logged
// choice of a cell (most recent first) has its global pool entry mapped
// through bias(w, g) and the pool renormalized. rating > 0.5 (g < 0)
// raises the chosen weights, < 0.5 lowers them, 0.5 changes nothing.
func (g *Grammar) Learn(typ int, log []Choice, rating float32) {
	s := 1 - 2*float64(rating)
	k := float32(float64(s*s*s) * 4)
	for i := len(log) - 1; i >= 0; i-- {
		c := log[i]
		var p *Pool
		if c.Depth < 0 {
			p = g.root(typ)
		} else {
			p = &g.Pools[c.Parent][c.Pos][c.Depth]
		}
		e := p.find(c.Op)
		e.W = biasF(e.W, k)
		p.normalize()
	}
}

// biasF mirrors FUN_10006010: the bias curve on float32 operands.
func biasF(w, g float32) float32 {
	if g == 0 || g != g { // FCOMP 0 / TEST AH,0x40: equal or unordered
		return w
	}
	a := float32(math.Abs(float64(g)))
	if a < 1 {
		return float32(float64(float64(w)*float64(g)+1-float64(g)) * float64(w))
	}
	p := float32(x87.Pow(2, float64(a)))
	if g < 0 {
		return float32(1 - x87.Pow(1-float64(w), float64(p)))
	}
	return float32(x87.Pow(float64(w), float64(p)))
}

// Farm is the dialog's state: the global grammar, the session's cells,
// and each cell's live dims record (size plus a generator that advances
// with every growth) and choice log.
type Farm struct {
	Grammar *Grammar
	Session *gdo.Session
	Type    int // root type of new cells: DAT_1000a3ac, image in RGB mode
	Dims    [gdo.SessionCells]Dims
	rngs    [gdo.SessionCells]*rng.Rand
	logs    [gdo.SessionCells][]Choice
	eng     *Engine
}

// Clone returns a deep copy of the grammar.
func (g *Grammar) Clone() *Grammar {
	c := &Grammar{Root: map[int]*Pool{}}
	for i := range g.Pools {
		for j := range g.Pools[i] {
			for k, p := range g.Pools[i][j] {
				c.Pools[i][j][k] = append(Pool(nil), p...)
			}
		}
	}
	for t, p := range g.Root {
		cp := append(Pool(nil), (*p)...)
		c.Root[t] = &cp
	}
	return c
}

// Clone returns an independent copy of the farm: grammar, session
// header and generators are copied. Cell records and choice logs are
// shared, since the farm only ever replaces them, never edits them.
func (f *Farm) Clone() *Farm {
	c := *f
	c.Grammar = f.Grammar.Clone()
	s := *f.Session
	c.Session = &s
	for i, r := range f.rngs {
		if r != nil {
			c.rngs[i] = r.Clone()
		}
	}
	return &c
}

// growSource is what the dialog's growth sees as the image. Evolution
// never depends on pixels (plasma children read them, but they only
// shape plasma values, never random draws), so any source will do.
type growSource struct{}

func (growSource) Size() (int32, int32)                 { return 1, 1 }
func (growSource) RGB(x, y int32) (uint8, uint8, uint8) { return 128, 128, 128 }

func newFarm(s *gdo.Session, g *Grammar, d Dims, typ int) *Farm {
	f := &Farm{Grammar: g, Session: s, Type: typ, eng: NewEngine()}
	f.eng.src = growSource{}
	for i := range f.Dims {
		f.Dims[i] = d
	}
	return f
}

// LoadFarm opens a session in the dialog: every cell is regrown from its
// saved seed (FUN_10002310 → FUN_10006540), which rebuilds the choice
// logs and leaves each cell's generator just past that growth. d is the
// dialog's preview cell size, which new cells record in their blobs.
// The grammar is the caller's: DefaultGrammar for a freshly loaded
// plugin. typ is the root type new cells get (TImage for RGB images,
// TScalar otherwise).
func LoadFarm(s *gdo.Session, g *Grammar, d Dims, typ int) *Farm {
	f := newFarm(s, g, d, typ)
	for i, c := range s.Cells {
		f.grow(i, rng.FromState(c.Context.RNG()))
	}
	return f
}

// SetCell puts a filter record into cell i and grows it from its saved
// seed, as LoadFarm does for every cell, so the cell's choice log and
// generator are what opening a session holding it would give. (A port
// extension: the dialog itself only loads whole sessions.)
func (f *Farm) SetCell(i int, c *gdo.File) {
	f.Session.Cells[i] = c
	f.grow(i, rng.FromState(c.Context.RNG()))
}

func (f *Farm) grow(i int, r *rng.Rand) {
	t := f.eng.growFrom(f.Session.Cells[i], f.Dims[i], r)
	f.rngs[i], f.logs[i] = t.Rng, t.Log
}

// Breed mirrors the dialog's Breed command (0x100037f1): every active
// cell is first rated by its slider (0..100, 50 = neutral) into the
// grammar, in cell order, then every active cell is replaced by a new
// cell grown from the updated grammar with its own advancing generator
// (FUN_100021e0 → FUN_100056c0).
func (f *Farm) Breed(sliders []int) {
	active := int(f.Session.Active)
	for i := 0; i < active; i++ {
		slider := 50
		if i < len(sliders) {
			slider = sliders[i]
		}
		f.Grammar.Learn(int(f.Session.Cells[i].TagB), f.logs[i], Rating(slider))
	}
	for i := 0; i < active; i++ {
		f.Session.Cells[i] = f.Grammar.NewCell(f.Type, f.Dims[i], f.rngs[i].State())
		f.grow(i, f.rngs[i])
	}
}

// NewSession mirrors FUN_10002a20 → FUN_10007520: nine cells of root
// type typ with fresh grammar copies and seeds (0x0165, now & 0xffff,
// index), no cell selected, an empty directory, then grown as the dialog
// does next (FUN_10002310). now is the C time() value.
func NewSession(g *Grammar, typ int, active int32, now uint32, d Dims) *Farm {
	s := &gdo.Session{Tag: 1000, Active: active, Selected: -1}
	for i := range s.Cells {
		seed := uint64(0x0165) | uint64(now&0xffff)<<16 | uint64(i)<<32
		s.Cells[i] = g.NewCell(typ, d, seed)
	}
	return LoadFarm(s, g, d, typ)
}
