package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// tasks/list over JSON-RPC: filters, pages and reports the total, in creation
// order.
func TestServerTasksList(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	h := srv.Handler()

	// Three tasks: two in context "ctx-a", one in "ctx-b".
	for _, body := range []string{
		`{"message":{"role":"user","contextId":"ctx-a","parts":[{"kind":"text","text":"one"}]}}`,
		`{"message":{"role":"user","contextId":"ctx-a","parts":[{"kind":"text","text":"two"}]}}`,
		`{"message":{"role":"user","contextId":"ctx-b","parts":[{"kind":"text","text":"three"}]}}`,
	} {
		if env := postRPC(t, h, rpcBody(MethodMessageSend, body, "1")); env.Error != nil {
			t.Fatalf("send: %+v", env.Error)
		}
	}

	env := postRPC(t, h, rpcBody(MethodTasksList, `{}`, "2"))
	if env.Error != nil {
		t.Fatalf("tasks/list: %+v", env.Error)
	}
	var all ListTasksResult
	if err := json.Unmarshal(env.Result, &all); err != nil {
		t.Fatalf("decode: %v (%s)", err, env.Result)
	}
	if len(all.Tasks) != 3 || all.TotalSize != 3 {
		t.Fatalf("all = %d tasks, total %d", len(all.Tasks), all.TotalSize)
	}
	if all.Tasks[0].Status.State != TaskStateCompleted {
		t.Fatalf("state = %q", all.Tasks[0].Status.State)
	}

	// Context filter.
	env = postRPC(t, h, rpcBody(MethodTasksList, `{"contextId":"ctx-a"}`, "3"))
	var byContext ListTasksResult
	if err := json.Unmarshal(env.Result, &byContext); err != nil {
		t.Fatal(err)
	}
	if len(byContext.Tasks) != 2 || byContext.TotalSize != 2 {
		t.Fatalf("ctx-a = %d tasks, total %d", len(byContext.Tasks), byContext.TotalSize)
	}
	for _, task := range byContext.Tasks {
		if task.ContextID != "ctx-a" {
			t.Fatalf("context filter leaked %q", task.ContextID)
		}
	}

	// Status filter: nothing failed, so an empty page with a zero total.
	env = postRPC(t, h, rpcBody(MethodTasksList, `{"status":"failed"}`, "4"))
	var failed ListTasksResult
	if err := json.Unmarshal(env.Result, &failed); err != nil {
		t.Fatal(err)
	}
	if len(failed.Tasks) != 0 || failed.TotalSize != 0 {
		t.Fatalf("failed = %+v", failed)
	}

	// Pagination: two pages of two, the second reachable only through the
	// token the first returns.
	env = postRPC(t, h, rpcBody(MethodTasksList, `{"pageSize":2}`, "5"))
	var page1 ListTasksResult
	if err := json.Unmarshal(env.Result, &page1); err != nil {
		t.Fatal(err)
	}
	if len(page1.Tasks) != 2 || page1.NextPageToken == "" || page1.TotalSize != 3 {
		t.Fatalf("page 1 = %+v", page1)
	}
	env = postRPC(t, h, rpcBody(MethodTasksList, `{"pageSize":2,"pageToken":"`+page1.NextPageToken+`"}`, "6"))
	var page2 ListTasksResult
	if err := json.Unmarshal(env.Result, &page2); err != nil {
		t.Fatal(err)
	}
	if len(page2.Tasks) != 1 || page2.NextPageToken != "" {
		t.Fatalf("page 2 = %+v", page2)
	}
	if page2.Tasks[0].ID == page1.Tasks[0].ID || page2.Tasks[0].ID == page1.Tasks[1].ID {
		t.Fatal("pages overlap")
	}

	// A page size of one pages one task at a time: the smallest size a client
	// can ask for must page, not fall back to everything.
	env = postRPC(t, h, rpcBody(MethodTasksList, `{"pageSize":1}`, "8"))
	var single ListTasksResult
	if err := json.Unmarshal(env.Result, &single); err != nil {
		t.Fatal(err)
	}
	if len(single.Tasks) != 1 || single.NextPageToken != "1" || single.TotalSize != 3 {
		t.Fatalf("pageSize=1 = %+v", single)
	}
	// Walking the tokens reaches every task exactly once.
	seen := map[string]bool{}
	token := ""
	for i := 0; i < 5; i++ {
		body := `{"pageSize":1`
		if token != "" {
			body += `,"pageToken":"` + token + `"`
		}
		body += `}`
		env = postRPC(t, h, rpcBody(MethodTasksList, body, "9"))
		var page ListTasksResult
		if err := json.Unmarshal(env.Result, &page); err != nil {
			t.Fatal(err)
		}
		for _, task := range page.Tasks {
			if seen[task.ID] {
				t.Fatalf("task %s returned twice while paging", task.ID)
			}
			seen[task.ID] = true
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if len(seen) != 3 {
		t.Fatalf("paging with pageSize=1 saw %d tasks, want 3", len(seen))
	}

	// A token this server never issued is a parameter error.
	env = postRPC(t, h, rpcBody(MethodTasksList, `{"pageToken":"bogus"}`, "7"))
	if env.Error == nil || env.Error.Code != codeInvalidParams {
		t.Fatalf("bogus token: %+v", env.Error)
	}
}

// A store that cannot enumerate answers -32004 rather than pretending the list
// is empty.
func TestServerTasksListNeedsALister(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler, Store: opaqueStore{inner: NewMemoryTaskStore()}})
	env := postRPC(t, srv.Handler(), rpcBody(MethodTasksList, `{}`, "1"))
	if env.Error == nil || env.Error.Code != codeUnsupportedOperation {
		t.Fatalf("want %d, got %+v", codeUnsupportedOperation, env.Error)
	}
}

