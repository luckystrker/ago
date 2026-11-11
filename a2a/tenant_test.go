package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// tenantStores resolves one MemoryTaskStore per tenant, the arrangement
// ServerConfig.StoreForTenant expects. An unknown tenant resolves to nil.
type tenantStores struct {
	mu sync.Mutex
	m  map[string]TaskStore
}

func newTenantStores(tenants ...string) *tenantStores {
	stores := &tenantStores{m: make(map[string]TaskStore, len(tenants))}
	for _, tenant := range tenants {
		stores.m[tenant] = NewMemoryTaskStore()
	}
	return stores
}

func (s *tenantStores) resolve(tenant string) TaskStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[tenant]
}

func (s *tenantStores) count(t *testing.T, tenant string) int {
	t.Helper()
	store := s.resolve(tenant)
	if store == nil {
		t.Fatalf("no store for tenant %q", tenant)
	}
	tasks, err := store.(TaskLister).List(context.Background())
	if err != nil {
		t.Fatalf("list %q store: %v", tenant, err)
	}
	return len(tasks)
}

// tenantServer builds a server whose tenants are isolated by store.
func tenantServer(t *testing.T, stores *tenantStores) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:           testCard(false),
		Handle:         echoHandler,
		StoreForTenant: stores.resolve,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// sendTenantRPC posts one JSON-RPC body whose params carry the given tenant.
func sendTenantRPC(t *testing.T, h http.Handler, method, tenant, params string) rpcResponse {
	t.Helper()
	if tenant != "" && params != "" {
		body := strings.TrimPrefix(params, "{")
		if strings.TrimSpace(body) == "}" {
			params = `{"tenant":"` + tenant + `"}`
		} else {
			params = `{"tenant":"` + tenant + `",` + body
		}
	}
	return postRPC(t, h, rpcBody(method, params, "1"))
}

// TestTenantsAreIsolated: two tenants on one server cannot read, cancel or
// list each other's tasks.
func TestTenantsAreIsolated(t *testing.T) {
	stores := newTenantStores("acme", "beta")
	srv := tenantServer(t, stores)

	sent := sendTenantRPC(t, srv.Config.Handler, MethodMessageSend, "acme", textMessageParams("hi"))
	if sent.Error != nil {
		t.Fatalf("acme send: %v", sent.Error)
	}
	var reply Message
	if err := json.Unmarshal(sent.Result, &reply); err != nil {
		t.Fatalf("decode acme reply: %v", err)
	}
	if reply.TaskID == "" {
		t.Fatalf("acme reply carries no task id: %s", sent.Result)
	}
	if got := stores.count(t, "acme"); got != 1 {
		t.Fatalf("acme store holds %d tasks, want 1", got)
	}
	if got := stores.count(t, "beta"); got != 0 {
		t.Fatalf("beta store holds %d tasks, want 0", got)
	}

	got := sendTenantRPC(t, srv.Config.Handler, MethodTasksGet, "beta", `{"id":"`+reply.TaskID+`"}`)
	if got.Error == nil || got.Error.Code != codeTaskNotFound {
		t.Fatalf("beta tasks/get of an acme task answered %#v, want %d not found", got.Error, codeTaskNotFound)
	}
	canceled := sendTenantRPC(t, srv.Config.Handler, MethodTasksCancel, "beta", `{"id":"`+reply.TaskID+`"}`)
	if canceled.Error == nil || canceled.Error.Code != codeTaskNotFound {
		t.Fatalf("beta tasks/cancel of an acme task answered %#v, want %d not found", canceled.Error, codeTaskNotFound)
	}
	listed := sendTenantRPC(t, srv.Config.Handler, MethodTasksList, "beta", `{}`)
	if listed.Error != nil {
		t.Fatalf("beta tasks/list: %v", listed.Error)
	}
	var page ListTasksResult
	if err := json.Unmarshal(listed.Result, &page); err != nil {
		t.Fatalf("decode beta task list: %v", err)
	}
	if len(page.Tasks) != 0 {
		t.Fatalf("beta lists %d tasks, want none (acme's task must not leak)", len(page.Tasks))
	}

	own := sendTenantRPC(t, srv.Config.Handler, MethodTasksGet, "acme", `{"id":"`+reply.TaskID+`"}`)
	if own.Error != nil {
		t.Fatalf("acme tasks/get of its own task: %v", own.Error)
	}
	if task, err := stores.resolve("acme").Get(context.Background(), reply.TaskID); err != nil || task.ID != reply.TaskID {
		t.Fatalf("acme store lookup: task=%v err=%v", task, err)
	}
}

