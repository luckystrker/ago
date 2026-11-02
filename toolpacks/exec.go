package toolpacks

import (
	"bytes"
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// defaultExecTimeout bounds one subprocess when the caller set no timeout
// (Python parity: LocalEnvironment timeout=60).
const defaultExecTimeout = 60 * time.Second

// defaultMaxOutput caps the characters a tool reports back (Python parity:
// LocalSandbox max_output=100_000).
const defaultMaxOutput = 100_000

// execEnvWhitelist names the parent variables a subprocess receives by default.
// The command line and the code snippet both come from the model, so the child
// environment is a disclosure channel: one `env` would hand the model every API
// key of the parent process. Only variables a program needs to locate
// executables, temp space, home, locale and its per-user profile directory pass
// through; anything else must be added explicitly via the env option or the
// full-inheritance opt-in.
// APPDATA and LOCALAPPDATA are profile locations, not secrets, and standard
// Windows tooling resolves its state through them: the py launcher, denied
// LOCALAPPDATA, treats its whole install database as missing and re-downloads a
// Python interpreter into the current directory on every run.
// AGO-DEVIATION: Python's LocalEnvironment inherits the complete parent
// environment; AGo defaults to this whitelist (security audit 2026-10-31, H-6),
// full inheritance is opt-in per tool.
var execEnvWhitelist = []string{
	"PATH", "TEMP", "TMP", "HOME", "USERPROFILE",
	"SystemRoot", "COMSPEC", "PATHEXT", "LANG", "LC_ALL",
	"USER", "USERNAME", "PWD", "APPDATA", "LOCALAPPDATA",
}

// InheritedEnv returns the environment a subprocess starts from: the complete
// parent set when the caller explicitly opted into inheritance, otherwise only
// the whitelisted variables. Exported because the ACP mediated terminals build
// their child environment the same way (audit H-6): one list shared by every
// model-driven subprocess surface, so the two cannot drift apart.
func InheritedEnv(inheritAll bool) []string {
	parent := os.Environ()
	if inheritAll {
		return parent
	}
	return filterWhitelistedEnv(parent)
}

// filterWhitelistedEnv keeps only the whitelisted entries of parent. Variable
// names compare case-insensitively on Windows, where the environment itself is
// case-insensitive (PATH may arrive as Path).
func filterWhitelistedEnv(parent []string) []string {
	windows := runtime.GOOS == "windows"
	allowed := make(map[string]struct{}, len(execEnvWhitelist))
	for _, k := range execEnvWhitelist {
		if windows {
			k = strings.ToLower(k)
		}
		allowed[k] = struct{}{}
	}
	out := make([]string, 0, len(execEnvWhitelist))
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		if windows {
			k = strings.ToLower(k)
		}
		if _, ok := allowed[k]; ok {
			out = append(out, kv)
		}
	}
	return out
}

// execResult is the outcome of one local subprocess (Python parity:
// sandbox.base.ExecResult): the combined stdout+stderr output and the exit code.
type execResult struct {
	Output   string
	ExitCode int
}

// ErrCommandInterrupted reports a command whose context was canceled while the
// child was still running: the tree kill stopped it, so no result exists
// (Python parity: the interrupted exec raises the turn's cancellation instead
// of returning). Tools surface it as a tool error, never as output.
var ErrCommandInterrupted = errors.New("toolpacks: command interrupted")

// ErrSandboxClosed reports a call on a tool whose Close already ran (Python
// parity: RuntimeError "LocalSandbox has been closed.").
var ErrSandboxClosed = errors.New("toolpacks: local sandbox has been closed")

// ErrSandboxClosedWhileRunning reports a command that Close stopped mid-flight
// (Python parity: RuntimeError "LocalSandbox was closed while the command was
// running.").
var ErrSandboxClosedWhileRunning = errors.New("toolpacks: local sandbox was closed while the command was running")

// limitedBuffer stores subprocess output up to limit bytes and silently counts
// everything beyond. The exec package serializes Write calls when Stdout and
// Stderr share one writer, so no extra locking is needed here.
type limitedBuffer struct {
	buf     bytes.Buffer
	limit   int
	dropped int64
}

