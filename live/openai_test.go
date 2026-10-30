package live

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// transcriptionFake is an httptest server answering POST /audio/transcriptions
// like OpenAI's whisper endpoint. It records the request for assertions.
type transcriptionFake struct {
	srv    *httptest.Server
	status int
	body   string

	gotAuth     string
	gotModel    string
	gotFormat   string
	gotLanguage string
	gotFilename string
	gotFileType string
	gotAudio    []byte
}

func newTranscriptionFake(t *testing.T, status int, body string) *transcriptionFake {
	t.Helper()
	f := &transcriptionFake{status: status, body: body}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		f.gotAuth = r.Header.Get("Authorization")
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, "not multipart: "+err.Error(), http.StatusBadRequest)
			return
		}
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			data, err := io.ReadAll(part)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			switch part.FormName() {
			case "file":
				f.gotFilename, f.gotFileType, f.gotAudio = part.FileName(), part.Header.Get("Content-Type"), data
			case "model":
				f.gotModel = string(data)
			case "response_format":
				f.gotFormat = string(data)
			case "language":
				f.gotLanguage = string(data)
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// TestOpenAITranscriberTranscribes: the clip goes up as multipart with the
// model and response_format fields, the auth header is set, and the text body
// comes back trimmed.
func TestOpenAITranscriberTranscribes(t *testing.T) {
	fake := newTranscriptionFake(t, http.StatusOK, " hello world \n")
	tr := &OpenAITranscriber{APIKey: "test-key", BaseURL: fake.srv.URL}

	text, err := tr.Transcribe(stdcontext.Background(), []byte("FAKEWAVBYTES"), "audio/wav")
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if text != "hello world" {
		t.Errorf("text = %q, want %q", text, "hello world")
	}
	if fake.gotAuth != "Bearer test-key" {
		t.Errorf("auth = %q, want %q", fake.gotAuth, "Bearer test-key")
	}
	if fake.gotModel != "whisper-1" || fake.gotFormat != "text" {
		t.Errorf("fields model=%q response_format=%q, want whisper-1/text", fake.gotModel, fake.gotFormat)
	}
	if fake.gotFilename != "speech.wav" || fake.gotFileType != "audio/wav" {
		t.Errorf("upload = %q (%s), want speech.wav (audio/wav)", fake.gotFilename, fake.gotFileType)
	}
	if string(fake.gotAudio) != "FAKEWAVBYTES" {
		t.Errorf("uploaded audio = %q, want the clip bytes", fake.gotAudio)
	}
	if fake.gotLanguage != "" {
		t.Errorf("language = %q, want the field omitted", fake.gotLanguage)
	}
}

// TestOpenAITranscriberLanguageAndModel: set fields land on the wire.
func TestOpenAITranscriberLanguageAndModel(t *testing.T) {
	fake := newTranscriptionFake(t, http.StatusOK, "hola")
	tr := &OpenAITranscriber{APIKey: "k", BaseURL: fake.srv.URL, Model: "whisper-large", Language: "es"}

	if _, err := tr.Transcribe(stdcontext.Background(), []byte("x"), "audio/mpeg"); err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if fake.gotModel != "whisper-large" || fake.gotLanguage != "es" {
		t.Errorf("fields model=%q language=%q, want whisper-large/es", fake.gotModel, fake.gotLanguage)
	}
	if fake.gotFilename != "speech.mp3" {
		t.Errorf("filename = %q, want speech.mp3 for audio/mpeg", fake.gotFilename)
	}
}

// TestOpenAITranscriberAPIError: a non-2xx surfaces as ErrAPI with the status
// and the body snippet.
func TestOpenAITranscriberAPIError(t *testing.T) {
	fake := newTranscriptionFake(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	tr := &OpenAITranscriber{APIKey: "k", BaseURL: fake.srv.URL}

	_, err := tr.Transcribe(stdcontext.Background(), []byte("x"), "audio/wav")
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("err = %v, want ErrAPI", err)
	}
	for _, want := range []string{"500", "boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

// speechFake is an httptest server answering POST /audio/speech. It records
// the request for assertions.
type speechFake struct {
	srv            *httptest.Server
	status         int
	body           string
	responseCT     string
	gotAuth        string
	gotContentType string
	gotReq         speechRequest
}

func newSpeechFake(t *testing.T, status int, responseCT, body string) *speechFake {
	t.Helper()
	f := &speechFake{status: status, body: body, responseCT: responseCT}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/audio/speech" {
			http.NotFound(w, r)
			return
		}
		f.gotAuth = r.Header.Get("Authorization")
		f.gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&f.gotReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if responseCT != "" {
			w.Header().Set("Content-Type", responseCT)
		}
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// TestOpenAISynthesizerSpeaks: the JSON request carries model/input/voice/
// response_format, and the audio bytes come back with the provider's
// Content-Type.
func TestOpenAISynthesizerSpeaks(t *testing.T) {
	fake := newSpeechFake(t, http.StatusOK, "audio/mpeg", "MP3DATA")
	sy := &OpenAISynthesizer{APIKey: "test-key", BaseURL: fake.srv.URL}

	audio, mime, err := sy.Synthesize(stdcontext.Background(), "hello there", "coral")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if string(audio) != "MP3DATA" {
		t.Errorf("audio = %q, want the provider bytes", audio)
	}
	if mime != "audio/mpeg" {
		t.Errorf("mime = %q, want audio/mpeg from the response Content-Type", mime)
	}
	if fake.gotAuth != "Bearer test-key" || !strings.HasPrefix(fake.gotContentType, "application/json") {
		t.Errorf("request headers auth=%q content-type=%q", fake.gotAuth, fake.gotContentType)
	}
	if fake.gotReq.Model != "tts-1" || fake.gotReq.Input != "hello there" ||
		fake.gotReq.Voice != "coral" || fake.gotReq.ResponseFormat != "mp3" || fake.gotReq.Speed != 0 {
		t.Errorf("request = %+v, want the default model/format and the given voice/input", fake.gotReq)
	}
}

// TestOpenAISynthesizerOverridesAndMimeFallback: explicit model/format/speed
// and the struct-level default voice land on the wire; an uninformative
// provider Content-Type falls back to the type implied by the format.
func TestOpenAISynthesizerOverridesAndMimeFallback(t *testing.T) {
	// application/octet-stream: no real media type, the format must decide.
	fake := newSpeechFake(t, http.StatusOK, "application/octet-stream", "PCMDATA")
	sy := &OpenAISynthesizer{APIKey: "k", BaseURL: fake.srv.URL, Model: "gpt-4o-mini-tts",
		Voice: "alloy", Format: "pcm", Speed: 1.5}

	audio, mime, err := sy.Synthesize(stdcontext.Background(), "speak", "")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if string(audio) != "PCMDATA" {
		t.Errorf("audio = %q, want the provider bytes", audio)
	}
	if mime != "audio/pcm" {
		t.Errorf("mime = %q, want audio/pcm from the format fallback", mime)
	}
	if fake.gotReq.Model != "gpt-4o-mini-tts" || fake.gotReq.Voice != "alloy" ||
		fake.gotReq.ResponseFormat != "pcm" || fake.gotReq.Speed != 1.5 {
		t.Errorf("request = %+v, want the overrides applied", fake.gotReq)
	}
}

// TestOpenAISynthesizerAPIError: a non-2xx surfaces as ErrAPI with the status.
func TestOpenAISynthesizerAPIError(t *testing.T) {
	fake := newSpeechFake(t, http.StatusServiceUnavailable, "application/json", `{"error":"overloaded"}`)
	sy := &OpenAISynthesizer{APIKey: "k", BaseURL: fake.srv.URL}

	_, _, err := sy.Synthesize(stdcontext.Background(), "hello", "alloy")
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("err = %v, want ErrAPI", err)
	}
	if !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("err = %v, want the status and the body snippet", err)
	}
}

// TestCascadeOverHTTPFakes: the whole cascade on OpenAI-shaped httptest fakes —
// STT and TTS over the real HTTP clients, the LLM as a plain function.
func TestCascadeOverHTTPFakes(t *testing.T) {
	sttFake := newTranscriptionFake(t, http.StatusOK, "say hi\n")
	ttsFake := newSpeechFake(t, http.StatusOK, "audio/wav", "WAVDATA")

	c := &Cascade{
		STT: &OpenAITranscriber{APIKey: "k", BaseURL: sttFake.srv.URL},
		LLM: func(_ stdcontext.Context, text string) (string, error) { return "echo: " + text, nil },
		TTS: &OpenAISynthesizer{APIKey: "k", BaseURL: ttsFake.srv.URL, Format: "wav"},
	}

	reply, audioOut, mimeOut, err := c.Handle(stdcontext.Background(), []byte("CLIP"), "audio/wav", "nova")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if reply != "echo: say hi" {
		t.Errorf("reply = %q, want %q", reply, "echo: say hi")
	}
	if string(audioOut) != "WAVDATA" || mimeOut != "audio/wav" {
		t.Errorf("audio out = %q/%q, want WAVDATA/audio/wav", audioOut, mimeOut)
	}
	if ttsFake.gotReq.Voice != "nova" {
		t.Errorf("tts request voice = %q, want the voice forwarded through the cascade", ttsFake.gotReq.Voice)
	}
}
