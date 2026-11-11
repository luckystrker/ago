package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/events"
)

// mapContent converts MCP tools/call content blocks into typed Input parts
// (parity with Python _extract_content, reduced to the brief's mapping):
//
//	text          → TextInput
//	image         → BinaryInput (base64-decoded, kind image)
//	audio         → BinaryInput (base64-decoded, kind audio)
//	resource_link → UrlInput (kind from the link's media type)
//	resource      → TextInput (text contents) or BinaryInput (base64 blob,
//	                kind from the media type); a resource carrying neither is
//	                kept as its JSON, never dropped
//	anything else → TextInput carrying the block's JSON (future variants are
//	                preserved, never dropped)
//
// A result without content blocks collapses to one TextInput carrying the whole
// result object as compact JSON (Python parity: model_dump_json of the result).
func mapContent(blocks []map[string]any, raw json.RawMessage) ([]events.Input, error) {
	if len(blocks) == 0 {
		return []events.Input{&events.TextInput{Content: compactJSON(raw)}}, nil
	}
	parts := make([]events.Input, 0, len(blocks))
	for _, block := range blocks {
		text, _ := block["type"].(string)
		switch text {
		case "text":
			content, _ := block["text"].(string)
			parts = append(parts, &events.TextInput{Content: content})
		case "image", "audio":
			kind := events.BinaryImage
			if text == "audio" {
				kind = events.BinaryAudio
			}
			part, err := binaryPart(block, kind)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case "resource_link":
			uri, _ := block["uri"].(string)
			mimeType, _ := block["mimeType"].(string)
			parts = append(parts, &events.UrlInput{URL: uri, Kind: kindFromMime(mimeType)})
		case "resource":
			part, err := resourcePart(block)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		default:
			// Any future block variant: JSON text.
			b, err := json.Marshal(block)
			if err != nil {
				return nil, fmt.Errorf("%w: encode %q content block: %v", ErrProtocol, text, err)
			}
			parts = append(parts, &events.TextInput{Content: string(b)})
		}
	}
	return parts, nil
}

// resourcePart maps an embedded resource block: text contents become a
// TextInput, a base64 blob a BinaryInput typed by its media type (Python
// parity: _extract_content). A resource with neither payload is preserved as
// the block's JSON.
func resourcePart(block map[string]any) (events.Input, error) {
	res, _ := block["resource"].(map[string]any)
	if text, ok := res["text"].(string); ok {
		return &events.TextInput{Content: text}, nil
	}
	if blob, ok := res["blob"].(string); ok {
		data, err := base64.StdEncoding.DecodeString(blob)
		if err != nil {
			return nil, fmt.Errorf("%w: resource content: %v", ErrProtocol, err)
		}
		mediaType, _ := res["mimeType"].(string)
		kind := kindFromMime(mediaType)
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return &events.BinaryInput{Data: data, MediaType: mediaType, Kind: kind}, nil
	}
	b, err := json.Marshal(block)
	if err != nil {
		return nil, fmt.Errorf("%w: encode \"resource\" content block: %v", ErrProtocol, err)
	}
	return &events.TextInput{Content: string(b)}, nil
}

// binaryPart decodes one base64 media block (image/audio).
func binaryPart(block map[string]any, kind events.BinaryType) (events.Input, error) {
	encoded, _ := block["data"].(string)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: %s content: %v", ErrProtocol, kind, err)
	}
	mediaType, _ := block["mimeType"].(string)
	return &events.BinaryInput{Data: data, MediaType: mediaType, Kind: kind}, nil
}

// documentMimes are the media types Python's DocumentMediaType union names; a
// resource link or blob carrying one is a document, not an opaque binary.
var documentMimes = map[string]bool{
	"application/pdf": true,
	// text and code
	"text/plain": true, "text/csv": true, "text/tsv": true, "text/html": true,
	"text/markdown": true, "text/xml": true, "text/css": true,
	"text/javascript": true, "text/calendar": true, "text/vcard": true,
	"text/srt": true, "text/vtt": true,
	"application/json": true, "application/sql": true, "message/rfc822": true,
	// rich documents
	"application/msword": true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true,
	"application/rtf":                         true,
	"application/vnd.oasis.opendocument.text": true,
	// spreadsheets
	"application/vnd.ms-excel": true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": true,
	// presentations
	"application/vnd.ms-powerpoint":                                             true,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
}

// kindFromMime classifies a media type into a BinaryType (Python parity:
// _kind_from_mime over the media-type unions). Empty or unknown → binary.
func kindFromMime(mimeType string) events.BinaryType {
	switch {
	case mimeType == "":
		return events.BinaryBinary
	case strings.HasPrefix(mimeType, "image/"):
		return events.BinaryImage
	case strings.HasPrefix(mimeType, "audio/"):
		return events.BinaryAudio
	case strings.HasPrefix(mimeType, "video/"):
		return events.BinaryVideo
	case documentMimes[mimeType]:
		return events.BinaryDocument
	default:
		return events.BinaryBinary
	}
}

// errorText renders the human-facing message of a failed call: the text blocks
// joined, or the raw result JSON when the server sent no text.
func errorText(blocks []map[string]any, raw json.RawMessage) string {
	var texts []string
	for _, block := range blocks {
		if t, ok := block["type"].(string); ok && t == "text" {
			if s, ok := block["text"].(string); ok && s != "" {
				texts = append(texts, s)
			}
		}
	}
	if len(texts) == 0 {
		return compactJSON(raw)
	}
	return strings.Join(texts, "\n")
}

// compactJSON renders a raw message without insignificant whitespace.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
