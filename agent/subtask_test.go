package agent

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
	"github.com/luckystrker/ago/usage"
)

// failingConfig is a model config whose client creation fails (delegation parity:
// a sub-task with a broken config fails at ask time, inside the Task lifecycle).
type failingConfig struct{ err error }

func (f failingConfig) Provider() config.Provider                        { return "test" }
func (f failingConfig) Model() string                                    { return "test" }
func (f failingConfig) Create() (config.LLMClient, error)                { return nil, f.err }
func (f failingConfig) Clone(map[string]any) (config.ModelConfig, error) { return f, nil }

// runToolResultText returns the text of the single tool result part carried by
// the (only) ToolResultsEvent on the stream.
func runToolResultText(t *testing.T, s stream.Stream, name string) string {
	t.Helper()
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	for _, ev := range evs {
		if tre, ok := events.AsValue(ev).(events.ToolResultsEvent); ok {
			for _, r := range tre.Results {
				if r.Name == name {
					return events.RenderForPrompt(r.Result.Parts[0])
				}
			}
		}
	}
	t.Fatalf("no tool result for %q on stream", name)
	return ""
}

// taskEvents collects the Task lifecycle events on a stream, in order.
type taskEvents struct {
	started   []events.TaskStarted
	completed []events.TaskCompleted
	failed    []events.TaskFailed
	usage     []events.UsageEvent
}

func collectTaskEvents(t *testing.T, s stream.Stream) taskEvents {
	t.Helper()
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var out taskEvents
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.TaskStarted:
			out.started = append(out.started, e)
		case events.TaskCompleted:
			out.completed = append(out.completed, e)
		case events.TaskFailed:
			out.failed = append(out.failed, e)
		case events.UsageEvent:
			out.usage = append(out.usage, e)
		}
	}
	return out
}

