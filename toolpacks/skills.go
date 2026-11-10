package toolpacks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// resourceCap bounds how many bundled resources one skill advertises (Python
// parity: SkillLoader _RESOURCE_CAP). Discovery stops scanning once the cap is
// reached and flags the skill as truncated.
const resourceCap = 50

// skillNamePattern is the agentskills.io name rule: lowercase alphanumeric
// words joined by single hyphens (Python parity: SkillLoader._SKILL_NAME_RE).
var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ErrSkillNotFound reports a skill the runtime does not own (Python parity:
// SkillNotFoundError). It also matches tools.ErrNotFound.
var ErrSkillNotFound = errors.New("toolpacks: skill not found")

// skillNotFound builds the not-found error both runtimes return.
func skillNotFound(name string) error {
	return fmt.Errorf("%w: %w: skill %q not found", tools.ErrNotFound, ErrSkillNotFound, name)
}

// SkillInfo is one discovered skill: its metadata plus where it lives
// (Python parity: skills.skill_types.Skill / SkillMetadata, flattened to the
// fields the toolkit surfaces).
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Location    string `json:"location"`
	// Full frontmatter, retained for Go callers; the list_skills wire shape
	// stays name + description + location (Python parity: discover_skills).
	Version       string            `json:"-"`
	License       string            `json:"-"`
	Compatibility string            `json:"-"`
	AllowedTools  string            `json:"-"`
	Metadata      map[string]string `json:"-"`
	Resources     []string          `json:"-"`
	// ResourcesTruncated reports that scanning stopped at resourceCap.
	ResourcesTruncated bool `json:"-"`
	// Scripts lists the files under scripts/, relative to it and sorted (what
	// run_skill_script may run).
	Scripts []string `json:"-"`
}

// SkillRuntime serves skills from one backing store (a directory, memory, or a
// future remote store). The toolkit and the plugin read through this interface.
type SkillRuntime interface {
	// Discover returns the skills sorted by name.
	Discover() []SkillInfo
	// Load returns the model-ready skill body (frontmatter stripped, the
	// resource list wrapped around it).
	Load(ctx *agocontext.Context, name string) (string, error)
	// ReadResource returns one bundled resource, confined to the skill.
	ReadResource(ctx *agocontext.Context, name, resource string) (string, error)
}

// SkillScriptRuntime is the optional SkillRuntime capability of running a
// skill's scripts (Python parity: SkillRuntime.execute). The toolkit adds
// run_skill_script only when some runtime reports ScriptsEnabled.
type SkillScriptRuntime interface {
	SkillRuntime
	// ScriptsEnabled reports whether this runtime executes scripts.
	ScriptsEnabled() bool
	// RunScript runs script of skill name with positional CLI arguments and
	// returns its output (a non-zero exit appends "[exit code: N]").
	RunScript(ctx *agocontext.Context, name, script string, args []string) (string, error)
}

// skill is one discovered local skill directory.
type skill struct {
	info SkillInfo
	dir  string
}

// LocalSkillRuntime loads agentskills.io-style skills from one directory
// (Python parity: skills.runtime.local.LocalRuntime over SkillLoader in lenient
// mode).
//
// Each skill lives in <dir>/<name>/SKILL.md with a `---` frontmatter block
// carrying at least name and description. Discovery happens at construction:
// a skill without a description is skipped, a missing name falls back to the
// directory name (lenient mode), and the full frontmatter — version, license,
// compatibility, allowed-tools, metadata — is retained on SkillInfo. The file
// system is read again per call for bodies and resources, so the runtime is
// safe for concurrent use.
type LocalSkillRuntime struct {
	dir    string
	skills []*skill
	script scriptConfig
}

var (
	_ SkillRuntime       = (*LocalSkillRuntime)(nil)
	_ SkillScriptRuntime = (*LocalSkillRuntime)(nil)
)

// scriptConfig is the run_skill_script configuration of a LocalSkillRuntime.
type scriptConfig struct {
	enabled   bool
	timeout   time.Duration
	maxOutput int
	blocked   []string
	factory   SandboxFactory
}

// LocalSkillOption configures NewLocalSkillRuntime.
type LocalSkillOption func(*LocalSkillRuntime) error

