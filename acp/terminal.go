package acp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// Mediated terminals for the Bridge role (Python parity: bridge.py
// TerminalManager): the terminal/* requests an agent raises are answered by
// running the command locally, confined under the filesystem root. Enabled
// together with the mediated filesystem — a Bridge without FS answers
// terminal/* with method-not-found, like a client that never advertised
// terminal support.
//
// AGO-DEVIATION: the Python exit status maps a signal-terminated process to a
// signal name; Go's process API reports a negative exit code for those, which
// is what travels here — the signal field stays empty (docs/DECISIONS.md
// D-066).

// terminal wire shapes (ACP terminal/*): create is
// {sessionId, command, args?, env?, cwd?, outputByteLimit?} → {terminalId};
// output → {output, truncated, exitStatus?}; wait_for_exit → {exitCode,
// signal?}; kill and release answer {}.
type createTerminalParams struct {
	SessionID       string        `json:"sessionId"`
	Command         string        `json:"command"`
	Args            []string      `json:"args"`
	Env             []envVariable `json:"env"`
	CWD             string        `json:"cwd"`
	OutputByteLimit int           `json:"outputByteLimit"`
}

type envVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type createTerminalResult struct {
	TerminalID string `json:"terminalId"`
}

type terminalOutputResult struct {
	Output     string      `json:"output"`
	Truncated  bool        `json:"truncated"`
	ExitStatus *exitStatus `json:"exitStatus,omitempty"`
}

type exitStatus struct {
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
}

type waitForExitResult struct {
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
}

type terminalIDParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

// terminalManager runs commands on behalf of the agent, confined under root.
// Safe for concurrent use.
type terminalManager struct {
	root string

	mu        sync.Mutex
	terminals map[string]*managedTerminal
	counter   int
}

func newTerminalManager(root string) *terminalManager {
	return &terminalManager{root: root, terminals: map[string]*managedTerminal{}}
}

// managedTerminal is one running command with capped output capture.
type managedTerminal struct {
	mu        sync.Mutex
	buf       []byte
	truncated bool
	limit     int

	cmd  *exec.Cmd
	done chan struct{} // closed after the command exited and the output drained
	exit *exitStatus
}

