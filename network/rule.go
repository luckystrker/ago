package network

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rule-level errors. Wrap with %w at the raise site.
var (
	// ErrInboxFull means the recipient's inbox is at Rule.Inbox.MaxPending
	// and the overflow policy is "reject" (Python InboxFull).
	ErrInboxFull = errors.New("network: inbox full")
	// ErrRateLimited means the sender's token bucket (Rule.Rate) is empty.
	ErrRateLimited = errors.New("network: rate limit exceeded")
	// ErrBadDuration is returned by ParseDuration for an unparseable string.
	ErrBadDuration = errors.New("network: invalid duration")
)

// Rule is the per-(hub, agent) access + limits policy. Defaults are
// permissive (allow everyone, no caps); apps tighten by passing a non-zero
// rule at registration. Both halves are enforced at the hub, never the
// client.
//
// AGO-DEVIATION vs Python's Rule: the brief pins a flat shape — channel-type
// access and default TTLs are not ported, and the Python limits.* nesting is
// flattened (MaxDelegationDepth ≙ limits.delegation_depth, Rate ≙
// limits.rate, Inbox ≙ limits.inbox).
type Rule struct {
	// InboundGlobs globs over sender *names*; a recipient accepts envelopes
	// only from matching senders. Empty = accept nobody (use DefaultRule).
	InboundGlobs []string `json:"inbound_globs,omitempty"`
	// OutboundGlobs globs over recipient *names*; the sender may address
	// only matching recipients.
	OutboundGlobs []string `json:"outbound_globs,omitempty"`
	// MaxDelegationDepth caps Envelope.Depth on send; 0 disables the cap.
	MaxDelegationDepth int `json:"max_delegation_depth,omitempty"`
	// MaxConcurrentChannels caps the channels one creator may keep
	// non-terminal; 0 disables the cap.
	MaxConcurrentChannels int `json:"max_concurrent_channels,omitempty"`
	// Rate is the sender's token-bucket limit on substantive envelopes; the
	// zero value disables it.
	//
	// AGO-DEVIATION: Python stores RateBlock on the rule but never enforces
	// it; the hub enforces it here (Send fails with ErrRateLimited).
	Rate RateBlock `json:"rate,omitzero"`
	// Inbox is the recipient's inbox capacity policy; the zero value means
	// no hub-side cap.
	Inbox InboxBlock `json:"inbox,omitzero"`
}

// RateBlock is a token-bucket rate limit: PerMinute tokens refill evenly,
// Burst is the bucket capacity (0 = PerMinute). PerMinute <= 0 disables the
// limiter.
type RateBlock struct {
	PerMinute int `json:"per_minute,omitempty"`
	Burst     int `json:"burst,omitempty"`
}

// Inbox overflow policies (InboxBlock.Overflow).
const (
	// OverflowReject fails the Send with ErrInboxFull before anything is
	// recorded (the default).
	OverflowReject = "reject"
	// OverflowDropNewest accepts and records the envelope but the full
	// recipient's inbox discards it.
	OverflowDropNewest = "drop_newest"
	// OverflowDropOldest accepts the envelope and has the full recipient's
	// inbox evict its oldest queued envelope.
	OverflowDropOldest = "drop_oldest"
)

// InboxBlock is the inbox capacity policy for one recipient.
//
// The hub keeps a pending counter per recipient (substantive envelopes
// dispatched and not yet acknowledged, see Hub.AckInbox). Only the reject
// policy can be decided at the hub; drop_newest and drop_oldest are applied by
// whoever holds the queue — AgentClient and HumanClient do, a bare Notifier
// must implement them itself.
//
// AGO-DEVIATION vs Python: MaxPending 0 means "no cap" (Python defaults to
// 1000 and treats 0 the same way for the pressure signal); HighWater 0 means
// the default of 80 % of MaxPending and a negative HighWater disables the
// signal (Python distinguishes None from 0); drop_newest and drop_oldest are
// real policies (Python treats both as reject).
type InboxBlock struct {
	MaxPending int    `json:"max_pending,omitempty"`
	Overflow   string `json:"overflow,omitempty"` // OverflowReject (default) | OverflowDropNewest | OverflowDropOldest
	HighWater  int    `json:"high_water,omitempty"`
}

