package a2a

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// wireLog records "METHOD path" of every request that reaches the server.
type wireLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *wireLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, r.Method+" "+r.URL.RequestURI())
}

func (l *wireLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (l *wireLog) count(prefix string) int {
	n := 0
	for _, line := range l.snapshot() {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// startDualServer mounts a Server whose card declares a JSON-RPC and a REST
// interface, both at the httptest URL, and logs every request.
func startDualServer(t *testing.T, store TaskStore, mutate func(url string, card *AgentCard)) (string, *wireLog) {
	t.Helper()
	log := &wireLog{}
	var inner http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	card := testCard(false)
	card.SupportedInterfaces = []AgentInterface{
		{URL: ts.URL, ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: ProtocolVersionCurrent},
		{URL: ts.URL, ProtocolBinding: ProtocolBindingREST, ProtocolVersion: ProtocolVersionCurrent},
	}
	if mutate != nil {
		mutate(ts.URL, &card)
	}
	inner = NewServer(ServerConfig{Card: card, Handle: echoHandler, Store: store}).Handler()
	return ts.URL, log
}

// TestClientRESTBinding drives send, get and cancel over the REST binding
// against the Go REST server and checks the wire: bare Message on
// POST /message:send, GET /tasks/{id}, POST /tasks/{id}:cancel.
func TestClientRESTBinding(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore()
	url, log := startDualServer(t, store, nil)
	client := NewClient(url, WithTransport(TransportREST))

	reply, err := client.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("over rest")))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got := textOfMessage(reply); got != "echo: over rest" {
		t.Fatalf("reply = %q", got)
	}
	if reply.TaskID == "" {
		t.Fatal("reply carries no task id")
	}

	task, err := client.TasksGet(ctx, reply.TaskID)
	if err != nil {
		t.Fatalf("TasksGet: %v", err)
	}
	if task.ID != reply.TaskID || task.Status.State != TaskStateCompleted {
		t.Fatalf("task = %+v", task)
	}

	// Terminal task: cancel maps 409 to ErrTaskNotCancelable.
	if _, err := client.TasksCancel(ctx, reply.TaskID); !errors.Is(err, ErrTaskNotCancelable) {
		t.Fatalf("cancel completed: err = %v, want ErrTaskNotCancelable", err)
	}
	// Unknown task: 404 maps to ErrTaskNotFound.
	if _, err := client.TasksGet(ctx, "absent"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("get absent: err = %v, want ErrTaskNotFound", err)
	}

	// A working task cancels cleanly.
	if err := store.Save(ctx, &Task{ID: "t-live", Status: TaskStatus{State: TaskStateWorking}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	canceled, err := client.TasksCancel(ctx, "t-live")
	if err != nil {
		t.Fatalf("TasksCancel: %v", err)
	}
	if canceled.Status.State != TaskStateCanceled {
		t.Fatalf("canceled state = %q", canceled.Status.State)
	}

	want := []string{
		"GET " + DefaultCardPath,
		"POST /message:send",
		"GET /tasks/" + reply.TaskID,
		"POST /tasks/" + reply.TaskID + ":cancel",
		"GET /tasks/absent",
		"POST /tasks/t-live:cancel",
	}
	if got := log.snapshot(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("wire log:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestClientRESTDriversSendMessageTask: the Task-shaped send used by the
// client-tools/HITL drivers rides the REST binding too.
func TestClientRESTDriversSendMessageTask(t *testing.T) {
	url, log := startDualServer(t, nil, nil)
	client := NewClient(url, WithTransport(TransportREST))
	task, err := client.SendMessageTask(context.Background(), NewMessage(RoleUser, NewTextPart("x")))
	if err != nil {
		t.Fatalf("SendMessageTask: %v", err)
	}
	if task.Status.State != TaskStateCompleted || task.Status.Message == nil {
		t.Fatalf("task = %+v", task)
	}
	if log.count("POST /message:send") != 1 {
		t.Fatalf("wire log = %v", log.snapshot())
	}
}

// TestClientHistoryLength: WithHistoryLength trims tasks/get on both bindings.
func TestClientHistoryLength(t *testing.T) {
	ctx := context.Background()
	for _, transport := range []string{TransportJSONRPC, TransportREST} {
		t.Run(transport, func(t *testing.T) {
			url, log := startDualServer(t, nil, nil)
			plain := NewClient(url, WithTransport(transport))
			first, err := plain.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("one")))
			if err != nil {
				t.Fatalf("first send: %v", err)
			}
			cont := NewMessage(RoleUser, NewTextPart("two"))
			cont.TaskID, cont.ContextID = first.TaskID, first.ContextID
			if _, err := plain.SendMessage(ctx, cont); err != nil {
				t.Fatalf("second send: %v", err)
			}

			full, err := plain.TasksGet(ctx, first.TaskID)
			if err != nil || len(full.History) != 2 {
				t.Fatalf("full history: len %d err %v, want 2", len(full.History), err)
			}
			trimmed, err := NewClient(url, WithTransport(transport), WithHistoryLength(1)).TasksGet(ctx, first.TaskID)
			if err != nil {
				t.Fatalf("trimmed get: %v", err)
			}
			if len(trimmed.History) != 1 {
				t.Fatalf("trimmed history len = %d, want 1 (log %v)", len(trimmed.History), log.snapshot())
			}
		})
	}
}

// TestClientInterfaceSelection: WithTransport honours the card's declared
// bindings and routes to the matching endpoint; a missing binding is refused.
func TestClientInterfaceSelection(t *testing.T) {
	ctx := context.Background()

	t.Run("jsonrpc preferred uses POST /", func(t *testing.T) {
		url, log := startDualServer(t, nil, nil)
		c := NewClient(url, WithTransport(TransportJSONRPC))
		if _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("a"))); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		if log.count("POST /message:send") != 0 || log.count("POST /") != 1 {
			t.Fatalf("wire log = %v", log.snapshot())
		}
	})

	t.Run("default picks the interface matching the base URL", func(t *testing.T) {
		// Declared order puts REST first, but only the JSON-RPC interface
		// has the client's base URL.
		url, log := startDualServer(t, nil, func(u string, card *AgentCard) {
			card.SupportedInterfaces = []AgentInterface{
				{URL: "http://elsewhere.invalid", ProtocolBinding: ProtocolBindingREST, ProtocolVersion: "1.0"},
				{URL: u, ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: "1.0"},
			}
		})
		c := NewClient(url)
		if err := c.Connect(ctx); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		if _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("a"))); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		if log.count("POST /message:send") != 0 {
			t.Fatalf("went REST: %v", log.snapshot())
		}
	})

	t.Run("default falls back to the first interface", func(t *testing.T) {
		url, log := startDualServer(t, nil, func(u string, card *AgentCard) {
			card.SupportedInterfaces = []AgentInterface{
				{URL: u + "/", ProtocolBinding: ProtocolBindingREST, ProtocolVersion: "1.0"},
			}
		})
		// The only interface is REST at base+"/": trailing slash still matches.
		c := NewClient(url)
		if err := c.Connect(ctx); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		if _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("a"))); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		if log.count("POST /message:send") != 1 {
			t.Fatalf("wire log = %v", log.snapshot())
		}
	})

	t.Run("undeclared transport is refused", func(t *testing.T) {
		url, _ := startDualServer(t, nil, func(u string, card *AgentCard) {
			card.SupportedInterfaces = card.SupportedInterfaces[:1] // JSON-RPC only
		})
		c := NewClient(url, WithTransport(TransportREST))
		if _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("a"))); !errors.Is(err, ErrInvalidCard) {
			t.Fatalf("err = %v, want ErrInvalidCard", err)
		}
	})

	t.Run("a card without interfaces is refused", func(t *testing.T) {
		card := &AgentCard{Name: "legacy", URL: "http://localhost:0"}
		c := NewClient("http://localhost:0", WithPresetCard(card))
		if err := c.Connect(ctx); !errors.Is(err, ErrInvalidCard) {
			t.Fatalf("err = %v, want ErrInvalidCard", err)
		}
	})

	t.Run("only an unsupported binding is refused", func(t *testing.T) {
		card := &AgentCard{Name: "grpc", SupportedInterfaces: []AgentInterface{
			{URL: "http://localhost:0", ProtocolBinding: "GRPC", ProtocolVersion: "1.0"},
		}}
		c := NewClient("http://localhost:0", WithPresetCard(card))
		if err := c.Connect(ctx); !errors.Is(err, ErrInvalidCard) {
			t.Fatalf("err = %v, want ErrInvalidCard", err)
		}
	})
}

