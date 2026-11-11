package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Tenants (parity: ag2/ag2/a2a/_session.py with_tenant plus the a2a-sdk's
// tenant plumbing). One server can expose the same agent to several tenants
// whose tasks must never meet:
//
//   - the client names the tenant per request — in the JSON-RPC params
//     ("tenant", which AG2's with_tenant injects) or on the REST path
//     ("/{tenant}/message:send", the SDK's Mount(path="/{tenant}") twin);
//   - the card advertises the tenant on the interface it is reached through
//     (AgentInterface.Tenant), and a client that resolves its interface
//     adopts that tenant when it was not told one;
//   - the server resolves the tenant's TaskStore through ServerConfig
//     .StoreForTenant, so a turn's tasks are stored per tenant and a tenant
//     cannot read, cancel or list another's.
//
// Per-call override from AG2's context.variables["a2a:tenant"] belongs to the
// conversational client (D-049): this package's Client is transport-level, so
// WithTenant is that knob here.

// TenantRequest is one request as a TenantResolver sees it: the live request
// (headers, URL, remote address), the tenant the client asked for, and the
// identity AuthorizeIdentity produced for that request.
type TenantRequest struct {
	// Request is the incoming HTTP request. Reading it is safe: the body is
	// unconsumed, and the resolver runs before any handler.
	Request *http.Request
	// Tenant is the tenant the client named — the JSON-RPC "tenant" param, the
	// REST path segment, or ServerConfig.Tenant — and "" when it named none.
	Tenant string
	// Identity is what AuthorizeIdentity returned for this request; nil when
	// the server was configured with the older Authorize hook (or with none).
	Identity any
}

// TenantResolver decides the tenant of one request, and is the place to check
// that the caller may act as it. Returning an error refuses the request: the
// JSON-RPC surface answers -32602, the REST surface 403, with the error text
// kept off the wire. Returning "" serves the request from the default store.
//
// It runs for every tenant-consuming request of a server that configured it —
// including one that names no tenant, so a deployment can derive the tenant
// from the authenticated identity and never trust the client's claim.
type TenantResolver func(ctx context.Context, req *TenantRequest) (string, error)

// identityContextKey carries the AuthorizeIdentity result.
type identityContextKey struct{}

// withIdentity stamps ctx with the authenticated caller's identity.
func withIdentity(ctx context.Context, identity any) context.Context {
	if identity == nil {
		return ctx
	}
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFrom returns the identity AuthorizeIdentity produced for the request
// behind ctx (nil when the server authenticated no identity). Handlers and a
// TenantResolver receive it through the request context as well as through
// TenantRequest.Identity.
func IdentityFrom(ctx context.Context) any {
	identity, _ := ctx.Value(identityContextKey{}).(any)
	return identity
}

// ErrUnknownTenant reports a request naming a tenant the server's
// ServerConfig.StoreForTenant does not resolve; the server answers -32602
// (invalid params), and a REST caller gets 400.
var ErrUnknownTenant = errors.New("a2a: unknown tenant")

// tenantContextKey carries the tenant of the request being served.
type tenantContextKey struct{}

// withTenant stamps ctx with the tenant a request names.
func withTenant(ctx context.Context, tenant string) context.Context {
	if tenant == "" {
		return ctx
	}
	return context.WithValue(ctx, tenantContextKey{}, tenant)
}

// tenantOf returns the tenant stamped on ctx ("" when the request names none).
func tenantOf(ctx context.Context) string {
	tenant, _ := ctx.Value(tenantContextKey{}).(string)
	return tenant
}

// WithTenant scopes every request of the client to a tenant (parity:
// A2AConfig.tenant — the tenant rides the JSON-RPC params and prefixes the
// REST paths). It also overrides a tenant the client adopted from the card's
// interface.
//
// WARNING — the tenant is a claim, not a credential: it says which tenant the
// caller wants, and a server that does not check it (see
// ServerConfig.TenantResolver) serves whatever is asked for. Never treat the
// tenant name as proof that the caller belongs to it.
func WithTenant(tenant string) ClientOption {
	return func(c *Client) { c.tenant = tenant }
}

// tenantParams stamps "tenant" into a marshalled JSON-RPC params object that
// does not carry one yet. Params that are not a JSON object (an array, a bare
// scalar) are left alone.
func tenantParams(params json.RawMessage, tenant string) json.RawMessage {
	if tenant == "" || len(params) == 0 {
		return params
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(params, &fields); err != nil || fields == nil {
		return params
	}
	if _, ok := fields["tenant"]; ok {
		return params
	}
	encoded, err := json.Marshal(tenant)
	if err != nil {
		return params
	}
	fields["tenant"] = encoded
	patched, err := json.Marshal(fields)
	if err != nil {
		return params
	}
	return patched
}

// restPath joins an endpoint's REST base with path, inserting the tenant as
// the first segment when the connection is scoped to one (parity: the SDK's
// TenantTransportDecorator prepending the tenant to every REST path).
func restPath(ep *endpoint, path string) string {
	if ep.tenant == "" {
		return ep.restURL + path
	}
	return ep.restURL + "/" + url.PathEscape(ep.tenant) + path
}

// tenantRouting reports whether the server resolves tenants at all: with no
// resolver and no per-tenant store a tenant name is inert, the pre-tenant
// single-store behavior.
func (s *Server) tenantRouting() bool {
	return s.cfg.StoreForTenant != nil || s.cfg.TenantResolver != nil
}

// resolveTenant resolves the request's tenant and stamps it on the returned
// context. Without a TenantResolver the client-named tenant is taken at face
// value (the StoreForTenant-only form, which authorizes nothing); with one, the
// resolver decides and may refuse by returning an error.
func (s *Server) resolveTenant(ctx context.Context, r *http.Request, requested string) (context.Context, error) {
	if s.cfg.TenantResolver == nil {
		if s.cfg.StoreForTenant == nil {
			return ctx, nil
		}
		return withTenant(ctx, requested), nil
	}
	tenant, err := s.cfg.TenantResolver(ctx, &TenantRequest{
		Request:  r,
		Tenant:   requested,
		Identity: IdentityFrom(ctx),
	})
	if err != nil {
		return ctx, err
	}
	return withTenant(ctx, tenant), nil
}

// tenantSurface reports whether a request is one whose tenant the server
// resolves: the REST message/task routes (with or without a tenant segment).
// The card routes are public and stay tenant-free; the JSON-RPC endpoint is
// resolved in serveRPC, where the params carry the tenant.
func tenantSurface(path string) bool {
	_, rest := splitTenantPath(path)
	switch {
	case rest == RESTMessageSendPath, rest == RESTTasksPath:
		return true
	case strings.HasPrefix(rest, RESTTasksPath+"/"):
		return true
	}
	return false
}

// storeFor returns the TaskStore the request's tenant owns, or the server's
// single store when tenancy is not configured (the pre-tenant behavior: every
// request shares one store and a tenant name is inert).
func (s *Server) storeFor(ctx context.Context) (TaskStore, error) {
	if s.cfg.StoreForTenant == nil {
		return s.store, nil
	}
	tenant := tenantOf(ctx)
	if tenant == "" {
		return s.store, nil
	}
	store := s.cfg.StoreForTenant(tenant)
	if store == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTenant, tenant)
	}
	return store, nil
}

// tenantFromParams reads the tenant a JSON-RPC request names.
func tenantFromParams(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var probe struct {
		Tenant string `json:"tenant"`
	}
	if err := json.Unmarshal(params, &probe); err != nil {
		return ""
	}
	return probe.Tenant
}

// restBaseRoute reports whether path already names one of the server's own
// REST routes, which therefore cannot be a tenant segment.
func restBaseRoute(path string) bool {
	switch {
	case path == RESTMessageSendPath, path == RESTTasksPath, path == ExtendedCardPath:
		return true
	case strings.HasPrefix(path, RESTTasksPath+"/"), strings.HasPrefix(path, "/.well-known/"):
		return true
	}
	return false
}

// splitTenantPath splits a REST path of the form "/{tenant}/route" into the
// tenant and the route below it (parity: the SDK mounting every REST route a
// second time under Mount(path="/{tenant}")). A path that names a base route
// directly, or that has no route below the first segment, is not tenant-
// scoped: tenant is empty and path is returned unchanged.
func splitTenantPath(path string) (tenant, rest string) {
	if path == "" || path == rpcPath || restBaseRoute(path) {
		return "", path
	}
	trimmed := strings.TrimPrefix(path, "/")
	first, remainder, ok := strings.Cut(trimmed, "/")
	if !ok || first == "" || remainder == "" {
		return "", path
	}
	return first, "/" + remainder
}