// TestTenantRESTPrefix: the tenant may ride the path (the SDK's
// Mount(path="/{tenant}")) and scopes the REST routes the same way.
func TestTenantRESTPrefix(t *testing.T) {
	stores := newTenantStores("acme", "beta")
	srv := tenantServer(t, stores)

	body, err := json.Marshal(NewMessage(RoleUser, NewTextPart("hi")))
	if err != nil {
		t.Fatalf("encode message: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/acme"+RESTMessageSendPath, bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /acme/message:send = %d (%s)", rec.Code, rec.Body.String())
	}
	var reply Message
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if got := stores.count(t, "acme"); got != 1 {
		t.Fatalf("acme store holds %d tasks, want 1", got)
	}

	get := func(path string) *httptest.ResponseRecorder {
		out := httptest.NewRecorder()
		srv.Config.Handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, path, nil))
		return out
	}
	if got := get("/beta" + RESTTasksPath + "/" + reply.TaskID); got.Code != http.StatusNotFound {
		t.Fatalf("GET /beta/tasks/{acme task} = %d, want 404", got.Code)
	}
	if got := get("/acme" + RESTTasksPath + "/" + reply.TaskID); got.Code != http.StatusOK {
		t.Fatalf("GET /acme/tasks/{own task} = %d, want 200 (%s)", got.Code, got.Body.String())
	}

	count := func(path string) int {
		out := get(path)
		if out.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (%s)", path, out.Code, out.Body.String())
		}
		var page ListTasksResult
		if err := json.Unmarshal(out.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return len(page.Tasks)
	}
	if got := count("/acme" + RESTTasksPath); got != 1 {
		t.Fatalf("GET /acme/tasks lists %d tasks, want 1", got)
	}
	if got := count("/beta" + RESTTasksPath); got != 0 {
		t.Fatalf("GET /beta/tasks lists %d tasks, want 0", got)
	}

	cancel := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(cancel, httptest.NewRequest(http.MethodPost,
		"/beta"+RESTTasksPath+"/"+reply.TaskID+":cancel", nil))
	if cancel.Code != http.StatusNotFound {
		t.Fatalf("POST /beta/tasks/{acme task}:cancel = %d, want 404", cancel.Code)
	}
}

// A tenant the resolver does not know is a client error, not an internal one.
func TestUnknownTenantRejected(t *testing.T) {
	stores := newTenantStores("acme")
	srv := tenantServer(t, stores)

	got := sendTenantRPC(t, srv.Config.Handler, MethodTasksList, "nope", `{}`)
	if got.Error == nil || got.Error.Code != codeInvalidParams {
		t.Fatalf("unknown tenant answered %#v, want %d invalid params", got.Error, codeInvalidParams)
	}

	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope"+RESTTasksPath, nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET /nope/tasks = %d, want 400", rec.Code)
	}
}

// TestTenantNameIsInertWithoutResolver keeps the zero-value behavior: a server
// with no tenant resolver answers exactly as it did before tenants existed.
func TestTenantNameIsInertWithoutResolver(t *testing.T) {
	srv := httptest.NewServer(NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler}).Handler())
	t.Cleanup(srv.Close)

	sent := sendTenantRPC(t, srv.Config.Handler, MethodMessageSend, "acme", textMessageParams("hi"))
	if sent.Error != nil {
		t.Fatalf("send with an inert tenant: %v", sent.Error)
	}
	var reply Message
	if err := json.Unmarshal(sent.Result, &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	got := postRPC(t, srv.Config.Handler, rpcBody(MethodTasksGet, `{"id":"`+reply.TaskID+`"}`, "2"))
	if got.Error != nil {
		t.Fatalf("tasks/get without a tenant: %v", got.Error)
	}
}

