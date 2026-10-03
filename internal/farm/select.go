package farm

import (
	"fmt"

	"ffarmgo/internal/gdo"
	"ffarmgo/internal/rng"
)

// selectEntry mirrors FUN_100055e0: pick an entry from the pool for
// (parent, pos, depth), or from the root list when depth < 0, consuming
// one double, plus one u32 when the roulette walk is exhausted. It also
// reports whether the roulette walk made the choice: only those choices
// go into the record's choice log (+0x40), which the Farm dialog's
// learning step reads.
func selectEntry(file *gdo.File, r *rng.Rand, parent, pos, depth int) (*gdo.Node, bool) {
	var pool *gdo.ParamList
	if depth < 0 {
		pool = file.FirstList // record +0x3c: the root list
	} else {
		if parent < 0 || parent >= len(file.Objects) {
			panic(fmt.Sprintf("selector: invalid parent %d (file has %d)", parent, len(file.Objects)))
		}
		if pos < 0 || pos > 2 || depth > 3 {
			panic(fmt.Sprintf("selector: invalid pos %d, depth %d", pos, depth))
		}
		// The original indexes record +0x38 at depth + 4·(3·parent + pos).
		pool = file.Objects[parent].Slots[pos*4+depth]
	}

	// The draw is stored as float32 (FSTP float at 0x10005625).
	draw := float32(r.NextDouble())

	// Roulette walk: the accumulator stays in an FPU register (FLD m32
	// 0.0, then FADD m32 per node) under 53-bit precision control, which
	// float64 addition reproduces; it is compared with the float32 draw.
	acc := 0.0
	count := 0
	for n := pool.Head; n != nil; n = n.Next {
		acc += float64(n.Weight)
		if acc >= float64(draw) {
			return n, true
		}
		count++
	}

	// Exhausted: uniform over all walked entries, zero weights included
	// (FUN_10007fd0, then CDQ/IDIV by the pool length at 0x1000565a). An
	// empty pool would divide by zero in the original.
	if count == 0 {
		panic("selector: empty pool (invalid .gdo or wrong index)")
	}
	k := int(r.NextUint31()) % count
	n := pool.Head
	for ; k > 0; k-- {
		n = n.Next
	}
	return n, false
}
