// Package mcpui builds MCP UI resources and UI actions: the data constructors of
// Python AG2's ag2/mcp_ui (resources.py, actions.py). A UI resource is a block a
// tool result hands to an MCP-UI-capable host, which renders HTML, an iframe URL
// or a Remote-DOM script from it; a UI action is the JSON an in-frame button
// posts back to the host.
//
// The Python package delegates to the mcp-ui-server SDK; AGo reproduces the wire
// shapes directly (stdlib only — the SDK adds no behavior the constructors need).
// Shapes verified against mcp-ui-server 1.0.0 + mcp 2.x serialization
// (by_alias, exclude_none): see docs/DECISIONS.md D-044.
package mcpui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// UIMetadataPrefix marks client-facing UI keys inside the resource `_meta`; the
// client recognizes a key only when it carries this prefix (Python parity:
// mcp_ui_server.types.UI_METADATA_PREFIX).
const UIMetadataPrefix = "mcpui.dev/ui-"

// Content MIME types. The client picks its renderer from the MIME type, so the
// type identifies the content kind on the wire; Remote-DOM additionally carries
// the framework as a parameter.
const (
	MIMEHTML             = "text/html"
	MIMEURL              = "text/uri-list"
	MIMERemoteDOMReact   = "application/vnd.mcp-ui.remote-dom+javascript; framework=react"
	MIMERemoteDOMWebComp = "application/vnd.mcp-ui.remote-dom+javascript; framework=webcomponents"
)

// Encoding selects the wire form of the resource contents: "text" ships the
// content string verbatim, "blob" base64-encodes it.
type Encoding string

const (
	EncodingText Encoding = "text"
	EncodingBlob Encoding = "blob"
)

// Framework names the Remote-DOM renderer the client mounts the script with.
type Framework string

const (
	FrameworkReact         Framework = "react"
	FrameworkWebComponents Framework = "webcomponents"
)

// Errors reported by the package. Match with errors.Is.
var (
	// ErrInvalidURI reports a uri outside the ui:// scheme (Python InvalidURIError).
	ErrInvalidURI = errors.New("mcpui: invalid uri")
	// ErrInvalidContent reports empty content, an unknown framework or an unknown
	// encoding (Python InvalidContentError).
	ErrInvalidContent = errors.New("mcpui: invalid content")
)

// Resource is the wire form of a UI resource (Python parity: mcp-ui-server
// UIResource, an mcp EmbeddedResource). Type is always "resource"; marshal order
// matches the mcp SDK (type, then the contents block).
//
// Rendering the exact mcp wire bytes takes Marshal below: encoding/json's
// default entry point always HTML-escapes <, > and & (\u003c, \u003e, \u0026),
// and it re-escapes even a Marshaler's output, so no custom MarshalJSON can
// opt out.
type Resource struct {
	Type     string   `json:"type"`
	Resource Contents `json:"resource"`
}