// TestSubtaskInheritsParentState pins delegation parity (run_task.py): the child
// starts from a copy of the parent's dependencies and variables minus exactly
// the approval bypass key, and the child transcript persists into the parent's
// storage under the child's own stream id.
func TestSubtaskInheritsParentState(t *testing.T) {
	childSaw := ""
	echo, err := tools.NewFunctionTool("echo", "Report what the child context carries",
		func(args struct {
			X string `json:"x"`
		}, cx *agocontext.Context) (string, error) {
			dep, _ := cx.Dependency("dep_key")
			v, _ := cx.Variable("var_key")
			_, bypass := cx.Variable(bypassKey)
			cx.SetVariable("child_var", "from-child")
			childSaw = fmt.Sprintf("dep=%v var=%v bypass=%v", dep, v, bypass)
			return childSaw, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	base := stream.NewMemoryStream()
	parentCfg := agotest.NewTestConfig(
		toolResp("run_subtask", `{"task":"call echo"}`),
		textResp("wrap"),
	)
	childCfg := agotest.NewTestConfig(
		toolResp("echo", `{"x":"1"}`),
		textResp("child final"),
	)
	a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
		WithTasks(TaskConfig{Config: childCfg, ExtraTools: []tools.Tool{echo}}),
		WithDependencies(map[string]any{"dep_key": "dep_value"}),
		WithVariables(map[string]any{"var_key": "var_value", bypassKey: "always-granted"}))
	if _, err := a.Ask(stdcontext.Background(), "delegate please"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if want := "dep=dep_value var=var_value bypass=false"; childSaw != want {
		t.Fatalf("child context = %q, want %q", childSaw, want)
	}

	// The child transcript landed in the parent's storage under its own stream id.
	te := collectTaskEvents(t, base)
	if len(te.completed) != 1 {
		t.Fatalf("completed events: %d", len(te.completed))
	}
	childID := te.completed[0].TaskStream
	if childID == "" || childID == base.ID() {
		t.Fatalf("child stream id %q must be its own (parent %q)", childID, base.ID())
	}
	childEvs, err := base.History().Storage().GetHistory(childID)
	if err != nil {
		t.Fatalf("child history from parent storage: %v", err)
	}
	var sawChildToolResult bool
	for _, ev := range childEvs {
		if tre, ok := events.AsValue(ev).(events.ToolResultsEvent); ok && tre.Results[0].Name == "echo" {
			sawChildToolResult = true
		}
	}
	if !sawChildToolResult {
		t.Fatalf("child transcript missing from parent storage: %d events", len(childEvs))
	}
}

// TestSubtaskVariablesScopedToChild pins the copy semantics: a variable the child
// sets is invisible in the parent's context after the delegation.
func TestSubtaskVariablesScopedToChild(t *testing.T) {
	// The parent's own tool would be inherited by the child too; the child's
	// script never calls it.
	probeSaw := ""
	probe, err := tools.NewFunctionTool("probe_var", "Read the child variable in the parent",
		func(_ struct{}, cx *agocontext.Context) (string, error) {
			v, ok := cx.Variable("child_var")
			probeSaw = fmt.Sprintf("present=%v value=%v", ok, v)
			return probeSaw, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	parentCfg := agotest.NewTestConfig(
		toolResp("run_subtask", `{"task":"set it"}`),
		toolResp("probe_var", "{}"),
		textResp("wrap"),
	)
	// The variable write happens inside the child via its extra tool.
	writer, err := tools.NewFunctionTool("write_var", "Write a variable",
		func(_ struct{}, cx *agocontext.Context) (string, error) {
			cx.SetVariable("child_var", "from-child")
			return "written", nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	childCfg := agotest.NewTestConfig(
		toolResp("write_var", "{}"),
		textResp("child final"),
	)
	a := New("parent", "", WithConfig(parentCfg),
		WithTasks(TaskConfig{Config: childCfg, ExtraTools: []tools.Tool{writer}}),
		WithTools(probe))
	if _, err := a.Ask(stdcontext.Background(), "delegate please"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if probeSaw != "present=false value=<nil>" {
		t.Fatalf("parent saw child variable: %q", probeSaw)
	}
}

// TestSubtaskBridgesHitlToParent pins the delegation HITL bridge: a child
// without its own hook asks through the parent's hook; the question reaches the
// parent stream stamped with the delegation's task id and the answer flows back
// to the child's tool.
func TestSubtaskBridgesHitlToParent(t *testing.T) {
	askHuman, err := tools.NewFunctionTool("ask_human", "Ask the human",
		func(args struct {
			Q string `json:"q"`
		}, cx *agocontext.Context) (string, error) {
			return cx.Input(stdcontext.Background(), args.Q, 5*time.Second)
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	hookGot := ""
	base := stream.NewMemoryStream()
	parentCfg := agotest.NewTestConfig(
		toolResp("run_subtask", `{"task":"ask the human"}`),
		textResp("wrap"),
	)
	childCfg := agotest.NewTestConfig(
		toolResp("ask_human", `{"q":"ping?"}`),
		textResp("child saw pong"),
	)
	a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
		WithTasks(TaskConfig{Config: childCfg, ExtraTools: []tools.Tool{askHuman}}),
		WithHitlHook(func(_ stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
			hookGot = req.Content
			return &events.HumanMessage{Content: "pong"}, nil
		}))
	if _, err := a.Ask(stdcontext.Background(), "delegate please"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if hookGot != "ping?" {
		t.Fatalf("parent hook asked %q, want %q", hookGot, "ping?")
	}
	// The child got the answer.
	te := collectTaskEvents(t, base)
	if len(te.completed) != 1 || te.completed[0].Result != "child saw pong" {
		t.Fatalf("child did not receive the answer: %+v", te.completed)
	}
	// The parent stream carries the question and the answer as ChildEvent
	// observations whose envelopes name the delegation; the request is not
	// re-published as a local trigger, so the answer is seen exactly once.
	started := te.started[0]
	evs, _ := base.History().GetEvents()
	var requestID string
	stamped, answers := false, 0
	for _, ev := range evs {
		if _, local := events.AsValue(ev).(events.HumanInputRequest); local {
			t.Fatal("bridge re-published the question as a local HumanInputRequest")
		}
		ce, ok := events.AsValue(ev).(events.ChildEvent)
		if !ok {
			continue
		}
		switch inner := events.AsValue(ce.Inner).(type) {
		case events.HumanInputRequest:
			if inner.Content == "ping?" {
				stamped = ce.TaskID == started.TaskID && started.TaskID != ""
				requestID = inner.ID
			}
		case events.HumanMessage:
			if inner.ParentID == requestID && inner.Content == "pong" {
				answers++
			}
		}
	}
	if !stamped {
		t.Fatal("forwarded question missing the delegation envelope on the parent stream")
	}
	if answers != 1 {
		t.Fatalf("forwarded answers = %d, want 1", answers)
	}
}

// TestSubtaskHitlRefusedWithoutHook pins the negative bridge path: with no hook
// on either side, a child question ends the parent's turn with a hitl error —
// never a ToolErrorEvent — after the TaskFailed event was emitted.
func TestSubtaskHitlRefusedWithoutHook(t *testing.T) {
	askHuman, err := tools.NewFunctionTool("ask_human", "Ask the human",
		func(args struct {
			Q string `json:"q"`
		}, cx *agocontext.Context) (string, error) {
			return cx.Input(stdcontext.Background(), args.Q, 0)
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	base := stream.NewMemoryStream()
	parentCfg := agotest.NewTestConfig(
		toolResp("run_subtask", `{"task":"ask the human"}`),
		textResp("never reached"),
	)
	childCfg := agotest.NewTestConfig(
		toolResp("ask_human", `{"q":"ping?"}`),
		textResp("never reached either"),
	)
	a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
		WithTasks(TaskConfig{Config: childCfg, ExtraTools: []tools.Tool{askHuman}}))
	_, askErr := a.Ask(stdcontext.Background(), "delegate please")
	if !hitl.IsError(askErr) {
		t.Fatalf("ask error = %v, want a hitl error", askErr)
	}
	// The refusal is not a tool result on either stream.
	evs, _ := base.History().GetEvents()
	for _, ev := range evs {
		if _, ok := events.AsValue(ev).(events.ToolErrorEvent); ok {
			t.Fatal("hitl refusal must not become a ToolErrorEvent")
		}
	}
	// The delegation was still reported as failed before the re-raise.
	te := collectTaskEvents(t, base)
	if len(te.failed) != 1 {
		t.Fatalf("TaskFailed events: %d, want 1", len(te.failed))
	}
	if !strings.Contains(te.failed[0].Error, "no human input hook") {
		t.Fatalf("TaskFailed error: %q", te.failed[0].Error)
	}
}

// TestSubtaskEmitsTaskLifecycle pins the lifecycle reporting on the parent
// stream: TaskStarted before the child turn, TaskCompleted after, matching ids;
// a failing child turn reports TaskFailed with the error text.
func TestSubtaskEmitsTaskLifecycle(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		base := stream.NewMemoryStream()
		parentCfg := agotest.NewTestConfig(
			toolResp("run_subtask", `{"task":"count to two"}`),
			textResp("wrap"),
		)
		childCfg := agotest.NewTestConfig(textResp("child result"))
		a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
			WithTasks(TaskConfig{Config: childCfg}))
		if _, err := a.Ask(stdcontext.Background(), "delegate please"); err != nil {
			t.Fatalf("ask: %v", err)
		}
		te := collectTaskEvents(t, base)
		if len(te.started) != 1 || len(te.completed) != 1 || len(te.failed) != 0 {
			t.Fatalf("lifecycle events: started=%d completed=%d failed=%d",
				len(te.started), len(te.completed), len(te.failed))
		}
		if te.started[0].TaskID == "" || te.started[0].TaskID != te.completed[0].TaskID {
			t.Fatalf("task ids: started %q completed %q", te.started[0].TaskID, te.completed[0].TaskID)
		}
		if te.started[0].AgentName != "subtask" || te.completed[0].AgentName != "subtask" {
			t.Fatalf("agent names: %q / %q", te.started[0].AgentName, te.completed[0].AgentName)
		}
		if te.completed[0].Result != "child result" {
			t.Fatalf("completed result: %q", te.completed[0].Result)
		}
	})
	t.Run("child config failure", func(t *testing.T) {
		base := stream.NewMemoryStream()
		parentCfg := agotest.NewTestConfig(
			toolResp("run_subtask", `{"task":"boom"}`),
			textResp("wrap"),
		)
		a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
			WithTasks(TaskConfig{Config: failingConfig{err: errors.New("boom in create")}}))
		if _, err := a.Ask(stdcontext.Background(), "delegate please"); err != nil {
			t.Fatalf("ask: %v", err)
		}
		te := collectTaskEvents(t, base)
		if len(te.failed) != 1 || len(te.completed) != 0 {
			t.Fatalf("lifecycle events: completed=%d failed=%d", len(te.completed), len(te.failed))
		}
		if !strings.Contains(te.failed[0].Error, "boom in create") {
			t.Fatalf("TaskFailed error: %q", te.failed[0].Error)
		}
	})
}

// TestSubtaskChildErrorIsToolResultText pins the error rendering parity: a child
// failure (here: client creation) reaches the model as the "Error: ..." tool
// result text, not as a ToolErrorEvent; a hitl failure is never rendered as text.
func TestSubtaskChildErrorIsToolResultText(t *testing.T) {
	base := stream.NewMemoryStream()
	parentCfg := agotest.NewTestConfig(
		toolResp("run_subtask", `{"task":"boom"}`),
		textResp("wrap"),
	)
	a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
		WithTasks(TaskConfig{Config: failingConfig{err: errors.New("boom in create")}}))
	if _, err := a.Ask(stdcontext.Background(), "delegate please"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	text := runToolResultText(t, base, "run_subtask")
	if !strings.HasPrefix(text, "Error: ") || !strings.Contains(text, "boom in create") {
		t.Fatalf("tool result text: %q", text)
	}
	evs, _ := base.History().GetEvents()
	for _, ev := range evs {
		if _, ok := events.AsValue(ev).(events.ToolErrorEvent); ok {
			t.Fatal("child failure must not become a ToolErrorEvent")
		}
	}
}

// TestSubtaskSourceUsageCarriesProvenance pins the delegation accounting
// contract: a child's model call reaches the parent as the original source
// UsageEvent (kind model_call, its own provider/model), stamped with the task
// id, agent name and path. There is no subtask rollup to reconcile.
func TestSubtaskSourceUsageCarriesProvenance(t *testing.T) {
	resp := func(content, model string) *events.ModelResponse {
		return &events.ModelResponse{
			Message:  &events.ModelMessage{Content: content},
			Model:    model,
			Provider: "testprov",
			Usage:    events.Usage{PromptTokens: events.I64(1), TotalTokens: events.I64(1)},
		}
	}
	base := stream.NewMemoryStream()
	parentCfg := agotest.NewTestConfig(
		toolResp("run_subtask", `{"task":"go"}`),
		textResp("wrap"),
	)
	childCfg := agotest.NewTestConfig(resp("child result", "m"))
	a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
		WithTasks(TaskConfig{Config: childCfg}))
	if _, err := a.Ask(stdcontext.Background(), "delegate please"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	te := collectTaskEvents(t, base)
	var childUsage []events.UsageEvent
	for _, u := range te.usage {
		if u.AgentName != "" {
			childUsage = append(childUsage, u)
		}
	}
	if len(childUsage) != 1 {
		t.Fatalf("forwarded usage events: %d, want 1 (no rollup)", len(childUsage))
	}
	ue := childUsage[0]
	if ue.Kind != events.UsageKindModelCall {
		t.Fatalf("kind = %q, want %q", ue.Kind, events.UsageKindModelCall)
	}
	if ue.Model != "m" || ue.Provider != "testprov" {
		t.Fatalf("attribution: model=%q provider=%q", ue.Model, ue.Provider)
	}
	if ue.AgentName != "subtask" || ue.TaskID == "" || len(ue.Path) != 1 || ue.Path[0] != ue.TaskID {
		t.Fatalf("provenance: agent=%q task=%q path=%v", ue.AgentName, ue.TaskID, ue.Path)
	}
	if te.usage[0].Kind == events.UsageKindSubtask {
		t.Fatal("a subtask rollup must not be emitted for source accounting")
	}
}

// TestSubtaskSourceUsageMixedAndSequential pins that each model call keeps its
// own provider/model pair and delegation identity: no rollup flattens or
// unlabels a mixed-model delegation, and sequential delegations stay separate.
func TestSubtaskSourceUsageMixedAndSequential(t *testing.T) {
	resp := func(content, model string) *events.ModelResponse {
		return &events.ModelResponse{
			Message:  &events.ModelMessage{Content: content},
			Model:    model,
			Provider: "testprov",
			Usage:    events.Usage{PromptTokens: events.I64(1), TotalTokens: events.I64(1)},
		}
	}
	t.Run("mixed pairs keep their models", func(t *testing.T) {
		base := stream.NewMemoryStream()
		parentCfg := agotest.NewTestConfig(
			toolResp("run_subtask", `{"task":"go"}`),
			textResp("wrap"),
		)
		echo, err := tools.NewFunctionTool("echo", "echo",
			func(struct{}, *agocontext.Context) (string, error) { return "echoed", nil })
		if err != nil {
			t.Fatalf("tool: %v", err)
		}
		childCfg := agotest.NewTestConfig(
			withModel(toolResp("echo", `{"x":"1"}`), "m1"),
			resp("child result", "m2"),
		)
		a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
			WithTasks(TaskConfig{Config: childCfg, ExtraTools: []tools.Tool{echo}}))
		if _, err := a.Ask(stdcontext.Background(), "delegate please"); err != nil {
			t.Fatalf("ask: %v", err)
		}
		models := map[string]bool{}
		for _, u := range collectTaskEvents(t, base).usage {
			if u.AgentName == "" {
				continue
			}
			if u.Kind == events.UsageKindSubtask {
				t.Fatal("subtask rollup emitted")
			}
			models[u.Model] = true
		}
		if len(models) != 2 || !models["m1"] || !models["m2"] {
			t.Fatalf("forwarded models = %v, want m1 and m2", models)
		}
	})
	t.Run("sequential delegations are separate publications", func(t *testing.T) {
		base := stream.NewMemoryStream()
		parentCfg := agotest.NewTestConfig(
			toolResp("run_subtasks", `{"tasks":["a","b"],"parallel":false}`),
			textResp("wrap"),
		)
		childCfg := agotest.NewTestConfig(resp("from m1", "m1"), resp("from m2", "m2"))
		a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
			WithTasks(TaskConfig{Config: childCfg}))
		if _, err := a.Ask(stdcontext.Background(), "delegate please"); err != nil {
			t.Fatalf("ask: %v", err)
		}
		var got []events.UsageEvent
		for _, u := range collectTaskEvents(t, base).usage {
			if u.AgentName != "" {
				got = append(got, u)
			}
		}
		if len(got) != 2 {
			t.Fatalf("forwarded usage events: %d, want 2", len(got))
		}
		if got[0].Model != "m1" || got[1].Model != "m2" {
			t.Fatalf("per-invocation models: %q / %q", got[0].Model, got[1].Model)
		}
		if got[0].AgentName != "subtask-0" || got[1].AgentName != "subtask-1" {
			t.Fatalf("agent names: %q / %q", got[0].AgentName, got[1].AgentName)
		}
		if got[0].TaskID == got[1].TaskID || got[0].Path[0] != got[0].TaskID || got[1].Path[0] != got[1].TaskID {
			t.Fatalf("delegation identities collided: %+v / %+v", got[0], got[1])
		}
	})
}

// withModel stamps a model name onto a scripted response (the per-call
// UsageEvent carries it onto the wire, model.go tracing fields).
func withModel(r *events.ModelResponse, model string) *events.ModelResponse {
	r.Model = model
	return r
}

// TestSubtaskNestedUsageKeepsLeafProvenance pins the nested tree: each source
// call reaches the lead once, with the delegation path that names the hops it
// travelled (one id for the child's own calls, two for the grandchild's).
func TestSubtaskNestedUsageKeepsLeafProvenance(t *testing.T) {
	outerResp := func(content, model string) *events.ModelResponse {
		return &events.ModelResponse{
			Message:  &events.ModelMessage{Content: content},
			Model:    model,
			Provider: "testprov",
			Usage:    events.Usage{PromptTokens: events.I64(1), TotalTokens: events.I64(1)},
		}
	}
	base := stream.NewMemoryStream()
	parentCfg := agotest.NewTestConfig(
		withModel(toolResp("task_child", `{"objective":"nest"}`), "outer-m"),
		textResp("wrap"),
	)
	grandchildCfg := agotest.NewTestConfig(outerResp("grand result", "grand-m"))
	childCfg := agotest.NewTestConfig(
		withModel(toolResp("run_subtask", `{"task":"go deeper"}`), "inner-m1"),
		outerResp("child result", "inner-m2"),
	)
	child := New("child", "", WithConfig(childCfg), WithTasks(TaskConfig{Config: grandchildCfg}))
	a := New("lead", "", WithConfig(parentCfg), WithStreamBase(base), WithTools(child.AsTool()))
	if _, err := a.Ask(stdcontext.Background(), "delegate please"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	var forwarded []events.UsageEvent
	for _, u := range collectTaskEvents(t, base).usage {
		if len(u.Path) > 0 {
			forwarded = append(forwarded, u)
		}
	}
	if len(forwarded) != 3 {
		t.Fatalf("forwarded usage events: %d, want 3 (child's two calls and the grandchild)", len(forwarded))
	}
	models := map[string]int{}
	for _, u := range forwarded {
		if u.Kind != events.UsageKindModelCall {
			t.Fatalf("kind = %q, want model_call", u.Kind)
		}
		models[u.Model]++
	}
	if models["inner-m1"] != 1 || models["inner-m2"] != 1 || models["grand-m"] != 1 {
		t.Fatalf("forwarded models = %v", models)
	}
	if models["outer-m"] != 0 {
		t.Fatal("the lead's own call must not be forwarded")
	}
	for _, u := range forwarded {
		want := 1
		if u.Model == "grand-m" {
			want = 2
		}
		if len(u.Path) != want {
			t.Fatalf("%s path = %v, want %d hops", u.Model, u.Path, want)
		}
	}
}

// TestSubtaskCustomChildPrompt pins the TaskConfig.Prompt wiring: the child
// agent is built with the configured prompt, not silently replaced by the
// default task prompt (Python parity: TaskConfig.prompt).
func TestSubtaskCustomChildPrompt(t *testing.T) {
	spy := &promptSpy{}
	parentCfg := agotest.NewTestConfig(
		toolResp("run_subtask", `{"task":"go"}`),
		textResp("wrap"),
	)
	a := New("parent", "", WithConfig(parentCfg),
		WithTasks(TaskConfig{Config: spy, Prompt: "CUSTOM-TASK-PROMPT"}))
	if _, err := a.Ask(stdcontext.Background(), "delegate"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	got := spy.recorded()
	if len(got) == 0 {
		t.Fatal("the child never reached its model call")
	}
	p := strings.Join(got[0], ",")
	if !strings.Contains(p, "CUSTOM-TASK-PROMPT") || strings.Contains(p, defaultTaskPrompt) {
		t.Fatalf("child system prompt = [%s], want the configured prompt, not the default", p)
	}
}

// TestSubtasksParallelHitlErrorPropagates pins the fan-out hitl semantics: when
// a parallel run_subtasks sibling cannot reach a human, the whole call ends
// with the hitl error (it must not degrade to "Error: ..." text) after both
// delegations reported their TaskFailed (Python parity: gather everything,
// then re-raise).
func TestSubtasksParallelHitlErrorPropagates(t *testing.T) {
	askHuman, err := tools.NewFunctionTool("ask_human", "Ask the human",
		func(args struct {
			Q string `json:"q"`
		}, cx *agocontext.Context) (string, error) {
			return cx.Input(stdcontext.Background(), args.Q, 0)
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	base := stream.NewMemoryStream()
	parentCfg := agotest.NewTestConfig(
		toolResp("run_subtasks", `{"tasks":["one","two"],"parallel":true}`),
		textResp("never reached"),
	)
	childCfg := freshScriptConfig{script: []*events.ModelResponse{
		toolResp("ask_human", `{"q":"ping?"}`),
		textResp("saw pong"),
	}}
	a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
		WithTasks(TaskConfig{Config: childCfg, ExtraTools: []tools.Tool{askHuman}}))
	_, askErr := a.Ask(stdcontext.Background(), "fan out")
	if !hitl.IsError(askErr) {
		t.Fatalf("ask error = %v, want a hitl error ending the turn", askErr)
	}
	te := collectTaskEvents(t, base)
	if len(te.failed) != 2 {
		t.Fatalf("TaskFailed events = %d, want 2 (both siblings report before the re-raise)", len(te.failed))
	}
}

// TestSubtaskToolFilters pins the allowlist semantics (Python
// _filter_subtask_tools): nil IncludeTools inherits every parent tool, an empty
// allowlist inherits none, and ExcludeTools applies after the allowlist.
func TestSubtaskToolFilters(t *testing.T) {
	echoFor := func(ran *bool) tools.Tool {
		tool, err := tools.NewFunctionTool("echo", "Echo the argument",
			func(args struct {
				X string `json:"x"`
			}, _ *agocontext.Context) (string, error) {
				*ran = true
				return "echoed " + args.X, nil
			})
		if err != nil {
			t.Fatalf("tool: %v", err)
		}
		return tool
	}
	// The scripted configs carry a cursor, so every case builds its own.
	childEcho := func() TaskConfig {
		return TaskConfig{Config: agotest.NewTestConfig(
			toolResp("echo", `{"x":"1"}`),
			textResp("child done"),
		)}
	}
	childNoTool := func() TaskConfig {
		return TaskConfig{Config: agotest.NewTestConfig()} // exhausted: the child cannot run
	}

	cases := []struct {
		name    string
		tc      TaskConfig
		wantRun bool
		wantErr bool
	}{
		{"nil inherits all", childEcho(), true, false},
		{"empty allowlist inherits none", withInclude(childNoTool(), []string{}), false, true},
		{"exclude after nil allowlist", childNoToolWithExclude(), false, true},
		{"allowlist passes echo", withInclude(childEcho(), []string{"echo"}), true, false},
		// A child that calls the excluded tool must find it gone: the filter,
		// not the tool, decides. The child still finishes (a missing tool is a
		// not-found result, not a turn failure) and never runs echo.
		{"exclude removes echo from a calling child", withExclude(childEcho(), []string{"echo"}), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			base := stream.NewMemoryStream()
			parentCfg := agotest.NewTestConfig(
				toolResp("run_subtask", `{"task":"call echo"}`),
				textResp("wrap"),
			)
			a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
				WithTools(echoFor(&ran)), WithTasks(tc.tc))
			if _, err := a.Ask(stdcontext.Background(), "delegate please"); err != nil {
				t.Fatalf("ask: %v", err)
			}
			if ran != tc.wantRun {
				t.Fatalf("echo ran = %v, want %v", ran, tc.wantRun)
			}
			text := runToolResultText(t, base, "run_subtask")
			if tc.wantErr && !strings.HasPrefix(text, "Error: ") {
				t.Fatalf("tool result text: %q, want an Error: rendering", text)
			}
			if !tc.wantErr && strings.HasPrefix(text, "Error: ") {
				t.Fatalf("tool result text: %q, want the child answer", text)
			}
		})
	}
}

func withInclude(tc TaskConfig, include []string) TaskConfig {
	tc.IncludeTools = include
	return tc
}

func withExclude(tc TaskConfig, exclude []string) TaskConfig {
	tc.ExcludeTools = exclude
	return tc
}

func childNoToolWithExclude() TaskConfig {
	return TaskConfig{
		Config:       agotest.NewTestConfig(), // exhausted: the child cannot run
		ExcludeTools: []string{"echo"},
	}
}

// TestSubtaskToolSurface pins the delegating tools' wire surface (Python parity:
// agent.py run_subtask / run_subtasks): names, exact descriptions, the `task`
// parameter, the optional `parallel` flag, the "## task" output format with the
// "Error: ..." rendering per failed sub-task, and parallel as the default.
func TestSubtaskToolSurface(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("unused"))
	a := New("parent", "", WithConfig(cfg), WithTasks(TaskConfig{}))
	a.mu.Lock()
	opts := a.opts
	a.mu.Unlock()
	run := &AgentRun{agent: a}
	ts := run.subtaskTools(opts)
	if len(ts) != 2 {
		t.Fatalf("subtask tools: %d", len(ts))
	}

	// run_subtask: exact description, `task` required, no other parameters.
	if ts[0].Name() != "run_subtask" {
		t.Fatalf("tool name: %q", ts[0].Name())
	}
	subSchema, err := ts[0].Schemas(nil)
	if err != nil {
		t.Fatalf("run_subtask schema: %v", err)
	}
	assertSchema(t, subSchema[0], "run_subtask", runSubtaskDescription,
		map[string]bool{"task": true})

	// run_subtasks: exact description, `tasks` required, `parallel` optional.
	if ts[1].Name() != "run_subtasks" {
		t.Fatalf("tool name: %q", ts[1].Name())
	}
	subSchema, err = ts[1].Schemas(nil)
	if err != nil {
		t.Fatalf("run_subtasks schema: %v", err)
	}
	assertSchema(t, subSchema[0], "run_subtasks", runSubtasksDescription,
		map[string]bool{"tasks": true, "parallel": false})

	// Output format: "## {task}\n\n{result}" joined by "\n\n---\n\n".
	t.Run("format", func(t *testing.T) {
		base := stream.NewMemoryStream()
		parentCfg := agotest.NewTestConfig(
			toolResp("run_subtasks", `{"tasks":["alpha","beta"],"parallel":false}`),
			textResp("wrap"),
		)
		childCfg := agotest.NewTestConfig(textResp("resA"), textResp("resB"))
		p := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
			WithTasks(TaskConfig{Config: childCfg}))
		if _, err := p.Ask(stdcontext.Background(), "fan out"); err != nil {
			t.Fatalf("ask: %v", err)
		}
		want := "## alpha\n\nresA\n\n---\n\n## beta\n\nresB"
		if got := runToolResultText(t, base, "run_subtasks"); got != want {
			t.Fatalf("combined output:\n%q\nwant:\n%q", got, want)
		}
	})
	// A failed sub-task renders as "Error: ..." in its section; the others run.
	t.Run("per task error", func(t *testing.T) {
		base := stream.NewMemoryStream()
		parentCfg := agotest.NewTestConfig(
			toolResp("run_subtasks", `{"tasks":["alpha","beta"],"parallel":false}`),
			textResp("wrap"),
		)
		childCfg := agotest.NewTestConfig(textResp("resA")) // beta exhausts the script
		p := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
			WithTasks(TaskConfig{Config: childCfg}))
		if _, err := p.Ask(stdcontext.Background(), "fan out"); err != nil {
			t.Fatalf("ask: %v", err)
		}
		got := runToolResultText(t, base, "run_subtasks")
		if !strings.HasPrefix(got, "## alpha\n\nresA\n\n---\n\n## beta\n\nError: ") {
			t.Fatalf("combined output: %q", got)
		}
	})
	// parallel defaults to true when the model omits it.
	t.Run("parallel default", func(t *testing.T) {
		base := stream.NewMemoryStream()
		parentCfg := agotest.NewTestConfig(
			toolResp("run_subtasks", `{"tasks":["solo"]}`),
			textResp("wrap"),
		)
		childCfg := agotest.NewTestConfig(textResp("resSolo"))
		p := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
			WithTasks(TaskConfig{Config: childCfg}))
		if _, err := p.Ask(stdcontext.Background(), "fan out"); err != nil {
			t.Fatalf("ask: %v", err)
		}
		if got := runToolResultText(t, base, "run_subtasks"); got != "## solo\n\nresSolo" {
			t.Fatalf("combined output: %q", got)
		}
	})
}

// assertSchema checks a tool schema: the exact description and the expected
// properties with their required flag.
func assertSchema(t *testing.T, schema any, name, description string, props map[string]bool) {
	t.Helper()
	b, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var m struct {
		Description string `json:"description"`
		Parameters  struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if m.Description != description {
		t.Fatalf("%s description = %q, want %q", name, m.Description, description)
	}
	gotRequired := map[string]bool{}
	for _, r := range m.Parameters.Required {
		gotRequired[r] = true
	}
	for p, required := range props {
		if _, ok := m.Parameters.Properties[p]; !ok {
			t.Fatalf("%s schema missing property %q", name, p)
		}
		if gotRequired[p] != required {
			t.Fatalf("%s property %q required = %v, want %v", name, p, gotRequired[p], required)
		}
	}
	for p := range m.Parameters.Properties {
		if _, ok := props[p]; !ok {
			t.Fatalf("%s schema has unexpected property %q", name, p)
		}
	}
}

// TestAsToolSurface pins the AsTool wire surface (Python parity:
// subagent_tool.py): the default name is task_<agent>, the schema is
// objective + optional context, the delegation is reported on the caller's
// stream, and a failed sub-task renders as text, not as a ToolErrorEvent.
func TestAsToolSurface(t *testing.T) {
	t.Run("name and schema", func(t *testing.T) {
		child := New("db pro", "", WithConfig(agotest.NewTestConfig(textResp("x"))))
		tool := child.AsTool()
		if tool.Name() != "task_db_pro" {
			t.Fatalf("default tool name: %q", tool.Name())
		}
		schemas, err := tool.Schemas(nil)
		if err != nil {
			t.Fatalf("schemas: %v", err)
		}
		assertSchema(t, schemas[0], "task_db_pro", "Delegate to the agent db pro.",
			map[string]bool{"objective": true, "context": false})
	})
	t.Run("lifecycle on caller stream", func(t *testing.T) {
		base := stream.NewMemoryStream()
		childCfg := agotest.NewTestConfig(textResp("specialist answer"))
		child := New("specialist", "", WithConfig(childCfg))
		parentCfg := agotest.NewTestConfig(
			toolResp("task_specialist", `{"objective":"advise"}`),
			textResp("wrap"),
		)
		parent := New("lead", "", WithConfig(parentCfg), WithStreamBase(base), WithTools(child.AsTool()))
		if _, err := parent.Ask(stdcontext.Background(), "consult"); err != nil {
			t.Fatalf("ask: %v", err)
		}
		te := collectTaskEvents(t, base)
		if len(te.started) != 1 || len(te.completed) != 1 {
			t.Fatalf("lifecycle events: started=%d completed=%d failed=%d",
				len(te.started), len(te.completed), len(te.failed))
		}
		if te.started[0].AgentName != "specialist" || te.completed[0].Result != "specialist answer" {
			t.Fatalf("started: %+v completed: %+v", te.started[0], te.completed[0])
		}
	})
	t.Run("failure is text", func(t *testing.T) {
		base := stream.NewMemoryStream()
		child := New("specialist", "", WithConfig(failingConfig{err: errors.New("boom in create")}))
		parentCfg := agotest.NewTestConfig(
			toolResp("task_specialist", `{"objective":"advise"}`),
			textResp("wrap"),
		)
		parent := New("lead", "", WithConfig(parentCfg), WithStreamBase(base), WithTools(child.AsTool()))
		if _, err := parent.Ask(stdcontext.Background(), "consult"); err != nil {
			t.Fatalf("ask: %v", err)
		}
		text := runToolResultText(t, base, "task_specialist")
		if !strings.HasPrefix(text, "Sub-task 'specialist' failed: ") {
			t.Fatalf("tool result text: %q", text)
		}
		evs, _ := base.History().GetEvents()
		for _, ev := range evs {
			if _, ok := events.AsValue(ev).(events.ToolErrorEvent); ok {
				t.Fatal("sub-task failure must not become a ToolErrorEvent")
			}
		}
	})
}

// echoConfig answers every call with "done <objective>", reading the objective
// off the child's initial ModelRequest. Concurrent subtasks therefore produce
// distinguishable results with no script to share (agotest.TestConfig hands
// every client the same cursor, which parallel children would consume in
// nondeterministic order).
type echoConfig struct{}

func (echoConfig) Provider() config.Provider { return "test" }
func (echoConfig) Model() string             { return "test" }
func (echoConfig) Create() (config.LLMClient, error) {
	return echoClient{}, nil
}
func (echoConfig) Clone(map[string]any) (config.ModelConfig, error) { return echoConfig{}, nil }

type echoClient struct{}

var _ config.LLMClient = echoClient{}

func (echoClient) Call(_ stdcontext.Context, messages []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	for _, ev := range messages {
		if m, ok := events.AsValue(ev).(events.ModelRequest); ok {
			return &events.ModelResponse{
				Message: &events.ModelMessage{Content: "done " + strings.TrimPrefix(events.RenderForPrompt(m), "User: ")},
			}, nil
		}
	}
	return &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}}, nil
}

