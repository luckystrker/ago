package a2a

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/luckystrker/ago/internal/uuid"
)

// Push notifications (parity: ag2/ag2/a2a/push.py + server.py's
// push_config_store / push_sender / push_url_validator wiring).
//
// A push config subscribes one webhook URL to one task: the server stores it,
// and when the task reaches a terminal state it POSTs the task record to the
// URL and keeps going — the subscriber reacts without holding the RPC open.
//
// Wire shapes follow the a2a-sdk TaskPushNotificationConfig proto (url, token,
// authentication{scheme, credentials}, id — src/a2a/types/a2a_pb2.pyi:289);
// the config id is server-issued on create when the caller leaves it empty
// (push.py A2APushConfig docstring). The JSON-RPC method names are the v0.3
// surface the package already speaks (jsonrpc.go).
const (
	// HeaderPushToken carries the webhook token on server→subscriber pushes
	// and is what PushReceiver validates (AGO-DEVIATION: the a2a-sdk leaves
	// the sender semantics to an app-supplied PushNotificationSender, so the
	// token header spelling is this package's contract between its own
	// default sender and receiver; a custom sender may use anything).
	HeaderPushToken = "X-A2A-Token"
)

// PushAuthentication is the auth metadata attached to a push webhook
// (parity: push.py A2APushAuthentication — scheme + opaque credentials).
type PushAuthentication struct {
	Scheme      string `json:"scheme"`
	Credentials string `json:"credentials,omitempty"`
}

// PushConfig is one push-notification subscription; ID is server-issued on
// create when empty (parity: push.py A2APushConfig).
type PushConfig struct {
	URL            string              `json:"url"`
	Token          string              `json:"token,omitempty"`
	Authentication *PushAuthentication `json:"authentication,omitempty"`
	ID             string              `json:"id,omitempty"`
}

// PushConfigStore persists push configs per task (parity: the a2a-sdk
// PushNotificationConfigStore the Python server takes as push_config_store).
// Implementations must be safe for concurrent use.
type PushConfigStore interface {
	// Set stores cfg for taskID, assigning ID when empty, and returns the
	// stored config.
	Set(ctx context.Context, taskID string, cfg PushConfig) (PushConfig, error)
	// Get returns the config with configID registered on taskID;
	// ErrPushConfigNotFound when absent.
	Get(ctx context.Context, taskID, configID string) (PushConfig, error)
	// List returns every config registered on taskID (stable order).
	List(ctx context.Context, taskID string) ([]PushConfig, error)
	// Delete drops configID from taskID; deleting an absent id is not an error.
	Delete(ctx context.Context, taskID, configID string) error
}

// ErrPushConfigNotFound reports a push config id absent from the store. The
// server maps it to the wire task-not-found error — a push config is scoped
// to its task, and both misses surface the same way.
var ErrPushConfigNotFound = fmt.Errorf("%w: push config", ErrTaskNotFound)

// ErrPushValidation reports a push config the server refuses at set time: an
// empty URL or one the configured URL validator rejects.
var ErrPushValidation = fmt.Errorf("a2a: invalid push notification config")

// MemoryPushConfigStore is the in-memory PushConfigStore. Safe for
// concurrent use; configs are returned by value.
type MemoryPushConfigStore struct {
	mu      sync.Mutex
	configs map[string][]PushConfig // taskID → configs, insertion order
}

// NewMemoryPushConfigStore builds an empty in-memory PushConfigStore.
func NewMemoryPushConfigStore() *MemoryPushConfigStore {
	return &MemoryPushConfigStore{configs: make(map[string][]PushConfig)}
}

// Set implements PushConfigStore.
func (s *MemoryPushConfigStore) Set(_ context.Context, taskID string, cfg PushConfig) (PushConfig, error) {
	if cfg.ID == "" {
		cfg.ID = uuid.New()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configs == nil {
		s.configs = make(map[string][]PushConfig)
	}
	s.configs[taskID] = append(s.configs[taskID], cfg)
	return cfg, nil
}

// Get implements PushConfigStore.
func (s *MemoryPushConfigStore) Get(_ context.Context, taskID, configID string) (PushConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cfg := range s.configs[taskID] {
		if cfg.ID == configID {
			return cfg, nil
		}
	}
	return PushConfig{}, fmt.Errorf("%w: %s on task %s", ErrPushConfigNotFound, configID, taskID)
}

// List implements PushConfigStore.
func (s *MemoryPushConfigStore) List(_ context.Context, taskID string) ([]PushConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PushConfig, len(s.configs[taskID]))
	copy(out, s.configs[taskID])
	return out, nil
}

// Delete implements PushConfigStore.
func (s *MemoryPushConfigStore) Delete(_ context.Context, taskID, configID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.configs[taskID]
	if !ok {
		return nil
	}
	kept := existing[:0]
	for _, cfg := range existing {
		if cfg.ID != configID {
			kept = append(kept, cfg)
		}
	}
	s.configs[taskID] = kept
	return nil
}

