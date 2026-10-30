package policies

import (
	"testing"

	"github.com/luckystrker/ago/knowledge"
)

func TestWorkingMemoryPolicy(t *testing.T) {
	t.Run("missing file injects nothing", func(t *testing.T) {
		p := NewWorkingMemoryPolicy(knowledge.NewMemoryStore())
		prompts, evs, err := p.Apply([]string{"sys"}, nil, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(prompts) != 1 || prompts[0] != "sys" {
			t.Errorf("prompts = %q, want unchanged", prompts)
		}
		if evs != nil {
			t.Errorf("events = %v, want nil passed through", evs)
		}
	})

	t.Run("file content becomes a prompt fragment", func(t *testing.T) {
		store := knowledge.NewMemoryStore()
		if err := store.Write(WorkingMemoryPath, []byte("# state\n- step 1")); err != nil {
			t.Fatalf("write: %v", err)
		}
		prompts, _, err := NewWorkingMemoryPolicy(store).Apply([]string{"sys"}, nil, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		want := "## Working Memory\n\n# state\n- step 1"
		if len(prompts) != 2 || prompts[1] != want {
			t.Errorf("prompts = %q, want %q appended", prompts, want)
		}
	})

	t.Run("nil store injects nothing", func(t *testing.T) {
		prompts, _, err := NewWorkingMemoryPolicy(nil).Apply([]string{"sys"}, nil, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(prompts) != 1 {
			t.Errorf("prompts = %q, want unchanged", prompts)
		}
	})
}

func TestEpisodicMemoryPolicy(t *testing.T) {
	t.Run("missing directory injects nothing", func(t *testing.T) {
		prompts, _, err := NewEpisodicMemoryPolicy(knowledge.NewMemoryStore()).Apply([]string{"sys"}, nil, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(prompts) != 1 || prompts[0] != "sys" {
			t.Errorf("prompts = %q, want unchanged", prompts)
		}
	})

	t.Run("summaries join into one fragment", func(t *testing.T) {
		store := knowledge.NewMemoryStore()
		if err := store.Write("conversations/2026-01", []byte("about dogs")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := store.Write("conversations/2026-02", []byte("about cats")); err != nil {
			t.Fatalf("write: %v", err)
		}
		prompts, _, err := NewEpisodicMemoryPolicy(store).Apply([]string{"sys"}, nil, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		want := "## Past Conversations\n\nabout dogs\n\n---\n\nabout cats"
		if len(prompts) != 2 || prompts[1] != want {
			t.Errorf("prompts = %q, want %q appended", prompts, want)
		}
	})

	t.Run("max episodes keeps the most recent", func(t *testing.T) {
		store := knowledge.NewMemoryStore()
		for name, summary := range map[string]string{
			"c1": "first",
			"c2": "second",
			"c3": "third",
		} {
			if err := store.Write(ConversationsDir+name, []byte(summary)); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		prompts, _, err := NewEpisodicMemoryPolicy(store, WithMaxEpisodes(1)).Apply(nil, nil, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		want := "## Past Conversations\n\nthird"
		if len(prompts) != 1 || prompts[0] != want {
			t.Errorf("prompts = %q, want %q", prompts, want)
		}
	})
}