// WithSkillScripts lets the runtime execute the scripts under a skill's
// scripts/ directory through run_skill_script.
//
// AGO-DEVIATION: Python's toolkit always offers run_skill_script. AGo offers it
// only when a runtime opts in, because it executes files from the skills
// directory on this host and existing toolkits never exposed that; every other
// WithSkillScript* option opts in too. See docs/DECISIONS.md.
func WithSkillScripts() LocalSkillOption {
	return func(rt *LocalSkillRuntime) error { rt.script.enabled = true; return nil }
}

// WithSkillScriptTimeout bounds one script run (default 60s; opts in).
func WithSkillScriptTimeout(d time.Duration) LocalSkillOption {
	return func(rt *LocalSkillRuntime) error {
		if d <= 0 {
			return errors.New("toolpacks: skill script timeout must be > 0")
		}
		rt.script.enabled, rt.script.timeout = true, d
		return nil
	}
}

// WithSkillScriptMaxOutput caps the reported output in characters (opts in).
func WithSkillScriptMaxOutput(n int) LocalSkillOption {
	return func(rt *LocalSkillRuntime) error {
		if n <= 0 {
			return errors.New("toolpacks: skill script max_output must be > 0")
		}
		rt.script.enabled, rt.script.maxOutput = true, n
		return nil
	}
}

// WithSkillScriptBlocked refuses a script run whose argv matches one of the
// patterns, word by word with the program also matched by its base name
// (Python parity: LocalRuntime blocked). The argv runs without a shell. Like
// every blocklist it is advisory: a script that runs another program is not
// covered (opts in).
func WithSkillScriptBlocked(patterns ...string) LocalSkillOption {
	return func(rt *LocalSkillRuntime) error {
		for _, p := range patterns {
			if strings.TrimSpace(p) == "" {
				return errors.New("toolpacks: skill script block pattern must not be empty")
			}
		}
		rt.script.enabled = true
		rt.script.blocked = append(rt.script.blocked, patterns...)
		return nil
	}
}

// WithSkillScriptSandbox runs scripts in a pluggable backend instead of a local
// subprocess rooted at the skill's scripts/ directory (Python parity:
// LocalRuntime sandbox=). The backend owns its working directory; the caller
// makes the scripts reachable there, and the command is ./<script> relative to
// it (opts in).
func WithSkillScriptSandbox(factory SandboxFactory) LocalSkillOption {
	return func(rt *LocalSkillRuntime) error {
		if factory == nil {
			return errors.New("toolpacks: skill script sandbox factory must not be nil")
		}
		rt.script.enabled, rt.script.factory = true, factory
		return nil
	}
}