// TestClientProtocolVersionCheck: pre-1.0 interfaces are refused at connect;
// an empty or unparsable version passes.
func TestClientProtocolVersionCheck(t *testing.T) {
	tests := []struct {
		version string
		wantErr bool
	}{
		{"0.3.0", true},
		{"0.9", true},
		{"v0.3", true},
		{"0", true},
		{"1.0", false},
		{"1.0.0", false},
		{"2.1", false},
		{"v1.0", false},
		{"", false},
		{"not-a-version", false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.version), func(t *testing.T) {
			card := &AgentCard{Name: "x", SupportedInterfaces: []AgentInterface{
				{URL: "http://localhost:0", ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: tt.version},
			}}
			err := NewClient("http://localhost:0", WithPresetCard(card)).Connect(context.Background())
			if tt.wantErr != errors.Is(err, ErrIncompatibleProtocolVersion) {
				t.Fatalf("version %q: err = %v, wantErr %v", tt.version, err, tt.wantErr)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("version %q: unexpected err %v", tt.version, err)
			}
		})
	}
}

// TestClientPresetCard: an injected card skips discovery entirely and
// AgentCard hands out copies.
func TestClientPresetCard(t *testing.T) {
	ctx := context.Background()
	url, log := startDualServer(t, nil, nil)
	preset := &AgentCard{Name: "preset", SupportedInterfaces: []AgentInterface{
		{URL: url, ProtocolBinding: ProtocolBindingREST, ProtocolVersion: "1.0"},
	}, Skills: []AgentSkill{{ID: "s", Name: "s"}}}
	c := NewClient(url, WithPresetCard(preset))

	got, err := c.AgentCard(ctx)
	if err != nil || got.Name != "preset" {
		t.Fatalf("AgentCard = %+v, %v", got, err)
	}
	got.Skills[0].Name = "mutated"
	got.Name = "mutated"
	if preset.Skills[0].Name != "s" || preset.Name != "preset" {
		t.Fatal("AgentCard leaked a reference to the preset card")
	}

	if _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("hi"))); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if n := log.count("GET " + DefaultCardPath); n != 0 {
		t.Fatalf("discovery requests = %d, want 0", n)
	}
	if log.count("POST /message:send") != 1 {
		t.Fatalf("not routed over the preset card's REST interface: %v", log.snapshot())
	}
}

