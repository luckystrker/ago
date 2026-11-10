package mcpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	agocontext "github.com/luckystrker/ago/context"
)

// ResourceContent is a resource body: text, or binary when Blob is set (Python
// parity: str → text, bytes → binary). MIMEType overrides the resource's own for
// this read.
type ResourceContent struct {
	Text     string
	Blob     []byte
	MIMEType string
}

// Resource is a static MCP resource exposed at a fixed URI (Python parity:
// ag2.mcp.Resource). Read returns the body and may use cx, the request-scoped
// context the served tools get (variables, dependencies, RequestDependency).
//
// AGO-DEVIATION: Python lets Title, Description, MIMEType and the values inside
// Meta be Variables resolved per request; AGo configs are plain values, so they
// are fixed at registration. Read still sees the request through cx.
type Resource struct {
	URI  string
	Name string
	// Read produces the body. MIMEType defaults to text/plain for text and
	// application/octet-stream for a blob.
	Read func(ctx context.Context, cx *agocontext.Context) (ResourceContent, error)

	// Title is the display name shown in resources/list (the read result has no
	// such field); Name stays the identifier.
	Title       string
	Description string
	MIMEType    string
	// Meta reaches _meta on both the listing entry and the read result. Empty
	// puts no _meta on the wire.
	Meta map[string]any
	// Unlisted keeps the resource out of resources/list; it stays readable by URI.
	Unlisted bool
}

// ResourceTemplate is a dynamic resource addressed by an RFC 6570 URI template
// (Python parity: ag2.mcp.ResourceTemplate). Only the simple-string ({var}) and
// reserved ({+var}) forms are supported: {var} matches one path segment, {+var}
// matches across "/". Read receives the matched variables, percent-decoded once
// as UTF-8 with a literal "+" preserved.
//
// Decoding never widens what a form matches: a {var} value stays one segment
// after decoding, so "files:///a%2Fb" does not match "files:///{var}". A
// {+var} value may span "/" and contain "..", so a reader that addresses a
// filesystem confines the path itself. A URI whose escapes are not valid UTF-8
// matches no template.
type ResourceTemplate struct {
	URITemplate string
	Name        string
	Read        func(ctx context.Context, vars map[string]string) (ResourceContent, error)
	Description string
	MIMEType    string
}

// ErrResourceNotFound reports a resources/read for a URI no resource or template
// matches; it reaches the client as JSON-RPC invalid params (-32602).
var ErrResourceNotFound = errors.New("mcpserver: no resource matches the URI")

// ErrPromptNotFound reports a prompts/get for an unknown prompt name.
var ErrPromptNotFound = errors.New("mcpserver: no such prompt")

var templateVar = regexp.MustCompile(`\{(\+?)(\w+)\}`)

type compiledTemplate struct {
	re          *regexp.Regexp
	segmentVars map[string]bool
	t           ResourceTemplate
}

// compileTemplate turns a {var}/{+var} template into a match expression.
func compileTemplate(t ResourceTemplate) (compiledTemplate, error) {
	var sb strings.Builder
	sb.WriteString("^")
	seg := map[string]bool{}
	last := 0
	for _, m := range templateVar.FindAllStringSubmatchIndex(t.URITemplate, -1) {
		sb.WriteString(regexp.QuoteMeta(t.URITemplate[last:m[0]]))
		reserved := t.URITemplate[m[2]:m[3]] == "+"
		name := t.URITemplate[m[4]:m[5]]
		if reserved {
			fmt.Fprintf(&sb, "(?P<%s>.+)", name)
		} else {
			fmt.Fprintf(&sb, "(?P<%s>[^/]+)", name)
			seg[name] = true
		}
		last = m[1]
	}
	sb.WriteString(regexp.QuoteMeta(t.URITemplate[last:]))
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return compiledTemplate{}, fmt.Errorf("mcpserver: resource template %q: %w", t.URITemplate, err)
	}
	return compiledTemplate{re: re, segmentVars: seg, t: t}, nil
}

