package toolpacks

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// writeTree creates files (slash-separated paths relative to dir) with content.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestShellIgnoreDirectoryReaders: grep and diff open files under a directory
// operand on their own, so the ignore list must be applied to every file there,
// not only to the literal words of the command.
func TestShellIgnoreDirectoryReaders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the restricted exec path relies on POSIX binaries")
	}
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".env":         "SECRET=1",
		"sub/.env":     "SECRET=2",
		"src/main.txt": "foo",
	})
	s, err := NewShellTool(WithShellWorkdir(dir), WithShellReadonly(), WithShellIgnore(".env"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, cx := newTestContext()
	for _, command := range []string{
		"grep -r SECRET .",
		"grep -r SECRET",
		"grep -R SECRET sub",
		"diff -r . sub",
		"diff . sub",
	} {
		got := resultPartText(t, callTool(t, s, cx, s.Name(), `{"command":`+quoteGo(command)+`}`))
		if !strings.Contains(got, "Access denied") || strings.Contains(got, "SECRET=") {
			t.Errorf("%q output = %q, want Access denied without the secret", command, got)
		}
	}
	// A directory without an ignored file still works.
	got := resultPartText(t, callTool(t, s, cx, s.Name(), `{"command":"grep -r foo src"}`))
	if strings.Contains(got, "Access denied") || !strings.Contains(got, "foo") {
		t.Errorf("grep -r foo src = %q, want the match", got)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

func denies(got string) bool { return strings.HasPrefix(got, "Access denied") }

// TestShellIgnoreGrepOperandParsing: option parsing errs towards "fewer file
// operands", which widens the check to the whole working directory.
func TestShellIgnoreGrepOperandParsing(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".env": "SECRET=1", "src/a.txt": "foo", "pats": "foo"})
	patterns := []string{".env"}
	for _, tc := range []struct {
		cmd    string
		denied bool
	}{
		{"grep -r foo src", false},
		{"grep -r -e foo src", false},
		{"grep -rn --regexp=foo src", false},
		{"grep -f pats src", false},
		{"grep -r -e foo", true},                  // pattern given by -e: no file operand, searches workdir
		{"grep -r --regexp foo", true},            // long option with a separate value
		{"grep -r --reg foo", true},               // unambiguous long prefix still takes a value
		{"grep -r --exclude-dir src foo", true},   // value of --exclude-dir is not a file
		{"grep -rA2 foo", true},                   // glued short value
		{"grep -r -A 2 foo", true},                // separate short value
		{"grep -r foo nothere", true},             // operand that does not exist
		{"egrep -r foo", true},                    // family members
		{"fgrep -r foo", true},                    //
		{"/usr/bin/grep -r foo", true},            // program matched by base name
		{"grep -r -- foo", true},                  // after "--" the first word is still the pattern
		{"grep -r foo -- src", false},             //
		{"grep -r foo src --include=*.go", false}, // option after the operand
		{"diff src/a.txt pats", false},            // diff of two plain files
	} {
		got := checkIgnoreLimit(tc.cmd, dir, patterns, shellIgnoreMaxEntries)
		if denies(got) != tc.denied {
			t.Errorf("%q = %q, denied want %v", tc.cmd, got, tc.denied)
		}
	}
}

// TestShellIgnoreAliasSymlink: a directory is walked under the name it was
// given, because an alias may have its own ignore rule, and under its real path.
func TestShellIgnoreAliasSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation")
	}
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"pub/a.txt": "foo", "pub/.env": "SECRET=1"})
	mustSymlink(t, "pub", filepath.Join(dir, "alias"))

	// The rule names the alias path only; the real files "pub/a.txt" do not match it.
	got := checkIgnoreLimit("grep -r foo alias", dir, []string{"alias/*.txt"}, shellIgnoreMaxEntries)
	if !denies(got) {
		t.Errorf("alias rule not applied during the walk: %q", got)
	}
	// And the real target is checked when the alias name is not ignored.
	got = checkIgnoreLimit("grep -r foo alias", dir, []string{".env"}, shellIgnoreMaxEntries)
	if !denies(got) {
		t.Errorf("real path not checked during the walk: %q", got)
	}
	// A symlink entry inside a walked directory whose target is ignored.
	writeTree(t, dir, map[string]string{"top/.keep": "", ".secret": "x"})
	mustSymlink(t, "../.secret", filepath.Join(dir, "top", "link"))
	if got := checkIgnoreLimit("grep -r foo top", dir, []string{".secret"}, shellIgnoreMaxEntries); !denies(got) {
		t.Errorf("symlink to an ignored file is not followed: %q", got)
	}
}

