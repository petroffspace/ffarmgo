// Package shaper implements the spline transfer curve (FUN_10008960)
// and its draw wrapper (FUN_10008930).
//
// Ground truth (RE session 2026-09; older-chat spec sheet incorporated):
//
//	Fold = 16383.5 (DAT_10009220, bits 0x40CFFFC000000000)
//	Refl = 32767.0 (DAT_10009218, bits 0x40DFFFC000000000)
//	Authentic input domain: integer u ∈ [0, 32767] via
//	u = Next31() & 0x7FFF (FUN_10008930), FILD-exact.
//
// THE SKEWED CONSTANT (port-critical): DAT_10009210 (bits
// 0x3FF71547652B84A7 = 1.4426950408890578) is NOT log2(e), which is
// 1.4426950408889634. The original author's constant is off by ~290
// ULPs. All L values are skewed accordingly — notably
// L(0.5) = -1.0000000000000653, NOT -1. Do not "fix" this: bit-parity
// requires THEIR bits.
//
// Consequence: at u = 0 and u = 32767, floor(L) = -2 (not -1). The DLL
// then reads A[-2] at e8f8 — which ALIASES the divisor slot — and B[-2]
// at a2e0 (8 bytes below the knot table; dump pending). The k = -2 path
// computes ((w-1)/divisor)·divisor + B[-2] with two separate roundings;
// the near-cancellation is NOT simplified algebraically.
//
//	FUN_10008960 Shape(u):
//	  x = u (sgn=-1) if u <= 16383.5 else 32767-u (sgn=+1)
//	  t = x + 0.5
//	  L = ln(t) · C1   [FYL2X 80-bit interior, FMUL C1, FSTP-to-double]
//	  k = ftol(floor(L))            — k ∈ [-2, 13]
//	  f = L − k
//	  z = (f · lnV) · log2e_true    [FLDL2E hardware constant — TRUE
//	                                 log2e, unlike C1!]
//	  w = 2^z                        [FRNDINT/F2XM1/FSCALE chain]
//	  val = ((w−1)/divisor)·A[k] + B[k]
//	  result = sgn · val
//
// Tables (address-space layout preserved):
//
//	A[k] @ e908+8k: k=-2 -> e8f8 (aliases the divisor); k >= -1 ->
//	     T[k+2] − T[k+1] for k ∈ [-1,13] (15 entries, e900..e970)
//	B[k] @ a2f0+8k: T[k+1] for k ∈ [-1,13]; B[-2] = qword @ a2e0
package shaper

import (
	"fmt"
	"math"
	"sync"

	"ffarmgo/internal/x87"
)

var (
	V    = math.Float64frombits(0x3FF5099546D7328A) // DAT_10009208 ≈ 1.31486
	Refl = math.Float64frombits(0x40DFFFC000000000) // DAT_10009218 = 32767.0
	Fold = math.Float64frombits(0x40CFFFC000000000) // DAT_10009220 = 16383.5

	// C1 is the ORIGINAL AUTHOR'S skewed "log2e" (DAT_10009210).
	// 1.4426950408890578 — off by ~290 ULPs from true log2(e).
	C1 = math.Float64frombits(0x3FF71547652B84A7)

	Half = math.Float64frombits(0x3FE0000000000000) // DAT_100091b8 = 0.5

	// Divisor (DAT_1000e8f8 = 0x3FD426551B5CCA28, per the verified
	// spec sheet). BIT-PROVEN equal to V−1: subtracting 1 from V
	// (bits 0x3FF5099546D7328A) drops the exponent by 2 and shifts the
	// mantissa left 2 — 0x5099546D7328A<<2 = 0x426551B5CCA28. Stored
	// verbatim for provenance; the equivalence is now closed, not
	// inferred.
	Divisor = math.Float64frombits(0x3FD426551B5CCA28)
)

// Knot table T[0..14] at DAT_1000a2e8–a358. Consult the BITS, not the
// decimal comments.
var T = [15]float64{
	math.Float64frombits(0x4011000000000000), // T[0]  = 4.25
	math.Float64frombits(0x4010333333333333), // T[1]  = 4.05
	math.Float64frombits(0x400EE147AE147AE1),
	math.Float64frombits(0x400D5C28F5C28F5C),
	math.Float64frombits(0x400BE5604189374C),
	math.Float64frombits(0x400A604189374BC7),
	math.Float64frombits(0x4008C6A7EF9DB22D),
	math.Float64frombits(0x400716872B020C4A),
	math.Float64frombits(0x400547AE147AE148), // T[8]  = 2.6625
	math.Float64frombits(0x40035810624DD2F2),
	math.Float64frombits(0x40013B645A1CAC08),
	math.Float64frombits(0x3FFDCED916872B02),
	math.Float64frombits(0x3FF88B4395810625),
	math.Float64frombits(0x3FF2666666666666), // T[13] = 1.15
	math.Float64frombits(0x3FE599999999999A), // T[14] = 0.675 (listing bytes @a358; was mis-copied as ...9999)
}

