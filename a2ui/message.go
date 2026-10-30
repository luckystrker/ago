package a2ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Errors reported by the wire types.
var (
	// ErrNoPayload reports a Message with no server→client payload set.
	ErrNoPayload = errors.New("a2ui: message has no payload (set exactly one of CreateSurface, UpdateComponents, UpdateDataModel, DeleteSurface, CallRendererFunction, AgentFunctionResponse)")
	// ErrAmbiguousPayload reports a Message with more than one payload set.
	ErrAmbiguousPayload = errors.New("a2ui: message has more than one payload (exactly one is required)")
	// ErrNotAMessageEnvelope reports a JSON object that is not a server→client
	// message envelope (no single recognized payload key).
	ErrNotAMessageEnvelope = errors.New("a2ui: not an A2UI message envelope")
)

// Message is one server→client A2UI message — the ServerToClientMessage union
// of _types.py. Exactly one payload field must be set; MarshalJSON emits the
// single-key envelope {"version": "v1.0", "<payload>": {...}} and
// UnmarshalJSON enforces the same shape on input.
//
// A Message is safe for concurrent use as long as its payload values are:
// the struct is a plain value type.
type Message struct {
	Version string // always VersionV1 in this port

	CreateSurface         *CreateSurface
	UpdateComponents      *UpdateComponents
	UpdateDataModel       *UpdateDataModel
	DeleteSurface         *DeleteSurface
	CallRendererFunction  *CallRendererFunction
	AgentFunctionResponse *FunctionResponse
}

// PayloadKey returns the message's payload key, or "" when none is set.
func (m Message) PayloadKey() string {
	switch {
	case m.CreateSurface != nil:
		return "createSurface"
	case m.UpdateComponents != nil:
		return "updateComponents"
	case m.UpdateDataModel != nil:
		return "updateDataModel"
	case m.DeleteSurface != nil:
		return "deleteSurface"
	case m.CallRendererFunction != nil:
		return "callRendererFunction"
	case m.AgentFunctionResponse != nil:
		return "agentFunctionResponse"
	default:
		return ""
	}
}

// jsonQuote encodes s as a JSON string without Go's HTML escaping (Python's
// json.dumps does not escape <, > or &). A string never fails to encode.
func jsonQuote(s string) string {
	b, _ := marshalNoHTMLEscape(s)
	return string(b)
}

func (m Message) MarshalJSON() ([]byte, error) {
	key := m.PayloadKey()
	if key == "" {
		return nil, ErrNoPayload
	}
	set := 0
	for _, present := range []bool{
		m.CreateSurface != nil, m.UpdateComponents != nil, m.UpdateDataModel != nil,
		m.DeleteSurface != nil, m.CallRendererFunction != nil, m.AgentFunctionResponse != nil,
	} {
		if present {
			set++
		}
	}
	if set > 1 {
		return nil, ErrAmbiguousPayload
	}
	var payload []byte
	var err error
	switch {
	case m.CreateSurface != nil:
		payload, err = json.Marshal(m.CreateSurface)
	case m.UpdateComponents != nil:
		payload, err = json.Marshal(m.UpdateComponents)
	case m.UpdateDataModel != nil:
		payload, err = json.Marshal(m.UpdateDataModel)
	case m.DeleteSurface != nil:
		payload, err = json.Marshal(m.DeleteSurface)
	case m.CallRendererFunction != nil:
		payload, err = json.Marshal(m.CallRendererFunction)
	default:
		payload, err = json.Marshal(m.AgentFunctionResponse)
	}
	if err != nil {
		return nil, err
	}
	version := m.Version
	if version == "" {
		version = VersionV1
	}
	var b bytes.Buffer
	b.WriteString(`{"version":`)
	b.WriteString(jsonQuote(version))
	b.WriteString(`,"`)
	b.WriteString(key)
	b.WriteString(`":`)
	b.Write(payload)
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	found := ""
	for _, k := range payloadKeys {
		if _, ok := envelope[k]; ok {
			if found != "" {
				return fmt.Errorf("%w: %q and %q both present", ErrNotAMessageEnvelope, found, k)
			}
			found = k
		}
	}
	if found == "" {
		return fmt.Errorf("%w: expected exactly one of %s", ErrNotAMessageEnvelope, pyStringList(payloadKeys[:]))
	}
	version := ""
	if raw, ok := envelope["version"]; ok {
		if err := json.Unmarshal(raw, &version); err != nil {
			return fmt.Errorf("a2ui: message version: %w", err)
		}
	}
	m.Version = version
	m.CreateSurface, m.UpdateComponents, m.UpdateDataModel, m.DeleteSurface, m.CallRendererFunction, m.AgentFunctionResponse = nil, nil, nil, nil, nil, nil
	switch found {
	case "createSurface":
		m.CreateSurface = new(CreateSurface)
		return json.Unmarshal(envelope[found], m.CreateSurface)
	case "updateComponents":
		m.UpdateComponents = new(UpdateComponents)
		return json.Unmarshal(envelope[found], m.UpdateComponents)
	case "updateDataModel":
		m.UpdateDataModel = new(UpdateDataModel)
		return json.Unmarshal(envelope[found], m.UpdateDataModel)
	case "deleteSurface":
		m.DeleteSurface = new(DeleteSurface)
		return json.Unmarshal(envelope[found], m.DeleteSurface)
	case "callRendererFunction":
		m.CallRendererFunction = new(CallRendererFunction)
		return json.Unmarshal(envelope[found], m.CallRendererFunction)
	default:
		m.AgentFunctionResponse = new(FunctionResponse)
		return json.Unmarshal(envelope[found], m.AgentFunctionResponse)
	}
}

