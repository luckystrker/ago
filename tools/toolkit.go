package tools

import (
	"fmt"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
)

// Toolkit groups tools under one name (Python parity: ag2/tools/final/toolkit.py).
// Its schemas are the concatenation of its members'; Register delegates to each
// member. A Toolkit is safe for concurrent use after construction: NewToolkit is the
// only mutator, and per-turn member snapshots (used by ResolveTools) are copies.
type Toolkit struct {
	name  string
	mws   []middleware.Middleware
	tools map[string]Tool
	order []string
}

// NewToolkit builds a toolkit from ts. A duplicate member name is a conflict.
// The toolkit's middleware is attached to FunctionTool members (outermost layer of
// their own chain); members of other Tool kinds receive it only through Register.
func NewToolkit(name string, mws []middleware.Middleware, ts ...Tool) (*Toolkit, error) {
	if name == "" {
		name = "toolkit"
	}
	tk := &Toolkit{
		name:  name,
		mws:   append([]middleware.Middleware{}, mws...),
		tools: map[string]Tool{},
	}
	for _, t := range ts {
		if err := tk.add(t); err != nil {
			return nil, err
		}
	}
	return tk, nil
}

func (tk *Toolkit) add(t Tool) error {
	if ft, ok := t.(*FunctionTool); ok && len(tk.mws) > 0 {
		t = ft.withPrependedMiddleware(tk.mws)
	}
	if _, exists := tk.tools[t.Name()]; exists {
		return fmt.Errorf("%w: could not add tool `%s`: tool with such name already registered", ErrConflict, t.Name())
	}
	tk.tools[t.Name()] = t
	tk.order = append(tk.order, t.Name())
	return nil
}

func (tk *Toolkit) Name() string         { return tk.name }
func (tk *Toolkit) DeclaredInCode() bool { return true }

// Tools returns the members in insertion order.
func (tk *Toolkit) Tools() []Tool {
	out := make([]Tool, 0, len(tk.order))
	for _, n := range tk.order {
		out = append(out, tk.tools[n])
	}
	return out
}

func (tk *Toolkit) Schemas(cx *agocontext.Context) ([]any, error) {
	schemas := []any{}
	for _, t := range tk.Tools() {
		ss, err := t.Schemas(cx)
		if err != nil {
			return nil, err
		}
		schemas = append(schemas, ss...)
	}
	return schemas, nil
}

// Register subscribes every member and returns the union of their subscription
// ids for the turn scope's teardown. A failing member unwinds the members already
// subscribed (nothing is left behind), Python ExitStack parity.
func (tk *Toolkit) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	var subs []stream.SubID
	for _, t := range tk.Tools() {
		ids, err := t.Register(cx, mw)
		if err != nil {
			for _, id := range subs {
				cx.Stream.Unsubscribe(id)
			}
			return nil, err
		}
		subs = append(subs, ids...)
	}
	return subs, nil
}

// --- internal hooks for ResolveTools (Python: Toolkit._snapshot/_tools) --------

// toolMembers returns the current members in insertion order.
func (tk *Toolkit) toolMembers() []Tool { return tk.Tools() }

// toolSnapshot copies the toolkit (shallow member references) so a turn resolves and
// prunes against the members discovered in that turn even if another turn
// redisCOVERS different ones meanwhile.
func (tk *Toolkit) toolSnapshot() Tool {
	cp := &Toolkit{
		name:  tk.name,
		mws:   append([]middleware.Middleware{}, tk.mws...),
		tools: make(map[string]Tool, len(tk.tools)),
		order: append([]string{}, tk.order...),
	}
	for k, v := range tk.tools {
		cp.tools[k] = v
	}
	return cp
}

// setMembers replaces the member set (used on per-turn snapshots after member
// discovery).
func (tk *Toolkit) setMembers(members []Tool) {
	tk.tools = make(map[string]Tool, len(members))
	tk.order = tk.order[:0]
	for _, m := range members {
		if m == nil {
			continue
		}
		tk.tools[m.Name()] = m
		tk.order = append(tk.order, m.Name())
	}
}

// withMembers returns a copy of the toolkit holding only the given members
// (ResolveTools pruning); nil entries are dropped. The receiver is never mutated.
func (tk *Toolkit) withMembers(members []Tool) *Toolkit {
	cp := &Toolkit{
		name:  tk.name,
		mws:   append([]middleware.Middleware{}, tk.mws...),
		tools: map[string]Tool{},
	}
	for _, m := range members {
		if m == nil {
			continue
		}
		cp.tools[m.Name()] = m
		cp.order = append(cp.order, m.Name())
	}
	return cp
}
