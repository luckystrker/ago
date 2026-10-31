package a2ui

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Encoding selects the response wire encoding (transports/rest.py): SSE
// frames or NDJSON lines. Both share the same request contract.
type Encoding int

const (
	// EncodingSSE streams text/event-stream frames (the Python default).
	EncodingSSE Encoding = iota
	// EncodingJSONL streams application/x-ndjson lines.
	EncodingJSONL
)

func (e Encoding) mediaType() string {
	if e == EncodingJSONL {
		return "application/x-ndjson"
	}
	return "text/event-stream"
}

// Turn is what a prompt handler returns for one turn: the agent's
// conversational prose plus its A2UI messages (dispatch.py's frames).
type Turn struct {
	Text     string
	Messages []Message
}

// PromptHandler receives the turn's prompt input — trailing user messages
// plus the generic rewrites of client envelopes that need the LLM (an
// unregistered click, a client error report, a rendererFunctionResponse) —
// and returns the turn's prose and A2UI messages.
type PromptHandler func(ctx context.Context, prompt string) (Turn, error)

// FunctionNameResolver looks up the name of an issued callRendererFunction by
// its functionCallId, so a rendererFunctionResponse prompt can name it.
type FunctionNameResolver func(functionCallID string) (string, bool)

// Server serves A2UI over HTTP (server.py REST → net/http): it accepts the
// minimal JSON request contract, answers clicks on registered actions itself,
// answers every callAgentFunction, and hands everything that needs the model
// (unregistered clicks, error reports, function responses, user text) to the
// PromptHandler. The server is stateless — clients send the full state each
// turn. Safe for concurrent use.
type Server struct {
	actions         *ActionRegistry
	prompt          PromptHandler
	resolveFunction FunctionNameResolver
	version         string
	catalogID       string
	encoding        Encoding
	logger          Logger
	authToken       string
	maxBody         int64
	turns           chan struct{}
}

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithActions registers clickable actions (the @a2ui_action set).
func WithActions(r *ActionRegistry) ServerOption {
	return func(s *Server) { s.actions = r }
}

// WithPromptHandler sets the seam that runs the model for prompts the server
// cannot answer itself. Without one, unregistered clicks and user text are
// accepted and dropped.
func WithPromptHandler(h PromptHandler) ServerOption {
	return func(s *Server) { s.prompt = h }
}

// WithFunctionNameResolver lets rendererFunctionResponse prompts name the
// function that was called.
func WithFunctionNameResolver(r FunctionNameResolver) ServerOption {
	return func(s *Server) { s.resolveFunction = r }
}

// WithEncoding selects the response encoding (default SSE).
func WithEncoding(e Encoding) ServerOption {
	return func(s *Server) { s.encoding = e }
}

// WithCatalogID overrides the active catalog id used to answer
// callAgentFunction (default DefaultCatalogID).
func WithCatalogID(id string) ServerOption {
	return func(s *Server) { s.catalogID = id }
}

// WithLogger attaches a logger for dropped envelopes and handler failures.
func WithLogger(l Logger) ServerOption {
	return func(s *Server) { s.logger = l }
}

// WithAuthToken turns on the endpoint's access boundary: every request must
// present "Authorization: Bearer <token>", anything else is answered 401
// before the body is read. The comparison is constant-time over SHA-256
// digests, so neither the token nor its length leaks through timing.
//
// Without a token the server does no auth checks and assumes a loopback /
// reverse-proxy trust boundary — anyone who can reach the port can drive the
// prompt handler. Actions marked AuthorizedOnly only ever run on requests
// that passed this check, so they never run on a token-less server.
// AGO-DEVIATION: hardening for non-loopback exposure; Python's a2ui REST
// server has no auth option.
func WithAuthToken(token string) ServerOption {
	return func(s *Server) { s.authToken = token }
}

// The limits a Server applies when nothing is configured: a body of 16 MiB
// and eight POST turns at once — the same defaults the agui Guard uses
// (audit M-23). Large enough that an ordinary A2UI request (full state each
// turn) never notices them, small enough that one client cannot spend the
// server's memory or its LLM budget on its own.
//
// AGO-DEVIATION: Python's a2ui REST server buffers the body and runs turns
// without limits; AGo caps both (security audit 2026-10-31, M-23).
const (
	defaultMaxBodyBytes       int64 = 16 << 20
	defaultMaxConcurrentTurns       = 8
)

