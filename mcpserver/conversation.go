package mcpserver

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/stream"
)

// ConversationMetaKey is where a conversation's handle travels back in a tool
// result's _meta, for clients threading it programmatically (Python parity:
// CONVERSATION_META_KEY).
const ConversationMetaKey = "ai.ag2/conversation"

// stdioSession is the conversation key of the stdio transport, which carries no
// Mcp-Session-Id and serves one client per process: every turn of the process
// shares one accumulating conversation.
const stdioSession = "stdio"

// ConversationConfig turns on multi-turn history for a served agent (Python
// parity: ag2.mcp.SessionConfig).
//
// AGO-DEVIATION: Python binds each conversation to the principal that created it
// (the access token's subject) and treats a handle from another principal as
// unknown. AGo has no OAuth access-token store (D-058), so the handle is the sole
// credential: it is an unguessable UUID the store minted, but anyone holding it
// can continue the conversation. Put an Authorize hook or an authenticating proxy
// in front of a server whose callers must not share conversations. A call lands in the conversation named by its
// "conversation" argument; without one it continues the MCP session's own
// conversation; with neither it starts a new conversation under a fresh handle.
type ConversationConfig struct {
	// MaxConversations is the LRU cap on conversations held at once. Every call
	// naming none, with no MCP session to fall back on, mints one — size for the
	// call rate and set a TTL. A conversation mid-turn is never evicted, so the
	// cap gives way while all of them are. Zero means 1024.
	MaxConversations int
	// TTL is the idle expiry measured from the end of the last turn; zero means
	// no expiry.
	TTL time.Duration
	// Storage backs the history of every conversation; nil means in-memory. The
	// handle-to-history table is per-process either way, so a shared storage does
	// not make a handle portable.
	Storage stream.Storage
}

const defaultMaxConversations = 1024

// ErrUnknownConversation reports a conversation handle naming no live
// conversation. It reaches the client as a tool-level error, so the model can
// start a new conversation instead of failing the turn.
var ErrUnknownConversation = errors.New("mcpserver: unknown or expired conversation handle")

// conversation is one conversation as a call sees it.
type conversation struct {
	stream *stream.MemoryStream
	handle string // "" for a stateless call: nothing to continue
}

type convEntry struct {
	key      string
	handle   string
	streamID string
	turn     sync.Mutex
	last     time.Time
	inTurn   int // turns running or waiting; an entry in a turn is never evicted
}

// conversations is the bounded LRU registry mapping a key to a persistent
// history. The key is a handle this store minted or, on the handshake era, the
// caller's MCP session id; it never adopts a key from a caller. Every call gets
// a fresh stream reading the shared history back, so per-call subscribers never
// accumulate.
type conversations struct {
	max     int
	ttl     time.Duration
	storage stream.Storage

	mu       sync.Mutex
	order    []string // keys, least recently used first
	entries  map[string]*convEntry
	byHandle map[string]string
}

func newConversations(cfg ConversationConfig) *conversations {
	max := cfg.MaxConversations
	if max <= 0 {
		max = defaultMaxConversations
	}
	storage := cfg.Storage
	if storage == nil {
		storage = stream.NewMemoryStorage()
	}
	return &conversations{max: max, ttl: cfg.TTL, storage: storage, entries: map[string]*convEntry{}, byHandle: map[string]string{}}
}

func (c *conversations) touchLocked(e *convEntry) {
	e.last = time.Now()
	for i, k := range c.order {
		if k == e.key {
			c.order = append(append(c.order[:i:i], c.order[i+1:]...), e.key)
			return
		}
	}
	c.order = append(c.order, e.key)
}

func (c *conversations) dropLocked(key string) {
	e := c.entries[key]
	if e == nil {
		return
	}
	delete(c.entries, key)
	delete(c.byHandle, e.handle)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	_ = c.storage.DropHistory(e.streamID)
}

func (c *conversations) evictLocked(serving string) {
	if c.ttl > 0 {
		now := time.Now()
		for _, k := range append([]string(nil), c.order...) {
			if e := c.entries[k]; e != nil && e.inTurn == 0 && now.Sub(e.last) > c.ttl {
				c.dropLocked(k)
			}
		}
	}
	for len(c.entries) > c.max {
		victim := ""
		for _, k := range c.order {
			if k != serving && c.entries[k].inTurn == 0 {
				victim = k
				break
			}
		}
		if victim == "" {
			return
		}
		c.dropLocked(victim)
	}
}

// acquire resolves the entry for key (created when absent, with minted when set
// as its handle) and reserves a turn on it.
func (c *conversations) acquire(key, minted string, create bool) (*convEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		if !create {
			return nil, ErrUnknownConversation
		}
		handle := minted
		if handle == "" {
			handle = uuid.New()
		}
		e = &convEntry{key: key, handle: handle, streamID: "conv-" + uuid.New()}
		c.entries[key] = e
		c.byHandle[handle] = key
	}
	c.touchLocked(e)
	e.inTurn++
	c.evictLocked(key)
	return e, nil
}

// run executes fn as a turn of the entry's conversation, one turn at a time.
func (c *conversations) run(e *convEntry, fn func(conversation) error) error {
	e.turn.Lock()
	defer func() {
		c.mu.Lock()
		e.inTurn--
		c.touchLocked(e)
		c.mu.Unlock()
		e.turn.Unlock()
	}()
	s := stream.NewMemoryStream(stream.WithStorage(c.storage), stream.WithStreamID(e.streamID))
	return fn(conversation{stream: s, handle: e.handle})
}

// session continues the conversation of an MCP session.
func (c *conversations) session(sessionID string, fn func(conversation) error) error {
	e, err := c.acquire(sessionID, "", true)
	if err != nil {
		return err
	}
	return c.run(e, fn)
}

// fresh mints a conversation under a new handle.
func (c *conversations) fresh(fn func(conversation) error) error {
	handle := uuid.New()
	e, err := c.acquire(handle, handle, true)
	if err != nil {
		return err
	}
	return c.run(e, fn)
}

// byHandleRun continues the conversation a handle names.
func (c *conversations) byHandleRun(handle string, fn func(conversation) error) error {
	c.mu.Lock()
	c.evictLocked("") // an expired conversation must not answer to its handle
	key, ok := c.byHandle[handle]
	c.mu.Unlock()
	if !ok {
		return ErrUnknownConversation
	}
	e, err := c.acquire(key, "", false)
	if err != nil {
		return err
	}
	return c.run(e, fn)
}

func (c *conversations) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// describeBounds words the conversation lifetime for the tool description, as
// the protocol requires of a stateful handle.
func (c *conversations) describeBounds() string {
	bound := fmt.Sprintf("once it is not among the %d most recently used", c.max)
	if c.ttl <= 0 {
		return "Lifetime: a conversation is dropped " + bound + "."
	}
	return fmt.Sprintf("Lifetime: a conversation is dropped after %g seconds idle, or %s.", c.ttl.Seconds(), bound)
}

// handleText is the text block carrying the handle for the model, which does not
// read protocol metadata.
func handleText(handle string) string {
	return "Conversation handle: " + handle + "\nPass it back as the 'conversation' argument to continue this conversation."
}
