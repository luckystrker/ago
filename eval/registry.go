package eval

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/luckystrker/ago/config"
)

// ScorerFactory builds a scorer from a data-driven parameter map. It is the Go
// stand-in for Python's @scorer DI decorator: where Python introspects a
// function's parameters and injects values by name, AGo resolves a scorer name
// plus a params map — so a Suite (or an eval spec file) can name its scorers as
// data instead of building them in code (D-061).
type ScorerFactory func(params map[string]any, scope CaseScope) (Scorer, error)

// CaseScope carries the per-case suite data a data-driven scorer may fall back
// to. Expected is the case's "expected" field: a correctness scorer without an
// explicit "expected" param grades against it, mirroring how a suite author
// in code builds Correctness{Expected: c.Expected}.
type CaseScope struct {
	Expected string
}

// ScorerRegistry resolves scorer names to factories. The zero value is not
// usable — construct with NewScorerRegistry, which pre-registers the built-ins
// (correctness, token_budget, tools_called, threshold, llm_judge, failure).
// Register and Build are safe for concurrent use.
type ScorerRegistry struct {
	mu        sync.RWMutex
	factories map[string]ScorerFactory
	client    config.LLMClient
}

// NewScorerRegistry creates a registry with the built-in scorers registered.
func NewScorerRegistry() *ScorerRegistry {
	r := &ScorerRegistry{factories: map[string]ScorerFactory{}}
	r.Register("correctness", r.buildCorrectness)
	r.Register("token_budget", buildTokenBudget)
	r.Register("tools_called", buildToolsCalled)
	r.Register("threshold", r.buildThreshold)
	r.Register("llm_judge", r.buildLLMJudge)
	r.Register("failure", r.buildFailure)
	return r
}

// SetClient installs the model the client-backed built-ins (llm_judge,
// failure) use; nil unsets.
func (r *ScorerRegistry) SetClient(c config.LLMClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.client = c
}

