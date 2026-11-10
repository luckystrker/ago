package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client side of the REST binding (parity: ag2/ag2/a2a/transports/_http.py
// make_a2a_client for the HTTP+JSON binding; routes and bodies are the ones
// rest.go serves): POST /message:send with the bare Message, GET
// /tasks/{id}[?historyLength=N], POST /tasks/{id}:cancel. Request and reply
// bodies are application/a2a+json; a failure answers a non-2xx status with
// {"error": {"code", "message"}}, mapped to the same sentinels as a JSON-RPC
// error object.

// sendRaw performs message/send on the resolved binding and returns the raw
// result: a Message, or a Task when the server parked the turn.
func (c *Client) sendRaw(ctx context.Context, msg *Message) (json.RawMessage, error) {
	ep, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	if ep.binding != bindingREST {
		return c.call(ctx, MethodMessageSend, map[string]any{"message": msg})
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("%w: encode message: %v", ErrTransport, err)
	}
	return c.restDo(ctx, MethodMessageSend, http.MethodPost, ep.restURL+RESTMessageSendPath, body)
}

// getRaw performs tasks/get on the resolved binding, honouring
// WithHistoryLength.
func (c *Client) getRaw(ctx context.Context, taskID string) (json.RawMessage, error) {
	ep, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	if ep.binding != bindingREST {
		params := map[string]any{"id": taskID}
		if c.historyLen > 0 {
			params["historyLength"] = c.historyLen
		}
		return c.call(ctx, MethodTasksGet, params)
	}
	target := ep.restURL + RESTTasksPath + "/" + url.PathEscape(taskID)
	if c.historyLen > 0 {
		target += "?historyLength=" + strconv.Itoa(c.historyLen)
	}
	return c.restDo(ctx, MethodTasksGet, http.MethodGet, target, nil)
}

// cancelRaw performs tasks/cancel on the resolved binding.
func (c *Client) cancelRaw(ctx context.Context, taskID string) (json.RawMessage, error) {
	ep, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	if ep.binding != bindingREST {
		return c.call(ctx, MethodTasksCancel, map[string]any{"id": taskID})
	}
	return c.restDo(ctx, MethodTasksCancel, http.MethodPost,
		ep.restURL+RESTTasksPath+"/"+url.PathEscape(taskID)+":cancel", nil)
}

// restDo performs one REST exchange and returns the raw 2xx body.
func (c *Client) restDo(ctx context.Context, method, verb, target string, body []byte) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, verb, target, rd)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrTransport, err)
	}
	req.Header.Set("Accept", MediaA2AJSON)
	if body != nil {
		req.Header.Set("Content-Type", MediaA2AJSON)
	}
	if err := c.intercept(ctx, method, req); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrTransport, method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBody))
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrTransport, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var env struct {
			Error *RPCError `json:"error"`
		}
		if json.Unmarshal(data, &env) == nil && env.Error != nil {
			return nil, rpcErrToGo(env.Error)
		}
		return nil, fmt.Errorf("%w: %s responded %d: %s", ErrTransport, method, resp.StatusCode,
			strings.TrimSpace(string(data)))
	}
	return data, nil
}
