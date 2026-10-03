package x87

import "math"

// Extended-precision model of the x87 transcendentals.
//
// Real x87 hardware computes FYL2X and F2XM1 to within an ulp of the
// 64-bit mantissa; the plugin then rounds those results to 53 bits in
// the next FMUL/FADD (precision control 0x27F). Modelling the
// instructions as correctly rounded to 64 bits therefore reproduces the
// hardware except when the exact result lies within ~2^-64 of a 53-bit
// rounding boundary. The arithmetic below is double-double (~106 bits),
// with exact products from math.FMA.

// dd is an unevaluated sum hi + lo with |lo| ≤ ulp(hi)/2.
type dd struct{ hi, lo float64 }

func twoSum(a, b float64) dd {
	s := a + b
	bb := s - a
	return dd{s, (a - (s - bb)) + (b - bb)}
}

func quickTwoSum(a, b float64) dd {
	s := a + b
	return dd{s, b - (s - a)}
}

func twoProd(a, b float64) dd {
	p := a * b
	return dd{p, math.FMA(a, b, -p)}
}

func (a dd) add(b dd) dd {
	s := twoSum(a.hi, b.hi)
	t := twoSum(a.lo, b.lo)
	s = quickTwoSum(s.hi, s.lo+t.hi)
	return quickTwoSum(s.hi, s.lo+t.lo)
}

func (a dd) neg() dd { return dd{-a.hi, -a.lo} }

func (a dd) mul(b dd) dd {
	p := twoProd(a.hi, b.hi)
	p.lo += a.hi*b.lo + a.lo*b.hi
	return quickTwoSum(p.hi, p.lo)
}

func (a dd) mulF(b float64) dd {
	p := twoProd(a.hi, b)
	p.lo += a.lo * b
	return quickTwoSum(p.hi, p.lo)
}

func (a dd) div(b dd) dd {
	q1 := a.hi / b.hi
	r := a.add(b.mulF(q1).neg())
	q2 := r.hi / b.hi
	r = r.add(b.mulF(q2).neg())
	q3 := r.hi / b.hi
	return quickTwoSum(q1, q2).add(dd{q3, 0})
}

// ln2DD is ln 2 to ~106 bits.
var ln2DD = dd{6.931471805599452862e-01, 2.319046813846299558e-17}

// expm1DD returns e^r − 1 for |r| ≤ 0.36 by Taylor series.
func expm1DD(r dd) dd {
	sum, term := r, r
	for k := 2; k < 40; k++ {
		term = term.mul(r).div(dd{float64(k), 0})
		sum = sum.add(term)
		if math.Abs(term.hi) < 1e-40*math.Abs(sum.hi) {
			break
		}
	}
	return sum
}

// expSlowDD returns e^x by halving and squaring. It needs no tables, so
// it builds expTable at start-up; expDD is the fast path.
func expSlowDD(x dd) dd {
	n := math.Round(x.hi / ln2DD.hi)
	r := x.add(ln2DD.mulF(-n))
	e := expm1DD(r.mulF(1.0 / 1024))
	// (1+e)^1024 via squaring: (1+e)^2 − 1 = e(2 + e)
	for i := 0; i < 10; i++ {
		e = e.mul(e.add(dd{2, 0}))
	}
	one := e.add(dd{1, 0})
	return dd{math.Ldexp(one.hi, int(n)), math.Ldexp(one.lo, int(n))}
}

// Tables for expDD: e^(k/64) for |k| ≤ expTableK, and 1/k!.
const (
	expTableK = 24 // |r| ≤ ln2/2 < 24/64
	expTerms  = 12 // |s| ≤ 1/128: (1/128)^13/13! < 2^-120
)

var (
	expTable [2*expTableK + 1]dd
	invFact  [expTerms + 1]dd
)

func init() {
	for k := -expTableK; k <= expTableK; k++ {
		expTable[k+expTableK] = expSlowDD(dd{float64(k) / 64, 0})
	}
	f := dd{1, 0}
	for k := 0; k <= expTerms; k++ {
		if k > 0 {
			f = f.mulF(float64(k)) // k! is exact in a double up to 18!
		}
		invFact[k] = dd{1, 0}.div(f)
	}
}

// expDD returns e^x: x = n·ln2 + k/64 + s with |s| ≤ 1/128, so
// e^x = 2^n · e^(k/64) · (1 + s + s²/2! + … + s^12/12!), evaluated by
// Horner's rule with precomputed reciprocal factorials (~106 bits).
func expDD(x dd) dd {
	n := math.Round(x.hi / ln2DD.hi)
	r := x.add(ln2DD.mulF(-n))
	k := math.Round(r.hi * 64)
	s := r.add(dd{-k / 64, 0})
	p := invFact[expTerms]
	for i := expTerms - 1; i >= 1; i-- {
		p = p.mul(s).add(invFact[i])
	}
	em1 := p.mul(s) // e^s − 1
	e := expTable[int(k)+expTableK]
	v := e.add(e.mul(em1))
	return dd{math.Ldexp(v.hi, int(n)), math.Ldexp(v.lo, int(n))}
}

