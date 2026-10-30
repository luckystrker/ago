package otelx

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

func passTurn(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	return &events.ModelResponse{}, nil
}

func passLLM(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	return &events.ModelResponse{}, nil
}

func TestInstallDefaultStdout(t *testing.T) {
	var buf bytes.Buffer
	factory, shutdown, err := InstallDefault(context.Background(), WithStdoutWriter(&buf))
	if err != nil {
		t.Fatalf("InstallDefault: %v", err)
	}
	if factory == nil || shutdown == nil {
		t.Fatal("factory or shutdown nil")
	}

	if _, err := factory().OnTurn(passTurn, &events.ModelRequest{}, nil); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"ago.turn"`)) {
		t.Errorf("stdout export missing the ago.turn span:\n%s", buf.String())
	}
}

// TestInstallDefaultCollector drives the OTLP/HTTP exporter against an
// httptest server (network-free per repo rules): the WithCollector endpoint
// must reach the standard /v1/traces path and the shutdown flush must deliver
// the span.
func TestInstallDefaultCollector(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("collector got %s, want /v1/traces", r.URL.Path)
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer srv.Close()

	factory, shutdown, err := InstallDefault(context.Background(), WithCollector(srv.URL))
	if err != nil {
		t.Fatalf("InstallDefault: %v", err)
	}

	if _, err := factory().OnLLMCall(passLLM, nil, nil); err != nil {
		t.Fatalf("OnLLMCall: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if hits.Load() == 0 {
		t.Error("collector received no export requests")
	}
}
