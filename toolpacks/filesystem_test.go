package toolpacks

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luckystrker/ago/tools"
)

func newFS(t *testing.T) (*FilesystemToolkit, string) {
	t.Helper()
	root := t.TempDir()
	f, err := NewFilesystemToolkit(root)
	if err != nil {
		t.Fatalf("NewFilesystemToolkit: %v", err)
	}
	return f, root
}

func TestFilesystemWriteReadRoundtrip(t *testing.T) {
	f, _ := newFS(t)
	_, cx := newTestContext()

	ev := callTool(t, f, cx, "write_file", `{"path":"docs/note.txt","content":"héllo wörld"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "13 characters") {
		t.Fatalf("write result = %q, want the character count", got)
	}
	ev = callTool(t, f, cx, "read_file", `{"path":"docs/note.txt"}`)
	if got := resultPartText(t, ev); got != "héllo wörld" {
		t.Fatalf("read result = %q, want the written content (UTF-8 round trip)", got)
	}
}

func TestFilesystemListAndMkdir(t *testing.T) {
	f, root := newFS(t)
	_, cx := newTestContext()

	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	ev := callTool(t, f, cx, "list_directory", `{}`)
	var entries []string
	if err := json.Unmarshal([]byte(firstDataJSON(t, ev)), &entries); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if strings.Join(entries, "|") != "a.txt|sub/" {
		t.Fatalf("listing = %v, want a.txt then sub/ (directories with a trailing slash)", entries)
	}
	ev = callTool(t, f, cx, "make_directory", `{"path":"deep/nested/dir"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "deep/nested/dir") {
		t.Fatalf("mkdir result = %q", got)
	}
	if info, err := os.Stat(filepath.Join(root, "deep", "nested", "dir")); err != nil || !info.IsDir() {
		t.Fatalf("nested dir not created: %v", err)
	}
}

func TestFilesystemReadOnly(t *testing.T) {
	root := t.TempDir()
	f, err := NewFilesystemToolkit(root, WithFilesystemReadOnly())
	if err != nil {
		t.Fatalf("NewFilesystemToolkit(read-only): %v", err)
	}
	var names []string
	for _, tool := range f.Tools() {
		names = append(names, tool.Name())
	}
	got := strings.Join(names, "|")
	if got != "read_file|list_directory|find_files" {
		t.Fatalf("read-only tools = %q, want the read-only members only", got)
	}
	if strings.Contains(got, "write_file") || strings.Contains(got, "make_directory") {
		t.Fatalf("read-only toolkit registered a mutating tool: %q", got)
	}
}

func TestFilesystemGlob(t *testing.T) {
	f, root := newFS(t)
	_, cx := newTestContext()
	for _, p := range []string{"top.txt", "src/one.go", "src/deep/two.txt", "src/deep/three.txt"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ev := callTool(t, f, cx, "find_files", `{"pattern":"**/*.txt"}`)
	var txts []string
	if err := json.Unmarshal([]byte(firstDataJSON(t, ev)), &txts); err != nil {
		t.Fatalf("decode glob: %v", err)
	}
	if strings.Join(txts, "|") != "src/deep/three.txt|src/deep/two.txt|top.txt" {
		t.Fatalf("**/*.txt = %v, want the three files sorted", txts)
	}
	ev = callTool(t, f, cx, "find_files", `{"pattern":"*.go","path":"src"}`)
	var gos []string
	if err := json.Unmarshal([]byte(firstDataJSON(t, ev)), &gos); err != nil {
		t.Fatalf("decode glob: %v", err)
	}
	if strings.Join(gos, "|") != "src/one.go" {
		t.Fatalf("*.go under src = %v", gos)
	}
}

// TestFilesystemGlobRecursiveWildcardPrefix pins recursive patterns whose prefix
// carries wildcards (upstream 5125cb5dfc): "**" must only change segment
// recursion, never rewrite the prefix, so src*/**/*.go and packages/[aw]*/src/**
// keep matching. AGo already matches segments directly; this is regression-only.
func TestFilesystemGlobRecursiveWildcardPrefix(t *testing.T) {
	f, root := newFS(t)
	_, cx := newTestContext()
	for _, p := range []string{
		"packages/api/src/main.go",
		"packages/api/src/util/helper.go",
		"packages/web/src/app.go",
		"packages/nested/worker/src/job.go",
		"packages/api/tests/test_main.go",
		"src/root.go",
		"srcgen/deep/tool.go",
	} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "packages", "api", "src", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		pattern  string
		expected []string
	}{
		{
			pattern:  "src*/**/*.go",
			expected: []string{"src/root.go", "srcgen/deep/tool.go"},
		},
		{
			pattern: "packages/*/src/**",
			expected: []string{
				"packages/api/src/main.go",
				"packages/api/src/util/helper.go",
				"packages/web/src/app.go",
			},
		},
		{
			pattern: "packages/a?i/src/**",
			expected: []string{
				"packages/api/src/main.go",
				"packages/api/src/util/helper.go",
			},
		},
		{
			pattern: "packages/[aw]*/src/**",
			expected: []string{
				"packages/api/src/main.go",
				"packages/api/src/util/helper.go",
				"packages/web/src/app.go",
			},
		},
		{
			pattern: "packages/**/src/**",
			expected: []string{
				"packages/api/src/main.go",
				"packages/api/src/util/helper.go",
				"packages/nested/worker/src/job.go",
				"packages/web/src/app.go",
			},
		},
		{
			pattern: "**/src/**",
			expected: []string{
				"packages/api/src/main.go",
				"packages/api/src/util/helper.go",
				"packages/nested/worker/src/job.go",
				"packages/web/src/app.go",
				"src/root.go",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.pattern, func(t *testing.T) {
			args := `{"pattern":` + quoteGo(tc.pattern) + `}`
			ev := callTool(t, f, cx, "find_files", args)
			var got []string
			if err := json.Unmarshal([]byte(firstDataJSON(t, ev)), &got); err != nil {
				t.Fatalf("decode glob: %v", err)
			}
			if strings.Join(got, "|") != strings.Join(tc.expected, "|") {
				t.Fatalf("%s = %v, want %v", tc.pattern, got, tc.expected)
			}
		})
	}
}

func TestFilesystemGuard(t *testing.T) {
	f, root := newFS(t)
	_, cx := newTestContext()
	// A secret outside the root that the guard must never let through.
	outside := filepath.Join(filepath.Dir(root), "secret_packs.txt")
	const secretContent = "TOPSECRET_CONTENT_9137"
	if err := os.WriteFile(outside, []byte(secretContent), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	for _, tc := range []struct {
		tool, args string
	}{
		{"read_file", `{"path":"../secret_packs.txt"}`},
		{"read_file", `{"path":"` + quoteGo(outside) + `"}`},
		{"write_file", `{"path":"../escape.txt","content":"x"}`},
		{"make_directory", `{"path":"../rogue"}`},
		{"find_files", `{"pattern":"../*.txt"}`},
		{"list_directory", `{"path":".."}`},
	} {
		ev := callTool(t, f, cx, tc.tool, tc.args)
		got := resultPartText(t, ev)
		if !strings.Contains(got, "TOOL_ERROR") {
			t.Fatalf("%s %s = %q, want an error", tc.tool, tc.args, got)
		}
		if strings.Contains(got, secretContent) {
			t.Fatalf("%s %s leaked the guarded file: %q", tc.tool, tc.args, got)
		}
	}

	// A symlink inside the root pointing outside is refused too.
	if err := os.Symlink(outside, filepath.Join(root, "leak.txt")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	ev := callTool(t, f, cx, "read_file", `{"path":"leak.txt"}`)
	if got := resultPartText(t, ev); strings.Contains(got, secretContent) {
		t.Fatalf("symlink escape leaked: %q", got)
	}
}

func TestFilesystemToolkitShape(t *testing.T) {
	f, _ := newFS(t)
	if f.Name() != "filesystem_toolkit" {
		t.Fatalf("name = %q", f.Name())
	}
	var names []string
	for _, m := range f.Tools() {
		names = append(names, m.Name())
	}
	want := "read_file,write_file,list_directory,make_directory,find_files"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("members = %q, want %q", got, want)
	}
	_, cx := newTestContext()
	schemas, err := f.Schemas(cx)
	if err != nil || len(schemas) != 5 {
		t.Fatalf("schemas = %d, %v; want 5", len(schemas), err)
	}
	if !f.DeclaredInCode() {
		t.Fatal("toolkit must be code-declared")
	}
	var _ tools.Tool = f
}

func TestFilesystemRootValidation(t *testing.T) {
	if _, err := NewFilesystemToolkit(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root error = %v, want os.ErrNotExist", err)
	}
	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFilesystemToolkit(file); err == nil {
		t.Fatal("a file root must be rejected")
	}
}
