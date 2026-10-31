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
// The child environment is always constructed explicitly: the whitelisted parent
// variables (or all of them when inheritAllEnv is set), with env merged on top.
func runLocalCommand(argv []string, dir string, env map[string]string, inheritAllEnv bool, timeout time.Duration, maxOutput int) execResult {
	if len(argv) == 0 {
		return execResult{ExitCode: 2}
	}
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	if maxOutput <= 0 {
		maxOutput = defaultMaxOutput
	}
	ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	// On timeout the default context kill terminates only the direct child,
	// leaving grandchildren running; setupTreeKill replaces it with a
	// whole-tree kill (process group on Unix, taskkill /T on Windows).
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
		if errors.Is(err, exec.ErrNotFound) || isPathLookupError(err) {
			return execResult{Output: fmt.Sprintf("Command not found: %v", err), ExitCode: 127}
		}
		return execResult{Output: err.Error(), ExitCode: 1}
	}
	waitErr := cmd.Wait()
	if ctx.Err() == stdcontext.DeadlineExceeded {
		return execResult{
			Output:   fmt.Sprintf("Command timed out after %gs", timeout.Seconds()),
			ExitCode: 124,
		}
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
	return res
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
