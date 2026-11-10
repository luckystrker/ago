package middleware

import (
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// TestDescribeBuiltins pins kind, config and completeness for every builtin
// (Python parity: each builtin's describe()).
func TestDescribeBuiltins(t *testing.T) {
	tests := []struct {
		name     string
		factory  Factory
		wantKind string
		config   map[string]any
		complete bool
	}{
		{
			name:     "history limiter",
			factory:  HistoryLimiter(7),
			wantKind: "HistoryLimiter",
			config:   map[string]any{"max_events": 7},
			complete: true,
		},
		{
			name:     "token limiter default chars per token",
			factory:  TokenLimiter(1000),
			wantKind: "TokenLimiter",
			config:   map[string]any{"max_tokens": 1000, "chars_per_token": 4},
			complete: true,
		},
		{
			name:     "token limiter configured chars per token",
			factory:  TokenLimiter(1000, WithCharsPerToken(3)),
			wantKind: "TokenLimiter",
			config:   map[string]any{"max_tokens": 1000, "chars_per_token": 3},
			complete: true,
		},
		{
			name:     "retry total attempts",
			factory:  Retry(3),
			wantKind: "RetryMiddleware",
			config:   map[string]any{"attempts": 3},
			complete: true,
		},
		{
			name:     "retry max retries",
			factory:  RetryMaxRetries(3),
			wantKind: "RetryMiddleware",
			config:   map[string]any{"max_retries": 3},
			complete: true,
		},
		{
			name:     "retry with a custom filter is incomplete",
			factory:  Retry(2, WithRetryOn(func(error) bool { return true })),
			wantKind: "RetryMiddleware",
			config:   map[string]any{"attempts": 2, "retry_on": "custom"},
			complete: false,
		},
		{
			name:     "logging",
			factory:  Logging(WithErrTextLimit(64)),
			wantKind: "LoggingMiddleware",
			config:   map[string]any{"logger": "slog.Default", "err_text_limit": 64},
			complete: true,
		},
		{
			name:     "metrics",
			factory:  Metrics(NewMetricsRegistry()),
			wantKind: "MetricsMiddleware",
			config:   map[string]any{"registry": true},
			complete: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DescribeFactory(tt.factory)
			if got.Kind != tt.wantKind {
				t.Fatalf("Kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if got.Complete != tt.complete {
				t.Fatalf("Complete = %v, want %v", got.Complete, tt.complete)
			}
			for k, want := range tt.config {
				if got.Config[k] != want {
					t.Fatalf("config[%q] = %v (%T), want %v (%T)", k, got.Config[k], got.Config[k], want, want)
				}
			}
		})
	}
}

// TestDescribeApproval pins the approval config, including the ternary message
// choice (Python parity).
func TestDescribeApproval(t *testing.T) {
	d := DescribeFactory(Approval())
	if d.Kind != "ApprovalRequired" {
		t.Fatalf("Kind = %q", d.Kind)
	}
	if d.Config["allow_always"] != true || d.Config["denied_message"] != "denied by human" {
		t.Fatalf("config = %v", d.Config)
	}
	if d.Config["timeout"] != float64(0) {
		t.Fatalf("timeout = %v, want 0", d.Config["timeout"])
	}
	if msg, _ := d.Config["message"].(string); msg != approvalPrompt {
		t.Fatalf("message = %q, want the with-Always prompt", msg)
	}

	d = DescribeFactory(Approval(
		WithAllowAlways(false),
		WithDeniedMessage("nope"),
		WithApprovalTimeout(2*time.Second),
		WithArgsScopedAlways(),
		WithApprovalMessage("call {tool_name}"),
	))
	if d.Config["allow_always"] != false || d.Config["denied_message"] != "nope" {
		t.Fatalf("config = %v", d.Config)
	}
	if d.Config["timeout"] != float64(2) || d.Config["args_scoped_always"] != true {
		t.Fatalf("config = %v", d.Config)
	}
	if d.Config["message"] != "call {tool_name}" {
		t.Fatalf("message = %v", d.Config["message"])
	}
}

// TestDescribeStatusbar pins the D-082 fields: persist and the callback source.
func TestDescribeStatusbar(t *testing.T) {
	source := func(*agocontext.Context) ([]events.Input, error) { return nil, nil }
	d := DescribeFactory(Statusbar(source, WithPersist(true)))
	if d.Kind != "statusbar_middleware" {
		t.Fatalf("Kind = %q", d.Kind)
	}
	if d.Config["persist"] != true {
		t.Fatalf("persist = %v", d.Config["persist"])
	}
	if src, _ := d.Config["callback"].(string); src == "" {
		t.Fatal("callback source is empty")
	}

	// A nil callback computes nothing, so its source stays empty.
	if d := DescribeFactory(Statusbar(nil)); d.Config["callback"] != "" {
		t.Fatalf("nil callback source = %v, want empty", d.Config["callback"])
	}
}

// TestDescribeConditionalAndChain pins composite descriptions and completeness
// propagation.
func TestDescribeConditionalAndChain(t *testing.T) {
	cond := events.TypeOf[events.ModelResponse]()
	d := DescribeFactory(If(cond, HistoryLimiter(3)))
	if d.Kind != "ConditionalMiddleware" {
		t.Fatalf("Kind = %q", d.Kind)
	}
	if len(d.Inner) != 1 || d.Inner[0].Kind != "HistoryLimiter" {
		t.Fatalf("inner = %v", d.Inner)
	}
	if d.Complete {
		t.Fatal("a closure condition cannot be named, so the description must be incomplete")
	}

	c := DescribeFactory(Chain(HistoryLimiter(1), TokenLimiter(10)))
	if c.Kind != "chain" || len(c.Inner) != 2 {
		t.Fatalf("chain description = %+v", c)
	}
	if c.Inner[0].Kind != "HistoryLimiter" || c.Inner[1].Kind != "TokenLimiter" {
		t.Fatalf("chain inner = %v", c.Inner)
	}
	if !c.Complete {
		t.Fatal("a chain of complete descriptions must be complete")
	}

	// An incomplete inner makes the composite incomplete.
	incomplete := DescribeFactory(Chain(HistoryLimiter(1), Retry(2, WithRetryOn(func(error) bool { return true }))))
	if incomplete.Complete {
		t.Fatal("chain with an incomplete inner must be incomplete")
	}
}

// TestDescribeFallback pins that a middleware without Describe is still
// describable, marked incomplete.
func TestDescribeFallback(t *testing.T) {
	got := Describe(plainMiddleware{})
	if got.Kind != "plainMiddleware" {
		t.Fatalf("Kind = %q, want plainMiddleware", got.Kind)
	}
	if got.Complete {
		t.Fatal("a middleware without Describe must be incomplete")
	}
	if got := Describe(nil); got.Kind != "Middleware" || got.Complete {
		t.Fatalf("nil description = %+v", got)
	}
}

// plainMiddleware implements Middleware through Base but does not opt in to
// Describable.
type plainMiddleware struct{ Base }
