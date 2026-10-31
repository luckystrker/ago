package live

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/luckystrker/ago/internal/httpx"
)

// quoteEscaper escapes a filename for a Content-Disposition header
// (multipart.Writer's own rule).
var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, `\"`)

// ErrAPI reports a non-2xx answer from a speech provider. Match with
// errors.Is; the error text carries the HTTP status and a body snippet.
var ErrAPI = errors.New("live: speech provider API error")

// defaultBaseURL is the public OpenAI API root; BaseURL overrides it (tests
// point it at httptest servers, proxies at gateways).
const defaultBaseURL = "https://api.openai.com/v1"

// defaultClient serves the transcriber and synthesizer when their Client
// field is nil. It behaves like http.DefaultClient except that redirects to
// another host drop every non-safelisted header: the Bearer key must not
// reach whatever host a redirect names.
var defaultClient = &http.Client{CheckRedirect: httpx.RedirectStripCredentials.CheckRedirect()}

// maxResponseBytes caps a buffered provider answer. TTS returns raw audio, so
// the cap sits well above the repo's 8 MiB JSON norm: 64 MiB holds over
// twenty minutes of 24 kHz 16-bit PCM with headroom.
const maxResponseBytes int64 = 64 << 20

// readCapped reads a provider answer up to maxResponseBytes; a larger body is
// an error instead of unbounded memory.
func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxResponseBytes {
		return nil, fmt.Errorf("live: response body exceeds %d bytes", maxResponseBytes)
	}
	return data, nil
}

// baseMimeType strips parameters and lower-cases a media type
// ("Audio/WAV; rate=16000" → "audio/wav").
func baseMimeType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// audioExt picks an upload filename extension for the clip. The transcription
// endpoint infers the container from the filename, so the mime type has to map
// onto one of the supported names; unknown audio/* subtypes pass through,
// everything else falls back to "bin".
func audioExt(mimeType string) string {
	switch baseMimeType(mimeType) {
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return "wav"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/mp4", "audio/x-m4a", "audio/m4a":
		return "m4a"
	case "audio/webm", "video/webm":
		return "webm"
	case "audio/ogg", "application/ogg":
		return "ogg"
	case "audio/flac", "audio/x-flac":
		return "flac"
	case "video/mp4":
		return "mp4"
	}
	if rest, ok := strings.CutPrefix(baseMimeType(mimeType), "audio/"); ok && isPlainToken(rest) {
		return rest
	}
	return "bin"
}

func isPlainToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// OpenAITranscriber calls POST /audio/transcriptions (Whisper) over net/http —
// Python parity: live/openai.py STTConfig. The zero value is unusable without
// an APIKey; unset fields take their documented defaults. Safe for concurrent
// use.
//
// AGO-DEVIATION: Python streams transcript deltas (TranscriptionChunkEvent) and
// reports TranscriptionCompletedEvent on the context; this client makes one
// blocking request (response_format=text) — the cascade shape has no live
// event stream to publish deltas to (D-057).
type OpenAITranscriber struct {
	APIKey string
	// Model is the transcription model id; "" → "whisper-1".
	Model string
	// BaseURL is the API root; "" → https://api.openai.com/v1.
	BaseURL string
	// Client is the HTTP client; nil → the package default client (stdlib
	// behavior plus credential stripping on cross-host redirects).
	Client *http.Client
	// Language is the optional ISO-639-1 hint of the input audio.
	Language string
}

var _ Transcriber = (*OpenAITranscriber)(nil)

func (t *OpenAITranscriber) model() string {
	if t.Model != "" {
		return t.Model
	}
	return "whisper-1"
}

func (t *OpenAITranscriber) baseURL() string {
	if t.BaseURL != "" {
		return t.BaseURL
	}
	return defaultBaseURL
}

func (t *OpenAITranscriber) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return defaultClient
}