// match returns the decoded variables, or false when the URI addresses nothing
// here (no match, bad escapes, or a decoded separator inside a {var}).
func (c compiledTemplate) match(uri string) (map[string]string, bool) {
	m := c.re.FindStringSubmatch(uri)
	if m == nil {
		return nil, false
	}
	vars := map[string]string{}
	for i, name := range c.re.SubexpNames() {
		if i == 0 || name == "" {
			continue
		}
		decoded, err := url.PathUnescape(m[i])
		if err != nil || !utf8.ValidString(decoded) {
			return nil, false
		}
		if c.segmentVars[name] && strings.Contains(decoded, "/") {
			return nil, false
		}
		vars[name] = decoded
	}
	return vars, true
}

// resourceProvider serves the registered resources and templates.
type resourceProvider struct {
	resources []Resource
	byURI     map[string]int
	templates []compiledTemplate
}

func newResourceProvider(rs []Resource, ts []ResourceTemplate) (*resourceProvider, error) {
	p := &resourceProvider{resources: rs, byURI: map[string]int{}}
	for i, r := range rs {
		if r.URI == "" || r.Read == nil {
			return nil, fmt.Errorf("%w: a Resource needs a URI and a Read function", ErrNotProvided)
		}
		if _, dup := p.byURI[r.URI]; dup {
			return nil, fmt.Errorf("%w: duplicate resource URI %q", ErrConflict, r.URI)
		}
		p.byURI[r.URI] = i
	}
	for _, t := range ts {
		if t.URITemplate == "" || t.Read == nil {
			return nil, fmt.Errorf("%w: a ResourceTemplate needs a URITemplate and a Read function", ErrNotProvided)
		}
		c, err := compileTemplate(t)
		if err != nil {
			return nil, err
		}
		p.templates = append(p.templates, c)
	}
	return p, nil
}

func (p *resourceProvider) hasTemplates() bool { return len(p.templates) > 0 }

// list is the resources/list result: the listed resources, in registration order.
func (p *resourceProvider) list() map[string]any {
	out := make([]map[string]any, 0, len(p.resources))
	for _, r := range p.resources {
		if r.Unlisted {
			continue
		}
		e := map[string]any{"uri": r.URI, "name": r.Name}
		putNonEmpty(e, "title", r.Title)
		putNonEmpty(e, "description", r.Description)
		putNonEmpty(e, "mimeType", r.MIMEType)
		if len(r.Meta) > 0 {
			e["_meta"] = r.Meta
		}
		out = append(out, e)
	}
	return map[string]any{"resources": out}
}

func (p *resourceProvider) listTemplates() map[string]any {
	out := make([]map[string]any, 0, len(p.templates))
	for _, c := range p.templates {
		e := map[string]any{"uriTemplate": c.t.URITemplate, "name": c.t.Name}
		putNonEmpty(e, "description", c.t.Description)
		putNonEmpty(e, "mimeType", c.t.MIMEType)
		out = append(out, e)
	}
	return map[string]any{"resourceTemplates": out}
}

