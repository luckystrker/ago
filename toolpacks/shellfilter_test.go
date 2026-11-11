package toolpacks

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestShellRestrictedAllowed covers the whitelist: a listed command runs as a
// checked argv, an unlisted one is refused, and the argv seam shows no shell
// interpreter is inserted.
func TestShellRestrictedAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the restricted exec path relies on POSIX binaries")
	}
	s, err := NewShellTool(WithShellAllowed("echo"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	argv, err := s.argvFor("echo hi")
	if err != nil {
		t.Fatalf("argvFor: %v", err)
	}
	if strings.Join(argv, "|") != "echo|hi" {
		t.Fatalf("restricted argv = %v, want [echo hi] (no sh -c wrapper)", argv)
	}
	if !s.restricted {
		t.Fatal("WithShellAllowed must switch on restricted mode")
	}

	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":"echo restricted_ok"}`)
	if got := resultPartText(t, ev); got != "restricted_ok" {
		t.Fatalf("allowed command output = %q", got)
	}

	// A bare variable is not expanded because no shell ran: echo prints the
	// literal text the check saw.
	ev = callTool(t, s, cx, s.Name(), `{"command":"echo $HOME"}`)
	if got := resultPartText(t, ev); got != "$HOME" {
		t.Fatalf("restricted echo output = %q, want the literal $HOME (no shell expansion)", got)
	}

	ev = callTool(t, s, cx, s.Name(), `{"command":"touch not_allowed.txt"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "Command not allowed") {
		t.Fatalf("unlisted command output = %q, want a refusal", got)
	}
}

// TestShellRestrictedShellSyntax: restricted mode rejects pipes, redirects and
// chaining instead of letting a shell interpret them after the check.
func TestShellRestrictedShellSyntax(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the restricted exec path relies on POSIX binaries")
	}
	s, err := NewShellTool(WithShellAllowed("echo"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":"echo hi | cat"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "shell syntax is not available") {
		t.Fatalf("piped command output = %q, want the shell-syntax refusal", got)
	}
	ev = callTool(t, s, cx, s.Name(), `{"command":"echo hi > out.txt"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "shell syntax is not available") {
		t.Fatalf("redirected command output = %q, want the shell-syntax refusal", got)
	}
}

// TestShellRestrictedBlocked: in restricted mode the blocklist matches the
// checked argv word by word, so a listed-but-blocked program is refused.
func TestShellRestrictedBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the restricted exec path relies on POSIX binaries")
	}
	s, err := NewShellTool(WithShellAllowed("echo", "rm"), WithShellBlockPatterns("rm"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":"rm -f nope"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "Command not allowed") {
		t.Fatalf("blocked command output = %q, want a refusal", got)
	}
	ev = callTool(t, s, cx, s.Name(), `{"command":"echo still_ok"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "still_ok") {
		t.Fatalf("unblocked command output = %q", got)
	}
}

// TestShellReadonly: the readonly preset blocks writes and runs read commands.
func TestShellReadonly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the restricted exec path relies on POSIX binaries")
	}
	dir := t.TempDir()
	s, err := NewShellTool(WithShellWorkdir(dir), WithShellReadonly())
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":"echo readonly_ok"}`)
	if got := resultPartText(t, ev); got != "readonly_ok" {
		t.Fatalf("readonly echo output = %q", got)
	}
	ev = callTool(t, s, cx, s.Name(), `{"command":"touch written.txt"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "Command not allowed") {
		t.Fatalf("readonly write output = %q, want a refusal", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "written.txt")); !os.IsNotExist(err) {
		t.Fatalf("readonly write created the file: %v", err)
	}
}

// TestShellIgnore: literal paths leaving the workdir and ignore-glob matches
// are refused before execution, including paths embedded in an option value.
func TestShellIgnore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the restricted exec path relies on POSIX binaries")
	}
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ok.txt", "ignore_ok")
	write(".env", "SECRET=1")
	write("key.pem", "PEM")
	write("nested/key.pem", "PEM")
	write("secrets/file.txt", "SECRET")
	s, err := NewShellTool(WithShellWorkdir(dir), WithShellAllowed("cat"),
		WithShellIgnore(".env", "**/*.pem", "secrets"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()

	ev := callTool(t, s, cx, s.Name(), `{"command":"cat ok.txt"}`)
	if got := resultPartText(t, ev); got != "ignore_ok" {
		t.Fatalf("allowed cat output = %q", got)
	}
	for _, command := range []string{
		"cat .env",
		`cat ".env"`,           // quoted literal still resolves to .env
		"cat -o.env",           // short-option tail (embeddedPaths)
		"cat --output=.env",    // "=" tail
		"cat key.pem",          // literal name
		"cat *.pem",            // glob token, matches a host file
		"cat nested/key.pem",   // glob pattern with a directory prefix
		"cat secrets/file.txt", // literal pattern with a directory prefix
		"cat ..",               // the workdir itself escaped
		"cat ../outside.txt",
		"cat ../../etc/passwd", // escapes the workdir
	} {
		ev := callTool(t, s, cx, s.Name(), `{"command":`+quoteGo(command)+`}`)
		if got := resultPartText(t, ev); !strings.Contains(got, "Access denied") {
			t.Fatalf("%q output = %q, want Access denied", command, got)
		}
	}
}

// TestShellIgnoreGlobWithoutBaseNameMatch: a glob pattern that matches the
// relative path but not the base name alone is still refused (the deniedPath
// rel/name disjunction).
func TestShellIgnoreGlobWithoutBaseNameMatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the restricted exec path relies on POSIX binaries")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "key.pem"), []byte("PEM"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewShellTool(WithShellWorkdir(dir), WithShellAllowed("cat"),
		WithShellIgnore("nested/*.pem"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":"cat nested/key.pem"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "Access denied") {
		t.Fatalf("nested glob output = %q, want Access denied", got)
	}
}

// TestShellIgnoreSymlinkEscape: the ignore guard resolves paths physically. A
// symlink to another directory plus ".." (or a not-yet-existing write target
// under a symlinked directory) must not smuggle a path outside the workdir
// past the lexical check; an ignore pattern reached through a symlink is
// refused the same way.
func TestShellIgnoreSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation and POSIX binaries")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "passwd"), []byte("OUTSIDE_SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "vault"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vault", ".env"), []byte("IN_ROOT_SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("inside_ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"link":      filepath.Join(outside, "sub"),
		"outlink":   outside,
		"vaultlink": filepath.Join(root, "vault"),
		"inside":    filepath.Join(root, "inside.txt"),
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Skipf("symlink: %v", err)
		}
	}

	s, err := NewShellTool(WithShellWorkdir(root), WithShellAllowed("cat", "touch"),
		WithShellIgnore(".env"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()

	for _, command := range []string{
		"cat link/../passwd",    // ".." after a symlinked directory
		"cat outlink/passwd",    // through a symlinked directory
		"cat vaultlink/.env",    // ignore pattern reached through a symlink
		"touch outlink/newfile", // write target under a symlinked directory
	} {
		ev := callTool(t, s, cx, s.Name(), `{"command":`+quoteGo(command)+`}`)
		got := resultPartText(t, ev)
		if !strings.Contains(got, "Access denied") {
			t.Fatalf("%q output = %q, want Access denied", command, got)
		}
		if strings.Contains(got, "OUTSIDE_SECRET") || strings.Contains(got, "IN_ROOT_SECRET") {
			t.Fatalf("%q leaked file content: %q", command, got)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "newfile")); !os.IsNotExist(err) {
		t.Fatalf("write through a symlink created a file outside the workdir: %v", err)
	}

	// An in-root symlink to an allowed file still reads through.
	ev := callTool(t, s, cx, s.Name(), `{"command":"cat inside"}`)
	if got := resultPartText(t, ev); got != "inside_ok" {
		t.Fatalf("in-root symlink read = %q", got)
	}
}

// TestMatchGlob pins the fnmatch-style matcher: "*" crosses path separators,
// "?" matches one rune, classes support ranges and negation, an unterminated
// "[" is a literal.
func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*.pem", "a.pem", true},
		{"*.pem", "dir/a.pem", true},
		{"*.pem", "a.txt", false},
		{"**/*.pem", "dir/a.pem", true},
		{"**/*.pem", "a.pem", false}, // fnmatch needs the slash; deniedPath handles "**/" via the name
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"[ab]x", "ax", true},
		{"[ab]x", "cx", false},
		{"[!a]x", "bx", true},
		{"[!a]x", "ax", false},
		{"[a-c]x", "bx", true},
		{"[a-c]x", "dx", false},
		{"[a-c]", "a", true},
		{"[a-c]", "c", true},
		{"[a-c]", "d", false},
		{"[a-]", "a", true},
		{"[]]", "]", true},
		{"[", "", false},
		{"[", "x", false},
		{"**", "", true},
		{"a*", "a", true},
		{"[abc", "[abc", true},
		{"a*b", "ab", true},
		{"a*b", "axxb", true},
		{"a*b", "axx", false},
		{"*", "", true},
	}
	for _, tc := range cases {
		if got := matchGlob(tc.pattern, tc.s); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

// TestSplitCommand pins POSIX quoting, including the double-quote backslash
// rules and the unbalanced-quote/trailing-escape failures.
func TestSplitCommand(t *testing.T) {
	ok := []struct {
		command string
		want    []string
	}{
		{`echo hi`, []string{"echo", "hi"}},
		{`echo "a\"b"`, []string{"echo", `a"b`}},
		{`echo "a\\b"`, []string{"echo", `a\b`}},
		{`echo "a\$b"`, []string{"echo", "a$b"}},
		{`echo "a\qb"`, []string{"echo", `a\qb`}},
		{`echo 'a b'`, []string{"echo", "a b"}},
		{`echo a\ b`, []string{"echo", "a b"}},
		{`echo ""`, []string{"echo", ""}},
		{"echo  a\tb", []string{"echo", "a", "b"}},
	}
	for _, tc := range ok {
		got, err := splitCommand(tc.command)
		if err != nil {
			t.Errorf("splitCommand(%q): %v", tc.command, err)
			continue
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("splitCommand(%q) = %q, want %q", tc.command, got, tc.want)
		}
	}
	for _, command := range []string{`echo "unterminated`, `echo 'unterminated`, `echo trailing\`} {
		if _, err := splitCommand(command); err == nil {
			t.Errorf("splitCommand(%q) = nil error, want a refusal", command)
		}
	}
}

// TestContainsShellOperator: operators inside quotes or escaped are not shell
// syntax; unquoted ones are.
func TestContainsShellOperator(t *testing.T) {
	cases := []struct {
		command string
		want    bool
	}{
		{"echo 'a|b'", false},
		{`echo "a;b"`, false},
		{`echo a\|b`, false},
		{"echo a|b", true},
		{`echo "a" 'b' > out`, true},
		{`echo $(date)`, true},
	}
	for _, tc := range cases {
		if got := containsShellOperator(tc.command); got != tc.want {
			t.Errorf("containsShellOperator(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

// TestMatchesArgv pins word-prefix matching, including the whole-word boundary
// and the too-short argv case.
func TestMatchesArgv(t *testing.T) {
	cases := []struct {
		pattern string
		argv    []string
		want    bool
	}{
		{"git log", []string{"git", "log", "-5"}, true},
		{"git log", []string{"git", "-c", "x", "log"}, false},
		{"git", []string{"git"}, true},
		{"git", []string{"gitconfig"}, false},
		{"git log", []string{"git"}, false},
		{"", []string{"git"}, true},
	}
	for _, tc := range cases {
		if got := matchesArgv(tc.pattern, tc.argv); got != tc.want {
			t.Errorf("matchesArgv(%q, %v) = %v, want %v", tc.pattern, tc.argv, got, tc.want)
		}
	}
}

// TestShellRestrictedQuotedOperators: an operator inside quotes is data, not
// shell syntax, and the checked argv runs it without a shell.
func TestShellRestrictedQuotedOperators(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the restricted exec path relies on POSIX binaries")
	}
	s, err := NewShellTool(WithShellAllowed("echo"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	for command, want := range map[string]string{
		`echo "a|b"`: "a|b",
		`echo 'a;b'`: "a;b",
		`echo a\|b`:  "a|b",
	} {
		ev := callTool(t, s, cx, s.Name(), `{"command":`+quoteGo(command)+`}`)
		if got := resultPartText(t, ev); got != want {
			t.Errorf("%q output = %q, want %q", command, got, want)
		}
	}
}

// TestShellRestrictedBasenameBlock: a blocked program is matched by its base
// name too, so /bin/rm is caught by the "rm" prefix.
func TestShellRestrictedBasenameBlock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the restricted exec path relies on POSIX binaries")
	}
	s, err := NewShellTool(WithShellAllowed("/bin/rm"), WithShellBlockPatterns("rm"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":"/bin/rm -f nope"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "Command not allowed") {
		t.Fatalf("/bin/rm output = %q, want a refusal by basename", got)
	}
}

// TestShellRestrictedPreset pins the option wiring: ignore alone and readonly
// alone switch on restricted mode, and an explicit allowlist wins over the
// readonly preset.
func TestShellRestrictedPreset(t *testing.T) {
	ignoreOnly, err := NewShellTool(WithShellIgnore(".env"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer ignoreOnly.Close()
	if !ignoreOnly.restricted {
		t.Fatal("WithShellIgnore must switch on restricted mode")
	}
	if ignoreOnly.allowed != nil {
		t.Fatalf("ignore-only allowed = %v, want nil", ignoreOnly.allowed)
	}

	readOnly, err := NewShellTool(WithShellReadonly())
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer readOnly.Close()
	if !readOnly.restricted {
		t.Fatal("WithShellReadonly must switch on restricted mode")
	}
	if got := strings.Join(readOnly.allowed, ","); got != strings.Join(ReadonlyCommands(), ",") {
		t.Fatalf("readonly whitelist = %v, want ReadonlyCommands", readOnly.allowed)
	}

	explicit, err := NewShellTool(WithShellReadonly(), WithShellAllowed("echo"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer explicit.Close()
	if !explicit.restricted {
		t.Fatal("an explicit allowlist must switch on restricted mode")
	}
	if len(explicit.allowed) != 1 || explicit.allowed[0] != "echo" {
		t.Fatalf("readonly overwrote the explicit allowlist: %v", explicit.allowed)
	}
}

// TestEmbeddedPaths pins the option-value extraction: only "=" tails and
// tails glued to a short option are added; long options are left whole.
func TestEmbeddedPaths(t *testing.T) {
	cases := []struct {
		token string
		want  []string
	}{
		{"file.txt", []string{"file.txt"}},
		{"--output=.env", []string{"--output=.env", "--output", ".env"}},
		{"-o.env", []string{"-o.env", ".env", "env", "nv", "v"}},
		{"--long.name", []string{"--long.name"}},
		{"=x", []string{"=x", "x"}},
	}
	for _, tc := range cases {
		got := embeddedPaths(tc.token)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("embeddedPaths(%q) = %q, want %q", tc.token, got, tc.want)
		}
	}
}

// TestUnquoteShell pins the operator scan: quoted and escaped text disappears,
// including the double-quote backslash rule.
func TestUnquoteShell(t *testing.T) {
	cases := []struct {
		command string
		want    string
	}{
		{"a|b", "a|b"},
		{"'a|b'", ""},
		{`"a|b"`, ""},
		{"a\\|b", "ab"},
		{`"a\"|b"`, ""},
		{`'a\"|b'`, ""},
		{"pre-`x`", "pre-`x`"},
	}
	for _, tc := range cases {
		if got := unquoteShell(tc.command); got != tc.want {
			t.Errorf("unquoteShell(%q) = %q, want %q", tc.command, got, tc.want)
		}
	}
}

// TestResolvePhysical: a relative path gains the working directory without a
// lexical clean, and an existing symlinked prefix plus ".." resolves
// physically.
func TestResolvePhysical(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got := resolvePhysical("relative.txt")
	if want := filepath.Join(wd, "relative.txt"); got != want {
		t.Fatalf("resolvePhysical(relative.txt) = %q, want %q", got, want)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "target", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target", "sub"), filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	got = resolvePhysical(root + string(filepath.Separator) + "link/../new.txt")
	want := filepath.Join(root, "target", "new.txt")
	if got != want {
		t.Fatalf("resolvePhysical(link/../new.txt) = %q, want %q", got, want)
	}
}

// TestShellPathBase pins base-name extraction on POSIX paths; on Windows the
// backslash separator is normalized.
func TestShellPathBase(t *testing.T) {
	got := shellPathBase(`C:\bin\rm`)
	want := `C:\bin\rm`
	if runtime.GOOS == "windows" {
		want = "rm"
	}
	if got != want {
		t.Fatalf("shellPathBase = %q, want %q", got, want)
	}
	if got := shellPathBase("/bin/rm"); got != "rm" {
		t.Fatalf("shellPathBase(/bin/rm) = %q", got)
	}
}

// TestShellRestrictedEmptyCommand: an empty command has an empty argv; the
// blocklist guard must not index argv[0]. The run reports the subprocess'
// empty-argv exit code instead.
func TestShellRestrictedEmptyCommand(t *testing.T) {
	s, err := NewShellTool(WithShellIgnore(".env"), WithShellBlockPatterns("rm"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":""}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "[exit code: 2]") {
		t.Fatalf("empty command output = %q, want the empty-argv exit code", got)
	}
}

// TestCheckIgnoreSymlinkedWorkdir: a workdir that is itself a symlink resolves
// its real path before the escape/ignore checks, so a relative token is judged
// against the directory it really lives in.
func TestCheckIgnoreSymlinkedWorkdir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation")
	}
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, ".env"), []byte("SECRET=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "ok.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "workdir")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if got := checkIgnore("cat .env", link, []string{".env"}); !strings.Contains(got, "Access denied") {
		t.Fatalf("ignored path through a symlinked workdir = %q, want Access denied", got)
	}
	if got := checkIgnore("cat ../outside.txt", link, []string{".env"}); !strings.Contains(got, "Access denied") {
		t.Fatalf("escape from a symlinked workdir = %q, want Access denied", got)
	}
	if got := checkIgnore("cat ok.txt", link, []string{".env"}); got != "" {
		t.Fatalf("allowed path through a symlinked workdir = %q, want no denial", got)
	}
}

// TestShellIgnoreGlobTokenLiteralPattern: a glob token (".en?") expands on the
// host and every match is checked, even when the ignore pattern is a literal
// name (".env").
func TestShellIgnoreGlobTokenLiteralPattern(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the restricted exec path relies on POSIX binaries")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewShellTool(WithShellWorkdir(dir), WithShellAllowed("cat"), WithShellIgnore(".env"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":"cat .en?"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "Access denied") {
		t.Fatalf("glob token output = %q, want Access denied", got)
	}
}

// TestShellRestrictedUnbalancedQuotes: a command whose quotes do not balance
// or ends with a dangling escape is refused instead of being handed to a shell.
func TestShellRestrictedUnbalancedQuotes(t *testing.T) {
	for _, command := range []string{
		`echo "unterminated`,
		`echo 'unterminated`,
		`echo trailing\`,
		"echo \"dangling\\",
	} {
		s, err := NewShellTool(WithShellAllowed("echo"))
		if err != nil {
			t.Fatalf("NewShellTool: %v", err)
		}
		_, cx := newTestContext()
		ev := callTool(t, s, cx, s.Name(), `{"command":`+quoteGo(command)+`}`)
		s.Close()
		if got := resultPartText(t, ev); !strings.Contains(got, "unbalanced quotes") {
			t.Fatalf("%q output = %q, want the unbalanced-quotes refusal", command, got)
		}
	}
}