// EffectiveOverflow resolves "" to OverflowReject.
func (b InboxBlock) EffectiveOverflow() string {
	if b.Overflow == "" {
		return OverflowReject
	}
	return b.Overflow
}

// highWaterMark resolves the pressure threshold; 0 = no signal.
func (b InboxBlock) highWaterMark() int {
	switch {
	case b.MaxPending <= 0 || b.HighWater < 0:
		return 0
	case b.HighWater > 0:
		return b.HighWater
	}
	if hw := b.MaxPending * 8 / 10; hw > 1 {
		return hw
	}
	return 1
}

// Validate rejects an unknown overflow policy.
func (b InboxBlock) Validate() error {
	switch b.EffectiveOverflow() {
	case OverflowReject, OverflowDropNewest, OverflowDropOldest:
		return nil
	}
	return fmt.Errorf("%w: unknown inbox overflow policy %q", ErrBadIdentity, b.Overflow)
}

// durationUnits maps ParseDuration suffixes to their length.
var durationUnits = map[byte]time.Duration{
	's': time.Second,
	'm': time.Minute,
	'h': time.Hour,
	'd': 24 * time.Hour,
}

// ParseDuration parses Python-style duration strings: "30s", "15m", "2h",
// "1d", or a plain integer (seconds). An empty string is zero. Unlike
// time.ParseDuration it accepts the day unit and rejects compound forms
// such as "1h30m".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	unit := time.Second
	digits := s
	if u, ok := durationUnits[s[len(s)-1]]; ok {
		unit = u
		digits = s[:len(s)-1]
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrBadDuration, s)
	}
	return time.Duration(n) * unit, nil
}

// TokenBucket is a refilling token bucket over an injectable clock. A bucket
// built from a disabled RateBlock allows everything. Safe for concurrent use.
type TokenBucket struct {
	mu       sync.Mutex
	now      func() time.Time
	perSec   float64
	capacity float64
	tokens   float64
	last     time.Time
	disabled bool
}

// NewTokenBucket builds a full bucket from b. A nil clock uses time.Now.
func NewTokenBucket(b RateBlock, now func() time.Time) *TokenBucket {
	if now == nil {
		now = time.Now
	}
	capacity := float64(b.Burst)
	if capacity <= 0 {
		capacity = float64(b.PerMinute)
	}
	return &TokenBucket{
		now:      now,
		perSec:   float64(b.PerMinute) / 60,
		capacity: capacity,
		tokens:   capacity,
		last:     now(),
		disabled: b.PerMinute <= 0,
	}
}

// Allow takes one token; false when the bucket is empty.
func (tb *TokenBucket) Allow() bool {
	if tb.disabled {
		return true
	}
	tb.mu.Lock()
	defer tb.mu.Unlock()
	t := tb.now()
	if elapsed := t.Sub(tb.last).Seconds(); elapsed > 0 {
		tb.tokens += elapsed * tb.perSec
		if tb.tokens > tb.capacity {
			tb.tokens = tb.capacity
		}
	}
	tb.last = t
	if tb.tokens < 1 {
		return false
	}
	tb.tokens--
	return true
}

// DefaultRule is the permissive rule handed out when none is given: every
// glob is "*", no caps.
func DefaultRule() Rule {
	return Rule{
		InboundGlobs:  []string{"*"},
		OutboundGlobs: []string{"*"},
	}
}

// Clone deep-copies the glob slices so a stored rule never aliases the
// caller's memory.
func (r Rule) Clone() Rule {
	out := r
	if len(r.InboundGlobs) > 0 {
		out.InboundGlobs = append([]string(nil), r.InboundGlobs...)
	}
	if len(r.OutboundGlobs) > 0 {
		out.OutboundGlobs = append([]string(nil), r.OutboundGlobs...)
	}
	return out
}

// Match reports whether s matches any glob in globs. Patterns use
// path.Match syntax; an empty pattern list matches nothing (a zero Rule
// denies everything — pairs with DefaultRule for the permissive default).
func Match(globs []string, s string) bool {
	for _, g := range globs {
		if ok, err := path.Match(g, s); err == nil && ok {
			return true
		}
	}
	return false
}
