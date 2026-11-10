package toolpacks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

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
}

var _ SkillRuntime = (*LocalSkillRuntime)(nil)

// NewLocalSkillRuntime scans dir for skills. The directory must exist;
// subdirectories without a SKILL.md are not skills.
func NewLocalSkillRuntime(dir string) (*LocalSkillRuntime, error) {
	abs, err := absDir(dir)
	if err != nil {
		return nil, fmt.Errorf("toolpacks: skills dir: %w", err)
	}
	rt := &LocalSkillRuntime{dir: abs}
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
	return nil, fmt.Errorf("%w: skill %q not found", tools.ErrNotFound, name)
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

// SkillsToolkit loads skills from one runtime and exposes them as tools
// (Python parity: ag2/tools/skills/toolkit.py SkillsToolkit over a single
// SkillRuntime — abbreviated: list, load and resource read; the script runner
// and the multi-runtime delegation chain are deferred, see docs/DECISIONS.md).
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

// NewSkillsToolkitFromRuntime builds the skills_toolkit over any SkillRuntime
// (a LocalSkillRuntime, a MemoryRuntime, or a custom store).
func NewSkillsToolkitFromRuntime(rt SkillRuntime) (*SkillsToolkit, error) {
	if rt == nil {
		return nil, errors.New("toolpacks: skills runtime must not be nil")
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
	tk, err := tools.NewToolkit("skills_toolkit", nil, listSkills, loadSkill, readResource)
	if err != nil {
		return nil, err
	}
	st.tk = tk
	return st, nil
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