// TestTenantCardInterfaceAndClientAdoption: the server stamps the tenant it
// serves on the interface it generates, and a client that resolves that card
// sends the tenant it found.
func TestTenantCardInterfaceAndClientAdoption(t *testing.T) {
	stores := newTenantStores("acme", "beta")
	defaultStore := NewMemoryTaskStore()
	serverCard := testCard(false)
	// No absolute URL: the generated interface points the client back at its
	// own base URL, which is where the test server listens.
	serverCard.URL = ""
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:           serverCard,
		Handle:         echoHandler,
		Tenant:         "acme",
		Store:          defaultStore,
		StoreForTenant: stores.resolve,
	}).Handler())
	t.Cleanup(srv.Close)

	fetched, err := NewClient(srv.URL).AgentCard(context.Background())
	if err != nil {
		t.Fatalf("fetch card: %v", err)
	}
	if len(fetched.SupportedInterfaces) != 1 || fetched.SupportedInterfaces[0].Tenant != "acme" {
		t.Fatalf("card interfaces = %#v, want the served tenant stamped", fetched.SupportedInterfaces)
	}

	client := NewClient(srv.URL, WithPresetCard(fetched))
	if _, err := client.SendMessage(context.Background(), NewMessage(RoleUser, NewTextPart("hi"))); err != nil {
		t.Fatalf("send through the adopted tenant: %v", err)
	}
	if got := stores.count(t, "acme"); got != 1 {
		t.Fatalf("acme store holds %d tasks, want 1 (the client must send the adopted tenant)", got)
	}
	if got := stores.count(t, "beta"); got != 0 {
		t.Fatalf("beta store holds %d tasks, want 0", got)
	}
	if tasks, err := defaultStore.List(context.Background()); err != nil || len(tasks) != 0 {
		t.Fatalf("default store holds %d tasks, want none", len(tasks))
	}
}

// TestClientTenantOverREST: an explicit tenant reaches a REST server through
// the path prefix.
func TestClientTenantOverREST(t *testing.T) {
	stores := newTenantStores("acme")
	card := testCard(false)
	// A card declaring only the REST binding: the client resolves it and
	// falls back to its own base URL, which is where the server listens.
	card.SupportedInterfaces = []AgentInterface{{
		ProtocolBinding: ProtocolBindingREST,
		ProtocolVersion: ProtocolVersionCurrent,
		Tenant:          "acme",
	}}
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:           card,
		Handle:         echoHandler,
		StoreForTenant: stores.resolve,
	}).Handler())
	t.Cleanup(srv.Close)

	client := NewClient(srv.URL, WithTransport(TransportREST), WithTenant("acme"))
	reply, err := client.SendMessage(context.Background(), NewMessage(RoleUser, NewTextPart("hi")))
	if err != nil {
		t.Fatalf("REST send with a tenant: %v", err)
	}
	if got := stores.count(t, "acme"); got != 1 {
		t.Fatalf("acme store holds %d tasks, want 1", got)
	}
	if _, err := client.TasksGet(context.Background(), reply.TaskID); err != nil {
		t.Fatalf("REST tasks/get with a tenant: %v", err)
	}
	page, err := client.TasksList(context.Background(), ListTasksParams{})
	if err != nil {
		t.Fatalf("REST tasks/list with a tenant: %v", err)
	}
	if len(page.Tasks) != 1 {
		t.Fatalf("REST tasks/list returned %d tasks, want 1", len(page.Tasks))
	}
}

// authResolverServer builds a server whose tenant resolver authorizes the
// tenant against the authenticated identity: alice may use "acme" only.
// Every call is recorded so a test can check what the resolver saw.
type resolverRecord struct {
	tenant   string
	identity any
	trace    string
}

