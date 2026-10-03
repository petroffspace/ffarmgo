package rng

import (
	"math"
	"testing"
)

// Reference states from seed 0xE88C5F62A74A (render.gdo blob limbs),
// generated independently (Python: x_{n+1} = (0x5DEECE66D·x_n + 0xB) mod 2^48).
// Index 0 is the restored state; 1..5 are the first five transitions.
var refStates = []uint64{
	0xE88C5F62A74A, // restored state (seed from render.gdo)
	0xE1EDA385B68D, // draw 1
	0xF76C51ED6814, // draw 2
	0x1A3B7807488F, // draw 3
	0xFB968C1E5EEE, // draw 4
	0xBDF4D9A03F61, // draw 5
}

func TestRestoreMatchesBlobLimbs(t *testing.T) {
	l := Restore(uint16(refStates[0]), uint16(refStates[0]>>16), uint16(refStates[0]>>32))
	if l.State() != refStates[0] {
		t.Fatalf("Restore limb order broken: got %012X want %012X", l.State(), refStates[0])
	}
}

func TestReferenceStateSequence(t *testing.T) {
	l := Restore(uint16(refStates[0]), uint16(refStates[0]>>16), uint16(refStates[0]>>32))
	for i := 1; i < len(refStates); i++ {
		d := l.NextDouble()
		if l.State() != refStates[i] {
			t.Fatalf("state mismatch after draw %d: got %012X want %012X", i, l.State(), refStates[i])
		}
		if d < 0.0 || d >= 1.0 || math.IsNaN(d) {
			t.Fatalf("draw %d out of [0,1): %v", i, d)
		}
	}
}

func TestNextUint31IsTopBits(t *testing.T) {
	l := Restore(uint16(refStates[0]), uint16(refStates[0]>>16), uint16(refStates[0]>>32))
	for i := 0; i < 5; i++ {
		before := l.State()
		n := l.NextUint31()
		after := l.State()
		if uint64(n) != (after >> 17) {
			t.Fatalf("NextUint31 (%08X) != top 31 bits of state %012X", n, after)
		}
		if before>>17 == after>>17 && i == 0 {
			t.Logf("draw %d: before=%08X, after=%08X (no change in upper bits)", i, before>>17, after>>17)
		}
	}
}

func TestSkip(t *testing.T) {
	for _, n := range []uint64{0, 1, 2, 3, 7, 64, 1000, 12345} {
		a, b := FromState(DitherState), FromState(DitherState)
		for i := uint64(0); i < n; i++ {
			a.NextDouble()
		}
		b.Skip(n)
		if a.State() != b.State() || a.Draws() != b.Draws() {
			t.Fatalf("Skip(%d): %012X, want %012X", n, b.State(), a.State())
		}
	}
}
