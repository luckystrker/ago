package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// streamLineLimit caps one NDJSON line of a streaming answer. Chunks carry
// plain text, so the cap only guards against a broken or hostile endpoint
// exhausting memory; it matches the buffered response cap of internal/httpx.
const streamLineLimit = 8 << 20

// callStreaming posts a streaming /api/chat request and folds the NDJSON chunk
// lines into progress events plus the final ModelResponse (Python parity:
// _call_streaming — reasoning chunks, text chunks, tool calls accumulated over
// the stream, usage and finish reason from the chunk flagged done).
//
// AGO-DEVIATION: the Python client reads the stream through the ollama SDK;
// this port scans NDJSON lines directly, because the shared transport
// internal/httpx reads SSE frames only and Ollama answers NDJSON.
//
// Transport retries are deliberately absent, as on the non-streaming path:
// the Python ollama SDK posts plain httpx without retry logic.
func (c *client) callStreaming(ctx context.Context, url string, headers map[string]string, body map[string]any, conv *agocontext.Context) (*events.ModelResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ollama: encoding request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("ollama: bad request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// A stream may be long-lived: no client-level Timeout, cancellation via ctx
	// (the same rule internal/httpx applies to streaming requests).
	hc := c.cfg.httpClient
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("ollama: stream request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, config.NewAPIError(config.ProviderOllama, "ollama: chat", resp.StatusCode, nil, data)
	}

	var (
		content strings.Builder
		calls   []events.ToolCallEvent
		usage   events.Usage
		finish  string
		model   string
	)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), streamLineLimit)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk chatResponse
		if err := json.Unmarshal(line, &chunk); err != nil {
			return nil, fmt.Errorf("ollama: decoding stream chunk: %w", err)
		}
		if chunk.Error != "" {
			return nil, fmt.Errorf("ollama: stream error: %s", chunk.Error)
		}
		if thinking := chunk.Message.Thinking; thinking != "" {
			if err := sendEvent(conv, &events.ModelReasoning{Content: thinking}); err != nil {
				return nil, err
			}
		}
		if text := chunk.Message.Content; text != "" {
			content.WriteString(text)
			if err := sendEvent(conv, &events.ModelMessageChunk{Content: text}); err != nil {
				return nil, err
			}
		}
		calls = append(calls, toolCallEvents(chunk.Message.ToolCalls)...)
		if chunk.Done {
			usage = normalizeUsage(&chunk)
			finish = chunk.DoneReason
			model = chunk.Model
		}
	}
	if err := sc.Err(); err != nil {
		// A canceled call tears the body read down; report the cancellation
		// itself instead of the transport noise around it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("ollama: reading stream: %w", err)
	}

	out := &events.ModelResponse{
		ToolCalls:    events.ToolCallsEvent{Calls: calls},
		Usage:        usage,
		Model:        model,
		Provider:     string(config.ProviderOllama),
		FinishReason: finish,
	}
	if content.Len() > 0 {
		msg := &events.ModelMessage{Content: content.String()}
		if err := sendEvent(conv, msg); err != nil {
			return nil, err
		}
		out.Message = msg
	}
	return out, nil
}