// WithMaxBodyBytes caps one request body at max bytes: past it the request
// fails 413 instead of being buffered whole. A non-positive max keeps the
// default.
func WithMaxBodyBytes(max int64) ServerOption {
	return func(s *Server) {
		if max > 0 {
			s.maxBody = max
		}
	}
}

// WithMaxConcurrentTurns caps how many POST turns are served at once; the
// next one gets 429 instead of a prompt-handler (LLM) call of its own. A
// non-positive n keeps the default.
func WithMaxConcurrentTurns(n int) ServerOption {
	return func(s *Server) {
		if n > 0 {
			s.turns = make(chan struct{}, n)
		}
	}
}

// Logger is the minimal logging seam the server uses; nil disables logging.
type Logger interface {
	Printf(format string, args ...any)
}

// NewServer builds an A2UI HTTP server with the default limits: a 16 MiB
// body and eight concurrent POST turns, no token (see WithMaxBodyBytes,
// WithMaxConcurrentTurns, WithAuthToken).
func NewServer(opts ...ServerOption) *Server {
	s := &Server{
		version: VersionV1, catalogID: DefaultCatalogID, encoding: EncodingSSE,
		maxBody: defaultMaxBodyBytes,
		turns:   make(chan struct{}, defaultMaxConcurrentTurns),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// userMessage is one entry of the request's messages array.
type userMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	authorized := false
	if s.authToken != "" {
		if !s.authorizedRequest(r) {
			writeJSONError(w, http.StatusUnauthorized, "missing or invalid authorization token")
			return
		}
		authorized = true
	}
	// One POST is one turn through the prompt handler — the slot covers the
	// whole exchange, so the model call counts against the cap; the next
	// request over the cap is answered, never queued (audit M-23).
	select {
	case s.turns <- struct{}{}:
		defer func() { <-s.turns }()
	default:
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusTooManyRequests, "too many concurrent requests")
		return
	}
	// The cap stops the read at the limit; the excess is never buffered.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// A body past the cap is refused for its size, not for its shape.
		status, msg := http.StatusBadRequest, "could not read request body"
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status, msg = http.StatusRequestEntityTooLarge, "request body exceeds the allowed size"
		}
		writeJSONError(w, status, msg)
		return
	}
	messages, a2ui, status, errMsg := parseRequestBody(body)
	if errMsg != "" {
		writeJSONError(w, status, errMsg)
		return
	}

	// Trailing user messages are the turn's plain text input; any other role
	// must be one the contract knows (request.py _map_messages). The content
	// is client-controlled and lands in an LLM prompt, so it passes through
	// the same sanitizer the envelope rewrites use.
	var userTexts []string
	for _, raw := range messages {
		var m userMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			writeJSONError(w, http.StatusBadRequest, "each message must be an object")
			return
		}
		switch m.Role {
		case "user":
			userTexts = append(userTexts, SanitizeForPrompt(m.Content))
		case "system", "developer", "assistant":
			// system/developer would prepend the model's prompt and assistant
			// replays history; the prompt-handler seam here takes the
			// user-facing input only.
		default:
			writeJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("unsupported message role: '%s'", m.Role))
			return
		}
	}

	prose, outMessages, dispatchErr := s.dispatch(r.Context(), a2ui, userTexts, authorized)

	// Response frames are fully serialized before the status is committed: a
	// message that cannot reach the wire (non-JSON prop value) fails the
	// request with a real status code instead of corrupting the stream.
	var frames []string
	proseFrame := func(text string) string {
		// json.dumps({"text": text}) — default separators, ensure_ascii.
		payload := escapeNonASCII(pyJSON(map[string]any{"text": text}))
		if s.encoding == EncodingJSONL {
			return payload + "\n"
		}
		return "event: text\ndata: " + payload + "\n\n"
	}
	if prose != "" {
		frames = append(frames, proseFrame(prose))
	}
	for _, m := range outMessages {
		line, err := ToJSONL([]Message{m})
		if err != nil {
			warnf(s.logger, "A2UI message serialization failed: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "could not serialize response message")
			return
		}
		if s.encoding == EncodingJSONL {
			frames = append(frames, line+"\n")
		} else {
			frames = append(frames, "data: "+line+"\n\n")
		}
	}
	if dispatchErr != nil {
		warnf(s.logger, "A2UI turn failed: %v", dispatchErr)
		// The turn failed after doing its job; surface the failure as an
		// error frame rather than dropping it silently (rest.py).
		payload := escapeNonASCII(pyJSON(map[string]any{"error": dispatchErr.Error()}))
		if s.encoding == EncodingJSONL {
			frames = append(frames, payload+"\n")
		} else {
			frames = append(frames, "event: error\ndata: "+payload+"\n\n")
		}
	} else if s.encoding == EncodingSSE {
		frames = append(frames, "event: done\ndata: {}\n\n")
	}

	w.Header().Set("Content-Type", s.encoding.mediaType())
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	for _, frame := range frames {
		_, _ = io.WriteString(w, frame)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// authorizedRequest reports whether r carries the server's bearer token.
// Digests are compared (not the raw values) so the comparison is constant
// time regardless of the presented length.
func (s *Server) authorizedRequest(r *http.Request) bool {
	const scheme = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return false
	}
	presented := sha256.Sum256([]byte(header[len(scheme):]))
	expected := sha256.Sum256([]byte(s.authToken))
	return subtle.ConstantTimeCompare(presented[:], expected[:]) == 1
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := marshalNoHTMLEscape(map[string]any{"error": msg})
	_, _ = w.Write([]byte(escapeNonASCII(string(b))))
}

// parseRequestBody validates the minimal JSON request contract (request.py):
// messages, variables, and the client→server a2ui envelope array.
func parseRequestBody(body []byte) (messages []json.RawMessage, a2ui []any, status int, errMsg string) {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, nil, http.StatusBadRequest, fmt.Sprintf("request body is not valid JSON: %v", err)
	}
	root, ok := decoded.(map[string]any)
	if !ok {
		return nil, nil, http.StatusBadRequest, "request body must be a JSON object"
	}
	if raw, present := root["messages"]; present && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return nil, nil, http.StatusBadRequest, "'messages' must be a list"
		}
		for _, item := range list {
			if _, ok := item.(map[string]any); !ok {
				return nil, nil, http.StatusBadRequest, "each message must be an object"
			}
			rawMsg, err := marshalNoHTMLEscape(item)
			if err != nil {
				return nil, nil, http.StatusBadRequest, "each message must be an object"
			}
			messages = append(messages, rawMsg)
		}
	}
	if raw, present := root["variables"]; present && raw != nil {
		if _, ok := raw.(map[string]any); !ok {
			return nil, nil, http.StatusBadRequest, "'variables' must be an object"
		}
	}
	if raw, present := root["a2ui"]; present && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return nil, nil, http.StatusBadRequest, "'a2ui' must be a list"
		}
		a2ui = list
	}
	return messages, a2ui, 0, ""
}

