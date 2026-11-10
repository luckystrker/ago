package toolpacks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/tools"
)

// TestSkillsFullFrontmatter: the frontmatter fields beyond name/description are
// retained on the discovered SkillInfo.
func TestSkillsFullFrontmatter(t *testing.T) {
	st, err := NewSkillsToolkit(newSkillsFixture(t))
	if err != nil {
		t.Fatalf("NewSkillsToolkit: %v", err)
	}
	skills := st.Skills()
	if len(skills) != 2 {
		t.Fatalf("skills = %v, want alpha and beta", skills)
	}
	alpha := skills[0]
	if alpha.Name != "alpha" {
		t.Fatalf("first skill = %q", alpha.Name)
	}
	if alpha.Version != "1.2.0" || alpha.License != "Apache-2.0" ||
		alpha.Compatibility != "ago>=1" || alpha.AllowedTools != "read_file" {
		t.Fatalf("frontmatter not carried: %+v", alpha)
	}
	if alpha.Metadata["owner"] != "core" {
		t.Fatalf("metadata mapping not carried: %v", alpha.Metadata)
	}
	// beta has no name, so the directory name supplies it (lenient mode).
	if skills[1].Name != "beta" {
		t.Fatalf("fallback name = %q", skills[1].Name)
	}
	// gamma has no description and is skipped.
	for _, s := range skills {
		if s.Name == "gamma" {
			t.Fatal("a skill without a description must be skipped")
		}
	}
}

// TestSkillsStrictMetadataValidation covers the agentskills.io rules.
func TestSkillsStrictMetadataValidation(t *testing.T) {
	valid := parseFrontmatterDocument("---\nname: alpha\ndescription: ok\ncompatibility: ago>=1\nmetadata:\n  owner: core\n---\nbody")
	if err := validateSkillMetadataStrict("alpha", valid); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	cases := []struct {
		name string
		dir  string
		text string
	}{
		{"missing name", "alpha", "---\ndescription: ok\n---"},
		{"missing description", "alpha", "---\nname: alpha\n---"},
		{"name mismatch", "alpha", "---\nname: beta\ndescription: ok\n---"},
		{"bad charset", "alpha", "---\nname: Alpha_One\ndescription: ok\n---"},
		{"long description", "alpha", "---\nname: alpha\ndescription: " + strings.Repeat("x", 1025) + "\n---"},
		{"long compatibility", "alpha", "---\nname: alpha\ndescription: ok\ncompatibility: " + strings.Repeat("x", 501) + "\n---"},
		{"metadata not a mapping", "alpha", "---\nname: alpha\ndescription: ok\nmetadata: nope\n---"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateSkillMetadataStrict(tc.dir, parseFrontmatterDocument(tc.text)); err == nil {
				t.Fatalf("%s: validation passed, want an error", tc.name)
			}
		})
	}
}

