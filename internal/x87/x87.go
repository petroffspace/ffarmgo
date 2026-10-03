// Package x87 models the x87 FPU behaviour ffarm.8bf depends on.
//
// The plugin runs with the Windows default control word 0x27F: 53-bit
// precision, round to nearest. FADD/FSUB/FMUL/FDIV therefore round to
// double after every instruction and plain float64 arithmetic reproduces
// them. Integer conversion (ftol), FRNDINT and the byte encoder are
// below; the transcendentals (FYL2X, F2XM1) and MSVCRT pow, whose
// results carry extra precision into the next rounding, are modelled in
// ext.go.
package x87

import (
	"math"
)

// Frndint mirrors the FPU's FRNDINT (round to nearest, ties to EVEN).
func Frndint(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) || x == math.Trunc(x) {
		return x
	}
	f := math.Floor(x)
	c := math.Ceil(x)
	df, dc := x-f, c-x
	switch {
	case df < dc:
		return f
	case dc < df:
		return c
	default:
		if int64(f)%2 == 0 {
			return f
		}
		return c
	}
}

// Ftol mirrors MSVCRT _ftol as the plugin uses it: FISTP to a 64-bit
// integer with truncation, of which callers keep only the low 32 bits
// (EAX). NaN, infinities and |x| ≥ 2^63 give the 64-bit "integer
// indefinite" 0x8000000000000000, whose low half is 0; values beyond the
// int32 range wrap modulo 2^32.
func Ftol(x float64) int32 {
	if math.IsNaN(x) || x >= 9223372036854775808.0 || x < -9223372036854775808.0 {
		return 0
	}
	return int32(int64(x))
}

// Clamp: signed clamp to [0, max].
func Clamp(x, max int32) int32 {
	if x < 0 {
		return 0
	}
	if x > max {
		return max
	}
	return x
}

// Encode: 256× value → ftol → clamp to byte.
func Encode(x float64) byte {
	return byte(Clamp(Ftol(256.0*x), 255))
}