// Marshal renders res as the exact wire bytes the mcp SDK produces (Python
// json/pydantic emit no HTML escapes). For JSON consumed by a parser the plain
// json.Marshal is equivalent — \u003c decodes to the same string as < — so the
// helper matters only for byte-exact wire comparisons.
func Marshal(res Resource) ([]byte, error) {
	// Alias sheds nothing (Resource has no MarshalJSON); it keeps the encoder
	// call shape obvious and non-recursive if one is ever added.
	type wire Resource
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(wire(res)); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Contents carries the resource body: URI plus the MIME-typed payload. A text
// encoding fills Text, a blob encoding fills Blob; the empty field is omitted
// from the wire form. Meta mirrors the resource `_meta` (both spellings follow
// the MCP wire aliases, not the Go field names).
type Contents struct {
	URI      string         `json:"uri"`
	MimeType string         `json:"mimeType"`
	Meta     map[string]any `json:"_meta,omitempty"`
	Text     string         `json:"text,omitempty"`
	Blob     string         `json:"blob,omitempty"`
}

// Option customizes a resource constructor call (Python parity: the ui_metadata /
// metadata / encoding / framework keyword arguments).
type Option func(*settings)

type settings struct {
	encoding   Encoding
	framework  Framework
	uiMetadata map[string]any
	metadata   map[string]any
}

// WithEncoding sets the wire form (default EncodingText).
func WithEncoding(e Encoding) Option { return func(s *settings) { s.encoding = e } }

// WithFramework sets the Remote-DOM framework (default FrameworkReact). Only
// RemoteDOM reads it.
func WithFramework(f Framework) Option { return func(s *settings) { s.framework = f } }

// WithUIMetadata passes client UI hints; each key is prefixed with
// UIMetadataPrefix so the client recognizes it.
func WithUIMetadata(m map[string]any) Option { return func(s *settings) { s.uiMetadata = m } }

// WithMetadata passes caller metadata, written verbatim into the resource
// `_meta`; it wins over the prefixed UI keys on a collision.
func WithMetadata(m map[string]any) Option { return func(s *settings) { s.metadata = m } }

// RawHTML builds a UI resource rendering an inline HTML string. The uri must
// start with ui:// and html must be non-empty (Python parity: mcp_ui.raw_html).
func RawHTML(uri string, html string, opts ...Option) (Resource, error) {
	return build(uri, html, "html", MIMEHTML, opts...)
}

// ExternalURL builds a UI resource the client renders by embedding url in an
// iframe. The uri must start with ui:// and url must be non-empty.
func ExternalURL(uri string, url string, opts ...Option) (Resource, error) {
	return build(uri, url, "url", MIMEURL, opts...)
}

// RemoteDOM builds a UI resource carrying a Remote-DOM script the client mounts.
// The uri is validated before the framework, so a caller tripping both guards
// gets the shared uri error first.
func RemoteDOM(uri string, script string, opts ...Option) (Resource, error) {
	s := apply(opts)
	// The uri guard runs ahead of the builder-specific framework guard so the
	// error every builder shares is the one a caller trips first (Python parity:
	// remote_dom checks _require_ui_uri before resolving the MIME type).
	if err := requireUIURI(uri); err != nil {
		return Resource{}, err
	}
	mime, err := remoteDOMMIME(s.framework)
	if err != nil {
		return Resource{}, err
	}
	return build(uri, script, "script", mime, opts...)
}

// build constructs the resource contents (Python parity: mcp_ui.resources._build,
// reproducing mcp_ui_server.create_ui_resource minus its AnyUrl bug).
func build(uri string, content string, contentName string, mimeType string, opts ...Option) (Resource, error) {
	if err := requireUIURI(uri); err != nil {
		return Resource{}, err
	}
	if content == "" {
		return Resource{}, fmt.Errorf("%s must be provided as a non-empty string: %w", contentName, ErrInvalidContent)
	}
	s := apply(opts)
	meta := resourceMeta(s.uiMetadata, s.metadata)
	c := Contents{URI: uri, MimeType: mimeType, Meta: meta}
	switch s.encoding {
	case EncodingText:
		c.Text = content
	case EncodingBlob:
		c.Blob = base64.StdEncoding.EncodeToString([]byte(content))
	default:
		return Resource{}, fmt.Errorf("Invalid encoding type: %s: %w", s.encoding, ErrInvalidContent)
	}
	return Resource{Type: "resource", Resource: c}, nil
}

// apply resolves the option defaults: text encoding, react framework.
func apply(opts []Option) settings {
	s := settings{encoding: EncodingText, framework: FrameworkReact}
	for _, opt := range opts {
		opt(&s)
	}
	return s
}

// requireUIURI rejects a uri outside the ui:// scheme. Called ahead of any
// builder-specific validation so the guard every builder shares is the one a
// caller trips first.
func requireUIURI(uri string) error {
	if strings.HasPrefix(uri, "ui://") {
		return nil
	}
	return fmt.Errorf("URI must start with 'ui://' but got: %s: %w", uri, ErrInvalidURI)
}

// remoteDOMMIME maps the framework to its wire MIME type.
func remoteDOMMIME(f Framework) (string, error) {
	switch f {
	case FrameworkReact:
		return MIMERemoteDOMReact, nil
	case FrameworkWebComponents:
		return MIMERemoteDOMWebComp, nil
	default:
		return "", fmt.Errorf("framework must be 'react' or 'webcomponents', got: %s: %w", f, ErrInvalidContent)
	}
}

// resourceMeta merges the prefixed UI keys with the caller metadata (caller keys
// win), mirroring `_meta = {prefixed} | metadata`. An empty merge yields nil so
// the `_meta` field drops off the wire form.
func resourceMeta(uiMetadata map[string]any, metadata map[string]any) map[string]any {
	if len(uiMetadata) == 0 && len(metadata) == 0 {
		return nil
	}
	meta := make(map[string]any, len(uiMetadata)+len(metadata))
	for key, value := range uiMetadata {
		meta[UIMetadataPrefix+key] = value
	}
	for key, value := range metadata {
		meta[key] = value
	}
	return meta
}
