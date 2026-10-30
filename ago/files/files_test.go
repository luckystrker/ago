package files

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// fakeClient records what API passes through.
type fakeClient struct {
	called string

	uploadData     []byte
	uploadFilename string
	uploadPurpose  string
	uploadResult   UploadedFile
	uploadErr      error

	readID     string
	readResult FileContent
	readErr    error

	listResult []UploadedFile
	listErr    error

	deletedID string
	deleteErr error
}

func (f *fakeClient) Upload(_ context.Context, data []byte, filename, purpose string) (UploadedFile, error) {
	f.called = "upload"
	f.uploadData, f.uploadFilename, f.uploadPurpose = data, filename, purpose
	return f.uploadResult, f.uploadErr
}

func (f *fakeClient) Read(_ context.Context, fileID string) (FileContent, error) {
	f.called = "read"
	f.readID = fileID
	return f.readResult, f.readErr
}

func (f *fakeClient) List(_ context.Context) ([]UploadedFile, error) {
	f.called = "list"
	return f.listResult, f.listErr
}

func (f *fakeClient) Delete(_ context.Context, fileID string) error {
	f.called = "delete"
	f.deletedID = fileID
	return f.deleteErr
}

func TestAPIUploadDelegates(t *testing.T) {
	want := UploadedFile{ID: "file-1", Filename: "a.txt", Provider: ProviderOpenAI}
	fc := &fakeClient{uploadResult: want}
	api := NewAPI(fc)

	data := []byte("hello")
	got, err := api.Upload(context.Background(), data, "a.txt", "assistants")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if fc.called != "upload" {
		t.Fatalf("client not called, called=%q", fc.called)
	}
	if !reflect.DeepEqual(fc.uploadData, data) || fc.uploadFilename != "a.txt" || fc.uploadPurpose != "assistants" {
		t.Fatalf("args not passed through: data=%q filename=%q purpose=%q", fc.uploadData, fc.uploadFilename, fc.uploadPurpose)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Upload = %+v, want %+v", got, want)
	}
}

func TestAPIReadListDeleteDelegate(t *testing.T) {
	readWant := FileContent{Data: []byte("x"), Filename: "a.txt", Provider: ProviderAnthropic}
	listWant := []UploadedFile{{ID: "file-1"}}

	fc := &fakeClient{readResult: readWant, listResult: listWant}
	api := NewAPI(fc)

	got, err := api.Read(context.Background(), "file-1")
	if err != nil || fc.readID != "file-1" || !reflect.DeepEqual(got, readWant) {
		t.Fatalf("Read: got=%+v id=%q err=%v", got, fc.readID, err)
	}
	lst, err := api.List(context.Background())
	if err != nil || !reflect.DeepEqual(lst, listWant) {
		t.Fatalf("List: got=%+v err=%v", lst, err)
	}
	if err := api.Delete(context.Background(), "file-9"); err != nil || fc.deletedID != "file-9" {
		t.Fatalf("Delete: id=%q err=%v", fc.deletedID, err)
	}
}

func TestAPIUploadValidation(t *testing.T) {
	fc := &fakeClient{}
	api := NewAPI(fc)

	if _, err := api.Upload(context.Background(), nil, "a.txt", ""); !errors.Is(err, ErrNoData) {
		t.Fatalf("nil data: err=%v, want ErrNoData", err)
	}
	if _, err := api.Upload(context.Background(), []byte("x"), "", ""); !errors.Is(err, ErrFilenameRequired) {
		t.Fatalf("empty filename: err=%v, want ErrFilenameRequired", err)
	}
	if fc.called != "" {
		t.Fatalf("client called on rejected upload (%q)", fc.called)
	}
	// An empty but non-nil slice is a valid (empty) file — Python parity with
	// b"" passing the `data is None` check.
	if _, err := api.Upload(context.Background(), []byte{}, "empty.txt", ""); err != nil {
		t.Fatalf("empty file: %v", err)
	}
}

func TestAPINilClient(t *testing.T) {
	api := NewAPI(nil)
	ctx := context.Background()
	if _, err := api.Upload(ctx, []byte("x"), "a.txt", ""); !errors.Is(err, ErrNoClient) {
		t.Fatalf("Upload: %v, want ErrNoClient", err)
	}
	if _, err := api.Read(ctx, "f"); !errors.Is(err, ErrNoClient) {
		t.Fatalf("Read: %v, want ErrNoClient", err)
	}
	if _, err := api.List(ctx); !errors.Is(err, ErrNoClient) {
		t.Fatalf("List: %v, want ErrNoClient", err)
	}
	if err := api.Delete(ctx, "f"); !errors.Is(err, ErrNoClient) {
		t.Fatalf("Delete: %v, want ErrNoClient", err)
	}
}

func TestAPIPassesThroughClientErrors(t *testing.T) {
	boom := errors.New("boom")
	api := NewAPI(&fakeClient{uploadErr: boom, readErr: boom, listErr: boom, deleteErr: boom})
	ctx := context.Background()
	if _, err := api.Upload(ctx, []byte("x"), "a.txt", ""); !errors.Is(err, boom) {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := api.Read(ctx, "f"); !errors.Is(err, boom) {
		t.Fatalf("Read: %v", err)
	}
	if _, err := api.List(ctx); !errors.Is(err, boom) {
		t.Fatalf("List: %v", err)
	}
	if err := api.Delete(ctx, "f"); !errors.Is(err, boom) {
		t.Fatalf("Delete: %v", err)
	}
}