// NewLocalSkillRuntime scans dir for skills. The directory must exist;
// subdirectories without a SKILL.md are not skills.
func NewLocalSkillRuntime(dir string, opts ...LocalSkillOption) (*LocalSkillRuntime, error) {
	abs, err := absDir(dir)
	if err != nil {
		return nil, fmt.Errorf("toolpacks: skills dir: %w", err)
	}
	rt := &LocalSkillRuntime{dir: abs}
	for _, opt := range opts {
		if err := opt(rt); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("toolpacks: skills dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillDir := filepath.Join(abs, e.Name())
		raw, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
		if err != nil {
			continue // not a skill directory
		}
		text := string(raw)
		fm := parseFrontmatterDocument(text)
		// Lenient discovery: only a missing/empty description is a reason to
		// skip. A name mismatch or an unexpected charset is tolerated so skills
		// authored for other clients still load (Python parity:
		// SkillLoader._validate_lenient).
		if strings.TrimSpace(fm.values["description"]) == "" {
			continue
		}
		name := fm.values["name"]
		if name == "" {
			name = e.Name()
		}
		resources, truncated := listResources(skillDir)
		info := SkillInfo{
			Scripts:            listScripts(skillDir),
			Name:               name,
			Description:        fm.values["description"],
			Location:           skillDir,
			Version:            fm.values["version"],
			License:            fm.values["license"],
			Compatibility:      fm.values["compatibility"],
			AllowedTools:       fm.values["allowed-tools"],
			Metadata:           fm.metadata,
			Resources:          resources,
			ResourcesTruncated: truncated,
		}
		rt.skills = append(rt.skills, &skill{
			info: info,
			dir:  skillDir,
		})
	}
	sort.Slice(rt.skills, func(i, j int) bool { return rt.skills[i].info.Name < rt.skills[j].info.Name })
	return rt, nil
}

// Dir returns the scanned directory.
func (rt *LocalSkillRuntime) Dir() string { return rt.dir }

// Discover returns the skills sorted by name.
func (rt *LocalSkillRuntime) Discover() []SkillInfo {
	out := make([]SkillInfo, 0, len(rt.skills))
	for _, s := range rt.skills {
		out = append(out, s.info)
	}
	return out
}

// find returns the named skill.
func (rt *LocalSkillRuntime) find(name string) (*skill, error) {
	for _, s := range rt.skills {
		if s.info.Name == name {
			return s, nil
		}
	}
	return nil, skillNotFound(name)
}

// Load returns the model-ready SKILL.md body (Python parity:
// LocalRuntime._read + _wrap_skill_content — identifying tags, the base
// directory and the resource list around the frontmatter-stripped body).
func (rt *LocalSkillRuntime) Load(_ *agocontext.Context, name string) (string, error) {
	s, err := rt.find(name)
	if err != nil {
		return "", err
	}
	target, err := resolveWithinDir(s.dir, filepath.Join(s.dir, "SKILL.md"))
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return "", err
	}
	body := stripFrontmatter(string(raw))
	lines := []string{
		fmt.Sprintf("<skill_content name=%q>", s.info.Name),
		strings.TrimSpace(body),
		"",
		"Skill directory: " + s.dir,
		"Relative paths in this skill are relative to the skill directory.",
	}
	if len(s.info.Resources) > 0 {
		lines = append(lines, "<skill_resources>")
		for _, r := range s.info.Resources {
			lines = append(lines, "  <file>"+r+"</file>")
		}
		lines = append(lines, "</skill_resources>")
	}
	lines = append(lines, "</skill_content>")
	return strings.Join(lines, "\n"), nil
}

// ReadResource returns one bundled resource, confined to the skill directory
// (Python parity: LocalRuntime._read_resource, abbreviated — no size cap beyond
// the generic output truncation of the tool result).
func (rt *LocalSkillRuntime) ReadResource(_ *agocontext.Context, name, resource string) (string, error) {
	s, err := rt.find(name)
	if err != nil {
		return "", err
	}
	if resource == "" {
		return "", errors.New("resource path must not be empty")
	}
	clean := filepath.Clean(filepath.FromSlash(resource))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("resource %q escapes the skill directory", resource)
	}
	known := false
	for _, r := range s.info.Resources {
		if r == filepath.ToSlash(clean) {
			known = true
			break
		}
	}
	if !known {
		return "", fmt.Errorf("resource %q not found in %s", resource, s.dir)
	}
	target, err := resolveWithinDir(s.dir, filepath.Join(s.dir, clean))
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(target)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ScriptsEnabled reports whether run_skill_script may execute scripts.
func (rt *LocalSkillRuntime) ScriptsEnabled() bool { return rt.script.enabled }

