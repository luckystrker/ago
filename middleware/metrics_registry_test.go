package middleware

import (
	"fmt"
	"testing"
)

// TestMetricsSamplesSortedByNameThenLabels pins the documented snapshot order:
// samples sort by metric name, and series of one metric sort by their label
// key. One metric carries many series whose insertion order is the reverse of
// the expected order, and its label values sort after the other metric's, so a
// comparator that ignores either order produces a different slice.
func TestMetricsSamplesSortedByNameThenLabels(t *testing.T) {
	reg := NewMetricsRegistry()
	toolLabels := func(agent string) map[string]string {
		return map[string]string{"agent": agent, "tool": "t", "outcome": "success", "error_type": ""}
	}
	const agents = 20
	for i := agents; i >= 1; i-- {
		reg.inc(MetricToolCallsTotal, toolLabels(fmt.Sprintf("z%02d", i)), 1)
	}
	reg.inc(MetricAgentTurnsTotal, map[string]string{"agent": "zz", "outcome": "success", "error_type": ""}, 1)

	samples := reg.Samples()
	want := []struct{ name, agent string }{{MetricAgentTurnsTotal, "zz"}}
	for i := 1; i <= agents; i++ {
		want = append(want, struct{ name, agent string }{MetricToolCallsTotal, fmt.Sprintf("z%02d", i)})
	}
	if len(samples) != len(want) {
		t.Fatalf("samples = %d, want %d: %+v", len(samples), len(want), samples)
	}
	for i, w := range want {
		if samples[i].Name != w.name || samples[i].Labels["agent"] != w.agent {
			t.Fatalf("sample %d = %s(agent=%s), want %s(agent=%s)", i,
				samples[i].Name, samples[i].Labels["agent"], w.name, w.agent)
		}
	}
}

// TestMetricsRegistryIgnoresWrongKind pins the kind guards: a counter cannot be
// observed and a histogram cannot be incremented, so neither call creates a
// series of the wrong shape.
func TestMetricsRegistryIgnoresWrongKind(t *testing.T) {
	reg := NewMetricsRegistry()
	counterLabels := map[string]string{"agent": "a", "outcome": "success", "error_type": ""}
	reg.observe(MetricAgentTurnsTotal, counterLabels, 0.5)
	if s, ok := reg.Sample(MetricAgentTurnsTotal, counterLabels); ok {
		t.Fatalf("observing a counter created a series: %+v", s)
	}

	histLabels := map[string]string{"agent": "a", "outcome": "success", "error_type": ""}
	reg.inc(MetricAgentTurnDurationSeconds, histLabels, 1)
	if s, ok := reg.Sample(MetricAgentTurnDurationSeconds, histLabels); ok {
		t.Fatalf("incrementing a histogram created a series: %+v", s)
	}
}

// TestMetricsHistogramBucketBoundary pins the inclusive bucket comparison: a
// value equal to a bound lands in that bucket, and a value just above it does
// not. The turn histogram's first bound is 0.1s.
func TestMetricsHistogramBucketBoundary(t *testing.T) {
	reg := NewMetricsRegistry()
	labels := map[string]string{"agent": "a", "outcome": "success", "error_type": ""}

	reg.observe(MetricAgentTurnDurationSeconds, labels, 0.1)
	s, ok := reg.Sample(MetricAgentTurnDurationSeconds, labels)
	if !ok {
		t.Fatal("series was not recorded")
	}
	if s.Buckets[0].UpperBound != 0.1 || s.Buckets[0].Count != 1 {
		t.Fatalf("bucket at 0.1 = %+v, want the observation counted (inclusive bound)", s.Buckets[0])
	}

	reg.observe(MetricAgentTurnDurationSeconds, labels, 0.1000001)
	s, _ = reg.Sample(MetricAgentTurnDurationSeconds, labels)
	if s.Buckets[0].Count != 1 {
		t.Fatalf("bucket at 0.1 = %d after a larger value, want 1", s.Buckets[0].Count)
	}
	if s.Buckets[1].Count != 2 {
		t.Fatalf("bucket at 0.25 = %d, want both observations", s.Buckets[1].Count)
	}
	if s.Count != 2 {
		t.Fatalf("Count = %d, want 2", s.Count)
	}
}
