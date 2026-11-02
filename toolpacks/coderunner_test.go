package toolpacks

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// TestCodeRunnerOptionsValidation pins the option guards: empty names, missing
// workdirs, non-positive limits and empty env keys are construction errors,
// their valid counterparts pass and land on the runner.
func TestCodeRunnerOptionsValidation(t *testing.T) {
	if _, err := NewLocalCodeRunner(WithCodeName("")); err == nil {
		t.Fatal("WithCodeName(\"\") must be refused")
	}
	named, err := NewLocalCodeRunner(WithCodeName("run_my_code"))
	if err != nil {
		t.Fatalf("named runner refused: %v", err)
	}
	defer named.Close()
	if named.Name() != "run_my_code" {
		t.Fatalf("tool name = %q, want run_my_code", named.Name())
	}

	dir := t.TempDir()
	w, err := NewLocalCodeRunner(WithCodeWorkdir(dir))
	if err != nil {
		t.Fatalf("workdir option refused: %v", err)
	}
	defer w.Close()
	if w.Workdir() == "" || !strings.EqualFold(w.Workdir(), dir) {
		t.Fatalf("Workdir = %q, want %q", w.Workdir(), dir)
	}
	if _, err := NewLocalCodeRunner(WithCodeWorkdir(filepath.Join(dir, "missing-subdir"))); err == nil {
		t.Fatal("a missing workdir must be refused")
	}

	if _, err := NewLocalCodeRunner(WithCodeTimeout(0)); err == nil {
		t.Fatal("WithCodeTimeout(0) must be refused")
	}
	if _, err := NewLocalCodeRunner(WithCodeEnv(map[string]string{"": "v"})); err == nil {
		t.Fatal("an empty env key must be refused")
	}
	envOk, err := NewLocalCodeRunner(WithCodeEnv(map[string]string{"AGO_TEST_VAR": "v"}))
	if err != nil {
		t.Fatalf("valid env refused: %v", err)
	}
	defer envOk.Close()
	if _, err := NewLocalCodeRunner(WithCodeMaxOutput(0)); err == nil {
		t.Fatal("WithCodeMaxOutput(0) must be refused")
	}
	if _, err := NewLocalCodeRunner(WithCodePythonBin("")); err == nil {
		t.Fatal("WithCodePythonBin(\"\") must be refused")
	}
	py, err := NewLocalCodeRunner(WithCodePythonBin("python"))
	if err != nil {
		t.Fatalf("valid python binary refused: %v", err)
	}
	defer py.Close()
}

// TestCodeRunnerUndecodableOutput is the adapter-level regression (Python
// parity: test_code_adapter.py / test_shell_adapter.py, errors="replace"):
// stderr bytes the host text layer cannot decode must lose neither the text
// nor the exit code, and at the JSON boundary the invalid byte surfaces as
// U+FFFD — the form the model actually sees.
func TestCodeRunnerUndecodableOutput(t *testing.T) {
	cases := []struct {
		name     string
		language string
		code     string
		skip     string
	}{
		{
			name:     "bash",
			language: "bash",
			code:     "printf 'error: \\201' >&2\nexit 7",
			skip:     "no working bash/sh on PATH",
		},
		{
			name:     "python",
			language: "python",
			code:     "import os\nos.write(2, b'error: \\x81')\nraise SystemExit(7)",
			skip:     "no python interpreter on PATH",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			switch tc.language {
			case "bash":
				if findShell() == "" {
					t.Skip(tc.skip)
				}
			default:
				if !pythonAvailable() {
					t.Skip(tc.skip)
				}
			}
			r, err := NewLocalCodeRunner()
			if err != nil {
				t.Fatalf("NewLocalCodeRunner: %v", err)
			}
			defer r.Close()
			_, cx := newTestContext()
			ev := callTool(t, r, cx, r.Name(),
				`{"code":`+quoteGo(tc.code)+`,"language":"`+tc.language+`"}`)
			got := resultPartText(t, ev)
			if !strings.Contains(got, "error: ") {
				t.Fatalf("output = %q, want the stderr text around the invalid byte", got)
			}
			if !strings.Contains(got, "[exit code: 7]") {
				t.Fatalf("output = %q, want [exit code: 7]", got)
			}
			// The invalid 0x81 byte is not valid UTF-8, so marshaling the result
			// text replaces it with the escaped U+FFFD (the Go twin of
			// errors="replace"): neither the text nor the exit code was lost to
			// a decode failure.
			j, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal result text: %v", err)
			}
			if !strings.Contains(string(j), `\ufffd`) {
				t.Fatalf("JSON result %s does not carry U+FFFD in place of the 0x81 byte", j)
			}
		})
	}
}

// TestCodeRunnerJavaScript pins the js runner: a javascript snippet runs
// through node via a script file written into the workdir (Python parity:
// DEFAULT_RUNNERS javascript).
func TestCodeRunnerJavaScript(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node interpreter on PATH")
	}
	r, err := NewLocalCodeRunner(WithCodeLanguages("javascript"))
	if err != nil {
		t.Fatalf("NewLocalCodeRunner: %v", err)
	}
	defer r.Close()
	_, cx := newTestContext()
	ev := callTool(t, r, cx, r.Name(), `{"code":"console.log('js_ok')","language":"javascript"}`)
	if got := resultPartText(t, ev); got != "js_ok" {
		t.Fatalf("output = %q, want js_ok", got)
	}
}

