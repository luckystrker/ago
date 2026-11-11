package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	agoevents "github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/uuid"
)

// Card routes. DefaultCardPath is the A2A 1.x discovery path
// (ag2/ag2/a2a/transports/_common.py:78); LegacyCardPath is the v0.x alias
// kept for pre-1.0 clients (_common.py:77). Both serve the same card.
const (
	DefaultCardPath = "/.well-known/agent-card.json"
	LegacyCardPath  = "/.well-known/agent.json"
	// ExtendedCardPath is the REST route of the authenticated card (A2A spec
	// §7.10; the a2a-sdk's rest_routes mount the same path).
	ExtendedCardPath = "/extendedAgentCard"
)

// rpcPath is the single JSON-RPC endpoint (parity: build_jsonrpc rpc_url="/").
const rpcPath = "/"

// maxRequestBody caps one JSON-RPC body; a well-formed A2A exchange is far
// below this, and the cap keeps a runaway client from pinning server memory.
const maxRequestBody = 8 << 20 // 8 MiB

// msgInternalError is the fixed text for wire errors whose real cause is
// server-internal (a handler failure, a task-store fault): internal detail —
// driver texts, resolved paths, handler error chains — stays off the wire.
// The failed task record keeps the full text for the operator (tasks/get).
const msgInternalError = "internal error"

