package knowledge

import (
	"errors"
	"strings"
	"testing"
)

// TestDefaultBootstrapLayout checks the standard layout: the root SKILL.md plus
// one SKILL.md per conventional directory, texts verbatim from bootstrap.py.
func TestDefaultBootstrapLayout(t *testing.T) {
	for _, be := range allBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			store := be.new()
			if err := (DefaultBootstrap{MentionTool: true}).Bootstrap(store, "alfred"); err != nil {
				t.Fatalf("bootstrap: %v", err)
			}

			root, err := store.Read("/SKILL.md")
			if err != nil {
				t.Fatalf("read root SKILL.md: %v", err)
			}
			rootText := string(root)
			for _, want := range []string{
				"# alfred Knowledge Store",
				"This is your persistent knowledge store. Use the `knowledge` tool to manage it.",
				"- `/" + LogsDir + "/` -- Conversation history (auto-managed)",
				"- `/artifacts/` -- External files and data",
				"- `/memory/` -- Working memory and summaries (auto-managed)",
			} {
				if !strings.Contains(rootText, want) {
					t.Errorf("root SKILL.md missing %q:\n%s", want, rootText)
				}
			}

			for path, want := range map[string]string{
				"/" + LogsDir + "/SKILL.md": "Conversation logs. Each file is a JSONL record of one conversation's events. " +
					"Auto-populated by the framework after each conversation.",
				"/artifacts/SKILL.md": "External data: uploaded files, downloaded content, reference materials. " +
					"Write here to store data you want to reference later.",
				"/memory/SKILL.md": "Working memory and conversation summaries. " +
					"`working.md` contains your current persistent state. " +
					"`conversations/` contains per-conversation summaries. " +
					"Both are auto-updated by aggregation strategies.",
			} {
				got, err := store.Read(path)
				if err != nil {
					t.Fatalf("read %s: %v", path, err)
				}
				if string(got) != want {
					t.Errorf("%s = %q, want %q", path, got, want)
				}
			}
		})
	}
}

// TestDefaultBootstrapNoTool checks the MentionTool=false intro (Python parity:
// the no-tool text used when expose_tool=False).
func TestDefaultBootstrapNoTool(t *testing.T) {
	store := NewMemoryStore()
	if err := (DefaultBootstrap{MentionTool: false}).Bootstrap(store, "agent-7"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	root, err := store.Read("/SKILL.md")
	if err != nil {
		t.Fatalf("read root SKILL.md: %v", err)
	}
	got := string(root)
	if strings.Contains(got, "`knowledge` tool") {
		t.Errorf("no-tool intro must not mention the knowledge tool:\n%s", got)
	}
	for _, want := range []string{
		"# agent-7 Knowledge Store",
		"This is the agent's persistent knowledge store. " +
			"It is read and written by framework policies and " +
			"aggregation strategies — there is no tool exposed to " +
			"the model for direct access.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("root SKILL.md missing %q:\n%s", want, got)
		}
	}
}

// TestBootstrapOnceSentinel checks the sentinel contract: the first call writes
// /.initialized with the actor name and the layout; a second call is skipped
// because the sentinel is present.
func TestBootstrapOnceSentinel(t *testing.T) {
	store := NewMemoryStore()
	b := DefaultBootstrap{MentionTool: true}
	if err := BootstrapOnce(store, "alfred", b); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if !store.Exists(SentinelPath) {
		t.Fatal("sentinel not written")
	}
	sentinel, err := store.Read(SentinelPath)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(sentinel) != "alfred" {
		t.Errorf("sentinel = %q, want actor name %q", sentinel, "alfred")
	}
	if !store.Exists("/SKILL.md") {
		t.Fatal("layout not written on first bootstrap")
	}

	// A later caller must not rewrite: tampered files survive.
	if err := store.Write("/artifacts/SKILL.md", []byte("TAMPERED")); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if err := BootstrapOnce(store, "someone-else", b); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	got, err := store.Read("/artifacts/SKILL.md")
	if err != nil {
		t.Fatalf("read tampered file: %v", err)
	}
	if string(got) != "TAMPERED" {
		t.Errorf("second bootstrap rewrote the layout: %q", got)
	}
	sentinel, err = store.Read(SentinelPath)
	if err != nil {
		t.Fatalf("re-read sentinel: %v", err)
	}
	if string(sentinel) != "alfred" {
		t.Errorf("sentinel overwritten: %q", sentinel)
	}
}

// flakyBootstrap fails on demand, to observe the sentinel ordering.
type flakyBootstrap struct{ fail bool }

func (f flakyBootstrap) Bootstrap(store Store, actorName string) error {
	if f.fail {
		return errors.New("bootstrap exploded")
	}
	return DefaultBootstrap{}.Bootstrap(store, actorName)
}

// TestBootstrapOnceSentinelAfterLayout proves the sentinel is the last write
// (audit L-22): a bootstrap that fails midway leaves no sentinel, so the next
// run retries the layout instead of settling for a half-initialized store.
func TestBootstrapOnceSentinelAfterLayout(t *testing.T) {
	store := NewMemoryStore()

	if err := BootstrapOnce(store, "alfred", flakyBootstrap{fail: true}); err == nil {
		t.Fatal("failing bootstrap: want error, got nil")
	}
	if store.Exists(SentinelPath) {
		t.Fatal("sentinel written before the layout completed")
	}

	if err := BootstrapOnce(store, "alfred", flakyBootstrap{}); err != nil {
		t.Fatalf("retry bootstrap: %v", err)
	}
	if !store.Exists(SentinelPath) {
		t.Fatal("successful bootstrap must write the sentinel")
	}
	if !store.Exists("/SKILL.md") {
		t.Fatal("successful bootstrap must write the layout")
	}
}
