package eval

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrInvalidRunID rejects a run id that is not a plain file name: ids become
// file names in the store, so a separator or dot-dot would escape it.
var ErrInvalidRunID = errors.New("eval: invalid run id")

// StoreRun writes r into the result store directory dir as <RunID>.json,
// creating dir when needed, and returns the path written (Python parity:
// results/store.py dump keyed by run id; the run's label and the full traces
// — events included — travel inside the file). A result without a RunID —
// one assembled by hand rather than returned by Run — is refused.
func StoreRun(dir string, r *RunResult) (string, error) {
	if r == nil {
		return "", errors.New("eval: StoreRun needs a run result")
	}
	if err := checkRunID(r.RunID); err != nil {
		return "", err
	}
	path := filepath.Join(dir, r.RunID+".json")
	if err := r.SaveJSON(path); err != nil {
		return "", err
	}
	return path, nil
}

// LoadRun reads the run stored under runID in dir (Python parity:
// results/store.py load_run).
func LoadRun(dir, runID string) (*RunResult, error) {
	if err := checkRunID(runID); err != nil {
		return nil, err
	}
	return LoadJSON(filepath.Join(dir, runID+".json"))
}

func checkRunID(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("%w: %q", ErrInvalidRunID, id)
	}
	return nil
}