// dispatch runs one turn's client interactions (dispatch.py stream_turn +
// server_action.py run_client_interactions): clicks on registered actions and
// every callAgentFunction are answered here; unregistered clicks, error
// reports, function responses and user text go to the prompt handler. It
// returns the turn's prose (if any) and the messages to emit, or the error
// that produced an error frame. authorized reports whether the request
// passed the auth-token check (always false without WithAuthToken).
func (s *Server) dispatch(ctx context.Context, envelopes []any, userTexts []string, authorized bool) (prose string, out []Message, dispatchErr error) {
	var prompts []string

	for _, envelope := range envelopes {
		result := ParseIncomingMessage(envelope)
		switch result.Kind {
		case IncomingActionKind:
			action, registered := s.actions.Get(result.Action.Name)
			if registered {
				if action.AuthorizedOnly && !authorized {
					// A privileged handler must not run for an anonymous
					// request, and its click must not fall through to the
					// LLM either — drop it.
					warnf(s.logger, "Dropping A2UI action %q: it requires an authorized request.",
						result.Action.Name)
					continue
				}
				out = append(out, s.runServerAction(ctx, action, result.Action)...)
				continue
			}
			prompt, ok := ActionToPrompt(result.Action)
			if !ok {
				warnf(s.logger, "Dropping A2UI action with no name.")
				continue
			}
			prompts = append(prompts, prompt)
		case IncomingFunctionCallKind:
			// Answered on the server even when nothing is registered — the
			// renderer is waiting; on v1.0 the answer always exists.
			out = append(out, s.runAgentFunction(ctx, result.FunctionCall, authorized)...)
		case IncomingFunctionResponseKind:
			name := ""
			if s.resolveFunction != nil {
				if n, ok := s.resolveFunction(result.FunctionResponse.FunctionCallID); ok {
					name = n
				}
			}
			prompts = append(prompts, FunctionResponseToPrompt(result.FunctionResponse, name))
		case IncomingErrorKind:
			prompts = append(prompts, ErrorToPrompt(result.Error))
		default:
			warnf(s.logger, "Skipping A2UI envelope of unrecognized kind: %s", result.ParseError)
		}
	}

	// A turn whose only content was handled server-side interactions (or that
	// carries nothing) is complete already; don't fabricate a blank agent turn
	// (dispatch.py). AGO-DEVIATION: Python also runs a blank turn when a
	// request has no inputs and no handled interactions; this stateless
	// server has no agent to fabricate one with and answers what it has.
	if len(userTexts) == 0 && len(prompts) == 0 {
		return "", out, nil
	}

	// Python feeds trailing user inputs first, then the rewritten prompts,
	// as the current turn's inputs; the seam takes them as one prompt.
	input := strings.Join(append(append([]string{}, userTexts...), prompts...), "\n")
	turn, err := s.runPrompt(ctx, input)
	if err != nil {
		return "", out, err
	}
	out = append(out, turn.Messages...)
	return turn.Text, out, nil
}

