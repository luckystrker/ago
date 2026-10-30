// The Anthropic Files API client: upload (multipart), read (content bytes +
// metadata), list (cursor pagination), delete. Parity with
// ag2/config/anthropic/files.py (AnthropicFilesClient), which drives the
// anthropic SDK's beta.files namespace; AGo speaks the wire protocol
// directly: {base}/v1/files under the files-api beta flag. The transport
// retries mirror internal/httpx (connection errors and 429/5xx, Retry-After
// honored) — httpx itself only speaks POST+JSON, which cannot carry these
// multipart/GET/DELETE requests.

package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/files"
)

// ErrFilesAPI marks a non-2xx Files API response. Match with errors.Is.
var ErrFilesAPI = errors.New("anthropic: files API")

const (
	// filesPath is the beta Files API root under the base URL.
	filesPath = "/v1/files"
	// filesBeta is the beta flag the Files API answers to (the Python client
	// reaches it through the SDK's beta.files namespace).
	filesBeta = "files-api-2025-04-14"

	acceptJSON          = "application/json"
	acceptBinaryContent = "application/octet-stream"
)

// FilesClient is the Files API client built by
// AnthropicConfig.CreateFilesClient. Safe for concurrent use.
type FilesClient struct {
	apiKey     string
	baseURL    string
	timeout    time.Duration // caps each request
	maxRetries int
	http       *http.Client
}

// CreateFilesClient returns the Files API client for this configuration
// (Python parity: create_files_client). The API key resolves like Create
// (config field, then ANTHROPIC_API_KEY); a missing key wraps
// config.ErrNotProvided. An unset Timeout becomes 600s — Python parity with
// files.py (`config.timeout if config.timeout is not None else 600.0`).
// AGO-DEVIATION: Python also forwards default_headers, http_client and
// open_telemetry to the SDK; the AGo config has no such fields.
func (c *AnthropicConfig) CreateFilesClient() (files.Client, error) {
	key := c.APIKey
	if key == "" {
		key = os.Getenv(APIKeyEnv)
	}
	if key == "" {
		return nil, fmt.Errorf("%w: api key (set APIKey or %s)", config.ErrNotProvided, APIKeyEnv)
	}
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 600 * time.Second
	}
	return &FilesClient{
		apiKey:     key,
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		timeout:    timeout,
		maxRetries: c.MaxRetries,
		http:       &http.Client{Timeout: timeout},
	}, nil
}

// Upload stores data under filename. The purpose argument only fills the
// returned metadata — Python parity: files.py never forwards it to the API,
// the multipart body carries the file alone. The part's content type is
// guessed from the filename extension (default application/octet-stream).
func (c *FilesClient) Upload(ctx context.Context, data []byte, filename, purpose string) (files.UploadedFile, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := writeFilePart(mw, "file", filename, data); err != nil {
		return files.UploadedFile{}, fmt.Errorf("anthropic: files upload: %w", err)
	}
	if err := mw.Close(); err != nil {
		return files.UploadedFile{}, fmt.Errorf("anthropic: files upload: %w", err)
	}

	raw, err := c.doObject(ctx, http.MethodPost, filesPath, mw.FormDataContentType(), acceptJSON, buf.Bytes())
	if err != nil {
		return files.UploadedFile{}, err
	}
	return files.UploadedFile{
		ID:       objString(raw, "id"),
		Filename: objString(raw, "filename"),
		Bytes:    objInt(raw, "size_bytes"),
		Purpose:  purpose,
		Provider: files.ProviderAnthropic,
		Raw:      raw,
	}, nil
}

// Read downloads the content of fileID, then its metadata for the filename
// and media type — the same two calls the Python client makes (download then
// retrieve_metadata). The metadata's mime_type has no typed FileContent
// field; Raw carries it.
func (c *FilesClient) Read(ctx context.Context, fileID string) (files.FileContent, error) {
	id := url.PathEscape(fileID)
	content, err := c.readBytes(ctx, filesPath+"/"+id+"/content")
	if err != nil {
		return files.FileContent{}, err
	}
	meta, err := c.doObject(ctx, http.MethodGet, filesPath+"/"+id, "", acceptJSON, nil)
	if err != nil {
		return files.FileContent{}, err
	}
	return files.FileContent{
		Data:     content,
		Filename: objString(meta, "filename"),
		Provider: files.ProviderAnthropic,
		Raw:      meta,
	}, nil
}

// List returns every file at the provider, walking the cursor pages: no
// query on the first request, then ?page=<next_page> while the answer keeps
// carrying one (AG2 test parity — the SDK follows next_page/page, not the
// has_more/after pair).
func (c *FilesClient) List(ctx context.Context) ([]files.UploadedFile, error) {
	var out []files.UploadedFile
	page := ""
	for {
		listPath := filesPath
		if page != "" {
			listPath += "?page=" + url.QueryEscape(page)
		}
		raw, err := c.doObject(ctx, http.MethodGet, listPath, "", acceptJSON, nil)
		if err != nil {
			return nil, err
		}
		items, _ := raw["data"].([]any)
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, files.UploadedFile{
				ID:       objString(m, "id"),
				Filename: objString(m, "filename"),
				Bytes:    objInt(m, "size_bytes"),
				Provider: files.ProviderAnthropic,
				Raw:      m,
			})
		}
		next, _ := raw["next_page"].(string)
		// next == page: a server repeating its cursor would loop forever.
		if next == "" || next == page {
			break
		}
		page = next
	}
	return out, nil
}

