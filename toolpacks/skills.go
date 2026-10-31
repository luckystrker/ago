package toolpacks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// resourceCap bounds how many bundled resources one skill advertises (Python
// parity: SkillLoader _RESOURCE_CAP).
const resourceCap = 50

// skill is one discovered skill directory (Python parity:
// skills.skill_types.Skill, abbreviated to the fields the toolkit surfaces).
type skill struct {
	name        string
	description string
	dir         string
	resources   []string // paths relative to the skill directory; SKILL.md excluded
}

// SkillsToolkit loads agentskills.io-style local skills from one directory and
// exposes them as tools (Python parity: ag2/tools/skills/toolkit.py
// SkillsToolkit over a single LocalRuntime — abbreviated: list, load and
// resource read; the script runner and the multi-runtime delegation chain are
// deferred, see docs/DECISIONS.md).
//
// Each skill lives in <dir>/<name>/SKILL.md with a `---` frontmatter block
// carrying at least name and description (a missing name falls back to the
// directory name). Discovery happens at construction; the file system is read
// again per call for bodies and resources, so the toolkit is safe for
// concurrent use.
type SkillsToolkit struct {
	dir    string
	skills []skill
	tk     *tools.Toolkit
}

var _ tools.Tool = (*SkillsToolkit)(nil)

// NewSkillsToolkit builds the skills_toolkit from the skills directory. The
// directory must exist; subdirectories without a SKILL.md are skipped (Python
// parity: lenient discovery).
func NewSkillsToolkit(dir string) (*SkillsToolkit, error) {
	abs, err := absDir(dir)
	if err != nil {
		return nil, fmt.Errorf("toolpacks: skills dir: %w", err)
	}
	st := &SkillsToolkit{dir: abs}
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
		meta := parseFrontmatter(string(raw))
		name := meta["name"]
		if name == "" {
			// AGO-DEVIATION: Python's lenient loader skips a skill whose
			// frontmatter name is missing or malformed; AGo falls back to the
			// directory name so a hand-authored skill still loads (the
			// agentskills.io name rules are not enforced).
			name = e.Name()
		}
		s := skill{name: name, description: meta["description"], dir: skillDir}
		s.resources = listResources(skillDir)
		st.skills = append(st.skills, s)
	}
	sort.Slice(st.skills, func(i, j int) bool { return st.skills[i].name < st.skills[j].name })

	listSkills, err := tools.NewFunctionTool("list_skills",
		"List available local skills with name and short description.",
		func() []skillInfo { return st.catalog() })
	if err != nil {
		return nil, err
	}
	loadSkill, err := tools.NewFunctionTool("load_skill",
		"Load the full SKILL.md content for a specific skill.",
		func(args struct {
			Name string `json:"name" agschema:"description=Skill name returned by list_skills,required"`
		}) (string, error) {
			return st.load(args.Name)
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
		}) (string, error) {
			return st.readResource(args.Name, args.Resource)
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
	tk, err := tools.NewToolkit("skills_toolkit", nil, listSkills, loadSkill, readResource)
	if err != nil {
		return nil, err
	}
	st.tk = tk
	return st, nil
}

// skillInfo is one catalog entry (Python parity: discover_skills rows).
type skillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Location    string `json:"location"`
}

// catalog returns the skill list sorted by name.
func (st *SkillsToolkit) catalog() []skillInfo {
	out := make([]skillInfo, 0, len(st.skills))
	for _, s := range st.skills {
		out = append(out, skillInfo{Name: s.name, Description: s.description, Location: s.dir})
	}
	return out
}

// names lists the known skill names (the enum constraint of the name argument).
func (st *SkillsToolkit) names() []string {
	out := make([]string, 0, len(st.skills))
	for _, s := range st.skills {
		out = append(out, s.name)
	}
	return out
}

// find returns the named skill.
func (st *SkillsToolkit) find(name string) (skill, error) {
	for _, s := range st.skills {
		if s.name == name {
			return s, nil
		}
	}
	return skill{}, fmt.Errorf("%w: skill %q not found", tools.ErrNotFound, name)
}

// load returns the model-ready SKILL.md body (Python parity:
// LocalRuntime._read + _wrap_skill_content — identifying tags, the base
// directory and the resource list around the frontmatter-stripped body).
func (st *SkillsToolkit) load(name string) (string, error) {
	s, err := st.find(name)
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
		fmt.Sprintf("<skill_content name=%q>", s.name),
		strings.TrimSpace(body),
		"",
		"Skill directory: " + s.dir,
		"Relative paths in this skill are relative to the skill directory.",
	}
	if len(s.resources) > 0 {
		lines = append(lines, "<skill_resources>")
		for _, r := range s.resources {
			lines = append(lines, "  <file>"+r+"</file>")
		}
		lines = append(lines, "</skill_resources>")
	}
	lines = append(lines, "</skill_content>")
	return strings.Join(lines, "\n"), nil
}

// readResource returns one bundled resource, confined to the skill directory
// (Python parity: LocalRuntime._read_resource, abbreviated — no size cap beyond
// the generic output truncation of the tool result).
func (st *SkillsToolkit) readResource(name, resource string) (string, error) {
	s, err := st.find(name)
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
	for _, r := range s.resources {
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

// resolveWithinDir resolves path — already lexically confined to dir — to its
// real location, refusing a symlink whose target lies outside dir. The skill
// directory is disk the model may write to through other tools, so a planted
// symlink (a resource pointing at ~/.ssh/id_rsa) must not turn a resource read
// into an arbitrary-file read; an unresolvable target (a symlink loop, a
// dangling link) fails closed (security audit 2026-10-31, M-18).
//
// AGO-DEVIATION: Python's LocalRuntime reads after a lexical clean only; AGo
// resolves the existing symlink components the way knowledge/disk.go does.
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

// parseFrontmatter extracts `key: value` lines from a `---`-fenced block (the
// YAML subset the toolkit reads: plain scalars; quoting is stripped). An absent
// or unparsable block yields an empty map — the caller decides what is required.
func parseFrontmatter(text string) map[string]string {
	text = strings.TrimPrefix(text, "\uFEFF") // UTF-8 BOM (Python parity)
	out := map[string]string{}
	if !strings.HasPrefix(text, "---") {
		return out
	}
	end := strings.Index(text[3:], "\n---")
	if end < 0 {
		return out
	}
	block := text[3 : 3+end]
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		key, value, found := strings.Cut(line, ":")
		if !found || key == "" {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		out[strings.TrimSpace(key)] = value
	}
	return out
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

// listResources collects the files of one skill directory (SKILL.md excluded),
// relative, sorted, capped at resourceCap.
func listResources(skillDir string) []string {
	var out []string
	_ = filepath.WalkDir(skillDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // skip unreadable subtrees; discovery stays lenient
		}
		rel, err := filepath.Rel(skillDir, p)
		if err != nil || rel == "SKILL.md" {
			return nil
		}
		if len(out) < resourceCap {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
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

// Dir returns the skills directory the toolkit scans.
func (st *SkillsToolkit) Dir() string { return st.dir }

// Tools returns the members: list_skills, load_skill, read_skill_resource.
func (st *SkillsToolkit) Tools() []tools.Tool { return st.tk.Tools() }

// Skills returns the discovered skills sorted by name.
func (st *SkillsToolkit) Skills() []skillInfo { return st.catalog() }

// Schemas concatenates the members' schemas.
func (st *SkillsToolkit) Schemas(cx *agocontext.Context) ([]any, error) { return st.tk.Schemas(cx) }

// Register subscribes every member on the conversation stream.
func (st *SkillsToolkit) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	return st.tk.Register(cx, mw)
}