// ServerConfig wires a Server.
type ServerConfig struct {
	// Card is served verbatim from the discovery routes. Its
	// Capabilities.Streaming flag gates message/stream.
	Card AgentCard
	// CardSigner signs every card the server serves, after the discovery
	// defaults and capability flips are applied and after any per-request
	// CardModifier/ExtendedCardModifier has rewritten it (parity:
	// transports/_common.py prepare_public_card / wrap_card_modifier). Nil
	// serves the cards as configured. Signing a card that already carries
	// signatures replaces them, so a composed signer (key rotation) lands one
	// signature per key and no stale one rides along.
	CardSigner CardSigner
	// Handle turns one incoming user Message into the agent's reply Message.
	// It runs per request; implementations must be safe for concurrent use.
	// A nil Handle answers plain message/send and message/stream with a
	// JSON-RPC internal error wrapping ErrNoHandler.
	Handle func(ctx context.Context, msg *Message) (*Message, error)
	// HandleStream drives message/stream when set: it replaces Handle for
	// streaming turns and receives a StreamEmitter for the progress the client
	// sees live — artifact chunks (StreamEmitter.Artifact) and non-terminal
	// state transitions (StreamEmitter.Status). Its return value ends the
	// turn exactly like Handle's, and the server streams the terminal status
	// update. Chunks are folded into the task record as they are emitted, so
	// tasks/get and a resubscribe see the same artifacts the stream carried.
	// Nil keeps the two-event stream (working, terminal) over Handle.
	// Implementations must be safe for concurrent use.
	HandleStream func(ctx context.Context, msg *Message, emit StreamEmitter) (*Message, error)
	// HandleClientTools serves the urn:ag2:client-tools:v1 extension (D-056):
	// when the incoming message announces client-side tools (a tool-schemas
	// data part), it replaces Handle for that turn. Returning calls parks the
	// task input-required with the calls attached on the status message — the
	// client executes them locally and resumes the task — while returning no
	// calls completes the task with reply directly (parity: executor.py's
	// pending_client_calls → updater.requires_input branch).
	HandleClientTools func(ctx context.Context, msg *Message, schemas []ToolSchema) (reply *Message, calls []ToolCall, err error)
	// HandleToolResults resumes a task parked input-required: the client came
	// back with tool-result data parts under the same task id. The returned
	// message completes the task (parity: executor.py's ToolResultsEvent
	// continuation leg). A message carrying results without this callback
	// fails the turn.
	HandleToolResults func(ctx context.Context, msg *Message, results []ToolResultEntry) (*Message, error)
	// RestoreHistory engages full-history shipping on continuations (parity:
	// executor.py's `stream.history.replace(parsed.history_events)`): when the
	// incoming message carries ag2.history+json data parts, the server decodes
	// them once per turn, in wire order, and hands the events here together
	// with the task the turn belongs to, before any handler runs. The hook is
	// where an embedder re-hydrates its own conversation state for the task;
	// the A2A layer itself stays stateless on AG2 history. Nil ignores the
	// parts (the pre-shipping behavior: history rode the wire but never
	// reached the server's handler). An error fails the turn as an internal
	// error, with the text kept off the wire.
	//
	// AGO-DEVIATION: Python's executor replaces the history of the per-request
	// MemoryStream (`stream.history.replace`); AGo's server owns no agent
	// stream — embedders keep their own conversation state — so the decode is
	// handed to a hook instead of written into a stream this package would
	// have to own.
	RestoreHistory func(ctx context.Context, taskID string, history []agoevents.Event) error
	// SyncVariables engages the context-variable sync (D-056): when the
	// incoming message carries a context_update metadata delta, it is
	// stripped of reserved prefixes and handed here keyed by task id; the
	// returned final set is stripped and attached to the reply under the same
	// key (parity: executor.py merged_variables / _build_final_message over
	// mappers/messages.py extract_context_update). Nil disables the sync.
	SyncVariables func(ctx context.Context, taskID string, incoming map[string]any) (outgoing map[string]any, err error)
	// PushStore enables the push-notification surface. The served card states
	// the capability either way — pushNotifications is always set, to "the
	// store exists", and marked present, which is what AG2 signs (server.py:218).
	// Nil leaves the push JSON-RPC methods answering -32004.
	PushStore PushConfigStore
	// PushSender delivers a terminal task to one subscriber webhook; nil
	// defaults to DefaultPushSender. Delivery is best-effort and asynchronous.
	PushSender PushSender
	// PushURLValidator rejects webhook URLs at set time; nil defaults to
	// DefaultPushURLValidator, which refuses loopback/private/link-local
	// targets (SSRF gate). A custom func replaces the default policy
	// wholesale — parity with server.py push_url_validator ends there:
	// the a2a-sdk validates nothing unless the app supplies one.
	PushURLValidator func(url string) bool
	// Store persists tasks for tasks/get and tasks/cancel. Nil defaults to
	// NewMemoryTaskStore.
	Store TaskStore
	// Tenant names the tenant this server serves when a request names none.
	// It is stamped on the discovery interface the server generates, so a
	// client resolving that interface adopts it. Meaningful only together
	// with StoreForTenant: without a resolver the server keeps one store and
	// tenant names are inert.
	Tenant string
	// StoreForTenant resolves the TaskStore a tenant's tasks live in, which is
	// what keeps tenants apart (parity: the a2a-sdk's owner-resolved
	// InMemoryTaskStore — a per-tenant dictionary keyed from the call context).
	// When set, every request naming a tenant (JSON-RPC params "tenant", the
	// REST "/{tenant}/..." prefix, or Tenant) reads and writes that tenant's
	// store; a tenant the resolver answers nil for is refused as unknown. Nil
	// keeps the single Store for every caller — the pre-tenant behavior, where
	// a tenant name changes nothing.
	//
	// The resolver must be safe for concurrent use and should return stable
	// stores per tenant for the Server's lifetime; a store returned once must
	// keep answering for the tenant.
	//
	// AGO-DEVIATION: the a2a-sdk threads a ServerCallContext into every store
	// method and the store resolves the owner itself; AGo keeps TaskStore free
	// of context plumbing and resolves the tenant here. A side effect is that
	// a tenant name is inert while no resolver is configured (Python always
	// scopes by owner): the zero value keeps the pre-tenant single store.
	//
	// WARNING — this hook does not authorize anything. The tenant it receives
	// is the one the *client named* (JSON-RPC "tenant" param, REST path
	// segment, or Tenant), so on its own any caller can reach any tenant's
	// store simply by asking for it. Configure TenantResolver to check the
	// tenant against the authenticated caller and refuse a foreign one.
	StoreForTenant func(tenant string) TaskStore
	// TenantResolver authorizes and canonicalizes the tenant of every request
	// the server resolves a tenant for: the REST message/task routes and every
	// JSON-RPC method. It receives the request, the tenant the client named and
	// the identity AuthorizeIdentity produced, so a deployment can refuse "this
	// tenant is not yours" (an error answers -32602 on JSON-RPC and 403 on
	// REST) or ignore the client's claim and derive the tenant from the
	// identity. It runs even when the request names no tenant. The card routes
	// (discovery and the extended card) are public and are not tenant-resolved.
	//
	// WARNING — a server configured with neither TenantResolver nor
	// StoreForTenant is single-tenant: a tenant name on the wire is inert and
	// every caller shares one store. A server with only StoreForTenant trusts
	// the client's tenant; that is the pre-resolver behavior, kept for
	// compatibility, not an isolation guarantee.
	TenantResolver TenantResolver
	// Authorize gates every request before dispatch — card, REST and JSON-RPC
	// surfaces alike: returning a non-nil error rejects the request with 403
	// and a fixed message, keeping the error text off the wire. Nil disables
	// the check. The hook sees the raw request (body unconsumed), must be
	// safe for concurrent use, and is the surface's only built-in auth point —
	// any non-loopback exposure needs one.
	Authorize func(r *http.Request) error
	// AuthorizeIdentity is Authorize with a result: the identity it returns is
	// put on the request context (IdentityFrom) and handed to TenantResolver,
	// so a tenant check can compare "who called" with "which tenant it asks
	// for". Returning an error rejects the request with 403 and a fixed
	// message, exactly like Authorize. Both hooks run when both are set —
	// Authorize first, then AuthorizeIdentity — so adding an identity check
	// never removes the older gate.
	AuthorizeIdentity func(r *http.Request) (identity any, err error)
	// AllowedHosts, when non-empty, admits only requests whose Host header
	// matches one entry exactly (host or host:port, case-insensitive);
	// anything else answers 403. This closes DNS-rebinding against loopback
	// deployments, where a rebinding page reaches the server with its own
	// Host. Nil disables the check.
	AllowedHosts []string
	// CardModifier rewrites the card served on the discovery routes per
	// request (parity: server.py card_modifier — tenant- or caller-specific
	// cards). It receives a deep copy, so mutate-and-return never leaks into
	// the long-lived card; returning a nil card serves the (mutated) copy. An
	// error answers 500 with a fixed message. The modified card is served
	// verbatim: discovery defaults are not re-applied. Nil serves the static
	// card. The hook must be safe for concurrent use.
	//
	// AGO-DEVIATION: Python's card_modifier takes the card alone; the extra
	// *http.Request lets a hook vary the card by caller (tenant header,
	// credentials) without a request-scoped context channel, which the
	// a2a-sdk provides through ServerCallContext on the extended-card hook.
	CardModifier func(r *http.Request, card *AgentCard) (*AgentCard, error)
	// CardCacheControl, when non-empty, is sent as the Cache-Control header
	// of the discovery routes (parity: build_jsonrpc card_cache_control), for
	// example "public, max-age=300". Empty sends no header.
	CardCacheControl string
	// ExtendedCard is the authenticated card: the one served by the JSON-RPC
	// agent/getAuthenticatedExtendedCard method and the REST
	// /extendedAgentCard route, and reachable only through the request gates
	// (Authorize, AllowedHosts, AllowedOrigins) — the point of a second card
	// is to show a caller that authenticated more than the public one does
	// (parity: server.py extended_card, which also flips
	// capabilities.extendedAgentCard on the public card). Nil makes both
	// surfaces answer -32004.
	ExtendedCard *AgentCard
	// ExtendedCardModifier rewrites the extended card per request (parity:
	// transports/_common.py wrap_extended_card_modifier — the hook receives a
	// copy, so mutate-and-return never leaks into the long-lived card). Nil
	// serves the card verbatim. The hook must be safe for concurrent use.
	ExtendedCardModifier func(r *http.Request, card *AgentCard) (*AgentCard, error)
	// ValidateInputModes makes the message surfaces reject a part whose media
	// type is not declared by the card (defaultInputModes or a skill's
	// inputModes), which is the SDK-side check the Python server turns on with
	// validate_input_modes (parity: server.py _check_input_modes /
	// the a2a-sdk's input-mode validation). An empty declaration accepts
	// everything, as in the SDK.
	ValidateInputModes bool
	// AllowedOrigins, when non-empty, requires every request carrying an
	// Origin header (browsers send it cross-origin) to match one entry
	// exactly (scheme://host[:port], case-insensitive). Requests without
	// Origin — ordinary server-to-server clients — pass, so this is a
	// browser-request fence, not authentication. Nil disables the check.
	AllowedOrigins []string
}

// Server exposes one agent over A2A: agent-card discovery, the JSON-RPC
// methods message/send, message/stream, tasks/get, tasks/cancel and the
// push-notification config surface, plus the REST twin of the core methods.
// The zero flow: Handler() is an http.Handler; mount it on any HTTP server.
// Safe for concurrent use: task state transitions are atomic (see updateTask).
type Server struct {
	cfg   ServerConfig
	card  AgentCard // the config card with discovery defaults applied
	store TaskStore
	// hub fans task progress out to live message/stream and tasks/resubscribe
	// streams (streamhub.go). It is also the write path of a task that has
	// subscribers, so a transition is persisted and broadcast atomically.
	hub *taskHub
	// taskLocks serializes one read-modify-write cycle per task id for stores
	// without TaskUpdater, which leave the cycle split across a Get and a
	// Save. Entries live for the Server's lifetime — bounded by the number of
	// distinct task ids, and unused at all with the default MemoryTaskStore.
	locksMu   sync.Mutex
	taskLocks map[string]*sync.Mutex
	// staleSignature is the configuration error of a server that must change
	// a card it has no key to re-sign: the card routes answer 500 rather than
	// serving a signature over a payload it does not match. NewServerChecked
	// returns it at construction instead.
	staleSignature error
}

