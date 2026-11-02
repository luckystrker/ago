package events

import (
	"bytes"
	"testing"
)

// Exact-sizing pins: the estimates below are arithmetic contracts (charsPerToken
// division plus flat additives), so each assert names the precise value rather
// than a bound.

func TestEstimatedTokensCharsPerTokenDefault(t *testing.T) {
	// charsPerToken <= 0 must fall back to 4 (Python parity: default 4), so a
	// 0 must not reach the division.
	if got := EstimatedTokens(TextInput{Content: "abcdefgh"}, 0); got != 2 {
		t.Errorf("charsPerToken=0: got %d, want 2 (default 4)", got)
	}
	if got := EstimatedTokens(TextInput{Content: "abcdefgh"}, -4); got != 2 {
		t.Errorf("charsPerToken=-4: got %d, want 2 (default 4)", got)
	}
}

func TestEstimatedTokensBinaryFlatBudget(t *testing.T) {
	// 400 bytes / 4 = 100, plus the flat binary budget 32.
	ev := BinaryInput{Data: bytes.Repeat([]byte("x"), 400), MediaType: "image/png", Kind: BinaryImage}
	if got, want := EstimatedTokens(ev, 4), 132; got != want {
		t.Errorf("BinaryInput = %d, want %d (100 + flat 32)", got, want)
	}
}

func TestEstimatedTokensModelResponseUsage(t *testing.T) {
	// Usage total wins: content 8/4 = 2 plus the reported total 50.
	resp := ModelResponse{Message: &ModelMessage{Content: "abcdefgh"}, Usage: Usage{TotalTokens: I64(50)}}
	if got, want := EstimatedTokens(resp, 4), 52; got != want {
		t.Errorf("ModelResponse total usage = %d, want %d (2 + 50)", got, want)
	}
	// Without a total, prompt + completion sum: 2 + (30 + 20).
	resp.Usage = Usage{PromptTokens: I64(30), CompletionTokens: I64(20)}
	if got, want := EstimatedTokens(resp, 4), 52; got != want {
		t.Errorf("ModelResponse prompt+completion = %d, want %d (2 + 50)", got, want)
	}
	// Only one side reported: the nil side counts as 0.
	resp.Usage = Usage{PromptTokens: I64(7)}
	if got, want := EstimatedTokens(resp, 4), 9; got != want {
		t.Errorf("ModelResponse prompt only = %d, want %d (2 + 7)", got, want)
	}
}

func TestEstimatedTokensToolCallNamePlusArguments(t *testing.T) {
	// "name(arguments)" sizing counts name + arguments together: 11 + 16 = 27 / 4 = 6.
	ev := ToolCallEvent{ID: "c1", Name: "get_weather", Arguments: `{"city":"Paris"}`}
	if got, want := EstimatedTokens(ev, 4), 6; got != want {
		t.Errorf("ToolCallEvent = %d, want %d", got, want)
	}
}
