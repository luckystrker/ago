package toolpacks

import (
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
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
	res, err := runLocalCommand(stdcontext.Background(), envDumpCmd(), "", nil, false, 10*time.Second, defaultMaxOutput)
	if err != nil {
		t.Fatalf("runLocalCommand: %v", err)
	}
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
	res, err := runLocalCommand(stdcontext.Background(), envDumpCmd(), "", nil, true, 10*time.Second, defaultMaxOutput)
	if err != nil {
		t.Fatalf("runLocalCommand: %v", err)
	}
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
	res, err := runLocalCommand(stdcontext.Background(), envDumpCmd(), "", map[string]string{"AGO_TEST_EXTRA": "extra_value"}, false, 10*time.Second, defaultMaxOutput)
	if err != nil {
		t.Fatalf("runLocalCommand: %v", err)
	}
	if !strings.Contains(res.Output, "AGO_TEST_EXTRA=extra_value") {
		t.Fatalf("merged variable not passed: %q", res.Output)
	}
	// …and wins over an inherited duplicate (os/exec keeps the last entry per key).
	res, err = runLocalCommand(stdcontext.Background(), envDumpCmd(), "", map[string]string{"AGO_TEST_MERGED": "merged_value"}, true, 10*time.Second, defaultMaxOutput)
	if err != nil {
		t.Fatalf("runLocalCommand: %v", err)
	}
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
	res, err := runLocalCommand(stdcontext.Background(), floodCmd(), "", nil, false, 30*time.Second, 1000)
	if err != nil {
		t.Fatalf("runLocalCommand: %v", err)
	}
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
// No `start /b` on windows: it does not spawn reliably on headless CI runners;
// the foreground pair keeps the same cmd→ping grandchild shape.
//
// The Unix sleep duration is unique per call: a plain "sleep 60" listing is
// matched by pgrep -f across every concurrent test binary (a parallel
// `go test ./...` against `-count=N ./toolpacks/` sees the other process's
// probes), which makes the alive/dead verdicts order-dependent. A unique
// fractional duration keeps each probe's listing to its own process tree.
func grandchildProbe() (start []string, listCmd []string, ok bool) {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "ping -n 60 127.0.0.1 >nul & ping -n 60 127.0.0.1 >nul"},
			[]string{"tasklist", "/FI", "IMAGENAME eq ping.exe", "/FO", "CSV", "/NH"}, true
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		return nil, nil, false
	}
	duration := uniqueSleepDuration()
	return []string{"sh", "-c", "sleep " + duration + " & sleep " + duration},
		[]string{"pgrep", "-f", "sleep " + duration}, true
}

// uniqueSleepDuration formats a fractional sleep long enough for the probe and
// unique to this test process and call.
func uniqueSleepDuration() string {
	n := (time.Now().UnixNano() + int64(os.Getpid())*1_000_003) % 1_000_000_000
	return fmt.Sprintf("59.%09d", n)
}

// listMatching runs the probe listing. pgrep exits 1 when nothing matches —
// for the post-kill check that is the success case, not an error.
func listMatching(t *testing.T, listCmd []string) string {
	t.Helper()
	out, err := exec.Command(listCmd[0], listCmd[1:]...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return string(out)
	}
	if err != nil {
		t.Fatalf("%s: %v", listCmd[0], err)
	}
	return string(out)
}

// probeAlive reports whether the listing shows a live matching process.
// pgrep -f prints only PIDs — the process name never occurs in its output —
// so on unix any non-empty listing is a match; tasklist prints image names,
// so there the name check works.
func probeAlive(t *testing.T, listCmd []string, name string) bool {
	t.Helper()
	out := strings.ToLower(strings.TrimSpace(listMatching(t, listCmd)))
	if runtime.GOOS == "windows" {
		return strings.Contains(out, strings.ToLower(name))
	}
	return out != ""
}