func authResolverServer(t *testing.T, stores *tenantStores, seen *[]resolverRecord, seenMu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:   testCard(false),
		Handle: echoHandler,
		AuthorizeIdentity: func(r *http.Request) (any, error) {
			caller := r.Header.Get("X-Caller")
			if caller == "" {
				return nil, errors.New("no caller")
			}
			return caller, nil
		},
		TenantResolver: func(ctx context.Context, req *TenantRequest) (string, error) {
			seenMu.Lock()
			*seen = append(*seen, resolverRecord{
				tenant:   req.Tenant,
				identity: req.Identity,
				trace:    req.Request.Header.Get("X-Trace"),
			})
			seenMu.Unlock()
			if IdentityFrom(ctx) != req.Identity {
				return "", errors.New("identity missing from context")
			}
			if req.Identity != "alice" || req.Tenant != "acme" {
				return "", errors.New("tenant does not belong to caller")
			}
			return req.Tenant, nil
		},
		StoreForTenant: stores.resolve,
		PushStore:      NewMemoryPushConfigStore(),
	}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// callRPC sends one JSON-RPC body as caller alice with the given tenant.
func callWithCaller(t *testing.T, h http.Handler, method, tenant, params string) rpcResponse {
	t.Helper()
	if tenant != "" && params != "" {
		body := strings.TrimPrefix(params, "{")
		if strings.TrimSpace(body) == "}" {
			params = `{"tenant":"` + tenant + `"}`
		} else {
			params = `{"tenant":"` + tenant + `",` + body
		}
	}
	req := httptest.NewRequest(http.MethodPost, rpcPath, strings.NewReader(rpcBody(method, params, "1")))
	req.Header.Set("X-Caller", "alice")
	req.Header.Set("X-Trace", "trace-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d (%s)", method, rec.Code, rec.Body.String())
	}
	var envelope rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s envelope: %v", method, err)
	}
	return envelope
}

// TestTenantResolverRefusesForeignTenant: with a resolver configured, a caller
// cannot reach a tenant that is not theirs — on send, get, cancel, list and
// push alike, over JSON-RPC and REST.
func TestTenantResolverRefusesForeignTenant(t *testing.T) {
	stores := newTenantStores("acme", "beta")
	var (
		seen   []resolverRecord
		seenMu sync.Mutex
	)
	srv := authResolverServer(t, stores, &seen, &seenMu)

	// Its own tenant works and lands in that tenant's store.
	sent := callWithCaller(t, srv.Config.Handler, MethodMessageSend, "acme", textMessageParams("hi"))
	if sent.Error != nil {
		t.Fatalf("acme send: %v", sent.Error)
	}
	var reply Message
	if err := json.Unmarshal(sent.Result, &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if got := stores.count(t, "acme"); got != 1 {
		t.Fatalf("acme store holds %d tasks, want 1", got)
	}

	// Every surface refuses the foreign tenant with the fixed -32602.
	for _, tc := range []struct{ method, params string }{
		{MethodMessageSend, textMessageParams("hi")},
		{MethodTasksGet, `{"id":"` + reply.TaskID + `"}`},
		{MethodTasksCancel, `{"id":"` + reply.TaskID + `"}`},
		{MethodTasksList, `{}`},
		{MethodPushSet, `{"taskId":"` + reply.TaskID + `","pushNotificationConfig":{"url":"https://example.test/hook"}}`},
	} {
		got := callWithCaller(t, srv.Config.Handler, tc.method, "beta", tc.params)
		if got.Error == nil || got.Error.Code != codeInvalidParams {
			t.Fatalf("%s as beta answered %#v, want %d (tenant not allowed)", tc.method, got.Error, codeInvalidParams)
		}
		if !strings.Contains(got.Error.Message, msgTenantForbidden) {
			t.Fatalf("%s error text = %q, want the fixed refusal", tc.method, got.Error.Message)
		}
	}

	// REST paths: the refusal is a 403, and the acme task is still invisible
	// to beta through the REST twin.
	rest := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Caller", "alice")
		req.Header.Set("X-Trace", "trace-123")
		rec := httptest.NewRecorder()
		srv.Config.Handler.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{
		"/beta" + RESTMessageSendPath,
		"/beta" + RESTTasksPath,
		"/beta" + RESTTasksPath + "/" + reply.TaskID,
		"/beta" + RESTTasksPath + "/" + reply.TaskID + ":cancel",
	} {
		if rec := rest(http.MethodPost, path); rec.Code != http.StatusForbidden && rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s = %d, want 403", path, rec.Code)
		}
	}
	if rec := rest(http.MethodGet, "/beta"+RESTTasksPath); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /beta/tasks = %d, want 403", rec.Code)
	}
	if rec := rest(http.MethodGet, "/beta"+RESTTasksPath+"/"+reply.TaskID); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /beta/tasks/{id} = %d, want 403", rec.Code)
	}
	if rec := rest(http.MethodGet, "/acme"+RESTTasksPath); rec.Code != http.StatusOK {
		t.Fatalf("GET /acme/tasks = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// The public card is not a tenant surface: it serves to anyone the
	// authorize hook admitted.
	if rec := rest(http.MethodGet, DefaultCardPath); rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", DefaultCardPath, rec.Code)
	}

	// The resolver saw the live request and the authenticated identity.
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the tenant resolver was never called")
	}
	for _, rec := range seen {
		if rec.identity != "alice" {
			t.Fatalf("resolver identity = %#v, want alice", rec.identity)
		}
		if rec.trace != "trace-123" {
			t.Fatalf("resolver trace header = %q, want the request's", rec.trace)
		}
	}
}

