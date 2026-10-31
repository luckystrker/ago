package toolpacks

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// envDumpCmd returns a command whose combined output lists the child's whole
// environment: `cmd /c set` on Windows, `env` elsewhere.
func envDumpCmd() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "set"}
	}
	return []string{"env"}
}

// secretProbeCmd prints "unset" when the child does not have name and "leaked"
// when it does — output independent of the shell's message locale.
func secretProbeCmd(name string) string {
	if runtime.GOOS == "windows" {
		return "if defined " + name + " (echo leaked) else (echo unset)"
	}
	return "echo ${" + name + ":-unset}"
}

// secretPrintCmd prints the raw value of name (only used when the variable is
// expected to be present, so the name=value line needs no locale wording).
func secretPrintCmd(name string) string {
	if runtime.GOOS == "windows" {
		return "set " + name
	}
	return "echo ${" + name + ":-unset}"
}

// secretValue is a canary planted in the parent environment: it looks like an
// API key and must never reach a subprocess unless the test opted in.
const secretValue = "ago-sk-test-canary"

func TestFilterWhitelistedEnv(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/u",
		"LOCALAPPDATA=C:\\Users\\u\\AppData\\Local",
		"TMPDIR=/tmp", // near-miss: only TEMP/TMP are whitelisted
		"OPENAI_API_KEY=sk-leak",
		"ANTHROPIC_API_KEY=sk-leak",
	}
	got := filterWhitelistedEnv(parent)
	kept := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		kept[k] = v
	}
	if kept["PATH"] != "/usr/bin:/bin" || kept["HOME"] != "/home/u" {
		t.Fatalf("whitelisted vars dropped: %v", got)
	}
	if kept["LOCALAPPDATA"] == "" {
		t.Fatalf("LOCALAPPDATA dropped: %v", got)
	}
	if _, ok := kept["TMPDIR"]; ok {
		t.Fatalf("TMPDIR passed the whitelist: %v", got)
	}
	if _, ok := kept["OPENAI_API_KEY"]; ok {
		t.Fatalf("API key passed the whitelist: %v", got)
	}
	if _, ok := kept["ANTHROPIC_API_KEY"]; ok {
		t.Fatalf("API key passed the whitelist: %v", got)
	}
	if runtime.GOOS == "windows" {
		// Windows stores names case-insensitively (Path, SystemRoot); the
		// filter must match them the same way.
		if got := filterWhitelistedEnv([]string{"SystemRoot=C:\\Windows"}); len(got) != 1 || got[0] != "SystemRoot=C:\\Windows" {
			t.Fatalf("case-insensitive name not matched on Windows: %v", got)
		}
	}
}

func TestRunLocalCommandEnvWhitelistDefault(t *testing.T) {
	t.Setenv("AGO_TEST_SECRET", secretValue)
	t.Setenv("HOME", "ago-test-home") // whitelisted var with a known value
	res := runLocalCommand(envDumpCmd(), "", nil, false, 10*time.Second, defaultMaxOutput)
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, output = %q", res.ExitCode, res.Output)
	}
	if strings.Contains(res.Output, secretValue) {
		t.Fatalf("secret leaked to the child by default: %q", res.Output)
	}
	if !strings.Contains(res.Output, "ago-test-home") {
		t.Fatalf("whitelisted HOME not passed to the child: %q", res.Output)
	}
}

func TestRunLocalCommandEnvInheritAllOptIn(t *testing.T) {
	t.Setenv("AGO_TEST_SECRET", secretValue)
	res := runLocalCommand(envDumpCmd(), "", nil, true, 10*time.Second, defaultMaxOutput)
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, output = %q", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, secretValue) {
		t.Fatalf("opt-in full inheritance lost the secret: %q", res.Output)
	}
}

func TestRunLocalCommandEnvMergeOnTop(t *testing.T) {
	t.Setenv("AGO_TEST_MERGED", "parent_value")
	// An explicit value passes even when not whitelisted…
	res := runLocalCommand(envDumpCmd(), "", map[string]string{"AGO_TEST_EXTRA": "extra_value"}, false, 10*time.Second, defaultMaxOutput)
	if !strings.Contains(res.Output, "AGO_TEST_EXTRA=extra_value") {
		t.Fatalf("merged variable not passed: %q", res.Output)
	}
	// …and wins over an inherited duplicate (os/exec keeps the last entry per key).
	res = runLocalCommand(envDumpCmd(), "", map[string]string{"AGO_TEST_MERGED": "merged_value"}, true, 10*time.Second, defaultMaxOutput)
	if !strings.Contains(res.Output, "merged_value") || strings.Contains(res.Output, "parent_value") {
		t.Fatalf("merged value did not override the inherited one: %q", res.Output)
	}
}