// TestAsToolChildKeepsOwnState pins the delegation state merge (Python parity:
// __build_context's `self._agent_dependencies | dependencies`): a child handed
// over AsTool keeps its own WithDependencies/WithVariables, the parent's
// snapshot wins on shared keys, and the approval bypass key is not inherited
// from the parent. Replacing the child's maps with the parent's snapshot — the
// pre-fix behavior — would strip its tools of their injected state.
func TestAsToolChildKeepsOwnState(t *testing.T) {
	childSaw := ""
	report, err := tools.NewFunctionTool("report_state", "Report the child context state",
		func(_ struct{}, cx *agocontext.Context) (string, error) {
			own, _ := cx.Dependency("own_dep")
			shared, _ := cx.Dependency("shared_dep")
			parentOnly, _ := cx.Dependency("parent_dep")
			vOwn, _ := cx.Variable("own_var")
			vShared, _ := cx.Variable("shared_var")
			_, bypass := cx.Variable(bypassKey)
			childSaw = fmt.Sprintf("own=%v shared=%v parent=%v vown=%v vshared=%v bypass=%v",
				own, shared, parentOnly, vOwn, vShared, bypass)
			return childSaw, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	base := stream.NewMemoryStream()
	childCfg := agotest.NewTestConfig(
		toolResp("report_state", "{}"),
		textResp("child done"),
	)
	child := New("specialist", "", WithConfig(childCfg), WithTools(report),
		WithDependencies(map[string]any{"own_dep": "child-own", "shared_dep": "from-child"}),
		WithVariables(map[string]any{"own_var": "child-var", "shared_var": "from-child-var"}))
	parentCfg := agotest.NewTestConfig(
		toolResp("task_specialist", `{"objective":"advise"}`),
		textResp("wrap"),
	)
	parent := New("lead", "", WithConfig(parentCfg), WithStreamBase(base), WithTools(child.AsTool()),
		WithDependencies(map[string]any{"shared_dep": "from-parent", "parent_dep": "parent-only"}),
		WithVariables(map[string]any{"shared_var": "from-parent-var", bypassKey: "always-granted"}))
	if _, err := parent.Ask(stdcontext.Background(), "consult"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	want := "own=child-own shared=from-parent parent=parent-only " +
		"vown=child-var vshared=from-parent-var bypass=false"
	if childSaw != want {
		t.Fatalf("child context = %q, want %q", childSaw, want)
	}
}

// TestSubtaskSourceUsageKeepsPerCallTotals pins that source accounting is not
// flattened: each model call keeps its own usage (including an absent
// total_tokens), and the aggregate report is the field-wise sum of those
// source records, with no subtask rollup to void or re-label them.
func TestSubtaskSourceUsageKeepsPerCallTotals(t *testing.T) {
	withUsage := func(content string, prompt, total *int64) *events.ModelResponse {
		return &events.ModelResponse{
			Message: &events.ModelMessage{Content: content},
			Usage:   events.Usage{PromptTokens: prompt, TotalTokens: total},
		}
	}
	callWithUsage := func(prompt, total *int64) *events.ModelResponse {
		return &events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
				ID: "call-echo", Name: "echo", Arguments: `{}`,
			}}},
			Usage: events.Usage{PromptTokens: prompt, TotalTokens: total},
		}
	}
	echo, err := tools.NewFunctionTool("echo", "echo",
		func(struct{}, *agocontext.Context) (string, error) { return "echoed", nil })
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	base := stream.NewMemoryStream()
	parentCfg := agotest.NewTestConfig(
		toolResp("run_subtask", `{"task":"go"}`),
		textResp("wrap"),
	)
	childCfg := agotest.NewTestConfig(
		callWithUsage(events.I64(2), events.I64(5)),
		withUsage("child final", events.I64(3), nil),
	)
	a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
		WithTasks(TaskConfig{Config: childCfg, ExtraTools: []tools.Tool{echo}}))
	if _, err := a.Ask(stdcontext.Background(), "delegate please"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	var got []events.UsageEvent
	for _, u := range collectTaskEvents(t, base).usage {
		if u.AgentName != "" {
			got = append(got, u)
		}
	}
	if len(got) != 2 {
		t.Fatalf("forwarded usage events: %d, want 2", len(got))
	}
	if got[0].Usage.TotalTokens == nil || *got[0].Usage.TotalTokens != 5 {
		t.Fatalf("first call total_tokens = %v, want 5", got[0].Usage.TotalTokens)
	}
	if got[1].Usage.TotalTokens != nil {
		t.Fatalf("second call total_tokens = %v, want absent", *got[1].Usage.TotalTokens)
	}
	for _, u := range got {
		if u.Kind != events.UsageKindModelCall {
			t.Fatalf("kind = %q, want model_call", u.Kind)
		}
	}
	childEvents := []events.Event{&got[0], &got[1]}
	report := usage.FromEvents(childEvents)
	if report.Total.PromptTokens == nil || *report.Total.PromptTokens != 5 {
		t.Fatalf("report prompt_tokens = %v, want 5", report.Total.PromptTokens)
	}
}

