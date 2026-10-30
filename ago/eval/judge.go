package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ago/config"
	"ago/events"
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
// AGO-DEVIATION: the Python judge is an Agent with a locked response_schema
// (native structured output + validation retries); AGo calls the client once
// headless and parses JSON, so a non-JSON verdict is an error (and a fail via
// ScoreWith) rather than a re-ask — D-051.
type LLMJudge struct {
	Client config.LLMClient
	Rubric string
}

func (j LLMJudge) Name() string { return "llm_judge" }

func (j LLMJudge) Score(ctx context.Context, t *Trace) (Score, error) {
	if j.Client == nil {
		return Score{}, errors.New("eval: LLMJudge needs a client")
	}
	resp, err := callHeadless(ctx, j.Client, judgePrompt(j.Rubric, answerText(t)))
	if err != nil {
		return Score{}, fmt.Errorf("eval: judge call: %w", err)
	}
	var v struct {
		Score  *float64 `json:"score"`
		Reason string   `json:"reason"`
	}
	if err := unmarshalVerdict(resp.Content(), &v); err != nil {
		return Score{}, err
	}
	if v.Score == nil {
		return Score{}, errors.New("eval: judge verdict has no score")
	}
	score := clamp01(*v.Score)
	details := map[string]any{"reason": v.Reason}
	if score != *v.Score {
		details["clamped"] = true
		details["raw_score"] = *v.Score
	}
	return Score{Name: j.Name(), Value: score, Passed: score >= 0.5, Details: details}, nil
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
func callHeadless(ctx context.Context, c config.LLMClient, prompt string) (*events.ModelResponse, error) {
	req := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: prompt}}}
	return c.Call(ctx, []events.Event{req}, nil, config.CallOptions{})
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