// waitProbeRunning polls the probe listing until a matching process appears
// (the command actually running) or the deadline passes. Only for the test
// goroutine: listMatching fails the test on a hard listing error.
func waitProbeRunning(t *testing.T, listCmd []string, name string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if probeAlive(t, listCmd, name) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// waitProbeGone polls until the probe listing no longer shows a live matching
// process, or returns false at the deadline. A killed process can take a moment
// to leave the process table, so a fixed sleep is a race.
func waitProbeGone(t *testing.T, listCmd []string, name string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if !probeAlive(t, listCmd, name) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunLocalCommandTimeoutKillsTree(t *testing.T) {
	start, listCmd, ok := grandchildProbe()
	if !ok {
		t.Skip("no process-listing tool on this platform")
	}
	// A stray matching process from outside the test would poison the verdict.
	if probeAlive(t, listCmd, nameOf(start)) {
		t.Skip("a matching process is already running; cannot isolate the check")
	}
	res, err := runLocalCommand(stdcontext.Background(), start, "", nil, false, 2*time.Second, defaultMaxOutput)
	if err != nil {
		t.Fatalf("runLocalCommand: %v", err)
	}
	if res.ExitCode != 124 {
		t.Fatalf("exit code = %d, want 124 (timeout), output = %q", res.ExitCode, res.Output)
	}
	// The kill runs inside the context cancel; the process table may take a
	// moment to settle.
	if !waitProbeGone(t, listCmd, nameOf(start), 5*time.Second) {
		t.Fatalf("a grandchild survived the timeout kill:\n%s", listMatching(t, listCmd))
	}
}

func TestRunLocalCommandCancelKillsTree(t *testing.T) {
	start, listCmd, ok := grandchildProbe()
	if !ok {
		t.Skip("no process-listing tool on this platform")
	}
	// A stray matching process from outside the test would poison the verdict.
	if probeAlive(t, listCmd, nameOf(start)) {
		t.Skip("a matching process is already running; cannot isolate the check")
	}
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	defer cancel()
	type outcome struct {
		res execResult
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		// A timeout far beyond the test: the cancel, not the deadline, must
		// stop the command.
		res, err := runLocalCommand(ctx, start, "", nil, false, time.Minute, defaultMaxOutput)
		ch <- outcome{res, err}
	}()
	if !waitProbeRunning(t, listCmd, nameOf(start), 30*time.Second) {
		t.Fatalf("the probe process never appeared; listing now: %q", strings.TrimSpace(listMatching(t, listCmd)))
	}
	cancel()
	select {
	case o := <-ch:
		if !errors.Is(o.err, ErrCommandInterrupted) {
			t.Fatalf("err = %v, want ErrCommandInterrupted (output %q, exit %d)", o.err, o.res.Output, o.res.ExitCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runLocalCommand did not return after the parent cancel")
	}
	if !waitProbeGone(t, listCmd, nameOf(start), 5*time.Second) {
		t.Fatalf("a grandchild survived the cancel kill:\n%s", listMatching(t, listCmd))
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

// TestFilterWhitelistedEnvNameCase pins the name matching per platform: on
// Windows the variable names are case-insensitive, so every casing of a
// whitelisted name passes; elsewhere only the exact spelling does.
func TestFilterWhitelistedEnvNameCase(t *testing.T) {
	parent := []string{
		"PATH=/upper",
		"Path=/mixed",
		"SECRET=leak",
	}
	got := filterWhitelistedEnv(parent)
	kept := map[string]bool{}
	for _, kv := range got {
		k, _, _ := strings.Cut(kv, "=")
		kept[k] = true
	}
	if runtime.GOOS == "windows" {
		if !kept["PATH"] || !kept["Path"] {
			t.Fatalf("mixed-case PATH dropped on Windows: %v", got)
		}
		if kept["SECRET"] {
			t.Fatalf("the secret passed the whitelist: %v", got)
		}
	} else if !kept["PATH"] || kept["Path"] {
		t.Fatalf("case-sensitive filter wrong on this platform: %v", got)
	}
}

// TestBuildOutputNoTruncationAtExactCap pins the truncation boundary: output of
// exactly maxOutput characters is not a truncation — the notice only appears
// beyond the cap.
func TestBuildOutputNoTruncationAtExactCap(t *testing.T) {
	b := &limitedBuffer{limit: 200}
	if _, err := b.Write([]byte(strings.Repeat("a", 50))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := buildOutput(b, 50); got != strings.Repeat("a", 50) {
		t.Fatalf("output at the exact cap = %q, want the untouched 50 characters", got)
	}
}

// TestRunLocalCommandZeroOptionsGetDefaults pins the default injection: a zero
// timeout means the default bound (not an instant expiry) and a zero maxOutput
// means the default cap (not "truncate everything away").
func TestRunLocalCommandZeroOptionsGetDefaults(t *testing.T) {
	argv := []string{"sh", "-c", "echo defaults_ok"}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", "echo defaults_ok"}
	}
	res, err := runLocalCommand(stdcontext.Background(), argv, "", nil, false, 0, 0)
	if err != nil {
		t.Fatalf("runLocalCommand: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, output = %q", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "defaults_ok") {
		t.Fatalf("output = %q, want the command output without a zero cap", res.Output)
	}
	if strings.Contains(res.Output, "timed out") || strings.Contains(res.Output, "truncated") {
		t.Fatalf("zero timeout/maxOutput must fall back to the defaults, got %q", res.Output)
	}
}

// TestRunLocalCommandNotFoundIs127 pins the launch-failure classification: a
// missing binary reports "Command not found" with exit code 127, not a generic
// failure (Python parity: the missing interpreter path).
func TestRunLocalCommandNotFoundIs127(t *testing.T) {
	res, err := runLocalCommand(stdcontext.Background(),
		[]string{"ago-definitely-missing-binary-xyz"}, "", nil, false, 10*time.Second, defaultMaxOutput)
	if err != nil {
		t.Fatalf("runLocalCommand: %v", err)
	}
	if res.ExitCode != 127 {
		t.Fatalf("exit code = %d, want 127 (command not found), output = %q", res.ExitCode, res.Output)
	}
	if !strings.HasPrefix(res.Output, "Command not found") {
		t.Fatalf("output = %q, want the Command not found classification", res.Output)
	}
}

// TestUndecodableHelper is not a real test: it is the child-process body of
// TestRunLocalCommandUndecodableOutputKeepsTextAndExitCode (stdlib idiom: the
// test binary re-invoked with an env flag, Python parity: sys.executable -c in
// test_local_sandbox.py). Without the flag it is a no-op.
func TestUndecodableHelper(t *testing.T) {
	if os.Getenv("AGO_UNDECODABLE_HELPER") == "" {
		return
	}
	payload := []byte("before\x81after\n")
	switch os.Getenv("AGO_UNDECODABLE_FD") {
	case "2":
		os.Stderr.Write(payload)
	default:
		os.Stdout.Write(payload)
	}
	code, err := strconv.Atoi(os.Getenv("AGO_UNDECODABLE_EXIT"))
	if err != nil {
		code = 0
	}
	os.Exit(code)
}

// TestRunLocalCommandUndecodableOutputKeepsTextAndExitCode is the regression
// for the undecodable-output tolerance (Python parity: local.py _run_subprocess
// errors="replace"): output bytes the host text layer cannot decode must lose
// neither the surrounding text nor the exit code. Go captures raw bytes, so
// the invalid byte stays raw in the output string here; the model-visible
// U+FFFD replacement happens at the JSON boundary (pinned in
// TestCodeRunnerUndecodableOutput).
func TestRunLocalCommandUndecodableOutputKeepsTextAndExitCode(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	for _, fd := range []string{"1", "2"} {
		for _, code := range []int{0, 7} {
			t.Run("fd"+fd+"/exit"+strconv.Itoa(code), func(t *testing.T) {
				env := map[string]string{
					"AGO_UNDECODABLE_HELPER": "1",
					"AGO_UNDECODABLE_FD":     fd,
					"AGO_UNDECODABLE_EXIT":   strconv.Itoa(code),
				}
				res, err := runLocalCommand(stdcontext.Background(),
					[]string{exe, "-test.run=TestUndecodableHelper$"}, "", env, false, 30*time.Second, defaultMaxOutput)
				if err != nil {
					t.Fatalf("runLocalCommand: %v", err)
				}
				if res.ExitCode != code {
					t.Fatalf("exit code = %d, want %d (output %q)", res.ExitCode, code, res.Output)
				}
				// The text around the undecodable byte survives, and the byte
				// itself stays raw at this layer.
				if !strings.Contains(res.Output, "before\x81after") {
					t.Fatalf("output = %q, want the raw 0x81 byte preserved between before/after", res.Output)
				}
			})
		}
	}
}
