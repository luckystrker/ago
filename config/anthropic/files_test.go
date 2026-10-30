package anthropic

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

func TestAnthropicFilesUpload(t *testing.T) {
	t.Run("pdf filename", func(t *testing.T) {
		var (
			gotKey, gotVersion, gotBeta string
			gotPurpose                  string
			purposePresent              bool
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/v1/files" {
				t.Errorf("request = %s %s, want POST /v1/files", r.Method, r.URL.Path)
			}
			gotKey = r.Header.Get("x-api-key")
			gotVersion = r.Header.Get("anthropic-version")
			gotBeta = r.Header.Get("anthropic-beta")
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("ParseMultipartForm: %v", err)
				return
			}
			// Python parity: the purpose argument is never sent to the API.
			_, purposePresent = r.MultipartForm.Value["purpose"]
			gotPurpose = r.FormValue("purpose")
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Errorf("FormFile(file): %v", err)
				return
			}
			defer file.Close()
			if header.Filename != "document.pdf" {
				t.Errorf("file part filename = %q, want document.pdf", header.Filename)
			}
			if ct := header.Header.Get("Content-Type"); ct != "application/pdf" {
				t.Errorf("file part content type = %q, want application/pdf", ct)
			}
			data, err := io.ReadAll(file)
			if err != nil || !bytes.Equal(data, []byte("pdf-data")) {
				t.Errorf("file content = %q (err %v), want pdf-data", data, err)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"file-011","type":"file","filename":"document.pdf","mime_type":"application/pdf","size_bytes":1024000,"created_at":"2025-01-01T00:00:00Z"}`)
		}))
		defer srv.Close()

		got, err := filesTestClient(srv).Upload(context.Background(), []byte("pdf-data"), "document.pdf", "user_data")
		if err != nil {
			t.Fatalf("Upload: %v", err)
		}
		if gotKey != "test-key" || gotVersion != APIVersion || gotBeta != filesBeta {
			t.Errorf("headers: x-api-key=%q anthropic-version=%q anthropic-beta=%q", gotKey, gotVersion, gotBeta)
		}
		if purposePresent {
			t.Errorf("purpose field sent to the API (%q), want none", gotPurpose)
		}
		want := files.UploadedFile{
			ID:       "file-011",
			Filename: "document.pdf",
			Bytes:    1024000,
			Purpose:  "user_data", // echoed from the argument, Python parity
			Provider: files.ProviderAnthropic,
		}
		if got.ID != want.ID || got.Filename != want.Filename || got.Bytes != want.Bytes ||
			got.Purpose != want.Purpose || got.Provider != want.Provider {
			t.Errorf("Upload = %+v, want %+v", got, want)
		}
		if got.Raw["mime_type"] != "application/pdf" {
			t.Errorf("Raw[mime_type] = %v", got.Raw["mime_type"])
		}
	})
	t.Run("unknown extension", func(t *testing.T) {
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
			fmt.Fprint(w, `{"id":"file-1","filename":"data.blob","size_bytes":3,"created_at":"2025-01-01T00:00:00Z"}`)
		}))
		defer srv.Close()

		got, err := filesTestClient(srv).Upload(context.Background(), []byte("abc"), "data.blob", "")
		if err != nil {
			t.Fatalf("Upload: %v", err)
		}
		if got.Purpose != "" {
			t.Errorf("Purpose = %q, want empty", got.Purpose)
		}
	})
}

func TestAnthropicFilesRead(t *testing.T) {
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.URL.Path)
		switch r.URL.Path {
		case "/v1/files/file-011/content":
			w.Header().Set("Content-Type", "text/csv")
			_, _ = w.Write([]byte("file-data"))
		case "/v1/files/file-011":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"file-011","type":"file","filename":"output.csv","mime_type":"text/csv","size_bytes":9,"created_at":"2025-01-01T00:00:00Z"}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	got, err := filesTestClient(srv).Read(context.Background(), "file-011")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := files.FileContent{Data: []byte("file-data"), Filename: "output.csv", Provider: files.ProviderAnthropic}
	if !bytes.Equal(got.Data, want.Data) || got.Filename != want.Filename || got.Provider != want.Provider {
		t.Errorf("Read = %+v, want %+v", got, want)
	}
	if got.Raw["mime_type"] != "text/csv" {
		t.Errorf("Raw[mime_type] = %v, want text/csv", got.Raw["mime_type"])
	}
	// Python parity: download first, then the metadata lookup.
	if len(order) != 2 || order[0] != "/v1/files/file-011/content" || order[1] != "/v1/files/file-011" {
		t.Errorf("request order = %v, want content then metadata", order)
	}
}

func TestAnthropicFilesListPagination(t *testing.T) {
	tests := []struct{ pageCount int }{{0}, {1}, {3}}
	for _, tc := range tests {
		name := fmt.Sprintf("pages=%d", tc.pageCount)
		t.Run(name, func(t *testing.T) {
			var queries []string
			count := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count++
				page := count
				queries = append(queries, r.URL.RawQuery)
				data := "[]"
				if tc.pageCount > 0 {
					data = fmt.Sprintf(`[{"id":"file-%d","type":"file","filename":"output.csv","mime_type":"text/csv","size_bytes":9,"created_at":"2025-01-01T00:00:00Z"}]`, page)
				}
				next := "null"
				if page < tc.pageCount {
					next = fmt.Sprintf(`"file-%d"`, page)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"data":%s,"next_page":%s}`, data, next)
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
				if f.ID != fmt.Sprintf("file-%d", i+1) || f.Filename != "output.csv" ||
					f.Bytes != 9 || f.Purpose != "" || f.Provider != files.ProviderAnthropic {
					t.Errorf("file[%d] = %+v, want file-%d", i, f, i+1)
				}
				if f.Raw["mime_type"] != "text/csv" {
					t.Errorf("file[%d].Raw[mime_type] = %v", i, f.Raw["mime_type"])
				}
			}
			// First page without params, then ?page=<previous next_page>
			// (AG2 test parity).
			wantQueries := []string{""}
			for page := 1; page < tc.pageCount; page++ {
				wantQueries = append(wantQueries, fmt.Sprintf("page=file-%d", page))
			}
			if fmt.Sprint(queries) != fmt.Sprint(wantQueries) {
				t.Errorf("queries = %v, want %v", queries, wantQueries)
			}
		})
	}
}

func TestAnthropicFilesDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"file-011","type":"file","deleted":true}`)
	}))
	defer srv.Close()

	if err := filesTestClient(srv).Delete(context.Background(), "file-011"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/files/file-011" {
		t.Errorf("request = %s %s, want DELETE /v1/files/file-011", gotMethod, gotPath)
	}
}

func TestAnthropicFilesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"type":"error","error":{"type":"not_found_error","message":"file not found"}}`)
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

func TestAnthropicCreateFilesClient(t *testing.T) {
	t.Run("missing key", func(t *testing.T) {
		t.Setenv(APIKeyEnv, "")
		cfg := NewAnthropic("claude-test")
		if _, err := cfg.CreateFilesClient(); !errors.Is(err, config.ErrNotProvided) {
			t.Fatalf("err = %v, want config.ErrNotProvided", err)
		}
	})
	t.Run("defaults", func(t *testing.T) {
		t.Setenv(APIKeyEnv, "env-key")
		cfg := NewAnthropic("claude-test")
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
		// Python parity: files.py turns an unset timeout into 600s.
		if impl.timeout != 600*time.Second {
			t.Errorf("timeout = %v, want 600s", impl.timeout)
		}
		if impl.maxRetries != DefaultMaxRetries {
			t.Errorf("maxRetries = %d, want %d", impl.maxRetries, DefaultMaxRetries)
		}
	})
	t.Run("explicit timeout and base URL", func(t *testing.T) {
		cfg := NewAnthropic("claude-test")
		cfg.APIKey = "explicit"
		cfg.BaseURL = "http://proxy.test/"
		cfg.Timeout = 5 * time.Second
		cfg.MaxRetries = 0
		client, err := cfg.CreateFilesClient()
		if err != nil {
			t.Fatalf("CreateFilesClient: %v", err)
		}
		fc := client.(*FilesClient)
		if fc.baseURL != "http://proxy.test" {
			t.Errorf("baseURL = %q, want trailing slash trimmed", fc.baseURL)
		}
		if fc.timeout != 5*time.Second {
			t.Errorf("timeout = %v, want 5s", fc.timeout)
		}
	})
	t.Run("implements files.Client", func(t *testing.T) {
		t.Setenv(APIKeyEnv, "env-key")
		fc, err := NewAnthropic("claude-test").CreateFilesClient()
		if err != nil {
			t.Fatalf("CreateFilesClient: %v", err)
		}
		var _ files.Client = fc
	})
}
