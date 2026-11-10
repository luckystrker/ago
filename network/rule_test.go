package network

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		globs []string
		s     string
		want  bool
	}{
		{[]string{"*"}, "anything", true},
		{[]string{"agent-*"}, "agent-1", true},
		{[]string{"agent-*"}, "human-1", false},
		{[]string{"a", "b*"}, "bob", true},
		{[]string{"a", "b*"}, "carol", false},
		{nil, "anything", false}, // empty pattern list matches nothing
	}
	for _, tc := range cases {
		if got := Match(tc.globs, tc.s); got != tc.want {
			t.Errorf("Match(%v, %q) = %v, want %v", tc.globs, tc.s, got, tc.want)
		}
	}
}

func TestDefaultRule(t *testing.T) {
	r := DefaultRule()
	if !Match(r.InboundGlobs, "anyone") || !Match(r.OutboundGlobs, "anyone") {
		t.Fatal("default rule must be permissive")
	}
	if r.MaxDelegationDepth != 0 || r.MaxConcurrentChannels != 0 {
		t.Fatal("default rule must have no caps")
	}
}

func TestRuleClone(t *testing.T) {
	r := Rule{InboundGlobs: []string{"a"}, OutboundGlobs: []string{"b"}, MaxDelegationDepth: 3}
	c := r.Clone()
	c.InboundGlobs[0] = "changed"
	if r.InboundGlobs[0] != "a" {
		t.Fatal("Clone must deep-copy the glob slices")
	}
	if c.MaxDelegationDepth != 3 {
		t.Fatal("clone lost the cap")
	}
}

func TestParseDuration(t *testing.T) {
	ok := map[string]time.Duration{
		"":    0,
		"30s": 30 * time.Second,
		"15m": 15 * time.Minute,
		"2h":  2 * time.Hour,
		"1d":  24 * time.Hour,
		"90":  90 * time.Second,
		" 5m": 5 * time.Minute,
		"0":   0,
	}
	for in, want := range ok {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"h", "2x", "1h30m", "abc", "1.5h", "m5"} {
		if _, err := ParseDuration(bad); !errors.Is(err, ErrBadDuration) {
			t.Errorf("ParseDuration(%q): want ErrBadDuration, got %v", bad, err)
		}
	}
}

func TestTokenBucket(t *testing.T) {
	clock := newFakeClock()
	tb := NewTokenBucket(RateBlock{PerMinute: 60, Burst: 3}, clock.Now)

	for i := 0; i < 3; i++ {
		if !tb.Allow() {
			t.Fatalf("burst token %d refused", i)
		}
	}
	if tb.Allow() {
		t.Fatal("empty bucket must refuse")
	}
	clock.Advance(500 * time.Millisecond) // 0.5 token: still empty
	if tb.Allow() {
		t.Fatal("half a token must not allow")
	}
	clock.Advance(500 * time.Millisecond) // 1 token total
	if !tb.Allow() {
		t.Fatal("refilled token refused")
	}
	if tb.Allow() {
		t.Fatal("only one token had refilled")
	}
	clock.Advance(time.Hour) // refill is capped at the burst
	for i := 0; i < 3; i++ {
		if !tb.Allow() {
			t.Fatalf("token %d after long idle refused", i)
		}
	}
	if tb.Allow() {
		t.Fatal("refill must cap at burst")
	}
}

func TestTokenBucketDefaultsAndDisabled(t *testing.T) {
	clock := newFakeClock()
	// Burst unset: capacity is PerMinute.
	tb := NewTokenBucket(RateBlock{PerMinute: 2}, clock.Now)
	if !tb.Allow() || !tb.Allow() || tb.Allow() {
		t.Fatal("capacity must default to PerMinute")
	}
	off := NewTokenBucket(RateBlock{}, clock.Now)
	for i := 0; i < 1000; i++ {
		if !off.Allow() {
			t.Fatal("a disabled bucket allows everything")
		}
	}
}

func TestInboxBlockPolicy(t *testing.T) {
	if (InboxBlock{}).EffectiveOverflow() != OverflowReject {
		t.Fatal("default overflow must be reject")
	}
	if err := (InboxBlock{Overflow: "spill"}).Validate(); !errors.Is(err, ErrBadIdentity) {
		t.Fatalf("unknown overflow: %v", err)
	}
	for _, mode := range []string{"", OverflowReject, OverflowDropNewest, OverflowDropOldest} {
		if err := (InboxBlock{Overflow: mode}).Validate(); err != nil {
			t.Errorf("mode %q: %v", mode, err)
		}
	}
	marks := []struct {
		b    InboxBlock
		want int
	}{
		{InboxBlock{}, 0},                              // no cap, no signal
		{InboxBlock{MaxPending: 10}, 8},                // default 80 %
		{InboxBlock{MaxPending: 1}, 1},                 // never below 1
		{InboxBlock{MaxPending: 10, HighWater: 3}, 3},  // explicit
		{InboxBlock{MaxPending: 10, HighWater: -1}, 0}, // disabled
		{InboxBlock{HighWater: 3}, 0},                  // no cap, no signal
	}
	for _, tc := range marks {
		if got := tc.b.highWaterMark(); got != tc.want {
			t.Errorf("%+v high-water: got %d want %d", tc.b, got, tc.want)
		}
	}
}

func TestRuleJSONOmitsUnsetLimits(t *testing.T) {
	data, _ := json.Marshal(DefaultRule())
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if _, ok := m["rate"]; ok {
		t.Errorf("zero rate must be omitted: %s", data)
	}
	if _, ok := m["inbox"]; ok {
		t.Errorf("zero inbox must be omitted: %s", data)
	}
	r := DefaultRule()
	r.Rate = RateBlock{PerMinute: 60, Burst: 5}
	r.Inbox = InboxBlock{MaxPending: 100, Overflow: OverflowDropOldest, HighWater: 80}
	data, _ = json.Marshal(r)
	var back Rule
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Rate != r.Rate || back.Inbox != r.Inbox {
		t.Fatalf("round trip: %+v", back)
	}
	// Python field names.
	if !strings.Contains(string(data), `"per_minute":60`) || !strings.Contains(string(data), `"max_pending":100`) ||
		!strings.Contains(string(data), `"high_water":80`) || !strings.Contains(string(data), `"overflow":"drop_oldest"`) {
		t.Fatalf("wire names: %s", data)
	}
}
