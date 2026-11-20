package gemini

import (
	"context"
	"reflect"
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
)

// TestNormalizeUsageCountsThinkingInCompletion pins the shared usage semantics
// (D-126, ADR 0036): Gemini reports thoughts beside the candidates, so
// completion is candidates + thoughts with thinking_tokens kept as the
// reasoning breakdown, and tool-use prompt tokens stay inside the prompt.
// Python parity: config/gemini/mappers.normalize_usage at v1.3.2.
func TestNormalizeUsageCountsThinkingInCompletion(t *testing.T) {
	cases := []struct {
		name string
		in   genUsage
		want events.Usage
	}{
		{
			name: "without thinking",
			in:   genUsage{PromptTokenCount: i64(100), CandidatesTokenCount: i64(20), TotalTokenCount: i64(120)},
			want: events.Usage{PromptTokens: i64(100), CompletionTokens: i64(20), TotalTokens: i64(120)},
		},
		{
			// Thinking is part of the output, so the total still equals
			// prompt + completion.
			name: "with thinking",
			in: genUsage{PromptTokenCount: i64(100), CandidatesTokenCount: i64(20),
				ThoughtsTokenCount: i64(30), TotalTokenCount: i64(150)},
			want: events.Usage{PromptTokens: i64(100), CompletionTokens: i64(50),
				TotalTokens: i64(150), ThinkingTokens: i64(30)},
		},
		{
			name: "thinking without candidates",
			in: genUsage{PromptTokenCount: i64(10), ThoughtsTokenCount: i64(40),
				TotalTokenCount: i64(50)},
			want: events.Usage{PromptTokens: i64(10), CompletionTokens: i64(40),
				TotalTokens: i64(50), ThinkingTokens: i64(40)},
		},
		{
			name: "tool use prompt tokens",
			in: genUsage{PromptTokenCount: i64(100), ToolUsePromptTokenCount: i64(40),
				CandidatesTokenCount: i64(20), ThoughtsTokenCount: i64(30), TotalTokenCount: i64(190)},
			want: events.Usage{PromptTokens: i64(140), CompletionTokens: i64(50),
				TotalTokens: i64(190), ThinkingTokens: i64(30)},
		},
		{
			// The batch API's total is authoritative and already includes the
			// thoughts (Python parity); unlike the ACP and Bedrock mappers it
			// is not raised to prompt + completion.
			name: "api total below the sum stays as reported",
			in: genUsage{PromptTokenCount: i64(100), CandidatesTokenCount: i64(20),
				ThoughtsTokenCount: i64(30), TotalTokenCount: i64(100)},
			want: events.Usage{PromptTokens: i64(100), CompletionTokens: i64(50),
				TotalTokens: i64(100), ThinkingTokens: i64(30)},
		},
		{
			// A missing total stays missing: the mapper reports the API's
			// totalTokenCount and never invents one from prompt + completion
			// (Python parity: total_tokens=_to_float(metadata.total_token_count)).
			name: "absent total is not computed",
			in: genUsage{PromptTokenCount: i64(100), CandidatesTokenCount: i64(20),
				ThoughtsTokenCount: i64(30)},
			want: events.Usage{PromptTokens: i64(100), CompletionTokens: i64(50),
				ThinkingTokens: i64(30)},
		},
		{
			name: "absent metadata stays nil",
			in:   genUsage{},
			want: events.Usage{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			if got := normalizeUsage(&in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := normalizeUsage(nil); got != (events.Usage{}) {
		t.Fatalf("nil metadata = %+v, want the zero usage", got)
	}
}

// TestNormalizeUsageStreamCountsThinkingInCompletion pins that the streaming
// path folds thoughts into completion exactly like the batch path.
func TestNormalizeUsageStreamCountsThinkingInCompletion(t *testing.T) {
	srv, _ := sseServer(t,
		`{"candidates": [{"content": {"role": "model", "parts": [{"text": "Hi"}]}}]}`,
		`{"candidates": [{"content": {"role": "model"}}],
		  "usageMetadata": {"promptTokenCount": 100, "candidatesTokenCount": 20,
		    "thoughtsTokenCount": 30, "totalTokenCount": 150}}`,
	)
	cl := startClient(t, newTestConfig(srv.URL, WithStreaming(true)))
	resp, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	want := events.Usage{PromptTokens: i64(100), CompletionTokens: i64(50),
		TotalTokens: i64(150), ThinkingTokens: i64(30)}
	if !reflect.DeepEqual(resp.Usage, want) {
		t.Fatalf("stream usage = %+v, want %+v", resp.Usage, want)
	}
}
