package live

import (
	stdcontext "context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTranscriberStripsAuthOnCrossHostRedirect: the default client must not
// carry the Bearer key to whatever host a redirect names.
func TestTranscriberStripsAuthOnCrossHostRedirect(t *testing.T) {
	var gotAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "redirected transcript")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/audio/transcriptions", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	tr := &OpenAITranscriber{APIKey: "secret", BaseURL: source.URL}
	text, err := tr.Transcribe(stdcontext.Background(), []byte("CLIP"), "audio/wav")
	if err != nil {
		t.Fatalf("transcribe through the redirect: %v", err)
	}
	if text != "redirected transcript" {
		t.Fatalf("text = %q, want the redirected answer", text)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization leaked to the redirected host: %q", gotAuth)
	}
}

// TestSynthesizerSameHostRedirectKeepsAuth: a hop inside the same authority
// (gateway re-routing) keeps the Bearer key and the audio answer.
func TestSynthesizerSameHostRedirectKeepsAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/final" {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "audio/mpeg")
		fmt.Fprint(w, "MP3DATA")
	}))
	defer srv.Close()

	sy := &OpenAISynthesizer{APIKey: "secret", BaseURL: srv.URL}
	audio, mime, err := sy.Synthesize(stdcontext.Background(), "hello", "alloy")
	if err != nil {
		t.Fatalf("synthesize through the redirect: %v", err)
	}
	if string(audio) != "MP3DATA" || mime != "audio/mpeg" {
		t.Fatalf("audio = %q/%q, want MP3DATA/audio/mpeg", audio, mime)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("same-host redirect must keep the Bearer key, got %q", gotAuth)
	}
}