// NewServer builds a Server from cfg. The served card flips the
// pushNotifications capability from the configured PushStore (parity:
// server.py:218), so the config card does not have to keep the flag in sync.
func NewServer(cfg ServerConfig) *Server {
	s, err := NewServerChecked(cfg)
	if err != nil {
		// Serve the refusal on the card routes instead of dropping it: a
		// misconfigured deployment must not quietly publish a signature over
		// a payload the server changed.
		s = &Server{cfg: cfg, card: withDiscoveryDefaults(cfg.Card, cfg.Tenant), staleSignature: err}
		s.store = s.defaultStore()
		s.hub = &taskHub{}
	}
	return s
}

// NewServerChecked builds a Server from cfg and refuses a configuration that
// cannot be served honestly: a card that carries signatures while the server
// must flip a capability on it and has no CardSigner to re-sign the result
// (parity: errors.py A2AStaleCardSignatureError — flipping a flag voids the
// JWS). Everything NewServer accepts, this accepts; the difference is the
// error at construction instead of a 500 on the card routes.
func NewServerChecked(cfg ServerConfig) (*Server, error) {
	card := withDiscoveryDefaults(cfg.Card, cfg.Tenant)
	// The server derives two flags and must say so explicitly: push support is
	// always stated (parity: server.py:218 passes push_notifications=<bool> to
	// build_card, so AG2 signs an explicit false), the extended card only when
	// there is one (parity: clone_card_with_capabilities flips it on). Both are
	// proto3 optional fields, so presence is part of what gets signed.
	card.Capabilities.SetPushNotifications(cfg.PushStore != nil)
	if cfg.ExtendedCard != nil {
		card.Capabilities.SetExtendedAgentCard(true)
	}
	// AGO-DEVIATION: Python raises A2AStaleCardSignatureError from the
	// transport builder (build_jsonrpc/build_rest); AGo builds the server from
	// one config, so NewServerChecked refuses it up front and NewServer answers
	// 500 on the card routes — a published signature is never silently wrong.
	//
	// The comparison is over the signed payload, not just the capability flags:
	// discovery defaults are applied here too, so a card that arrives without
	// interfaces or without a stated push capability is exactly as stale as one
	// whose flags the server flipped.
	if len(cfg.Card.Signatures) > 0 && cfg.CardSigner == nil {
		signed, err := cardSigningPayload(&cfg.Card)
		if err != nil {
			return nil, err
		}
		served, err := cardSigningPayload(&card)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(signed, served) {
			return nil, fmt.Errorf("%w: the card carries signatures and the server must change it", ErrStaleCardSignature)
		}
	}
	if cfg.CardSigner != nil {
		if err := resignCard(&card, cfg.CardSigner); err != nil {
			return nil, err
		}
	}
	s := &Server{cfg: cfg, card: card, hub: &taskHub{}}
	s.store = s.defaultStore()
	return s, nil
}

// defaultStore is the store a request with no tenant falls back to.
func (s *Server) defaultStore() TaskStore {
	if s.cfg.Store != nil {
		return s.cfg.Store
	}
	return NewMemoryTaskStore()
}

// resignCard replaces the signatures on card with fresh ones from signer
// (parity: _common.py sign_card on a signature-stripped copy).
func resignCard(card *AgentCard, signer CardSigner) error {
	card.Signatures = nil
	return signer(card)
}

// withDiscoveryDefaults returns card with the fields a 1.x client requires to
// resolve a transport filled in: the protocol version and — when the card
// declares no interfaces — one JSONRPC interface over the card URL (parity:
// ag2/ag2/a2a/card.py _build_interfaces, which always emits supported_interfaces
// stamped with PROTOCOL_VERSION_CURRENT; an SDK 1.x or AG2 client fails card
// resolution on an empty supportedInterfaces, _http.py select_interface).
// Interfaces the config declares are served verbatim.
func withDiscoveryDefaults(card AgentCard, tenant string) AgentCard {
	version := card.ProtocolVersion
	if version == "" {
		version = ProtocolVersionCurrent
	}
	if len(card.SupportedInterfaces) == 0 {
		card.SupportedInterfaces = []AgentInterface{{
			URL:             card.URL,
			ProtocolBinding: ProtocolBindingJSONRPC,
			ProtocolVersion: version,
			Tenant:          tenant,
		}}
	}
	card.ProtocolVersion = version
	return card
}

// Handler returns the http.Handler serving the card discovery routes, the
// JSON-RPC endpoint and the REST transport.
func (s *Server) Handler() http.Handler { return s }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r, ok := s.admit(w, r)
	if !ok {
		return
	}
	if s.tenantRouting() && tenantSurface(r.URL.Path) {
		routed, err := s.withPathTenant(r)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r = routed
	}
	if s.serveREST(w, r) {
		return
	}
	switch {
	case r.URL.Path == ExtendedCardPath:
		// A card route, not a REST task route: it answers 405 to a non-GET
		// like the discovery paths do.
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveExtendedCard(w, r)
	case r.URL.Path == DefaultCardPath || r.URL.Path == LegacyCardPath:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveCard(w, r)
	case r.URL.Path == rpcPath:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveRPC(w, r)
	default:
		http.NotFound(w, r)
	}
}

// withPathTenant resolves the request's tenant — from the path segment of
// "/{tenant}/tasks" when there is one (parity: the SDK mounting the REST
// routes under Mount(path="/{tenant}")), and always through the configured
// TenantResolver — and strips the segment so every route below matches as if
// the tenant were not there. An error is the resolver refusing the request
// (the caller answers 403); the resolver also decides for a request that names
// no tenant, so a deployment can force tenancy from the identity alone.
func (s *Server) withPathTenant(r *http.Request) (*http.Request, error) {
	requested, rest := splitTenantPath(r.URL.Path)
	ctx, err := s.resolveTenant(r.Context(), r, requested)
	if err != nil {
		return nil, err
	}
	if rest == r.URL.Path {
		return r.WithContext(ctx), nil
	}
	clone := new(http.Request)
	*clone = *r
	u := *r.URL
	u.Path = rest
	clone.URL = &u
	return clone.WithContext(ctx), nil
}

// serveCard answers a discovery request: the static card, or the
// CardModifier's per-request rewrite of a deep copy of it.
func (s *Server) serveCard(w http.ResponseWriter, r *http.Request) {
	if s.staleSignature != nil {
		http.Error(w, msgStaleSignature, http.StatusInternalServerError)
		return
	}
	card := &s.card
	if s.cfg.CardModifier != nil {
		clone, err := cloneCard(&s.card)
		if err != nil {
			http.Error(w, msgInternalError, http.StatusInternalServerError)
			return
		}
		out, err := s.cfg.CardModifier(r, clone)
		if err != nil {
			http.Error(w, msgInternalError, http.StatusInternalServerError)
			return
		}
		card = clone
		if out != nil {
			card = out
		}
		if s.cfg.CardSigner != nil {
			// The modifier rewrote the payload the static signature covered:
			// re-sign what is actually served (parity: wrap_card_modifier).
			if err := resignCard(card, s.cfg.CardSigner); err != nil {
				http.Error(w, msgInternalError, http.StatusInternalServerError)
				return
			}
		}
	}
	if s.cfg.CardCacheControl != "" {
		w.Header().Set("Cache-Control", s.cfg.CardCacheControl)
	}
	writeJSON(w, http.StatusOK, card)
}

