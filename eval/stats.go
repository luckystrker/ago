package eval

import (
	"math"
	"sort"
)

// wilsonZ is the two-sided 95% normal quantile (Python parity: pairwise.py
// _Z_95 = 1.96).
const wilsonZ = 1.96

// WilsonCI returns the 95% Wilson score interval for a proportion — small-N
// friendly, clamped to [0, 1] (Python parity: pairwise._wilson_ci). phat is the
// observed rate over n samples; n == 0 yields (0, 0).
func WilsonCI(phat float64, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 0
	}
	fn := float64(n)
	denom := 1 + wilsonZ*wilsonZ/fn
	center := (phat + wilsonZ*wilsonZ/(2*fn)) / denom
	margin := (wilsonZ / denom) * math.Sqrt(phat*(1-phat)/fn+wilsonZ*wilsonZ/(4*fn*fn))
	return math.Max(0, center-margin), math.Min(1, center+margin)
}

// PassRateCI is the Wilson interval around RunResult.PassRate over len(r.Cases).
func (r *RunResult) PassRateCI() (lo, hi float64) {
	if r == nil {
		return 0, 0
	}
	return WilsonCI(r.PassRate, len(r.Cases))
}

// Agreement is two raters' agreement over paired verdicts: the raw match rate
// and Cohen's kappa, the same agreement corrected for the agreement chance
// alone would produce (Python parity: pairwise.Agreement — judge vs human).
type Agreement struct {
	Rate  float64 `json:"rate"`
	Kappa float64 `json:"cohen_kappa"`
	N     int     `json:"n"`
}

// CohenKappa scores two raters' agreement; pairs[i] is (raterX, raterY) for one
// case, labels from the pairwise vocabulary ("a"/"b"/"tie" — or any categorical
// set: the expected-agreement term uses the labels that occur). n == 0 is the
// zero Agreement; degenerate unanimous-pair data (expected agreement of 1)
// reports kappa 1 rather than dividing by zero (Python parity: the
// expected >= 1 branch of pairwise._cohen_kappa).
func CohenKappa(pairs [][2]string) Agreement {
	n := len(pairs)
	if n == 0 {
		return Agreement{}
	}
	fn := float64(n)
	matches := 0
	px := map[string]int{}
	py := map[string]int{}
	for _, p := range pairs {
		if p[0] == p[1] {
			matches++
		}
		px[p[0]]++
		py[p[1]]++
	}
	observed := float64(matches) / fn
	// Deterministic label order so the float sum is stable run to run (and, for
	// the a/b/tie vocabulary, the same order the Python reference sums in).
	labels := make([]string, 0, len(px)+len(py))
	for lbl := range px {
		labels = append(labels, lbl)
	}
	for lbl := range py {
		if _, dup := px[lbl]; !dup {
			labels = append(labels, lbl)
		}
	}
	sort.Strings(labels)
	expected := 0.0
	for _, lbl := range labels {
		expected += (float64(px[lbl]) / fn) * (float64(py[lbl]) / fn)
	}
	kappa := 1.0
	if expected < 1 {
		kappa = (observed - expected) / (1 - expected)
	}
	return Agreement{Rate: observed, Kappa: kappa, N: n}
}
