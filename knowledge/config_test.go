package knowledge

import "testing"

// TestNewConfigDefaults checks the Python KnowledgeConfig defaults.
func TestNewConfigDefaults(t *testing.T) {
	cfg := NewConfig(NewMemoryStore())
	if !cfg.ExposeTool {
		t.Error("NewConfig: ExposeTool should default to true")
	}
	if !cfg.WriteEventLog {
		t.Error("NewConfig: WriteEventLog should default to true")
	}
	if cfg.Bootstrap != nil {
		t.Error("NewConfig: Bootstrap should default to nil (DefaultBootstrap fallback)")
	}
	if cfg.Store == nil {
		t.Error("NewConfig: store not carried")
	}
	// The strategy fields default to unset, like Python's None — wiring them is
	// opt-in (the agent constructor adds no middleware without a strategy).
	if cfg.Compact != nil {
		t.Error("NewConfig: Compact should default to nil")
	}
	if cfg.CompactTrigger != nil {
		t.Error("NewConfig: CompactTrigger should default to nil")
	}
	if cfg.Aggregate != nil {
		t.Error("NewConfig: Aggregate should default to nil")
	}
	if cfg.AggregateTrigger != nil {
		t.Error("NewConfig: AggregateTrigger should default to nil")
	}
}

// TestDepKey pins the dependency id the agent registers the store under.
func TestDepKey(t *testing.T) {
	if DepKey != "ag:knowledge" {
		t.Errorf("DepKey = %q, want %q", DepKey, "ag:knowledge")
	}
}