// PushSender delivers one terminal task to one subscriber webhook. Return
// values are dropped by the server — a webhook outage must not fail the
// already-terminal RPC (parity: the a2a-sdk fires push after the task
// transition, independent of the reply).
type PushSender func(ctx context.Context, taskID string, cfg PushConfig, task *Task) error

// DefaultPushSender POSTs the task record to cfg.URL as
// application/a2a+json, with the token on HeaderPushToken and the config's
// authentication credentials on Authorization when set.
func DefaultPushSender(ctx context.Context, taskID string, cfg PushConfig, task *Task) error {
	if cfg.URL == "" {
		return fmt.Errorf("%w: empty url", ErrPushValidation)
	}
	body, err := json.Marshal(task)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/a2a+json")
	if cfg.Token != "" {
		req.Header.Set(HeaderPushToken, cfg.Token)
	}
	if cfg.Authentication != nil && cfg.Authentication.Credentials != "" {
		scheme := cfg.Authentication.Scheme
		if scheme == "" {
			scheme = "Bearer"
		}
		req.Header.Set("Authorization", scheme+" "+cfg.Authentication.Credentials)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}

// PushReceiver is the http.Handler receiving server→subscriber pushes: POST
// only, the X-A2A-Token header checked against the expected token in constant
// time, the body decoded as a Task, the callback invoked with it. Build with
// NewPushReceiver.
type PushReceiver struct {
	token    string
	callback func(ctx context.Context, task *Task) error
}

// NewPushReceiver builds a webhook receiver validating token (empty token
// disables validation) and handing every decoded task to callback.
func NewPushReceiver(token string, callback func(ctx context.Context, task *Task) error) *PushReceiver {
	return &PushReceiver{token: token, callback: callback}
}

// ServeHTTP implements http.Handler.
func (r *PushReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.token != "" && subtle.ConstantTimeCompare([]byte(req.Header.Get(HeaderPushToken)), []byte(r.token)) != 1 {
		http.Error(w, "push token mismatch", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBody))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		http.Error(w, "decode task: "+err.Error(), http.StatusBadRequest)
		return
	}
	if r.callback == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := r.callback(req.Context(), &task); err != nil {
		http.Error(w, "callback: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// --- server-side JSON-RPC surface --------------------------------------------

// servePushSet handles tasks/pushNotificationConfig/set: params
// {taskId, pushNotificationConfig} → the stored config with its server-issued
// id (parity: push.py create_push_notification_config).
func (s *Server) servePushSet(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	if s.cfg.PushStore == nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeUnsupportedOperation, Message: "push notifications are not enabled on this server"})
		return
	}
	var params struct {
		TaskID string      `json:"taskId"`
		Config *PushConfig `json:"pushNotificationConfig"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params: " + err.Error()})
		return
	}
	if params.TaskID == "" || params.Config == nil || params.Config.URL == "" {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params.taskId and params.pushNotificationConfig.url are required"})
		return
	}
	if s.cfg.PushURLValidator != nil && !s.cfg.PushURLValidator(params.Config.URL) {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "push url rejected by the server validator"})
		return
	}
	// Task must exist — a push config scopes to a live task record.
	if _, err := s.store.Get(ctx, params.TaskID); err != nil {
		writeRPCError(w, req.ID, storeError(err))
		return
	}
	stored, err := s.cfg.PushStore.Set(ctx, params.TaskID, *params.Config)
	if err != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInternalError, Message: "save push config: " + err.Error()})
		return
	}
	writeRPCResult(w, req.ID, stored)
}

// servePushGet handles tasks/pushNotificationConfig/get: params
// {taskId, id} → the config (parity: push.py get_push_notification_config).
func (s *Server) servePushGet(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	cfg, rpcErr := s.pushConfigOp(ctx, w, req, func(ctx context.Context, taskID, configID string) (any, error) {
		return s.cfg.PushStore.Get(ctx, taskID, configID)
	})
	if rpcErr == nil {
		writeRPCResult(w, req.ID, cfg)
	}
}

// servePushList handles tasks/pushNotificationConfig/list: params {taskId} →
// {configs: [...]} (parity: push.py list_push_notification_configs).
func (s *Server) servePushList(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	cfgs, rpcErr := s.pushConfigOp(ctx, w, req, func(ctx context.Context, taskID, _ string) (any, error) {
		return s.cfg.PushStore.List(ctx, taskID)
	})
	if rpcErr == nil {
		writeRPCResult(w, req.ID, map[string]any{"configs": cfgs})
	}
}

// servePushDelete handles tasks/pushNotificationConfig/delete: params
// {taskId, id} → null result (parity: push.py delete_push_notification_config).
func (s *Server) servePushDelete(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	_, rpcErr := s.pushConfigOp(ctx, w, req, func(ctx context.Context, taskID, configID string) (any, error) {
		return nil, s.cfg.PushStore.Delete(ctx, taskID, configID)
	})
	if rpcErr == nil {
		writeRPCResult(w, req.ID, nil)
	}
}

// pushConfigOp validates the {taskId, id} params, checks that the push
// surface is enabled, runs op, and writes the wire error when it fails.
func (s *Server) pushConfigOp(ctx context.Context, w http.ResponseWriter, req *rpcRequest, op func(ctx context.Context, taskID, configID string) (any, error)) (any, *RPCError) {
	if s.cfg.PushStore == nil {
		err := &RPCError{Code: codeUnsupportedOperation, Message: "push notifications are not enabled on this server"}
		writeRPCError(w, req.ID, err)
		return nil, err
	}
	var params struct {
		TaskID string `json:"taskId"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		err := &RPCError{Code: codeInvalidParams, Message: "params: " + err.Error()}
		writeRPCError(w, req.ID, err)
		return nil, err
	}
	if params.TaskID == "" {
		err := &RPCError{Code: codeInvalidParams, Message: "params.taskId is required"}
		writeRPCError(w, req.ID, err)
		return nil, err
	}
	if params.ID == "" && req.Method != MethodPushList {
		err := &RPCError{Code: codeInvalidParams, Message: "params.id is required"}
		writeRPCError(w, req.ID, err)
		return nil, err
	}
	if _, err := s.store.Get(ctx, params.TaskID); err != nil {
		err := storeError(err)
		writeRPCError(w, req.ID, err)
		return nil, err
	}
	out, opErr := op(ctx, params.TaskID, params.ID)
	if opErr != nil {
		rpcErr := storeError(opErr)
		writeRPCError(w, req.ID, rpcErr)
		return nil, rpcErr
	}
	return out, nil
}

