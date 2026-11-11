package agui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// toolSeeingConfig wraps a scripted config and records the tool names each model
// call was offered.
type toolSeeingConfig struct {
	*agotest.TestConfig
	mu      *sync.Mutex
	seen    *[][]string
	schemas *[][]tools.Schema
	// hook, when set, runs inside each model call after it was recorded, with
	// the history the call was handed and the tool names it was offered.
	hook func(msgs []events.Event, names []string)
}

func newToolSeeingConfig(responses ...*events.ModelResponse) toolSeeingConfig {
	return toolSeeingConfig{TestConfig: agotest.NewTestConfig(responses...), mu: &sync.Mutex{},
		seen: new([][]string), schemas: new([][]tools.Schema)}
}

func (c toolSeeingConfig) Create() (config.LLMClient, error) { return toolSeeingClient{c}, nil }

func (c toolSeeingConfig) Clone(m map[string]any) (config.ModelConfig, error) {
	inner, err := c.TestConfig.Clone(m)
	if err != nil {
		return nil, err
	}
	c.TestConfig = inner.(*agotest.TestConfig)
	return c, nil
}

func (c toolSeeingConfig) offered() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]string(nil), *c.seen...)
}

type toolSeeingClient struct{ cfg toolSeeingConfig }

func (c toolSeeingClient) Call(ctx context.Context, msgs []events.Event, cx *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	var (
		names   []string
		schemas []tools.Schema
	)
	for _, s := range opts.Tools {
		if schema, ok := s.(tools.Schema); ok {
			names = append(names, schema.Name)
			schemas = append(schemas, schema)
		}
	}
	c.cfg.mu.Lock()
	*c.cfg.seen = append(*c.cfg.seen, names)
	*c.cfg.schemas = append(*c.cfg.schemas, schemas)
	c.cfg.mu.Unlock()
	if c.cfg.hook != nil {
		c.cfg.hook(msgs, names)
	}
	return c.cfg.TestConfig.Call(ctx, msgs, cx, opts)
}

const frontendToolDecl = `{"name":"draw_chart","description":"Draw a chart in the page","parameters":{"type":"object","properties":{"kind":{"type":"string"}},"required":["kind"]}}`

func frontendRunBody(extra string) string {
	return `{"threadId":"t1","runId":"r1","messages":[{"id":"m1","role":"user","content":"chart it"}],"tools":[` + frontendToolDecl + `]` + extra + `}`
}

func clientToolCall() *events.ModelResponse {
	return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "c1", Name: "draw_chart", Arguments: `{"kind":"bar"}`},
	}}}
}