// TestClientConnectFailureNotCached: a card fetch failure surfaces on the
// call and the next call retries instead of keeping the failure.
func TestClientConnectFailureNotCached(t *testing.T) {
	ctx := context.Background()
	var healthy atomic.Bool
	var inner http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() && r.URL.Path == DefaultCardPath {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer ts.Close()
	card := testCard(false)
	card.URL = ts.URL // the default interface points at the server
	inner = NewServer(ServerConfig{Card: card, Handle: echoHandler}).Handler()

	c := NewClient(ts.URL, WithTransport(TransportJSONRPC))
	if _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("a"))); !errors.Is(err, ErrTransport) {
		t.Fatalf("first call: err = %v, want ErrTransport", err)
	}
	healthy.Store(true)
	if _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("a"))); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
}

// TestClientInterceptors: interceptors see every request with the method
// name, can set headers, and abort the call by failing.
func TestClientInterceptors(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	seen := map[string]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = r.Header.Get("X-Trace")
		mu.Unlock()
		NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler}).ServeHTTP(w, r)
	}))
	defer ts.Close()

	var methods []string
	stamp := func(_ context.Context, method string, req *http.Request) error {
		methods = append(methods, method)
		req.Header.Set("X-Trace", "t-"+method)
		return nil
	}

	c := NewClient(ts.URL, WithInterceptors(stamp))
	if _, err := c.AgentCard(ctx); err != nil {
		t.Fatalf("AgentCard: %v", err)
	}
	if _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("a"))); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got := strings.Join(methods, ","); got != "agent-card,message/send" {
		t.Fatalf("interceptor methods = %s", got)
	}
	mu.Lock()
	if seen["POST /"] != "t-message/send" || seen["GET "+DefaultCardPath] != "t-agent-card" {
		t.Fatalf("headers on the wire = %v", seen)
	}
	mu.Unlock()

	// REST requests pass through the same hook.
	url, _ := startDualServer(t, nil, nil)
	methods = nil
	rc := NewClient(url, WithTransport(TransportREST), WithInterceptors(stamp))
	if _, err := rc.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("a"))); err != nil {
		t.Fatalf("REST SendMessage: %v", err)
	}
	if got := strings.Join(methods, ","); got != "agent-card,message/send" {
		t.Fatalf("REST interceptor methods = %s", got)
	}

	// A failing interceptor aborts before anything hits the wire.
	boom := errors.New("boom")
	bad := NewClient(ts.URL, WithInterceptors(func(context.Context, string, *http.Request) error { return boom }))
	_, err := bad.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("a")))
	if !errors.Is(err, boom) || !errors.Is(err, ErrTransport) {
		t.Fatalf("err = %v, want boom wrapped in ErrTransport", err)
	}
}

