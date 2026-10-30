package toolpacks

import (
	"bytes"
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// defaultExecTimeout bounds one subprocess when the caller set no timeout
// (Python parity: LocalEnvironment timeout=60).
const defaultExecTimeout = 60 * time.Second

// defaultMaxOutput caps the characters a tool reports back (Python parity:
// LocalSandbox max_output=100_000).
const defaultMaxOutput = 100_000

// execResult is the outcome of one local subprocess (Python parity:
// sandbox.base.ExecResult): the combined stdout+stderr output and the exit code.
type execResult struct {
	Output   string
	ExitCode int
}

// runLocalCommand executes argv in dir with the given timeout and reports the
// combined output (Python parity: sandbox.local._run_subprocess): stdout followed
// by stderr, whitespace-trimmed, truncated to maxOutput characters with a trailing
// notice. A missing interpreter reports "Command not found: …" with exit code 127;
// an expired timeout reports "Command timed out after Ns" with exit code 124.
func runLocalCommand(argv []string, dir string, env map[string]string, timeout time.Duration, maxOutput int) execResult {
	if len(argv) == 0 {
		return execResult{ExitCode: 2}
	}
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		// Python parity: the child inherits the parent environment; per-call
		// values are merged on top.
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
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
	output := strings.TrimSpace(out.String())
	if total := len([]rune(output)); total > maxOutput {
		r := []rune(output)
		output = string(r[:maxOutput]) + fmt.Sprintf("\n[truncated: showing first %d of %d chars]", maxOutput, total)
	}
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
