package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SaveJSON writes the run as indented JSON, creating parent directories when
// needed (Python parity: results/store.py dump).
func (r *RunResult) SaveJSON(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: marshal run result: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("eval: create run-result directory: %w", err)
		}
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("eval: write run result: %w", err)
	}
	return nil
}

// LoadJSON reads a run written by SaveJSON. Events round-trip through their
// "@type" registry (AGO-DEVIATION vs Python's event-less load — D-051), so
// Tokens and event-based scorers work on a loaded run.
func LoadJSON(path string) (*RunResult, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read run result: %w", err)
	}
	var r RunResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("eval: decode run result: %w", err)
	}
	return &r, nil
}

// Comparison tallies two runs' cases paired by Name (Python parity:
// pairwise.py win_rate — variant B's rate, ties counting 0.5). A case wins for
// the run whose case passed while the other's failed; everything else — both
// passing, both failing, or a case present in only one run — is a tie.
// WinRate is B's rate: (BWins + 0.5*Ties) / n.
type Comparison struct {
	AWins   int     `json:"a_wins"`
	BWins   int     `json:"b_wins"`
	Ties    int     `json:"ties"`
	WinRate float64 `json:"win_rate"`
}

// Compare tallies run a against run b.
func Compare(a, b *RunResult) Comparison {
	var cmp Comparison
	if a == nil || b == nil {
		return cmp
	}
	bByName := make(map[string]*CaseResult, len(b.Cases))
	for i := range b.Cases {
		bByName[b.Cases[i].Name] = &b.Cases[i]
	}
	for i := range a.Cases {
		bc, ok := bByName[a.Cases[i].Name]
		if !ok {
			continue
		}
		switch {
		case a.Cases[i].Passed && !bc.Passed:
			cmp.AWins++
		case bc.Passed && !a.Cases[i].Passed:
			cmp.BWins++
		default:
			cmp.Ties++
		}
	}
	if n := cmp.AWins + cmp.BWins + cmp.Ties; n > 0 {
		cmp.WinRate = (float64(cmp.BWins) + 0.5*float64(cmp.Ties)) / float64(n)
	}
	return cmp
}
