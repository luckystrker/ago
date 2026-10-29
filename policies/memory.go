package policies

import (
	"errors"
	"fmt"
	"strings"

	agocontext "ago/context"
	"ago/events"
	"ago/knowledge"
)

// WorkingMemoryPath is the knowledge file holding the agent's persistent state
// (Python parity: knowledge.WORKING_MEMORY_PATH).
const WorkingMemoryPath = "/memory/working.md"

// WorkingMemoryPolicy injects WorkingMemoryPath from the knowledge store as a
// prompt fragment, giving the agent continuity between conversations. A missing
// or empty file injects nothing.
//
// AGO-DEVIATION: Python resolves the store from context dependencies; AGo takes
// it in the constructor.
type WorkingMemoryPolicy struct {
	store knowledge.Store
}

// NewWorkingMemoryPolicy injects the working-memory file from store.
func NewWorkingMemoryPolicy(store knowledge.Store) WorkingMemoryPolicy {
	return WorkingMemoryPolicy{store: store}
}

// Apply appends the working-memory fragment; the event list passes through.
func (p WorkingMemoryPolicy) Apply(prompts []string, evs []events.Event, _ *agocontext.Context) ([]string, []events.Event, error) {
	if p.store == nil {
		return prompts, evs, nil
	}
	content, err := p.store.Read(WorkingMemoryPath)
	if err != nil {
		if errors.Is(err, knowledge.ErrNotFound) {
			return prompts, evs, nil
		}
		return nil, nil, fmt.Errorf("policies: read working memory: %w", err)
	}
	if len(content) > 0 {
		prompts = append(prompts, fmt.Sprintf("## Working Memory\n\n%s", content))
	}
	return prompts, evs, nil
}

// ConversationsDir is the knowledge directory holding per-conversation summary
// files, one file per conversation.
//
// AGO-DEVIATION: Python reads /memory/conversations/ (CONVERSATIONS_PREFIX); the
// AGo contract names the directory "conversations/".
const ConversationsDir = "conversations/"

// defaultMaxEpisodes is the Python episodic_memory.py default.
const defaultMaxEpisodes = 5

// EpisodicMemoryPolicy injects the summaries of the most recent conversations
// from ConversationsDir as one prompt fragment. A missing directory, or no
// readable summaries, injects nothing.
//
// AGO-DEVIATION: Python's transparency note ("[episodic_memory] Injected N past
// conversation summaries.") is dropped — the AGo contract names a single
// fragment.
type EpisodicMemoryPolicy struct {
	store       knowledge.Store
	maxEpisodes int
}

// EpisodicOption configures an EpisodicMemoryPolicy.
type EpisodicOption func(*EpisodicMemoryPolicy)

// WithMaxEpisodes bounds how many of the most recent conversation summaries are
// injected (default 5).
func WithMaxEpisodes(n int) EpisodicOption {
	return func(p *EpisodicMemoryPolicy) { p.maxEpisodes = n }
}

// NewEpisodicMemoryPolicy injects conversation summaries from store.
func NewEpisodicMemoryPolicy(store knowledge.Store, opts ...EpisodicOption) EpisodicMemoryPolicy {
	p := EpisodicMemoryPolicy{store: store, maxEpisodes: defaultMaxEpisodes}
	for _, o := range opts {
		o(&p)
	}
	return p
}

// Apply appends the past-conversations fragment; the event list passes through.
func (p EpisodicMemoryPolicy) Apply(prompts []string, evs []events.Event, _ *agocontext.Context) ([]string, []events.Event, error) {
	if p.store == nil {
		return prompts, evs, nil
	}
	entries, err := p.store.List(ConversationsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("policies: list conversations: %w", err)
	}
	if len(entries) == 0 {
		return prompts, evs, nil
	}
	limit := p.maxEpisodes
	if limit <= 0 {
		limit = defaultMaxEpisodes
	}
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	var summaries []string
	for _, entry := range entries {
		content, err := p.store.Read(ConversationsDir + entry)
		if err != nil {
			if errors.Is(err, knowledge.ErrNotFound) {
				continue
			}
			return nil, nil, fmt.Errorf("policies: read conversation summary %s: %w", entry, err)
		}
		if len(content) > 0 {
			summaries = append(summaries, string(content))
		}
	}
	if len(summaries) == 0 {
		return prompts, evs, nil
	}
	block := "## Past Conversations\n\n" + strings.Join(summaries, "\n\n---\n\n")
	return append(prompts, block), evs, nil
}