// TestClientRESTRefusesPush: push-config methods exist on JSON-RPC only.
func TestClientRESTRefusesPush(t *testing.T) {
	url, log := startDualServer(t, nil, nil)
	c := NewClient(url, WithTransport(TransportREST))
	_, err := c.ListPushConfigs(context.Background(), "t1")
	if !errors.Is(err, ErrBindingUnsupported) {
		t.Fatalf("err = %v, want ErrBindingUnsupported", err)
	}
	if n := log.count("POST"); n != 0 {
		t.Fatalf("a POST leaked out: %v", log.snapshot())
	}
}

// TestServerCardModifierAndCacheControl: CardModifier rewrites a per-request
// deep copy on both discovery routes; CardCacheControl sets the header.
func TestServerCardModifierAndCacheControl(t *testing.T) {
	card := testCard(false)
	card.Skills = []AgentSkill{{ID: "base", Name: "base"}}
	srv := NewServer(ServerConfig{
		Card:             card,
		CardCacheControl: "public, max-age=300",
		CardModifier: func(r *http.Request, c *AgentCard) (*AgentCard, error) {
			// Mutate in place and return nil: the copy is served.
			c.Name = "tenant-" + r.Header.Get("X-Tenant")
			c.Skills[0].Name = "mutated"
			c.Skills = append(c.Skills, AgentSkill{ID: "extra", Name: "extra"})
			return nil, nil
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{DefaultCardPath, LegacyCardPath} {
		for _, tenant := range []string{"a", "b"} {
			client := NewClient(ts.URL, WithCardPath(path), WithHeaders(map[string]string{"X-Tenant": tenant}))
			got, err := client.AgentCard(context.Background())
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if got.Name != "tenant-"+tenant {
				t.Fatalf("%s: name = %q", path, got.Name)
			}
			// The shared card must not accumulate earlier requests' edits.
			if len(got.Skills) != 2 || got.Skills[1].ID != "extra" {
				t.Fatalf("%s: skills = %+v", path, got.Skills)
			}
		}
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=300" {
			t.Fatalf("%s: Cache-Control = %q", path, cc)
		}
	}
	// The static card is untouched.
	if srv.card.Name != "tester" || len(srv.card.Skills) != 1 || srv.card.Skills[0].Name != "base" {
		t.Fatalf("static card mutated: %+v", srv.card)
	}
}

// TestServerCardModifierReplaceAndError: a returned card replaces the copy;
// an error answers 500 with the fixed text.
func TestServerCardModifierReplaceAndError(t *testing.T) {
	replace := NewServer(ServerConfig{Card: testCard(false),
		CardModifier: func(r *http.Request, c *AgentCard) (*AgentCard, error) {
			return &AgentCard{Name: "replacement"}, nil
		}})
	rec := httptest.NewRecorder()
	replace.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, DefaultCardPath, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"replacement"`) {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "" {
		t.Fatalf("Cache-Control set without config: %q", rec.Header().Get("Cache-Control"))
	}

	failing := NewServer(ServerConfig{Card: testCard(false),
		CardModifier: func(r *http.Request, c *AgentCard) (*AgentCard, error) {
			return nil, errors.New("secret detail")
		}})
	rec = httptest.NewRecorder()
	failing.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, DefaultCardPath, nil))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("failing: %d %q", rec.Code, rec.Body.String())
	}
}

// TestClientSelectedInterfaceOnAnotherHost: the card is served by one host,
// the selected interface lives on another; requests go to the interface URL
// verbatim (JSON-RPC) or as its REST base, and never reach the card host.
func TestClientSelectedInterfaceOnAnotherHost(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name        string
		binding     string
		transport   string
		pathPrefix  string // mounted on the interface host
		trailing    string // appended to the interface URL on the card
		wantRequest string // on the interface host
		exactPath   bool   // pathPrefix is the whole JSON-RPC endpoint
	}{
		{"jsonrpc host root", ProtocolBindingJSONRPC, TransportJSONRPC, "", "", "POST /", false},
		{"jsonrpc host root slash", ProtocolBindingJSONRPC, TransportJSONRPC, "", "/", "POST /", false},
		{"jsonrpc under path", ProtocolBindingJSONRPC, TransportJSONRPC, "/v1", "/v1/", "POST /v1/", false},
		{"jsonrpc exact path no slash", ProtocolBindingJSONRPC, TransportJSONRPC, "/rpc", "/rpc", "POST /rpc", true},
		{"rest host root", ProtocolBindingREST, TransportREST, "", "", "POST /message:send", false},
		{"rest host root slash", ProtocolBindingREST, TransportREST, "", "/", "POST /message:send", false},
		{"rest under path", ProtocolBindingREST, TransportREST, "/v1", "/v1/", "POST /v1/message:send", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ifaceLog := &wireLog{}
			api := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler}).Handler()
			switch {
			case tc.exactPath:
				// A JSON-RPC endpoint is the exact URL: only "/rpc" answers,
				// and it reaches the server's root route.
				inner := api
				api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != tc.pathPrefix {
						http.NotFound(w, r)
						return
					}
					r.URL.Path = "/"
					inner.ServeHTTP(w, r)
				})
			case tc.pathPrefix != "":
				api = http.StripPrefix(tc.pathPrefix, api)
			}
			ifaceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ifaceLog.add(r)
				api.ServeHTTP(w, r)
			}))
			defer ifaceSrv.Close()

			baseLog := &wireLog{}
			card := testCard(false)
			card.SupportedInterfaces = []AgentInterface{
				{URL: ifaceSrv.URL + tc.trailing, ProtocolBinding: tc.binding, ProtocolVersion: "1.0"},
			}
			cardSrv := NewServer(ServerConfig{Card: card})
			baseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				baseLog.add(r)
				if r.URL.Path != DefaultCardPath {
					http.Error(w, "the card host serves no RPC", http.StatusTeapot)
					return
				}
				cardSrv.ServeHTTP(w, r)
			}))
			defer baseSrv.Close()

			// No WithTransport: the card's only interface wins by fallback.
			c := NewClient(baseSrv.URL)
			if err := c.Connect(ctx); err != nil {
				t.Fatalf("Connect: %v", err)
			}
			reply, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("far")))
			if err != nil {
				t.Fatalf("SendMessage: %v (interface log %v, base log %v)", err, ifaceLog.snapshot(), baseLog.snapshot())
			}
			if textOfMessage(reply) != "echo: far" {
				t.Fatalf("reply = %q", textOfMessage(reply))
			}
			if _, err := c.TasksGet(ctx, reply.TaskID); err != nil {
				t.Fatalf("TasksGet: %v", err)
			}
			if got := ifaceLog.snapshot(); len(got) != 2 || got[0] != tc.wantRequest {
				t.Fatalf("interface host log = %v, want first %q and 2 requests", got, tc.wantRequest)
			}
			if got := baseLog.snapshot(); len(got) != 1 || got[0] != "GET "+DefaultCardPath {
				t.Fatalf("card host log = %v, want the card fetch only", got)
			}
		})
	}
}

// TestClientInterfaceWithoutURLUsesBase: an interface that omits its URL is
// reached through the client's base URL, on both bindings.
func TestClientInterfaceWithoutURLUsesBase(t *testing.T) {
	ctx := context.Background()
	for _, transport := range []string{TransportJSONRPC, TransportREST} {
		t.Run(transport, func(t *testing.T) {
			url, log := startDualServer(t, nil, func(_ string, card *AgentCard) {
				for i := range card.SupportedInterfaces {
					card.SupportedInterfaces[i].URL = ""
				}
			})
			c := NewClient(url, WithTransport(transport))
			if _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("a"))); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			want := "POST /"
			if transport == TransportREST {
				want = "POST /message:send"
			}
			if log.count(want) != 1 {
				t.Fatalf("wire log = %v, want %q", log.snapshot(), want)
			}
		})
	}
}

// TestClientRESTRequestHeaders: REST requests carry the binding media type —
// Accept always, Content-Type on bodies only.
func TestClientRESTRequestHeaders(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	type seen struct{ contentType, accept string }
	headers := map[string]seen{}
	var inner http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers[r.Method+" "+r.URL.Path] = seen{r.Header.Get("Content-Type"), r.Header.Get("Accept")}
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	defer ts.Close()
	card := testCard(false)
	card.URL = ts.URL
	card.SupportedInterfaces = []AgentInterface{{URL: ts.URL, ProtocolBinding: ProtocolBindingREST, ProtocolVersion: "1.0"}}
	store := NewMemoryTaskStore()
	inner = NewServer(ServerConfig{Card: card, Handle: echoHandler, Store: store}).Handler()
	if err := store.Save(ctx, &Task{ID: "t1", Status: TaskStatus{State: TaskStateWorking}}); err != nil {
		t.Fatal(err)
	}

	c := NewClient(ts.URL, WithTransport(TransportREST))
	reply, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("h")))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if _, err := c.TasksGet(ctx, reply.TaskID); err != nil {
		t.Fatalf("TasksGet: %v", err)
	}
	if _, err := c.TasksCancel(ctx, "t1"); err != nil {
		t.Fatalf("TasksCancel: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := map[string]seen{
		"POST /message:send":         {MediaA2AJSON, MediaA2AJSON},
		"GET /tasks/" + reply.TaskID: {"", MediaA2AJSON},
		"POST /tasks/t1:cancel":      {"", MediaA2AJSON},
	}
	for key, w := range want {
		if got := headers[key]; got != w {
			t.Errorf("%s: content-type/accept = %+v, want %+v", key, got, w)
		}
	}
}

// TestClientRESTErrorWithoutEnvelope: a non-2xx answer that carries no
// {"error": {...}} body is a plain transport error with the status and body
// text — never mistaken for a task sentinel.
func TestClientRESTErrorWithoutEnvelope(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"plain text 400", http.StatusBadRequest, "bad things\n"},
		{"json without error key", http.StatusNotFound, `{"detail":"nope"}`},
		{"empty 502", http.StatusBadGateway, ""},
		{"300 without Location", http.StatusMultipleChoices, "many choices"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			card := &AgentCard{Name: "x", SupportedInterfaces: []AgentInterface{
				{URL: ts.URL, ProtocolBinding: ProtocolBindingREST, ProtocolVersion: "1.0"},
			}}
			c := NewClient(ts.URL, WithPresetCard(card))
			for name, call := range map[string]func() error{
				"send":   func() error { _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("x"))); return err },
				"get":    func() error { _, err := c.TasksGet(ctx, "t"); return err },
				"cancel": func() error { _, err := c.TasksCancel(ctx, "t"); return err },
			} {
				err := call()
				if !errors.Is(err, ErrTransport) {
					t.Fatalf("%s: err = %v, want ErrTransport", name, err)
				}
				if errors.Is(err, ErrTaskNotFound) || errors.Is(err, ErrRPC) {
					t.Fatalf("%s: err = %v misread as an RPC sentinel", name, err)
				}
				if !strings.Contains(err.Error(), fmt.Sprint(tc.status)) || !strings.Contains(err.Error(), strings.TrimSpace(tc.body)) {
					t.Fatalf("%s: err = %q lacks status %d / body %q", name, err, tc.status, tc.body)
				}
			}
		})
	}
}

// TestClientConnectOnce: a successful Connect is remembered — repeated
// Connect and later calls never fetch the card again.
func TestClientConnectOnce(t *testing.T) {
	ctx := context.Background()
	url, log := startDualServer(t, nil, nil)
	c := NewClient(url, WithTransport(TransportREST))
	for i := 0; i < 3; i++ {
		if err := c.Connect(ctx); err != nil {
			t.Fatalf("Connect #%d: %v", i, err)
		}
	}
	if _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("a"))); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if n := log.count("GET " + DefaultCardPath); n != 1 {
		t.Fatalf("card fetched %d times, want 1 (log %v)", n, log.snapshot())
	}
}

// TestClientSelectionMatchesBaseWithTrailingSlash: the base-URL match ignores
// a trailing slash on the card's interface URL, even when that interface is
// not the first listed.
func TestClientSelectionMatchesBaseWithTrailingSlash(t *testing.T) {
	ctx := context.Background()
	url, log := startDualServer(t, nil, func(u string, card *AgentCard) {
		card.SupportedInterfaces = []AgentInterface{
			{URL: "http://elsewhere.invalid", ProtocolBinding: ProtocolBindingREST, ProtocolVersion: "1.0"},
			{URL: u + "/", ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: "1.0"},
		}
	})
	c := NewClient(url)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("a"))); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if log.count("POST /") != 1 || log.count("POST /message:send") != 0 {
		t.Fatalf("wire log = %v, want JSON-RPC through the matching interface", log.snapshot())
	}
}

// TestClientRESTEscapesTaskID: task ids are path-escaped on get and cancel,
// so reserved characters cannot split the path.
func TestClientRESTEscapesTaskID(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore()
	url, _ := startDualServer(t, store, nil)
	const id = "t?x#y"
	if err := store.Save(ctx, &Task{ID: id, Status: TaskStatus{State: TaskStateWorking}}); err != nil {
		t.Fatal(err)
	}
	c := NewClient(url, WithTransport(TransportREST))
	got, err := c.TasksGet(ctx, id)
	if err != nil || got.ID != id {
		t.Fatalf("TasksGet: %+v, %v", got, err)
	}
	canceled, err := c.TasksCancel(ctx, id)
	if err != nil || canceled.Status.State != TaskStateCanceled {
		t.Fatalf("TasksCancel: %+v, %v", canceled, err)
	}
}