// Register adds a factory under name; an empty name, a nil factory, or a
// duplicate name is an error.
func (r *ScorerRegistry) Register(name string, f ScorerFactory) error {
	if name == "" {
		return errors.New("eval: scorer name must not be empty")
	}
	if f == nil {
		return fmt.Errorf("eval: scorer %q: factory must not be nil", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[name]; dup {
		return fmt.Errorf("eval: scorer %q is already registered", name)
	}
	r.factories[name] = f
	return nil
}

// Names lists the registered scorer names, sorted.
func (r *ScorerRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Build constructs the scorer registered under name from params. Built-in
// scorers reject unknown parameter keys (a data-driven config must not swallow
// typos); user-registered factories validate their own params.
func (r *ScorerRegistry) Build(name string, params map[string]any, scope CaseScope) (Scorer, error) {
	r.mu.RLock()
	f, ok := r.factories[name]
	allowed, builtin := builtinParams[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("eval: no scorer named %q (registered: %v)", name, r.Names())
	}
	if builtin {
		if err := checkParams(params, allowed); err != nil {
			return nil, fmt.Errorf("eval: scorer %q: %w", name, err)
		}
	}
	return f(params, scope)
}

// BuildScorers constructs a spec list in order (Python parity: passing a list
// of scorer objects to run_agent).
func (r *ScorerRegistry) BuildScorers(specs []ScorerSpec, scope CaseScope) ([]Scorer, error) {
	out := make([]Scorer, 0, len(specs))
	for _, spec := range specs {
		sc, err := r.Build(spec.Name, spec.Params, scope)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, nil
}

// ScorerSpec is one data-driven scorer request: a registered name plus its
// params (the "scorers" entries of a JSONL suite line).
type ScorerSpec struct {
	Name   string         `json:"name"`
	Params map[string]any `json:"params,omitempty"`
}

// builtinParams is the allowed parameter key set per built-in name — the typo
// guard for Build.
var builtinParams = map[string][]string{
	"correctness":  {"expected", "mode"},
	"token_budget": {"max"},
	"tools_called": {"names", "mode"},
	"threshold":    {"inner", "min"},
	"llm_judge":    {"rubric"},
	"failure":      {"agent_name", "key", "taxonomy"},
}

func (r *ScorerRegistry) buildCorrectness(params map[string]any, scope CaseScope) (Scorer, error) {
	c := Correctness{Expected: scope.Expected}
	if v, ok, err := paramStringOpt(params, "expected"); err != nil {
		return nil, err
	} else if ok {
		c.Expected = v
	}
	mode, err := paramEnumOpt(params, "mode", "casefold", MatchExact, MatchCasefold, MatchContains)
	if err != nil {
		return nil, err
	}
	c.Mode = MatchMode(mode)
	return c, nil
}

func buildTokenBudget(params map[string]any, _ CaseScope) (Scorer, error) {
	max, err := paramInt64(params, "max")
	if err != nil {
		return nil, err
	}
	return TokenBudget{Max: max}, nil
}

func buildToolsCalled(params map[string]any, _ CaseScope) (Scorer, error) {
	names, err := paramStringSlice(params, "names")
	if err != nil {
		return nil, err
	}
	mode, err := paramEnumOpt(params, "mode", "all", CallAll, CallAny)
	if err != nil {
		return nil, err
	}
	return ToolsCalled{Names: names, Mode: CallMode(mode)}, nil
}

func (r *ScorerRegistry) buildThreshold(params map[string]any, scope CaseScope) (Scorer, error) {
	inner, err := paramScorer(params, "inner", r)
	if err != nil {
		return nil, err
	}
	min, err := paramFloat(params, "min")
	if err != nil {
		return nil, err
	}
	return Threshold{Inner: inner, Min: min}, nil
}

func (r *ScorerRegistry) buildLLMJudge(params map[string]any, _ CaseScope) (Scorer, error) {
	rubric, err := paramString(params, "rubric")
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	client := r.client
	r.mu.RUnlock()
	if client == nil {
		return nil, errors.New("eval: scorer \"llm_judge\": set a client on the registry first (SetClient)")
	}
	return LLMJudge{Client: client, Rubric: rubric}, nil
}

func (r *ScorerRegistry) buildFailure(params map[string]any, _ CaseScope) (Scorer, error) {
	f := FailureAttribution{}
	if v, ok, err := paramStringOpt(params, "agent_name"); err != nil {
		return nil, err
	} else if ok {
		f.AgentName = v
	}
	if v, ok, err := paramStringOpt(params, "key"); err != nil {
		return nil, err
	} else if ok {
		f.Key = v
	}
	if raw, ok := params["taxonomy"]; ok {
		items, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("eval: param %q must be a list of strings, got %T", "taxonomy", raw)
		}
		f.Taxonomy = make([]string, 0, len(items))
		for i, item := range items {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("eval: param %q[%d] must be a string, got %T", "taxonomy", i, item)
			}
			f.Taxonomy = append(f.Taxonomy, s)
		}
	}
	r.mu.RLock()
	client := r.client
	r.mu.RUnlock()
	if client != nil {
		f.Client = client
	}
	return f, nil
}

// paramScorer decodes a nested scorer spec {"name": …, "params": {…}} — how
// Threshold carries its inner scorer in data.
func paramScorer(params map[string]any, key string, r *ScorerRegistry) (Scorer, error) {
	raw, ok := params[key]
	if !ok {
		return nil, fmt.Errorf("eval: param %q is required", key)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("eval: param %q must be an object {\"name\", \"params\"}, got %T", key, raw)
	}
	name, _ := m["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("eval: param %q needs a \"name\"", key)
	}
	sub, _ := m["params"].(map[string]any)
	return r.Build(name, sub, CaseScope{})
}

// --- param decoding helpers -------------------------------------------------

func checkParams(params map[string]any, allowed []string) error {
	if len(params) == 0 {
		return nil
	}
	allow := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		allow[k] = true
	}
	extra := make([]string, 0)
	for k := range params {
		if !allow[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return nil
	}
	sort.Strings(extra)
	return fmt.Errorf("unknown param %q (allowed: %v)", extra[0], allowed)
}

func paramString(params map[string]any, key string) (string, error) {
	raw, ok := params[key]
	if !ok {
		return "", fmt.Errorf("eval: param %q is required", key)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("eval: param %q must be a string, got %T", key, raw)
	}
	return s, nil
}

func paramStringOpt(params map[string]any, key string) (string, bool, error) {
	raw, ok := params[key]
	if !ok {
		return "", false, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", false, fmt.Errorf("eval: param %q must be a string, got %T", key, raw)
	}
	return s, true, nil
}

// paramEnumOpt reads an optional enum string, validating it against the
// allowed values and falling back to def.
func paramEnumOpt[T ~string](params map[string]any, key string, def T, allowed ...T) (T, error) {
	raw, ok := params[key]
	if !ok {
		return def, nil
	}
	s, ok := raw.(string)
	if !ok {
		return def, fmt.Errorf("eval: param %q must be a string, got %T", key, raw)
	}
	for _, a := range allowed {
		if string(a) == s {
			return T(s), nil
		}
	}
	return def, fmt.Errorf("eval: param %q is %q, want one of %v", key, s, allowed)
}

func paramFloat(params map[string]any, key string) (float64, error) {
	raw, ok := params[key]
	if !ok {
		return 0, fmt.Errorf("eval: param %q is required", key)
	}
	f, ok := raw.(float64)
	if !ok {
		return 0, fmt.Errorf("eval: param %q must be a number, got %T", key, raw)
	}
	return f, nil
}

// paramInt64 reads an integral number (JSON numbers decode as float64).
func paramInt64(params map[string]any, key string) (int64, error) {
	f, err := paramFloat(params, key)
	if err != nil {
		return 0, err
	}
	if f != float64(int64(f)) {
		return 0, fmt.Errorf("eval: param %q must be a whole number, got %v", key, f)
	}
	return int64(f), nil
}

func paramStringSlice(params map[string]any, key string) ([]string, error) {
	raw, ok := params[key]
	if !ok {
		return nil, fmt.Errorf("eval: param %q is required", key)
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("eval: param %q must be a list of strings, got %T", key, raw)
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("eval: param %q[%d] must be a string, got %T", key, i, item)
		}
		out = append(out, s)
	}
	return out, nil
}