// cloneCard deep-copies a card through its wire form, so a modifier can
// mutate slices and maps freely.
func cloneCard(card *AgentCard) (*AgentCard, error) {
	data, err := json.Marshal(card)
	if err != nil {
		return nil, err
	}
	var out AgentCard
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ErrNoExtendedCard reports a server with no authenticated card configured.
var ErrNoExtendedCard = errors.New("a2a: no authenticated extended agent card configured")

// resolveExtendedCard builds the card both extended-card surfaces serve: the
// configured one, discovery defaults applied, through the per-request
// modifier. The request gates in admit have already run, so a caller that gets
// this far passed whatever authentication the deployment configured.
func (s *Server) resolveExtendedCard(r *http.Request) (*AgentCard, error) {
	if s.cfg.ExtendedCard == nil {
		return nil, ErrNoExtendedCard
	}
	card, err := cloneCard(s.cfg.ExtendedCard)
	if err != nil {
		return nil, err
	}
	fresh := withDiscoveryDefaults(*card, s.cfg.Tenant)
	card = &fresh
	if s.cfg.ExtendedCardModifier != nil {
		out, err := s.cfg.ExtendedCardModifier(r, card)
		if err != nil {
			return nil, err
		}
		if out != nil {
			card = out
		}
	}
	if s.cfg.CardSigner != nil {
		// The extended card is signed for every served copy, including a
		// modifier's rewrite, so the client can verify the second card too
		// (parity: wrap_extended_card_modifier).
		if err := resignCard(card, s.cfg.CardSigner); err != nil {
			return nil, err
		}
	}
	return card, nil
}

// serveExtendedCard answers REST GET /extendedAgentCard.
func (s *Server) serveExtendedCard(w http.ResponseWriter, r *http.Request) {
	if s.staleSignature != nil {
		http.Error(w, msgStaleSignature, http.StatusInternalServerError)
		return
	}
	card, err := s.resolveExtendedCard(r)
	switch {
	case errors.Is(err, ErrNoExtendedCard):
		http.Error(w, msgNoExtendedCard, http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, msgInternalError, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, card)
}

// msgNoExtendedCard is the fixed text for a server with no extended card
// configured: which card to serve is deployment detail, not client business.
const msgNoExtendedCard = "no authenticated extended agent card is configured"

// msgTenantForbidden is the fixed answer when a TenantResolver refuses the
// request's tenant: the reason is the deployment's, not the caller's business.
const msgTenantForbidden = "tenant not allowed"

// msgStaleSignature is the fixed text of the card routes of a server whose
// configured card carries signatures the server cannot re-create.
const msgStaleSignature = "agent card signatures do not cover the card this server must serve"

// serveExtendedRPC answers agent/getAuthenticatedExtendedCard with the same
// card the REST route serves.
func (s *Server) serveExtendedRPC(w http.ResponseWriter, r *http.Request, req *rpcRequest) {
	if s.staleSignature != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInternalError, Message: msgStaleSignature})
		return
	}
	card, err := s.resolveExtendedCard(r)
	switch {
	case errors.Is(err, ErrNoExtendedCard):
		writeRPCError(w, req.ID, &RPCError{Code: codeUnsupportedOperation, Message: msgNoExtendedCard})
		return
	case err != nil:
		writeRPCError(w, req.ID, &RPCError{Code: codeInternalError, Message: msgInternalError})
		return
	}
	writeRPCResult(w, req.ID, card)
}

// admit applies the request gates — the Authorize hook, then the Host and
// Origin allowlists — before any dispatch, reporting whether the request may
// proceed. Every gate answers 403 with a fixed message: rejection reasons can
// carry internal detail (hook errors, configured hosts) that has no business
// on the wire. The gates are fail-closed: a set allowlist admits nothing
// until a header matches.
func (s *Server) admit(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	// Both gates run, in this order: Authorize is the older request-level check
	// (an IP fence, a shared token) and must not be bypassed by adding the
	// identity form. A refusal from either answers 403.
	if s.cfg.Authorize != nil {
		if err := s.cfg.Authorize(r); err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return nil, false
		}
	}
	if s.cfg.AuthorizeIdentity != nil {
		identity, err := s.cfg.AuthorizeIdentity(r)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return nil, false
		}
		// The identity rides the request context: TenantResolver and the
		// handlers read it with IdentityFrom.
		r = r.WithContext(withIdentity(r.Context(), identity))
	}
	if len(s.cfg.AllowedHosts) > 0 && !matchFold(r.Host, s.cfg.AllowedHosts) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	if origin := r.Header.Get("Origin"); len(s.cfg.AllowedOrigins) > 0 && origin != "" && !matchFold(origin, s.cfg.AllowedOrigins) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	return r, true
}

// matchFold reports whether s equals one of the allowed entries
// case-insensitively. Host and Origin headers are matched whole (host or
// host:port; scheme://host[:port]) — no prefix or suffix tricks, so a
// "good.example" entry never admits "good.example.evil.io".
func matchFold(s string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(s, a) {
			return true
		}
	}
	return false
}

// writeJSON writes one JSON body with the given status. Discovery and
// JSON-RPC replies use application/json; the REST transport overrides the
// header to the binding media type (rest.go).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// serveRPC reads one JSON-RPC request and dispatches it. Errors before and
// during dispatch travel back as JSON-RPC error objects (HTTP 200), matching
// the JSON-RPC-over-HTTP convention of the Python transport.
func (s *Server) serveRPC(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(nil), Error: &RPCError{
			Code: codeInternalError, Message: msgInternalError,
		}})
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		// A batch is well-formed JSON, so it is an invalid request, not a
		// parse error. JSON-RPC 2.0 batches have no A2A use; refuse them.
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(nil), Error: &RPCError{
			Code: codeInvalidRequest, Message: "batch requests are not supported",
		}})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(nil), Error: &RPCError{
			Code: codeParseError, Message: "parse error: " + err.Error(),
		}})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(req.ID), Error: &RPCError{
			Code: codeInvalidRequest, Message: "not a JSON-RPC 2.0 request",
		}})
		return
	}

	ctx := r.Context()
	if s.tenantRouting() {
		resolved, err := s.resolveTenant(ctx, r, tenantFromParams(req.Params))
		if err != nil {
			// The resolver refused this tenant for this caller. The text stays
			// off the wire; -32602 is the A2A client's "bad request" shape.
			writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: msgTenantForbidden})
			return
		}
		ctx = resolved
	}
	switch req.Method {
	case MethodMessageSend:
		s.messageSend(ctx, w, &req)
	case MethodMessageStream:
		s.messageStream(ctx, w, &req)
	case MethodTasksGet:
		s.tasksGet(ctx, w, &req)
	case MethodTasksList:
		s.tasksList(ctx, w, &req)
	case MethodTasksCancel:
		s.tasksCancel(ctx, w, &req)
	case MethodTasksResubscribe:
		s.tasksResubscribe(ctx, w, &req)
	case MethodExtendedCard:
		s.serveExtendedRPC(w, r, &req)
	case MethodPushSet:
		s.servePushSet(ctx, w, &req)
	case MethodPushGet:
		s.servePushGet(ctx, w, &req)
	case MethodPushList:
		s.servePushList(ctx, w, &req)
	case MethodPushDelete:
		s.servePushDelete(ctx, w, &req)
	default:
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(req.ID), Error: &RPCError{
			Code: codeMethodNotFound, Message: "method not supported: " + req.Method,
		}})
	}
}

