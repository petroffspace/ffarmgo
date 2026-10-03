package farm

import (
	"testing"

	"ffarmgo/internal/gdo"
	"ffarmgo/internal/rng"
)

// list builds a nil-terminated pool, as FUN_100063b0 does.
func list(entries ...[2]uint16) *gdo.ParamList {
	pl := &gdo.ParamList{}
	link := &pl.Head
	for _, e := range entries {
		n := &gdo.Node{ID: e[0], RawWeight: e[1], Weight: float32(e[1]) / 65536.0}
		*link = n
		link = &n.Next
	}
	return pl
}

// synthFile returns a file whose root pool is root and whose every
// object slot holds a single zero-arity "mu" entry (op 0).
func synthFile(root *gdo.ParamList) *gdo.File {
	f := &gdo.File{TagA: 1000, TagB: TScalar, FirstList: root, Objects: make([]gdo.Object, len(ops))}
	for i := range f.Objects {
		for s := range f.Objects[i].Slots {
			f.Objects[i].Slots[s] = list([2]uint16{0, 0xFFFF})
		}
	}
	return f
}

const testSeed = uint64(0xE88C5F62A74A)

// When the weights never reach the draw, FUN_100055e0 picks
// NextUint31() % count, where count is exactly the pool length, and
// does not log the choice.
func TestSelectorFallbackUsesPoolLength(t *testing.T) {
	f := synthFile(list([2]uint16{3, 0}, [2]uint16{4, 0}, [2]uint16{5, 0}))
	r := rng.FromState(testSeed)
	got, logged := selectEntry(f, r, 0, 0, -1)

	ref := rng.FromState(testSeed)
	ref.NextDouble()
	want := []uint16{3, 4, 5}[int(ref.NextUint31())%3]
	if got.ID != want || logged {
		t.Fatalf("fallback picked op %d (logged %v), want %d (not logged)", got.ID, logged, want)
	}
	if r.Draws() != 2 {
		t.Fatalf("fallback consumed %d draws, want 2 (double + u32)", r.Draws())
	}
}

// minpe is unary (registrar 07720 passes spec bytes eff8,0,0), so a
// non-leaf minpe grows exactly one child.
func TestMinpeIsUnary(t *testing.T) {
	f := synthFile(list([2]uint16{2, 0xFFFF}))
	e := withSource(gradient{8, 8})
	tr := e.Grow(f, Dims{8, 8})
	if tr.Root.Name != "minpe" || len(tr.Root.Kids) != 1 {
		t.Fatalf("root %q with %d children, want minpe with 1", tr.Root.Name, len(tr.Root.Kids))
	}
}