func TestShellToolEnvSecretHidden(t *testing.T) {
	t.Setenv("AGO_TEST_SECRET", secretValue)
	s, err := NewShellTool()
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":`+quoteGo(secretProbeCmd("AGO_TEST_SECRET"))+`}`)
	got := resultPartText(t, ev)
	if strings.Contains(got, secretValue) || strings.Contains(got, "leaked") {
		t.Fatalf("shell tool leaked the secret by default: %q", got)
	}
	if !strings.Contains(got, "unset") {
		t.Fatalf("child output does not confirm the variable is absent: %q", got)
	}

	// Full inheritance is opt-in and then actually delivers the variable.
	si, err := NewShellTool(WithShellInheritEnv())
	if err != nil {
		t.Fatalf("NewShellTool(WithShellInheritEnv): %v", err)
	}
	defer si.Close()
	ev = callTool(t, si, cx, si.Name(), `{"command":`+quoteGo(secretPrintCmd("AGO_TEST_SECRET"))+`}`)
	if got := resultPartText(t, ev); !strings.Contains(got, secretValue) {
		t.Fatalf("opt-in inheritance lost the secret: %q", got)
	}
}

func TestCodeRunnerEnvSecretHidden(t *testing.T) {
	t.Setenv("AGO_TEST_SECRET", secretValue)
	var code, language string
	switch {
	case pythonAvailable():
		code, language = "import os\nprint(os.environ.get('AGO_TEST_SECRET', 'unset'))", "python"
	default:
		if findShell() == "" {
			t.Skip("no python or working bash/sh on PATH")
		}
		code, language = "echo ${AGO_TEST_SECRET:-unset}", "bash"
	}
	r, err := NewLocalCodeRunner()
	if err != nil {
		t.Fatalf("NewLocalCodeRunner: %v", err)
	}
	defer r.Close()
	_, cx := newTestContext()
	ev := callTool(t, r, cx, r.Name(), `{"code":`+quoteGo(code)+`,"language":"`+language+`"}`)
	got := resultPartText(t, ev)
	if strings.Contains(got, secretValue) {
		t.Fatalf("code runner leaked the secret by default: %q", got)
	}
	if !strings.Contains(got, "unset") {
		t.Fatalf("child output does not confirm the variable is absent: %q", got)
	}
}

// --- Output buffer cap (security audit 2026-10-31, M-17) ----------------------

func TestLimitedBufferCapsWrites(t *testing.T) {
	b := &limitedBuffer{limit: 10}
	for _, chunk := range []string{"12345", "678", "90ab", "cdef"} {
		n, err := b.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
		// A full-length report keeps the exec copiers from treating the cap as
		// a short write.
		if n != len(chunk) {
			t.Fatalf("Write(%q) = %d, want %d", chunk, n, len(chunk))
		}
	}
	if got := b.buf.String(); got != "1234567890" {
		t.Fatalf("stored = %q, want the first 10 bytes only", got)
	}
	if b.dropped != 6 { // "ab" + "cdef"
		t.Fatalf("dropped = %d, want 6", b.dropped)
	}
}