// freshScriptConfig hands every client its own replay of the same script from
// the start (Python TestConfig.create() parity: an independent script iterator
// per client). Parallel subtasks share one TaskConfig, so a shared cursor would
// hand out scripted responses in nondeterministic order across children.
type freshScriptConfig struct {
	script []*events.ModelResponse
}

func (c freshScriptConfig) Provider() config.Provider { return "test" }
func (c freshScriptConfig) Model() string             { return "test" }
func (c freshScriptConfig) Create() (config.LLMClient, error) {
	return &freshScriptClient{script: c.script}, nil
}
func (c freshScriptConfig) Clone(map[string]any) (config.ModelConfig, error) { return c, nil }

type freshScriptClient struct {
	script []*events.ModelResponse
	next   int
}

var _ config.LLMClient = (*freshScriptClient)(nil)

func (c *freshScriptClient) Call(_ stdcontext.Context, _ []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	if c.next >= len(c.script) {
		return nil, fmt.Errorf("freshScriptConfig: scripted responses exhausted (call %d)", c.next+1)
	}
	resp := c.script[c.next]
	c.next++
	return resp, nil
}

// TestSubtasksRunParallel pins the concurrent fan-out: four parallel subtasks
// under MaxConcurrency=2 all report their lifecycle on the parent stream with
// distinct matching ids, and the combined tool output renders each result under
// its own task in task order — the child echoes its objective, so a result
// routed to the wrong section cannot hide behind identical texts.
func TestSubtasksRunParallel(t *testing.T) {
	tasks := []string{"alpha", "beta", "gamma", "delta"}
	start, err := json.Marshal(tasks)
	if err != nil {
		t.Fatalf("marshal tasks: %v", err)
	}
	base := stream.NewMemoryStream()
	parentCfg := agotest.NewTestConfig(
		toolResp("run_subtasks", fmt.Sprintf(`{"tasks":%s,"parallel":true}`, start)),
		textResp("wrap"),
	)
	a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
		WithTasks(TaskConfig{Config: echoConfig{}, MaxConcurrency: 2}))
	if _, err := a.Ask(stdcontext.Background(), "fan out"); err != nil {
		t.Fatalf("ask: %v", err)
	}

	// The combined output: sections in task order, each carrying that task's own
	// child answer.
	sections := make([]string, len(tasks))
	for i, task := range tasks {
		sections[i] = fmt.Sprintf("## %s\n\ndone %s", task, task)
	}
	want := strings.Join(sections, "\n\n---\n\n")
	if got := runToolResultText(t, base, "run_subtasks"); got != want {
		t.Fatalf("combined output:\n%q\nwant:\n%q", got, want)
	}

	te := collectTaskEvents(t, base)
	if len(te.started) != 4 || len(te.completed) != 4 || len(te.failed) != 0 {
		t.Fatalf("lifecycle events: started=%d completed=%d failed=%d",
			len(te.started), len(te.completed), len(te.failed))
	}
	objectives := map[string]string{} // task id -> objective
	names := map[string]bool{}
	for _, s := range te.started {
		if s.TaskID == "" {
			t.Fatal("TaskStarted without a task id")
		}
		if _, dup := objectives[s.TaskID]; dup {
			t.Fatalf("duplicate task id %q", s.TaskID)
		}
		objectives[s.TaskID] = s.Objective
		names[s.AgentName] = true
	}
	for i := range tasks {
		if !names[fmt.Sprintf("subtask-%d", i)] {
			t.Fatalf("no TaskStarted for subtask-%d: %v", i, names)
		}
	}
	for _, c := range te.completed {
		obj, ok := objectives[c.TaskID]
		if !ok {
			t.Fatalf("TaskCompleted %q without a matching TaskStarted", c.TaskID)
		}
		if c.Result != "done "+obj {
			t.Fatalf("TaskCompleted for %q carried result %q, want %q", obj, c.Result, "done "+obj)
		}
	}
}