// writeRPC sends one JSON-RPC reply over HTTP 200.
func writeRPC(w http.ResponseWriter, resp *rpcResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// writeRPCResult replies with a result.
func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	data, err := json.Marshal(result)
	if err != nil {
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(id), Error: &RPCError{
			Code: codeInternalError, Message: "encode result: " + err.Error(),
		}})
		return
	}
	writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(id), Result: data})
}

// writeRPCError replies with an error object.
func writeRPCError(w http.ResponseWriter, id json.RawMessage, rpcErr *RPCError) {
	writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(id), Error: rpcErr})
}

// newTask derives a fresh Task from an incoming message. The task id is
// always server-generated: accepting a client-supplied id for a task that
// does not exist yet would let a peer pre-select the record it later
// continues, reads and hijacks (guessable or leaked ids become keys to other
// clients' exchanges), and pins one store record per attacker-chosen id.
//
// AGO-DEVIATION: the Python executor honors a client-supplied task id for
// new tasks (executor.py:89-90); AGo always mints its own UUID so id
// ownership stays server-side. The client-supplied contextId still threads
// the conversation (parity kept there).
func (s *Server) newTask(msg *Message) *Task {
	contextID := msg.ContextID
	if contextID == "" {
		contextID = uuid.New()
	}
	return &Task{ID: uuid.New(), ContextID: contextID, Status: TaskStatus{State: TaskStateWorking}}
}

// loadOrCreateTask resolves the task a turn belongs to: a message carrying a
// task id continues the stored record (the client-tools and HITL
// continuations do exactly that), an unknown or absent id opens a fresh task
// with a server-minted id — a task id the store does not know names no
// record, so it cannot be resurrected or pre-created from the client side.
func (s *Server) loadOrCreateTask(ctx context.Context, msg *Message) (*Task, error) {
	if msg.TaskID != "" {
		store, err := s.storeFor(ctx)
		if err != nil {
			return nil, err
		}
		if task, err := store.Get(ctx, msg.TaskID); err == nil {
			return task, nil
		}
	}
	return s.newTask(msg), nil
}

// normalizeReply stamps the reply with role and the task identity so the
// client can correlate it. Reply-side metadata survives so a context_update
// delta reaches the client.
func normalizeReply(m *Message, task *Task) *Message {
	if m == nil {
		m = &Message{}
	}
	if m.Role == "" {
		m.Role = RoleAgent
	}
	if m.MessageID == "" {
		m.MessageID = uuid.New()
	}
	if m.ContextID == "" {
		m.ContextID = task.ContextID
	}
	if m.TaskID == "" {
		m.TaskID = task.ID
	}
	return m
}

// errorMessage wraps err into the agent message carried on a failed status.
func errorMessage(err error) *Message {
	return &Message{Role: RoleAgent, MessageID: uuid.New(), Parts: []Part{NewTextPart(err.Error())}}
}

// authRequiredMessage is the parked message of an auth-required task: the
// handler's error text says what credentials the caller has to obtain (the A2A
// spec's TaskStatus.message half of the auth-required transition).
func authRequiredMessage(err error) *Message {
	return &Message{Role: RoleAgent, MessageID: uuid.New(), Parts: []Part{NewTextPart(err.Error())}}
}

// sendOutcome is what one message/send-style turn produced.
type sendOutcome struct {
	task      *Task     // the stored record, terminal or parked
	reply     *Message  // the reply message (completed) or the parked message (input-required)
	state     TaskState // terminal state, or input-required
	handleErr error     // the handler failure, if the turn failed
	// refused reports that the stored task was already canceled when the
	// turn tried to commit (the cancel won the race): the turn's outcome was
	// dropped, the stored record stayed untouched and task/reply/state carry
	// that stored record, not the turn's result.
	refused bool
}

// executeSend runs one turn end to end: resolve the task, dispatch (plain
// handle, client-tools extension leg, or tool-results continuation), apply
// the context-variable sync, commit the outcome and fire the push webhooks.
// The *RPCError return covers task-store failures only; handler failures come
// back inside the outcome as a failed task.
func (s *Server) executeSend(ctx context.Context, msg *Message) (*sendOutcome, *RPCError) {
	task, rpcErr := s.beginTurn(ctx, msg)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return s.finishTurn(ctx, task, msg), nil
}

