package live

import (
	stdcontext "context"
	"errors"
	"strings"
	"testing"
)

// fakeSTT is a scripted Transcriber that records what it was handed.
type fakeSTT struct {
	text      string
	err       error
	gotAudio  []byte
	gotMime   string
	afterText string
}

func (f *fakeSTT) Transcribe(_ stdcontext.Context, audio []byte, mime string) (string, error) {
	f.gotAudio = audio
	f.gotMime = mime
	if f.err != nil {
		return "", f.err
	}
	return f.text, nil
}

// fakeTTS is a scripted Synthesizer that records what it was handed.
type fakeTTS struct {
	audio    []byte
	mime     string
	err      error
	gotText  string
	gotVoice string
}

func (f *fakeTTS) Synthesize(_ stdcontext.Context, text, voice string) ([]byte, string, error) {
	f.gotText = text
	f.gotVoice = voice
	if f.err != nil {
		return nil, "", f.err
	}
	return f.audio, f.mime, nil
}

// countingLLM is an LLM stage that records its calls and answers.
type countingLLM struct {
	calls  int
	answer func(text string) string
}

func (c *countingLLM) call(_ stdcontext.Context, text string) (string, error) {
	c.calls++
	return c.answer(text), nil
}

// TestCascadeHandleHappyPath: one spoken turn flows STT→LLM→TTS with every
// stage seeing its inputs.
func TestCascadeHandleHappyPath(t *testing.T) {
	stt := &fakeSTT{text: "hello there"}
	tts := &fakeTTS{audio: []byte("AU"), mime: "audio/mpeg"}
	llm := &countingLLM{answer: strings.ToUpper}
	c := &Cascade{STT: stt, LLM: llm.call, TTS: tts}

	reply, audioOut, mimeOut, err := c.Handle(stdcontext.Background(), []byte("clip"), "audio/wav", "alloy")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if reply != "HELLO THERE" {
		t.Errorf("reply = %q, want %q", reply, "HELLO THERE")
	}
	if string(audioOut) != "AU" || mimeOut != "audio/mpeg" {
		t.Errorf("audio out = %q/%q, want %q/%q", audioOut, mimeOut, "AU", "audio/mpeg")
	}
	if string(stt.gotAudio) != "clip" || stt.gotMime != "audio/wav" {
		t.Errorf("stt saw %q/%q, want %q/%q", stt.gotAudio, stt.gotMime, "clip", "audio/wav")
	}
	if tts.gotText != "HELLO THERE" || tts.gotVoice != "alloy" {
		t.Errorf("tts saw text=%q voice=%q, want %q/%q", tts.gotText, tts.gotVoice, "HELLO THERE", "alloy")
	}
}

// TestCascadeSilenceEndsTurnSilently: a whitespace-only transcript ends the
// turn with empty outputs and nil error, without calling the LLM or TTS
// (Python parity: cascade.py `_turn`).
func TestCascadeSilenceEndsTurnSilently(t *testing.T) {
	stt := &fakeSTT{text: "   \n"}
	tts := &fakeTTS{}
	llm := &countingLLM{answer: func(string) string { return "should not happen" }}
	c := &Cascade{STT: stt, LLM: llm.call, TTS: tts}

	reply, audioOut, mimeOut, err := c.Handle(stdcontext.Background(), []byte("clip"), "audio/wav", "alloy")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if reply != "" || audioOut != nil || mimeOut != "" {
		t.Errorf("outputs = %q/%v/%q, want empty", reply, audioOut, mimeOut)
	}
	if llm.calls != 0 || tts.gotText != "" {
		t.Errorf("silence reached the downstream stages (llm calls=%d, tts text=%q)", llm.calls, tts.gotText)
	}
}

// TestCascadeTranscribeErrorAbortsTurn: a transcription failure fails the turn
// wrapped with the live: transcribe context, and nothing downstream runs.
func TestCascadeTranscribeErrorAbortsTurn(t *testing.T) {
	boom := errors.New("transcriber offline")
	stt := &fakeSTT{err: boom}
	tts := &fakeTTS{}
	llm := &countingLLM{}
	c := &Cascade{STT: stt, LLM: llm.call, TTS: tts}

	_, _, _, err := c.Handle(stdcontext.Background(), nil, "audio/wav", "alloy")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the transcriber error", err)
	}
	if !strings.Contains(err.Error(), "live: transcribe") {
		t.Errorf("err = %v, want the live: transcribe context", err)
	}
	if llm.calls != 0 {
		t.Errorf("llm ran %d times after a transcription failure", llm.calls)
	}
}

// TestCascadeLLMError and TestCascadeSynthesizeError: downstream failures fail
// the turn wrapped with their stage context.
func TestCascadeLLMError(t *testing.T) {
	boom := errors.New("model down")
	c := &Cascade{STT: &fakeSTT{text: "hi"}, LLM: func(stdcontext.Context, string) (string, error) { return "", boom }, TTS: &fakeTTS{}}
	_, _, _, err := c.Handle(stdcontext.Background(), nil, "audio/wav", "alloy")
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "live: llm") {
		t.Fatalf("err = %v, want the wrapped live: llm error", err)
	}
}

func TestCascadeSynthesizeError(t *testing.T) {
	boom := errors.New("speaker offline")
	c := &Cascade{
		STT: &fakeSTT{text: "hi"},
		LLM: func(_ stdcontext.Context, text string) (string, error) { return text, nil },
		TTS: &fakeTTS{err: boom},
	}
	_, _, _, err := c.Handle(stdcontext.Background(), nil, "audio/wav", "alloy")
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "live: synthesize") {
		t.Fatalf("err = %v, want the wrapped live: synthesize error", err)
	}
}

// TestCascadeMissingStage: an incomplete cascade is rejected with ErrIncomplete.
func TestCascadeMissingStage(t *testing.T) {
	llm := func(stdcontext.Context, string) (string, error) { return "", nil }
	for name, c := range map[string]*Cascade{
		"empty":      {},
		"no llm":     {STT: &fakeSTT{}, TTS: &fakeTTS{}},
		"no tts":     {STT: &fakeSTT{}, LLM: llm},
		"no stt":     {LLM: llm, TTS: &fakeTTS{}},
		"nil target": nil,
	} {
		_, _, _, err := c.Handle(stdcontext.Background(), nil, "audio/wav", "alloy")
		if !errors.Is(err, ErrIncomplete) {
			t.Errorf("%s: err = %v, want ErrIncomplete", name, err)
		}
	}
}