// TestTenantResolverRunsWithoutClientTenant: the resolver is consulted even
// when the request names no tenant, so a deployment can derive tenancy from
// the identity and never trust the client's claim.
func TestTenantResolverRunsWithoutClientTenant(t *testing.T) {
	stores := newTenantStores("acme")
	defaultStore := NewMemoryTaskStore()
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:   testCard(false),
		Handle: echoHandler,
		AuthorizeIdentity: func(*http.Request) (any, error) {
			return "alice", nil
		},
		TenantResolver: func(_ context.Context, req *TenantRequest) (string, error) {
			if req.Tenant != "" {
				t.Errorf("resolver asked for %q, want the empty client claim", req.Tenant)
			}
			// The client names nothing; the identity decides.
			return "acme", nil
		},
		Store:          defaultStore,
		StoreForTenant: stores.resolve,
	}).Handler())
	t.Cleanup(srv.Close)

	reply, err := NewClient(srv.URL).SendMessage(context.Background(), NewMessage(RoleUser, NewTextPart("hi")))
	if err != nil {
		t.Fatalf("send without a tenant: %v", err)
	}
	if got := stores.count(t, "acme"); got != 1 {
		t.Fatalf("acme store holds %d tasks, want 1 (the resolver's tenant)", got)
	}
	if tasks, err := defaultStore.List(context.Background()); err != nil || len(tasks) != 0 {
		t.Fatalf("default store holds %d tasks, want none", len(tasks))
	}
	if _, err := NewClient(srv.URL).TasksGet(context.Background(), reply.TaskID); err != nil {
		t.Fatalf("tasks/get through the derived tenant: %v", err)
	}
}

// TestTenantLessRESTRoutesStayUntenanted: with tenancy configured, a request
// that names no tenant still uses the plain routes — "/tasks" and
// "/tasks/{id}" must not be mistaken for a tenant segment called "tasks", and
// they address the default (untenanted) store.
func TestTenantLessRESTRoutesStayUntenanted(t *testing.T) {
	stores := newTenantStores("acme")
	defaultStore := NewMemoryTaskStore()
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:           testCard(false),
		Handle:         echoHandler,
		Store:          defaultStore,
		StoreForTenant: stores.resolve,
	}).Handler())
	t.Cleanup(srv.Close)

	// One task in acme, none in the default store.
	sent := sendTenantRPC(t, srv.Config.Handler, MethodMessageSend, "acme", textMessageParams("hi"))
	if sent.Error != nil {
		t.Fatalf("acme send: %v", sent.Error)
	}
	var reply Message
	if err := json.Unmarshal(sent.Result, &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.Config.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := get(RESTTasksPath); rec.Code != http.StatusOK {
		t.Fatalf("GET /tasks = %d, want 200 (a tenant segment must not be read as %q)", rec.Code, "tasks")
	}
	var page ListTasksResult
	rec := get(RESTTasksPath)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode /tasks: %v", err)
	}
	if len(page.Tasks) != 0 {
		t.Fatalf("GET /tasks lists %d tasks, want the empty default store", len(page.Tasks))
	}
	if rec := get(RESTTasksPath + "/" + reply.TaskID); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /tasks/{acme task} = %d, want 404 (path parsing must not invent a tenant)", rec.Code)
	}
	cancel := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(cancel, httptest.NewRequest(http.MethodPost, RESTTasksPath+"/"+reply.TaskID+":cancel", nil))
	if cancel.Code != http.StatusNotFound {
		t.Fatalf("POST /tasks/{acme task}:cancel = %d, want 404", cancel.Code)
	}
}

