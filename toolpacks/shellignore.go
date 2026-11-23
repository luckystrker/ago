package toolpacks

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// shellIgnoreMaxEntries is the number of filesystem entries one command may make
// the ignore filter look at: wildcard matches, entries read while expanding a
// wildcard, and entries walked under a directory operand all spend the same
// counter. A command over the budget is refused (fail closed) with a hint to
// name a smaller directory. It is a constant, not an option, so a caller cannot
// switch the bound off by accident.
const shellIgnoreMaxEntries = 20_000

// maxSymlinkHops bounds symlink resolution (the Linux MAXSYMLINKS is 40; Go's
// own EvalSymlinks allows 255); a longer chain is treated as a loop.
const maxSymlinkHops = 255

// dirReadBatch is how many entries are read from a directory at a time, so the
// budget stops a huge directory without listing it in full.
const dirReadBatch = 128

// errIgnoreUncheckable marks something the ignore filter cannot inspect. The
// command is refused rather than let through.
var errIgnoreUncheckable = errors.New("cannot be checked against ignore")

// ignoreError is a refusal reason; it matches errIgnoreUncheckable under
// errors.Is without repeating that sentinel's text in the message the model
// reads.
type ignoreError struct{ msg string }

func (e *ignoreError) Error() string        { return e.msg }
func (e *ignoreError) Is(target error) bool { return target == errIgnoreUncheckable }

func uncheckablef(format string, args ...any) error {
	return &ignoreError{msg: fmt.Sprintf(format, args...)}
}

// ignoreBudget is the shared entry counter of one checkIgnore call.
type ignoreBudget struct {
	limit, used int
}

func (b *ignoreBudget) spend() error {
	b.used++
	if b.used > b.limit {
		return uncheckablef("more than %d files to check against ignore; name a smaller directory", b.limit)
	}
	return nil
}

// directoryReaders are the programs in readonlyCommands that open files inside a
// directory operand on their own: grep -r, diff -r, and diff dir1 dir2 even
// without -r. Matching the directory word against the patterns is not enough, so
// every file under such an operand is checked.
var directoryReaders = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "diff": true}

var grepFamily = map[string]bool{"grep": true, "egrep": true, "fgrep": true}

// isDirectoryReader reports whether program (argv[0]) reads inside directories.
func isDirectoryReader(program string) bool { return directoryReaders[shellPathBase(program)] }

// checkIgnoreHost is the host-path part of checkIgnore: every word and the paths
// it may stand for are checked, and for grep/diff every file under a directory
// operand too. A non-empty result is the refusal text.
//
// AGO-DEVIATION: Python runs this check in a worker thread (asyncio.to_thread)
// so the event loop is not blocked. The Go tool is called on the turn's
// goroutine, which blocking does not harm, and the 20 000-entry budget bounds the
// work, so the check runs synchronously. See D-133.
func checkIgnoreHost(tokens []string, workdir string, patterns []string, limit int) string {
	absWorkdir, err := absPath(workdir)
	if err != nil {
		return "Access denied: " + err.Error()
	}
	root, err := resolveReal(absWorkdir)
	if err != nil {
		return "Access denied: " + err.Error()
	}
	budget := &ignoreBudget{limit: limit}

	// Each word with the paths its wildcard matches, relative to the workdir. A
	// wildcard is expanded once here and the result reused by both checks below.
	type word struct {
		index int // position of the word in the command; 0 is the program
		paths []string
	}
	var words []word
	for i, token := range tokens {
		for _, candidate := range embeddedPaths(token) {
			if candidate == "" {
				continue
			}
			matches, err := globMatches(absWorkdir, root, candidate, budget)
			if err != nil {
				return "Access denied: " + err.Error()
			}
			words = append(words, word{index: i, paths: append([]string{candidate}, matches...)})
		}
	}

	for _, w := range words {
		for _, p := range w.paths {
			resolved, err := resolveReal(joinWorkdir(root, p))
			if err != nil {
				return "Access denied: " + err.Error()
			}
			if denied := deniedPath(resolved, root, patterns); denied != "" {
				return denied
			}
		}
	}

	if len(tokens) == 0 || !isDirectoryReader(tokens[0]) {
		return ""
	}
	var operands []string // the words after the program, with their wildcard matches
	for _, w := range words {
		if w.index > 0 {
			operands = append(operands, w.paths...)
		}
	}
	return ignoredUnderOperands(tokens, root, patterns, budget, operands)
}