// Delete removes fileID; any non-2xx answer is an error.
func (c *FilesClient) Delete(ctx context.Context, fileID string) error {
	resp, err := c.do(ctx, http.MethodDelete, filesPath+"/"+url.PathEscape(fileID), "", acceptJSON, nil)
	if err != nil {
		return err
	}
	data, err := readAndClose(resp)
	if err != nil {
		return err
	}
	if !statusOK(resp.StatusCode) {
		return filesStatusErr(resp.StatusCode, data)
	}
	return nil
}

// doObject performs one request and decodes a JSON-object response,
// translating a non-2xx status into an ErrFilesAPI error.
func (c *FilesClient) doObject(ctx context.Context, method, reqPath, contentType, accept string, body []byte) (map[string]any, error) {
	resp, err := c.do(ctx, method, reqPath, contentType, accept, body)
	if err != nil {
		return nil, err
	}
	data, err := readAndClose(resp)
	if err != nil {
		return nil, err
	}
	if !statusOK(resp.StatusCode) {
		return nil, filesStatusErr(resp.StatusCode, data)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("anthropic: decoding files response: %w", err)
	}
	return m, nil
}

// readBytes fetches a binary endpoint (the /content download).
func (c *FilesClient) readBytes(ctx context.Context, reqPath string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, reqPath, "", acceptBinaryContent, nil)
	if err != nil {
		return nil, err
	}
	data, err := readAndClose(resp)
	if err != nil {
		return nil, err
	}
	if !statusOK(resp.StatusCode) {
		return nil, filesStatusErr(resp.StatusCode, data)
	}
	return data, nil
}

// do performs one Files API request with the transport retries: connection
// errors and 429/5xx are retried up to maxRetries times with exponential
// backoff, honoring a server-sent Retry-After (internal/httpx semantics).
func (c *FilesClient) do(ctx context.Context, method, reqPath, contentType, accept string, body []byte) (*http.Response, error) {
	client := c.http
	if client == nil {
		client = &http.Client{Timeout: c.timeout}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+reqPath, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("anthropic: files request: %w", err)
		}
		req.Header.Set("x-api-key", c.apiKey)
		req.Header.Set("anthropic-version", APIVersion)
		req.Header.Set("anthropic-beta", filesBeta)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if attempt < c.maxRetries && sleepCtx(ctx, retryBackoff(attempt)) == nil {
				continue
			}
			return nil, fmt.Errorf("anthropic: files request failed after %d attempt(s): %w", attempt+1, err)
		}
		if retryableStatus(resp.StatusCode) && attempt < c.maxRetries {
			delay, ok := retryAfter(resp.Header)
			if !ok {
				delay = retryBackoff(attempt)
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if sleepCtx(ctx, delay) == nil {
				continue
			}
			return resp, ctx.Err()
		}
		return resp, nil
	}
}

func statusOK(code int) bool { return code >= 200 && code < 300 }

func filesStatusErr(status int, data []byte) error {
	return fmt.Errorf("%w: returned %d: %s", ErrFilesAPI, status, snippet(data))
}

// readAndClose drains and closes the response body.
func readAndClose(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: reading files response: %w", err)
	}
	return data, nil
}

// objString/objInt read fields off a decoded response object. Numbers arrive
// as float64 (encoding/json); sizes and timestamps are integral.
func objString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func objInt(m map[string]any, key string) int64 {
	f, _ := m[key].(float64)
	return int64(f)
}

// snippet renders the first bytes of an error body for an error message.
func snippet(b []byte) string {
	const max = 512
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// Retry shaping mirrors internal/httpx: exponential base delays capped at
// 8s, a server-sent Retry-After capped at a minute.
var (
	retryBaseDelay = 500 * time.Millisecond
	maxRetryDelay  = 8 * time.Second
	maxRetryAfter  = time.Minute
)

func retryBackoff(attempt int) time.Duration {
	d := retryBaseDelay << attempt
	if d > maxRetryDelay || d <= 0 {
		return maxRetryDelay
	}
	return d
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

func retryAfter(h http.Header) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		d := time.Duration(secs * float64(time.Second))
		if d < 0 {
			d = 0
		}
		if d > maxRetryAfter {
			d = maxRetryAfter
		}
		return d, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		if d > maxRetryAfter {
			d = maxRetryAfter
		}
		return d, true
	}
	return 0, false
}

// sleepCtx waits d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// writeFilePart appends one multipart file part: disposition with the
// filename, content type guessed from the extension — both SDKs default a
// tuple without an explicit type to application/octet-stream.
func writeFilePart(mw *multipart.Writer, field, filename string, data []byte) error {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
		escapeQuotes.Replace(field), escapeQuotes.Replace(filename)))
	ct := mime.TypeByExtension(path.Ext(filename))
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = part.Write(data)
	return err
}

var escapeQuotes = strings.NewReplacer("\\", "\\\\", `"`, `\"`)
