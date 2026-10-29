package events

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BinaryType classifies binary inputs and results.
type BinaryType string

const (
	BinaryBinary   BinaryType = "binary"
	BinaryImage    BinaryType = "image"
	BinaryAudio    BinaryType = "audio"
	BinaryDocument BinaryType = "document"
	BinaryVideo    BinaryType = "video"
)

// Input is a user-supplied payload: one part of a ModelRequest or of a ToolResult.
type Input interface {
	Event
	inputMarker()
}

// --- concrete inputs -------------------------------------------------------

// TextInput is plain text content.
type TextInput struct {
	Base
	Content string `json:"content"`
}

func (TextInput) EventType() string    { return "TextInput" }
func (TextInput) inputMarker()         {}
func (TextInput) Transient() bool      { return false }
func (TextInput) Conversational() bool { return true }

// DataInput carries an arbitrary JSON-decodable value.
type DataInput struct {
	Base
	Data any `json:"data"`
}

func (DataInput) EventType() string    { return "DataInput" }
func (DataInput) inputMarker()         {}
func (DataInput) Transient() bool      { return false }
func (DataInput) Conversational() bool { return true }

// BinaryInput carries raw bytes with a media type.
type BinaryInput struct {
	Base
	Data           []byte         `json:"data"`
	MediaType      string         `json:"media_type"`
	Kind           BinaryType     `json:"kind"`
	VendorMetadata map[string]any `json:"vendor_metadata,omitempty"`
}

func (BinaryInput) EventType() string    { return "BinaryInput" }
func (BinaryInput) inputMarker()         {}
func (BinaryInput) Transient() bool      { return false }
func (BinaryInput) Conversational() bool { return true }

// UrlInput references binary content by URL.
type UrlInput struct {
	Base
	URL  string     `json:"url"`
	Kind BinaryType `json:"kind"`
}

func (UrlInput) EventType() string    { return "UrlInput" }
func (UrlInput) inputMarker()         {}
func (UrlInput) Transient() bool      { return false }
func (UrlInput) Conversational() bool { return true }

// FileIdInput references a file previously uploaded through a provider Files API.
type FileIdInput struct {
	Base
	FileID   string `json:"file_id"`
	Filename string `json:"filename,omitempty"`
}

func (FileIdInput) EventType() string    { return "FileIdInput" }
func (FileIdInput) inputMarker()         {}
func (FileIdInput) Transient() bool      { return false }
func (FileIdInput) Conversational() bool { return true }

// --- request events --------------------------------------------------------