// beginTurn resolves the task a turn continues and re-hydrates the AG2
// conversation history the message ships (RestoreHistory).
func (s *Server) beginTurn(ctx context.Context, msg *Message) (*Task, *RPCError) {
	task, err := s.loadOrCreateTask(ctx, msg)
	if err != nil {
		return nil, storeError(err)
	}
	if s.cfg.RestoreHistory != nil {
		history, err := ParseHistory(msg)
		if err != nil {
			return nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		if len(history) > 0 {
			if err := s.cfg.RestoreHistory(ctx, task.ID, history); err != nil {
				return nil, &RPCError{Code: codeInternalError, Message: msgInternalError}
			}
		}
	}
	return task, nil
}

// finishTurn runs and commits one turn against task. The commit is atomic
// against other turns on the same task and against tasks/cancel: when the
// stored record turned terminal in the meantime the turn's outcome is
// refused and the outcome reports the stored record (a canceled task never
// resurrects as completed).
func (s *Server) finishTurn(ctx context.Context, task *Task, msg *Message) *sendOutcome {
	return s.finishTurnOn(ctx, task, msg, nil)
}

// finishTurnOn is finishTurn for a streaming turn: turn carries the stream the
// terminal update is published on and delivered to (nil for a plain turn).
func (s *Server) finishTurnOn(ctx context.Context, task *Task, msg *Message, turn *streamTurn) *sendOutcome {
	reply, state, handleErr := s.runTurn(ctx, task, msg)
	outcome, _ := s.commitTurnOutcome(ctx, task, msg, turn, reply, state, handleErr)
	return outcome
}

// commitTurnOutcome commits one turn's outcome through the task hub and returns
// the outcome together with the terminal stream event that describes it. The
// commit is atomic against other turns on the same task and against
// tasks/cancel: when the stored record turned terminal in the meantime the
// turn's outcome is refused and the outcome reports the stored record (a
// canceled task never resurrects as completed). The hub publishes the terminal
// update to a streaming turn's subscribers (or the task snapshot for a plain
// turn) and closes the stream.
func (s *Server) commitTurnOutcome(ctx context.Context, task *Task, msg *Message, turn *streamTurn, reply *Message, state TaskState, handleErr error) (*sendOutcome, *StatusUpdate) {
	var outcome *sendOutcome
	write := func() (*Task, bool, error) {
		stored, refused, err := s.commitTask(ctx, task, msg, reply, state, handleErr)
		switch {
		case err != nil:
			outcome = &sendOutcome{
				task: task, state: TaskStateFailed,
				handleErr: fmt.Errorf("save task: %w", err),
			}
			return nil, false, err
		case refused:
			outcome = &sendOutcome{
				task: stored, reply: stored.Status.Message,
				state: stored.Status.State, refused: true,
			}
		default:
			outcome = &sendOutcome{task: stored, reply: reply, state: state, handleErr: handleErr}
		}
		return stored, refused, nil
	}
	var event func(*Task) any
	if turn != nil {
		event = func(*Task) any { return terminalStatus(outcome) }
	}
	if _, _, err := s.hub.commit(ctx, task.ID, write, event); err != nil {
		update := terminalStatus(outcome)
		if turn != nil {
			turn.deliver(update)
		}
		return outcome, update
	}
	s.dispatchPush(ctx, outcome.task.ID, outcome.task)
	update := terminalStatus(outcome)
	if turn != nil {
		turn.deliver(update)
	}
	return outcome, update
}

// terminalStatus builds the final stream event of a turn: the stored record's
// status, so a streamed turn ends with exactly what tasks/get reports. The one
// exception is a commit that failed to reach the store — there is no record to
// echo then, and the stream reports the failure.
func terminalStatus(outcome *sendOutcome) *StatusUpdate {
	update := &StatusUpdate{
		Kind:      KindStatusUpdate,
		TaskID:    outcome.task.ID,
		ContextID: outcome.task.ContextID,
		Final:     true,
	}
	if outcome.handleErr != nil && outcome.task.Status.State != TaskStateFailed {
		update.Status = TaskStatus{State: TaskStateFailed, Message: errorMessage(outcome.handleErr)}
		return update
	}
	update.Status = outcome.task.Status
	return update
}

// runTurn dispatches one turn and applies the context-variable sync around
// it: inbound deltas merge before the turn (via the configured callback), the
// returned final set rides the reply (parity: executor.py's
// _dispatch_to_agent merged_variables / _build_final_message bracket).
func (s *Server) runTurn(ctx context.Context, task *Task, msg *Message) (*Message, TaskState, error) {
	return s.runWithContextSync(ctx, task, msg, func() (*Message, TaskState, error) {
		reply, state, err := s.dispatchTurn(ctx, msg)
		return s.turnOutcome(reply, state, err)
	})
}

// runStreamTurn dispatches one streaming turn through HandleStream, under the
// same context-variable bracket and the same outcome mapping as runTurn.
func (s *Server) runStreamTurn(ctx context.Context, task *Task, msg *Message, emit StreamEmitter) (*Message, TaskState, error) {
	return s.runWithContextSync(ctx, task, msg, func() (*Message, TaskState, error) {
		reply, err := s.cfg.HandleStream(ctx, msg, emit)
		return s.turnOutcome(reply, TaskStateCompleted, err)
	})
}

// runWithContextSync brackets one dispatch with the configured context-variable
// sync.
func (s *Server) runWithContextSync(ctx context.Context, task *Task, msg *Message, dispatch func() (*Message, TaskState, error)) (*Message, TaskState, error) {
	var outgoing map[string]any
	if s.cfg.SyncVariables != nil {
		if incoming := ExtractContextUpdate(msg); len(incoming) > 0 {
			var err error
			outgoing, err = s.cfg.SyncVariables(ctx, task.ID, StripReservedVariables(incoming))
			if err != nil {
				return nil, TaskStateFailed, fmt.Errorf("context sync: %w", err)
			}
		}
	}
	reply, state, err := dispatch()
	if err != nil {
		return nil, TaskStateFailed, err
	}
	if len(outgoing) > 0 {
		if reply == nil {
			reply = &Message{}
		}
		WithContextUpdate(reply, StripReservedVariables(outgoing))
	}
	return reply, state, nil
}

// turnOutcome maps a handler's (reply, state, error) into the turn's state: a
// handler that refuses the task or asks for credentials names the state itself
// (parity: a2a TASK_STATE_REJECTED / TASK_STATE_AUTH_REQUIRED); anything else
// is a failure.
func (s *Server) turnOutcome(reply *Message, state TaskState, err error) (*Message, TaskState, error) {
	switch {
	case err == nil:
		return reply, state, nil
	case errors.Is(err, ErrTaskAuthRequired):
		return authRequiredMessage(err), TaskStateAuthRequired, nil
	case errors.Is(err, ErrTaskRejected):
		return errorMessage(err), TaskStateRejected, nil
	}
	return nil, TaskStateFailed, err
}

// dispatchTurn picks the turn's leg by the message's extension parts
// (parity: executor.py _build_initial_event — tool results make a
// continuation, tool schemas arm client execution, anything else is a plain
// request).
func (s *Server) dispatchTurn(ctx context.Context, msg *Message) (*Message, TaskState, error) {
	results, err := ParseToolResults(msg)
	if err != nil {
		return nil, TaskStateFailed, err
	}
	if len(results) > 0 {
		if s.cfg.HandleToolResults == nil {
			return nil, TaskStateFailed, fmt.Errorf("%w: tool results arrived but no HandleToolResults is configured", ErrNoHandler)
		}
		reply, err := s.cfg.HandleToolResults(ctx, msg, results)
		return reply, TaskStateCompleted, err
	}
	schemas, err := ParseToolSchemas(msg)
	if err != nil {
		return nil, TaskStateFailed, err
	}
	if len(schemas) > 0 && s.cfg.HandleClientTools != nil {
		reply, calls, err := s.cfg.HandleClientTools(ctx, msg, schemas)
		if err != nil {
			return nil, TaskStateFailed, err
		}
		if len(calls) == 0 {
			return reply, TaskStateCompleted, nil
		}
		if reply == nil {
			reply = &Message{Role: RoleAgent}
		}
		for _, c := range calls {
			reply.Parts = append(reply.Parts, ToolCallsPart(c))
		}
		return reply, TaskStateInputRequired, nil
	}
	reply, err := s.runHandle(ctx, msg)
	return reply, TaskStateCompleted, err
}

// updateTask applies mutate to the stored record for id as one atomic
// read-modify-write: the guard inside mutate and the write cannot interleave
// with another transition on the same task. Stores implementing TaskUpdater
// run the cycle under their own lock; for the rest the server holds a
// per-task mutex across the Get→Save pair. The mutate decides the policy and
// returns whether to apply; returning false keeps the stored record
// untouched and refused reports that. ErrTaskNotFound passes through for
// absent ids.
func (s *Server) updateTask(ctx context.Context, id string, mutate func(current *Task) (apply bool)) (stored *Task, refused bool, err error) {
	store, storeErr := s.storeFor(ctx)
	if storeErr != nil {
		return nil, false, storeErr
	}
	if up, ok := store.(TaskUpdater); ok {
		stored, err := up.Update(ctx, id, func(current *Task) (*Task, error) {
			if !mutate(current) {
				refused = true
			}
			return current, nil
		})
		return stored, refused, err
	}
	unlock := s.lockTask(id)
	defer unlock()
	current, err := store.Get(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if !mutate(current) {
		return current, true, nil
	}
	if err := store.Save(ctx, current); err != nil {
		return nil, false, fmt.Errorf("save task: %w", err)
	}
	return current, false, nil
}

// lockTask returns the unlock func of the per-task mutex for id, creating it
// on first use.
func (s *Server) lockTask(id string) func() {
	s.locksMu.Lock()
	if s.taskLocks == nil {
		s.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := s.taskLocks[id]
	if mu == nil {
		mu = &sync.Mutex{}
		s.taskLocks[id] = mu
	}
	s.locksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// commitTask records the turn's outcome atomically: completed with the reply
// on status.message, input-required parked with the calls/question, or failed
// with the error text. History merges onto the record as the store holds it
// at commit time — a concurrent continuation's entry stays, and the turn
// adds exactly its incoming message. A stored canceled record refuses the
// commit (the cancel won the race; the turn's outcome is dropped and the
// task stays canceled — it never resurrects as completed). A completed or
// failed record continues: an explicit new message on the same task id is a
// client-driven continuation (parity: the executor keeps appending history
// for whatever task id arrives). The first commit of a task not yet in the
// store runs through the same atomic create-or-update cycle (see
// updateOrCreateTask): there is no stored state to merge with and nothing to
// resurrect. Returns the record now in the store and whether the commit
// was refused.
func (s *Server) commitTask(ctx context.Context, task *Task, msg *Message, reply *Message, state TaskState, handleErr error) (stored *Task, refused bool, err error) {
	apply := func(current *Task) bool {
		if current.Status.State == TaskStateCanceled {
			return false
		}
		current.History = append(current.History, *msg)
		switch {
		case handleErr != nil:
			current.Status = TaskStatus{State: TaskStateFailed, Message: errorMessage(handleErr)}
		case state == TaskStateInputRequired || state == TaskStateAuthRequired:
			current.Status = TaskStatus{State: state, Message: normalizeReply(reply, current)}
		case state == TaskStateRejected:
			current.Status = TaskStatus{State: TaskStateRejected, Message: normalizeReply(reply, current)}
		default:
			current.Status = TaskStatus{State: TaskStateCompleted, Message: normalizeReply(reply, current)}
		}
		return true
	}
	return s.updateOrCreateTask(ctx, task, apply)
}

// updateOrCreateTask commits one turn against the record for task.ID with
// the same atomicity as updateTask — including the first commit of a record
// the store does not hold yet: the create and any parallel writer on the
// same id (a sibling turn, a store eviction landing mid-turn) resolve under
// one lock instead of last-write-wins. Stores implementing TaskUpserter run
// the whole create-or-update under their own lock. TaskUpdater stores keep
// every write to a live record on their atomic Update path — the per-task
// mutex only fences the create, and a record another writer created in the
// meantime is updated atomically, never replaced by a plain Save (which
// shares no lock with the store's Update and would clobber it). Plain stores
// run the whole cycle under the per-task mutex, the same one updateTask's
// fallback uses. apply receives the fresh task from newTask when the record
// is absent — its working state lets the commit proceed.
func (s *Server) updateOrCreateTask(ctx context.Context, task *Task, apply func(current *Task) bool) (*Task, bool, error) {
	store, storeErr := s.storeFor(ctx)
	if storeErr != nil {
		return nil, false, storeErr
	}
	if up, ok := store.(TaskUpserter); ok {
		var refused bool
		stored, err := up.Upsert(ctx, task.ID, func(current *Task) (*Task, error) {
			if current == nil {
				current = task
			}
			if !apply(current) {
				refused = true
			}
			return current, nil
		})
		if err != nil {
			return nil, refused, err
		}
		return stored, refused, nil
	}
	if _, ok := store.(TaskUpdater); ok {
		for {
			stored, refused, err := s.updateTask(ctx, task.ID, apply)
			if !errors.Is(err, ErrTaskNotFound) {
				return stored, refused, err
			}
			unlock := s.lockTask(task.ID)
			_, getErr := store.Get(ctx, task.ID)
			switch {
			case getErr == nil:
				// A parallel turn created the record: release and update it
				// through the store's atomic path.
				unlock()
				continue
			case !errors.Is(getErr, ErrTaskNotFound):
				unlock()
				return nil, false, getErr
			}
			apply(task)
			if err := store.Save(ctx, task); err != nil {
				unlock()
				return nil, false, fmt.Errorf("save task: %w", err)
			}
			unlock()
			return task, false, nil
		}
	}
	unlock := s.lockTask(task.ID)
	defer unlock()
	current, err := store.Get(ctx, task.ID)
	switch {
	case err == nil:
		if !apply(current) {
			return current, true, nil
		}
		if err := store.Save(ctx, current); err != nil {
			return nil, false, fmt.Errorf("save task: %w", err)
		}
		return current, false, nil
	case errors.Is(err, ErrTaskNotFound):
		apply(task)
		if err := store.Save(ctx, task); err != nil {
			return nil, false, fmt.Errorf("save task: %w", err)
		}
		return task, false, nil
	default:
		return nil, false, err
	}
}

// runHandle invokes the configured handler, mapping a nil handler to a
// sentinel error.
func (s *Server) runHandle(ctx context.Context, msg *Message) (*Message, error) {
	if s.cfg.Handle == nil {
		return nil, fmt.Errorf("%w", ErrNoHandler)
	}
	return s.cfg.Handle(ctx, msg)
}

// messageSend serves the synchronous exchange: Handle (or the extension leg
// the message selects), persist the task, answer with the reply Message —
// or, when the turn parked input-required (client-tools round trip, HITL
// question), with the Task record whose status carries the parked message
// (parity: the a2a-sdk message/send returning a Task for deferred turns;
// AG2's client absorbs both shapes, client.py result probing).
//
// AGO-DEVIATION: the exchange is recorded completed/failed in the shared
// store before answering — the synchronous handler gives the store its
// terminal state in the same call, which is what makes tasks/get and
// tasks/cancel resolvable (D-050).
func (s *Server) messageSend(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	msg, rpcErr := decodeMessageParams(req)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	if rpcErr := s.checkInputModes(msg); rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	outcome, rpcErr := s.executeSend(ctx, msg)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	if outcome.handleErr != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInternalError, Message: msgInternalError})
		return
	}
	// A refused commit answers with the stored Task record — the turn lost
	// the race to a cancel or an earlier terminal transition, and the record
	// is the only honest reply (same shape as the deferred turns below).
	if outcome.state == TaskStateInputRequired || outcome.state == TaskStateAuthRequired ||
		outcome.state == TaskStateRejected || outcome.refused {
		writeRPCResult(w, req.ID, outcome.task)
		return
	}
	writeRPCResult(w, req.ID, normalizeReply(outcome.reply, outcome.task))
}

// tasksGet serves tasks/get. historyLength>0 truncates the echoed history to
// the most recent N messages (parity: tasks.py get_task history_length).
func (s *Server) tasksGet(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	var params struct {
		ID            string `json:"id"`
		HistoryLength int    `json:"historyLength"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params: " + err.Error()})
		return
	}
	if params.ID == "" {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params.id is required"})
		return
	}
	task, rpcErr := s.getTask(ctx, params.ID, params.HistoryLength)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	writeRPCResult(w, req.ID, task)
}

// getTask is the transport-independent tasks/get body.
func (s *Server) getTask(ctx context.Context, id string, historyLength int) (*Task, *RPCError) {
	store, err := s.storeFor(ctx)
	if err != nil {
		return nil, storeError(err)
	}
	task, err := store.Get(ctx, id)
	if err != nil {
		return nil, storeError(err)
	}
	if n := historyLength; n > 0 && len(task.History) > n {
		task.History = task.History[len(task.History)-n:]
	}
	return task, nil
}

// tasksCancel serves tasks/cancel: a submitted/working task flips to
// canceled; a terminal task answers -32002 (parity: a2a TaskNotCancelableError).
func (s *Server) tasksCancel(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params: " + err.Error()})
		return
	}
	if params.ID == "" {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params.id is required"})
		return
	}
	task, rpcErr := s.cancelTask(ctx, params.ID)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	writeRPCResult(w, req.ID, task)
}

// cancelTask is the transport-independent tasks/cancel body: the terminal
// check and the Canceled write are one atomic step, so a turn finishing
// concurrently either lands before the cancel (which then answers -32002) or
// the cancel lands first and the turn's commit is refused — a canceled task
// never resurrects (parity: a2a TaskNotCancelableError).
func (s *Server) cancelTask(ctx context.Context, id string) (*Task, *RPCError) {
	var stored *Task
	var refused bool
	_, _, err := s.hub.commit(ctx, id, func() (*Task, bool, error) {
		var err error
		stored, refused, err = s.updateTask(ctx, id, func(current *Task) bool {
			if terminalTaskState(current.Status.State) {
				return false
			}
			current.Status = TaskStatus{State: TaskStateCanceled}
			return true
		})
		return stored, refused, err
	}, nil)
	if err != nil {
		return nil, storeError(err)
	}
	if refused {
		return nil, &RPCError{
			Code:    codeTaskNotCancelable,
			Message: fmt.Sprintf("task %s is already in terminal state %q", stored.ID, stored.Status.State),
		}
	}
	return stored, nil
}

// storeError maps a TaskStore failure to its wire error. Store failure texts
// (driver errors, ids, paths) are internal detail and stay off the wire:
// not-found answers a fixed message, everything else the fixed internal
// error.
func storeError(err error) *RPCError {
	if errors.Is(err, ErrTaskNotFound) {
		return &RPCError{Code: codeTaskNotFound, Message: "task not found"}
	}
	if errors.Is(err, ErrUnknownTenant) {
		return &RPCError{Code: codeInvalidParams, Message: "unknown tenant"}
	}
	return &RPCError{Code: codeInternalError, Message: msgInternalError}
}

// messageStream serves message/stream as a Server-Sent Events body: each SSE
// data entry is a JSON-RPC response whose result is one stream item — a
// StatusUpdate, an ArtifactUpdate from HandleStream, or (on a resubscribe) a
// Task snapshot. A turn parked input-required streams a non-final
// input-required update before the final event (the client-tools and HITL
// continuations resume the task through message/send).
//
// AGO-DEVIATION: the Handle contract is one synchronous call, so a turn that
// wants to stream artifacts installs ServerConfig.HandleStream; without it the
// stream stays the two terminal-outcome events (D-050).
func (s *Server) messageStream(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	if !s.cfg.Card.Capabilities.Streaming {
		writeRPCError(w, req.ID, &RPCError{
			Code:    codeUnsupportedOperation,
			Message: "message/stream requires capabilities.streaming on the agent card",
		})
		return
	}
	msg, rpcErr := decodeMessageParams(req)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	if rpcErr := s.checkInputModes(msg); rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeRPCError(w, req.ID, &RPCError{
			Code:    codeInternalError,
			Message: "streaming unsupported by the HTTP connection",
		})
		return
	}
	s.serveMessageStream(ctx, w, flusher, msg, jsonrpcStreamFraming(w, req.ID))
}

// tasksResubscribe serves tasks/resubscribe: the live stream of an existing
// task. It opens with the task as the server currently holds it — so a client
// that lost the connection while the task moved on converges to the server's
// state — and then continues with every later event until the terminal state,
// which closes the stream. A terminal task answers its final state and ends; an
// unknown task answers -32001. The tenant of the request is resolved through
// the tenant's store, so a task of another tenant is simply unknown here.
//
// AGO-DEVIATION: the a2a-sdk refuses to subscribe to a terminal task with
// UnsupportedOperationError (spec §3.1.6: it cannot be subscribed to); this
// server answers the task's final state and closes instead, which is the shape
// wave-4 invariant 7 needs (the terminal state is always observed) and what
// this package's client falls back from when a remote server does refuse.
func (s *Server) tasksResubscribe(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	if !s.cfg.Card.Capabilities.Streaming {
		writeRPCError(w, req.ID, &RPCError{
			Code:    codeUnsupportedOperation,
			Message: "tasks/resubscribe requires capabilities.streaming on the agent card",
		})
		return
	}
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params: " + err.Error()})
		return
	}
	if params.ID == "" {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params.id is required"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeRPCError(w, req.ID, &RPCError{
			Code:    codeInternalError,
			Message: "streaming unsupported by the HTTP connection",
		})
		return
	}
	s.serveResubscribe(ctx, w, flusher, params.ID, jsonrpcStreamFraming(w, req.ID))
}

// decodeMessageParams unmarshals the {message} params shared by message/send
// and message/stream.
func decodeMessageParams(req *rpcRequest) (*Message, *RPCError) {
	if len(req.Params) == 0 {
		return nil, &RPCError{Code: codeInvalidParams, Message: "params are required: {\"message\": {...}}"}
	}
	var params struct {
		Message *Message `json:"message"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &RPCError{Code: codeInvalidParams, Message: "params: " + err.Error()}
	}
	if params.Message == nil {
		return nil, &RPCError{Code: codeInvalidParams, Message: "params.message is required"}
	}
	return params.Message, nil
}
