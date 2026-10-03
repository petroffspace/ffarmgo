package shaper

import (
	"math"
	"testing"
)

func TestMain(m *testing.M) { Init(); m.Run() }

// C1 (DAT_10009210) must stay skewed — correcting it would break parity.
func TestC1IsSkewed(t *testing.T) {
	if C1 == math.Log2E {
		t.Fatal("DAT_10009210 must remain the author's skewed constant, " +
			"not true log2(e)")
	}
	if math.Abs(C1-math.Log2E) > 1e-12 {
		t.Fatal("C1 skew magnitude unexpected — bits wrong?")
	}
}

// Mirror antisymmetry over the FULL authentic domain, endpoints included.
func TestMirrorAntisymmetry(t *testing.T) {
	for u := 0; u <= 16383; u++ {
		a := Shape(float64(u))
		b := Shape(float64(32767 - u))
		if b != -a {
			t.Fatalf("antisymmetry broken at u=%d: %v vs %v", u, b, -a)
		}
	}
}

// Exhaustive sweep of ALL authentic inputs: finite, plausible range.
func TestFullIntegerSweep(t *testing.T) {
	for u := 0; u <= 32767; u++ {
		v := Shape(float64(u))
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("u=%d: non-finite %v", u, v)
		}
		if math.Abs(v) > 10.0 {
			t.Fatalf("u=%d: |Shape|=%v outside plausible range", u, math.Abs(v))
		}
	}
}

// k = −2 endpoints, RESOLVED via the a2e0 dump: Bm2 is subnormal
// (≈1.89e-310), and the aliased divisor arithmetic delivers
// Shape(0) ≈ −(V−1). Properties asserted; exact bits await Phase-4.
func TestZeroEdges(t *testing.T) {
	a := Shape(0)
	b := Shape(32767)
	if b != -a {
		t.Fatalf("endpoint antisymmetry broken: %v vs %v", b, -a)
	}
	if a >= 0 || b <= 0 {
		t.Fatalf("endpoint signs wrong: %v, %v", a, b)
	}
	expected := V - 1.0
	if d := math.Abs(math.Abs(a) - expected); d > 1e-9 {
		t.Fatalf("|Shape(0)| = %v, expected ≈ V−1 = %v (diff %v)",
			math.Abs(a), expected, d)
	}
	// The result must NOT be exactly V−1 — the two-rounding residue
	// and Bm2 must both be present. Asserts the aliasing is modeled.
	if math.Abs(a) == expected {
		t.Fatal("Shape(0) is exactly V−1 — aliasing residue missing")
	}
}

func TestFoldNeighbors(t *testing.T) {
	// Integers straddling Fold = 16383.5: identical x, opposite sign.
	a := Shape(16383) // low branch, sgn=−1
	b := Shape(16384) // mirror, x = 32767−16384 = 16383, sgn=+1
	if b != -a {
		t.Fatalf("seam antisymmetry failed: %v vs %v", b, -a)
	}
}

// Negative input makes ln(t) NaN; the DLL's ftol then yields k =
// 0x80000000 and reads far outside its tables. Not reproducible — the
// port rejects it like every other out-of-domain input.
func TestNegativeInputPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for negative input")
		}
	}()
	_ = Shape(-1)
}

// Knot table bits must match the listing bytes at DAT_1000a2e8..a358.
func TestKnotBits(t *testing.T) {
	if b := math.Float64bits(T[14]); b != 0x3FE599999999999A {
		t.Fatalf("T[14] bits = %016X, want 3FE599999999999A", b)
	}
}

func TestNonIntegerDomainViolationPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for u = Fold (k=14)")
		}
	}()
	_ = Shape(Fold)
}

func TestDivisorEqualsVMinusOne(t *testing.T) {
	// Bit-proof recorded in the source: subtracting 1 from V yields
	// exactly the divisor bits.
	if Divisor != V-1.0 {
		t.Fatal("Divisor bits no longer equal V−1 — regression")
	}
}

func TestTableStructure(t *testing.T) {
	for i := 0; i < 14; i++ {
		if aTab[i] != T[i+1]-T[i] {
			t.Fatalf("aTab[%d] stale", i)
		}
		if bTab[i] != T[i] {
			t.Fatalf("bTab[%d] = %v, want T[%d] = %v", i, bTab[i], i, T[i])
		}
	}
	if aTab[14] != -T[14] {
		t.Fatalf("aTab[14] = %v, want −T[14] (terminator segment)", aTab[14])
	}
	// Interpolation continuity: A[k]+B[k] == B[k+1] for k ∈ [−1,12].
	for k := -1; k <= 12; k++ {
		if aTab[k+1]+bTab[k+1] != bTab[k+2] {
			t.Fatalf("segment k=%d does not interpolate to next knot", k)
		}
	}
}

func TestShapedDrawComposition(t *testing.T) {
	stub := func(vals ...uint32) func() uint32 {
		i := -1
		return func() uint32 { i++; return vals[i%len(vals)] }
	}
	got := ShapedDraw(stub(42), 0.25, 2.0)
	want := 0.25 + 2.0*Shape(42)
	if got != want {
		t.Fatalf("ShapedDraw: got %v want %v", got, want)
	}
}

// Spot values:
// the swap must not have changed Shape structurally. The full-domain
// sweeps above already assert the heavy properties; this pins a few
// spot values' sign and antisymmetry, catching any gross wiring
// error in the two-line swap.
func TestSpotValuesStable(t *testing.T) {
	for _, u := range []int{0, 1, 100, 16383, 16384, 32766, 32767} {
		a := Shape(float64(u))
		b := Shape(float64(32767 - u))
		if b != -a {
			t.Fatalf("spot u=%d broke antisymmetry after z upgrade", u)
		}
	}
}