// TestShellIgnoreSymlinkLoop: a symlink back to an ancestor is not entered
// again, so a clean tree with a loop is allowed and costs few entries.
func TestShellIgnoreSymlinkLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation")
	}
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"sub/a.txt": "foo"})
	mustSymlink(t, "..", filepath.Join(dir, "sub", "up"))
	mustSymlink(t, "../sub", filepath.Join(dir, "sub", "self"))
	if got := checkIgnoreLimit("grep -r foo sub", dir, []string{".env"}, 30); got != "" {
		t.Errorf("loop in a clean tree = %q, want allowed", got)
	}
	// The ignored file behind the loop is still found once.
	writeTree(t, dir, map[string]string{".env": "SECRET=1"})
	if got := checkIgnoreLimit("grep -r foo sub", dir, []string{".env"}, 30); !denies(got) {
		t.Errorf("ignored file behind a loop = %q, want denied", got)
	}
}

// TestShellIgnoreUnlistableDirectory: a directory the filter cannot read is
// refused, both as a walk operand and on the path of a wildcard. The tree is a
// copy in t.TempDir with mode 000; os.ReadDir is not replaced.
func TestShellIgnoreUnlistableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Fatal("this test needs a POSIX host and a non-root user: chmod 000 must make a directory unlistable")
	}
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"locked/inner/x.txt": "foo", "open/y.txt": "foo"})
	locked := filepath.Join(dir, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	for _, cmd := range []string{
		"grep -r foo locked",
		"cat locked/*/x.txt",
	} {
		got := checkIgnoreLimit(cmd, dir, []string{".env"}, shellIgnoreMaxEntries)
		if !denies(got) {
			t.Errorf("%q = %q, want Access denied", cmd, got)
		}
	}
	if got := checkIgnoreLimit("cat open/*.txt", dir, []string{".env"}, shellIgnoreMaxEntries); got != "" {
		t.Errorf("listable wildcard = %q, want allowed", got)
	}
}

// TestShellIgnoreRecursiveWildcard: "**" cannot be bounded by the budget.
func TestShellIgnoreRecursiveWildcard(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a/b/c.txt": "x"})
	for _, cmd := range []string{"cat **/c.txt", "cat a/**", "cat a/**/c.txt", "ls ./**"} {
		if got := checkIgnoreLimit(cmd, dir, []string{".env"}, shellIgnoreMaxEntries); !denies(got) {
			t.Errorf("%q = %q, want Access denied", cmd, got)
		}
	}
	if got := checkIgnoreLimit("cat a/*/c.txt", dir, []string{".env"}, shellIgnoreMaxEntries); got != "" {
		t.Errorf("single-level wildcard = %q, want allowed", got)
	}
}

// TestShellIgnoreAbsoluteWildcard: an absolute wildcard inside the workdir is
// expanded and checked; one outside it is refused as a path.
func TestShellIgnoreAbsoluteWildcard(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".env": "SECRET=1", "ok.txt": "x"})
	if got := checkIgnoreLimit("cat "+dir+"/.e*", dir, []string{".env"}, shellIgnoreMaxEntries); !denies(got) {
		t.Errorf("absolute wildcard onto an ignored file = %q, want denied", got)
	}
	if got := checkIgnoreLimit("cat "+dir+"/o*.txt", dir, []string{".env"}, shellIgnoreMaxEntries); got != "" {
		t.Errorf("absolute wildcard onto a plain file = %q, want allowed", got)
	}
	if got := checkIgnoreLimit("cat /etc/pass*", dir, []string{".env"}, shellIgnoreMaxEntries); !denies(got) {
		t.Errorf("absolute wildcard outside the workdir = %q, want denied", got)
	}
}

// TestShellIgnoreBudget: matches and walked entries share one counter, every
// directory entry read is charged (also the ones that do not match), and each
// wildcard is expanded once.
func TestShellIgnoreBudget(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 10; i++ {
		files[filepath.ToSlash(filepath.Join("src", string(rune('a'+i)), "f.txt"))] = "x"
		files[filepath.ToSlash(filepath.Join("flat", "n"+string(rune('a'+i))))] = "x"
	}
	writeTree(t, dir, files)
	p := []string{".env"}

	// Walk: src has 10 dirs + 10 files = 20 entries.
	if got := checkIgnoreLimit("grep -r foo src", dir, p, 19); !denies(got) || !strings.Contains(got, "smaller directory") {
		t.Errorf("walk over the budget = %q, want denied with a hint", got)
	}
	if got := checkIgnoreLimit("grep -r foo src", dir, p, 20); got != "" {
		t.Errorf("walk at the budget = %q, want allowed", got)
	}
	// "src/*/*/nothing" reads src (10 entries) and then each subdirectory (1 each)
	// and matches nothing at the end: the scan is not free.
	if got := checkIgnoreLimit("cat src/*/*/nothing", dir, p, 19); !denies(got) {
		t.Errorf("wildcard scan without matches over the budget = %q, want denied", got)
	}
	if got := checkIgnoreLimit("cat src/*/*/nothing", dir, p, 20); got != "" {
		t.Errorf("wildcard scan at the budget = %q, want allowed", got)
	}
	// Entries that do not match the wildcard are charged too: "z*" matches none of
	// the 10 names in flat, yet all 10 were read.
	if got := checkIgnoreLimit("cat flat/z*", dir, p, 9); !denies(got) {
		t.Errorf("non-matching entries not charged = %q, want denied", got)
	}
	if got := checkIgnoreLimit("cat flat/z*", dir, p, 10); got != "" {
		t.Errorf("non-matching wildcard at the budget = %q, want allowed", got)
	}
	// Matches and walked entries share one counter: "cat flat/n*" reads 10 entries,
	// "grep -r foo src" walks 20; together 30.
	if got := checkIgnoreLimit("grep -r foo src flat/n*", dir, p, 29); !denies(got) {
		t.Errorf("shared counter = %q, want denied at 29", got)
	}
	if got := checkIgnoreLimit("grep -r foo src flat/n*", dir, p, 30); got != "" {
		t.Errorf("shared counter = %q, want allowed at 30", got)
	}
	// A wildcard is expanded once: 10 entries, limit 10 passes, 9 fails.
	if got := checkIgnoreLimit("cat flat/*", dir, p, 10); got != "" {
		t.Errorf("single expansion = %q, want allowed at the exact size", got)
	}
	if got := checkIgnoreLimit("cat flat/*", dir, p, 9); !denies(got) {
		t.Errorf("single expansion = %q, want denied under the size", got)
	}
}

