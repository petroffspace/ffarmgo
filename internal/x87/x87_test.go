package x87

import (
	"math"
	"testing"
)

var lnVTest = math.Log(1.31486)

const dblEps = 2.220446049250313e-16

func TestFrndintIntegralPassthrough(t *testing.T) {
	for _, x := range []float64{0, 1, -1, 42, -42, 1e15, -1e15} {
		if Frndint(x) != x {
			t.Fatalf("Frndint(%v) = %v, want passthrough", x, Frndint(x))
		}
	}
}

func TestFrndintNearest(t *testing.T) {
	cases := map[float64]float64{0.4: 0, 0.6: 1, 1.4: 1, 1.6: 2,
		-0.4: 0, -0.6: -1, -1.4: -1, -1.6: -2}
	for x, want := range cases {
		if Frndint(x) != want {
			t.Fatalf("Frndint(%v) = %v, want %v", x, Frndint(x), want)
		}
	}
}

func TestFrndintTiesToEven(t *testing.T) {
	cases := map[float64]float64{0.5: 0, 1.5: 2, 2.5: 2, 3.5: 4, 4.5: 4,
		-0.5: 0, -1.5: -2, -2.5: -2, -3.5: -4}
	for x, want := range cases {
		if Frndint(x) != want {
			t.Fatalf("Frndint(%v) = %v, want %v (tie to even)", x, Frndint(x), want)
		}
	}
}

func TestFtolTruncatesTowardZero(t *testing.T) {
	cases := map[float64]int32{1.9: 1, -1.9: -1, 0.99: 0, -0.99: 0,
		2147483647.0: 2147483647, 255.999: 255}
	for x, want := range cases {
		if got := Ftol(x); got != want {
			t.Fatalf("Ftol(%v) = %d, want %d", x, got, want)
		}
	}
}

// _ftol returns EAX of a 64-bit FISTP: indefinite (0x8000000000000000)
// has a zero low half, and large values wrap modulo 2^32.
func TestFtolLowHalfOf64Bit(t *testing.T) {
	cases := map[float64]int32{
		math.NaN(): 0, math.Inf(1): 0, math.Inf(-1): 0, 1e300: 0, -1e300: 0,
		2147483648.0: -2147483648, 4294967301.0: 5, -2147483649.0: 2147483647,
		-9223372036854775808.0: 0,
	}
	for x, want := range cases {
		if got := Ftol(x); got != want {
			t.Errorf("Ftol(%v) = %d, want %d", x, got, want)
		}
	}
}

func TestClamp(t *testing.T) {
	if Clamp(-1, 255) != 0 {
		t.Fatal("negative must clamp to 0")
	}
	if Clamp(255, 255) != 255 {
		t.Fatal("max must be inclusive")
	}
	if Clamp(256, 255) != 255 {
		t.Fatal("over-max must clamp")
	}
}

func TestEncode(t *testing.T) {
	cases := map[float64]byte{0: 0, 0.5: 128, 1.0: 255, -0.5: 0, 0.00390625: 1, 0.999: 255}
	for x, want := range cases {
		if got := Encode(x); got != want {
			t.Fatalf("Encode(%v) = %d, want %d", x, got, want)
		}
	}
}

// FYL2X and F2XM1 must be correctly rounded to 64 bits; reference values
// computed with 60-digit decimal arithmetic.
func TestExtendedTranscendentals(t *testing.T) {
	for _, c := range []struct {
		x      float64
		hi, lo float64
	}{
		{1.5, 0.4054651081081643848591511, -2.87313575708658675011975e-18},
		{37.5, 3.624340932976365170503641, -3.924811864397526051106979e-17},
	} {
		if r := FYL2X(c.x, LN2_80); r.hi != c.hi || r.lo != c.lo {
			t.Errorf("FYL2X(%v) = %v + %v, want %v + %v", c.x, r.hi, r.lo, c.hi, c.lo)
		}
	}
	if r := F2XM1(0.25); r.hi != 0.1892071150027210546529233 || r.lo != 1.206174916890123682833291e-17 {
		t.Errorf("F2XM1(0.25) = %v + %v", r.hi, r.lo)
	}
}

// Pow must be correctly rounded where math.Pow is not.
func TestPowCorrectlyRounded(t *testing.T) {
	cases := [][3]float64{ // x, y, correctly rounded x^y (60-digit decimal)
		{0.9, 0.3, 0.96888616119726334},
		{2, 1.7, 3.2490095854249419},
		{0.123, 5.5, 9.8736587434161523e-06},
		{1.0000001, 12345, 1.0012352622477032},
	}
	for _, c := range cases {
		if got := Pow(c[0], c[1]); got != c[2] {
			t.Errorf("Pow(%v, %v) = %.17g, want %.17g", c[0], c[1], got, c[2])
		}
	}
	if !math.IsNaN(Pow(-2, 0.5)) || Pow(-2, 3) != -8 || Pow(0, 2) != 0 || Pow(5, 0) != 1 {
		t.Error("Pow special cases")
	}
}
