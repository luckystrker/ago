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
	"github.com/luckystrker/ago/middleware"
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
	h := ServeAgent(agent.New("served", "You are served.", agent.WithConfig(cfg), ClientTools()))

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

// The Server path wires the same: Options() carries the client tools, and the
// declared tool is withdrawn once the turn ended.
func TestClientDeclaredToolsOnServer(t *testing.T) {
	srv, err := NewServer()
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	cfg := newToolSeeingConfig(clientToolCall())
	h := srv.Handler(agent.New("served", "", append(srv.Options(), agent.WithConfig(cfg))...))

	evs, raw := postRun(t, h, frontendRunBody(""))
	want := []EventType{EventRunStarted, EventToolCallStart, EventToolCallArgs, EventToolCallEnd, EventRunFinished}
	if got := eventTypes(evs); !equalTypes(got, want) {
		t.Fatalf("types = %v\nwant  = %v\nraw: %q", got, want, raw)
	}
	if offered := cfg.offered(); len(offered) != 1 || len(offered[0]) != 1 || offered[0][0] != "draw_chart" {
		t.Fatalf("the model was offered %v", offered)
	}
	waitNoDeclared(t)
}

// declaredCount is how many turns have client tools declared right now.
func declaredCount() int {
	n := 0
	declaredClientTools.Range(func(_, _ any) bool { n++; return true })
	return n
}

// waitNoDeclared fails unless every declared tool set is withdrawn shortly: a
// served turn withdraws its tools as its driver winds down, just after the
// response ends.
func waitNoDeclared(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for declaredCount() != 0 {
		if time.Now().After(deadline) {
			declaredClientTools.Range(func(k, _ any) bool {
				t.Errorf("client tools of stream %v still declared", k)
				return true
			})
			return
		}
		time.Sleep(5 * time.Millisecond)
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
	h := ServeAgent(agent.New("served", "", agent.WithConfig(cfg), agent.WithTools(own), ClientTools()))

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

// Tools declared for an agent that was not built with ClientTools() are ignored,
// not an error.
func TestClientToolsIgnoredWithoutOption(t *testing.T) {
	cfg := newToolSeeingConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}})
	var declaredDuringTurn = -1
	cfg.hook = func([]events.Event, []string) { declaredDuringTurn = declaredCount() }
	waitNoDeclared(t)
	evs, _ := postRun(t, ServeAgent(agent.New("served", "", agent.WithConfig(cfg))), frontendRunBody(""))
	if declaredDuringTurn != 0 {
		t.Errorf("tools declared during a turn of an agent without ClientTools() = %d, want 0", declaredDuringTurn)
	}
	if got := eventTypes(evs); got[len(got)-1] != EventRunFinished {
		t.Fatalf("types = %v", got)
	}
	if offered := cfg.offered(); len(offered) != 1 || len(offered[0]) != 0 {
		t.Fatalf("model was offered %v, want no tools", offered)
	}
}

// The model sees the client's description and parameters, and a tool declared
// with neither is offered with neither.
func TestClientToolSchemaAsModelSees(t *testing.T) {
	cfg := newToolSeeingConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}})
	h := ServeAgent(agent.New("served", "", agent.WithConfig(cfg), ClientTools()))
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

// A tool-set resolved for a context with no turn behind it contributes nothing
// and does not panic.
func TestClientToolSetNilGuard(t *testing.T) {
	set := clientToolSet{}
	for name, cx := range map[string]*agocontext.Context{
		"nil context": nil,
		"nil stream":  {},
		"unknown":     agocontext.New(stream.NewMemoryStream()),
	} {
		schemas, err := set.Schemas(cx)
		if err != nil || len(schemas) != 0 {
			t.Errorf("%s: Schemas = %v, %v", name, schemas, err)
		}
		if cx != nil && cx.Stream != nil {
			subs, err := set.Register(cx, nil)
			if err != nil || len(subs) != 0 {
				t.Errorf("%s: Register = %v, %v", name, subs, err)
			}
		}
	}
}

// fakeClientTool subscribes a counter on the stream, or fails to register.
type fakeClientTool struct {
	name string
	fail bool
	hits *int
}

func (f fakeClientTool) Name() string         { return f.name }
func (f fakeClientTool) DeclaredInCode() bool { return false }
func (f fakeClientTool) Schemas(*agocontext.Context) ([]any, error) {
	return []any{tools.Schema{Name: f.name}}, nil
}
func (f fakeClientTool) Register(cx *agocontext.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	if f.fail {
		return nil, errors.New("register failed")
	}
	id := cx.Stream.Subscribe(func(events.Event) error { *f.hits++; return nil }, stream.SubscribeOptions{})
	return []stream.SubID{id}, nil
}

// When one tool of the set fails to register, the ones already registered are
// withdrawn: nothing stays subscribed on the stream.
func TestClientToolSetRegisterRollsBack(t *testing.T) {
	var hits int
	cx := agocontext.New(stream.NewMemoryStream())
	declaredClientTools.Store(cx.Stream.ID(), []tools.Tool{
		fakeClientTool{name: "first", hits: &hits},
		fakeClientTool{name: "second", fail: true},
	})
	defer declaredClientTools.Delete(cx.Stream.ID())

	subs, err := clientToolSet{}.Register(cx, nil)
	if err == nil || len(subs) != 0 {
		t.Fatalf("Register = %v, %v, want the failure and no subscriptions", subs, err)
	}
	if err := cx.Stream.Send(events.NewToolCallEvent("anything")); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatalf("the first tool's subscription survived the failed Register (%d events seen)", hits)
	}

	// Control: with no failing tool the subscription is live.
	declaredClientTools.Store(cx.Stream.ID(), []tools.Tool{fakeClientTool{name: "first", hits: &hits}})
	subs, err = clientToolSet{}.Register(cx, nil)
	if err != nil || len(subs) != 1 {
		t.Fatalf("Register = %v, %v", subs, err)
	}
	if err := cx.Stream.Send(events.NewToolCallEvent("anything")); err != nil {
		t.Fatal(err)
	}
	if hits == 0 {
		t.Fatal("control subscription never fired")
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
	waitNoDeclared(t)
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
		// While both turns are in flight both declarations are held; neither
		// turn ends before both have looked.
		if n := declaredCount(); n != 2 {
			t.Errorf("declared tool sets while both turns run = %d, want 2", n)
		}
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
	waitNoDeclared(t)
}

// failingStorage refuses to seed a transcript, which fails a turn before it
// starts.
type failingStorage struct{ stream.Storage }

func (failingStorage) SetHistory(string, []events.Event) error { return errors.New("storage down") }

// A turn that fails while its history is seeded still withdraws the client tools
// it declared — on the server path and on the plain handler path.
func TestClientToolsWithdrawnWhenHistorySeedFails(t *testing.T) {
	waitNoDeclared(t)
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
		"handler": ServeAgent(newAgent(ClientTools())),
	} {
		evs, _ := postRun(t, h, frontendRunBody(""))
		if got := eventTypes(evs); got[len(got)-1] != EventRunError {
			t.Fatalf("%s: types = %v, want the run to end in RUN_ERROR", name, got)
		}
		waitNoDeclared(t)
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
	h := ServeAgent(agent.New("served", "", agent.WithConfig(cfg), agent.WithTools(kit), ClientTools()))
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
