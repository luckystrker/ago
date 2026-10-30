package knowledge

import "fmt"

// SentinelPath marks a store that has already been bootstrapped. It holds the
// actor name that ran the bootstrap (Python parity: _KnowledgeContext writes
// "/.initialized" with the agent name before running the bootstrap).
const SentinelPath = "/.initialized"

// StoreBootstrap initializes a store with a starting structure. It runs once
// per store — the first time an actor starts against a store without
// SentinelPath (Python parity: the StoreBootstrap protocol in bootstrap.py).
type StoreBootstrap interface {
	Bootstrap(store Store, actorName string) error
}

// DefaultBootstrap creates the standard store layout of SKILL.md files.
// MentionTool mirrors Python's mention_tool: when true the root SKILL.md tells
// the model to use the "knowledge" tool; set it to ExposeTool's value so the
// prompt never references a tool the model cannot call.
type DefaultBootstrap struct {
	MentionTool bool
}

// Bootstrap writes the standard layout: the root SKILL.md plus one SKILL.md per
// conventional directory (log, artifacts, memory). Texts follow bootstrap.py.
func (b DefaultBootstrap) Bootstrap(store Store, actorName string) error {
	rootIntro := "This is the agent's persistent knowledge store. " +
		"It is read and written by framework policies and " +
		"aggregation strategies — there is no tool exposed to " +
		"the model for direct access."
	if b.MentionTool {
		rootIntro = "This is your persistent knowledge store. Use the `knowledge` tool to manage it."
	}

	// AGO-DEVIATION: Python's layout names the log directory "/log/"; the AGo
	// event log lives under "/logs/" (the LogsDir pin from the D-027 brief), so
	// both the root SKILL.md listing and the log SKILL.md location use "/logs/".
	// Everything else — texts and paths — is bootstrap.py verbatim.
	root := "# " + actorName + " Knowledge Store\n\n" +
		rootIntro + "\n\n" +
		"## Directories\n" +
		"- `/" + LogsDir + "/` -- Conversation history (auto-managed)\n" +
		"- `/artifacts/` -- External files and data\n" +
		"- `/memory/` -- Working memory and summaries (auto-managed)\n"

	if err := store.Write("/"+LogsDir+"/SKILL.md", []byte(
		"Conversation logs. Each file is a JSONL record of one conversation's events. "+
			"Auto-populated by the framework after each conversation.",
	)); err != nil {
		return fmt.Errorf("knowledge: bootstrap log skill: %w", err)
	}

	if err := store.Write("/artifacts/SKILL.md", []byte(
		"External data: uploaded files, downloaded content, reference materials. "+
			"Write here to store data you want to reference later.",
	)); err != nil {
		return fmt.Errorf("knowledge: bootstrap artifacts skill: %w", err)
	}

	if err := store.Write("/memory/SKILL.md", []byte(
		"Working memory and conversation summaries. "+
			"`working.md` contains your current persistent state. "+
			"`conversations/` contains per-conversation summaries. "+
			"Both are auto-updated by aggregation strategies.",
	)); err != nil {
		return fmt.Errorf("knowledge: bootstrap memory skill: %w", err)
	}

	if err := store.Write("/SKILL.md", []byte(root)); err != nil {
		return fmt.Errorf("knowledge: bootstrap root skill: %w", err)
	}
	return nil
}

// BootstrapOnce runs b on store unless SentinelPath marks the store
// initialized. The sentinel (holding actorName) is written before the layout,
// so a concurrent second caller never duplicates the bootstrap (Python parity:
// _KnowledgeContext.enter — same order, same file).
func BootstrapOnce(store Store, actorName string, b StoreBootstrap) error {
	if store.Exists(SentinelPath) {
		return nil
	}
	if err := store.Write(SentinelPath, []byte(actorName)); err != nil {
		return fmt.Errorf("knowledge: write bootstrap sentinel: %w", err)
	}
	return b.Bootstrap(store, actorName)
}
