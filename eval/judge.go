package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
)

// LLMJudge grades the trace's FinalResponse against a rubric with one model
// call: the rubric plus the answer go to Client, whose reply must be a JSON
// verdict {"score": <0..1>, "reason": "<justification>"} (Python parity:
// scorers/judge.py agent_judge — one judge grades one criterion). The verdict
// score is clamped to [0,1] (the clamp is noted in Details).
//
// Passed is score >= 0.5, the midpoint of the judge's scale; use Threshold for
// a different gate (Python parity: the judge's optional threshold shorthand).
//
// StructuredOutput asks the provider to constrain the answer to the verdict
// schema (the typed Verdict of the Python judge); Retries re-asks a judge whose
// answer does not validate, telling it what was wrong. Both default off, which
// is the pre-existing single-call wire shape.
//
// AGO-DEVIATION: the Python judge is an Agent with a locked response_schema and
// validation retries by default; AGo calls the client headless, so the schema
// is an opt-in request field and a failed verdict is an error (and a fail via
// ScoreWith) once the retries are spent.
type LLMJudge struct {
	Client config.LLMClient
	Rubric string
	// StructuredOutput sends the verdict JSON schema as the call's
	// ResponseSchema, so a provider that supports structured output
	// constrains the answer instead of hoping for well-formed JSON.
	StructuredOutput bool
	// Retries is how many times a verdict that fails parsing or validation is
	// re-asked (Python parity: retries=1 means one re-ask). 0 keeps the single
	// call.
	Retries int
}

// judgeVerdictSchema is the verdict shape the structured-output request pins
// and every parsed answer is checked against: both fields are required, as the
// Python Verdict declares them (a required-only schema is what OpenAI's strict
// structured-output mode accepts).
var judgeVerdictSchema = response.NewSchema[judgeVerdict]("Verdict",
	"A single judge's structured grade on one criterion.")

// judgeVerdict is one parsed judge answer.
type judgeVerdict struct {
	Score  *float64 `json:"score"`
	Reason string   `json:"reason"`
}

func (j LLMJudge) Name() string { return "llm_judge" }

// Score asks the judge for a verdict, re-asking a failed one up to Retries
// times with the validation error appended (Python parity: agent.py
// reply.content's retry loop).
func (j LLMJudge) Score(ctx context.Context, t *Trace) (Score, error) {
	if j.Client == nil {
		return Score{}, errors.New("eval: LLMJudge needs a client")
	}
	prompt := judgePrompt(j.Rubric, answerText(t))
	attempts := max(j.Retries, 0) + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		resp, err := callHeadless(ctx, j.Client, prompt, j.callOptions())
		if err != nil {
			return Score{}, fmt.Errorf("eval: judge call: %w", err)
		}
		var v judgeVerdict
		if err := unmarshalVerdict(resp.Content(), &v); err != nil {
			lastErr = err
		} else if v.Score == nil {
			lastErr = errors.New("eval: judge verdict has no score")
		} else {
			return j.verdictScore(*v.Score, v.Reason), nil
		}
		prompt = retryPrompt(prompt, lastErr)
	}
	return Score{}, lastErr
}

// verdictScore turns a parsed score into the scorer's verdict, noting a clamp.
func (j LLMJudge) verdictScore(raw float64, reason string) Score {
	score := clamp01(raw)
	details := map[string]any{"reason": reason}
	if score != raw {
		details["clamped"] = true
		details["raw_score"] = raw
	}
	return Score{Name: j.Name(), Value: score, Passed: score >= 0.5, Details: details}
}

// callOptions is the structured-output request, or an empty option set when it
// is off (the old wire shape).
func (j LLMJudge) callOptions() config.CallOptions {
	if !j.StructuredOutput {
		return config.CallOptions{}
	}
	return config.CallOptions{ResponseSchema: judgeVerdictSchema}
}

// retryPrompt is the re-ask: the original prompt plus what failed (Python
// parity: reply.content's "could not be validated by schema" continuation).
func retryPrompt(prompt string, err error) string {
	return prompt + "\n\nYour previous response could not be validated.\n" +
		"Please answer again with ONLY the JSON object.\n\n== Validation Error ==\n" + err.Error()
}

func judgePrompt(rubric, answer string) string {
	var b strings.Builder
	b.WriteString("You are a strict evaluator grading an AI agent's response against a single criterion.\n")
	fmt.Fprintf(&b, "Criterion: %s\n", rubric)
	b.WriteString(`Return ONLY a JSON object: {"score": <number from 0 to 1>, "reason": "<brief justification>"}.` + "\n")
	b.WriteString("Judge only this criterion — nothing else.\n\n")
	fmt.Fprintf(&b, "## Agent answer\n%s", answer)
	return b.String()
}

func answerText(t *Trace) string {
	if t == nil || t.FinalResponse == "" {
		return "(no answer)"
	}
	return t.FinalResponse
}

// callHeadless performs one client call over a single synthetic request with no
// conversation (a nil conv is the documented headless mode —
// config/openaicompat.go:61).
func callHeadless(ctx context.Context, c config.LLMClient, prompt string, opts config.CallOptions) (*events.ModelResponse, error) {
	req := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: prompt}}}
	return c.Call(ctx, []events.Event{req}, nil, opts)
}

// unmarshalVerdict parses a JSON verdict, tolerating markdown fences and prose
// around the object (structured output is the happy path; the fallbacks catch
// models that wrap the verdict in ```json ... ``` or a sentence).
func unmarshalVerdict(text string, v any) error {
	s := strings.TrimSpace(text)
	if fence := stripFences(s); fence != "" {
		s = fence
	}
	if err := json.Unmarshal([]byte(s), v); err == nil {
		return nil
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(s[start:end+1]), v); err == nil {
			return nil
		}
	}
	return fmt.Errorf("eval: judge returned no JSON verdict: %.200s", text)
}

// stripFences removes a leading ```lang / trailing ``` pair; "" when the text
// is not fenced.
func stripFences(s string) string {
	if !strings.HasPrefix(s, "```") {
		return ""
	}
	body := s[3:]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	} else {
		return ""
	}
	body = strings.TrimSuffix(strings.TrimSpace(body), "```")
	return strings.TrimSpace(body)
}

func clamp01(v float64) float64 { return min(max(v, 0.0), 1.0) }
