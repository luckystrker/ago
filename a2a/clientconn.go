package a2a

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Client connection knobs: card injection, interface selection with the
// protocol-version check, request interceptors and tasks/get history
// trimming (parity: ag2/ag2/a2a/config.py A2AConfig preset_card / prefer /
// interceptors / history_length, transports/_http.py select_interface and
// validate_protocol_version). Everything is opt-in: a Client built without
// these options keeps posting JSON-RPC to baseURL + "/" and never reads the
// card's interfaces.

// Transport names for WithTransport (parity: TransportName "jsonrpc" | "rest").
const (
	TransportJSONRPC = "jsonrpc"
	TransportREST    = "rest"
)

// ProtocolBindingREST is the card spelling of the HTTP+JSON binding
// (a2a-sdk TransportProtocol.HTTP_JSON).
const ProtocolBindingREST = "HTTP+JSON"

var (
	// ErrInvalidCard marks an agent card the client cannot connect through:
	// no supportedInterfaces, a requested transport the card does not
	// declare, or only bindings this client does not speak (parity:
	// A2AInvalidCardError).
	ErrInvalidCard = errors.New("a2a: invalid agent card")
	// ErrIncompatibleProtocolVersion marks a selected interface advertising a
	// protocol version older than 1.0 (parity:
	// A2AIncompatibleProtocolVersionError).
	ErrIncompatibleProtocolVersion = errors.New("a2a: incompatible protocol version")
	// ErrBindingUnsupported marks an operation the resolved binding does not
	// carry: the push-notification config methods exist on JSON-RPC only.
	ErrBindingUnsupported = errors.New("a2a: operation not supported by the selected binding")
)

// CallInterceptor runs before every outgoing request of a Client, after the
// configured headers are stamped: method is the A2A method name
// ("message/send", "tasks/get", ..., or "agent-card" for discovery) and req
// may be modified in place (extra headers, signing). A non-nil error aborts
// the call with ErrTransport wrapping it.
type CallInterceptor func(ctx context.Context, method string, req *http.Request) error

// WithPresetCard injects an already-resolved card: AgentCard returns a copy
// and interface selection uses it, so no discovery request is made (parity:
// A2AConfig.from_card / preset_card). The presence of a preset card also
// switches on interface selection (see Connect).
func WithPresetCard(card *AgentCard) ClientOption {
	return func(c *Client) { c.preset = card }
}

// WithTransport forces the binding used when the card declares several:
// TransportJSONRPC or TransportREST (parity: A2AConfig.prefer). It switches
// on interface selection: the first call reads the card, honours this
// choice and checks the interface's protocol version.
func WithTransport(name string) ClientOption {
	return func(c *Client) { c.prefer = name }
}

// WithHistoryLength asks the server to trim Task.history on TasksGet to the
// most recent n messages (parity: A2AConfig.history_length); 0 keeps the
// full history. A pure server-side hint — it never changes what is uploaded.
func WithHistoryLength(n int) ClientOption {
	return func(c *Client) { c.historyLen = n }
}

// WithInterceptors appends request interceptors (parity:
// A2AConfig.interceptors).
func WithInterceptors(fns ...CallInterceptor) ClientOption {
	return func(c *Client) { c.interceptors = append(c.interceptors, fns...) }
}

const (
	bindingJSONRPC = "jsonrpc"
	bindingREST    = "rest"
)

// endpoint is the connection target after interface selection.
type endpoint struct {
	binding string
	rpcURL  string // JSON-RPC endpoint (binding jsonrpc)
	restURL string // REST base without trailing slash (binding rest)
}

