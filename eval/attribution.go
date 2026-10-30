package eval

import (
	"context"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
)

// ErrorModes is the starter failure taxonomy: mechanical modes come from the
// deterministic detector, semantic ones only from the LLM attributor (Python
// parity: scorers/attribution.py ERROR_MODES).
var ErrorModes = []string{
	"none",                  // no failure
	"tool_failure",          // a tool errored (terminally)
	"hallucinated_tool",     // called a tool that does not exist (semantic)
	"loop",                  // stuck repeating without progress (semantic)
	"crash",                 // the run raised an exception
	"no_answer",             // finished with no final answer
	"incorrect_answer",      // answered, but wrong (semantic)
	"premature_termination", // stopped before completing the task (semantic)
	"ignored_constraint",    // violated an instruction/constraint (semantic)
	"hallucinated_fact",     // asserted a false fact (semantic)
	"other",
}

// Attribution is structured failure attribution for one run (Python parity:
// attribution.Attribution, serialized into the score's Details).
type Attribution struct {
	Failed           bool   `json:"failed"`
	ErrorMode        string `json:"error_mode"`
	DecisiveStep     *int   `json:"decisive_step"` // index into Trace.Events of the decisive event
	ResponsibleAgent string `json:"responsible_agent,omitempty"`
	Reasoning        string `json:"reasoning"`
}

// DetectMechanical attributes the mechanical failures a Trace carries on its
// face: the ask raised (Trace.Err — Python parity: Trace.exception), a tool
// errored terminally with no answer produced, or the run finished without an
// answer. nil defers to the LLM attributor — the trace answered, so only a
// semantic read can tell whether the answer was actually right (Python parity:
// attribution._detect_mechanical).
func DetectMechanical(t *Trace) *Attribution {
	if t.Err != "" {
		return mech(t, lastIndex(t), "crash", fmt.Sprintf("run raised: %s", t.Err))
	}
	hasAnswer := t.FinalResponse != ""
	if idx, name, ok := firstToolError(t); ok && !hasAnswer {
		return mech(t, &idx, "tool_failure", fmt.Sprintf("tool %q errored and the run produced no answer", name))
	}
	if !hasAnswer {
		return &Attribution{
			Failed:           true,
			ErrorMode:        "no_answer",
			ResponsibleAgent: t.AgentName,
			Reasoning:        "run produced no final answer",
		}
	}
	return nil
}

func mech(t *Trace, step *int, mode, reasoning string) *Attribution {
	return &Attribution{
		Failed:           true,
		ErrorMode:        mode,
		DecisiveStep:     step,
		ResponsibleAgent: t.AgentName,
		Reasoning:        reasoning,
	}
}

func lastIndex(t *Trace) *int {
	if len(t.Events) == 0 {
		return nil
	}
	last := len(t.Events) - 1
	return &last
}

// firstToolError returns the index into Trace.Events of the first tool error
// event and the tool's name (Python parity: trace.events_of(ToolErrorEvent)[0];
// the not-found event is a tool error is-a in both codebases).
func firstToolError(t *Trace) (int, string, bool) {
	for i, ev := range t.Events {
		switch e := events.AsValue(ev).(type) {
		case events.ToolErrorEvent:
			return i, e.Name, true
		case events.ToolNotFoundEvent:
			return i, e.Name, true
		}
	}
	return 0, "", false
}

// FailureAttribution names what went wrong on a failed trace (Python parity:
// scorers/attribution.py failure_attribution). The deterministic detector runs
// first; when it finds nothing and Client is set, the LLM attributor reads the
// numbered trajectory and attributes the semantic failures (incorrect answer,
// ignored constraint, …) the rules can't see. Without a Client a clean-looking
// trace is reported as error_mode "none".
//
// The score's Value is 1.0/0.0 by !Attribution.Failed (the run convention for
// pass/fail scorers, so pass-rate math treats it uniformly); the categorical
// verdict lives in Details["error_mode"] — Python parity: Feedback.value = the
// error mode, Feedback.detail = the Attribution dump — with the full
// Attribution alongside.
type FailureAttribution struct {
	// Client is the attributor model; nil keeps the scorer deterministic-only.
	Client config.LLMClient
	// Key is the score name; "" uses "failure" (the Python default), so a
	// suite's error-mode distribution reads off the scores by this key.
	Key string
	// AgentName labels ResponsibleAgent (single-agent today; the multi-agent
	// "who" is the extension point, Python parity).
	AgentName string
	// Taxonomy bounds the LLM attributor's choice; nil uses ErrorModes.
	Taxonomy []string
}

func (f FailureAttribution) Name() string {
	if f.Key != "" {
		return f.Key
	}
	return "failure"
}

