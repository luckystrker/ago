package acp

import (
	"reflect"
	"testing"

	"github.com/luckystrker/ago/events"
)

// TestMapUsageCountsCacheInPrompt pins the shared usage semantics (D-126, ADR
// 0036) for the ACP client: ACP's inputTokens leaves the cache out and its
// totalTokens sums input, output and both cache counts, so both cache counts go
// into prompt_tokens and a reported total below prompt+output is not believed.
// Python parity: acp/mappers.map_usage at v1.3.2.
func TestMapUsageCountsCacheInPrompt(t *testing.T) {
	cases := []struct {
		name string
		in   *PromptUsage
		want events.Usage
	}{
		{
			name: "no cache",
			in:   &PromptUsage{InputTokens: events.I64(100), OutputTokens: events.I64(20), TotalTokens: events.I64(120)},
			want: events.Usage{PromptTokens: events.I64(100), CompletionTokens: events.I64(20), TotalTokens: events.I64(120)},
		},
		{
			name: "cache read and write",
			in: &PromptUsage{InputTokens: events.I64(100), OutputTokens: events.I64(20), TotalTokens: events.I64(1070),
				CachedReadTokens: events.I64(900), CachedWriteTokens: events.I64(50)},
			want: events.Usage{PromptTokens: events.I64(1050), CompletionTokens: events.I64(20), TotalTokens: events.I64(1070),
				CacheReadInputTokens: events.I64(900), CacheCreationInputTokens: events.I64(50)},
		},
		{
			// ACP's total may legitimately exceed prompt+output; it is kept.
			name: "reported total above the sum",
			in: &PromptUsage{InputTokens: events.I64(10), OutputTokens: events.I64(5), TotalTokens: events.I64(99),
				ThoughtTokens: events.I64(3)},
			want: events.Usage{PromptTokens: events.I64(10), CompletionTokens: events.I64(5), TotalTokens: events.I64(99),
				ThinkingTokens: events.I64(3)},
		},
		{
			name: "reported total below the whole input plus output",
			in: &PromptUsage{InputTokens: events.I64(100), OutputTokens: events.I64(20), TotalTokens: events.I64(100),
				CachedReadTokens: events.I64(900), CachedWriteTokens: events.I64(50)},
			want: events.Usage{PromptTokens: events.I64(1050), CompletionTokens: events.I64(20), TotalTokens: events.I64(1070),
				CacheReadInputTokens: events.I64(900), CacheCreationInputTokens: events.I64(50)},
		},
		{
			name: "absent usage stays empty",
			in:   nil,
			want: events.Usage{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapUsage(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}
