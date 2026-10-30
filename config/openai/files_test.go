package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/files"
)

// filesTestClient builds a FilesClient against srv without transport retries
// (handlers below count requests; a retry would corrupt the counts).
func filesTestClient(srv *httptest.Server) *FilesClient {
	return &FilesClient{apiKey: "test-key", baseURL: srv.URL, http: srv.Client()}
}

func TestOpenAIFilesUpload(t *testing.T) {
	tests := []struct {
		name        string
		purpose     string
		wantPurpose string // what the server must receive
	}{
		{name: "default purpose", purpose: "", wantPurpose: DefaultUploadPurpose},
		{name: "explicit purpose", purpose: "fine-tune", wantPurpose: "fine-tune"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/files" {
					t.Errorf("request = %s %s, want POST /files", r.Method, r.URL.Path)
				}
				gotAuth = r.Header.Get("Authorization")
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("ParseMultipartForm: %v", err)
					return
				}
				if got := r.FormValue("purpose"); got != tc.wantPurpose {
					t.Errorf("purpose field = %q, want %q", got, tc.wantPurpose)
				}
				file, header, err := r.FormFile("file")
				if err != nil {
					t.Errorf("FormFile(file): %v", err)
					return
				}
				defer file.Close()
				if header.Filename != "doc.pdf" {
					t.Errorf("file part filename = %q, want doc.pdf", header.Filename)
				}
				if ct := header.Header.Get("Content-Type"); ct != "application/pdf" {
					t.Errorf("file part content type = %q, want application/pdf", ct)
				}
				data, err := io.ReadAll(file)
				if err != nil || !bytes.Equal(data, []byte("pdf-data")) {
					t.Errorf("file content = %q (err %v), want pdf-data", data, err)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"file-abc","object":"file","filename":"doc.pdf","bytes":2048,"purpose":"assistants","created_at":1700000000}`)
			}))
			defer srv.Close()

			got, err := filesTestClient(srv).Upload(context.Background(), []byte("pdf-data"), "doc.pdf", tc.purpose)
			if err != nil {
				t.Fatalf("Upload: %v", err)
			}
			if gotAuth != "Bearer test-key" {
				t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
			}
			want := files.UploadedFile{
				ID:       "file-abc",
				Filename: "doc.pdf",
				Bytes:    2048,
				Purpose:  "assistants",
				Provider: files.ProviderOpenAI,
			}
			if got.ID != want.ID || got.Filename != want.Filename || got.Bytes != want.Bytes ||
				got.Purpose != want.Purpose || got.Provider != want.Provider {
				t.Errorf("Upload = %+v, want %+v", got, want)
			}
			if raw, ok := got.Raw["created_at"].(float64); !ok || raw != 1700000000 {
				t.Errorf("Raw[created_at] = %v, want 1700000000", got.Raw["created_at"])
			}
		})
	}
}

func TestOpenAIFilesUploadUnknownExtension(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("FormFile(file): %v", err)
			return
		}
		defer file.Close()
		if ct := header.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("content type = %q, want application/octet-stream", ct)
		}
		fmt.Fprint(w, `{"id":"file-1","filename":"data.blob","bytes":3,"purpose":"assistants"}`)
	}))
	defer srv.Close()

	got, err := filesTestClient(srv).Upload(context.Background(), []byte("abc"), "data.blob", "")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got.Filename != "data.blob" {
		t.Errorf("filename = %q", got.Filename)
	}
}

func TestOpenAIFilesRead(t *testing.T) {
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.URL.Path)
		switch r.URL.Path {
		case "/files/file-1/content":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("file-bytes"))
		case "/files/file-1":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"file-1","object":"file","filename":"doc.pdf","bytes":10,"purpose":"assistants","created_at":1700000000}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	got, err := filesTestClient(srv).Read(context.Background(), "file-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := files.FileContent{Data: []byte("file-bytes"), Filename: "doc.pdf", Provider: files.ProviderOpenAI}
	if got.Data == nil || !bytes.Equal(got.Data, want.Data) || got.Filename != want.Filename || got.Provider != want.Provider {
		t.Errorf("Read = %+v, want %+v", got, want)
	}
	if got.Raw["filename"] != "doc.pdf" {
		t.Errorf("Raw[filename] = %v", got.Raw["filename"])
	}
	// Python parity: content first, then the metadata lookup.
	if len(order) != 2 || order[0] != "/files/file-1/content" || order[1] != "/files/file-1" {
		t.Errorf("request order = %v, want content then metadata", order)
	}
}

func TestOpenAIFilesListPagination(t *testing.T) {
	tests := []struct{ pageCount int }{{0}, {1}, {3}}
	for _, tc := range tests {
		name := fmt.Sprintf("pages=%d", tc.pageCount)
		t.Run(name, func(t *testing.T) {
			var queries []string
			count := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count++
				queries = append(queries, r.URL.RawQuery)
				page := count
				data := "[]"
				if tc.pageCount > 0 {
					data = fmt.Sprintf(`[{"id":"file-%d","object":"file","filename":"%d.txt","bytes":%d,"purpose":"assistants","created_at":1700000000}]`, page, page, page)
				}
				hasMore := "false"
				if page < tc.pageCount {
					hasMore = "true"
				}
				lastID := "null"
				if data != "" {
					lastID = fmt.Sprintf(`"file-%d"`, page)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"object":"list","data":%s,"has_more":%s,"last_id":%s}`, data, hasMore, lastID)
			}))
			defer srv.Close()

			got, err := filesTestClient(srv).List(context.Background())
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != tc.pageCount {
				t.Fatalf("List returned %d files, want %d", len(got), tc.pageCount)
			}
			for i, f := range got {
				page := i + 1
				if f.ID != fmt.Sprintf("file-%d", page) || f.Filename != fmt.Sprintf("%d.txt", page) ||
					f.Bytes != int64(page) || f.Purpose != "assistants" || f.Provider != files.ProviderOpenAI {
					t.Errorf("file[%d] = %+v, want file-%d", i, f, page)
				}
				if f.Raw["created_at"] != float64(1700000000) {
					t.Errorf("file[%d].Raw[created_at] = %v", i, f.Raw["created_at"])
				}
			}
			// First page without params, then ?after=<previous last_id>.
			wantQueries := []string{""}
			for page := 1; page < tc.pageCount; page++ {
				wantQueries = append(wantQueries, fmt.Sprintf("after=file-%d", page))
			}
			if fmt.Sprint(queries) != fmt.Sprint(wantQueries) {
				t.Errorf("queries = %v, want %v", queries, wantQueries)
			}
		})
	}
}

func TestOpenAIFilesDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"file-abc","object":"file","deleted":true}`)
	}))
	defer srv.Close()

	if err := filesTestClient(srv).Delete(context.Background(), "file-abc"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/files/file-abc" {
		t.Errorf("request = %s %s, want DELETE /files/file-abc", gotMethod, gotPath)
	}
}

func TestOpenAIFilesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"message":"No such file: file-x","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()

	c := filesTestClient(srv)
	ctx := context.Background()
	if _, err := c.Read(ctx, "file-x"); !errors.Is(err, ErrFilesAPI) {
		t.Errorf("Read: %v, want ErrFilesAPI", err)
	}
	if _, err := c.List(ctx); !errors.Is(err, ErrFilesAPI) {
		t.Errorf("List: %v, want ErrFilesAPI", err)
	}
	if err := c.Delete(ctx, "file-x"); !errors.Is(err, ErrFilesAPI) {
		t.Errorf("Delete: %v, want ErrFilesAPI", err)
	}
	if _, err := c.Upload(ctx, []byte("x"), "a.txt", ""); !errors.Is(err, ErrFilesAPI) {
		t.Errorf("Upload: %v, want ErrFilesAPI", err)
	}
}

func TestOpenAICreateFilesClient(t *testing.T) {
	t.Run("missing key", func(t *testing.T) {
		t.Setenv(envAPIKey, "")
		cfg := NewOpenAICompletions("gpt-test")
		if _, err := cfg.CreateFilesClient(); !errors.Is(err, config.ErrNotProvided) {
			t.Fatalf("err = %v, want config.ErrNotProvided", err)
		}
	})
	t.Run("env fallback", func(t *testing.T) {
		t.Setenv(envAPIKey, "env-key")
		cfg := NewOpenAICompletions("gpt-test")
		fc, err := cfg.CreateFilesClient()
		if err != nil {
			t.Fatalf("CreateFilesClient: %v", err)
		}
		impl, ok := fc.(*FilesClient)
		if !ok {
			t.Fatalf("CreateFilesClient returned %T", fc)
		}
		if impl.apiKey != "env-key" {
			t.Errorf("apiKey = %q, want env-key", impl.apiKey)
		}
		if impl.baseURL != DefaultBaseURL {
			t.Errorf("baseURL = %q, want %q", impl.baseURL, DefaultBaseURL)
		}
	})
	t.Run("explicit fields", func(t *testing.T) {
		cfg := NewOpenAICompletions("gpt-test")
		cfg.APIKey = "explicit"
		cfg.BaseURL = "http://proxy.test/v1/"
		cfg.Organization = "org-1"
		cfg.Project = "proj-1"
		cfg.MaxRetries = 2
		cfg.Timeout = 5 * time.Second
		cfg.DefaultHeaders = map[string]string{"X-Custom": "yes"}
		client, err := cfg.CreateFilesClient()
		if err != nil {
			t.Fatalf("CreateFilesClient: %v", err)
		}
		fc := client.(*FilesClient)
		if fc.apiKey != "explicit" {
			t.Errorf("apiKey = %q, want explicit", fc.apiKey)
		}
		if fc.baseURL != "http://proxy.test/v1" {
			t.Errorf("baseURL = %q, want trailing slash trimmed", fc.baseURL)
		}
		if fc.organization != "org-1" || fc.project != "proj-1" {
			t.Errorf("org/project = %q/%q", fc.organization, fc.project)
		}
		if fc.maxRetries != 2 || fc.timeout != 5*time.Second {
			t.Errorf("maxRetries/timeout = %d/%v", fc.maxRetries, fc.timeout)
		}
		if fc.defaultHeaders["X-Custom"] != "yes" {
			t.Errorf("defaultHeaders = %v", fc.defaultHeaders)
		}
	})
	t.Run("implements files.Client", func(t *testing.T) {
		t.Setenv(envAPIKey, "env-key")
		var _ files.Client = mustFilesClient(t, NewOpenAICompletions("gpt-test"))
	})
}

func mustFilesClient(t *testing.T, cfg *OpenAICompletionsConfig) files.Client {
	t.Helper()
	fc, err := cfg.CreateFilesClient()
	if err != nil {
		t.Fatalf("CreateFilesClient: %v", err)
	}
	return fc
}