// RunScript runs one script of skill name (Python parity: LocalRuntime.execute).
// The script must be a file listed under the skill's scripts/ directory and
// resolve inside it (a symlink pointing out is refused); the command is built
// from the shebang, or python3 for .py and sh for .sh, and runs as an argv
// without a shell from the scripts/ directory (or in the configured backend).
// A blocklist match is refused before anything starts; a non-zero exit is part
// of the result text.
func (rt *LocalSkillRuntime) RunScript(cx *agocontext.Context, name, script string, args []string) (string, error) {
	s, err := rt.find(name)
	if err != nil {
		return "", err
	}
	if !rt.script.enabled {
		return "", errors.New("toolpacks: script execution is not enabled for this skill runtime")
	}
	scriptsDir := filepath.Join(s.dir, "scripts")
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(script)))
	known := false
	for _, k := range s.info.Scripts {
		if k == clean {
			known = true
			break
		}
	}
	resolved, rerr := resolveWithinDir(scriptsDir, filepath.Join(scriptsDir, filepath.FromSlash(clean)))
	if !known || rerr != nil {
		return "", fmt.Errorf("%w: script %q not found in %s", tools.ErrNotFound, script, scriptsDir)
	}
	if info, err := os.Stat(resolved); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: script %q not found in %s", tools.ErrNotFound, script, scriptsDir)
	}
	argv, err := scriptCommand(resolved, clean, useHostPython(runtime.GOOS, rt.script.factory != nil))
	if err != nil {
		return "", err
	}
	argv = append(argv, args...)
	if denied := argvBlocked(rt.script.blocked, argv); denied {
		return fmt.Sprintf("Command not allowed: %q", strings.Join(argv, " ")), nil
	}
	ctx := cx.TurnContext()
	var res ExecResult
	opts := ExecOptions{Timeout: rt.script.timeout}
	if rt.script.factory != nil {
		err = withSandbox(ctx, cx, rt.script.factory, func(sb Sandbox) error {
			var xerr error
			res, xerr = sb.Exec(ctx, argv, opts)
			return xerr
		})
	} else {
		sbOpts := []LocalSandboxOption{WithLocalSandboxDir(scriptsDir)}
		if rt.script.maxOutput > 0 {
			sbOpts = append(sbOpts, WithLocalSandboxMaxOutput(rt.script.maxOutput))
		}
		var sb *LocalSandbox
		if sb, err = NewLocalSandbox(sbOpts...); err == nil {
			defer sb.Close()
			res, err = sb.Exec(ctx, argv, opts)
		}
	}
	if err != nil {
		return "", err
	}
	return formatExit(res), nil
}

// useHostPython reports whether .py scripts run under the host's interpreter
// regardless of their shebang: only on Windows, and only for the default local
// backend — a supplied sandbox must never receive a host interpreter name.
func useHostPython(goos string, sandboxed bool) bool { return goos == "windows" && !sandboxed }

// scriptCommand builds the argv that runs a skill script from its scripts/
// directory (Python parity: _script_command): a shebang takes precedence;
// otherwise .py and .sh map to their interpreters and anything else is made
// executable and run directly. hostPython runs .py under the host's python and
// ignores the shebang (on Windows python3 is often missing or a Store stub).
func scriptCommand(resolved, rel string, hostPython bool) ([]string, error) {
	arg := "./" + rel
	ext := strings.ToLower(filepath.Ext(resolved))
	if hostPython && ext == ".py" {
		return []string{findPythonOr("python"), arg}, nil
	}
	head, err := os.ReadFile(resolved)
	if err != nil {
		return nil, err
	}
	first, _, _ := strings.Cut(string(head), "\n")
	switch {
	case strings.HasPrefix(first, "#!"):
		return []string{arg}, markExecutable(resolved)
	case ext == ".py":
		return []string{findPythonOr("python3"), arg}, nil
	case ext == ".sh":
		return []string{"sh", arg}, nil
	}
	return []string{arg}, markExecutable(resolved)
}

// findPythonOr returns the discovered interpreter, or fallback when none is on
// PATH (the run then reports "Command not found").
func findPythonOr(fallback string) string {
	if p := findPython(); p != "" {
		return p
	}
	return fallback
}

// markExecutable adds the execute bits to a script that is run directly.
func markExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.Chmod(path, info.Mode().Perm()|0o111)
}