func TestBuildOutputTruncationNotices(t *testing.T) {
	// Three-byte runes: 800 "あ" are 2400 bytes against a 2000-byte cap, so
	// 666 whole runes and a 2-byte partial rune (2000 bytes) are stored and
	// 400 bytes dropped. 667 runes plus the notice stay within maxOutput=1000
	// characters, the per-call character truncation does not fire, and the
	// buffer-cap notice is visible.
	b := &limitedBuffer{limit: 2000}
	if _, err := b.Write([]byte(strings.Repeat("あ", 800))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buildOutput(b, 1000)
	if !strings.HasPrefix(got, strings.Repeat("あ", 666)) {
		t.Fatalf("output does not start with the stored runes: %q…", got[:40])
	}
	if !strings.Contains(got, "dropped 400 bytes beyond the 2000-byte buffer cap") {
		t.Fatalf("buffer-cap notice missing: %q", got)
	}
	if strings.Contains(got, "showing first") {
		t.Fatalf("unexpected character truncation on top of the buffer cap: %q", got)
	}

	// One-byte runes: the cap fills with 2000 ASCII characters, which exceeds
	// maxOutput, so the per-call character truncation fires and its notice
	// names the counts the model actually sees.
	b2 := &limitedBuffer{limit: 2000}
	if _, err := b2.Write([]byte(strings.Repeat("x", 5000))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got2 := buildOutput(b2, 1000)
	if !strings.HasPrefix(got2, strings.Repeat("x", 1000)) {
		t.Fatalf("output does not start with 1000 characters: len=%d", len(got2))
	}
	if !strings.Contains(got2, "[truncated: showing first 1000 of") {
		t.Fatalf("character-truncation notice missing: %q", got2)
	}

	// Below both limits the output passes through untouched.
	b3 := &limitedBuffer{limit: 2000}
	if _, err := b3.Write([]byte("  plain output \n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got3 := buildOutput(b3, 1000); got3 != "plain output" {
		t.Fatalf("untrimmed passthrough = %q", got3)
	}
}

// floodCmd prints ~85 KiB of ASCII lines through the platform shell.
func floodCmd() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "for /l %i in (1,1,5000) do @echo 0123456789ABCDEF"}
	}
	return []string{"sh", "-c", "i=0; while [ $i -lt 5000 ]; do echo 0123456789ABCDEF; i=$((i+1)); done"}
}

func TestRunLocalCommandOutputBufferCap(t *testing.T) {
	// maxOutput=1000 caps the buffer at 2000 bytes; the flood command emits
	// ~85 KiB, so without the cap the buffer would hold all of it.
	res := runLocalCommand(floodCmd(), "", nil, false, 30*time.Second, 1000)
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, output = %q", res.ExitCode, res.Output)
	}
	if total := len([]rune(res.Output)); total > 1000+100 { // 1000 chars + notice line
		t.Fatalf("reported output is %d runes, want it capped near 1000", total)
	}
	if !strings.Contains(res.Output, "[truncated: showing first 1000 of") {
		t.Fatalf("truncation notice missing: %q", res.Output)
	}
}

// --- Timeout kills the whole process tree (security audit 2026-10-31, L-13) ---

// grandchildProbe names a long-running process only this test starts, and how
// to list matching processes on the platform ("" lines = none running).
func grandchildProbe() (start []string, listCmd []string, ok bool) {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "start /b ping -n 60 127.0.0.1 >nul & ping -n 60 127.0.0.1 >nul"},
			[]string{"tasklist", "/FI", "IMAGENAME eq ping.exe", "/FO", "CSV", "/NH"}, true
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		return nil, nil, false
	}
	return []string{"sh", "-c", "sleep 60 & sleep 60"}, []string{"pgrep", "-f", "sleep 60"}, true
}

func TestRunLocalCommandTimeoutKillsTree(t *testing.T) {
	start, listCmd, ok := grandchildProbe()
	if !ok {
		t.Skip("no process-listing tool on this platform")
	}
	// A stray matching process from outside the test would poison the verdict.
	if out, err := exec.Command(listCmd[0], listCmd[1:]...).Output(); err == nil && strings.Contains(strings.ToLower(string(out)), strings.ToLower(nameOf(start))) {
		t.Skip("a matching process is already running; cannot isolate the check")
	}
	res := runLocalCommand(start, "", nil, false, 2*time.Second, defaultMaxOutput)
	if res.ExitCode != 124 {
		t.Fatalf("exit code = %d, want 124 (timeout), output = %q", res.ExitCode, res.Output)
	}
	// The kill runs inside the context cancel; give the table a moment to
	// settle before listing.
	time.Sleep(500 * time.Millisecond)
	out, err := exec.Command(listCmd[0], listCmd[1:]...).Output()
	if err != nil {
		t.Fatalf("%s: %v", listCmd[0], err)
	}
	if strings.Contains(strings.ToLower(string(out)), strings.ToLower(nameOf(start))) {
		t.Fatalf("a grandchild survived the timeout kill:\n%s", out)
	}
}

// nameOf extracts the process name the probe listings match on (ping.exe on
// Windows, "sleep 60" elsewhere).
func nameOf(start []string) string {
	if runtime.GOOS == "windows" {
		return "ping.exe"
	}
	return "sleep 60"
}