// Write implements io.Writer; it always reports the full length so the exec
// copiers never see a short write when the cap discards the tail.
func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	if room <= 0 {
		b.dropped += int64(len(p))
		return len(p), nil
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		b.dropped += int64(len(p) - room)
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// truncationNotice is appended when the buffer cap discarded output, so the
// model knows the result is incomplete instead of assuming it saw everything.
const truncationNotice = "[output truncated: dropped %d bytes beyond the %d-byte buffer cap]"

// buildOutput renders the stored bytes as the reported output: whitespace
// trimmed, the buffer-cap notice appended, then the per-call maxOutput rune
// truncation (Python parity: "\n[truncated: showing first %d of %d chars]").
// Bytes and characters differ, so both notices can appear — the cap is set to
// 2*maxOutput bytes, which leaves headroom for multi-byte runes while keeping a
// chatty child (`yes`) from growing the buffer without bound.
func buildOutput(b *limitedBuffer, maxOutput int) string {
	output := strings.TrimSpace(b.buf.String())
	if b.dropped > 0 {
		output += fmt.Sprintf("\n"+truncationNotice, b.dropped, b.limit)
	}
	if total := len([]rune(output)); total > maxOutput {
		r := []rune(output)
		output = string(r[:maxOutput]) + fmt.Sprintf("\n[truncated: showing first %d of %d chars]", maxOutput, total)
	}
	return output
}

// runLocalCommand executes argv in dir with the given timeout and reports the
// combined output (Python parity: sandbox.local._run_subprocess): stdout followed
// by stderr, whitespace-trimmed, truncated to maxOutput characters with a trailing
// notice. A missing interpreter reports "Command not found: …" with exit code 127;
// an expired timeout reports "Command timed out after Ns" with exit code 124.
// A canceled parent context kills the whole process tree and yields
// ErrCommandInterrupted: a command stopped mid-flight has no result. A nil ctx
// means stdcontext.Background().
// The child environment is always constructed explicitly: the whitelisted parent
// variables (or all of them when inheritAllEnv is set), with env merged on top.
func runLocalCommand(ctx stdcontext.Context, argv []string, dir string, env map[string]string, inheritAllEnv bool, timeout time.Duration, maxOutput int) (execResult, error) {
	if len(argv) == 0 {
		return execResult{ExitCode: 2}, nil
	}
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	if maxOutput <= 0 {
		maxOutput = defaultMaxOutput
	}
	if ctx == nil {
		ctx = stdcontext.Background()
	}
	cctx, cancel := stdcontext.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	// On timeout the default context kill terminates only the direct child,
	// leaving grandchildren running; setupTreeKill replaces it with a
	// whole-tree kill (process group on Unix, taskkill /T on Windows). The
	// same kill serves a parent cancel: every cancellation of cctx — deadline,
	// turn cancel, Close — stops the whole tree.
	setupTreeKill(cmd)
	// Per-call values are appended last and os/exec keeps the last entry per
	// key, so an explicit value overrides an inherited one.
	cmdEnv := InheritedEnv(inheritAllEnv)
	for k, v := range env {
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	cmd.Env = cmdEnv
	// The buffer cap keeps unbounded child output from growing host memory:
	// maxOutput only shapes the text handed back, the subprocess itself can
	// emit far more (security audit 2026-10-31, M-17).
	out := &limitedBuffer{limit: 2 * maxOutput}
	cmd.Stdout = out
	cmd.Stderr = out
	// A shell killed at the deadline may leave grandchildren (a stalled ping, a
	// sleep) holding the output pipes; without this bound cmd.Wait would block
	// until they exit on their own. WaitDelay closes the pipes and unblocks the
	// wait once the context cancel has fired.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		// A context that died before the start is the cancellation (or an
		// already-passed deadline), not a launch failure.
		if errors.Is(cctx.Err(), stdcontext.Canceled) {
			return execResult{}, ErrCommandInterrupted
		}
		if errors.Is(cctx.Err(), stdcontext.DeadlineExceeded) {
			return execResult{Output: fmt.Sprintf("Command timed out after %gs", timeout.Seconds()), ExitCode: 124}, nil
		}
		if errors.Is(err, exec.ErrNotFound) || isPathLookupError(err) {
			return execResult{Output: fmt.Sprintf("Command not found: %v", err), ExitCode: 127}, nil
		}
		return execResult{Output: err.Error(), ExitCode: 1}, nil
	}
	waitErr := cmd.Wait()
	if errors.Is(cctx.Err(), stdcontext.DeadlineExceeded) {
		return execResult{
			Output:   fmt.Sprintf("Command timed out after %gs", timeout.Seconds()),
			ExitCode: 124,
		}, nil
	}
	// A cancel that landed while the child ran killed it through cmd.Cancel, so
	// the failed wait is the interruption, not a command outcome (Python parity:
	// the cancelled exec raises). A wait that already succeeded before the
	// cancel landed keeps its real result — upstream's collect loop returns a
	// finished command's output without consulting the cancel flag.
	if waitErr != nil && errors.Is(cctx.Err(), stdcontext.Canceled) {
		return execResult{}, ErrCommandInterrupted
	}
	output := buildOutput(out, maxOutput)
	res := execResult{Output: output}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			if res.ExitCode < 0 { // killed by a signal: report a generic failure
				res.ExitCode = 1
			}
		} else {
			res.Output = strings.TrimSpace(output + "\n" + waitErr.Error())
			res.ExitCode = 1
		}
	}
	return res, nil
}

