package toolpacks

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luckystrker/ago/tools"
)

// newSkillsFixture writes a skills directory with two skills (one frontmatter
// named, one falling back to its directory name) and one non-skill entry.
func newSkillsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("alpha/SKILL.md", `---
name: alpha
description: Alpha skill for testing.
---

# Alpha

Follow the alpha procedure.
`)
	write("alpha/references/guide.md", "# Guide\n\nStep one.")
	write("beta/SKILL.md", "no frontmatter here, just a body")
	write("notaskill/notes.txt", "not a skill")
	return root
}

func TestSkillsToolkitCatalogAndLoad(t *testing.T) {
	st, err := NewSkillsToolkit(newSkillsFixture(t))
	if err != nil {
		t.Fatalf("NewSkillsToolkit: %v", err)
	}
	if st.Name() != "skills_toolkit" {
		t.Fatalf("name = %q", st.Name())
	}
	members := st.Tools()
	if len(members) != 3 ||
		members[0].Name() != "list_skills" ||
		members[1].Name() != "load_skill" ||
		members[2].Name() != "read_skill_resource" {
		t.Fatalf("members = %v", members)
	}

	_, cx := newTestContext()
	ev := callTool(t, st, cx, "list_skills", `{}`)
	var catalog []map[string]string
	if err := json.Unmarshal([]byte(firstDataJSON(t, ev)), &catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(catalog) != 2 {
		t.Fatalf("catalog = %v, want alpha and beta", catalog)
	}
	// Sorted by name; the frontmatter-less skill falls back to its directory
	// name; notaskill is skipped.
	if catalog[0]["name"] != "alpha" || catalog[0]["description"] != "Alpha skill for testing." {
		t.Fatalf("alpha entry = %v", catalog[0])
	}
	if catalog[1]["name"] != "beta" {
		t.Fatalf("beta entry = %v, want the directory-name fallback", catalog[1])
	}

	ev = callTool(t, st, cx, "load_skill", `{"name":"alpha"}`)
	body := resultPartText(t, ev)
	for _, want := range []string{
		`<skill_content name="alpha"`,
		"Follow the alpha procedure.",
		"Skill directory: ",
		"<file>references/guide.md</file>",
		"</skill_content>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("load_skill body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "description: Alpha skill") {
		t.Fatalf("load_skill leaked the frontmatter:\n%s", body)
	}
}

func TestSkillsToolkitResourceRead(t *testing.T) {
	st, err := NewSkillsToolkit(newSkillsFixture(t))
	if err != nil {
		t.Fatalf("NewSkillsToolkit: %v", err)
	}
	_, cx := newTestContext()

	ev := callTool(t, st, cx, "read_skill_resource", `{"name":"alpha","resource":"references/guide.md"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "Step one.") {
		t.Fatalf("resource read = %q", got)
	}

	// Unknown skill and unknown resource are tools.ErrNotFound-flavored errors.
	for _, args := range []string{
		`{"name":"ghost","resource":"x.md"}`,
		`{"name":"alpha","resource":"missing.md"}`,
		`{"name":"alpha","resource":"../alpha/SKILL.md"}`,
		`{"name":"alpha","resource":"../outside.txt"}`,
	} {
		ev := callTool(t, st, cx, "read_skill_resource", args)
		if got := resultPartText(t, ev); !strings.Contains(got, "TOOL_ERROR") {
			t.Fatalf("read_skill_resource %s = %q, want an error", args, got)
		}
	}
}

func TestSkillsToolkitNameEnum(t *testing.T) {
	st, err := NewSkillsToolkit(newSkillsFixture(t))
	if err != nil {
		t.Fatalf("NewSkillsToolkit: %v", err)
	}
	_, cx := newTestContext()
	schemas, err := st.Schemas(cx)
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	// The load_skill schema constrains the name to the discovered skills (the
	// Literal[name, ...] parity).
	b, err := json.Marshal(schemas)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"enum":["alpha","beta"]`) {
		t.Fatalf("schemas = %s, want the name enum", b)
	}
	var _ tools.Tool = st
}

func TestSkillsToolkitEmptyAndMissingDir(t *testing.T) {
	empty := t.TempDir()
	st, err := NewSkillsToolkit(empty)
	if err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, st, cx, "list_skills", `{}`)
	if got := firstDataJSON(t, ev); got != "[]" {
		t.Fatalf("empty catalog = %q", got)
	}
	if _, err := NewSkillsToolkit(filepath.Join(empty, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing dir error = %v, want os.ErrNotExist", err)
	}
}
