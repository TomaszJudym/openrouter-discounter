package rank

import (
	"slices"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

// admechModel is hermesModel with the catalog intelligence index filled,
// which admechPool reads straight from the catalog.
func admechModel(id string, c caps, ii float64) discounts.Model {
	m := hermesModel(id, c)
	m.Benchmarks.ArtificialAnalysis.IntelligenceIndex = ii
	return m
}

func admechIDs(out []SectorResult, sec Sector) []string {
	for _, sr := range out {
		if sr.Sector == sec {
			return idsOf(sr.Rows)
		}
	}
	return nil
}

// sectorScores wraps by-model scores with the epoch source name.
func sectorScores(by map[string]float64) SectorScores {
	return SectorScores{Source: "Epoch AI", ByModel: by}
}

// near-free prices keep cheapness above cheapEnough, so the diversity
// penalty never fires and ordering follows scores alone.
func TestSelectAdmechFilters(t *testing.T) {
	strong := 40.0
	models := []discounts.Model{
		admechModel("a/all", caps{ctx: 500_000, tools: true, think: true}, strong),
		admechModel("b/notools", caps{ctx: 500_000, think: true}, strong),
		admechModel("c/smallctx", caps{ctx: 128_000, tools: true}, strong),
		admechModel("d/big", caps{ctx: 500_000, tools: true}, strong),
		admechModel("e/noii", caps{ctx: 500_000, tools: true}, 0),
		admechModel("f/x:batch", caps{ctx: 500_000, tools: true}, strong),
		admechModel("g/x:free", caps{ctx: 500_000, tools: true}, strong),
		admechModel("h/nonchat", caps{ctx: 500_000, tools: true}, strong),
	}
	models[7].Architecture.OutputModalities = []string{"image"} // not chat
	ids := []string{"a/all", "b/notools", "c/smallctx", "d/big", "e/noii", "f/x:batch", "g/x:free", "h/nonchat"}
	discs := make([]discounts.Discount, 0, len(ids))
	for _, id := range ids {
		discs = append(discs, disc(id, 0.001, 0.002))
	}
	scores := map[Sector]SectorScores{
		Math: sectorScores(map[string]float64{"a/all": 60, "c/smallctx": 50, "d/big": 40}),
		Code: sectorScores(map[string]float64{"a/all": 90, "c/smallctx": 70, "d/big": 60}),
		LCR:  sectorScores(map[string]float64{"a/all": 90, "b/notools": 90, "c/smallctx": 90, "d/big": 90}),
	}
	out := SelectAdmech(models, discs, discs, scores)
	tests := []struct {
		sec  Sector
		want []string
	}{
		{AdmechOracle, []string{"a/all", "c/smallctx", "d/big"}},    // tools
		{AdmechReviewer, []string{"a/all", "c/smallctx", "d/big"}},  // tools
		{AdmechResearcher, []string{"a/all", "b/notools", "d/big"}}, // ctx ≥ 200k
		{AdmechAuditor, []string{"a/all", "b/notools", "d/big"}},    // ctx ≥ 400k
		{AdmechDelegate, []string{"a/all", "c/smallctx", "d/big"}},  // tools
		{AdmechScout, []string{"a/all", "b/notools", "c/smallctx"}}, // any pool model, top 3
	}
	for _, tt := range tests {
		if got := admechIDs(out, tt.sec); !slices.Equal(got, tt.want) {
			t.Errorf("%s = %v, want %v", tt.sec, got, tt.want)
		}
	}
}

// The oracle weighs quality 80% against discount depth, so the stronger
// model wins despite the same discount; scout ranks by price, so the
// cheapest wins regardless of scores.
func TestSelectAdmechObjectiveOrdering(t *testing.T) {
	models := []discounts.Model{
		admechModel("a/strong", caps{ctx: 500_000, tools: true}, 40),
		admechModel("b/cheap", caps{ctx: 500_000, tools: true}, 20),
		admechModel("c/mid", caps{ctx: 500_000, tools: true}, 30),
	}
	discs := []discounts.Discount{
		disc("a/strong", 0.2, 0.6), // priciest
		disc("b/cheap", 0.01, 0.03),
		disc("c/mid", 0.1, 0.3),
	}
	by := map[string]float64{"a/strong": 90, "b/cheap": 10, "c/mid": 50}
	scores := map[Sector]SectorScores{Math: sectorScores(by), Code: sectorScores(by)}
	out := SelectAdmech(models, discs, discs, scores)
	if got := admechIDs(out, AdmechOracle); got == nil || got[0] != "a/strong" {
		t.Errorf("oracle = %v, want a/strong first (stronger, pricier)", got)
	}
	if got := admechIDs(out, AdmechScout); got == nil || got[0] != "b/cheap" {
		t.Errorf("scout = %v, want b/cheap first (cheapest)", got)
	}
	if sr := out[0]; sr.Sector != AdmechOracle || sr.Source != SourceOracle {
		t.Errorf("first sector/source = %s/%s, want %s/%s", sr.Sector, sr.Source, AdmechOracle, SourceOracle)
	}
}

// The scout floor keeps only candidates at or above the pool's median
// popularity: c and d are cheaper than everyone but too unpopular. The
// auditor floor works the same way over its own pool's scores.
func TestSelectAdmechFloorsDropTooWeak(t *testing.T) {
	models := []discounts.Model{
		admechModel("a/hot", caps{ctx: 500_000}, 40),
		admechModel("b/warm", caps{ctx: 500_000}, 30),
		admechModel("c/cool", caps{ctx: 500_000}, 20),
		admechModel("d/cold", caps{ctx: 500_000}, 10),
	}
	lcr := sectorScores(map[string]float64{"a/hot": 90, "b/warm": 50, "c/cool": 30, "d/cold": 10})
	discs := []discounts.Discount{
		disc("a/hot", 0.5, 1.0),
		disc("b/warm", 0.05, 0.1),
		disc("c/cool", 0.05, 0.1),
		disc("d/cold", 0.01, 0.02),
	}
	out := SelectAdmech(models, discs, discs, map[Sector]SectorScores{LCR: lcr})
	if got := admechIDs(out, AdmechScout); !slices.Equal(got, []string{"b/warm", "a/hot"}) {
		t.Errorf("scout = %v, want [b/warm a/hot] (below-median c/d dropped)", got)
	}
	if got := admechIDs(out, AdmechAuditor); !slices.Equal(got, []string{"b/warm", "a/hot"}) {
		t.Errorf("auditor = %v, want [b/warm a/hot] (below-median c/d dropped)", got)
	}
	// the auditor floor uses the auditor pool's own scores: a/hot unscored
	partial := sectorScores(map[string]float64{"b/warm": 50, "c/cool": 30, "d/cold": 10})
	out = SelectAdmech(models, discs, discs, map[Sector]SectorScores{LCR: partial})
	if got := admechIDs(out, AdmechAuditor); !slices.Equal(got, []string{"b/warm", "c/cool"}) {
		t.Errorf("auditor = %v, want [b/warm c/cool] (unscored a/hot dropped)", got)
	}
}

// The oracle's #1 repeats in the reviewer slot only when its quality lead
// beats the diversity penalty.
func TestSelectAdmechDiversityPenalty(t *testing.T) {
	c := caps{ctx: 500_000, tools: true}
	models := []discounts.Model{admechModel("a/top", c, 40), admechModel("b/close", c, 30), admechModel("c/far", c, 20)}
	discs := []discounts.Discount{disc("a/top", 0.12, 0.45), disc("b/close", 0.12, 0.45), disc("c/far", 0.12, 0.45)}
	// close scores: a's 5-point reviewer lead is under the 15-point penalty
	code := sectorScores(map[string]float64{"a/top": 100, "b/close": 95, "c/far": 90})
	math := sectorScores(map[string]float64{"a/top": 100, "b/close": 90, "c/far": 80})
	out := SelectAdmech(models, discs, discs, map[Sector]SectorScores{Code: code, Math: math})
	if got := admechIDs(out, AdmechOracle); got == nil || got[0] != "a/top" {
		t.Fatalf("oracle = %v, want a/top first", got)
	}
	if got := admechIDs(out, AdmechReviewer); got == nil || got[0] != "b/close" {
		t.Errorf("reviewer = %v, want b/close first (a penalised)", got)
	}
	// a far stronger lead survives the penalty
	code = sectorScores(map[string]float64{"a/top": 100, "b/close": 60, "c/far": 30})
	out = SelectAdmech(models, discs, discs, map[Sector]SectorScores{Code: code})
	if got := admechIDs(out, AdmechReviewer); got == nil || got[0] != "a/top" {
		t.Errorf("reviewer = %v, want a/top first (lead > penalty)", got)
	}
}

// A role whose source failed ranks price-only — unscored rows, deepest
// discount first for byDiscount roles — carrying the sanitized source
// error verbatim for the notice.
func TestSelectAdmechPriceOnlyFallback(t *testing.T) {
	c := caps{ctx: 500_000, tools: true}
	models := []discounts.Model{admechModel("a/deep", c, 40), admechModel("b/shallow", c, 30)}
	discs := []discounts.Discount{
		deep("a/deep", 0.05, 0.15),    // −90%
		disc("b/shallow", 0.1, 0.196), // −50%
	}
	errScores := map[Sector]SectorScores{Code: {Source: "Epoch AI", Err: "429 rate limit, retries exhausted"}}
	out := SelectAdmech(models, discs, discs, errScores)
	var sr SectorResult
	for _, r := range out {
		if r.Sector == AdmechReviewer {
			sr = r
		}
	}
	if sr.Sector != AdmechReviewer {
		t.Fatal("reviewer omitted from results")
	}
	if sr.Err != "429 rate limit, retries exhausted" {
		t.Errorf("reviewer Err = %q, want the source error", sr.Err)
	}
	if sr.Source != "Epoch AI" {
		t.Errorf("reviewer Source = %q, want Epoch AI", sr.Source)
	}
	if got := admechIDs(out, AdmechReviewer); !slices.Equal(got, []string{"a/deep", "b/shallow"}) {
		t.Errorf("reviewer ids = %v, want deepest discount first (price-only)", got)
	}
	for _, row := range sr.Rows {
		if row.Scored {
			t.Errorf("reviewer row %s scored, want unscored price-only rows", row.ModelID)
		}
	}
	// missing map entry: same fallback, synthetic no-scores message
	out = SelectAdmech(models, discs, discs, map[Sector]SectorScores{})
	for _, r := range out {
		if r.Sector == AdmechReviewer && r.Err != noScoresMessage {
			t.Errorf("reviewer Err = %q, want %q", r.Err, noScoresMessage)
		}
	}
	// oracle with both sources failed names the first present source's error
	errScores[Math] = SectorScores{Source: "Epoch AI", Err: "source down"}
	out = SelectAdmech(models, discs, discs, errScores)
	for _, r := range out {
		if r.Sector == AdmechOracle && r.Err != "source down" {
			t.Errorf("oracle Err = %q, want %q", r.Err, "source down")
		}
	}
}

// When one oracle source fails the other is used alone, with no error.
func TestSelectAdmechOracleGoodSourceSurvivesBadOne(t *testing.T) {
	c := caps{ctx: 500_000, tools: true}
	models := []discounts.Model{admechModel("a/m", c, 40), admechModel("b/m", c, 30)}
	discs := []discounts.Discount{disc("a/m", 0.1, 0.4), disc("b/m", 0.1, 0.4)}
	scores := map[Sector]SectorScores{
		Math: {Source: "Epoch AI", Err: "source down"},
		Code: sectorScores(map[string]float64{"a/m": 90, "b/m": 80}),
	}
	out := SelectAdmech(models, discs, discs, scores)
	for _, sr := range out {
		if sr.Sector != AdmechOracle {
			continue
		}
		if sr.Err != "" {
			t.Errorf("oracle Err = %q, want empty (one source is good)", sr.Err)
		}
		if sr.Source != "Epoch AI" {
			t.Errorf("oracle Source = %q, want the good source's name", sr.Source)
		}
		if got := idsOf(sr.Rows); !slices.Equal(got, []string{"a/m", "b/m"}) {
			t.Errorf("oracle rows = %v, want [a/m b/m]", got)
		}
		return
	}
	t.Fatal("oracle omitted from results")
}

// The delegate keeps only code-scored candidates when at least three have
// one, ranking quality against cheapness; with fewer, everyone competes
// by cheapness alone.
func TestSelectAdmechDelegateCodeFloor(t *testing.T) {
	c := caps{ctx: 500_000, tools: true}
	models := []discounts.Model{
		admechModel("a/scored1", c, 40),
		admechModel("b/scored2", c, 30),
		admechModel("c/scored3", c, 20),
		admechModel("d/cheap", c, 10),
	}
	discs := []discounts.Discount{
		disc("a/scored1", 0.3, 0.6),
		disc("b/scored2", 0.2, 0.4),
		disc("c/scored3", 0.15, 0.3),
		disc("d/cheap", 0.01, 0.02),
	}
	code := sectorScores(map[string]float64{"a/scored1": 90, "b/scored2": 80, "c/scored3": 70})
	out := SelectAdmech(models, discs, discs, map[Sector]SectorScores{Code: code})
	got := admechIDs(out, AdmechDelegate)
	if !slices.Equal(got, []string{"c/scored3", "b/scored2", "a/scored1"}) {
		t.Errorf("delegate = %v, want the scored candidates only, quality vs cheapness", got)
	}
	// fewer than 3 scored: d/cheap leads on price
	out = SelectAdmech(models, discs, discs, map[Sector]SectorScores{Code: sectorScores(map[string]float64{"a/scored1": 90, "b/scored2": 80})})
	got = admechIDs(out, AdmechDelegate)
	if !slices.Equal(got, []string{"d/cheap", "c/scored3", "b/scored2"}) {
		t.Errorf("delegate = %v, want d/cheap first (too few code scores: cheapest wins)", got)
	}
	for _, sr := range out {
		if sr.Sector == AdmechDelegate && (sr.Source != SourceCheapness || sr.Err != "") {
			t.Errorf("delegate source/err = %q/%q, want %q/empty", sr.Source, sr.Err, SourceCheapness)
		}
	}
}

func TestSelectAdmechRoleOrder(t *testing.T) {
	c := caps{ctx: 500_000, tools: true}
	models := []discounts.Model{admechModel("a/m", c, 40)}
	out := SelectAdmech(models, []discounts.Discount{disc("a/m", 0.1, 0.4)}, []discounts.Discount{disc("a/m", 0.1, 0.4)}, map[Sector]SectorScores{})
	want := []Sector{AdmechOracle, AdmechReviewer, AdmechResearcher, AdmechAuditor, AdmechDelegate, AdmechScout}
	if len(out) != len(want) {
		t.Fatalf("got %d sectors, want %d", len(out), len(want))
	}
	for i, sr := range out {
		if sr.Sector != want[i] {
			t.Errorf("position %d = %s, want %s", i, sr.Sector, want[i])
		}
	}
}

func TestSelectAdmechNeedsRegistered(t *testing.T) {
	for _, sec := range []Sector{AdmechOracle, AdmechReviewer, AdmechResearcher, AdmechAuditor, AdmechDelegate, AdmechScout} {
		if _, ok := needs[sec]; !ok {
			t.Errorf("%s missing from needs map", sec)
		}
		n := sec.Need()
		if !n.Require.Capped {
			t.Errorf("%s Need not Capped", sec)
		}
	}
	if n := AdmechOracle.Need(); n.Require.MinOutput != PersonaMaxTokens || !n.Require.Tools {
		t.Errorf("oracle Need = %+v, want Tools with MinOutput %d", n, PersonaMaxTokens)
	}
	if n := AdmechResearcher.Need(); n.Require.MinContext != researcherMinCtx {
		t.Errorf("researcher MinContext = %d, want %d", n.Require.MinContext, researcherMinCtx)
	}
	if n := AdmechAuditor.Need(); n.Require.MinContext != auditorMinCtx {
		t.Errorf("auditor MinContext = %d, want %d", n.Require.MinContext, auditorMinCtx)
	}
	if n := AdmechDelegate.Need(); n.Require.MinContext != 0 || !n.Require.Tools {
		t.Errorf("delegate Need = %+v, want Tools without MinContext", n)
	}
}