// TestSubtasksBridgeHitlInParallel pins the HITL bridge under fan-out: two
// parallel subtasks both ask through the parent's hook — the hook answers each
// delegation exactly once — and both complete with the answer.
func TestSubtasksBridgeHitlInParallel(t *testing.T) {
	askHuman, err := tools.NewFunctionTool("ask_human", "Ask the human",
		func(args struct {
			Q string `json:"q"`
		}, cx *agocontext.Context) (string, error) {
			return cx.Input(stdcontext.Background(), args.Q, 5*time.Second)
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	var (
		mu    sync.Mutex
		hooks int
	)
	base := stream.NewMemoryStream()
	parentCfg := agotest.NewTestConfig(
		toolResp("run_subtasks", `{"tasks":["one","two"],"parallel":true}`),
		textResp("wrap"),
	)
	childCfg := freshScriptConfig{script: []*events.ModelResponse{
		toolResp("ask_human", `{"q":"ping?"}`),
		textResp("saw pong"),
	}}
	a := New("parent", "", WithConfig(parentCfg), WithStreamBase(base),
		WithTasks(TaskConfig{Config: childCfg, ExtraTools: []tools.Tool{askHuman}}),
		WithHitlHook(func(_ stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
			mu.Lock()
			hooks++
			mu.Unlock()
			return &events.HumanMessage{Content: "pong"}, nil
		}))
	if _, err := a.Ask(stdcontext.Background(), "fan out"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if hooks != 2 {
		t.Fatalf("parent hook calls = %d, want 2", hooks)
	}

	te := collectTaskEvents(t, base)
	if len(te.started) != 2 || len(te.completed) != 2 || len(te.failed) != 0 {
		t.Fatalf("lifecycle events: started=%d completed=%d failed=%d",
			len(te.started), len(te.completed), len(te.failed))
	}
	ids := map[string]bool{}
	for _, s := range te.started {
		ids[s.TaskID] = true
	}
	for _, c := range te.completed {
		if !ids[c.TaskID] {
			t.Fatalf("TaskCompleted %q without a matching TaskStarted", c.TaskID)
		}
		if c.Result != "saw pong" {
			t.Fatalf("TaskCompleted result %q, want the bridged answer", c.Result)
		}
	}
	// Both forwarded questions reached the parent stream, each stamped with its
	// own delegation's task id.
	evs, err := base.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	stamped := map[string]bool{}
	for _, ev := range evs {
		ce, ok := events.AsValue(ev).(events.ChildEvent)
		if !ok {
			continue
		}
		if hr, ok := events.AsValue(ce.Inner).(events.HumanInputRequest); ok && hr.Content == "ping?" {
			if ce.TaskID == "" || !ids[ce.TaskID] {
				t.Fatalf("forwarded question stamped with unknown task id %q", ce.TaskID)
			}
			stamped[ce.TaskID] = true
		}
	}
	if len(stamped) != 2 {
		t.Fatalf("forwarded questions: %d distinct task ids, want 2", len(stamped))
	}
}

// TestAsToolPersistentStreamForwardsOnlyThisInvocation pins the persistent
// stream contract after source accounting: a reused child stream does not
// re-forward earlier spend, so the parent sees each invocation's UsageEvents
// once, while TaskCompleted.Usage remains the stream's cumulative snapshot.
func TestAsToolPersistentStreamForwardsOnlyThisInvocation(t *testing.T) {
	resp := func(content string, prompt int64) *events.ModelResponse {
		return &events.ModelResponse{
			Message:  &events.ModelMessage{Content: content},
			Model:    "m",
			Provider: "testprov",
			Usage:    events.Usage{PromptTokens: events.I64(prompt), TotalTokens: events.I64(prompt)},
		}
	}
	// The child keeps one stream across delegations (WithStreamBase); the shared
	// script cursor is deterministic here because the delegations are sequential.
	child := New("specialist", "", WithConfig(agotest.NewTestConfig(
		resp("one", 10),
		resp("two", 20),
	)), WithStreamBase(stream.NewMemoryStream()))
	base := stream.NewMemoryStream()
	parentCfg := agotest.NewTestConfig(
		toolResp("task_specialist", `{"objective":"first"}`),
		toolResp("task_specialist", `{"objective":"second"}`),
		textResp("wrap"),
	)
	parent := New("lead", "", WithConfig(parentCfg), WithStreamBase(base), WithTools(child.AsTool()))
	if _, err := parent.Ask(stdcontext.Background(), "delegate twice"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	te := collectTaskEvents(t, base)
	var forwarded []events.UsageEvent
	for _, u := range te.usage {
		if u.AgentName == "specialist" {
			forwarded = append(forwarded, u)
		}
	}
	if len(forwarded) != 2 {
		t.Fatalf("forwarded usage events: %d, want 2 (one per invocation)", len(forwarded))
	}
	if got := forwarded[0].Usage.PromptTokens; got == nil || *got != 10 {
		t.Fatalf("forwarded[0] prompt_tokens = %v, want 10", got)
	}
	if got := forwarded[1].Usage.PromptTokens; got == nil || *got != 20 {
		t.Fatalf("forwarded[1] prompt_tokens = %v, want 20 (not re-forwarded 10+20)", got)
	}
	// TaskCompleted.Usage is the cumulative stream total at completion time.
	for i, want := range []int64{10, 30} {
		got := te.completed[i].Usage.TotalTokens
		if got == nil || *got != want {
			t.Fatalf("TaskCompleted[%d] total_tokens = %v, want %d (running total)", i, got, want)
		}
	}
}