// TestShellIgnoreBudgetDefault: the default budget is 20 000 entries.
func TestShellIgnoreBudgetDefault(t *testing.T) {
	if shellIgnoreMaxEntries != 20000 {
		t.Fatalf("shellIgnoreMaxEntries = %d, want 20000", shellIgnoreMaxEntries)
	}
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	if err := os.MkdirAll(big, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < shellIgnoreMaxEntries+1; i++ {
		f, err := os.Create(filepath.Join(big, "f"+strconvItoa(i)))
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	s, err := NewShellTool(WithShellWorkdir(dir), WithShellReadonly(), WithShellIgnore(".env"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, cx := newTestContext()
	for _, command := range []string{"grep -r x big", "ls big/*/nothing", "ls big/*"} {
		got := resultPartText(t, callTool(t, s, cx, s.Name(), `{"command":`+quoteGo(command)+`}`))
		if !denies(got) || !strings.Contains(got, "smaller directory") {
			t.Errorf("%q over 20000 entries = %q, want a budget refusal", command, got)
		}
	}
}

// TestShellIgnoreUnresolvablePath: a path that cannot be resolved is refused,
// not skipped. A relative workdir whose current directory was removed cannot be
// made absolute.
func TestShellIgnoreUnresolvablePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("removing the current directory")
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", orig) // keep os.Getwd from trusting a stale $PWD
	if err := os.Chdir(gone); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Getwd(); err == nil {
		t.Fatal("precondition: Getwd must fail in a removed directory")
	}
	if got := checkIgnoreLimit("cat a.txt", "relative-workdir", []string{".env"}, shellIgnoreMaxEntries); !denies(got) {
		t.Errorf("unresolvable workdir = %q, want Access denied", got)
	}
}

// TestShellIgnoreSymlinkCycleIsUnresolvable: a symlink loop on the path is refused.
func TestShellIgnoreSymlinkCycleIsUnresolvable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation")
	}
	dir := t.TempDir()
	mustSymlink(t, "b", filepath.Join(dir, "a"))
	mustSymlink(t, "a", filepath.Join(dir, "b"))
	if got := checkIgnoreLimit("cat a", dir, []string{".env"}, shellIgnoreMaxEntries); !denies(got) {
		t.Errorf("symlink cycle = %q, want Access denied", got)
	}
}

// TestShellIgnoreRemoteDirectoryReaders: a remote backend cannot be walked from
// the host, so grep and diff are refused outright while an ignore list is set.
func TestShellIgnoreRemoteDirectoryReaders(t *testing.T) {
	for _, cmd := range []string{"grep -r foo src", "egrep foo f", "fgrep foo f", "diff a b", "/usr/bin/grep foo f"} {
		if got := checkIgnorePosix(cmd, "/workspace", []string{".env"}); !denies(got) {
			t.Errorf("%q = %q, want Access denied", cmd, got)
		}
	}
	if got := checkIgnorePosix("cat f", "/workspace", []string{".env"}); got != "" {
		t.Errorf("cat on a remote workdir = %q, want allowed", got)
	}
	f := newMemFactory()
	remote := &workdirFactory{memFactory: f, dir: "/workspace"}
	s, err := NewShellTool(WithShellSandbox(remote), WithShellAllowed("grep", "ls"), WithShellIgnore(".env"))
	if err != nil {
		t.Fatal(err)
	}
	_, cx := newTestContext()
	if got := resultPartText(t, callTool(t, s, cx, s.Name(), `{"command":"grep -r foo ."}`)); !denies(got) {
		t.Errorf("remote grep with ignore = %q, want Access denied", got)
	}
	if got := resultPartText(t, callTool(t, s, cx, s.Name(), `{"command":"ls src"}`)); denies(got) {
		t.Errorf("remote ls = %q, want allowed", got)
	}
}

func strconvItoa(i int) string { return strconv.Itoa(i) }
