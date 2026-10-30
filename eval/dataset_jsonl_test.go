package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSuite(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "suite.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write suite: %v", err)
	}
	return path
}

func TestSuiteFromJSONL(t *testing.T) {
	path := writeSuite(t, strings.Join([]string{
		`{"name": "weather", "input": "Weather in Paris?", "expected": "sunny in Paris",` +
			` "scorers": ["correctness", {"name": "tools_called", "params": {"names": ["get_weather"], "mode": "any"}}]}`,
		``,
		`{"input": "Capital of France?", "expected": "Paris", "scorers": [{"name": "correctness", "params": {"expected": "Lyon."}}]}`,
		``,
	}, "\n"))
	suite, err := SuiteFromJSONL(path, NewScorerRegistry())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(suite) != 2 {
		t.Fatalf("cases: %d", len(suite))
	}
	w := suite[0]
	if w.Name != "weather" || w.Input != "Weather in Paris?" || w.Expected != "sunny in Paris" {
		t.Errorf("weather case: %+v", w)
	}
	if len(w.Scorers) != 2 {
		t.Fatalf("weather scorers: %d", len(w.Scorers))
	}
	// The bare "correctness" entry grades against the case's expected.
	got, err := w.Scorers[0].Score(context.Background(), answeredTrace("sunny in Paris"))
	if err != nil || !got.Passed {
		t.Errorf("bare correctness entry: err=%v score=%+v", err, got)
	}
	got, err = w.Scorers[1].Score(context.Background(), failedToolTrace(""))
	if err != nil || !got.Passed {
		t.Errorf("tools_called entry: err=%v score=%+v", err, got)
	}
	// Missing name auto-fills in dataset order (Python parity: task-0000…).
	c := suite[1]
	if c.Name != "task-0001" {
		t.Errorf("auto name: %q", c.Name)
	}
	if len(c.Scorers) != 1 {
		t.Fatalf("capital scorers: %d", len(c.Scorers))
	}
	if got, _ = c.Scorers[0].Score(context.Background(), answeredTrace("Lyon.")); !got.Passed {
		t.Errorf("explicit expected must override the case scope (Paris): %+v", got)
	}
}

func TestSuiteFromJSONLRunsEndToEnd(t *testing.T) {
	path := writeSuite(t, strings.Join([]string{
		`{"name": "one", "input": "q1", "expected": "sunny in Paris", "scorers": ["correctness"]}`,
		`{"name": "two", "input": "q2", "expected": "rain in Paris", "scorers": ["correctness"]}`,
	}, "\n"))
	suite, err := SuiteFromJSONL(path, NewScorerRegistry())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	a := scriptedAgent(t, textResp("sunny in Paris"), textResp("rain in Paris"))
	res, err := Run(context.Background(), a, suite, WithConcurrency(1))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.PassRate != 1.0 || len(res.Cases) != 2 {
		t.Fatalf("result: %+v", res)
	}
	if res.Cases[0].Name != "one" || res.Cases[1].Name != "two" {
		t.Errorf("case order: %q %q", res.Cases[0].Name, res.Cases[1].Name)
	}
}

func TestSuiteFromJSONLErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		content  string
		registry bool
		want     []string
	}{
		{
			name:    "invalid JSON",
			content: "{\"input\": \"x\"\n",
			want:    []string{"line 1", "not valid JSON"},
		},
		{
			name:    "not an object",
			content: "{\"input\": \"x\"}\n[1, 2]\n",
			want:    []string{"line 2", "must be a JSON object", "array"},
		},
		{
			name:    "missing input",
			content: "{\"name\": \"no-input\"}\n",
			want:    []string{"missing required field", "\"input\""},
		},
		{
			name:    "non-string input",
			content: "{\"input\": 42}\n",
			want:    []string{"\"input\"", "must be a string"},
		},
		{
			name:    "scorers without a registry",
			content: "{\"input\": \"x\", \"scorers\": [\"correctness\"]}\n",
			want:    []string{"no scorer registry"},
		},
		{
			name:     "unknown scorer",
			content:  "{\"input\": \"x\", \"scorers\": [\"nonsense\"]}\n",
			registry: true,
			want:     []string{"no scorer named"},
		},
		{
			name:     "scorer object without a name",
			content:  "{\"input\": \"x\", \"scorers\": [{\"params\": {}}]}\n",
			registry: true,
			want:     []string{"needs a \"name\""},
		},
		{
			name:     "scorers not a list",
			content:  "{\"input\": \"x\", \"scorers\": \"correctness\"}\n",
			registry: true,
			want:     []string{"must be a list"},
		},
	} {
		path := writeSuite(t, tc.content)
		var r *ScorerRegistry
		if tc.registry {
			r = NewScorerRegistry()
		}
		_, err := SuiteFromJSONL(path, r)
		if err == nil {
			t.Errorf("%s: must error", tc.name)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q misses %q", tc.name, err, want)
			}
		}
	}
}

func TestSuiteFromJSONLBlankAndBOM(t *testing.T) {
	path := writeSuite(t, "\ufeff{\"input\": \"x\"}\n\n   \n{\"input\": \"y\"}\n")
	suite, err := SuiteFromJSONL(path, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(suite) != 2 || suite[0].Name != "task-0000" || suite[1].Name != "task-0001" {
		t.Errorf("BOM/blank handling: %+v", suite)
	}
}
