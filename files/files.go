// Package files is the provider-agnostic Files API: the shared vocabulary
// (FileProvider, UploadedFile, FileContent), the Client contract every
// provider's files client satisfies, and API — the parity wrapper over
// Python's FilesAPI. Providers implement Client in their own packages
// (config/openai, config/anthropic); this package imports no config package —
// the dependency runs the other way, config/<provider> returns a files.Client.
//
// Parity with ag2/ag2/files (types.py, protocol.py, api.py).
package files

import (
	"context"
	"errors"
)

// FileProvider names the vendor storing the file (Python parity: the
// FileProvider str-enum).
type FileProvider string

// Providers with a Python files client (ag2/ag2/config/*/files.py). AGo ships
// clients for the first two; the rest name files stored by those providers.
const (
	ProviderOpenAI    FileProvider = "openai"
	ProviderAnthropic FileProvider = "anthropic"
	ProviderGemini    FileProvider = "gemini"
	ProviderMistral   FileProvider = "mistral"
	ProviderXAI       FileProvider = "xai"
	ProviderZAI       FileProvider = "zai"
)

// UploadedFile is one file stored at a provider (Python parity:
// files.types.UploadedFile).
//
// AGO-DEVIATION: the fields Python inherits from FileIdInput beyond
// id/filename (created_at) and the Anthropic metadata (mime_type, size_bytes)
// have no typed field — the provider's raw response object in Raw carries
// them (brief contract; recorded in DECISIONS D-046).
type UploadedFile struct {
	ID       string
	Filename string
	// Bytes is the stored size in bytes; 0 when the provider does not report one.
	Bytes int64
	// Purpose is the upload purpose (OpenAI: "assistants", "fine-tune", ...);
	// empty where the provider has none (Anthropic list entries).
	Purpose  string
	Provider FileProvider
	// Raw is the provider's response object as received.
	Raw map[string]any
}

// FileContent is a downloaded file (Python parity: files.types.FileContent —
// name/data/media_type).
//
// AGO-DEVIATION: the provider-reported media type (Anthropic: mime_type of
// the metadata) has no typed field — Raw carries the metadata object it came
// from (brief contract; recorded in DECISIONS D-046).
type FileContent struct {
	Data     []byte
	Filename string
	Provider FileProvider
	// Raw is the metadata response the content was resolved against.
	Raw map[string]any
}

// Client is one provider's Files API (Python parity:
// files.protocol.FilesClient). Implementations are safe for concurrent use.
type Client interface {
	// Upload stores data under filename for purpose (empty = provider
	// default) and returns the stored file's metadata.
	Upload(ctx context.Context, data []byte, filename, purpose string) (UploadedFile, error)
	// Read downloads the full content of fileID.
	Read(ctx context.Context, fileID string) (FileContent, error)
	// List returns every file at the provider, following pagination.
	List(ctx context.Context) ([]UploadedFile, error)
	// Delete removes fileID.
	Delete(ctx context.Context, fileID string) error
}

// Errors reported by API. Match with errors.Is.
var (
	// ErrNoData reports Upload without data (Python parity: "Either 'path' or
	// 'data' must be provided." — AGo has no path parameter, so nil data is
	// the only way to hit it).
	ErrNoData = errors.New("files: no data to upload")
	// ErrFilenameRequired reports Upload with an empty filename (Python
	// parity: "'filename' is required when using 'data' without 'path'.").
	ErrFilenameRequired = errors.New("files: filename is required")
	// ErrNoClient reports an API built without a client.
	ErrNoClient = errors.New("files: no client")
)

// API delegates to one provider's Client — parity with files.api.FilesAPI.
//
// AGO-DEVIATION: Python takes path= OR data= with a required filename; AGo
// takes bytes only (the caller reads disk files itself) and keeps the
// wrapper's two upload validations as sentinels (recorded in DECISIONS
// D-046). Safe for concurrent use.
type API struct {
	client Client
}

// API satisfies the same contract it delegates to (Python parity: FilesAPI
// implements FilesClient).
var _ Client = (*API)(nil)

// NewAPI returns an API delegating to client.
func NewAPI(client Client) *API { return &API{client: client} }

// Upload validates the arguments the Python wrapper validates, then
// delegates.
func (a *API) Upload(ctx context.Context, data []byte, filename, purpose string) (UploadedFile, error) {
	if a.client == nil {
		return UploadedFile{}, ErrNoClient
	}
	if data == nil {
		return UploadedFile{}, ErrNoData
	}
	if filename == "" {
		return UploadedFile{}, ErrFilenameRequired
	}
	return a.client.Upload(ctx, data, filename, purpose)
}

// Read downloads file content by ID.
func (a *API) Read(ctx context.Context, fileID string) (FileContent, error) {
	if a.client == nil {
		return FileContent{}, ErrNoClient
	}
	return a.client.Read(ctx, fileID)
}

// List returns every file at the provider.
func (a *API) List(ctx context.Context) ([]UploadedFile, error) {
	if a.client == nil {
		return nil, ErrNoClient
	}
	return a.client.List(ctx)
}

// Delete removes a file by ID.
func (a *API) Delete(ctx context.Context, fileID string) error {
	if a.client == nil {
		return ErrNoClient
	}
	return a.client.Delete(ctx, fileID)
}