// TestTenantParamsLeaveNonObjectsAlone: params that are not a JSON object (a
// bare null, for instance) are passed through untouched rather than patched —
// and never a nil-map write.
func TestTenantParamsLeaveNonObjectsAlone(t *testing.T) {
	for _, params := range []string{"null", `"scalar"`, "[1,2]"} {
		if got := tenantParams([]byte(params), "acme"); string(got) != params {
			t.Fatalf("tenantParams(%s) = %s, want it unchanged", params, got)
		}
	}
	got := tenantParams([]byte(`{"id":"t1"}`), "acme")
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode patched params: %v", err)
	}
	if decoded["tenant"] != "acme" || decoded["id"] != "t1" {
		t.Fatalf("patched params = %v, want tenant plus the existing members", decoded)
	}
	// No tenant configured: nothing is injected (parity: with_tenant only sets
	// the key when there is a tenant).
	if got := tenantParams([]byte(`{"id":"t1"}`), ""); string(got) != `{"id":"t1"}` {
		t.Fatalf("tenantParams with no tenant = %s, want the params unchanged", got)
	}
	if got := tenantParams(nil, "acme"); len(got) != 0 {
		t.Fatalf("tenantParams(nil) = %s, want empty params", got)
	}

	// An explicit tenant on the params wins over the client's default.
	got = tenantParams([]byte(`{"tenant":"explicit"}`), "acme")
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if decoded["tenant"] != "explicit" {
		t.Fatalf("tenant = %v, want the explicit one", decoded["tenant"])
	}
}

// TestTenantResolverWithoutStore: a resolver on its own switches tenant
// resolution on (it decides the tenant even with no per-tenant store), so a
// deployment can use it purely as an access check on the tenant claim.
func TestTenantResolverWithoutStore(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:   testCard(false),
		Handle: echoHandler,
		AuthorizeIdentity: func(*http.Request) (any, error) {
			return "alice", nil
		},
		TenantResolver: func(_ context.Context, req *TenantRequest) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, req.Tenant)
			if req.Tenant != "" && req.Tenant != "acme" {
				return "", errors.New("not your tenant")
			}
			return req.Tenant, nil
		},
	}).Handler())
	t.Cleanup(srv.Close)

	if got := sendTenantRPC(t, srv.Config.Handler, MethodMessageSend, "acme", textMessageParams("hi")); got.Error != nil {
		t.Fatalf("acme send with a resolver and no tenant store: %v", got.Error)
	}
	if got := sendTenantRPC(t, srv.Config.Handler, MethodMessageSend, "beta", textMessageParams("hi")); got.Error == nil || got.Error.Code != codeInvalidParams {
		t.Fatalf("beta send answered %#v, want the resolver's refusal", got.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "acme" || seen[1] != "beta" {
		t.Fatalf("resolver saw %v, want [acme beta]", seen)
	}
}

// TestIdentityReachesHandler: the AuthorizeIdentity result is on the request
// context, so a handler can tell who called without re-authenticating.
func TestIdentityReachesHandler(t *testing.T) {
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card: testCard(false),
		Handle: func(ctx context.Context, msg *Message) (*Message, error) {
			return NewMessage(RoleAgent, NewTextPart("caller:"+IdentityFrom(ctx).(string))), nil
		},
		AuthorizeIdentity: func(r *http.Request) (any, error) {
			return r.Header.Get("X-Caller"), nil
		},
	}).Handler())
	t.Cleanup(srv.Close)

	req := httptest.NewRequest(http.MethodPost, rpcPath, strings.NewReader(messageSendBody("hi", "1")))
	req.Header.Set("X-Caller", "alice")
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	var envelope rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Error != nil {
		t.Fatalf("send: %v", envelope.Error)
	}
	var reply Message
	if err := json.Unmarshal(envelope.Result, &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if got := textOf(&reply); got != "caller:alice" {
		t.Fatalf("handler saw identity %q, want alice", got)
	}
}