// CreateSurface is the createSurface payload: it can carry a whole UI in one
// message (components + dataModel are v1.0-only).
type CreateSurface struct {
	SurfaceID     string         `json:"surfaceId"`
	CatalogID     string         `json:"catalogId,omitempty"`
	SendDataModel bool           `json:"sendDataModel,omitempty"`
	Components    []Component    `json:"components,omitempty"`
	DataModel     map[string]any `json:"dataModel,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// UpdateComponents is the updateComponents payload.
type UpdateComponents struct {
	SurfaceID  string      `json:"surfaceId"`
	Components []Component `json:"components"`
}

// UpdateDataModel is the updateDataModel payload. Value is always emitted
// (v1.0 requires it; JSON null deletes the value at Path).
type UpdateDataModel struct {
	SurfaceID string `json:"surfaceId"`
	Path      string `json:"path,omitempty"`
	Value     any    `json:"value"`
}

// DeleteSurface is the deleteSurface payload.
type DeleteSurface struct {
	SurfaceID string `json:"surfaceId"`
}

// CallRendererFunction is the v1.0 callRendererFunction payload: the agent
// asks the renderer to run a catalog function; the renderer answers with a
// rendererFunctionResponse copying FunctionCallID verbatim.
type CallRendererFunction struct {
	SurfaceID      string       `json:"surfaceId,omitempty"`
	FunctionCallID string       `json:"functionCallId"`
	CallFunction   FunctionCall `json:"callFunction"`
}

// FunctionCall is a common_types.json#/$defs/FunctionCall: the named function,
// its catalog, and its arguments.
type FunctionCall struct {
	Call      string         `json:"@call"`
	CatalogID string         `json:"catalogId,omitempty"`
	Args      map[string]any `json:"args,omitempty"`
}

// FunctionResponse is the FunctionResponse body shared by
// agentFunctionResponse (server→client) and rendererFunctionResponse
// (client→server): FunctionCallID plus exactly one of Value or Error.
// MarshalJSON emits "error" when Error is set, else "value" (even null).
type FunctionResponse struct {
	FunctionCallID string         `json:"functionCallId"`
	Value          any            `json:"-"`
	Error          *FunctionError `json:"-"`
}

func (f FunctionResponse) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"functionCallId":`)
	b.WriteString(jsonQuote(f.FunctionCallID))
	if f.Error != nil {
		b.WriteString(`,"error":{"code":`)
		b.WriteString(jsonQuote(f.Error.Code))
		b.WriteString(`,"message":`)
		b.WriteString(jsonQuote(f.Error.Message))
		b.WriteString("}}")
	} else {
		value, err := marshalNoHTMLEscape(f.Value)
		if err != nil {
			return nil, err
		}
		b.WriteString(`,"value":`)
		b.Write(value)
		b.WriteByte('}')
	}
	return b.Bytes(), nil
}