// opaqueStore is a TaskStore that hides the List extension: a named field, not
// an embedding, so the interface is not promoted.
type opaqueStore struct{ inner *MemoryTaskStore }

func (o opaqueStore) Save(ctx context.Context, task *Task) error { return o.inner.Save(ctx, task) }
func (o opaqueStore) Get(ctx context.Context, id string) (*Task, error) {
	return o.inner.Get(ctx, id)
}
func (o opaqueStore) Delete(ctx context.Context, id string) error { return o.inner.Delete(ctx, id) }

// The client lists over JSON-RPC and over REST, and both see the same tasks.
func TestClientTasksList(t *testing.T) {
	card := testCard(false)
	srv := NewServer(ServerConfig{Card: card, Handle: echoHandler})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	// The served card must declare the REST interface for the REST client to
	// select it.
	card.SupportedInterfaces = []AgentInterface{
		{URL: ts.URL, ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: ProtocolVersionCurrent},
		{URL: ts.URL, ProtocolBinding: ProtocolBindingREST, ProtocolVersion: ProtocolVersionCurrent},
	}
	srv = NewServer(ServerConfig{Card: card, Handle: echoHandler})
	ts.Config.Handler = srv.Handler()

	ctx := context.Background()
	c := NewClient(ts.URL)
	if _, err := c.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("hello"))); err != nil {
		t.Fatalf("send: %v", err)
	}
	listed, err := c.TasksList(ctx, ListTasksParams{})
	if err != nil {
		t.Fatalf("tasks/list: %v", err)
	}
	if len(listed.Tasks) != 1 || listed.TotalSize != 1 {
		t.Fatalf("listed = %+v", listed)
	}
	if listed.Tasks[0].Status.State != TaskStateCompleted {
		t.Fatalf("state = %q", listed.Tasks[0].Status.State)
	}

	// The REST binding answers the same query.
	rest := NewClient(ts.URL, WithTransport(TransportREST))
	restListed, err := rest.TasksList(ctx, ListTasksParams{PageSize: 1})
	if err != nil {
		t.Fatalf("REST tasks/list: %v", err)
	}
	if len(restListed.Tasks) != 1 || restListed.Tasks[0].ID != listed.Tasks[0].ID {
		t.Fatalf("REST listed = %+v", restListed)
	}
}