// listScripts lists the files under scripts/ relative to it, sorted (Python
// parity: loader._list_scripts).
func listScripts(skillDir string) []string {
	scriptsDir := filepath.Join(skillDir, "scripts")
	var out []string
	_ = filepath.WalkDir(scriptsDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if rel, rerr := filepath.Rel(scriptsDir, p); rerr == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// SkillsToolkit loads skills from one or more runtimes and exposes them as
// tools (Python parity: ag2/tools/skills/toolkit.py SkillsToolkit): list, load
// and resource read, plus run_skill_script when a runtime executes scripts.
//
// With several runtimes each tool routes by last-to-first chain delegation: the
// runtimes are tried from last to first and the first that owns the skill name
// serves the call, so the last runtime shadows earlier ones on a name clash
// (project overrides global).
type SkillsToolkit struct {
	rt SkillRuntime
	tk *tools.Toolkit
}

var _ tools.Tool = (*SkillsToolkit)(nil)

// NewSkillsToolkit builds the skills_toolkit from the skills directory.
func NewSkillsToolkit(dir string) (*SkillsToolkit, error) {
	rt, err := NewLocalSkillRuntime(dir)
	if err != nil {
		return nil, err
	}
	return NewSkillsToolkitFromRuntime(rt)
}

// NewSkillsToolkitFromDirs builds the skills_toolkit over several skill
// directories served together, for example a global one and a project one. A
// skill in a later directory shadows one of the same name in an earlier one.
// Each directory gets a plain read-only runtime; build LocalSkillRuntimes with
// WithSkillScripts and pass them to NewSkillsToolkitFromRuntime to run scripts.
func NewSkillsToolkitFromDirs(dirs ...string) (*SkillsToolkit, error) {
	rts := make([]SkillRuntime, 0, len(dirs))
	for _, dir := range dirs {
		rt, err := NewLocalSkillRuntime(dir)
		if err != nil {
			return nil, err
		}
		rts = append(rts, rt)
	}
	return NewSkillsToolkitFromRuntime(rts...)
}

// NewSkillsToolkitFromRuntime builds the skills_toolkit over any SkillRuntimes
// (a LocalSkillRuntime, a MemoryRuntime, or a custom store). Passing several
// composes them with last-wins name resolution (see SkillsToolkit); a single
// runtime is used as is.
func NewSkillsToolkitFromRuntime(rts ...SkillRuntime) (*SkillsToolkit, error) {
	if len(rts) == 0 {
		return nil, errors.New("toolpacks: skills runtime must not be nil")
	}
	for _, rt := range rts {
		if rt == nil {
			return nil, errors.New("toolpacks: skills runtime must not be nil")
		}
	}
	var rt SkillRuntime = rts[0]
	if len(rts) > 1 {
		rt = &chainRuntime{rts: append([]SkillRuntime(nil), rts...)}
	}
	st := &SkillsToolkit{rt: rt}

	listSkills, err := tools.NewFunctionTool("list_skills",
		"List available local skills with name and short description.",
		func() []SkillInfo { return st.rt.Discover() })
	if err != nil {
		return nil, err
	}
	loadSkill, err := tools.NewFunctionTool("load_skill",
		"Load the full SKILL.md content for a specific skill.",
		func(args struct {
			Name string `json:"name" agschema:"description=Skill name returned by list_skills,required"`
		}, cx *agocontext.Context) (string, error) {
			return st.rt.Load(cx, args.Name)
		},
		tools.WithToolSchema(stringEnumSchema(
			"name", "Skill name returned by list_skills.", st.names(), nil)))
	if err != nil {
		return nil, err
	}
	readResource, err := tools.NewFunctionTool("read_skill_resource",
		"Read a bundled resource file from a skill's directory, given a path relative to the skill.",
		func(args struct {
			Name     string `json:"name" agschema:"description=Skill name returned by list_skills,required"`
			Resource string `json:"resource" agschema:"description=Resource path relative to the skill directory,required"`
		}, cx *agocontext.Context) (string, error) {
			return st.rt.ReadResource(cx, args.Name, args.Resource)
		},
		tools.WithToolSchema(stringEnumSchema("name", "Skill name returned by list_skills.", st.names(),
			&enumField{
				name:        "resource",
				description: "Resource path relative to the skill directory, for example references/guide.md.",
				required:    true,
			})))
	if err != nil {
		return nil, err
	}
	members := []tools.Tool{listSkills, loadSkill, readResource}
	if anyScriptsEnabled(rts) {
		runScript, err := newRunSkillScriptTool(st)
		if err != nil {
			return nil, err
		}
		members = append(members, runScript)
	}
	tk, err := tools.NewToolkit("skills_toolkit", nil, members...)
	if err != nil {
		return nil, err
	}
	st.tk = tk
	return st, nil
}

// anyScriptsEnabled reports whether some runtime executes scripts.
func anyScriptsEnabled(rts []SkillRuntime) bool {
	for _, rt := range rts {
		if sr, ok := rt.(SkillScriptRuntime); ok && sr.ScriptsEnabled() {
			return true
		}
	}
	return false
}

// newRunSkillScriptTool builds run_skill_script (Python parity: the
// run_skill_script member): skill name, script name, and optional positional
// arguments.
func newRunSkillScriptTool(st *SkillsToolkit) (*tools.FunctionTool, error) {
	schema := stringEnumSchema("name", "Skill name returned by list_skills.", st.names(), &enumField{
		name:        "script",
		description: "Script name, for example scaffold.py or build.sh, as in the skill's scripts directory.",
		required:    true,
	})
	schema["properties"].(map[string]any)["args"] = map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": `Positional command-line arguments of the script, for example ["input.docx"].`,
	}
	return tools.NewFunctionTool("run_skill_script",
		"Run a script from a skill's scripts directory. Only .py and .sh scripts, and scripts with a shebang, are supported.",
		func(args struct {
			Name   string          `json:"name"`
			Script string          `json:"script"`
			Args   json.RawMessage `json:"args"`
		}, cx *agocontext.Context) (string, error) {
			argv, err := scriptArgs(args.Args)
			if err != nil {
				return "", err
			}
			return st.runScript(cx, args.Name, args.Script, argv)
		},
		tools.WithToolSchema(schema))
}

// scriptArgs decodes the args argument: absent or null is none, an array of
// strings is the positional list. A JSON object (named arguments for
// in-process scripts in Python) is refused, as Python's local runtime does.
func scriptArgs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		return nil, errors.New("file-based scripts require positional string arguments (an array); named arguments (an object) are only supported for in-process scripts")
	}
	return argv, nil
}