func (f FailureAttribution) Score(ctx context.Context, t *Trace) (Score, error) {
	attr := DetectMechanical(t)
	if attr == nil {
		if f.Client != nil {
			var err error
			if attr, err = f.llmAttribute(ctx, t); err != nil {
				return Score{}, err
			}
		} else {
			attr = &Attribution{
				Failed:           false,
				ErrorMode:        "none",
				ResponsibleAgent: f.AgentName,
				Reasoning:        "no mechanical failure detected",
			}
		}
	}
	details := map[string]any{
		"error_mode": attr.ErrorMode,
		"failed":     attr.Failed,
		"reasoning":  attr.Reasoning,
	}
	if attr.DecisiveStep != nil {
		details["decisive_step"] = *attr.DecisiveStep
	}
	if attr.ResponsibleAgent != "" {
		details["responsible_agent"] = attr.ResponsibleAgent
	}
	return boolScore(f.Name(), !attr.Failed, details), nil
}

// llmAttribute asks the attributor model over the rendered trajectory (Python
// parity: _llm_attribute + _render + _system_prompt).
//
// AGO-DEVIATION: the Python attributor is an Agent with a locked
// response_schema and validation retries; AGo calls the client headless and
// parses a JSON verdict — a non-JSON verdict is an error (a recorded fail via
// ScoreWith), and a verdict with no error_mode degrades to "other" (Python:
// content() returning None) — D-051, D-061.
func (f FailureAttribution) llmAttribute(ctx context.Context, t *Trace) (*Attribution, error) {
	resp, err := callHeadless(ctx, f.Client, f.attributePrompt(t))
	if err != nil {
		return nil, fmt.Errorf("eval: attributor call: %w", err)
	}
	var v struct {
		Failed       *bool  `json:"failed"`
		ErrorMode    string `json:"error_mode"`
		DecisiveStep *int   `json:"decisive_step"`
		Reasoning    string `json:"reasoning"`
	}
	if err := unmarshalVerdict(resp.Content(), &v); err != nil {
		return nil, err
	}
	if v.ErrorMode == "" {
		return &Attribution{
			Failed:           false,
			ErrorMode:        "other",
			ResponsibleAgent: f.AgentName,
			Reasoning:        "attributor returned no verdict",
		}, nil
	}
	failed := v.Failed != nil && *v.Failed
	step := v.DecisiveStep
	if step != nil && *step < 0 {
		step = nil // Python: decisive_step -1 means "not applicable"
	}
	return &Attribution{
		Failed:           failed,
		ErrorMode:        v.ErrorMode,
		DecisiveStep:     step,
		ResponsibleAgent: f.AgentName,
		Reasoning:        v.Reasoning,
	}, nil
}

func (f FailureAttribution) attributePrompt(t *Trace) string {
	taxonomy := f.Taxonomy
	if taxonomy == nil {
		taxonomy = ErrorModes
	}
	var b strings.Builder
	b.WriteString("You analyze an agent run and attribute any failure. Given the task and the numbered " +
		"trajectory, decide: whether the run failed; the single error_mode from this taxonomy — " +
		fmt.Sprintf("[%s] ('none' if it succeeded); the decisive_step (the step number where ", strings.Join(taxonomy, ", ")) +
		"it went wrong, or -1 if not applicable); and a brief reasoning. Judge the trajectory as a whole.\n" +
		`Return ONLY a JSON object: {"failed": <bool>, "error_mode": "<mode>", "decisive_step": <int>, "reasoning": "<brief justification>"}.` + "\n\n")
	task := ""
	if len(t.LLMCalls) > 0 {
		task = t.LLMCalls[0].InputText
	}
	// AGO-DEVIATION: Python renders the task from inputs["input"] and a
	// reference section from reference_outputs; the scorer sees only the trace,
	// so the first model call's input stands in for the task and the reference
	// section is dropped — D-061.
	fmt.Fprintf(&b, "## Task\n%s\n\n", task)
	fmt.Fprintf(&b, "## Final answer\n%s\n\n", answerText(t))
	fmt.Fprintf(&b, "## Trajectory (numbered steps)\n%s", renderAttributionSteps(t))
	return b.String()
}

// renderAttributionSteps renders the numbered trajectory (Python parity:
// _render_steps + _describe).
func renderAttributionSteps(t *Trace) string {
	if len(t.Events) == 0 {
		return "(no steps)"
	}
	lines := make([]string, 0, len(t.Events))
	for i, ev := range t.Events {
		lines = append(lines, fmt.Sprintf("[%d] %s", i, describeEvent(ev)))
	}
	return strings.Join(lines, "\n")
}

func describeEvent(ev events.Event) string {
	switch e := events.AsValue(ev).(type) {
	case events.ToolErrorEvent:
		return fmt.Sprintf("tool error: %s -> %s", e.Name, e.Error)
	case events.ToolNotFoundEvent:
		return fmt.Sprintf("tool error: %s -> %s", e.Name, e.Error)
	case events.ToolResultEvent:
		return fmt.Sprintf("tool result: %s", e.Name)
	case events.ToolCallEvent:
		return fmt.Sprintf("tool call: %s(%s)", e.Name, e.Arguments)
	case events.ModelResponse:
		return fmt.Sprintf("model: %q", e.Content())
	default:
		return ev.EventType()
	}
}
