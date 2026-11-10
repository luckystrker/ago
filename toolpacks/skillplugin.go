package toolpacks

import (
	"strings"

	"github.com/luckystrker/ago/plugin"
)

// NewSkillPlugin turns a skills toolkit into an agent plugin that injects the
// skill catalog into the system prompt and exposes the activation tools
// (Python parity: ag2/tools/skills/plugin.py SkillPlugin).
//
// Instead of spending a tool round-trip on list_skills, the model sees an
// <available_skills> block (name, description and location per skill) on
// startup, then calls load_skill(name) and, when some skill bundles resources,
// read_skill_resource(name, resource). The tools are a construction-time
// snapshot of the discovered set, and a capability gate keeps dead tools out:
// read_skill_resource is registered only when at least one skill has a
// resource. With no skills the plugin contributes nothing.
//
// The result is an ordinary plugin.Bundle contribution, applied through the
// agent's WithPlugin.
func NewSkillPlugin(toolkit *SkillsToolkit) plugin.Plugin {
	bundle := plugin.NewBundle("skills")
	if toolkit == nil {
		return bundle.Plugin()
	}
	skills := toolkit.Skills()
	if len(skills) == 0 {
		return bundle.Plugin()
	}
	catalog := make([]string, 0, len(skills))
	hasResources := false
	for _, s := range skills {
		catalog = append(catalog, skillXML(s))
		if len(s.Resources) > 0 {
			hasResources = true
		}
	}
	prompt := "The following skills provide specialized instructions for specific tasks.\n" +
		"When a task matches a skill's description, call load_skill(name) to load its\n" +
		"full instructions before proceeding.\n" +
		"<available_skills>\n" + strings.Join(catalog, "\n") + "\n</available_skills>"
	bundle.WithPrompt(prompt)
	for _, tool := range toolkit.Tools() {
		switch tool.Name() {
		case "load_skill":
			bundle.WithTool(tool)
		case "read_skill_resource":
			if hasResources {
				bundle.WithTool(tool)
			}
		}
	}
	return bundle.Plugin()
}

// SkillPlugin is an alias of NewSkillPlugin kept for parity with the Python
// constructor name.
func SkillPlugin(toolkit *SkillsToolkit) plugin.Plugin { return NewSkillPlugin(toolkit) }

// skillXML renders one catalog entry; values are XML-escaped so a description
// containing &, < or > cannot break the surrounding block (Python parity:
// plugin._skill_xml).
func skillXML(s SkillInfo) string {
	return "  <skill>\n" +
		"    <name>" + xmlEscape(s.Name) + "</name>\n" +
		"    <description>" + xmlEscape(s.Description) + "</description>\n" +
		"    <location>" + xmlEscape(s.Location) + "</location>\n" +
		"  </skill>"
}

// xmlEscape escapes text for inclusion in an XML element body the way Python's
// xml.sax.saxutils.escape does: only &, < and > change; quotes and newlines
// stay as they are.
func xmlEscape(text string) string {
	return xmlReplacer.Replace(text)
}

var xmlReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