func putNonEmpty(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// read resolves a URI against the static resources, then the templates in
// registration order.
func (p *resourceProvider) read(ctx context.Context, cx *agocontext.Context, uri string) (map[string]any, error) {
	if i, ok := p.byURI[uri]; ok {
		r := p.resources[i]
		body, err := r.Read(ctx, cx)
		if err != nil {
			return nil, err
		}
		if body.MIMEType == "" {
			body.MIMEType = r.MIMEType // the listing's media type also types the read
		}
		return readResult(uri, body, r.Meta), nil
	}
	for _, c := range p.templates {
		if vars, ok := c.match(uri); ok {
			body, err := c.t.Read(ctx, vars)
			if err != nil {
				return nil, err
			}
			if body.MIMEType == "" {
				body.MIMEType = c.t.MIMEType
			}
			return readResult(uri, body, nil), nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrResourceNotFound, uri)
}

// readResult maps a body onto the wire contents variant matching its type, with
// the MIME defaults Resource documents (mcp.types.TextResourceContents /
// BlobResourceContents).
func readResult(uri string, body ResourceContent, meta map[string]any) map[string]any {
	c := map[string]any{"uri": uri}
	if body.Blob != nil {
		c["mimeType"] = orDefault(body.MIMEType, "application/octet-stream")
		c["blob"] = base64.StdEncoding.EncodeToString(body.Blob)
	} else {
		c["mimeType"] = orDefault(body.MIMEType, "text/plain")
		c["text"] = body.Text
	}
	if len(meta) > 0 {
		c["_meta"] = meta
	}
	return map[string]any{"contents": []map[string]any{c}}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// --- prompts -----------------------------------------------------------------------

// PromptArgument is a declared argument of a Prompt, advertised in prompts/list.
type PromptArgument struct {
	Name        string
	Description string
	Required    bool
}

// PromptMessage is one message of a rendered prompt.
type PromptMessage struct {
	// Role is "user" or "assistant".
	Role string
	Text string
}

// Prompt is a reusable prompt template exposed over MCP (prompts/list +
// prompts/get; Python parity: ag2.mcp.Prompt). Render receives the supplied
// arguments and returns the messages; required arguments are present before it
// runs. A Render returning no messages renders nothing.
type Prompt struct {
	Name        string
	Description string
	Arguments   []PromptArgument
	Render      func(ctx context.Context, args map[string]string) ([]PromptMessage, error)
}

// UserPrompt renders text as one user message — the shape Python's bare-string
// render result takes.
func UserPrompt(text string) []PromptMessage { return []PromptMessage{{Role: "user", Text: text}} }

type promptProvider struct {
	prompts []Prompt
	byName  map[string]int
}

func newPromptProvider(ps []Prompt) (*promptProvider, error) {
	p := &promptProvider{prompts: ps, byName: map[string]int{}}
	for i, pr := range ps {
		if pr.Name == "" || pr.Render == nil {
			return nil, fmt.Errorf("%w: a Prompt needs a Name and a Render function", ErrNotProvided)
		}
		if _, dup := p.byName[pr.Name]; dup {
			return nil, fmt.Errorf("%w: duplicate prompt name %q", ErrConflict, pr.Name)
		}
		p.byName[pr.Name] = i
	}
	return p, nil
}

func (p *promptProvider) list() map[string]any {
	out := make([]map[string]any, 0, len(p.prompts))
	for _, pr := range p.prompts {
		args := make([]map[string]any, 0, len(pr.Arguments))
		for _, a := range pr.Arguments {
			e := map[string]any{"name": a.Name, "required": a.Required}
			putNonEmpty(e, "description", a.Description)
			args = append(args, e)
		}
		e := map[string]any{"name": pr.Name, "arguments": args}
		putNonEmpty(e, "description", pr.Description)
		out = append(out, e)
	}
	return map[string]any{"prompts": out}
}

// errMissingPromptArgs carries the names, so the wire message lists them.
type errMissingPromptArgs struct {
	prompt  string
	missing []string
}

func (e *errMissingPromptArgs) Error() string {
	return fmt.Sprintf("Missing required arguments for prompt '%s': %s", e.prompt, strings.Join(e.missing, ", "))
}

func (p *promptProvider) get(ctx context.Context, name string, args map[string]string) (map[string]any, error) {
	i, ok := p.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrPromptNotFound, name)
	}
	pr := p.prompts[i]
	var missing []string
	for _, a := range pr.Arguments {
		if _, present := args[a.Name]; a.Required && !present {
			missing = append(missing, a.Name)
		}
	}
	if len(missing) > 0 {
		return nil, &errMissingPromptArgs{prompt: name, missing: missing}
	}
	msgs, err := pr.Render(ctx, args)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{"role": m.Role, "content": map[string]any{"type": "text", "text": m.Text}})
	}
	res := map[string]any{"messages": out}
	putNonEmpty(res, "description", pr.Description)
	return res, nil
}
