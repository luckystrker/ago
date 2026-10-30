package tools

import (
	"fmt"
	"log/slog"

	agocontext "github.com/luckystrker/ago/context"
)

// ResolvedTools is the set of tools one turn registers and the schemas it exposes:
// one tool per callable name.
type ResolvedTools struct {
	Tools   []Tool
	Schemas []any
	// Known holds every callable name: function names for function schemas, the
	// provider-side type for hosted-tool schemas. The executor's not-found
	// fallback skips them.
	Known map[string]bool
}

// ResolveTools resolves all so that every name the model can call belongs to one
// tool (Python parity: ag2/tools/precedence.py). The model calls a tool by name and
// a ToolCallEvent reaches every tool subscribed to that name, so a turn keeps
// exactly one tool per name. Toolkits are resolved member by member, after their
// Schemas call discovers the members.
//
// Precedence, when two tools share a name:
//   - tools declared in code override each other in declaration order: the later one
//     wins, with a warning;
//   - a tool a remote peer provides at runtime never overrides one declared in code;
//     it is dropped with a warning;
//   - between two remote-provided tools the first one wins; the other is dropped.
//
// Hosted-tool schemas of one type may repeat; only a function name clashes. A
// dropped tool is not registered, so it never receives a call, and losing toolkit
// members are pruned from the toolkit for this turn.
func ResolveTools(all []Tool, cx *agocontext.Context) (*ResolvedTools, error) {
	var leaves []*leaf
	declared := make([]Tool, len(all))
	for i, t := range all {
		nt, err := collectLeaves(t, cx, "", &leaves)
		if err != nil {
			return nil, err
		}
		declared[i] = nt
	}

	keep := selectLeaves(leaves)
	pos := 0
	resolved := []Tool{}
	for _, t := range declared {
		if p := pruneTool(t, keep, &pos); p != nil {
			resolved = append(resolved, p)
		}
	}

	schemas := []any{}
	for _, t := range resolved {
		ss, err := t.Schemas(cx)
		if err != nil {
			return nil, err
		}
		schemas = append(schemas, ss...)
	}
	known := map[string]bool{}
	for _, s := range schemas {
		k, err := schemaKeyOf(s)
		if err != nil {
			return nil, err
		}
		known[k.name] = true
	}
	return &ResolvedTools{Tools: resolved, Schemas: schemas, Known: known}, nil
}

// leaf is one schema-contributing tool together with the names it claims.
type leaf struct {
	tool   Tool
	keys   []schemaKey
	source string
}

// collectLeaves walks the tool tree depth-first, recording one leaf per
// schema-contributing tool, and returns the per-turn snapshot to resolve against.
// Toolkit schemas are the discovery hook; the snapshot pins the discovered members.
func collectLeaves(t Tool, cx *agocontext.Context, parent string, out *[]*leaf) (Tool, error) {
	source := parent + toolSource(t)
	if tk, ok := t.(*Toolkit); ok {
		if _, err := tk.Schemas(cx); err != nil {
			return nil, err
		}
		snap := tk.toolSnapshot().(*Toolkit)
		members := snap.toolMembers()
		collected := make([]Tool, len(members))
		for i, m := range members {
			r, err := collectLeaves(m, cx, source+" > ", out)
			if err != nil {
				return nil, err
			}
			collected[i] = r
		}
		snap.setMembers(collected)
		return snap, nil
	}
	ss, err := t.Schemas(cx)
	if err != nil {
		return nil, err
	}
	keys, err := keysFromSchemas(ss)
	if err != nil {
		return nil, err
	}
	*out = append(*out, &leaf{tool: t, keys: keys, source: source})
	return t, nil
}

type ownerEntry struct {
	leaf       *leaf
	isFunction bool
}

// selectLeaves decides, for every leaf in order, whether it keeps its place in the
// turn. A tool declared in code wins over every rival already in place; a
// runtime-provided tool loses to any rival already in place.
func selectLeaves(leaves []*leaf) []bool {
	owners := map[string][]ownerEntry{}
	kept := []*leaf{}
	for _, lf := range leaves {
		var rivals []*leaf
		for _, k := range lf.keys {
			for _, o := range owners[k.name] {
				// Hosted schemas of one type may repeat; only a function name —
				// which clashes with anything sharing it — creates a rivalry.
				if (k.isFunction || o.isFunction) && !containsLeaf(rivals, o.leaf) {
					rivals = append(rivals, o.leaf)
				}
			}
		}
		if len(rivals) > 0 && !lf.tool.DeclaredInCode() {
			reportPrecedence(rivals[0], lf)
			continue
		}
		for _, rival := range rivals {
			reportPrecedence(lf, rival)
			kept = removeLeaf(kept, rival)
			for name, entries := range owners {
				owners[name] = filterOwners(entries, rival)
			}
		}
		kept = append(kept, lf)
		for _, k := range lf.keys {
			owners[k.name] = append(owners[k.name], ownerEntry{leaf: lf, isFunction: k.isFunction})
		}
	}
	keep := make([]bool, len(leaves))
	for i, lf := range leaves {
		keep[i] = containsLeaf(kept, lf)
	}
	return keep
}

// reportPrecedence logs one resolution decision (Python parity: _report).
func reportPrecedence(winner, loser *leaf) {
	var name string
	for _, k := range loser.keys {
		for _, wk := range winner.keys {
			if k.name == wk.name {
				name = k.name
				break
			}
		}
		if name != "" {
			break
		}
	}
	switch {
	case loser.tool.DeclaredInCode():
		slog.Warn(fmt.Sprintf("Tool `%s` from %s is overridden by %s.", name, loser.source, winner.source))
	case winner.tool.DeclaredInCode():
		slog.Warn(fmt.Sprintf(
			"Tool `%s` provided by %s is ignored: %s declares a tool with that name, "+
				"and tools declared in code take precedence over tools a remote peer provides.",
			name, loser.source, winner.source))
	default:
		slog.Warn(fmt.Sprintf(
			"Tool `%s` provided by %s is ignored: %s already provides it. "+
				"If the tools come from MCP servers, set `tool_name_prefix` on a server config to keep both.",
			name, loser.source, winner.source))
	}
}

// pruneTool rebuilds the tool tree without the leaves dropped by selectLeaves,
// consuming one keep flag per leaf in collection order (Python parity: _prune).
func pruneTool(t Tool, keep []bool, pos *int) Tool {
	if tk, ok := t.(*Toolkit); ok {
		members := tk.toolMembers()
		pruned := make([]Tool, len(members))
		same := true
		for i, m := range members {
			p := pruneTool(m, keep, pos)
			pruned[i] = p
			if p == nil || !sameTool(p, m) {
				same = false
			}
		}
		if same {
			return tk
		}
		return tk.withMembers(pruned)
	}
	if *pos >= len(keep) || !keep[*pos] {
		*pos++
		return nil
	}
	*pos++
	return t
}

// sameTool reports identity: a pruned subtree that lost nothing is the original.
func sameTool(a, b Tool) bool {
	if a == nil || b == nil {
		return false
	}
	return a == b
}

func containsLeaf(leaves []*leaf, l *leaf) bool {
	for _, x := range leaves {
		if x == l {
			return true
		}
	}
	return false
}

func removeLeaf(leaves []*leaf, l *leaf) []*leaf {
	out := leaves[:0:0]
	for _, x := range leaves {
		if x != l {
			out = append(out, x)
		}
	}
	return out
}

func filterOwners(entries []ownerEntry, l *leaf) []ownerEntry {
	out := entries[:0:0]
	for _, e := range entries {
		if e.leaf != l {
			out = append(out, e)
		}
	}
	return out
}