// walkStart is a directory to walk: lexical is the path it is reached by (an
// alias such as a symlink may carry its own ignore rule), real its target.
type walkStart struct{ lexical, real string }

// ignoredUnderOperands walks every directory among the operands of a grep/diff
// command (relative to root or absolute) and returns the refusal for the first
// file that may not be read.
func ignoredUnderOperands(tokens []string, root string, patterns []string, budget *ignoreBudget, operands []string) string {
	var starts []walkStart
	for _, p := range operands {
		joined := joinWorkdir(root, p)
		if !isDir(joined) {
			continue
		}
		real, err := resolveReal(joined)
		if err != nil {
			return "Access denied: " + err.Error()
		}
		starts = append(starts, walkStart{lexical: filepath.Clean(joined), real: real})
	}
	if grepFamily[shellPathBase(tokens[0])] {
		found := false
		for _, operand := range grepFileOperands(tokens[1:]) {
			if pathExists(joinWorkdir(root, operand)) {
				found = true
				break
			}
		}
		if !found {
			// With no file operand a recursive grep searches the working directory.
			starts = append(starts, walkStart{lexical: root, real: root})
		}
	}
	denied, err := ignoredInside(starts, root, patterns, budget)
	if err != nil {
		return "Access denied: " + err.Error()
	}
	return denied
}

// ancestry is a persistent list of the real directories above the one being
// walked, so a symlink back into an ancestor is recognised without copying a set.
type ancestry struct {
	real   string
	parent *ancestry
}

func (a *ancestry) contains(real string) bool {
	for ; a != nil; a = a.parent {
		if a.real == real {
			return true
		}
	}
	return false
}

// pendingDir is a directory still to be walked.
type pendingDir struct {
	lexical, real string
	ancestors     *ancestry
}

// ignoredInside returns "Access denied: <path>" for the first file under the
// starts directories that may not be read. Every entry is checked both by the
// path it is reached under and by its real target, and symlinks are followed
// the way grep -R and diff -r follow them. Entries are read in small batches and
// each is charged to budget, so a large tree is refused without being listed in
// full. A directory that cannot be listed or resolved is an error (fail closed).
// A symlink back into one of its own ancestors is not entered again.
func ignoredInside(starts []walkStart, root string, patterns []string, budget *ignoreBudget) (string, error) {
	pending := make([]pendingDir, 0, len(starts))
	for _, s := range starts {
		pending = append(pending, pendingDir{s.lexical, s.real, &ancestry{real: s.real}})
	}
	for len(pending) > 0 {
		cur := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		var denied string
		err := eachEntry(cur.real, func(entry fs.DirEntry) error {
			if err := budget.spend(); err != nil {
				return err
			}
			child := filepath.Join(cur.lexical, entry.Name())
			// The directory is already real, so only a symlink entry needs resolving.
			target := filepath.Join(cur.real, entry.Name())
			if entry.Type()&fs.ModeSymlink != 0 {
				var err error
				if target, err = resolveReal(target); err != nil {
					return err
				}
			}
			for _, candidate := range []string{child, target} {
				if d := deniedPath(candidate, root, patterns); d != "" {
					denied = d
					return errStopWalk
				}
			}
			if isDir(target) && !cur.ancestors.contains(target) {
				pending = append(pending, pendingDir{child, target, &ancestry{real: target, parent: cur.ancestors}})
			}
			return nil
		})
		if denied != "" {
			return denied, nil
		}
		if err != nil {
			if errors.Is(err, errIgnoreUncheckable) {
				return "", err
			}
			return "", uncheckablef("cannot list %s", cur.lexical)
		}
	}
	return "", nil
}

