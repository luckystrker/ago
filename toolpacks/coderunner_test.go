package toolpacks

import (
	"os"
	"strings"
	"testing"
	"time"
)

// pythonAvailable reports whether a python interpreter can be found.
func pythonAvailable() bool {
	return findPython() != ""
}

func TestCodeRunnerPython(t *testing.T) {
	if !pythonAvailable() {
		t.Skip("no python interpreter on PATH")
	}
	r, err := NewLocalCodeRunner()
	if err != nil {
		t.Fatalf("NewLocalCodeRunner: %v", err)
	}
	defer r.Close()
	_, cx := newTestContext()
	ev := callTool(t, r, cx, r.Name(), `{"code":"print('py_ok')","language":"python"}`)
	if got := resultPartText(t, ev); got != "py_ok" {
		t.Fatalf("output = %q, want py_ok", got)
	}
}

// TestCodeRunnerShell is the skip-compatible fallback: the same assertions run
// through a shell script when python is not on PATH (and through python when it
// is, so both runners stay covered on any machine that has one of the two).
func TestCodeRunnerShell(t *testing.T) {
	r, err := NewLocalCodeRunner()
	if err != nil {
		t.Fatalf("NewLocalCodeRunner: %v", err)
	}
	defer r.Close()
	// The guard mirrors production discovery: a bash on PATH that cannot run
	// (the Windows WSL launcher stub) is not a shell to test against.
	if findShell() == "" {
		t.Skip("no working bash/sh on PATH")
	}
	_, cx := newTestContext()
	ev := callTool(t, r, cx, r.Name(), `{"code":"echo sh_ok","language":"bash"}`)
	if got := resultPartText(t, ev); got != "sh_ok" {
		t.Fatalf("output = %q, want sh_ok", got)
	}
}

func TestCodeRunnerExitCodeAndStderr(t *testing.T) {
	var code string
	switch {
	case pythonAvailable():
		code = "import sys\nprint('out_line')\nprint('err_line', file=sys.stderr)\nsys.exit(3)"
	default:
		if findShell() == "" {
			t.Skip("no python or working bash/sh on PATH")
		}
		code = "echo out_line\necho err_line 1>&2\nexit 3"
	}
	r, err := NewLocalCodeRunner()
	if err != nil {
		t.Fatalf("NewLocalCodeRunner: %v", err)
	}
	defer r.Close()
	lang := "bash"
	if pythonAvailable() {
		lang = "python"
	}
	_, cx := newTestContext()
	ev := callTool(t, r, cx, r.Name(),
		`{"code":`+quoteGo(code)+`,"language":"`+lang+`"}`)
	got := resultPartText(t, ev)
	if !strings.Contains(got, "out_line") || !strings.Contains(got, "err_line") {
		t.Fatalf("output = %q, want stdout and stderr combined", got)
	}
	if !strings.Contains(got, "[exit code: 3]") {
		t.Fatalf("output = %q, want [exit code: 3]", got)
	}
}

func TestCodeRunnerTimeout(t *testing.T) {
	var code, lang string
	if pythonAvailable() {
		code, lang = "import time; time.sleep(30)", "python"
	} else {
		if findShell() == "" {
			t.Skip("no python or working bash/sh on PATH")
		}
		code, lang = "sleep 30", "bash"
	}
	r, err := NewLocalCodeRunner(WithCodeTimeout(300 * time.Millisecond))
	if err != nil {
		t.Fatalf("NewLocalCodeRunner: %v", err)
	}
	defer r.Close()
	_, cx := newTestContext()
	ev := callTool(t, r, cx, r.Name(), `{"code":`+quoteGo(code)+`,"language":"`+lang+`"}`)
	got := resultPartText(t, ev)
	if !strings.Contains(got, "timed out") || !strings.Contains(got, "[exit code: 124]") {
		t.Fatalf("output = %q, want a timeout with exit code 124", got)
	}
}

func TestCodeRunnerUnsupportedLanguage(t *testing.T) {
	r, err := NewLocalCodeRunner()
	if err != nil {
		t.Fatalf("NewLocalCodeRunner: %v", err)
	}
	defer r.Close()
	_, cx := newTestContext()
	ev := callTool(t, r, cx, r.Name(), `{"code":"1","language":"cobol"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "Unsupported language") {
		t.Fatalf("output = %q, want an unsupported-language notice", got)
	}
}

func TestCodeRunnerLanguageRestriction(t *testing.T) {
	r, err := NewLocalCodeRunner(WithCodeLanguages("python"))
	if err != nil {
		t.Fatalf("NewLocalCodeRunner: %v", err)
	}
	defer r.Close()
	if _, err := NewLocalCodeRunner(WithCodeLanguages("python", "cobol")); err == nil {
		t.Fatal("WithCodeLanguages must reject unsupported languages")
	}
	_, cx := newTestContext()
	ev := callTool(t, r, cx, r.Name(), `{"code":"echo x","language":"bash"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "not enabled") {
		t.Fatalf("output = %q, want a not-enabled notice", got)
	}
}

func TestCodeRunnerDescriptionStatesNoIsolation(t *testing.T) {
	// The description is the model's contract (security audit 2026-10-31,
	// C-2): the local backend has no sandbox, so claiming one invites the
	// model to run destructive snippets it believes are disposable.
	r, err := NewLocalCodeRunner()
	if err != nil {
		t.Fatalf("NewLocalCodeRunner: %v", err)
	}
	defer r.Close()
	_, cx := newTestContext()
	schemas, err := r.Schemas(cx)
	if err != nil || len(schemas) != 1 {
		t.Fatalf("schemas = %v, %v", schemas, err)
	}
	sch, ok := schemas[0].(toolsSchema)
	if !ok {
		t.Fatalf("schema type %T", schemas[0])
	}
	if strings.Contains(strings.ToLower(sch.Description), "sandbox") {
		t.Fatalf("description claims a sandbox: %q", sch.Description)
	}
	for _, want := range []string{"without isolation", "not a security boundary", "human approval"} {
		if !strings.Contains(sch.Description, want) {
			t.Fatalf("description = %q, want it to contain %q", sch.Description, want)
		}
	}
}

func TestCodeRunnerWorkdirAndClose(t *testing.T) {
	dir := t.TempDir()
	r, err := NewLocalCodeRunner(WithCodeWorkdir(dir))
	if err != nil {
		t.Fatalf("NewLocalCodeRunner: %v", err)
	}
	if r.Workdir() == "" {
		t.Fatal("Workdir empty")
	}
	// A caller-supplied workdir survives Close.
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("caller workdir removed by Close: %v", err)
	}

	// An auto-created workdir is removed by Close.
	auto, err := NewLocalCodeRunner()
	if err != nil {
		t.Fatalf("NewLocalCodeRunner: %v", err)
	}
	target := auto.Workdir()
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("auto workdir missing: %v", err)
	}
	if err := auto.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("auto workdir survived Close: %v", err)
	}
}
