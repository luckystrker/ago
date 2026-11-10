package toolpacks

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// readonlyCommands is what WithShellReadonly allows when no explicit whitelist
// is given (Python parity: sandbox/filter.py READONLY_COMMANDS). Every entry has
// no option that writes a file or runs another program, so the guarantee can be
// checked by reading this list. "find" (-exec), "file" (-C) and "git"
// (--output, programs named in .git/config) are deliberately absent; a caller
// that needs them lists them in WithShellAllowed.
var readonlyCommands = []string{
	"cat", "head", "tail", "ls", "grep", "egrep", "fgrep", "wc", "du", "df",
	"diff", "stat", "which", "pwd", "echo", "printenv", "cut",
}

// ReadonlyCommands returns the commands WithShellReadonly allows when no
// explicit whitelist is given. The returned slice is a copy.
func ReadonlyCommands() []string {
	out := make([]string, len(readonlyCommands))
	copy(out, readonlyCommands)
	return out
}

// shellOperators is the shell syntax restricted mode rejects up front
// (Python parity: filter.py _SHELL_OPERATORS). Restricted mode runs the parsed
// argv without a shell, so none of it would take effect; refusing it gives the
// model a clear error instead of a confusing one from the command
// ("cat: '>': No such file").
var shellOperators = []string{">", ">>", "<", "|", ";", "&", "&&", "||", "\n", "\r", "`", "$("}

// splitCommand splits command with POSIX shell quoting, or reports an error
// when the quotes or the trailing escape do not balance (Python parity:
// filter.py split_command / shlex.split). It never invokes a shell.
func splitCommand(command string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	rs := []rune(command)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if escaped {
			cur.WriteRune(c)
			inWord = true
			escaped = false
			continue
		}
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case quote == '"':
			switch c {
			case '"':
				quote = 0
			case '\\':
				if i+1 < len(rs) {
					n := rs[i+1]
					// POSIX: inside double quotes a backslash only escapes
					// $ ` " \ and newline; anything else is a literal backslash.
					if n == '"' || n == '\\' || n == '$' || n == '`' || n == '\n' {
						cur.WriteRune(n)
						i++
					} else {
						cur.WriteRune(c)
					}
				} else {
					return nil, errors.New("unbalanced quotes")
				}
			default:
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inWord = true
		case c == '\\':
			if i+1 >= len(rs) {
				return nil, errors.New("trailing escape")
			}
			escaped = true
			inWord = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unbalanced quotes")
	}
	if escaped {
		return nil, errors.New("trailing escape")
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}

// containsShellOperator reports whether command contains shell syntax outside
// quotes and escapes (Python parity: filter.py contains_shell_operator).
func containsShellOperator(command string) bool {
	unquoted := unquoteShell(command)
	for _, op := range shellOperators {
		if strings.Contains(unquoted, op) {
			return true
		}
	}
	return false
}

// unquoteShell drops quoted and backslash-escaped text (Python parity:
// filter.py _unquoted), so an operator inside quotes is not shell syntax.
func unquoteShell(command string) string {
	var kept strings.Builder
	rs := []rune(command)
	var quote rune
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(rs) {
				i++
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '\\':
			if i+1 < len(rs) {
				i++
			}
		default:
			kept.WriteRune(c)
		}
	}
	return kept.String()
}

// matchesArgv reports whether argv starts with the words of pattern
// (Python parity: filter.py matches_argv). "git log" matches
// ["git","log","-5"] but not ["git","-c","x","log"].
func matchesArgv(pattern string, argv []string) bool {
	words := strings.Fields(pattern)
	if len(words) > len(argv) {
		return false
	}
	for i, word := range words {
		if argv[i] != word {
			return false
		}
	}
	return true
}

// checkIgnore returns "Access denied: <path>" when any literal path in command
// leaves workdir or matches one of patterns, and "" otherwise (Python parity:
// filter.py check_ignore). Tokens are extracted with POSIX quoting, resolved
// against the real filesystem (symlinks included) and checked both as written
// and as the paths a program may read out of a word (after "=" and glued to a
// short option).
func checkIgnore(command, workdir string, patterns []string) string {
	if len(patterns) == 0 {
		return ""
	}
	tokens, err := splitCommand(command)
	if err != nil {
		tokens = strings.Fields(command)
	}
	root, err := filepath.Abs(workdir)
	if err != nil {
		return ""
	}
	if resolved, rerr := filepath.EvalSymlinks(root); rerr == nil {
		root = resolved
	}
	for _, token := range tokens {
		for _, candidate := range embeddedPaths(token) {
			if candidate == "" {
				continue
			}
			resolved, matches := ignoreCandidates(root, candidate)
			for _, p := range append([]string{resolved}, matches...) {
				if denied := deniedPath(p, root, patterns); denied != "" {
					return denied
				}
			}
		}
	}
	return ""
}

// embeddedPaths returns token plus the paths a program may read out of it: the
// tail after "=" (of=.env, --output=.env) and the tails glued to a short option
// (-o.env, -ro.env). Checking every tail errs towards refusing.
func embeddedPaths(token string) []string {
	found := []string{token}
	if strings.Contains(token, "=") {
		for _, part := range strings.Split(token, "=") {
			if part != "" {
				found = append(found, part)
			}
		}
	}
	if strings.HasPrefix(token, "-") && !strings.HasPrefix(token, "--") {
		for i := 2; i < len(token); i++ {
			found = append(found, token[i:])
		}
	}
	return found
}