// A tool the client declares reaches the model, its call is handed back to the
// client as TOOL_CALL_START/ARGS/END ending the run, and a follow-up run whose
// history carries the result continues the turn.
func TestClientDeclaredToolsRoundTrip(t *testing.T) {
	cfg := newToolSeeingConfig(
		clientToolCall(),
		&events.ModelResponse{Message: &events.ModelMessage{Content: "Chart drawn."}},
	)
	h := ServeAgent(agent.New("served", "You are served.", agent.WithConfig(cfg)))

	evs, raw := postRun(t, h, frontendRunBody(""))
	want := []EventType{
		EventRunStarted,
		EventToolCallStart, EventToolCallArgs, EventToolCallEnd,
		EventRunFinished,
	}
	if got := eventTypes(evs); !equalTypes(got, want) {
		t.Fatalf("types = %v\nwant  = %v\nraw: %q", got, want, raw)
	}
	start := evs[1].(ToolCallStartEvent)
	if start.ToolCallID != "c1" || start.ToolCallName != "draw_chart" {
		t.Fatalf("start = %+v", start)
	}
	if args := evs[2].(ToolCallArgsEvent); args.Delta != `{"kind":"bar"}` {
		t.Fatalf("args = %+v", args)
	}
	offered := cfg.offered()
	if len(offered) != 1 || len(offered[0]) != 1 || offered[0][0] != "draw_chart" {
		t.Fatalf("the model was offered %v, want [[draw_chart]]", offered)
	}

	// The client ran the tool and answers in a follow-up run.
	follow := `{"threadId":"t1","runId":"r2","messages":[` +
		`{"id":"m1","role":"user","content":"chart it"},` +
		`{"id":"a1","role":"assistant","toolCalls":[{"id":"c1","type":"function","function":{"name":"draw_chart","arguments":"{\"kind\":\"bar\"}"}}]},` +
		`{"id":"t1","role":"tool","toolCallId":"c1","content":"drawn"}],"tools":[` + frontendToolDecl + `]}`
	evs, raw = postRun(t, h, follow)
	if got := eventTypes(evs); len(got) < 2 || got[0] != EventRunStarted || got[len(got)-1] != EventRunFinished {
		t.Fatalf("follow-up types = %v\nraw: %q", got, raw)
	}
	var text strings.Builder
	for _, e := range evs {
		if c, ok := e.(TextMessageChunkEvent); ok {
			text.WriteString(c.Delta)
		}
	}
	if text.String() != "Chart drawn." {
		t.Fatalf("follow-up text = %q (events %v)", text.String(), eventTypes(evs))
	}
	offered = cfg.offered()
	if len(offered) != 2 || len(offered[1]) != 1 || offered[1][0] != "draw_chart" {
		t.Fatalf("the follow-up model call was offered %v", offered)
	}
	// The model saw the client's answer in its history.
	last := cfg.Calls()[1]
	var sawResult bool
	for _, ev := range last {
		if tr, ok := events.AsValue(ev).(events.ToolResultsEvent); ok && len(tr.Results) == 1 && tr.Results[0].ParentID == "c1" {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatalf("follow-up history has no tool result: %v", last)
	}
}

// The Server path wires the same: the declared tool is that run's, and a later
// direct turn does not see it.
func TestClientDeclaredToolsOnServer(t *testing.T) {
	srv, err := NewServer()
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	cfg := newToolSeeingConfig(
		clientToolCall(),
		&events.ModelResponse{Message: &events.ModelMessage{Content: "plain reply"}},
	)
	a := agent.New("served", "", append(srv.Options(), agent.WithConfig(cfg))...)
	h := srv.Handler(a)

	evs, raw := postRun(t, h, frontendRunBody(""))
	want := []EventType{EventRunStarted, EventToolCallStart, EventToolCallArgs, EventToolCallEnd, EventRunFinished}
	if got := eventTypes(evs); !equalTypes(got, want) {
		t.Fatalf("types = %v\nwant  = %v\nraw: %q", got, want, raw)
	}
	offered := cfg.offered()
	if len(offered) != 1 || len(offered[0]) != 1 || offered[0][0] != "draw_chart" {
		t.Fatalf("the model was offered %v", offered)
	}
	// The client tools belong to that run: a later direct turn of the same
	// agent does not see them.
	if _, err := a.Ask(context.Background(), "plain turn"); err != nil {
		t.Fatalf("direct follow-up ask: %v", err)
	}
	offered = cfg.offered()
	if len(offered) != 2 || len(offered[1]) != 0 {
		t.Fatalf("the direct turn was offered %v, want no client tools", offered)
	}
}

// A client tool named like a tool the agent has does not replace it; the rest of
// the client's tools still arrive.
func TestClientToolNamedLikeAgentToolIsDropped(t *testing.T) {
	own, err := tools.NewFunctionTool("draw_chart", "the agent's own",
		func(args struct{}, cx *agocontext.Context) (string, error) { return "own", nil })
	if err != nil {
		t.Fatal(err)
	}
	cfg := newToolSeeingConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}})
	h := ServeAgent(agent.New("served", "", agent.WithConfig(cfg), agent.WithTools(own)))

	body := `{"threadId":"t1","runId":"r1","messages":[{"id":"m1","role":"user","content":"hi"}],"tools":[` +
		frontendToolDecl + `,{"name":"pick_date","description":"Pick a date"}]}`
	postRun(t, h, body)
	offered := cfg.offered()
	if len(offered) != 1 {
		t.Fatalf("offered = %v", offered)
	}
	got := map[string]int{}
	for _, n := range offered[0] {
		got[n]++
	}
	if got["draw_chart"] != 1 || got["pick_date"] != 1 || len(got) != 2 {
		t.Fatalf("model was offered %v, want the agent's draw_chart once and the client's pick_date", offered[0])
	}
}

