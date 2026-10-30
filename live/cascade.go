// Package live is the voice cascade: speech-to-text, one LLM turn and
// text-to-speech chained over plain HTTP (Python parity: ag2/live/cascade.py —
// the STT→LLM→TTS slice of CascadeConfig, per the porting brief; the
// realtime-websocket half of the Python live stack is deferred, D-057).
package live

import (
	stdcontext "context"
	"errors"
	"fmt"
	"strings"
)

// Transcriber turns one audio clip into text.
type Transcriber interface {
	Transcribe(ctx stdcontext.Context, audio []byte, mime string) (string, error)
}

// Synthesizer renders text as speech, reporting the media type of the produced
// audio bytes.
type Synthesizer interface {
	Synthesize(ctx stdcontext.Context, text string, voice string) (audio []byte, mime string, err error)
}

// ErrIncomplete reports a Cascade built without one of its stages. Match with
// errors.Is.
var ErrIncomplete = errors.New("live: cascade needs STT, LLM and TTS stages")

// Cascade wires one STT, one LLM turn and one TTS into a single spoken-turn
// handler (Python parity: CascadeConfig's stt/model/tts trio, simplified to a
// plain function chain — no LiveAgent session, no turn detector, no tool loop).
// The LLM stage is a plain function so any driver works: an agent reply body,
// a config client call, or a test stub. Zero-value fields are rejected by
// Handle with ErrIncomplete. Safe for concurrent use when its stages are.
type Cascade struct {
	STT Transcriber
	// LLM answers one transcribed utterance.
	LLM func(ctx stdcontext.Context, text string) (string, error)
	TTS Synthesizer
}

// Handle runs one spoken turn: transcribe the clip, answer it, synthesize the
// answer in the requested voice. An empty (whitespace-only) transcript ends the
// turn silently — empty outputs, nil error — because there was nothing worth
// answering (Python parity: cascade.py `_turn`: `if not text.strip(): return`).
func (c *Cascade) Handle(ctx stdcontext.Context, audio []byte, mime, voice string) (replyText string, audioOut []byte, mimeOut string, err error) {
	if c == nil || c.STT == nil || c.LLM == nil || c.TTS == nil {
		return "", nil, "", ErrIncomplete
	}
	text, err := c.STT.Transcribe(ctx, audio, mime)
	if err != nil {
		return "", nil, "", fmt.Errorf("live: transcribe: %w", err)
	}
	if strings.TrimSpace(text) == "" {
		return "", nil, "", nil
	}
	reply, err := c.LLM(ctx, text)
	if err != nil {
		return "", nil, "", fmt.Errorf("live: llm: %w", err)
	}
	audioOut, mimeOut, err = c.TTS.Synthesize(ctx, reply, voice)
	if err != nil {
		return "", nil, "", fmt.Errorf("live: synthesize: %w", err)
	}
	return reply, audioOut, mimeOut, nil
}