// The authenticated extended card is served on both surfaces, the public card
// advertises it, and the per-request modifier shapes it.
func TestExtendedCard(t *testing.T) {
	extended := testCard(false)
	extended.Name = "tester-private"
	extended.Description = "the card behind authentication"
	srv := NewServer(ServerConfig{
		Card:         testCard(false),
		Handle:       echoHandler,
		ExtendedCard: &extended,
		ExtendedCardModifier: func(r *http.Request, card *AgentCard) (*AgentCard, error) {
			if tenant := r.Header.Get("X-Tenant"); tenant != "" {
				card.Name = card.Name + "@" + tenant
			}
			return nil, nil // mutate in place
		},
	})
	h := srv.Handler()

	// The public card flips the capability flag and keeps its own identity.
	req := httptest.NewRequest(http.MethodGet, DefaultCardPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var public AgentCard
	if err := json.Unmarshal(rec.Body.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	if !public.Capabilities.ExtendedAgentCard {
		t.Fatal("public card must advertise the extended card")
	}
	if public.Name != "tester" {
		t.Fatalf("public card name = %q", public.Name)
	}

	// JSON-RPC: the extended card, through the modifier.
	env := postRPC(t, h, rpcBody(MethodExtendedCard, `{}`, "1"))
	if env.Error != nil {
		t.Fatalf("extended card: %+v", env.Error)
	}
	var card AgentCard
	if err := json.Unmarshal(env.Result, &card); err != nil {
		t.Fatal(err)
	}
	if card.Name != "tester-private" {
		t.Fatalf("extended card = %+v", card)
	}

	// REST: the same card, and the modifier sees the caller.
	req = httptest.NewRequest(http.MethodGet, ExtendedCardPath, nil)
	req.Header.Set("X-Tenant", "acme")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("REST status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatal(err)
	}
	if card.Name != "tester-private@acme" {
		t.Fatalf("modified card = %+v", card)
	}

	// The modifier receives a copy: the long-lived card is untouched.
	env = postRPC(t, h, rpcBody(MethodExtendedCard, `{}`, "2"))
	if err := json.Unmarshal(env.Result, &card); err != nil {
		t.Fatal(err)
	}
	if card.Name != "tester-private" {
		t.Fatalf("card leaked the modifier's mutation: %q", card.Name)
	}
}

// A server with no extended card answers -32004 / 404, and the client surfaces
// the RPC error.
func TestExtendedCardAbsent(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	env := postRPC(t, ts.Config.Handler, rpcBody(MethodExtendedCard, `{}`, "1"))
	if env.Error == nil || env.Error.Code != codeUnsupportedOperation {
		t.Fatalf("want %d, got %+v", codeUnsupportedOperation, env.Error)
	}
	if _, err := NewClient(ts.URL).ExtendedCard(context.Background()); !errors.Is(err, ErrRPC) {
		t.Fatalf("client err = %v, want ErrRPC", err)
	}

	resp, err := http.Get(ts.URL + ExtendedCardPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("REST status = %d, want 404", resp.StatusCode)
	}
}

// validate_input_modes rejects a part whose media type the card does not
// declare — on every message surface — and leaves a declared one alone.
func TestValidateInputModes(t *testing.T) {
	srv := NewServer(ServerConfig{
		Card:               testCard(false),
		Handle:             echoHandler,
		ValidateInputModes: true,
	})
	h := srv.Handler()

	// text/plain is declared: accepted.
	env := postRPC(t, h, messageSendBody("hello", "1"))
	if env.Error != nil {
		t.Fatalf("declared text part: %+v", env.Error)
	}
	// A data part of an undeclared type is refused before the handler runs.
	body := `{"message":{"role":"user","parts":[{"kind":"data","mediaType":"application/pdf","data":{}}]}}`
	env = postRPC(t, h, rpcBody(MethodMessageSend, body, "2"))
	if env.Error == nil || env.Error.Code != codeInvalidParams {
		t.Fatalf("undeclared data part: %+v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "application/pdf") {
		t.Fatalf("error must name the media type: %q", env.Error.Message)
	}

	// Off, the same part passes (the pre-existing behavior).
	off := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	if env := postRPC(t, off.Handler(), rpcBody(MethodMessageSend, body, "3")); env.Error != nil {
		t.Fatalf("validation off: %+v", env.Error)
	}
}

// The build-time half: a card that declares modes but not the vnd.ag2 types
// would lock AG2 clients out, and the config says so.
func TestInputModesError(t *testing.T) {
	bad := ServerConfig{Card: testCard(false), ValidateInputModes: true}
	err := bad.InputModesError()
	if err == nil {
		t.Fatal("a card without the vnd.ag2 modes must be refused")
	}
	for _, mode := range AG2InputModes {
		if !strings.Contains(err.Error(), mode) {
			t.Fatalf("error must name %q: %v", mode, err)
		}
	}

	good := testCard(false)
	good.DefaultInputModes = append(good.DefaultInputModes, AG2InputModes...)
	if err := (ServerConfig{Card: good, ValidateInputModes: true}).InputModesError(); err != nil {
		t.Fatalf("declared card: %v", err)
	}

	// Declaring nothing accepts everything, as in the SDK.
	if err := (ServerConfig{Card: AgentCard{Name: "bare"}, ValidateInputModes: true}).InputModesError(); err != nil {
		t.Fatalf("empty declaration: %v", err)
	}
	// Validation off never complains.
	if err := (ServerConfig{Card: testCard(false)}).InputModesError(); err != nil {
		t.Fatalf("validation off: %v", err)
	}
	// A skill's inputModes count as declared.
	skill := AgentCard{Name: "skilled", Skills: []AgentSkill{{ID: "s", InputModes: AG2InputModes}}}
	if err := (ServerConfig{Card: skill, ValidateInputModes: true}).InputModesError(); err != nil {
		t.Fatalf("skill modes: %v", err)
	}
}

// A handler that asks for credentials parks the task auth-required (not
// failed): the caller can resume it with the same task id. A handler that
// refuses the task ends it rejected, which is terminal.
func TestTaskStatesAuthRequiredAndRejected(t *testing.T) {
	// The first turn asks for a token, every later turn succeeds.
	var calls int
	var mu sync.Mutex
	handle := func(ctx context.Context, msg *Message) (*Message, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			return nil, fmt.Errorf("%w: present a bearer token", ErrTaskAuthRequired)
		}
		return NewMessage(RoleAgent, NewTextPart("welcome")), nil
	}
	h := NewServer(ServerConfig{Card: testCard(false), Handle: handle}).Handler()

	env := postRPC(t, h, messageSendBody("let me in", "1"))
	if env.Error != nil {
		t.Fatalf("send: %+v", env.Error)
	}
	var parked Task
	if err := json.Unmarshal(env.Result, &parked); err != nil {
		t.Fatalf("parked result is not a Task: %v (%s)", err, env.Result)
	}
	if parked.Status.State != TaskStateAuthRequired {
		t.Fatalf("state = %q, want auth-required", parked.Status.State)
	}
	if got := textOfMessage(parked.Status.Message); !strings.Contains(got, "bearer token") {
		t.Fatalf("parked message = %q", got)
	}
	// auth-required is an interruption, not a terminal state: cancel works.
	if env := postRPC(t, h, rpcBody(MethodTasksCancel, `{"id":"`+parked.ID+`"}`, "2")); env.Error != nil {
		t.Fatalf("cancel of an auth-required task: %+v", env.Error)
	}

	// Rejected: terminal, so a cancel is refused with -32002.
	rejecting := NewServer(ServerConfig{Card: testCard(false), Handle: func(context.Context, *Message) (*Message, error) {
		return nil, fmt.Errorf("%w: off topic", ErrTaskRejected)
	}}).Handler()
	env = postRPC(t, rejecting, messageSendBody("do my taxes", "3"))
	if env.Error != nil {
		t.Fatalf("send: %+v", env.Error)
	}
	var rejected Task
	if err := json.Unmarshal(env.Result, &rejected); err != nil {
		t.Fatalf("rejected result is not a Task: %v (%s)", err, env.Result)
	}
	if rejected.Status.State != TaskStateRejected {
		t.Fatalf("state = %q, want rejected", rejected.Status.State)
	}
	env = postRPC(t, rejecting, rpcBody(MethodTasksCancel, `{"id":"`+rejected.ID+`"}`, "4"))
	if env.Error == nil || env.Error.Code != codeTaskNotCancelable {
		t.Fatalf("cancel of a rejected task: %+v", env.Error)
	}
}

func TestTaskStateTerminality(t *testing.T) {
	cases := map[TaskState]bool{
		TaskStateSubmitted:     false,
		TaskStateWorking:       false,
		TaskStateInputRequired: false,
		TaskStateAuthRequired:  false,
		TaskStateCompleted:     true,
		TaskStateFailed:        true,
		TaskStateCanceled:      true,
		TaskStateRejected:      true,
		TaskStateUnknown:       false,
	}
	for state, want := range cases {
		if got := terminalTaskState(state); got != want {
			t.Errorf("terminalTaskState(%q) = %v, want %v", state, got, want)
		}
	}
	// The wire spellings are the A2A spec's.
	for _, want := range []string{"auth-required", "rejected", "unknown"} {
		if string(TaskState(want)) != want {
			t.Errorf("state constant %q is not its wire spelling", want)
		}
	}
}

// tasks/list works with no params field at all — the method's parameters are
// optional, and a client that sends none asks for the first unfiltered page.
func TestServerTasksListWithoutParams(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	h := srv.Handler()
	if env := postRPC(t, h, messageSendBody("hello", "1")); env.Error != nil {
		t.Fatalf("send: %+v", env.Error)
	}

	for _, body := range []string{
		`{"jsonrpc":"2.0","id":2,"method":"` + MethodTasksList + `"}`,
		`{"jsonrpc":"2.0","id":3,"method":"` + MethodTasksList + `","params":null}`,
		`{"jsonrpc":"2.0","id":4,"method":"` + MethodTasksList + `","params":{}}`,
	} {
		env := postRPC(t, h, body)
		if env.Error != nil {
			t.Fatalf("%s: %+v", body, env.Error)
		}
		var listed ListTasksResult
		if err := json.Unmarshal(env.Result, &listed); err != nil {
			t.Fatalf("%s: decode: %v", body, err)
		}
		if len(listed.Tasks) != 1 || listed.TotalSize != 1 {
			t.Fatalf("%s: listed = %d tasks, total %d", body, len(listed.Tasks), listed.TotalSize)
		}
	}
}

// historyLength truncates the echoed history, and 0 means "no truncation" —
// the full history comes back, which is not the same as "zero messages".
func TestServerTasksListHistoryLength(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	h := srv.Handler()

	env := postRPC(t, h, messageSendBody("first", "1"))
	if env.Error != nil {
		t.Fatalf("send: %+v", env.Error)
	}
	var first Message
	if err := json.Unmarshal(env.Result, &first); err != nil {
		t.Fatal(err)
	}
	if first.TaskID == "" {
		t.Fatalf("first reply carries no task id: %s", env.Result)
	}
	// A continuation under the same task id grows the history to two messages.
	cont := `{"message":{"role":"user","taskId":"` + first.TaskID + `","parts":[{"kind":"text","text":"second"}]}}`
	if env := postRPC(t, h, rpcBody(MethodMessageSend, cont, "2")); env.Error != nil {
		t.Fatalf("continuation: %+v", env.Error)
	}

	history := func(params string) []string {
		t.Helper()
		env := postRPC(t, h, rpcBody(MethodTasksList, params, "3"))
		if env.Error != nil {
			t.Fatalf("%s: %+v", params, env.Error)
		}
		var listed ListTasksResult
		if err := json.Unmarshal(env.Result, &listed); err != nil {
			t.Fatal(err)
		}
		if len(listed.Tasks) != 1 {
			t.Fatalf("%s: %d tasks", params, len(listed.Tasks))
		}
		texts := make([]string, 0, len(listed.Tasks[0].History))
		for _, msg := range listed.Tasks[0].History {
			texts = append(texts, textOfMessage(&msg))
		}
		return texts
	}

	if got := history(`{}`); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("unfiltered history = %v, want [first second]", got)
	}
	if got := history(`{"historyLength":0}`); len(got) != 2 || got[1] != "second" {
		t.Fatalf("historyLength=0 returned %v, want the full [first second]", got)
	}
	// Truncation keeps the most recent messages, not the oldest.
	if got := history(`{"historyLength":1}`); len(got) != 1 || got[0] != "second" {
		t.Fatalf("historyLength=1 returned %v, want [second]", got)
	}
	// The REST twin reads the same parameter.
	req := httptest.NewRequest(http.MethodGet, RESTTasksPath+"?historyLength=0", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("REST status = %d", rec.Code)
	}
	var restListed ListTasksResult
	if err := json.Unmarshal(rec.Body.Bytes(), &restListed); err != nil {
		t.Fatal(err)
	}
	if len(restListed.Tasks) != 1 || len(restListed.Tasks[0].History) != 2 {
		t.Fatalf("REST historyLength=0: %d tasks, %d messages", len(restListed.Tasks), len(restListed.Tasks[0].History))
	}
}

// A query parameter that is not a number is a 400 with the JSON-RPC error
// object in the body, not a silently ignored filter.
func TestRESTTasksListRejectsBadNumbers(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	h := srv.Handler()
	for _, query := range []string{"pageSize=abc", "pageSize=-1", "historyLength=ten", "historyLength=-2"} {
		req := httptest.NewRequest(http.MethodGet, RESTTasksPath+"?"+query, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body %s)", query, rec.Code, rec.Body.String())
		}
		var env struct {
			Error *RPCError `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: decode error body: %v (%s)", query, err, rec.Body.String())
		}
		if env.Error == nil || env.Error.Code != codeInvalidParams {
			t.Fatalf("%s: error = %+v", query, env.Error)
		}
	}
}

// includeArtifacts is accepted on both surfaces. AGo tasks carry no artifacts,
// so the flag changes nothing — the test pins that it is accepted (not a
// parameter error) and that the result is identical with and without it.
func TestTasksListIncludeArtifacts(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	h := srv.Handler()
	if env := postRPC(t, h, messageSendBody("hello", "1")); env.Error != nil {
		t.Fatalf("send: %+v", env.Error)
	}

	ids := func(body string) []string {
		t.Helper()
		env := postRPC(t, h, body)
		if env.Error != nil {
			t.Fatalf("%s: %+v", body, env.Error)
		}
		var listed ListTasksResult
		if err := json.Unmarshal(env.Result, &listed); err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(listed.Tasks))
		for i, task := range listed.Tasks {
			out[i] = task.ID
		}
		return out
	}
	plain := ids(rpcBody(MethodTasksList, `{}`, "2"))
	withArtifacts := ids(rpcBody(MethodTasksList, `{"includeArtifacts":true}`, "3"))
	if len(plain) != 1 || len(withArtifacts) != 1 || plain[0] != withArtifacts[0] {
		t.Fatalf("includeArtifacts changed the result: %v vs %v", plain, withArtifacts)
	}

	req := httptest.NewRequest(http.MethodGet, RESTTasksPath+"?includeArtifacts=true", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("REST includeArtifacts status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var restListed ListTasksResult
	if err := json.Unmarshal(rec.Body.Bytes(), &restListed); err != nil {
		t.Fatal(err)
	}
	if len(restListed.Tasks) != 1 || restListed.Tasks[0].ID != plain[0] {
		t.Fatalf("REST includeArtifacts result = %+v", restListed)
	}
}
