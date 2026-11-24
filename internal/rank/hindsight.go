package rank

import (
	"cmp"
	"slices"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

// HindsightFree is the free preset for the Hindsight memory daemon's
// consolidation and reflect jobs. Reflect runs a tool loop, so every pick
// must call tools.
const HindsightFree Sector = "hindsight-free"

// hindsightMinShare drops candidates scoring below this share of the best
// one's AA Intelligence Index: a tiny model that happens to accept a schema
// stores worse memories than none.
const hindsightMinShare = 0.5

// SelectHindsightFree returns the top 3 free chat models that call tools,
// strongest first by AA Intelligence Index; among equal scores, models that
// accept a structured output schema lead (Hindsight asks for JSON on every
// call), then id. Models below hindsightMinShare of the best score and
// harness-only :free endpoints are skipped. Unlike TextFree, vision
// models qualify: Hindsight sends text only, and image input costs nothing.
func SelectHindsightFree(freeIDs []string, models []discounts.Model, intel map[string]float64) SectorResult {
	catalog := make(map[string]discounts.Model, len(models))
	for _, m := range models {
		catalog[m.ID] = m
	}
	type cand struct {
		Row
		json bool
	}
	var cands []cand
	for _, id := range freeIDs {
		m, ok := catalog[id]
		// harness-only endpoints serve named agent harnesses; the memory
		// daemon is not one
		if !ok || !m.ChatText() || !m.ToolCalling() || HarnessOnly(id) {
			continue
		}
		if ii := intel[id]; ii > 0 {
			cands = append(cands, cand{Row{ModelID: id, Score: ii, Scored: true}, m.StructuredOutput()})
		}
	}
	slices.SortStableFunc(cands, func(a, b cand) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		if a.json != b.json {
			if a.json {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.ModelID, b.ModelID)
	})
	sr := SectorResult{Sector: HindsightFree, Source: SourceIntelligence}
	for _, c := range cands {
		if len(sr.Rows) == roleTopN || c.Score < hindsightMinShare*cands[0].Score {
			break
		}
		sr.Rows = append(sr.Rows, c.Row)
	}
	return sr
}