// Client tools arrive with the run, so an agent needs no construction-time
// client-tool option to see them.
func TestClientToolsWorkWithoutOption(t *testing.T) {
	cfg := newToolSeeingConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}})
	evs, _ := postRun(t, ServeAgent(agent.New("served", "", agent.WithConfig(cfg))), frontendRunBody(""))
	if got := eventTypes(evs); got[len(got)-1] != EventRunFinished {
		t.Fatalf("types = %v", got)
	}
	if offered := cfg.offered(); len(offered) != 1 || len(offered[0]) != 1 || offered[0][0] != "draw_chart" {
		t.Fatalf("model was offered %v, want the declared draw_chart", offered)
	}
}

// The model sees the client's description and parameters, and a tool declared
// with neither is offered with neither.
func TestClientToolSchemaAsModelSees(t *testing.T) {
	cfg := newToolSeeingConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}})
	h := ServeAgent(agent.New("served", "", agent.WithConfig(cfg)))
	body := `{"threadId":"t1","runId":"r1","messages":[{"id":"m1","role":"user","content":"hi"}],"tools":[` +
		frontendToolDecl + `,{"name":"bare"}]}`
	postRun(t, h, body)

	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	if len(*cfg.schemas) != 1 || len((*cfg.schemas)[0]) != 2 {
		t.Fatalf("schemas offered = %+v", *cfg.schemas)
	}
	byName := map[string]tools.Schema{}
	for _, s := range (*cfg.schemas)[0] {
		byName[s.Name] = s
	}
	chart := byName["draw_chart"]
	if chart.Description != "Draw a chart in the page" {
		t.Errorf("description = %q", chart.Description)
	}
	wantParams := map[string]any{
		"type":       "object",
		"properties": map[string]any{"kind": map[string]any{"type": "string"}},
		"required":   []any{"kind"},
	}
	if !reflect.DeepEqual(chart.Parameters, wantParams) {
		t.Errorf("parameters = %#v, want %#v", chart.Parameters, wantParams)
	}
	if bare := byName["bare"]; bare.Description != "" || len(bare.Parameters) != 0 {
		t.Errorf("bare tool carries description %q / parameters %v", bare.Description, bare.Parameters)
	}
}

func userTextOf(msgs []events.Event) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if req, ok := events.AsValue(msgs[i]).(events.ModelRequest); ok {
			for _, p := range req.Parts {
				if ti, ok := events.AsValue(p).(events.TextInput); ok {
					return ti.Content
				}
			}
		}
	}
	return ""
}