// ModelRequest is the user turn: a batch of input parts sent to the model.
type ModelRequest struct {
	Base
	Parts    []Input        `json:"parts"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func (ModelRequest) EventType() string    { return "ModelRequest" }
func (ModelRequest) Transient() bool      { return false }
func (ModelRequest) Conversational() bool { return true }

// DrainedModelRequest marks an inbox drain: a merged ModelRequest re-emitted after its
// parts were consumed. The LLM-trigger subscriber checks for it to avoid a recursive
// model call (Python parity: same subclass check in agent._call_client).
type DrainedModelRequest struct {
	ModelRequest
}

func (DrainedModelRequest) EventType() string { return "DrainedModelRequest" }

// MessageEnqueued announces an inbox append (transient bookkeeping).
type MessageEnqueued struct {
	Base
}

func (MessageEnqueued) EventType() string    { return "MessageEnqueued" }
func (MessageEnqueued) Transient() bool      { return true }
func (MessageEnqueued) Conversational() bool { return false }

// --- marshaling for structs holding Input parts -----------------------------

type wireParts struct {
	Parts    []json.RawMessage `json:"parts"`
	Metadata map[string]any    `json:"metadata"`
	Base
}

func marshalParts(p []Input) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(p))
	for _, part := range p {
		b, err := MarshalEvent(part)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func (m ModelRequest) MarshalJSON() ([]byte, error) {
	parts, err := marshalParts(m.Parts)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Type     string            `json:"@type"`
		Parts    []json.RawMessage `json:"parts"`
		Metadata map[string]any    `json:"metadata,omitempty"`
		Base
	}{"ModelRequest", parts, m.Metadata, m.Base})
}

func (m *ModelRequest) UnmarshalJSON(data []byte) error {
	var w wireParts
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	parts, err := unmarshalParts(w.Parts)
	if err != nil {
		return err
	}
	m.Base, m.Parts, m.Metadata = w.Base, parts, w.Metadata
	return nil
}

func (d DrainedModelRequest) MarshalJSON() ([]byte, error) {
	parts, err := marshalParts(d.Parts)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Type     string            `json:"@type"`
		Parts    []json.RawMessage `json:"parts"`
		Metadata map[string]any    `json:"metadata,omitempty"`
		Base
	}{"DrainedModelRequest", parts, d.Metadata, d.Base})
}

func (d *DrainedModelRequest) UnmarshalJSON(data []byte) error {
	var w wireParts
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	parts, err := unmarshalParts(w.Parts)
	if err != nil {
		return err
	}
	d.Base, d.Parts, d.Metadata = w.Base, parts, w.Metadata
	return nil
}

func unmarshalParts(raw []json.RawMessage) ([]Input, error) {
	parts := make([]Input, 0, len(raw))
	for _, p := range raw {
		in, err := UnmarshalInput(p)
		if err != nil {
			return nil, err
		}
		parts = append(parts, in)
	}
	return parts, nil
}

// --- input helpers ----------------------------------------------------------

// EnsureInput wraps raw content as an Input: string → *TextInput, Input passes through,
// anything else → *DataInput.
func EnsureInput(content any) Input {
	switch v := content.(type) {
	case nil:
		return &DataInput{}
	case Input:
		return v
	case string:
		return &TextInput{Content: v}
	default:
		return &DataInput{Data: v}
	}
}

// EnsureRequest wraps raw message contents as a single ModelRequest.
func EnsureRequest(msgs ...any) *ModelRequest {
	req := &ModelRequest{Parts: make([]Input, 0, len(msgs))}
	for _, m := range msgs {
		req.Parts = append(req.Parts, EnsureInput(m))
	}
	return req
}

// --- media factories (Python ImageInput/DocumentInput/AudioInput/VideoInput) --

var mediaTypeByExt = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg", ".flac": "audio/flac",
	".m4a": "audio/mp4", ".aac": "audio/aac",
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime", ".avi": "video/x-msvideo",
	".pdf": "application/pdf", ".txt": "text/plain", ".md": "text/markdown", ".csv": "text/csv",
	".html": "text/html", ".json": "application/json", ".xml": "application/xml",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// MediaTypeForPath infers a media type from a file extension ("" if unknown).
func MediaTypeForPath(path string) string {
	return mediaTypeByExt[strings.ToLower(filepath.Ext(path))]
}

type mediaSpec struct {
	url       string
	fileID    string
	path      string
	data      []byte
	mediaType string
	filename  string
}

func buildBinary(kind BinaryType, spec mediaSpec) (Input, error) {
	sources := 0
	for _, set := range []bool{spec.url != "", spec.fileID != "", spec.path != "", spec.data != nil} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return nil, fmt.Errorf("events: %s input requires exactly one of url, file_id, path or data", kind)
	}
	switch {
	case spec.url != "":
		return &UrlInput{URL: spec.url, Kind: kind}, nil
	case spec.fileID != "":
		return &FileIdInput{FileID: spec.fileID, Filename: spec.filename}, nil
	case spec.path != "":
		mt := spec.mediaType
		if mt == "" {
			mt = MediaTypeForPath(spec.path)
		}
		if mt == "" {
			return nil, fmt.Errorf("events: cannot infer %s media type from extension '%s'. Provide media_type explicitly",
				kind, strings.ToLower(filepath.Ext(spec.path)))
		}
		// The bytes travel with the request: provider APIs take URLs or uploaded
		// ids, not local paths (Python parity: input_events.py p.read_bytes()).
		b, err := os.ReadFile(spec.path)
		if err != nil {
			return nil, fmt.Errorf("events: %s input: %w", kind, err)
		}
		return &BinaryInput{
			Data:           b,
			MediaType:      mt,
			Kind:           kind,
			VendorMetadata: map[string]any{"filename": filepath.Base(spec.path)},
		}, nil
	default:
		mt := spec.mediaType
		if mt == "" {
			return nil, fmt.Errorf("events: %s input from data requires a media type", kind)
		}
		return &BinaryInput{Data: spec.data, MediaType: mt, Kind: kind}, nil
	}
}

// The media factories take exactly one source: url, fileID, path or data (with a
// mandatory mediaType). A path is read eagerly; its base name rides in
// VendorMetadata["filename"], and an unknown extension fails — pass mediaType
// explicitly. A named file_id has no positional slot: build FileIdInput directly.

func ImageInput(url, path string, data []byte, mediaType, fileID string) (Input, error) {
	return buildBinary(BinaryImage, mediaSpec{url: url, path: path, data: data, mediaType: mediaType, fileID: fileID})
}

func DocumentInput(url, path string, data []byte, mediaType, fileID string) (Input, error) {
	return buildBinary(BinaryDocument, mediaSpec{url: url, path: path, data: data, mediaType: mediaType, fileID: fileID})
}

func AudioInput(url, path string, data []byte, mediaType, fileID string) (Input, error) {
	return buildBinary(BinaryAudio, mediaSpec{url: url, path: path, data: data, mediaType: mediaType, fileID: fileID})
}

func VideoInput(url, path string, data []byte, mediaType, fileID string) (Input, error) {
	return buildBinary(BinaryVideo, mediaSpec{url: url, path: path, data: data, mediaType: mediaType, fileID: fileID})
}

func init() {
	RegisterEvent(TextInput{})
	RegisterEvent(DataInput{})
	RegisterEvent(BinaryInput{})
	RegisterEvent(UrlInput{})
	RegisterEvent(FileIdInput{})
	RegisterEvent(ModelRequest{})
	RegisterEvent(DrainedModelRequest{})
	RegisterEvent(MessageEnqueued{})
}