func (s *Server) runPrompt(ctx context.Context, input string) (Turn, error) {
	if s.prompt == nil {
		return Turn{}, nil
	}
	return s.prompt(ctx, input)
}

// runServerAction runs a click's handler and maps its result to A2UI messages
// (server_action.py run_server_action): a failing handler is logged and
// answered with nothing — it must not tear down the turn.
func (s *Server) runServerAction(ctx context.Context, action Action, click *IncomingAction) (messages []Message) {
	defer func() {
		if rec := recover(); rec != nil {
			warnf(s.logger, "A2UI server action %q failed: %v", click.Name, rec)
			messages = nil
		}
	}()
	result, err := action.Fn(ctx, click.Context)
	if err != nil {
		warnf(s.logger, "A2UI server action %q failed: %v", click.Name, err)
		return nil
	}
	messages = MessagesOf(result, s.version)
	if len(messages) == 0 && result != nil {
		warnf(s.logger, "A2UI server action %q returned a value, but an action has no response channel; "+
			"dropping it. Use a callAgentFunction to return a value to the renderer.", click.Name)
	}
	return messages
}

// runAgentFunction answers a renderer's callAgentFunction (v1.0) with an
// agentFunctionResponse correlated by functionCallId — always exactly one
// (server_action.py run_agent_function): UNKNOWN_FUNCTION when nothing is
// registered under that name, the catalog does not match, or an anonymous
// request names a privileged (AuthorizedOnly) handler; INVALID_FUNCTION_CALL
// when the arguments do not fit, FUNCTION_FAILED when the handler raised.
// Messages the handler returns are emitted ahead of the response.
func (s *Server) runAgentFunction(ctx context.Context, call *IncomingFunctionCall, authorized bool) (messages []Message) {
	if !call.CatalogIDValid {
		return []Message{FunctionResponseMessage(call.FunctionCallID, s.version, nil,
			&FunctionError{Code: CodeInvalidCall,
				Message: fmt.Sprintf("Invalid catalogId for function '%s'.", call.Call)})}
	}
	action, registered := s.actions.Get(call.Call)
	// A privileged handler is answered as unknown so an anonymous caller
	// cannot learn it exists, let alone run it.
	if !registered || (call.CatalogID != "" && call.CatalogID != s.catalogID) ||
		(action.AuthorizedOnly && !authorized) {
		return []Message{FunctionResponseMessage(call.FunctionCallID, s.version, nil,
			&FunctionError{Code: CodeUnknownFunction,
				Message: fmt.Sprintf("Unknown function '%s'.", call.Call)})}
	}
	args, err := call.Arguments()
	if err != nil {
		return []Message{InvalidArgumentsResponse(call, s.version)}
	}

	defer func() {
		if rec := recover(); rec != nil {
			warnf(s.logger, "A2UI function %q failed: %v", call.Call, rec)
			messages = []Message{FunctionResponseMessage(call.FunctionCallID, s.version, nil,
				&FunctionError{Code: CodeFunctionFailed,
					Message: fmt.Sprintf("Function '%s' failed.", call.Call)})}
		}
	}()
	result, fnErr := action.Fn(ctx, args)
	if fnErr != nil {
		warnf(s.logger, "A2UI function %q failed: %v", call.Call, fnErr)
		return []Message{FunctionResponseMessage(call.FunctionCallID, s.version, nil,
			&FunctionError{Code: CodeFunctionFailed,
				Message: fmt.Sprintf("Function '%s' failed.", call.Call)})}
	}

	messages = MessagesOf(result, s.version)
	// Messages are the handler's surface updates; there is no separate value.
	value := any(nil)
	if len(messages) == 0 {
		value = result
	}
	return append(messages, FunctionResponseMessage(call.FunctionCallID, s.version, value, nil))
}
