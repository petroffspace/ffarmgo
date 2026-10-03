package rng

import "math/bits"

// Faithful reimplementation of the plugin's 48-bit LCG
// (FUN_10007de0 step; FUN_10007f70 / FUN_10007fd0 readouts).
//
//	X' = (0x5DEECE66D · X + 0xB) mod 2^48
//
// Blob layout at offset +12: three u16 limbs, little-endian offsets
// (limb 0 = low 16 bits). FUN_10007ec0 proved the double conversion is
// state/2^48 of the POST-advance state, constructed exactly.

const (
	mult   = 0x5DEECE66D
	inc    = 0xB
	mask48 = uint64(1)<<48 - 1
)

type Rand struct {
	state uint64
	draws int // instrumentation: total master draws consumed
}

// Restore from the blob's three little-endian u16 limbs.
func Restore(l0, l1, l2 uint16) *Rand {
	return &Rand{state: uint64(l0) | uint64(l1)<<16 | uint64(l2)<<32}
}

// Convenience: restore from the raw blob bytes (blob[12:18]).
func RestoreBytes(b []byte) *Rand {
	return Restore(
		uint16(b[0])|uint16(b[1])<<8,
		uint16(b[2])|uint16(b[3])<<8,
		uint16(b[4])|uint16(b[5])<<8,
	)
}

func (r *Rand) Draws() int { return r.draws }

// State returns the current 48-bit LCG state (checkpoint/verification).
func (r *Rand) State() uint64 { return r.state }

func (r *Rand) advance() {
	// mult·state overflows 64 bits, but every contribution of the high word
	// is a multiple of 2^64, which vanishes mod 2^48 — so the low-word path
	// alone is exact.
	_, lo := bits.Mul64(mult, r.state)
	r.state = (lo + inc) & mask48
	r.draws++
}

// NextDouble mirrors FUN_10007f70: advance, return state/2^48.
// Exact in float64 (48 bits fit the 53-bit mantissa; power-of-two divide).
func (r *Rand) NextDouble() float64 {
	r.advance()
	return float64(r.state) / 281474976710656.0
}

// NextUint31 mirrors FUN_10007fd0: advance, return top 31 bits
// ((state >> 17)), jrand48-style — always non-negative, so the
// selector's signed IDIV can never yield a negative remainder.
func (r *Rand) NextUint31() uint32 {
	r.advance()
	return uint32(r.state >> 17)
}

// FromState builds a generator positioned at a raw 48-bit state.
func FromState(s uint64) *Rand {
	return &Rand{state: s & mask48}
}

// Seed48 mirrors srand48 (FUN_10007d70) and the per-corner seeding in
// FUN_10007da0: state = seed<<16 | 0x330E (low 32 bits of seed).
func Seed48(seed uint32) *Rand {
	return &Rand{state: uint64(seed)<<16 | 0x330E}
}

// Clone copies the generator state (the plasma ctors copy the 6-byte
// growth state into their descriptor and draw from the copy).
func (r *Rand) Clone() *Rand {
	return &Rand{state: r.state}
}

// Skip advances the generator by n steps in O(log n): n steps of
// x → a·x + c compose to x → A·x + C, built by repeated squaring
// (mod 2^64 arithmetic is exact mod 2^48).
func (r *Rand) Skip(n uint64) {
	a, c := uint64(mult), uint64(inc)
	A, C := uint64(1), uint64(0)
	for k := n; k > 0; k >>= 1 {
		if k&1 != 0 {
			A, C = A*a, C*a+c
		}
		a, c = a*a, c*a+c
	}
	r.state = (A*r.state + C) & mask48
	r.draws += int(n)
}