func (f *FunctionResponse) UnmarshalJSON(data []byte) error {
	var raw struct {
		FunctionCallID string          `json:"functionCallId"`
		Value          json.RawMessage `json:"value"`
		Error          *FunctionError  `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	f.FunctionCallID = raw.FunctionCallID
	f.Error = raw.Error
	if len(raw.Value) > 0 {
		var v any
		if err := json.Unmarshal(raw.Value, &v); err != nil {
			return err
		}
		f.Value = v
	}
	return nil
}

// FunctionError is the error arm of a FunctionResponse.
type FunctionError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// marshalNoHTMLEscape marshals v without Go's HTML escaping (Python's
// json.dumps does not escape <, > or &).
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// pyStringList renders a string slice as a Python list repr
// (['a', 'b']) — the format the parser's envelope error pins.
func pyStringList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = "'" + s + "'"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Component is one flat A2UI component: the ComponentCommon envelope (id,
// catalogId, accessibility, metadata) plus the catalog definition's own
// properties, kept flat on the wire exactly as the v1.0 catalog declares them
// ({"id": "text1", "component": "Text", "text": "Hello"}). Properties outside
// the envelope live in Props (text, src/url, action, variant, ...).
type Component struct {
	ID            string         // wire "id"
	CatalogID     string         // wire "catalogId", omitted when empty
	Kind          string         // wire "component": the catalog component name
	Accessibility map[string]any // wire "accessibility"
	Metadata      map[string]any // wire "metadata"
	Child         string         // wire "child" (wrapper components)
	Children      ChildrenRef    // wire "children" (layout components), omitted when empty
	Props         map[string]any // every other property, emitted sorted
}

// ListTemplate is the dynamic form of "children": generate a list of children
// from a data-model list using componentId as the template.
type ListTemplate struct {
	ComponentID string `json:"componentId"`
	Path        string `json:"path"`
}

// ChildrenRef holds the two wire forms of "children": a static list of
// component ids, or a ListTemplate object.
type ChildrenRef struct {
	IDs      []string
	Template *ListTemplate
}

func (c ChildrenRef) MarshalJSON() ([]byte, error) {
	if c.Template != nil {
		return marshalNoHTMLEscape(c.Template)
	}
	return marshalNoHTMLEscape(c.IDs)
}

func (c *ChildrenRef) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		c.Template = nil
		return json.Unmarshal(data, &c.IDs)
	}
	c.IDs = nil
	c.Template = new(ListTemplate)
	return json.Unmarshal(data, c.Template)
}

func (c Component) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	b.WriteString(jsonQuote(c.ID))
	b.WriteString(`,"component":`)
	b.WriteString(jsonQuote(c.Kind))
	if c.CatalogID != "" {
		b.WriteString(`,"catalogId":`)
		b.WriteString(jsonQuote(c.CatalogID))
	}
	if c.Accessibility != nil {
		b.WriteString(`,"accessibility":`)
		acc, err := marshalNoHTMLEscape(c.Accessibility)
		if err != nil {
			return nil, err
		}
		b.Write(acc)
	}
	if c.Metadata != nil {
		b.WriteString(`,"metadata":`)
		md, err := marshalNoHTMLEscape(c.Metadata)
		if err != nil {
			return nil, err
		}
		b.Write(md)
	}
	if c.Child != "" {
		b.WriteString(`,"child":`)
		b.WriteString(jsonQuote(c.Child))
	}
	if c.Children.IDs != nil || c.Children.Template != nil {
		b.WriteString(`,"children":`)
		ch, err := json.Marshal(c.Children)
		if err != nil {
			return nil, err
		}
		b.Write(ch)
	}
	for _, k := range sortedKeys(c.Props) {
		b.WriteString(",")
		b.WriteString(jsonQuote(k))
		b.WriteString(":")
		v, err := marshalNoHTMLEscape(c.Props[k])
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (c *Component) UnmarshalJSON(data []byte) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	c.Props = map[string]any{}
	for k, raw := range envelope {
		switch k {
		case "id":
			if err := json.Unmarshal(raw, &c.ID); err != nil {
				return err
			}
		case "catalogId":
			if err := json.Unmarshal(raw, &c.CatalogID); err != nil {
				return err
			}
		case "component":
			if err := json.Unmarshal(raw, &c.Kind); err != nil {
				return err
			}
		case "accessibility":
			if err := json.Unmarshal(raw, &c.Accessibility); err != nil {
				return err
			}
		case "metadata":
			if err := json.Unmarshal(raw, &c.Metadata); err != nil {
				return err
			}
		case "child":
			if err := json.Unmarshal(raw, &c.Child); err != nil {
				return err
			}
		case "children":
			if err := json.Unmarshal(raw, &c.Children); err != nil {
				return err
			}
		default:
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				return err
			}
			c.Props[k] = v
		}
	}
	if c.ID == "" {
		return fmt.Errorf(`a2ui: component is missing "id"`)
	}
	if c.Kind == "" {
		return fmt.Errorf(`a2ui: component %q is missing "component" (the catalog component name)`, c.ID)
	}
	return nil
}

// NewText builds a Text component.
func NewText(id, text string) Component {
	return Component{ID: id, Kind: "Text", Props: map[string]any{"text": text}}
}

// NewImage builds an Image component (v1.0: the property is "url").
func NewImage(id, url string) Component {
	return Component{ID: id, Kind: "Image", Props: map[string]any{"url": url}}
}
