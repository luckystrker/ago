package toolpacks

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/tools"
)

// memoryResourceReadCap bounds one in-memory resource read (Python parity:
// MemoryRuntime _RESOURCE_READ_CAP).
const memoryResourceReadCap = 100_000

// MemorySkill is a skill defined in code rather than discovered on disk
// (Python parity: skills/runtime/memory/skill.py MemorySkill). It carries its
// instructions (static text or a per-load renderer) and resources as in-memory
// values.
//
// AGO-DEVIATION: Python's MemorySkill also carries in-process scripts; AGo does
// not (run_skill_script runs the file-based scripts of a LocalSkillRuntime).
// Instructions and resources are the parts SkillPlugin activates.
//
// A skill is owned by a MemoryRuntime; pass one (or several) to
// NewMemoryRuntime and serve the runtime through NewSkillsToolkitFromRuntime or
// NewSkillPlugin.
type MemorySkill struct {
	Name          string
	Description   string
	Version       string
	Compatibility string

	instructions string
	render       func(ctx *agocontext.Context) (string, error)
	resources    map[string]func(ctx *agocontext.Context) (string, error)
}

// NewMemorySkill starts an in-code skill. Name and description are required:
// the description is how the model discovers the skill.
func NewMemorySkill(name, description string) (*MemorySkill, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("toolpacks: memory skill name must not be empty")
	}
	if strings.TrimSpace(description) == "" {
		return nil, errors.New("toolpacks: memory skill description must not be empty")
	}
	return &MemorySkill{
		Name:        name,
		Description: description,
		resources:   map[string]func(ctx *agocontext.Context) (string, error){},
	}, nil
}

// SetInstructions sets a static skill body and returns the skill for chaining.
func (m *MemorySkill) SetInstructions(text string) *MemorySkill {
	m.instructions = text
	m.render = nil
	return m
}

// SetInstructionsFunc sets a body rendered on every Load, so it can reflect the
// live conversation (Python parity: a callable Instructions body). It takes
// precedence over a static body.
func (m *MemorySkill) SetInstructionsFunc(fn func(ctx *agocontext.Context) (string, error)) *MemorySkill {
	m.render = fn
	return m
}

// AddResource registers a named resource whose content is produced on every
// read. The name is the path the model passes to read_skill_resource.
func (m *MemorySkill) AddResource(name string, fn func(ctx *agocontext.Context) (string, error)) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("toolpacks: memory skill resource name must not be empty")
	}
	if fn == nil {
		return errors.New("toolpacks: memory skill resource function must not be nil")
	}
	m.resources[name] = fn
	return nil
}

// MemoryRuntime serves code-defined MemorySkill values (Python parity:
// skills/runtime/memory/runtime.py MemoryRuntime). It is read-only: there is no
// installable storage. Safe for concurrent use once constructed, provided the
// resource functions are.
type MemoryRuntime struct {
	skills []*MemorySkill
}

var _ SkillRuntime = (*MemoryRuntime)(nil)

// NewMemoryRuntime builds a runtime over the given skills, last-wins on a name
// clash (Python parity: the dict built in MemoryRuntime.__init__).
func NewMemoryRuntime(skills ...*MemorySkill) (*MemoryRuntime, error) {
	rt := &MemoryRuntime{}
	for _, s := range skills {
		if s == nil {
			return nil, errors.New("toolpacks: memory skill must not be nil")
		}
		if strings.TrimSpace(s.Name) == "" {
			return nil, errors.New("toolpacks: memory skill name must not be empty")
		}
		if strings.TrimSpace(s.Description) == "" {
			return nil, fmt.Errorf("toolpacks: memory skill %q has an empty description", s.Name)
		}
		replaced := false
		for i, existing := range rt.skills {
			if existing.Name == s.Name {
				rt.skills[i] = s
				replaced = true
				break
			}
		}
		if !replaced {
			rt.skills = append(rt.skills, s)
		}
	}
	return rt, nil
}

// Discover returns the skills sorted by name.
func (rt *MemoryRuntime) Discover() []SkillInfo {
	out := make([]SkillInfo, 0, len(rt.skills))
	for _, s := range rt.skills {
		names := make([]string, 0, len(s.resources))
		for name := range s.resources {
			names = append(names, name)
		}
		sort.Strings(names)
		out = append(out, SkillInfo{
			Name:          s.Name,
			Description:   s.Description,
			Version:       s.Version,
			Compatibility: s.Compatibility,
			Resources:     names,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// find returns the named skill.
func (rt *MemoryRuntime) find(name string) (*MemorySkill, error) {
	for _, s := range rt.skills {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, skillNotFound(name)
}

// Load renders the skill body with its resource list, the same envelope the
// local runtime uses (Python parity: MemoryRuntime.read +
// _wrap_memory_content).
func (rt *MemoryRuntime) Load(ctx *agocontext.Context, name string) (string, error) {
	s, err := rt.find(name)
	if err != nil {
		return "", err
	}
	body := s.instructions
	if s.render != nil {
		body, err = s.render(ctx)
		if err != nil {
			return "", err
		}
	}
	lines := []string{
		fmt.Sprintf("<skill_content name=%q>", s.Name),
		strings.TrimSpace(body),
	}
	names := make([]string, 0, len(s.resources))
	for rname := range s.resources {
		names = append(names, rname)
	}
	sort.Strings(names)
	if len(names) > 0 {
		lines = append(lines, "<skill_resources>")
		for _, rname := range names {
			lines = append(lines, "  <file>"+rname+"</file>")
		}
		lines = append(lines, "</skill_resources>")
	}
	lines = append(lines, "</skill_content>")
	return strings.Join(lines, "\n"), nil
}

// ReadResource runs the named resource function and returns its text, capped
// like Python's MemoryRuntime (Python parity: read_resource).
func (rt *MemoryRuntime) ReadResource(ctx *agocontext.Context, name, resource string) (string, error) {
	s, err := rt.find(name)
	if err != nil {
		return "", err
	}
	fn, ok := s.resources[resource]
	if !ok {
		return "", fmt.Errorf("%w: resource %q not found in memory skill %q", tools.ErrNotFound, resource, name)
	}
	text, err := fn(ctx)
	if err != nil {
		return "", err
	}
	if len([]rune(text)) > memoryResourceReadCap {
		r := []rune(text)
		return string(r[:memoryResourceReadCap]) + "\n<!-- resource truncated -->", nil
	}
	return text, nil
}