// runScript routes one script run through the runtimes, last to first.
func (st *SkillsToolkit) runScript(cx *agocontext.Context, name, script string, args []string) (string, error) {
	sr, ok := st.rt.(SkillScriptRuntime)
	if !ok {
		return "", errors.New("toolpacks: no skill runtime executes scripts")
	}
	return sr.RunScript(cx, name, script, args)
}

// chainRuntime composes runtimes with last-to-first delegation (Python parity:
// the _route_* helpers of SkillsToolkit). A call goes to the last runtime that
// owns the skill name.
type chainRuntime struct {
	rts []SkillRuntime
}

var _ SkillScriptRuntime = (*chainRuntime)(nil)

// owner returns the last runtime that owns name.
func (c *chainRuntime) owner(name string) (SkillRuntime, error) {
	for i := len(c.rts) - 1; i >= 0; i-- {
		for _, info := range c.rts[i].Discover() {
			if info.Name == name {
				return c.rts[i], nil
			}
		}
	}
	return nil, fmt.Errorf("%w: %w: skill %q not found in any runtime", tools.ErrNotFound, ErrSkillNotFound, name)
}

// Discover merges the runtimes' skills, deduplicated by name with the last
// runtime winning, sorted by name.
func (c *chainRuntime) Discover() []SkillInfo {
	merged := map[string]SkillInfo{}
	for _, rt := range c.rts {
		for _, info := range rt.Discover() {
			merged[info.Name] = info
		}
	}
	out := make([]SkillInfo, 0, len(merged))
	for _, info := range merged {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Load serves the skill from its owning runtime.
func (c *chainRuntime) Load(cx *agocontext.Context, name string) (string, error) {
	rt, err := c.owner(name)
	if err != nil {
		return "", err
	}
	return rt.Load(cx, name)
}

// ReadResource serves the resource from the skill's owning runtime.
func (c *chainRuntime) ReadResource(cx *agocontext.Context, name, resource string) (string, error) {
	rt, err := c.owner(name)
	if err != nil {
		return "", err
	}
	return rt.ReadResource(cx, name, resource)
}

// ScriptsEnabled reports whether some runtime executes scripts.
func (c *chainRuntime) ScriptsEnabled() bool { return anyScriptsEnabled(c.rts) }

// RunScript runs the script on the skill's owning runtime. A skill shadowed by
// a later runtime that cannot run scripts does not fall through to the earlier
// one: the shadowing runtime is the one that owns the name.
func (c *chainRuntime) RunScript(cx *agocontext.Context, name, script string, args []string) (string, error) {
	rt, err := c.owner(name)
	if err != nil {
		return "", err
	}
	sr, ok := rt.(SkillScriptRuntime)
	if !ok || !sr.ScriptsEnabled() {
		return "", fmt.Errorf("toolpacks: the runtime serving skill %q does not execute scripts", name)
	}
	return sr.RunScript(cx, name, script, args)
}

// names lists the known skill names (the enum constraint of the name argument).
func (st *SkillsToolkit) names() []string {
	infos := st.rt.Discover()
	out := make([]string, 0, len(infos))
	for _, s := range infos {
		out = append(out, s.Name)
	}
	return out
}

// resolveWithinDir resolves path — already lexically confined to dir — to its
// real location, refusing a symlink whose target lies outside dir. The skill
// directory is disk the model may write to through other tools, so a planted
// symlink (a resource pointing at ~/.ssh/id_rsa) must not turn a resource read
// into an arbitrary-file read; an unresolvable target (a symlink loop, a
// dangling link) fails closed (security audit 2026-10-31, M-18).
//
// AGO-DEVIATION: Python's _resolve_within already resolves symlinks for
// resource and script paths; its SKILL.md body read (SkillLoader.load) has no
// such check. AGo applies the same symlink-resolving containment to the
// SKILL.md read as well.
func resolveWithinDir(dir, path string) (string, error) {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if real != realDir && !strings.HasPrefix(real, realDir+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the skill directory", path)
	}
	return real, nil
}

// frontmatter is the parsed `---` block of a SKILL.md: plain scalars plus the
// nested `metadata:` mapping (the YAML subset the loader understands).
type frontmatter struct {
	values            map[string]string
	metadata          map[string]string
	metadataIsMapping bool
}

// parseFrontmatterDocument extracts the frontmatter block: `key: value` pairs,
// with indented lines under `metadata:` collected as a mapping (Python parity:
// loader.parse_frontmatter for the subset; quoting is stripped).
func parseFrontmatterDocument(text string) frontmatter {
	out := frontmatter{values: map[string]string{}, metadata: map[string]string{}}
	text = strings.TrimPrefix(text, "\uFEFF") // UTF-8 BOM (Python parity)
	if !strings.HasPrefix(text, "---") {
		return out
	}
	end := strings.Index(text[3:], "\n---")
	if end < 0 {
		return out
	}
	block := text[3 : 3+end]
	parent := ""
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, ":") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		key, value, _ := strings.Cut(trimmed, ":")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key == "" {
			continue
		}
		if indent > 0 && parent == "metadata" {
			out.metadata[key] = value
			out.metadataIsMapping = true
			continue
		}
		parent = key
		out.values[key] = value
		if key == "metadata" {
			out.metadataIsMapping = value == ""
		}
	}
	return out
}