// TestFindShellFindsWorkingBash pins shell discovery by execution: when any
// candidate directory really contains a bash that runs, findShell must return
// one — a LookPath hit alone is not enough on Windows, where the WSL launcher
// stub pretends to be bash.
func TestFindShellFindsWorkingBash(t *testing.T) {
	// Independent proof: run each candidate directly, bypassing findShell and
	// its helpers, so a broken probe cannot hide behind the skip guard.
	working := ""
	for _, dir := range shellDirs() {
		name := "bash"
		if runtime.GOOS == "windows" {
			name = "bash.exe"
		}
		p := filepath.Join(dir, name)
		if exec.Command(p, "-c", "exit 0").Run() == nil {
			working = p
			break
		}
	}
	if working == "" {
		t.Skip("no independently working bash on this machine")
	}
	if got := findShell(); got == "" {
		t.Fatalf("findShell returned empty although %s executes", working)
	}
}

// TestShellDirsGitInstallFirst pins the search order: on Windows the shell
// directories of the Git install are searched ahead of PATH (where the WSL
// launcher stub in System32 would win), and the PATH entries stay the fallback.
func TestShellDirsGitInstallFirst(t *testing.T) {
	dirs := shellDirs()
	path := filepath.SplitList(os.Getenv("PATH"))
	if runtime.GOOS != "windows" {
		// Off Windows only the PATH directories are searched, in order.
		if len(dirs) != len(path) {
			t.Fatalf("shellDirs = %d entries, want exactly the %d PATH entries", len(dirs), len(path))
		}
		for i := range path {
			if dirs[i] != path[i] {
				t.Fatalf("shellDirs[%d] = %q, want the PATH entry %q", i, dirs[i], path[i])
			}
		}
		return
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH; the PATH fallback order is not pinned without it")
	}
	usrBin := filepath.Join(filepath.Dir(filepath.Dir(git)), "usr", "bin")
	if fi, err := os.Stat(usrBin); err == nil && fi.IsDir() {
		if len(dirs) == 0 || dirs[0] != usrBin {
			t.Fatalf("shellDirs[0] = %q, want the Git install dir %q ahead of PATH", dirs[0], usrBin)
		}
	}
	// The PATH entries remain the fallback behind the Git install dirs.
	if len(dirs) <= len(path) {
		t.Fatalf("shellDirs lost the PATH fallback: %d entries vs %d PATH entries", len(dirs), len(path))
	}
}

// TestShellWorksRejectsFailedProbe pins the execution probe itself: a command
// that cannot run — a regular file that is no executable image, or a name not
// on PATH at all — must read as "does not work", not "works" (runLocalCommand
// reports those as a non-zero exit code with a nil error). This is the failure
// profile of the WSL launcher stub that findShell screens out.
func TestShellWorksRejectsFailedProbe(t *testing.T) {
	dir := t.TempDir()
	name := "broken-shell"
	if runtime.GOOS == "windows" {
		name = "broken-shell.exe"
	}
	broken := filepath.Join(dir, name)
	if err := os.WriteFile(broken, []byte("this is not an executable image"), 0o755); err != nil {
		t.Fatalf("write broken candidate: %v", err)
	}
	if shellWorks(broken) {
		t.Fatalf("shellWorks accepted %q: a file that exists but cannot start is not a working shell", broken)
	}
	if shellWorks("ago-no-such-shell-anywhere") {
		t.Fatal("shellWorks accepted a command that is not on PATH at all")
	}
}

// TestPickShellRejectsBrokenCandidate pins candidate selection at the unit
// level: a candidate that exists as a regular executable file but fails the
// run probe must be skipped in favor of a later working one — existence alone
// must not elect it (on Windows that is the WSL launcher stub sitting in PATH
// behind the Git install directories).
func TestPickShellRejectsBrokenCandidate(t *testing.T) {
	brokenDir := t.TempDir()
	name := "bash"
	if runtime.GOOS == "windows" {
		name = "bash.exe"
	}
	broken := filepath.Join(brokenDir, name)
	if err := os.WriteFile(broken, []byte("this is not an executable image"), 0o755); err != nil {
		t.Fatalf("write broken candidate: %v", err)
	}
	dirs := []string{brokenDir}
	if sh := findShell(); sh != "" {
		dirs = append(dirs, filepath.Dir(sh))
	}
	got := pickShell(dirs)
	if got == broken {
		t.Fatalf("pickShell elected %q although it fails the execution probe", broken)
	}
	if got == "" {
		if sh := findShell(); sh != "" {
			t.Fatalf("pickShell returned empty although %q works", sh)
		}
		return
	}
	if !shellWorks(got) {
		t.Fatalf("pickShell elected %q, which does not run", got)
	}
}

// TestFindPythonPicksPlatformOrder pins the interpreter discovery order: the
// first name of the platform's candidate list that LookPath resolves wins
// (Windows convention first: python, py, python3).
func TestFindPythonPicksPlatformOrder(t *testing.T) {
	names := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		names = []string{"python", "py", "python3"}
	}
	want := ""
	for _, n := range names {
		if _, err := exec.LookPath(n); err == nil {
			want = n
			break
		}
	}
	if want == "" {
		t.Skip("no python interpreter on PATH")
	}
	if got := findPython(); got != want {
		t.Fatalf("findPython = %q, want the first working candidate %q of %v", got, want, names)
	}
}