// ignoreCandidates resolves token relative to root and, for a glob token,
// returns the files it matches on the host path.
func ignoreCandidates(root, token string) (string, []string) {
	// Concatenate instead of filepath.Join: Join cleans lexically, which would
	// erase a symlink followed by ".." ("link/../x") before it can be resolved
	// physically.
	joined := token
	if !filepath.IsAbs(joined) {
		joined = root + string(filepath.Separator) + token
	}
	resolved := resolvePhysical(joined)
	var matches []string
	if strings.ContainsAny(token, "*?[") {
		if found, err := filepath.Glob(joined); err == nil {
			for _, m := range found {
				matches = append(matches, resolvePhysical(m))
			}
		}
	}
	return resolved, matches
}

// resolvePhysical resolves path through the filesystem the way realpath /
// Python Path.resolve() does. It must not clean lexically first: "link/../x"
// where link is a symlink to another directory escapes through the link, and
// EvalSymlinks on the whole path fails when the tail does not exist yet (a
// write target). So resolve the longest existing prefix, append the remaining
// segments, and clean only at the end.
func resolvePhysical(path string) string {
	// Do not use filepath.Abs/filepath.Clean here: both clean lexically, which
	// would erase a symlink followed by ".." before Lstat can see it.
	abs := path
	if !filepath.IsAbs(abs) {
		if wd, err := os.Getwd(); err == nil {
			abs = wd + string(filepath.Separator) + path
		} else {
			return filepath.Clean(path)
		}
	}
	existing := abs
	var tail []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		i := strings.LastIndex(existing, string(filepath.Separator))
		if i < 0 {
			break
		}
		base := existing[i+1:]
		existing = existing[:i]
		if existing == "" {
			existing = string(filepath.Separator)
		}
		tail = append([]string{base}, tail...)
	}
	resolved := existing
	if real, err := filepath.EvalSymlinks(existing); err == nil {
		resolved = real
	}
	for _, seg := range tail {
		if seg == "" {
			continue
		}
		resolved = filepath.Join(resolved, seg)
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	return filepath.Clean(resolved)
}

// deniedPath applies patterns to one resolved path (Python parity:
// filter.py _denied): a path outside the workdir is denied, then glob patterns
// match the relative path or the base name and literal patterns match the name,
// the relative path or one of its parent segments.
func deniedPath(resolved, root string, patterns []string) string {
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "Access denied: " + resolved
	}
	rel = filepath.ToSlash(rel)
	name := filepath.Base(resolved)
	for _, pattern := range patterns {
		if strings.ContainsAny(pattern, "*?[") {
			if matchGlob(pattern, rel) || matchGlob(pattern, name) {
				return "Access denied: " + resolved
			}
			if strings.HasPrefix(pattern, "**/") && matchGlob(pattern[3:], name) {
				return "Access denied: " + resolved
			}
			continue
		}
		if name == pattern || rel == pattern || strings.HasPrefix(rel, pattern+"/") {
			return "Access denied: " + resolved
		}
	}
	return ""
}

// matchGlob matches s against a shell-style glob where "*" spans path
// separators too (Python parity: fnmatch.fnmatch). path.Match cannot be used
// directly: its "*" never crosses "/".
func matchGlob(pattern, s string) bool {
	return matchGlobRunes([]rune(pattern), []rune(s))
}

func matchGlobRunes(pattern, s []rune) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if matchGlobRunes(pattern, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		case '[':
			class, rest, ok := parseGlobClass(pattern)
			if !ok { // unterminated "[" is a literal
				if len(s) == 0 || s[0] != '[' {
					return false
				}
				pattern, s = pattern[1:], s[1:]
				continue
			}
			if len(s) == 0 || !matchGlobClass(class, s[0]) {
				return false
			}
			pattern, s = rest, s[1:]
		default:
			if len(s) == 0 || pattern[0] != s[0] {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		}
	}
	return len(s) == 0
}

// parseGlobClass parses a "[...]" class at the start of pattern, returning the
// class body and the remainder. ok is false when the class is unterminated.
func parseGlobClass(pattern []rune) (body, rest []rune, ok bool) {
	j := 1
	if j < len(pattern) && (pattern[j] == '!' || pattern[j] == '^') {
		j++
	}
	if j < len(pattern) && pattern[j] == ']' {
		j++
	}
	for j < len(pattern) && pattern[j] != ']' {
		j++
	}
	if j >= len(pattern) {
		return nil, nil, false
	}
	return pattern[1:j], pattern[j+1:], true
}

func matchGlobClass(class []rune, c rune) bool {
	negated := false
	if len(class) > 0 && (class[0] == '!' || class[0] == '^') {
		negated = true
		class = class[1:]
	}
	matched := false
	for i := 0; i < len(class); i++ {
		if i+2 < len(class) && class[i+1] == '-' {
			if class[i] <= c && c <= class[i+2] {
				matched = true
			}
			i += 2
			continue
		}
		if class[i] == c {
			matched = true
		}
	}
	if negated {
		return !matched
	}
	return matched
}

// shellPathBase returns the base name of an argv[0] the way Python's
// PurePosixPath(...).name does, independent of the host separator.
func shellPathBase(program string) string {
	if runtime.GOOS == "windows" {
		program = strings.ReplaceAll(program, "\\", "/")
	}
	return program[strings.LastIndex(program, "/")+1:]
}
