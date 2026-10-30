package toolpacks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// FilesystemToolkit exposes local file operations as function tools, every path
// confined to one root directory (Python parity: ag2/tools/toolkits/
// filesystem.py FilesystemToolkit — base_path, the traversal guard, the UTF-8
// pinning). The member set is the porting brief's read/write/list/mkdir/glob;
// Python's update_file and delete_file are deferred (docs/DECISIONS.md).
//
// All tools read the filesystem per call, so the toolkit is safe for concurrent
// use; concurrent writers race the way concurrent writers to one file do.
type FilesystemToolkit struct {
	root string
	tk   *tools.Toolkit
}

var _ tools.Tool = (*FilesystemToolkit)(nil)

// NewFilesystemToolkit builds the five tools rooted at dir. The root must be an
// existing directory (Python parity: _resolve_dir). Model paths are relative to
// the root; absolute paths and any path resolving outside the root are refused.
func NewFilesystemToolkit(root string) (*FilesystemToolkit, error) {
	abs, err := absDir(root)
	if err != nil {
		return nil, fmt.Errorf("toolpacks: filesystem root: %w", err)
	}
	// Pin the root through its real path so a symlinked root does not turn every
	// containment check into a false escape.
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	f := &FilesystemToolkit{root: abs}

	readFn := func(args struct {
		Path string `json:"path" agschema:"description=Relative path to the file to read,required"`
	}) (string, error) {
		target, err := f.resolve(args.Path)
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(target)
		if err != nil {
			return "", err
		}
		// Text only: the porting brief's read is the UTF-8 read (Python also
		// offers raw=true base64 for binary files — deferred).
		return string(b), nil
	}
	readFile, err := tools.NewFunctionTool("read_file",
		"Read the contents of a file (UTF-8 text).", readFn)
	if err != nil {
		return nil, err
	}

	writeFn := func(args struct {
		Path    string `json:"path" agschema:"description=Relative path to the file to create or overwrite,required"`
		Content string `json:"content" agschema:"description=The full content to write to the file,required"`
	}) (string, error) {
		target, err := f.resolve(args.Path)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		// UTF-8 always (Python parity: write_text(encoding="utf-8")).
		if err := os.WriteFile(target, []byte(args.Content), 0o644); err != nil {
			return "", err
		}
		return fmt.Sprintf("Successfully wrote %d characters to %s", len(args.Content), args.Path), nil
	}
	writeFile, err := tools.NewFunctionTool("write_file",
		"Create or overwrite a file with the given content. Parent directories are created automatically.",
		writeFn)
	if err != nil {
		return nil, err
	}

	listFn := func(args struct {
		Path string `json:"path" agschema:"description=Relative path to the directory to list"`
	}) ([]string, error) {
		rel := args.Path
		if rel == "" {
			rel = "."
		}
		target, err := f.resolve(rel)
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(target)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			out = append(out, name)
		}
		sort.Strings(out)
		return out, nil
	}
	listDir, err := tools.NewFunctionTool("list_directory",
		"List a directory's entries. Directories carry a trailing slash.", listFn)
	if err != nil {
		return nil, err
	}

	mkdirFn := func(args struct {
		Path string `json:"path" agschema:"description=Relative path of the directory to create,required"`
	}) (string, error) {
		target, err := f.resolve(args.Path)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return "", err
		}
		return fmt.Sprintf("Successfully created directory %s", args.Path), nil
	}
	mkdir, err := tools.NewFunctionTool("make_directory",
		"Create a directory (and any missing parents) inside the workspace.", mkdirFn)
	if err != nil {
		return nil, err
	}

	globFn := func(args struct {
		Pattern string `json:"pattern" agschema:"description=Glob pattern to match files such as '*.py' or '**/*.txt',required"`
		Path    string `json:"path" agschema:"description=Relative path to the directory to search from"`
	}) ([]string, error) {
		if err := checkGlobPattern(args.Pattern); err != nil {
			return nil, err
		}
		rel := args.Path
		if rel == "" {
			rel = "."
		}
		base, err := f.resolve(rel)
		if err != nil {
			return nil, err
		}
		matches, err := globWithin(base, args.Pattern)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(matches))
		for _, m := range matches {
			relToRoot, err := filepath.Rel(f.root, m)
			if err != nil {
				continue
			}
			out = append(out, filepath.ToSlash(relToRoot))
		}
		sort.Strings(out)
		return out, nil
	}
	findFiles, err := tools.NewFunctionTool("find_files",
		"Search for files matching a glob pattern. Use recursive patterns like '**/*.py' to search subdirectories.",
		globFn)
	if err != nil {
		return nil, err
	}

	tk, err := tools.NewToolkit("filesystem_toolkit", nil, readFile, writeFile, listDir, mkdir, findFiles)
	if err != nil {
		return nil, err
	}
	f.tk = tk
	return f, nil
}

// Root returns the resolved root directory.
func (f *FilesystemToolkit) Root() string { return f.root }