// --- in-flight command registry (Python parity: LocalSandbox._running) --------

// runningSlot is one in-flight command: the cancel that stops it and the channel
// closed once its call has fully returned.
type runningSlot struct {
	cancel stdcontext.CancelFunc
	done   chan struct{}
}

// localSandbox is the lifecycle plumbing shared by ShellTool and CodeRunner
// (Python parity: LocalSandbox._running plus the closed guard): every command
// registers itself so Close can stop the whole set and wait for it to die, and
// calls on a closed sandbox fail instead of starting work in a disposed one.
// The zero value is ready for use.
type localSandbox struct {
	mu      sync.Mutex
	closed  bool
	running map[*runningSlot]struct{}
}

// isClosed reports whether shutdown has started.
func (s *localSandbox) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// call runs one command under parent, registering it so shutdown can stop it.
// The closed check and the registration share one critical section with
// shutdown's snapshot, so a call either sees the closed flag or is guaranteed
// to be canceled by the shutdown that follows. An interruption is reported as
// ErrCommandInterrupted — or as ErrSandboxClosedWhileRunning when it was Close
// that stopped the command.
func (s *localSandbox) call(parent stdcontext.Context, argv []string, dir string, env map[string]string, inheritAllEnv bool, timeout time.Duration, maxOutput int) (execResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return execResult{}, ErrSandboxClosed
	}
	callCtx, cancel := stdcontext.WithCancel(parent)
	slot := &runningSlot{cancel: cancel, done: make(chan struct{})}
	if s.running == nil {
		s.running = make(map[*runningSlot]struct{})
	}
	s.running[slot] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, slot)
		s.mu.Unlock()
		close(slot.done)
	}()

	res, err := runLocalCommand(callCtx, argv, dir, env, inheritAllEnv, timeout, maxOutput)
	if errors.Is(err, ErrCommandInterrupted) && s.isClosed() {
		// The command did not outlive a close: report the close that stopped it,
		// not a bare interruption (Python parity: "LocalSandbox was closed while
		// the command was running.").
		return execResult{}, ErrSandboxClosedWhileRunning
	}
	return res, err
}

// shutdown marks the sandbox closed, cancels every in-flight command and waits
// for all of them to return (Python parity: aclose cancels self._running and
// awaits it). Commands must be dead before their workdir goes away: on Windows
// the directory cannot be removed while a child still holds it as its CWD.
func (s *localSandbox) shutdown() {
	s.mu.Lock()
	s.closed = true
	slots := make([]*runningSlot, 0, len(s.running))
	for slot := range s.running {
		slots = append(slots, slot)
	}
	s.mu.Unlock()
	for _, slot := range slots {
		slot.cancel()
	}
	for _, slot := range slots {
		<-slot.done
	}
}

// isPathLookupError reports whether err is the exec launcher's look-path failure
// (Go <=1.19 wording wraps exec.ErrNotFound; newer wording may not).
func isPathLookupError(err error) bool {
	var look *exec.Error
	return errors.As(err, &look)
}

// absDir resolves dir to an absolute path and requires it to be an existing
// directory (Python parity: LocalEnvironment materializes its path).
func absDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("toolpacks: workdir %q: %w", dir, err)
	}
	info, err := os.Stat(abs)
	switch {
	case err != nil:
		return "", fmt.Errorf("toolpacks: workdir %q: %w", dir, err)
	case !info.IsDir():
		return "", fmt.Errorf("toolpacks: workdir %q: not a directory", dir)
	}
	return abs, nil
}

// formatExit appends the exit-code suffix to a non-zero outcome (Python parity:
// sandbox.adapter.shell._format — the suffix travels inside the result text, the
// refusal is an ordinary output line, not an error).
func formatExit(r execResult) string {
	if r.ExitCode != 0 {
		suffix := fmt.Sprintf("[exit code: %d]", r.ExitCode)
		if r.Output == "" {
			return suffix
		}
		return r.Output + "\n" + suffix
	}
	return r.Output
}