// parseFrontmatter extracts the plain `key: value` fields (the historical
// helper; nested metadata is available through parseFrontmatterDocument).
func parseFrontmatter(text string) map[string]string {
	return parseFrontmatterDocument(text).values
}

// ValidateSkillMetadata applies the agentskills.io metadata rules to the
// contents of one SKILL.md file (Python parity:
// SkillLoader.validate_skill_metadata, used by the install path). The lenient
// local discovery only requires a description; this is the strict gate for
// callers that install or ship skills.
func ValidateSkillMetadata(dirName, text string) error {
	return validateSkillMetadataStrict(dirName, parseFrontmatterDocument(text))
}

// validateSkillMetadataStrict applies the agentskills.io metadata rules
// (Python parity: SkillLoader.validate_skill_metadata). The lenient local
// runtime only requires a description; this validator is the strict gate an
// install path would use.
func validateSkillMetadataStrict(dirName string, fm frontmatter) error {
	if _, ok := fm.values["name"]; !ok {
		return fmt.Errorf("skill %q is missing required frontmatter field: name", dirName)
	}
	description, ok := fm.values["description"]
	if !ok {
		return fmt.Errorf("skill %q is missing required frontmatter field: description", dirName)
	}
	name := fm.values["name"]
	if len(name) < 1 || len(name) > 64 {
		return fmt.Errorf("invalid skill name %q: expected length 1-64", name)
	}
	if !skillNamePattern.MatchString(name) {
		return fmt.Errorf("invalid skill name %q: expected lowercase alnum and single hyphens", name)
	}
	if name != dirName {
		return fmt.Errorf("skill name %q must match directory name %q", name, dirName)
	}
	if len(description) < 1 || len(description) > 1024 {
		return fmt.Errorf("invalid description for %q: expected length 1-1024", name)
	}
	if compatibility, ok := fm.values["compatibility"]; ok && compatibility != "" {
		if len(compatibility) < 1 || len(compatibility) > 500 {
			return fmt.Errorf("invalid compatibility for %q: expected length 1-500", name)
		}
	}
	if _, ok := fm.values["metadata"]; ok && !fm.metadataIsMapping {
		return fmt.Errorf("invalid metadata for %q: expected mapping", name)
	}
	return nil
}