// create starts command and returns the terminal id immediately: the command
// runs in the background while the agent keeps working (Python parity:
// TerminalManager.create). cwd is confined under the root, defaulting to the
// root itself; stderr is merged into stdout (Python parity: stderr=STDOUT).
func (m *terminalManager) create(params createTerminalParams) (string, *rpcError) {
	if params.Command == "" {
		return "", rpcErr(codeInvalidParams, "terminal/create: command is required")
	}
	cwd := m.root
	if params.CWD != "" {
		resolved, err := confine(m.root, params.CWD)
		if err != nil {
			return "", rpcErr(codeResourceNotFound, "%s", err.Error())
		}
		cwd = resolved
	}
	cmd := exec.Command(params.Command, params.Args...)
	cmd.Dir = cwd
	env := append([]string(nil), os.Environ()...)
	for _, kv := range params.Env {
		env = append(env, kv.Name+"="+kv.Value)
	}
	cmd.Env = env

	pr, pw, err := os.Pipe()
	if err != nil {
		return "", rpcErr(codeInternalError, "terminal output pipe: %s", err.Error())
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	term := &managedTerminal{limit: params.OutputByteLimit, cmd: cmd, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return "", rpcErr(codeInvalidParams, "terminal/create %s: %s", params.Command, err.Error())
	}
	// The child holds its own copy of the write end; dropping the parent's
	// lets the reader see EOF when the process (and anything holding the pipe
	// with it) is gone.
	pw.Close()
	go term.capture(pr)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.counter++
	id := fmt.Sprintf("term-%d", m.counter)
	m.terminals[id] = term
	return id, nil
}

// capture reads the output until EOF, then waits the process out and records
// the exit status (Python parity: _Terminal._read + wait). Output beyond the
// byte limit is dropped, the head kept and the excess marked truncated.
func (t *managedTerminal) capture(pr *os.File) {
	defer close(t.done)
	defer pr.Close()
	buf := make([]byte, 4096)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			t.mu.Lock()
			if t.limit > 0 && len(t.buf)+n > t.limit {
				keep := max(0, t.limit-len(t.buf))
				t.buf = append(t.buf, buf[:keep]...)
				t.truncated = true
			} else {
				t.buf = append(t.buf, buf[:n]...)
			}
			t.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	waitErr := t.cmd.Wait()
	var st exitStatus
	var ee *exec.ExitError
	switch {
	case waitErr == nil:
		code := t.cmd.ProcessState.ExitCode()
		st.ExitCode = &code
	case errors.As(waitErr, &ee):
		code := ee.ExitCode()
		st.ExitCode = &code
	default:
		// The process could not be waited on at all; the status stays unset.
	}
	t.mu.Lock()
	t.exit = &st
	t.mu.Unlock()
}

// wait blocks until the command completes and reports its exit status
// (Python parity: TerminalManager.wait). Runs inline — the Bridge's exchanges
// are serialized, so a wait parks the connection the same way Python's await
// does.
func (m *terminalManager) wait(id string) (waitForExitResult, *rpcError) {
	term, rpcErr := m.get(id)
	if rpcErr != nil {
		return waitForExitResult{}, rpcErr
	}
	<-term.done
	term.mu.Lock()
	defer term.mu.Unlock()
	if term.exit == nil {
		return waitForExitResult{}, nil
	}
	return waitForExitResult{ExitCode: term.exit.ExitCode, Signal: term.exit.Signal}, nil
}

// output snapshots the captured output without waiting (Python parity:
// TerminalManager.output).
func (m *terminalManager) output(id string) (terminalOutputResult, *rpcError) {
	term, rpcErr := m.get(id)
	if rpcErr != nil {
		return terminalOutputResult{}, rpcErr
	}
	term.mu.Lock()
	out := string(term.buf)
	truncated := term.truncated
	term.mu.Unlock()
	res := terminalOutputResult{Output: out, Truncated: truncated}
	if isClosed(term.done) {
		term.mu.Lock()
		res.ExitStatus = term.exit
		term.mu.Unlock()
	}
	return res, nil
}

// isClosed reports whether the done channel is closed, without waiting.
func isClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// kill terminates the command without releasing the terminal (Python parity:
// TerminalManager.kill — the id stays valid for output/wait/release).
func (m *terminalManager) kill(id string) *rpcError {
	term, rpcErr := m.get(id)
	if rpcErr != nil {
		return rpcErr
	}
	if term.cmd.Process != nil {
		_ = term.cmd.Process.Kill()
	}
	return nil
}

// release kills the command if still running and retires the id: every later
// terminal/* method naming it answers resource-not-found (Python parity:
// TerminalManager.release).
func (m *terminalManager) release(id string) {
	m.mu.Lock()
	term := m.terminals[id]
	delete(m.terminals, id)
	m.mu.Unlock()
	if term == nil {
		return
	}
	if term.cmd.Process != nil {
		_ = term.cmd.Process.Kill()
	}
}

// get fetches a live terminal by id; a released or unknown id is a
// resource-not-found (ACP error doc: terminal/* naming an unknown terminal
// gets -32002).
func (m *terminalManager) get(id string) (*managedTerminal, *rpcError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	term, ok := m.terminals[id]
	if !ok {
		return nil, rpcErr(codeResourceNotFound, "no such terminal: %s", id)
	}
	return term, nil
}

// --- Bridge-side answers (the mediated methods) --------------------------------

// requireTerms refuses the mediated terminal methods when no filesystem root
// is configured — the disable option (Python parity: a client that never
// advertised terminal support answers method-not-found). The manager is built
// on first use: FS may be set on the Bridge only after NewBridge returned.
func (b *Bridge) requireTerms(method string) *rpcError {
	if b.FS == nil {
		return rpcErr(codeMethodNotFound, "method %q is not implemented", method)
	}
	b.termsOnce.Do(func() {
		if root, err := b.FS.root(); err == nil {
			b.terms = newTerminalManager(root)
		}
	})
	if b.terms == nil {
		return rpcErr(codeInternalError, "resolve fs root failed")
	}
	return nil
}

// fsCreateTerminal answers one terminal/create request.
func (b *Bridge) fsCreateTerminal(params createTerminalParams) (createTerminalResult, *rpcError) {
	if rpcErr := b.requireTerms("terminal/create"); rpcErr != nil {
		return createTerminalResult{}, rpcErr
	}
	id, rpcErr := b.terms.create(params)
	if rpcErr != nil {
		return createTerminalResult{}, rpcErr
	}
	return createTerminalResult{TerminalID: id}, nil
}

// terminalOutput answers one terminal/output request.
func (b *Bridge) terminalOutput(p terminalIDParams) (terminalOutputResult, *rpcError) {
	if rpcErr := b.requireTerms("terminal/output"); rpcErr != nil {
		return terminalOutputResult{}, rpcErr
	}
	return b.terms.output(p.TerminalID)
}

// terminalWait answers one terminal/wait_for_exit request.
func (b *Bridge) terminalWait(p terminalIDParams) (waitForExitResult, *rpcError) {
	if rpcErr := b.requireTerms("terminal/wait_for_exit"); rpcErr != nil {
		return waitForExitResult{}, rpcErr
	}
	return b.terms.wait(p.TerminalID)
}

// terminalKill answers one terminal/kill request.
func (b *Bridge) terminalKill(p terminalIDParams) *rpcError {
	if rpcErr := b.requireTerms("terminal/kill"); rpcErr != nil {
		return rpcErr
	}
	return b.terms.kill(p.TerminalID)
}

// terminalRelease answers one terminal/release request.
func (b *Bridge) terminalRelease(p terminalIDParams) *rpcError {
	if rpcErr := b.requireTerms("terminal/release"); rpcErr != nil {
		return rpcErr
	}
	b.terms.release(p.TerminalID)
	return nil
}