// expFastDD returns e^x to about 2^-72 relative: expDD's reduction,
// then e^s − 1 = s + s²/2 + s³·q(s) with only s and hi(s)²/2 in
// double-double (|s³| ≤ 2^-21, so q's float64 rounding stays below 2^-74).
func expFastDD(x dd) dd {
	n := math.Round(x.hi / ln2DD.hi)
	r := x.add(ln2DD.mulF(-n))
	k := math.Round(r.hi * 64)
	s := r.add(dd{-k / 64, 0})
	h := s.hi
	q := 1.0/6 + h*(1.0/24+h*(1.0/120+h*(1.0/720+h*(1.0/5040+h*(1.0/40320+h*(1.0/362880))))))
	sq := twoProd(h, h).mulF(0.5)
	em1 := s.add(sq).add(dd{h*s.lo + h*h*h*q, 0})
	e := expTable[int(k)+expTableK]
	v := e.add(e.mul(em1))
	return dd{math.Ldexp(v.hi, int(n)), math.Ldexp(v.lo, int(n))}
}

// logDD returns ln x for finite x > 0: one Newton step
// l ← l + x·e^−l − 1 takes math.Log's ~53 correct bits past 100.
func logDD(x float64) dd {
	l := dd{math.Log(x), 0}
	e := expDD(l.neg())
	return l.add(e.mulF(x).add(dd{-1, 0}))
}

// round64 rounds a double-double to a 64-bit mantissa (x87 extended),
// nearest-even, and returns it again as hi + lo. hi already lies on the
// 64-bit grid of the value's binade, so only lo is rounded; its parity
// decides ties because hi is a multiple of 2^11 grid steps.
func round64(v dd) dd {
	if v.hi == 0 || math.IsInf(v.hi, 0) || math.IsNaN(v.hi) {
		return v
	}
	_, e := math.Frexp(v.hi) // 2^(e−1) ≤ |hi| < 2^e
	if math.Abs(v.hi) == math.Ldexp(0.5, e) && (v.lo < 0) == (v.hi > 0) && v.lo != 0 {
		e-- // the value lies just below a power of two
	}
	ulp64 := math.Ldexp(1, e-64)
	return quickTwoSum(v.hi, math.RoundToEven(v.lo/ulp64)*ulp64)
}

// Round53 rounds an extended value to the nearest double (FSTP double).
func (a dd) Round53() float64 { return a.hi + a.lo }

// Ext80 is an x87 extended value held exactly as hi + lo (64-bit
// mantissa).
type Ext80 = dd

// LN2_80 and L2E_80 are FLDLN2 and FLDL2E: ln 2 and log2 e rounded to a
// 64-bit mantissa (round-to-nearest).
var (
	LN2_80 = round64(ln2DD)
	L2E_80 = round64(dd{1, 0}.div(ln2DD))
)

// FYL2X models FYL2X: st1 · log2(st0), correctly rounded to 64 bits.
func FYL2X(st0 float64, st1 Ext80) Ext80 {
	return round64(logDD(st0).div(ln2DD).mul(st1))
}

// F2XM1 models F2XM1: 2^st0 − 1 for |st0| ≤ 1, rounded to 64 bits.
func F2XM1(st0 float64) Ext80 {
	return round64(expm1DD(dd{st0, 0}.mul(ln2DD)))
}

// Mul53 is an FMUL of two extended values under 53-bit precision control.
func Mul53(a, b Ext80) float64 { return a.mul(b).Round53() }

// Add53 is an FADD of two extended values under 53-bit precision control.
func Add53(a, b Ext80) float64 { return a.add(b).Round53() }

// FromFloat converts a double to an extended value.
func FromFloat(f float64) Ext80 { return dd{f, 0} }

// Pow models MSVCRT's _CIpow as a correctly rounded pow: its x87
// implementation works with a 64-bit mantissa, so it rounds correctly
// except within ~2^-11 ulp of a tie. Special cases follow C99 pow, which
// matches MSVCRT for the inputs the plugin produces (NaN for a negative
// base with a non-integer exponent).
func Pow(x, y float64) float64 {
	switch {
	case y == 0:
		return 1
	case x == 1:
		return 1
	case math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) || x == 0:
		return math.Pow(x, y)
	}
	sign := 1.0
	if x < 0 {
		if y != math.Trunc(y) {
			return math.NaN()
		}
		if math.Mod(y, 2) != 0 {
			sign = -1
		}
		x = -x
	}
	// Ziv's strategy: a cheap ~2^-72 evaluation settles the rounding
	// unless the result lies within its error bound of a rounding
	// boundary; only then is the ~2^-104 path taken.
	l := dd{math.Log(x), 0}
	t := l.add(expFastDD(l.neg()).mulF(x).add(dd{-1, 0})).mulF(y)
	if math.Abs(t.hi) > 700 { // near overflow/underflow: no extra precision needed for the plugin's ranges
		return sign * math.Pow(x, y)
	}
	v := expFastDD(t)
	e := (math.Abs(y) + 2) * 0x1p-68 * math.Abs(v.hi)
	if lo, hi := v.hi+(v.lo-e), v.hi+(v.lo+e); lo == hi {
		return sign * lo
	}
	return sign * expDD(logDD(x).mulF(y)).Round53()
}