// stripFrontmatter returns the markdown body after the frontmatter block
// (Python parity: loader.strip_frontmatter).
func stripFrontmatter(text string) string {
	text = strings.TrimPrefix(text, "\uFEFF") // UTF-8 BOM (Python parity)
	if !strings.HasPrefix(text, "---") {
		return strings.TrimSpace(text)
	}
	end := strings.Index(text[3:], "\n---")
	if end < 0 {
		return strings.TrimSpace(text)
	}
	after := text[3+end+1:] // past the closing '---'
	if nl := strings.Index(after, "\n"); nl >= 0 {
		after = after[nl+1:]
	} else {
		after = ""
	}
	return strings.TrimSpace(after)
}

// listResources collects the files of one skill directory (SKILL.md and the
// scripts/ tree excluded), relative and sorted, stopping at resourceCap. The
// second result reports that more resources exist beyond the cap (Python
// parity: loader._list_resources).
func listResources(skillDir string) ([]string, bool) {
	var out []string
	truncated := false
	scriptsDir := filepath.Join(skillDir, "scripts")
	_ = filepath.WalkDir(skillDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // skip unreadable subtrees; discovery stays lenient
		}
		rel, err := filepath.Rel(skillDir, p)
		if err != nil || rel == "SKILL.md" {
			return nil
		}
		if p == scriptsDir || strings.HasPrefix(p, scriptsDir+string(filepath.Separator)) {
			return nil
		}
		if len(out) >= resourceCap {
			truncated = true
			return filepath.SkipAll
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, truncated
}

// enumField is an extra property of a hand-built parameters schema.
type enumField struct {
	name        string
	description string
	required    bool
}

// stringEnumSchema builds the parameters schema for tools whose (first) argument
// is the skill name constrained to the discovered names — the port of Python's
// Literal[name, ...] annotation. With no skills the enum is omitted (a Literal
// cannot be empty; Python falls back to str the same way).
func stringEnumSchema(nameField, description string, names []string, extra *enumField) map[string]any {
	nameProp := map[string]any{"type": "string", "description": description}
	if len(names) > 0 {
		enum := make([]any, len(names))
		for i, n := range names {
			enum[i] = n
		}
		nameProp["enum"] = enum
	}
	props := map[string]any{nameField: nameProp}
	required := []string{nameField}
	if extra != nil {
		props[extra.name] = map[string]any{"type": "string", "description": extra.description}
		if extra.required {
			required = append(required, extra.name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

// Name returns the toolkit name.
func (st *SkillsToolkit) Name() string { return st.tk.Name() }

// DeclaredInCode reports a program-declared tool (framework contract).
func (st *SkillsToolkit) DeclaredInCode() bool { return st.tk.DeclaredInCode() }

// Dir returns the local skills directory, or "" for a non-directory runtime.
func (st *SkillsToolkit) Dir() string {
	if local, ok := st.rt.(*LocalSkillRuntime); ok {
		return local.dir
	}
	return ""
}

// Runtime returns the backing runtime.
func (st *SkillsToolkit) Runtime() SkillRuntime { return st.rt }

// Tools returns the members: list_skills, load_skill, read_skill_resource.
func (st *SkillsToolkit) Tools() []tools.Tool { return st.tk.Tools() }

// Skills returns the discovered skills sorted by name.
func (st *SkillsToolkit) Skills() []SkillInfo { return st.rt.Discover() }

// Schemas concatenates the members' schemas.
func (st *SkillsToolkit) Schemas(cx *agocontext.Context) ([]any, error) { return st.tk.Schemas(cx) }

// Register subscribes every member on the conversation stream.
func (st *SkillsToolkit) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	return st.tk.Register(cx, mw)
}