// Connect resolves the connection target: it takes the preset card or
// fetches it, selects the interface (WithTransport if set, else the one whose
// URL is the client's base URL, else the first listed), refuses a protocol
// version older than 1.0 and remembers the result for every later call. A
// failure is not cached, so a later call retries. Client calls connect lazily
// when WithPresetCard or WithTransport is configured; call Connect directly
// to surface card problems up front, or to opt in without either option.
func (c *Client) Connect(ctx context.Context) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.resolved != nil {
		return nil
	}
	card, err := c.AgentCard(ctx)
	if err != nil {
		return err
	}
	iface, binding, err := selectInterface(card, c.base, c.prefer)
	if err != nil {
		return err
	}
	if err := validateProtocolVersion(iface, binding); err != nil {
		return err
	}
	// The interface URL is the JSON-RPC endpoint verbatim, and (trailing slash
	// trimmed) the base of the REST routes; an interface without one is
	// reached through the client's own base URL.
	rpc := iface.URL
	rest := strings.TrimSuffix(iface.URL, "/")
	if rpc == "" {
		rpc, rest = c.base+rpcPath, c.base
	}
	ep := &endpoint{binding: binding, rpcURL: rpc, restURL: rest}
	c.resolved = ep
	return nil
}

// endpoint returns the connection target: the default (JSON-RPC at
// baseURL + "/") unless interface selection is switched on.
func (c *Client) endpoint(ctx context.Context) (*endpoint, error) {
	c.connMu.Lock()
	resolved := c.resolved
	c.connMu.Unlock()
	if resolved != nil {
		return resolved, nil
	}
	if c.preset == nil && c.prefer == "" {
		return &endpoint{binding: bindingJSONRPC, rpcURL: c.base + rpcPath, restURL: c.base}, nil
	}
	if err := c.Connect(ctx); err != nil {
		return nil, err
	}
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.resolved, nil
}

// bindingName maps a card binding spelling to a transport name; ok is false
// for bindings this client does not speak (gRPC, D-071).
func bindingName(binding string) (string, bool) {
	switch binding {
	case ProtocolBindingJSONRPC:
		return bindingJSONRPC, true
	case ProtocolBindingREST:
		return bindingREST, true
	}
	return "", false
}

// selectInterface picks the interface the client connects through
// (parity: _http.py select_interface): the preferred transport if declared;
// else the interface whose URL is base; else the first listed.
func selectInterface(card *AgentCard, base, prefer string) (AgentInterface, string, error) {
	ifaces := card.SupportedInterfaces
	if len(ifaces) == 0 {
		return AgentInterface{}, "", fmt.Errorf("%w: card of %q has no supportedInterfaces", ErrInvalidCard, base)
	}
	if prefer != "" {
		for _, iface := range ifaces {
			if name, ok := bindingName(iface.ProtocolBinding); ok && name == prefer {
				return iface, name, nil
			}
		}
		declared := make([]string, len(ifaces))
		for i, iface := range ifaces {
			declared[i] = iface.ProtocolBinding
		}
		return AgentInterface{}, "", fmt.Errorf("%w: card of %q does not declare transport %q; available: %v",
			ErrInvalidCard, base, prefer, declared)
	}
	for _, iface := range ifaces {
		if strings.TrimSuffix(iface.URL, "/") == base {
			if name, ok := bindingName(iface.ProtocolBinding); ok {
				return iface, name, nil
			}
		}
	}
	first := ifaces[0]
	name, ok := bindingName(first.ProtocolBinding)
	if !ok {
		return AgentInterface{}, "", fmt.Errorf("%w: card of %q declares unsupported binding %q",
			ErrInvalidCard, base, first.ProtocolBinding)
	}
	return first, name, nil
}

// validateProtocolVersion refuses an interface older than A2A 1.0, a breaking
// change against 0.x that would otherwise surface as obscure RPC failures
// (parity: _http.py validate_protocol_version). An empty or unparsable
// version counts as compatible: the field is optional and 1.0 servers may
// omit it.
func validateProtocolVersion(iface AgentInterface, binding string) error {
	raw := strings.TrimPrefix(strings.TrimSpace(iface.ProtocolVersion), "v")
	major, _, _ := strings.Cut(raw, ".")
	n, err := strconv.Atoi(major)
	if err != nil || n >= 1 {
		return nil
	}
	return fmt.Errorf("%w: %s interface %q advertises protocol version %q, want 1.0 or newer",
		ErrIncompatibleProtocolVersion, binding, iface.URL, iface.ProtocolVersion)
}