// T15: the zero terminator at a360 participates as knot 15:
// A[13] = T[15] − T[14] = −0.675.
const T15 = 0.0

// aTab[i] holds A[k] for k = i−1, i ∈ [0,14] (k ∈ [−1,13]).
// bTab[i] holds B[k] = T[k+1] for the same range.
var aTab, bTab [15]float64

var lnV float64

// init fills the tables at package load, as FUN_100088d0 runs at plugin
// load; Init is kept for callers that want to be explicit.
func init() { Init() }

// Init computes the derived tables (FUN_100088d0). Idempotent.
func Init() {
	for i := 0; i < 15; i++ { // bTab[i] = T[i] = B[i-1]
		bTab[i] = T[i]
	}
	// aTab[i] = A[i-1] = T[i+1] − T[i], i ∈ [0,14], T[15] = terminator
	for i := 0; i < 14; i++ {
		aTab[i] = T[i+1] - T[i]
	}
	aTab[14] = T15 - T[14] // A[13] = −0.675

	lnV = x87.FYL2X(V, x87.LN2_80).Round53() // DAT_1000e8f0: FLDLN2; FLD V; FYL2X; FSTP double (088d0)
}

// Bm2 is B[-2]: the qword at DAT_1000a2e0 (8 bytes below the knot
// table), read by the DLL at u = 0 and u = 32767. Bits
// 0x0000037A11D7007B — a SUBNORMAL double ≈ 1.89e-310 (leftover data,
// effectively zero, but reproduced verbatim: at k=−2 the DLL computes
// ((w−1)/D)·D + Bm2, and w ≈ V there, so Shape(0) ≈ −(V−1) ≈ −0.31486
// — the spline's full-scale limit, reached via divisor aliasing).
var Bm2 = math.Float64frombits(0x0000037A11D7007B)

// Shape mirrors FUN_10008960 gate-for-gate.
func Shape(u float64) float64 {
	var x, sgn float64
	if !(u > Fold) { // u < fold, u == fold, or NaN — TEST AH,0x41
		x, sgn = u, -1.0
	} else {
		x, sgn = Refl-u, 1.0 // FSUBR DAT_10009218
	}
	t := x + Half
	// FLDLN2; FXCH; FYL2X leaves ln t at 64-bit precision; FMUL by the
	// skewed C1 rounds it to 53 bits (precision control 0x27F).
	L := x87.Mul53(x87.FYL2X(t, x87.LN2_80), x87.FromFloat(C1))
	// Authentic integer u keeps k in [-2,13]. Anything else (u = Fold
	// itself gives L = 14; negative u gives NaN, which ftol turns into
	// 0x80000000) makes the DLL index far outside its tables — not
	// reproducible, so it is rejected.
	if math.IsNaN(L) || L < -2 || L >= 14 {
		panic(fmt.Sprintf("shaper: input %v outside the authentic domain (L=%v)", u, L))
	}
	k := int(math.Floor(L))
	f := L - float64(k)
	// z = (f·lnV)·log2e (FMUL e8f0; FLDL2E; FMULP): two 53-bit roundings,
	// the second with the 64-bit FLDL2E constant. Then FRNDINT, F2XM1
	// (64-bit result), FLD1/FADDP (rounded to 53), FSCALE (exact).
	z := x87.Mul53(x87.FromFloat(f*lnV), x87.L2E_80)
	n := x87.Frndint(z)
	w := math.Ldexp(x87.Add53(x87.F2XM1(z-n), x87.FromFloat(1)), int(n))
	var val float64
	if k == -2 {
		// u = 0 or u = 32767. A[-2] @ e8f8 ALIASES the divisor slot:
		// FDIV and FMUL read the SAME qword — two separate roundings,
		// reproduced literally. Do not cancel algebraically.
		val = ((w-1.0)/Divisor)*Divisor + Bm2
	} else {
		val = ((w-1.0)/Divisor)*aTab[k+1] + bTab[k+1]
	}
	return sgn * val
}

// ShapedDraw mirrors FUN_10008930: u = Next31() & 0x7FFF (FILD-exact),
// result = offset + scale·Shape(u).
func ShapedDraw(next31 func() uint32, offset, scale float64) float64 {
	u := next31() & 0x7FFF
	tableOnce.Do(fillTable)
	return float64(scale*table[u]) + offset // FMUL scale, FADD offset; no fused multiply-add
}

// Shape depends only on the 15-bit draw, and the extended-precision
// transcendentals are slow, so ShapedDraw reads a precomputed table.
var (
	tableOnce sync.Once
	table     [32768]float64
)

func fillTable() {
	for u := range table {
		table[u] = Shape(float64(u))
	}
}
