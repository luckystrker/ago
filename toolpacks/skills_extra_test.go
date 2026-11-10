package toolpacks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
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
