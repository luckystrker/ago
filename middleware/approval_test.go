package middleware

import (
	"strings"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

func approveNext(ran *bool) ToolExecFunc {
	return func(ev *events.ToolCallEvent, _ *agocontext.Context) (events.Event, error) {
		*ran = true
		return events.FromResultCall(*ev, events.NewToolResult(false, "ran")), nil
	}
}

// TestApprovalCustomMessage pins message=: a caller template gets the tool name
// and the fenced argument block substituted.
func TestApprovalCustomMessage(t *testing.T) {
	ctx, asked := approvalContext(t, "y")
	m := Approval(WithApprovalMessage("Approve {tool_name}? payload:\n{tool_arguments}"))()
	ran := false
	args := `{"env":"prod"}`
	if _, err := m.OnToolExecution(approveNext(&ran), toolCall("deploy", "tk1", args), ctx); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !ran {
		t.Fatal("approved call did not run")
	}
	want := "Approve deploy? payload:\n" + approvalArgsBlock(args)
	if q := asked(); len(q) != 1 || q[0] != want {
		t.Fatalf("asked = %q, want %q", q, want)
	}
}

// TestApprovalCustomMessageKeepsArgumentsFenced pins the M-16 hardening on the
// custom path too: {tool_arguments} expands to the indented, marked block.
func TestApprovalCustomMessageKeepsArgumentsFenced(t *testing.T) {
	ctx, asked := approvalContext(t, "y")
	m := Approval(WithApprovalMessage("{tool_arguments}"))()
	ran := false
	injected := "line1\nline2"
	if _, err := m.OnToolExecution(approveNext(&ran), toolCall("deploy", "tk1", injected), ctx); err != nil {
		t.Fatalf("call: %v", err)
	}
	for _, line := range strings.Split(asked()[0], "\n") {
		if !strings.HasPrefix(line, approvalArgsIndent) {
			t.Fatalf("payload line %q lost the fence indent", line)
		}
	}
}

// TestApprovalEmptyCustomMessage pins Python parity for message="": an
// explicitly empty template is used as the question, not replaced by the
// built-in one.
func TestApprovalEmptyCustomMessage(t *testing.T) {
	ctx, asked := approvalContext(t, "y")
	m := Approval(WithApprovalMessage(""))()
	ran := false
	if _, err := m.OnToolExecution(approveNext(&ran), toolCall("deploy", "tk1", "{}"), ctx); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !ran {
		t.Fatal("approved call did not run")
	}
	if q := asked(); len(q) != 1 || q[0] != "" {
		t.Fatalf("asked = %q, want one empty question", q)
	}
}

// TestApprovalDeniedMessage pins denied_message=: the refusal the model sees is
// the caller's text, not the default.
func TestApprovalDeniedMessage(t *testing.T) {
	ctx, _ := approvalContext(t, "n")
	m := Approval(WithDeniedMessage("operator said no"))()
	ran := false
	res, err := m.OnToolExecution(approveNext(&ran), toolCall("deploy", "tk1", "{}"), ctx)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if ran {
		t.Fatal("denied call must not run")
	}
	terr, ok := res.(events.ToolErrorEvent)
	if !ok {
		t.Fatalf("result = %T, want ToolErrorEvent", res)
	}
	if terr.Error != "operator said no" {
		t.Fatalf("refusal = %q, want the configured denied message", terr.Error)
	}
}

// TestApprovalAllowAlwaysOff pins allow_always=false: "always" no longer grants
// anything (it denies the call), every call is asked again, and the built-in
// question stops offering the answer.
func TestApprovalAllowAlwaysOff(t *testing.T) {
	ctx, asked := approvalContext(t, "always", "y")
	m := Approval(WithAllowAlways(false))()
	ran := false
	next := approveNext(&ran)

	first := toolCall("deploy", "tk1", "{}")
	res, err := m.OnToolExecution(next, first, ctx)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if ran {
		t.Fatal("with allow_always=false an \"always\" answer must not run the tool")
	}
	if _, ok := res.(events.ToolErrorEvent); !ok {
		t.Fatalf("first result = %T, want ToolErrorEvent", res)
	}

	ran = false
	second := toolCall("deploy", "tk1", "{}")
	if _, err := m.OnToolExecution(next, second, ctx); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !ran {
		t.Fatal("the second call must be asked again and approved by the scripted \"y\"")
	}
	qs := asked()
	if len(qs) != 2 {
		t.Fatalf("asked %d times, want 2 (no grant may survive)", len(qs))
	}
	if strings.Contains(qs[0], "Always") {
		t.Fatalf("question offers Always while allow_always=false: %q", qs[0])
	}
}
