package eval

import (
	"math"
	"testing"
)

// Golden values computed with the Python reference formulas (ag2/ag2/eval/
// pairwise.py _wilson_ci / _cohen_kappa) run verbatim:
//
//	wilson p=0.5 n=10 -> (0.23658959361548731, 0.7634104063845126)
//	wilson p=1.0 n=5  -> (0.5655085052479191, 1.0)
//	wilson p=0.8 n=25 -> (0.6086866446346176, 0.9113954589934753)
//	wilson p=0.0 n=3  -> (0.0, 0.5615060804490177)
//	wilson p=0.8 n=5  -> (0.3755282641185388, 0.9637768390302125)
//	wilson p=0.5 n=0  -> (0.0, 0.0)
//
//	kappa perfect      ([a a][b b][tie tie][a a])          -> 1.0
//	kappa mixed        ([a a][a b][b b][tie a][b b][tie tie]) -> 0.49999999999999994
//	kappa single label ([a a][a a][a a])                   -> 1.0  (expected >= 1 branch)
func TestWilsonCIGolden(t *testing.T) {
	for _, tc := range []struct {
		p      float64
		n      int
		lo, hi float64
	}{
		{0.5, 10, 0.23658959361548731, 0.7634104063845126},
		{1.0, 5, 0.5655085052479191, 1.0},
		{0.8, 25, 0.6086866446346176, 0.9113954589934753},
		{0.0, 3, 0.0, 0.5615060804490177},
		{0.8, 5, 0.3755282641185388, 0.9637768390302125},
		{0.5, 0, 0.0, 0.0},
	} {
		lo, hi := WilsonCI(tc.p, tc.n)
		if math.Abs(lo-tc.lo) > 1e-12 || math.Abs(hi-tc.hi) > 1e-12 {
			t.Errorf("WilsonCI(%v, %d) = (%v, %v), want (%v, %v)", tc.p, tc.n, lo, hi, tc.lo, tc.hi)
		}
		if lo < 0 || hi > 1 {
			t.Errorf("WilsonCI(%v, %d) left [0,1]: (%v, %v)", tc.p, tc.n, lo, hi)
		}
	}
}

func TestCohenKappaGolden(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pairs [][2]string
		rate  float64
		kappa float64
		n     int
	}{
		{
			name:  "perfect",
			pairs: [][2]string{{"a", "a"}, {"b", "b"}, {"tie", "tie"}, {"a", "a"}},
			rate:  1, kappa: 1, n: 4,
		},
		{
			// Python reference value 0.49999999999999994; the comparison is
			// tolerance-based because the summation order of the expected term
			// is not part of the contract.
			name:  "mixed",
			pairs: [][2]string{{"a", "a"}, {"a", "b"}, {"b", "b"}, {"tie", "a"}, {"b", "b"}, {"tie", "tie"}},
			rate:  4.0 / 6.0, kappa: 0.5, n: 6,
		},
		{
			// One label only: expected agreement is 1, the degenerate branch
			// reports kappa 1 instead of dividing by zero.
			name:  "single label",
			pairs: [][2]string{{"a", "a"}, {"a", "a"}, {"a", "a"}},
			rate:  1, kappa: 1, n: 3,
		},
	} {
		got := CohenKappa(tc.pairs)
		if got.N != tc.n || math.Abs(got.Rate-tc.rate) > 1e-12 || math.Abs(got.Kappa-tc.kappa) > 1e-12 {
			t.Errorf("%s: got %+v, want rate=%v kappa=%v n=%d", tc.name, got, tc.rate, tc.kappa, tc.n)
		}
	}
	if got := CohenKappa(nil); got != (Agreement{}) {
		t.Errorf("empty pairs must be the zero Agreement, got %+v", got)
	}
}

func TestPassRateCI(t *testing.T) {
	res := &RunResult{PassRate: 0.8, Cases: make([]CaseResult, 5)}
	lo, hi := res.PassRateCI()
	wantLo, wantHi := WilsonCI(0.8, 5)
	if lo != wantLo || hi != wantHi {
		t.Errorf("PassRateCI = (%v, %v), want WilsonCI(0.8, 5) = (%v, %v)", lo, hi, wantLo, wantHi)
	}
	// A run with no cases has no interval.
	lo, hi = (&RunResult{}).PassRateCI()
	if lo != 0 || hi != 0 {
		t.Errorf("empty run CI = (%v, %v), want (0, 0)", lo, hi)
	}
	// Nil receiver stays safe.
	if lo, hi = (*RunResult)(nil).PassRateCI(); lo != 0 || hi != 0 {
		t.Errorf("nil run CI = (%v, %v), want (0, 0)", lo, hi)
	}
}