// Transcribe uploads the clip as multipart form data and returns the transcript
// text. The filename extension is derived from mime (the endpoint infers the
// container from it), the response_format is fixed to "text" so the body is the
// plain transcript. Whitespace around the transcript is trimmed.
func (t *OpenAITranscriber) Transcribe(ctx stdcontext.Context, audio []byte, mime string) (string, error) {
	if t == nil {
		return "", errors.New("live: nil OpenAITranscriber")
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	// CreateFormFile pins application/octet-stream; the endpoint infers the
	// container from the part's type, so the part carries the real mime.
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		`form-data; name="file"; filename="`+quoteEscaper.Replace("speech."+audioExt(mime))+`"`)
	h.Set("Content-Type", mime)
	fh, err := w.CreatePart(h)
	if err != nil {
		return "", fmt.Errorf("live: openai transcribe: %w", err)
	}
	if _, err := fh.Write(audio); err != nil {
		return "", fmt.Errorf("live: openai transcribe: %w", err)
	}
	for _, f := range []struct{ k, v string }{
		{"model", t.model()},
		{"response_format", "text"},
		{"language", t.Language},
	} {
		if f.v == "" {
			continue
		}
		if err := w.WriteField(f.k, f.v); err != nil {
			return "", fmt.Errorf("live: openai transcribe: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("live: openai transcribe: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(t.baseURL(), "/")+"/audio/transcriptions", &body)
	if err != nil {
		return "", fmt.Errorf("live: openai transcribe: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := t.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("live: openai transcribe: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Only a snippet of an error body is ever reported — never buffer a
		// giant one whole.
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("live: openai transcribe: %w: status %d: %s", ErrAPI, resp.StatusCode, snippet(data))
	}
	data, err := readCapped(resp.Body)
	if err != nil {
		return "", fmt.Errorf("live: openai transcribe: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// OpenAISynthesizer calls POST /audio/speech over net/http — Python parity:
// live/openai.py TTSConfig. The zero value is unusable without an APIKey; unset
// fields take their documented defaults. Safe for concurrent use.
//
// AGO-DEVIATION: Python pins response_format="pcm" to feed its realtime
// player; here Format is a field ("" → "mp3", the API default) because the
// sound-device pipeline is deferred (D-057) and the cascade reports the media
// type of whatever comes back.
type OpenAISynthesizer struct {
	APIKey string
	// Model is the speech model id; "" → "tts-1".
	Model string
	// BaseURL is the API root; "" → https://api.openai.com/v1.
	BaseURL string
	// Client is the HTTP client; nil → the package default client (stdlib
	// behavior plus credential stripping on cross-host redirects).
	Client *http.Client
	// Voice is the fallback voice when a call passes ""; "" → "alloy"
	// (Python parity: TTSConfig's voice default).
	Voice string
	// Format is the response_format: mp3, opus, aac, flac, wav or pcm;
	// "" → "mp3".
	Format string
	// Speed is the optional speech rate; 0 omits the field.
	Speed float64
}

var _ Synthesizer = (*OpenAISynthesizer)(nil)

type speechRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed,omitempty"`
}

func (s *OpenAISynthesizer) model() string {
	if s.Model != "" {
		return s.Model
	}
	return "tts-1"
}

func (s *OpenAISynthesizer) baseURL() string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return defaultBaseURL
}

func (s *OpenAISynthesizer) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return defaultClient
}

// Synthesize posts the JSON request and returns the raw audio bytes plus the
// media type: the response's Content-Type when present, otherwise the type
// implied by Format.
func (s *OpenAISynthesizer) Synthesize(ctx stdcontext.Context, text string, voice string) ([]byte, string, error) {
	if s == nil {
		return nil, "", errors.New("live: nil OpenAISynthesizer")
	}
	v := voice
	if v == "" {
		v = s.Voice
	}
	if v == "" {
		v = "alloy"
	}
	format := s.Format
	if format == "" {
		format = "mp3"
	}

	payload, err := json.Marshal(speechRequest{
		Model:          s.model(),
		Input:          text,
		Voice:          v,
		ResponseFormat: format,
		Speed:          s.Speed,
	})
	if err != nil {
		return nil, "", fmt.Errorf("live: openai synthesize: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(s.baseURL(), "/")+"/audio/speech", bytes.NewReader(payload))
	if err != nil {
		return nil, "", fmt.Errorf("live: openai synthesize: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("live: openai synthesize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Only a snippet of an error body is ever reported — never buffer a
		// giant one whole.
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("live: openai synthesize: %w: status %d: %s", ErrAPI, resp.StatusCode, snippet(data))
	}
	data, err := readCapped(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("live: openai synthesize: %w", err)
	}

	mimeType := baseMimeType(resp.Header.Get("Content-Type"))
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = mimeForFormat(format)
	}
	return data, mimeType, nil
}

// mimeForFormat maps a speech response_format onto its media type, used when
// the provider omits the response Content-Type.
func mimeForFormat(format string) string {
	switch format {
	case "mp3":
		return "audio/mpeg"
	case "opus":
		return "audio/ogg"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "wav":
		return "audio/wav"
	case "pcm":
		return "audio/pcm"
	default:
		return "application/octet-stream"
	}
}

// snippet condenses an error body into one line for the error text.
func snippet(b []byte) string {
	s := strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", " ")
	if len(s) > 256 {
		s = s[:256] + "..."
	}
	return s
}