// dispatchPush fires the configured sender on every config registered for a
// just-committed terminal task. Best-effort and asynchronous: the RPC has
// already produced its terminal state, so a slow or failing webhook is
// dropped, and the request's cancellation must not kill the delivery
// (context.WithoutCancel keeps the goroutine alive after ServeHTTP returns).
func (s *Server) dispatchPush(ctx context.Context, taskID string, task *Task) {
	if !terminalTaskState(task.Status.State) || s.cfg.PushStore == nil {
		return
	}
	cfgs, err := s.cfg.PushStore.List(ctx, taskID)
	if err != nil || len(cfgs) == 0 {
		return
	}
	sender := s.cfg.PushSender
	if sender == nil {
		sender = DefaultPushSender
	}
	pushCtx := context.WithoutCancel(ctx)
	for _, cfg := range cfgs {
		go func(cfg PushConfig) {
			_ = sender(pushCtx, taskID, cfg, task)
		}(cfg)
	}
}

// --- client side (parity: push.py create/get/list/delete) --------------------

// SetPushConfig registers cfg as a push webhook on taskID and returns the
// stored config with its server-issued id (parity:
// create_push_notification_config).
func (c *Client) SetPushConfig(ctx context.Context, taskID string, cfg PushConfig) (*PushConfig, error) {
	result, err := c.call(ctx, MethodPushSet, map[string]any{
		"taskId":                 taskID,
		"pushNotificationConfig": cfg,
	})
	if err != nil {
		return nil, err
	}
	var stored PushConfig
	if err := json.Unmarshal(result, &stored); err != nil {
		return nil, fmt.Errorf("%w: decode push config: %v", ErrWire, err)
	}
	return &stored, nil
}

// GetPushConfig fetches a previously-registered config by id (parity:
// get_push_notification_config); ErrTaskNotFound when absent.
func (c *Client) GetPushConfig(ctx context.Context, taskID, configID string) (*PushConfig, error) {
	result, err := c.call(ctx, MethodPushGet, map[string]any{"taskId": taskID, "id": configID})
	if err != nil {
		return nil, err
	}
	var cfg PushConfig
	if err := json.Unmarshal(result, &cfg); err != nil {
		return nil, fmt.Errorf("%w: decode push config: %v", ErrWire, err)
	}
	return &cfg, nil
}

// ListPushConfigs lists the configs registered on taskID (parity:
// list_push_notification_configs).
func (c *Client) ListPushConfigs(ctx context.Context, taskID string) ([]PushConfig, error) {
	result, err := c.call(ctx, MethodPushList, map[string]any{"taskId": taskID})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Configs []PushConfig `json:"configs"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode push configs: %v", ErrWire, err)
	}
	return payload.Configs, nil
}

// DeletePushConfig drops a registered config (parity:
// delete_push_notification_config).
func (c *Client) DeletePushConfig(ctx context.Context, taskID, configID string) error {
	_, err := c.call(ctx, MethodPushDelete, map[string]any{"taskId": taskID, "id": configID})
	return err
}