var errStopWalk = errors.New("stop")

// eachEntry calls fn for every entry of dir, reading in batches so fn can stop a
// huge directory early. errStopWalk from fn ends the scan without an error.
func eachEntry(dir string, fn func(fs.DirEntry) error) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		entries, err := f.ReadDir(dirReadBatch)
		for _, e := range entries {
			if ferr := fn(e); ferr != nil {
				if errors.Is(ferr, errStopWalk) {
					return nil
				}
				return ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// globMatches returns the paths, relative to the workdir, that a wildcard token
// matches. The command runs as argv, but a program may expand a wildcard itself
// (MSYS tools do on Windows), so a pattern naming an ignored file, or a
// directory holding one, must be refused. The pattern is expanded one path
// segment at a time and every directory entry read on the way, matching or not,
// is charged to budget. A pattern that cannot be expanded here is an error:
// "**", whose scan the budget cannot bound, and a pattern crossing a directory
// that cannot be listed.
func globMatches(absWorkdir, root, token string, budget *ignoreBudget) ([]string, error) {
	if !strings.ContainsAny(token, "*?[") {
		return nil, nil
	}
	pattern := token
	if filepath.IsAbs(pattern) {
		rel, ok := relativeTo(pattern, absWorkdir)
		if !ok {
			rel, ok = relativeTo(pattern, root)
		}
		if !ok {
			return nil, nil // outside the workdir: the word itself is refused
		}
		pattern = rel
	}
	if strings.Contains(token, "**") {
		return nil, uncheckablef("cannot check recursive wildcard %q", token)
	}
	found := []string{""}
	for _, part := range strings.Split(filepath.ToSlash(pattern), "/") {
		if part == "" || part == "." {
			continue
		}
		var next []string
		for _, prefix := range found {
			if strings.ContainsAny(part, "*?[") {
				matched, err := matchingEntries(root, prefix, part, budget)
				if err != nil {
					return nil, err
				}
				next = append(next, matched...)
			} else {
				next = append(next, joinRel(prefix, part))
			}
		}
		found = next
	}
	if len(found) == 1 && found[0] == "" {
		return nil, nil
	}
	return found, nil
}

// matchingEntries returns prefix/name for each entry of the prefix directory
// whose name matches the wildcard part.
func matchingEntries(root, prefix, part string, budget *ignoreBudget) ([]string, error) {
	dir := joinWorkdir(root, prefix)
	if prefix == "" {
		dir = root
	}
	var matched []string
	err := eachEntry(dir, func(entry fs.DirEntry) error {
		if err := budget.spend(); err != nil {
			return err
		}
		if matchGlob(part, entry.Name()) {
			matched = append(matched, joinRel(prefix, entry.Name()))
		}
		return nil
	})
	switch {
	case err == nil:
		return matched, nil
	case errors.Is(err, errIgnoreUncheckable):
		return nil, err
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		return nil, nil // nothing there for the wildcard to match
	default:
		return nil, uncheckablef("cannot list %s", dir)
	}
}

func joinRel(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// relativeTo returns p relative to base when p lies under base (lexically).
func relativeTo(p, base string) (string, bool) {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return rel, true
}

// joinWorkdir joins a token onto the resolved workdir the way a shell would: an
// absolute token replaces it. It concatenates instead of cleaning, because a
// lexical clean would erase a symlink followed by ".." before it is resolved.
func joinWorkdir(root, token string) string {
	if filepath.IsAbs(token) {
		return token
	}
	return root + string(filepath.Separator) + token
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// absPath makes p absolute without cleaning; an error means the working
// directory is unknown, so a relative path cannot be placed.
func absPath(p string) (string, error) {
	if filepath.IsAbs(p) {
		return p, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", uncheckablef("cannot resolve %q: %v", p, err)
	}
	return wd + string(filepath.Separator) + p, nil
}

// resolveReal resolves p through the filesystem the way realpath / Python
// Path.resolve() does, and reports an error where the path cannot be resolved
// (a symlink loop, a component that cannot be inspected, an unknown working
// directory). It must not clean lexically first: "link/../x" where link is a
// symlink to another directory escapes through the link, and a path whose tail
// does not exist yet (a write target) is still resolved up to the part that
// does. Components that do not exist are kept as written and later ".." pops
// them lexically.
func resolveReal(p string) (string, error) {
	p, err := absPath(p)
	if err != nil {
		return "", err
	}
	sep := string(filepath.Separator)
	vol := filepath.VolumeName(p)
	root := vol + sep
	remaining := splitSegments(p[len(vol):])
	var resolved []string
	hops := 0
	for len(remaining) > 0 {
		seg := remaining[0]
		remaining = remaining[1:]
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(resolved) > 0 {
				resolved = resolved[:len(resolved)-1]
			}
			continue
		}
		cand := root + strings.Join(append(append([]string(nil), resolved...), seg), sep)
		fi, err := os.Lstat(cand)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				resolved = append(resolved, seg)
				continue
			}
			return "", uncheckablef("cannot resolve %q", p)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			hops++
			if hops > maxSymlinkHops {
				return "", uncheckablef("cannot resolve %q (symlink loop)", p)
			}
			target, err := os.Readlink(cand)
			if err != nil {
				return "", uncheckablef("cannot resolve %q", p)
			}
			if filepath.IsAbs(target) {
				vol = filepath.VolumeName(target)
				root = vol + sep
				resolved = nil
				target = target[len(vol):]
			}
			remaining = append(splitSegments(target), remaining...)
			continue
		}
		resolved = append(resolved, seg)
	}
	return root + strings.Join(resolved, sep), nil
}

func splitSegments(p string) []string {
	if filepath.Separator != '/' {
		p = strings.ReplaceAll(p, "/", string(filepath.Separator))
	}
	return strings.Split(p, string(filepath.Separator))
}

// shortGrepValueFlags lists the grep short options whose value may be the next
// word (-e PATTERN, -f FILE, -m NUM, -A/-B/-C NUM, -d/-D ACTION).
const shortGrepValueFlags = "efmABCdD"

// longGrepValueOptions are the grep long options whose value may be the next
// word. Treating an option as taking a value when it does not only makes the
// check stricter, so long names match by prefix (getopt accepts unambiguous
// abbreviations).
var longGrepValueOptions = []string{
	"regexp", "file", "max-count", "after-context", "before-context", "context",
	"directories", "devices", "label", "include", "exclude", "exclude-from",
	"exclude-dir", "binary-files", "group-separator",
}

// grepFileOperands returns the words grep takes as files (best effort, erring
// towards none). The first positional word is the pattern unless -e / -f gives
// one. When this guesses wrong it returns fewer operands, which makes the caller
// check the whole working directory.
func grepFileOperands(args []string) []string {
	var positionals []string
	explicitPattern := false
	optionsEnded := false
	for i := 0; i < len(args); {
		arg := args[i]
		i++
		switch {
		case optionsEnded || arg == "-" || !strings.HasPrefix(arg, "-"):
			positionals = append(positionals, arg)
		case arg == "--":
			optionsEnded = true
		case strings.HasPrefix(arg, "--"):
			name, _, hasValue := strings.Cut(arg[2:], "=")
			explicitPattern = explicitPattern || name == "regexp" || name == "file"
			if !hasValue {
				for _, option := range longGrepValueOptions {
					if strings.HasPrefix(option, name) {
						i++
						break
					}
				}
			}
		default:
			letters := []rune(arg[1:])
			for pos, letter := range letters {
				explicitPattern = explicitPattern || letter == 'e' || letter == 'f'
				if strings.ContainsRune(shortGrepValueFlags, letter) {
					if pos == len(letters)-1 {
						i++
					}
					break
				}
			}
		}
	}
	if explicitPattern {
		return positionals
	}
	if len(positionals) == 0 {
		return nil
	}
	return positionals[1:]
}