// Two turns running at once on one server each offer the model only the tools
// their own client declared, and both are withdrawn afterwards.
func TestConcurrentTurnsKeepTheirOwnClientTools(t *testing.T) {
	srv, err := NewServer()
	if err != nil {
		t.Fatal(err)
	}
	cfg := newToolSeeingConfig(
		&events.ModelResponse{Message: &events.ModelMessage{Content: "one"}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "two"}},
	)
	var (
		mu       sync.Mutex
		offered  = map[string][]string{}
		arrived  sync.WaitGroup
		checked  sync.WaitGroup
		bothIn   = make(chan struct{})
		bothOnce sync.Once
	)
	arrived.Add(2)
	checked.Add(2)
	cfg.hook = func(msgs []events.Event, names []string) {
		mu.Lock()
		offered[userTextOf(msgs)] = names
		mu.Unlock()
		arrived.Done()
		bothOnce.Do(func() { go func() { arrived.Wait(); close(bothIn) }() })
		select {
		case <-bothIn:
		case <-time.After(5 * time.Second):
			t.Error("the two turns never overlapped")
			return
		}
		// Neither turn may end before both have looked, so the offered sets
		// below were captured while both runs were in flight.
		checked.Done()
		checked.Wait()
	}
	h := srv.Handler(agent.New("served", "", append(srv.Options(), agent.WithConfig(cfg))...))
	ts := httptest.NewServer(h)
	defer ts.Close()

	var wg sync.WaitGroup
	for _, c := range []struct{ user, tool string }{{"A", "tool_a"}, {"B", "tool_b"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"threadId":"thread-` + c.user + `","runId":"r","messages":[{"id":"m","role":"user","content":"` +
				c.user + `"}],"tools":[{"name":"` + c.tool + `"}]}`
			resp, err := http.Post(ts.URL, "application/json", strings.NewReader(body))
			if err != nil {
				t.Errorf("post %s: %v", c.user, err)
				return
			}
			defer resp.Body.Close()
			out, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(out), "RUN_FINISHED") {
				t.Errorf("turn %s did not finish: %s", c.user, out)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(offered["A"], []string{"tool_a"}) || !reflect.DeepEqual(offered["B"], []string{"tool_b"}) {
		t.Fatalf("offered = %v, want A→[tool_a], B→[tool_b]", offered)
	}
}

// failingStorage refuses to seed a transcript, which fails a turn before it
// starts.
type failingStorage struct{ stream.Storage }

func (failingStorage) SetHistory(string, []events.Event) error { return errors.New("storage down") }

// A turn that fails while its history is seeded still withdraws the client tools
// it declared — on the server path and on the plain handler path.
func TestClientToolsWithdrawnWhenHistorySeedFails(t *testing.T) {
	newAgent := func(opts ...agent.Option) *agent.Agent {
		base := stream.NewMemoryStream(stream.WithStorage(failingStorage{stream.NewMemoryStorage()}))
		cfg := newToolSeeingConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "unreached"}})
		return agent.New("served", "", append(opts, agent.WithConfig(cfg), agent.WithStreamBase(base))...)
	}
	srv, err := NewServer()
	if err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string]http.Handler{
		"server":  srv.Handler(newAgent(srv.Options()...)),
		"handler": ServeAgent(newAgent()),
	} {
		evs, _ := postRun(t, h, frontendRunBody(""))
		if got := eventTypes(evs); got[len(got)-1] != EventRunError {
			t.Fatalf("%s: types = %v, want the run to end in RUN_ERROR", name, got)
		}
	}
}

// A tool living inside one of the agent's toolkits counts as the agent's own: a
// client tool with its name is dropped and the client's other tools still arrive.
func TestClientToolNamedLikeToolkitMemberIsDropped(t *testing.T) {
	member, err := tools.NewFunctionTool("draw_chart", "the toolkit's own",
		func(struct{}) (string, error) { return "own", nil })
	if err != nil {
		t.Fatal(err)
	}
	kit, err := tools.NewToolkit("charts", nil, member)
	if err != nil {
		t.Fatal(err)
	}
	cfg := newToolSeeingConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}})
	h := ServeAgent(agent.New("served", "", agent.WithConfig(cfg), agent.WithTools(kit)))
	postRun(t, h, `{"threadId":"t1","runId":"r1","messages":[{"id":"m1","role":"user","content":"hi"}],"tools":[`+
		frontendToolDecl+`,{"name":"pick_date"}]}`)
	offered := cfg.offered()
	got := map[string]int{}
	for _, n := range offered[0] {
		got[n]++
	}
	if got["draw_chart"] != 1 || got["pick_date"] != 1 || len(got) != 2 {
		t.Fatalf("model was offered %v, want the toolkit's draw_chart once and the client's pick_date", offered[0])
	}
}
