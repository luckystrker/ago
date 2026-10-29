// Package transport carries the wire vocabulary of the network layer: the
// Frame envelope and the Link abstraction between a client and a hub.
//
// The package is a pure leaf — it interprets no payloads. Semantics
// (envelopes, channels, handshakes) live in the parent network package; a
// wire transport embeds them as opaque bytes inside frames.
package transport

import (
	"encoding/json"
	"fmt"
)

// Frame kinds. The vocabulary is fixed: handshake (Hello/Welcome), control
// plane (Request/Response, correlated by RequestID), data plane
// (Notify/Receipt) and heartbeat (Ping/Pong).
const (
	FrameHello    = "hello"
	FrameWelcome  = "welcome"
	FrameRequest  = "request"
	FrameResponse = "response"
	FrameNotify   = "notify"
	FrameReceipt  = "receipt"
	FramePing     = "ping"
	FramePong     = "pong"
)

// Frame is one wire unit between a client and a hub.
//
// Payload is opaque to this package — senders encode their bodies (envelope
// JSON, RPC params, ...) as bytes and the receiver decodes them at its own
// layer. The address fields make delivery routing possible without opening
// the payload: To/From name the link endpoints, RecipientID names the
// registered identity a Notify is for (one link may host several).
type Frame struct {
	Kind        string `json:"kind"`
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	RequestID   string `json:"request_id,omitempty"` // correlates Request ↔ Response
	RecipientID string `json:"recipient_id,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	Code        string `json:"code,omitempty"` // stable error code (Response failure, handshake rejection)
	Message     string `json:"message,omitempty"`
	Payload     []byte `json:"payload,omitempty"`
}

// ErrUnknownKind is returned by UnmarshalFrame for a kind outside the
// vocabulary.
var ErrUnknownKind = fmt.Errorf("transport: unknown frame kind")

// validKinds is the closed set of frame kinds.
var validKinds = map[string]bool{
	FrameHello:    true,
	FrameWelcome:  true,
	FrameRequest:  true,
	FrameResponse: true,
	FrameNotify:   true,
	FrameReceipt:  true,
	FramePing:     true,
	FramePong:     true,
}

// MarshalFrame serialises a frame to JSON. The payload travels base64-encoded
// (standard []byte JSON encoding) — opaque bytes, losslessly round-tripped.
func MarshalFrame(f Frame) ([]byte, error) {
	if !validKinds[f.Kind] {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, f.Kind)
	}
	return json.Marshal(f)
}

// UnmarshalFrame reconstructs a frame from JSON produced by MarshalFrame.
func UnmarshalFrame(data []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return Frame{}, err
	}
	if !validKinds[f.Kind] {
		return Frame{}, fmt.Errorf("%w: %q", ErrUnknownKind, f.Kind)
	}
	return f, nil
}