// TestAuthorizeAndIdentityBothRun: the identity form does not replace the
// older request gate — a deployment that adds one keeps the other, and either
// can refuse. (Review round 3: silently skipping Authorize would drop an IP
// fence that was there before.)
func TestAuthorizeAndIdentityBothRun(t *testing.T) {
	t.Run("authorize refuses while identity allows", func(t *testing.T) {
		var identityCalls int
		srv := httptest.NewServer(NewServer(ServerConfig{
			Card:   testCard(false),
			Handle: echoHandler,
			Authorize: func(*http.Request) error {
				return errors.New("not from the office network")
			},
			AuthorizeIdentity: func(*http.Request) (any, error) {
				identityCalls++
				return "alice", nil
			},
		}).Handler())
		t.Cleanup(srv.Close)

		rec := httptest.NewRecorder()
		srv.Config.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, DefaultCardPath, nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("card = %d, want 403 (the legacy gate must still run)", rec.Code)
		}
		if identityCalls != 0 {
			t.Fatalf("identity hook ran %d times after the legacy gate refused, want 0", identityCalls)
		}
	})

	t.Run("both allow", func(t *testing.T) {
		var authorizeCalls, identityCalls int
		srv := httptest.NewServer(NewServer(ServerConfig{
			Card:   testCard(false),
			Handle: echoHandler,
			Authorize: func(*http.Request) error {
				authorizeCalls++
				return nil
			},
			AuthorizeIdentity: func(*http.Request) (any, error) {
				identityCalls++
				return "alice", nil
			},
		}).Handler())
		t.Cleanup(srv.Close)

		rec := httptest.NewRecorder()
		srv.Config.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, DefaultCardPath, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("card = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		if authorizeCalls != 1 || identityCalls != 1 {
			t.Fatalf("authorize=%d identity=%d, want both gates to run once", authorizeCalls, identityCalls)
		}
	})
}

// TestSplitTenantPathParses pins the path grammar, including the two shapes the
// third review round asked about: a doubled slash ("//tasks") is not a tenant
// segment — trimming one slash leaves an empty first segment, so the path is
// passed through unchanged — and a trailing slash after the tenant ("/acme/")
// has no route below it, so it is not tenant-scoped either.
func TestSplitTenantPathParses(t *testing.T) {
	cases := []struct {
		path   string
		tenant string
		rest   string
	}{
		{"", "", ""},
		{"/", "", "/"},
		{"//tasks", "", "//tasks"},
		{"/acme/", "", "/acme/"},
		{"/tasks", "", "/tasks"},
		{"/tasks/abc", "", "/tasks/abc"},
		{"/tasks/abc:cancel", "", "/tasks/abc:cancel"},
		{"/message:send", "", "/message:send"},
		{"/extendedAgentCard", "", "/extendedAgentCard"},
		{"/.well-known/agent-card.json", "", "/.well-known/agent-card.json"},
		{"/acme/tasks", "acme", "/tasks"},
		{"/acme/tasks/abc", "acme", "/tasks/abc"},
		{"/acme/tasks/abc:cancel", "acme", "/tasks/abc:cancel"},
		{"/acme/message:send", "acme", "/message:send"},
	}
	for _, tc := range cases {
		tenant, rest := splitTenantPath(tc.path)
		if tenant != tc.tenant || rest != tc.rest {
			t.Fatalf("splitTenantPath(%q) = (%q, %q), want (%q, %q)", tc.path, tenant, rest, tc.tenant, tc.rest)
		}
	}

	// Only the message/task routes are tenant surfaces; "//tasks" and "/acme/"
	// reach the router untouched and answer as plain (unknown) paths.
	for path, want := range map[string]bool{
		"/tasks":                       true,
		"/tasks/abc":                   true,
		"/tasks/abc:cancel":            true,
		"/message:send":                true,
		"/acme/tasks":                  true,
		"/acme/message:send":           true,
		"//tasks":                      false,
		"/acme/":                       false,
		"/extendedAgentCard":           false,
		"/.well-known/agent-card.json": false,
	} {
		if got := tenantSurface(path); got != want {
			t.Fatalf("tenantSurface(%q) = %v, want %v", path, got, want)
		}
	}
}