// Name returns the toolkit name.
func (f *FilesystemToolkit) Name() string { return f.tk.Name() }

// DeclaredInCode reports a program-declared tool (framework contract).
func (f *FilesystemToolkit) DeclaredInCode() bool { return f.tk.DeclaredInCode() }

// Tools returns the members in declaration order.
func (f *FilesystemToolkit) Tools() []tools.Tool { return f.tk.Tools() }

// Schemas concatenates the members' schemas.
func (f *FilesystemToolkit) Schemas(cx *agocontext.Context) ([]any, error) { return f.tk.Schemas(cx) }

// Register subscribes every member on the conversation stream.
func (f *FilesystemToolkit) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	return f.tk.Register(cx, mw)
}

// resolve maps a model-supplied relative path onto the root and refuses escapes
// (Python parity: _resolve_path — resolve, then require containment). Absolute
// paths are refused outright; existing prefixes are resolved through symlinks so
// a link pointing outside the root is caught, non-existent paths are checked
// lexically.
func (f *FilesystemToolkit) resolve(rel string) (string, error) {
	if rel == "" {
		return "", errors.New("path must not be empty")
	}
	if path.IsAbs(rel) || filepath.IsAbs(rel) ||
		(len(rel) >= 2 && rel[1] == ':') { // drive-letter paths on Windows
		return "", fmt.Errorf("absolute paths are not allowed: %q (paths are relative to the workspace root)", rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace root", rel)
	}
	joined := filepath.Join(f.root, clean)
	// Symlink guard: resolve the deepest existing prefix and re-check
	// containment (a symlink inside the root can still point outside it).
	resolved, err := resolveExisting(joined)
	if err != nil {
		return "", err
	}
	if !withinRoot(f.root, resolved) {
		return "", fmt.Errorf("path %q escapes the workspace root", rel)
	}
	return resolved, nil
}

// resolveExisting evaluates symlinks along the existing prefix of p and appends
// the remaining non-existent segments lexically.
func resolveExisting(p string) (string, error) {
	existing := p
	var tail []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		dir, base := filepath.Split(existing)
		if dir == existing { // reached the root of the path
			break
		}
		tail = append([]string{base}, tail...)
		existing = filepath.Clean(dir)
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	for _, seg := range tail {
		resolved = filepath.Join(resolved, seg)
	}
	return filepath.Abs(resolved)
}

// withinRoot reports whether p is the root or lives under it (path-separator
// aware, so /root2 does not pass for /root).
func withinRoot(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// checkGlobPattern refuses patterns that could walk outside the search directory
// (Python parity: _check_pattern — no anchor, no ".." segment).
func checkGlobPattern(pattern string) error {
	if pattern == "" {
		return errors.New("glob pattern must not be empty")
	}
	if path.IsAbs(pattern) || filepath.IsAbs(pattern) {
		return fmt.Errorf("pattern %q escapes the workspace root", pattern)
	}
	for _, seg := range strings.FieldsFunc(pattern, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if seg == ".." {
			return fmt.Errorf("pattern %q escapes the workspace root", pattern)
		}
	}
	return nil
}

// globWithin matches pattern under base with Python's 3.13 semantics: "**"
// crosses directory levels and matches zero levels. filepath.Glob has no "**", so
// the match walks the tree for ** segments and hands the rest to filepath.Match.
func globWithin(base, pattern string) ([]string, error) {
	pat := filepath.FromSlash(pattern)
	if !strings.Contains(pat, "**") {
		matches, err := filepath.Glob(filepath.Join(base, pat))
		if err != nil {
			return nil, err
		}
		return filesOnly(matches), nil
	}
	var matches []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		ok, merr := matchRecursive(filepath.ToSlash(rel), filepath.ToSlash(pattern))
		if merr != nil {
			return merr
		}
		if ok && !d.IsDir() {
			matches = append(matches, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return matches, nil
}

// matchRecursive matches rel (slash-separated) against a glob pattern where "**"
// spans zero or more whole path segments.
func matchRecursive(rel, pattern string) (bool, error) {
	relSegs := strings.Split(rel, "/")
	patSegs := strings.Split(pattern, "/")
	return matchSegs(relSegs, patSegs)
}

func matchSegs(rel, pat []string) (bool, error) {
	if len(pat) == 0 {
		return len(rel) == 0, nil
	}
	if pat[0] == "**" {
		// "**" eats zero or more segments; collapse a doubled "**/ **".
		rest := pat[1:]
		for len(rest) > 0 && rest[0] == "**" {
			rest = rest[1:]
		}
		for i := 0; i <= len(rel); i++ {
			ok, err := matchSegs(rel[i:], rest)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	if len(rel) == 0 {
		return false, nil
	}
	ok, err := filepath.Match(pat[0], rel[0])
	if err != nil || !ok {
		return false, err
	}
	return matchSegs(rel[1:], pat[1:])
}

// filesOnly keeps regular files.
func filesOnly(paths []string) []string {
	out := paths[:0]
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	return out
}