// TestSkillsResourceCap: discovery stops at resourceCap and flags truncation;
// scripts/ is not part of the resource list.
func TestSkillsResourceCap(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "capped")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: capped\ndescription: many resources\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < resourceCap+5; i++ {
		p := filepath.Join(skillDir, "references", fmt.Sprintf("%03d.txt", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "scripts", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := NewSkillsToolkit(root)
	if err != nil {
		t.Fatalf("NewSkillsToolkit: %v", err)
	}
	info := st.Skills()[0]
	if len(info.Resources) != resourceCap {
		t.Fatalf("resources = %d, want %d", len(info.Resources), resourceCap)
	}
	if !info.ResourcesTruncated {
		t.Fatal("ResourcesTruncated = false, want the cap noticed")
	}
	for _, r := range info.Resources {
		if strings.HasPrefix(r, "scripts/") {
			t.Fatalf("scripts/ leaked into resources: %q", r)
		}
	}
}

// TestMemoryRuntime: code-defined skills serve instructions (static and
// rendered per load) and resources through the same toolkit surface.
func TestMemoryRuntime(t *testing.T) {
	skill, err := NewMemorySkill("memory-skill", "Memory skill for testing.")
	if err != nil {
		t.Fatalf("NewMemorySkill: %v", err)
	}
	skill.SetInstructions("Follow the memory procedure.")
	if err := skill.AddResource("notes.txt", func(*agocontext.Context) (string, error) {
		return "memory_resource", nil
	}); err != nil {
		t.Fatalf("AddResource: %v", err)
	}
	rt, err := NewMemoryRuntime(skill)
	if err != nil {
		t.Fatalf("NewMemoryRuntime: %v", err)
	}
	st, err := NewSkillsToolkitFromRuntime(rt)
	if err != nil {
		t.Fatalf("NewSkillsToolkitFromRuntime: %v", err)
	}
	_, cx := newTestContext()

	ev := callTool(t, st, cx, "load_skill", `{"name":"memory-skill"}`)
	body := resultPartText(t, ev)
	for _, want := range []string{"Follow the memory procedure.", "<file>notes.txt</file>", "</skill_content>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("load_skill body missing %q:\n%s", want, body)
		}
	}
	ev = callTool(t, st, cx, "read_skill_resource", `{"name":"memory-skill","resource":"notes.txt"}`)
	if got := resultPartText(t, ev); got != "memory_resource" {
		t.Fatalf("resource = %q", got)
	}

	// A callable body is rendered on every load, so it sees the live context.
	skill.SetInstructionsFunc(func(*agocontext.Context) (string, error) {
		return "rendered on load", nil
	})
	ev = callTool(t, st, cx, "load_skill", `{"name":"memory-skill"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "rendered on load") {
		t.Fatalf("rendered body = %q", got)
	}
}

// TestSkillPlugin: the catalog reaches the system prompt, the activation tools
// are the capability-gated set, and XML-sensitive text is escaped.
func TestSkillPlugin(t *testing.T) {
	st, err := NewSkillsToolkit(newSkillsFixture(t))
	if err != nil {
		t.Fatalf("NewSkillsToolkit: %v", err)
	}
	pl := NewSkillPlugin(st)
	fragments := pl.PromptFragments()
	if len(fragments) != 1 {
		t.Fatalf("prompt fragments = %v", fragments)
	}
	for _, want := range []string{
		"<available_skills>",
		"<name>alpha</name>",
		"<description>Alpha skill for testing.</description>",
		"<name>beta</name>",
		"</available_skills>",
	} {
		if !strings.Contains(fragments[0], want) {
			t.Fatalf("catalog missing %q:\n%s", want, fragments[0])
		}
	}
	var names []string
	for _, tool := range pl.Tools() {
		names = append(names, tool.Name())
	}
	// list_skills is replaced by the catalog; alpha has a resource, so the read
	// tool is present.
	if got := strings.Join(names, "|"); got != "load_skill|read_skill_resource" {
		t.Fatalf("plugin tools = %q", got)
	}

	// No resources -> no dead read tool.
	skill, err := NewMemorySkill("plain", "A skill without resources.")
	if err != nil {
		t.Fatalf("NewMemorySkill: %v", err)
	}
	skill.SetInstructions("body")
	rt, err := NewMemoryRuntime(skill)
	if err != nil {
		t.Fatalf("NewMemoryRuntime: %v", err)
	}
	plain, err := NewSkillsToolkitFromRuntime(rt)
	if err != nil {
		t.Fatalf("NewSkillsToolkitFromRuntime: %v", err)
	}
	plainPlugin := NewSkillPlugin(plain)
	var plainNames []string
	for _, tool := range plainPlugin.Tools() {
		plainNames = append(plainNames, tool.Name())
	}
	if got := strings.Join(plainNames, "|"); got != "load_skill" {
		t.Fatalf("resource-less plugin tools = %q, want only load_skill", got)
	}

	// No skills -> an empty plugin.
	empty, err := NewSkillsToolkit(t.TempDir())
	if err != nil {
		t.Fatalf("NewSkillsToolkit(empty): %v", err)
	}
	emptyPlugin := NewSkillPlugin(empty)
	if len(emptyPlugin.PromptFragments()) != 0 || len(emptyPlugin.Tools()) != 0 {
		t.Fatalf("empty plugin contributes %v / %v", emptyPlugin.PromptFragments(), emptyPlugin.Tools())
	}
}

// TestSkillPluginEscapesCatalog: a description containing XML syntax cannot
// break the catalog block.
func TestSkillPluginEscapesCatalog(t *testing.T) {
	skill, err := NewMemorySkill("escaped", "A & B <tag> Don't \"x\"\n")
	if err != nil {
		t.Fatalf("NewMemorySkill: %v", err)
	}
	skill.SetInstructions("body")
	rt, err := NewMemoryRuntime(skill)
	if err != nil {
		t.Fatalf("NewMemoryRuntime: %v", err)
	}
	st, err := NewSkillsToolkitFromRuntime(rt)
	if err != nil {
		t.Fatalf("NewSkillsToolkitFromRuntime: %v", err)
	}
	fragments := NewSkillPlugin(st).PromptFragments()
	if len(fragments) != 1 {
		t.Fatalf("fragments = %v", fragments)
	}
	if !strings.Contains(fragments[0], "A &amp; B &lt;tag&gt; Don't \"x\"\n") {
		t.Fatalf("catalog not escaped like saxutils.escape:\n%s", fragments[0])
	}
	if strings.Contains(fragments[0], "<tag>") {
		t.Fatalf("raw XML leaked into the catalog:\n%s", fragments[0])
	}
	if strings.Contains(fragments[0], "&#") {
		t.Fatalf("quotes/newlines must stay unchanged:\n%s", fragments[0])
	}
}

// writeSkillTree writes name -> content files under root.
func writeSkillTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSkillsToolkitMultiRuntimeLastWins: global + project directories are served
// together; on a name clash the last runtime shadows the earlier one, for the
// catalog, the body and the resources alike.
func TestSkillsToolkitMultiRuntimeLastWins(t *testing.T) {
	global, project := t.TempDir(), t.TempDir()
	writeSkillTree(t, global, map[string]string{
		"dup/SKILL.md":          "---\nname: dup\ndescription: global dup\n---\n\nGLOBAL BODY",
		"dup/references/old.md": "old resource",
		"only-global/SKILL.md":  "---\ndescription: from global\n---\n\nG",
	})
	writeSkillTree(t, project, map[string]string{
		"dup/SKILL.md":          "---\nname: dup\ndescription: project dup\n---\n\nPROJECT BODY",
		"dup/references/new.md": "new resource",
		"only-project/SKILL.md": "---\ndescription: from project\n---\n\nP",
	})
	st, err := NewSkillsToolkitFromDirs(global, project)
	if err != nil {
		t.Fatalf("NewSkillsToolkitFromDirs: %v", err)
	}
	_, cx := newTestContext()

	var names []string
	descr := map[string]string{}
	for _, s := range st.Skills() {
		names = append(names, s.Name)
		descr[s.Name] = s.Description
	}
	if strings.Join(names, ",") != "dup,only-global,only-project" || descr["dup"] != "project dup" {
		t.Fatalf("catalog = %v %v, want the merged set with the project dup winning", names, descr)
	}
	if got := resultPartText(t, callTool(t, st, cx, "load_skill", `{"name":"dup"}`)); !strings.Contains(got, "PROJECT BODY") || strings.Contains(got, "GLOBAL BODY") {
		t.Fatalf("load dup = %q, want the project body only", got)
	}
	if got := resultPartText(t, callTool(t, st, cx, "load_skill", `{"name":"only-global"}`)); !strings.Contains(got, "<skill_content") {
		t.Fatalf("load only-global = %q, want it served by the earlier runtime", got)
	}
	if got := resultPartText(t, callTool(t, st, cx, "read_skill_resource", `{"name":"dup","resource":"references/new.md"}`)); got != "new resource" {
		t.Fatalf("resource = %q", got)
	}
	// The shadowed skill's resource is not reachable through the winner.
	if got := resultPartText(t, callTool(t, st, cx, "read_skill_resource", `{"name":"dup","resource":"references/old.md"}`)); !strings.Contains(got, "TOOL_ERROR") {
		t.Fatalf("shadowed resource = %q, want an error", got)
	}
	// The name enum covers the merged set.
	schemas, _ := st.Schemas(cx)
	if b, _ := json.Marshal(schemas); !strings.Contains(string(b), `"only-global"`) || !strings.Contains(string(b), `"only-project"`) {
		t.Fatalf("schemas lack the merged enum: %s", b)
	}

	if _, err := st.Runtime().Load(cx, "ghost"); !errors.Is(err, ErrSkillNotFound) || !errors.Is(err, tools.ErrNotFound) {
		t.Fatalf("unknown skill error = %v, want ErrSkillNotFound and tools.ErrNotFound", err)
	}
	if _, err := NewSkillsToolkitFromRuntime(); err == nil {
		t.Fatal("no runtime must be rejected")
	}
}

// TestSkillsToolkitRunSkillScript: scripts run locally from the skill's scripts/
// directory with positional args, the deny rules refuse what must not run, and
// the member only exists when a runtime opts in.
func TestSkillsToolkitRunSkillScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scripts need sh")
	}
	root := t.TempDir()
	writeSkillTree(t, root, map[string]string{
		"tool/SKILL.md":            "---\ndescription: has scripts\n---\n\nbody",
		"tool/scripts/echo.sh":     "echo \"args:$1,$2\"\nexit 3\n",
		"tool/scripts/shebang":     "#!/bin/sh\necho shebang-ran\n",
		"tool/scripts/nested/a.sh": "echo nested-ran\n",
	})
	outside := filepath.Join(t.TempDir(), "outside.sh")
	if err := os.WriteFile(outside, []byte("echo ESCAPED\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "tool", "scripts", "leak.sh")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Without the opt-in the toolkit does not offer script execution.
	plain, err := NewSkillsToolkit(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range plain.Tools() {
		if m.Name() == "run_skill_script" {
			t.Fatal("run_skill_script must be opt-in")
		}
	}

	rt, err := NewLocalSkillRuntime(root, WithSkillScriptBlocked("rm"), WithSkillScriptTimeout(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewSkillsToolkitFromRuntime(rt)
	if err != nil {
		t.Fatal(err)
	}
	_, cx := newTestContext()
	run := func(args string) string {
		return resultPartText(t, callTool(t, st, cx, "run_skill_script", args))
	}
	if got := run(`{"name":"tool","script":"echo.sh","args":["a","b c"]}`); got != "args:a,b c\n[exit code: 3]" {
		t.Fatalf("echo.sh = %q, want its output and the exit-code suffix", got)
	}
	if got := run(`{"name":"tool","script":"shebang"}`); got != "shebang-ran" {
		t.Fatalf("shebang script = %q", got)
	}
	if got := run(`{"name":"tool","script":"nested/a.sh"}`); got != "nested-ran" {
		t.Fatalf("nested script = %q", got)
	}
	for _, bad := range []string{
		`{"name":"tool","script":"missing.sh"}`,                        // not listed
		`{"name":"tool","script":"../SKILL.md"}`,                       // outside scripts/
		`{"name":"tool","script":"leak.sh"}`,                           // symlink out of scripts/
		`{"name":"tool","script":"echo.sh","args":{"named":"object"}}`, // object args
	} {
		got := run(bad)
		if !strings.Contains(got, "TOOL_ERROR") || strings.Contains(got, "ESCAPED") {
			t.Fatalf("%s = %q, want a refusal", bad, got)
		}
	}

	// Blocked patterns match the built argv (here the sh interpreter's own name).
	blocked, err := NewLocalSkillRuntime(root, WithSkillScriptBlocked("sh ./echo.sh"))
	if err != nil {
		t.Fatal(err)
	}
	bst, _ := NewSkillsToolkitFromRuntime(blocked)
	if got := resultPartText(t, callTool(t, bst, cx, "run_skill_script", `{"name":"tool","script":"echo.sh"}`)); !strings.Contains(got, "Command not allowed") {
		t.Fatalf("blocked script = %q", got)
	}
}

// TestSkillScriptsRunInPluggableBackend: with a sandbox backend the argv goes to
// the backend instead of a local subprocess (the caller makes scripts reachable
// there).
func TestSkillScriptsRunInPluggableBackend(t *testing.T) {
	root := t.TempDir()
	writeSkillTree(t, root, map[string]string{
		"tool/SKILL.md":       "---\ndescription: has scripts\n---\n\nbody",
		"tool/scripts/run.sh": "echo hi\n",
	})
	f := newMemFactory()
	rt, err := NewLocalSkillRuntime(root, WithSkillScriptSandbox(f))
	if err != nil {
		t.Fatal(err)
	}
	_, cx := newTestContext()
	got, err := rt.RunScript(cx, "tool", "run.sh", []string{"x"})
	if err != nil || got != "not found\n[exit code: 127]" { // the fake backend answers 127 to any argv it does not know
		t.Fatalf("RunScript = %q, %v", got, err)
	}
	if len(f.sb.execs) != 1 || strings.Join(f.sb.execs[0], " ") != "sh ./run.sh x" || f.opens != f.releases {
		t.Fatalf("backend saw %v (opens=%d releases=%d), want one sh ./run.sh x", f.sb.execs, f.opens, f.releases)
	}
}

// TestSkillChainRunScriptRouting: run_skill_script through several runtimes goes
// to the last runtime that owns the skill; an earlier runtime serves skills the
// later ones lack; a later runtime without script execution shadows an earlier
// one instead of letting the call fall through to it.
func TestSkillChainRunScriptRouting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scripts need sh")
	}
	mk := func(skills map[string]string, opts ...LocalSkillOption) *LocalSkillRuntime {
		root := t.TempDir()
		files := map[string]string{}
		for name, out := range skills {
			files[name+"/SKILL.md"] = "---\ndescription: " + name + "\n---\n\nbody"
			files[name+"/scripts/go.sh"] = "echo " + out + "\n"
		}
		writeSkillTree(t, root, files)
		rt, err := NewLocalSkillRuntime(root, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}
	earlier := mk(map[string]string{"dup": "EARLIER", "only-early": "EARLY_ONLY", "shadowed": "EARLIER_SHADOWED"}, WithSkillScripts())
	later := mk(map[string]string{"dup": "LATER"}, WithSkillScripts())
	noScripts := mk(map[string]string{"shadowed": "NEVER"}) // owns "shadowed" but cannot run scripts
	st, err := NewSkillsToolkitFromRuntime(earlier, later, noScripts)
	if err != nil {
		t.Fatal(err)
	}
	_, cx := newTestContext()
	run := func(name string) string {
		return resultPartText(t, callTool(t, st, cx, "run_skill_script", `{"name":"`+name+`","script":"go.sh"}`))
	}
	if got := run("dup"); got != "LATER" {
		t.Fatalf("dup = %q, want the last owner (LATER)", got)
	}
	if got := run("only-early"); got != "EARLY_ONLY" {
		t.Fatalf("only-early = %q, want it served by the earlier runtime", got)
	}
	got := run("shadowed")
	if !strings.Contains(got, "TOOL_ERROR") || !strings.Contains(got, "does not execute scripts") || strings.Contains(got, "EARLIER_SHADOWED") {
		t.Fatalf("shadowed = %q, want the shadowing runtime's refusal, not the earlier runtime's script", got)
	}
	if got := run("ghost"); !strings.Contains(got, "TOOL_ERROR") || !strings.Contains(got, "not found") {
		t.Fatalf("ghost = %q, want a not-found error", got)
	}

	chain := st.Runtime().(*chainRuntime)
	if !chain.ScriptsEnabled() {
		t.Fatal("a chain with one script-capable runtime must report scripts enabled")
	}
	if (&chainRuntime{rts: []SkillRuntime{noScripts, noScripts}}).ScriptsEnabled() {
		t.Fatal("a chain of script-less runtimes must not report scripts enabled")
	}
	// Direct call: the error of the owning runtime comes back unchanged.
	if _, err := chain.RunScript(cx, "dup", "missing.sh", nil); !errors.Is(err, tools.ErrNotFound) {
		t.Fatalf("missing script through the chain: %v, want tools.ErrNotFound", err)
	}
}

// TestSkillScriptPython: a .py script without a shebang runs under the
// discovered python interpreter with its positional arguments.
func TestSkillScriptPython(t *testing.T) {
	if findPython() == "" {
		t.Skip("no python interpreter on PATH")
	}
	root := t.TempDir()
	writeSkillTree(t, root, map[string]string{
		"py/SKILL.md":      "---\ndescription: python script\n---\n\nbody",
		"py/scripts/hi.py": "import sys\nprint('py-ran', sys.argv[1])\n",
	})
	rt, err := NewLocalSkillRuntime(root, WithSkillScripts())
	if err != nil {
		t.Fatal(err)
	}
	_, cx := newTestContext()
	if got, err := rt.RunScript(cx, "py", "hi.py", []string{"X"}); err != nil || got != "py-ran X" {
		t.Fatalf("hi.py = %q, %v", got, err)
	}
}

// TestScriptCommandInterpreters pins the argv built for each script kind and
// the python fallback when no interpreter is on PATH.
func TestScriptCommandInterpreters(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	sh := write("a.sh", "echo\n")
	shebang := write("tool", "#!/bin/sh\necho\n")
	plain := write("noext", "data\n")
	py := write("a.py", "print(1)\n")
	for _, tc := range []struct {
		path, rel string
		want0     string // "" = the script itself
		wantExec  bool
	}{
		{sh, "a.sh", "sh", false},
		{shebang, "tool", "./tool", true},
		{plain, "noext", "./noext", true},
	} {
		argv, err := scriptCommand(tc.path, tc.rel, false)
		if err != nil {
			t.Fatal(err)
		}
		if argv[0] != tc.want0 {
			t.Errorf("%s: argv = %q, want it to start with %q", tc.rel, argv, tc.want0)
		}
		info, _ := os.Stat(tc.path)
		if got := info.Mode().Perm()&0o100 != 0; got != tc.wantExec {
			t.Errorf("%s: executable bit = %v, want %v", tc.rel, got, tc.wantExec)
		}
	}
	// python without an interpreter on PATH falls back to the literal names.
	t.Setenv("PATH", "")
	argv, err := scriptCommand(py, "a.py", false)
	if err != nil || argv[0] != "python3" || argv[1] != "./a.py" {
		t.Fatalf("a.py without interpreter = %q, %v, want python3 ./a.py", argv, err)
	}
	if argv, _ := scriptCommand(py, "a.py", true); argv[0] != "python" {
		t.Fatalf("host-python fallback = %q, want python", argv)
	}
}

// TestSkillScriptOptionZeroValues: a zero timeout or output cap is refused, not
// silently treated as unlimited.
func TestSkillScriptOptionZeroValues(t *testing.T) {
	root := t.TempDir()
	for name, opt := range map[string]LocalSkillOption{
		"timeout 0":    WithSkillScriptTimeout(0),
		"timeout <0":   WithSkillScriptTimeout(-time.Second),
		"max_output 0": WithSkillScriptMaxOutput(0),
		"block empty":  WithSkillScriptBlocked(" "),
		"nil sandbox":  WithSkillScriptSandbox(nil),
	} {
		if _, err := NewLocalSkillRuntime(root, opt); err == nil {
			t.Errorf("%s: want a constructor error", name)
		}
	}
	rt, err := NewLocalSkillRuntime(root, WithSkillScriptTimeout(time.Second))
	if err != nil || !rt.ScriptsEnabled() {
		t.Fatalf("a valid script option must opt in: %v, enabled=%v", err, rt != nil && rt.ScriptsEnabled())
	}
	if plain, _ := NewLocalSkillRuntime(root); plain.ScriptsEnabled() {
		t.Fatal("scripts must stay off without an option")
	}
}

func TestUseHostPython(t *testing.T) {
	for _, tc := range []struct {
		goos      string
		sandboxed bool
		want      bool
	}{
		{"windows", false, true},
		{"windows", true, false},
		{"linux", false, false},
		{"linux", true, false},
	} {
		if got := useHostPython(tc.goos, tc.sandboxed); got != tc.want {
			t.Errorf("useHostPython(%q, %v) = %v, want %v", tc.goos, tc.sandboxed, got, tc.want)
		}
	}
}

// TestSkillScriptGuards: the script lookup refuses what is not a listed regular
// file inside scripts/, and a shebang wins over the extension off Windows.
func TestSkillScriptGuards(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scripts need sh")
	}
	root := t.TempDir()
	writeSkillTree(t, root, map[string]string{
		"g/SKILL.md":           "---\ndescription: guards\n---\n\nbody",
		"g/scripts/ok.sh":      "echo ok\n",
		"g/scripts/sub/x.sh":   "echo x\n",
		"g/scripts/shebang.py": "#!/bin/sh\necho shebang-won\n",
	})
	if err := os.Symlink(filepath.Join(root, "g", "scripts", "sub"), filepath.Join(root, "g", "scripts", "dirlink")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	rt, err := NewLocalSkillRuntime(root, WithSkillScripts())
	if err != nil {
		t.Fatal(err)
	}
	// Created after discovery: inside scripts/ and a regular file, but not listed.
	writeSkillTree(t, root, map[string]string{"g/scripts/late.sh": "echo LATE\n"})
	_, cx := newTestContext()
	for _, bad := range []string{"late.sh", "dirlink", "sub", "./../scripts/../SKILL.md"} {
		got, err := rt.RunScript(cx, "g", bad, nil)
		if !errors.Is(err, tools.ErrNotFound) || strings.Contains(got, "LATE") {
			t.Errorf("RunScript(%q) = %q, %v; want a not-found refusal", bad, got, err)
		}
	}
	if got, err := rt.RunScript(cx, "g", "./sub/x.sh", nil); err != nil || got != "x" {
		t.Fatalf("a listed script spelled with ./ = %q, %v", got, err)
	}
	if got, err := rt.RunScript(cx, "g", "shebang.py", nil); err != nil || got != "shebang-won" {
		t.Fatalf("shebang.py = %q, %v; the shebang must win over the .py extension", got, err)
	}
	if _, err := rt.RunScript(cx, "ghost", "ok.sh", nil); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("unknown skill: %v", err)
	}
	off, _ := NewLocalSkillRuntime(root)
	if _, err := off.RunScript(cx, "g", "ok.sh", nil); err == nil {
		t.Fatal("a runtime without the opt-in must refuse to run scripts")
	}
}

// TestSkillsToolkitSingleRuntimeUsedAsIs: one runtime is not wrapped in a
// chain, several are.
func TestSkillsToolkitSingleRuntimeUsedAsIs(t *testing.T) {
	a, _ := NewLocalSkillRuntime(t.TempDir())
	b, _ := NewLocalSkillRuntime(t.TempDir())
	one, err := NewSkillsToolkitFromRuntime(a)
	if err != nil || one.Runtime() != SkillRuntime(a) {
		t.Fatalf("single runtime wrapped: %v %T", err, one.Runtime())
	}
	two, err := NewSkillsToolkitFromRuntime(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := two.Runtime().(*chainRuntime); !ok {
		t.Fatalf("two runtimes: Runtime() = %T, want the chain", two.Runtime())
	}
}
