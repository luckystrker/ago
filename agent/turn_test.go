package agent

import (
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/luckystrker/ago/events"
)

// quickMergeOrigins enumerates every origin state an inbox request can carry,
// including the legacy empty one that counts as user in the merge rule.
var quickMergeOrigins = []events.RequestOrigin{"", events.OriginUser, events.OriginService}

// quickMergeCase generates one pending-inbox batch: zero to six requests, each
// with a random origin, zero to three text parts, and occasionally metadata
// sharing the key "shared" across requests so the last-wins rule has material
// to bite on.
type quickMergeCase struct {
	reqs []*events.ModelRequest
}

func (quickMergeCase) Generate(r *rand.Rand, size int) reflect.Value {
	var c quickMergeCase
	if n := r.Intn(7); n > 0 {
		c.reqs = make([]*events.ModelRequest, n)
	}
	for i := range c.reqs {
		req := &events.ModelRequest{Origin: quickMergeOrigins[r.Intn(len(quickMergeOrigins))]}
		for j := r.Intn(4); j > 0; j-- {
			req.Parts = append(req.Parts, &events.TextInput{Content: string(rune('a' + r.Intn(3)))})
		}
		if r.Intn(2) == 0 {
			req.Metadata = map[string]any{"shared": i, "own": i}
		}
		c.reqs[i] = req
	}
	return reflect.ValueOf(c)
}

// TestQuickMergeRequestsFoldReference sweeps random inbox batches and pins
// mergeRequests against a straightforward reference fold (generative
// companion of TestMergeRequestsOriginRule): parts concatenate in request
// order, metadata is last-wins per key, and the merged origin follows "public
// request wins" — user when any merged request is user-origin or legacy-empty,
// service when every request is service-origin, and the zero origin for an
// empty batch, which must also carry no parts and no metadata.
func TestQuickMergeRequestsFoldReference(t *testing.T) {
	prop := func(c quickMergeCase) bool {
		var wantParts []events.Input
		wantMeta := map[string]any(nil)
		wantOrigin := events.RequestOrigin("")
		if len(c.reqs) > 0 {
			wantOrigin = events.OriginService
			for _, req := range c.reqs {
				wantParts = append(wantParts, req.Parts...)
				for k, v := range req.Metadata {
					if wantMeta == nil {
						wantMeta = map[string]any{}
					}
					wantMeta[k] = v
				}
				if req.Origin == events.OriginUser || req.Origin == "" {
					wantOrigin = events.OriginUser
				}
			}
		}

		got := mergeRequests(c.reqs)
		if got.Origin != wantOrigin {
			return false
		}
		if len(wantParts) == 0 {
			if len(got.Parts) != 0 {
				return false
			}
		} else if !reflect.DeepEqual(got.Parts, wantParts) {
			return false
		}
		return reflect.DeepEqual(got.Metadata, wantMeta)
	}
	cfg := &quick.Config{MaxCount: 1000, Rand: rand.New(rand.NewSource(3))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatalf("mergeRequests fold property failed: %v", err)
	}
}
