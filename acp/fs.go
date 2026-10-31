package acp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Mediated filesystem for the Bridge role (Python parity: bridge.py
// read_text_file / write_text_file + _confine): the fs/* requests an agent
// raises against the client's disk are answered here, confined under one
// root. A path that escapes the root is refused, never followed.
//
// The Bridge answers fs/* only when FS is set — an unset FS is the disable
// option, and the requests are then refused method-not-found, exactly like a
// client that never advertised fs capabilities (the ACP initialization doc:
// a capability-gated method not advertised gets -32601). The agent itself is
// expected to check the clientCapabilities.fs advertisement before calling;
// this side answers rather than advertises.

// FSConfig enables and confines the mediated filesystem (and the mediated
// terminals, which share the root — Python parity: BridgeState's
// fs_root-or-cwd). Root is the confinement root; when empty the Bridge's own
// working directory is used, the way Python falls back to config.cwd.
type FSConfig struct {
	Root string

	// TerminalInheritEnv opts the mediated terminals into the complete parent
	// environment instead of the shared toolpacks whitelist. The terminal
	// command line is model-controlled, so by default one `env` in a terminal
	// must not disclose the parent's keys (audit H-6); turn this on only when
	// the parent process carries nothing secret.
	//
	// AGO-DEVIATION: Python's TerminalManager inherits everything; AGo
	// defaults to the whitelist, full inheritance is this opt-in.
	TerminalInheritEnv bool
}

// root resolves the configured confinement root to its real path.
func (f *FSConfig) root() (string, error) {
	r := f.Root
	if r == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("acp: resolve fs root: %w", err)
		}
		r = wd
	}
	return realpath(r)
}

// realpath resolves symlinks in the existing components of path (Python
// parity: os.path.realpath, strict=False — a path whose tail does not exist
// yet resolves as far as the filesystem allows).
func realpath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	dir, base := filepath.Split(abs)
	if dir == "" {
		return abs, nil
	}
	resolvedDir, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return abs, nil // nothing resolvable — the clean path is the best available
	}
	return filepath.Join(resolvedDir, base), nil
}

// ErrPathEscapes reports a path that resolves outside the confinement root
// (Python parity: _confine's PermissionError).
var ErrPathEscapes = errors.New("acp: path escapes the filesystem root")

// confine resolves path under root and refuses anything that escapes it. A
// path equal to the root itself is allowed — the Python guard allows it the
// same way (full != root and not startswith root + sep).
func confine(root, path string) (string, error) {
	full := path
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, full)
	}
	resolved, err := realpath(full)
	if err != nil {
		return "", err
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: path %q escapes root %q", ErrPathEscapes, path, root)
	}
	return resolved, nil
}

// openConfined returns a root handle plus path relative to it: the pair every
// mediated read and write goes through. confine alone leaves a TOCTOU window
// — it resolves the symlinks, hands back a plain path, and the later open
// re-walks the disk an agent controls, so a component swapped for a symlink
// between the two would be followed. An os.Root re-resolves symlinks at open
// time and refuses any that escape, closing the window (audit M-21).
//
// AGO-DEVIATION: Python's _confine checks a realpath and opens by path; AGo
// opens through a root handle.
func openConfined(root, path string) (*os.Root, string, error) {
	full, err := confine(root, path)
	if err != nil {
		return nil, "", err
	}
	rel, err := filepath.Rel(root, full)
	if err != nil {
		// confine guarantees full is root itself or under it; an unreachable
		// branch kept for safety.
		return nil, "", fmt.Errorf("%w: path %q escapes root %q", ErrPathEscapes, path, root)
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", err
	}
	return rt, filepath.ToSlash(rel), nil
}

// mkdirAllRoot creates the parent directories of rel through the root handle,
// one Mkdir per component — Root has no MkdirAll, and creating each directory
// through the handle keeps the walk inside the confinement.
func mkdirAllRoot(rt *os.Root, rel string) error {
	dir := filepath.Dir(rel)
	if dir == "." || dir == "" {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(dir, "/") {
		if part == "" || part == "." {
			continue
		}
		if cur == "" {
			cur = part
		} else {
			cur = cur + "/" + part
		}
		if err := rt.Mkdir(cur, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return nil
}

// read_text_file wire shapes (ACP fs/read_text_file): {sessionId, path, line?,
// limit?} → {content}. line is the 1-based line to start at; limit caps how
// many lines are returned.
type readTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      *int   `json:"line"`
	Limit     *int   `json:"limit"`
}

type readTextFileResult struct {
	Content string `json:"content"`
}

// fsRead answers one fs/read_text_file under the root guard (Python parity:
// BridgeState.read_text_file — whole file, then the line/limit window).
func (b *Bridge) fsRead(params readTextFileParams) (readTextFileResult, *rpcError) {
	if b.FS == nil {
		return readTextFileResult{}, rpcErr(codeMethodNotFound, "method %q is not implemented", "fs/read_text_file")
	}
	root, err := b.FS.root()
	if err != nil {
		return readTextFileResult{}, rpcErr(codeInternalError, "%s", err.Error())
	}
	rt, rel, cerr := openConfined(root, params.Path)
	if cerr != nil {
		return readTextFileResult{}, rpcErr(codeResourceNotFound, "%s", cerr.Error())
	}
	defer rt.Close()
	f, rerr := rt.Open(rel)
	if rerr != nil {
		return readTextFileResult{}, rpcErr(codeResourceNotFound, "read %s: %s", params.Path, rerr.Error())
	}
	data, rerr := io.ReadAll(f)
	f.Close()
	if rerr != nil {
		return readTextFileResult{}, rpcErr(codeResourceNotFound, "read %s: %s", params.Path, rerr.Error())
	}
	content := string(data)
	if params.Line != nil || params.Limit != nil {
		// SplitAfter keeps each line's terminator, so a window of lines joins
		// back to the exact bytes (Python parity: readlines + slicing).
		lines := strings.SplitAfter(content, "\n")
		if params.Line != nil && *params.Line > 1 {
			if *params.Line-1 >= len(lines) {
				lines = nil
			} else {
				lines = lines[*params.Line-1:]
			}
		}
		if params.Limit != nil && *params.Limit < len(lines) {
			lines = lines[:*params.Limit]
		}
		content = strings.Join(lines, "")
	}
	return readTextFileResult{Content: content}, nil
}

// write_text_file wire shapes (ACP fs/write_text_file): {sessionId, path,
// content} → {}.
type writeTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

// fsWrite answers one fs/write_text_file under the root guard, creating
// parent directories the way Python's write does (Python parity:
// BridgeState.write_text_file).
func (b *Bridge) fsWrite(params writeTextFileParams) *rpcError {
	if b.FS == nil {
		return rpcErr(codeMethodNotFound, "method %q is not implemented", "fs/write_text_file")
	}
	root, err := b.FS.root()
	if err != nil {
		return rpcErr(codeInternalError, "%s", err.Error())
	}
	rt, rel, cerr := openConfined(root, params.Path)
	if cerr != nil {
		return rpcErr(codeResourceNotFound, "%s", cerr.Error())
	}
	defer rt.Close()
	if merr := mkdirAllRoot(rt, rel); merr != nil {
		return rpcErr(codeInternalError, "write %s: %s", params.Path, merr.Error())
	}
	f, werr := rt.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if werr != nil {
		return rpcErr(codeInternalError, "write %s: %s", params.Path, werr.Error())
	}
	if _, werr = f.WriteString(params.Content); werr != nil {
		f.Close()
		return rpcErr(codeInternalError, "write %s: %s", params.Path, werr.Error())
	}
	if werr = f.Close(); werr != nil {
		return rpcErr(codeInternalError, "write %s: %s", params.Path, werr.Error())
	}
	return nil
}
