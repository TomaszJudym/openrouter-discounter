package rank

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

// Admech role sectors: one preset per admech coding-team slot.
const (
	AdmechOracle     Sector = "admech-code-oracle"
	AdmechReviewer   Sector = "admech-code-reviewer"
	AdmechResearcher Sector = "admech-code-researcher"
	AdmechAuditor    Sector = "admech-code-auditor"
	AdmechDelegate   Sector = "admech-code-delegate"
	AdmechScout      Sector = "admech-code-scout"
	AdmechVision     Sector = "admech-code-vision"
)

// Role-context floors and the oracle's merged-source label.
const (
	researcherMinCtx = 200_000
	auditorMinCtx    = 400_000
	// SourceOracle labels the oracle's merged scores when both of its
	// sources contribute; spelled out because rank cannot import epoch
	// (epoch imports rank for the sector types).
	SourceOracle = "max(Epoch AI OTIS, Epoch AI WebDev)"
)

// admechRoles lists the roles in pick order: earlier roles claim their #1
// first, later roles pay the diversity penalty for reusing it. The keep
// filters repeat the model-level half of each role's Need (see needs.go);
// the endpoint-level half is applied by the re-pick.
var admechRoles = []role{
	{AdmechOracle, func(c candidate) bool { return c.model.ToolCalling() }, byDiscount},
	{AdmechReviewer, func(c candidate) bool { return c.model.ToolCalling() }, byDiscount},
	{AdmechResearcher, func(c candidate) bool { return c.model.ContextLength >= researcherMinCtx }, byDiscount},
	{AdmechAuditor, func(c candidate) bool { return c.model.ContextLength >= auditorMinCtx }, byCost},
	{AdmechDelegate, func(c candidate) bool { return c.model.ToolCalling() }, byCost},
	// scout answers quick lookups: any pool model will do
	{AdmechScout, func(c candidate) bool { return true }, byCost},
	// looker is the admech family's vision companion (like hermes-vision):
	// vision-only, ranked by cheapness so it exists even on zero-discount days
	{AdmechVision, func(c candidate) bool { return c.model.Vision() }, byCost},
}

// SelectAdmech picks the top 3 models for each admech role, in role order
// oracle, reviewer, researcher, auditor, delegate, scout, looker. The pool and
// endpoint re-picking follow SelectHermes: byDiscount roles draw from the
// discounted rows and rank quality against discount depth; byCost roles
// draw from every priced row and rank quality against absolute cheapness.
// Quality comes from the already fetched report sectors in scores:
// oracle the max of its min-max scaled Math and Code scores, reviewer and
// delegate the Code scores, researcher and auditor the LCR scores, and
// scout's popularity floor the LCR scores too (the role itself ranks by
// cheapness). When a role's source fails or none of its candidates is
// scored, the role ranks price-only — unscored rows, Err carrying the
// sanitized source error for the notice. A model already #1 in an earlier
// role loses diversityPenalty unless it is cheapEnough for the role.
// Roles with no eligible model are omitted.
func SelectAdmech(models []discounts.Model, discs, priced []discounts.Discount, scores map[Sector]SectorScores) []SectorResult {
	pools := map[pricing][]candidate{
		byDiscount: admechPool(models, discs),
		byCost:     admechPool(models, priced),
	}
	taken := make(map[string]bool)
	var out []SectorResult
	for _, r := range admechRoles {
		var cands []candidate
		for _, c := range pools[r.pricing] {
			if r.keep(c) {
				cands = append(cands, c)
			}
		}
		sr := admechScoreRole(r, cands, scores, taken)
		if len(sr.Rows) == 0 {
			continue
		}
		taken[sr.Rows[0].ModelID] = true
		out = append(out, sr)
	}
	return out
}

// admechPool keeps chat models with a positive catalog intelligence index
// from rows, excluding :free and :batch variants; one row per model id.
// (hermesPool is the same filter over a passed-in intelligence map; the
// admech signature carries only the fetched sector scores, so the index
// comes straight from the catalog.)
func admechPool(models []discounts.Model, discs []discounts.Discount) []candidate {
	catalog := make(map[string]discounts.Model, len(models))
	for _, m := range models {
		catalog[m.ID] = m
	}
	seen := make(map[string]bool, len(discs))
	var pool []candidate
	for _, d := range discs {
		if seen[d.ModelID] || strings.HasSuffix(d.ModelID, ":free") || strings.HasSuffix(d.ModelID, ":batch") {
			continue
		}
		m, ok := catalog[d.ModelID]
		if !ok || !m.ChatText() {
			continue
		}
		ii := m.Benchmarks.ArtificialAnalysis.IntelligenceIndex
		if ii <= 0 {
			continue
		}
		seen[d.ModelID] = true
		pool = append(pool, candidate{model: m, disc: d, ii: ii})
	}
	return pool
}

// admechScoreRole ranks one role's candidates and keeps the top 3 pinnable
// rows. Quality mode mixes the role's score with its price term (−Δ% for
// byDiscount roles, cheapness for byCost ones) at the role's weight
// (weights.go). Cheap mode ranks by cheapness and shows it as the score
// (scout, and delegate when too few candidates carry a code score).
// Price-only mode — the source failed — ranks by price unscored: discount
// depth for byDiscount roles, absolute price for byCost ones, with Err
// carrying the notice error.
func admechScoreRole(r role, pool []candidate, scores map[Sector]SectorScores, taken map[string]bool) SectorResult {
	need := r.sector.Need()
	cands := repick(r, pool)
	cheap := make(map[string]float64, len(cands))
	for _, c := range cands {
		cheap[c.disc.ModelID] = cheapness(c.disc.Price, need.Workload)
	}
	source, cands, raw, serr := admechScores(r.sector, cands, scores)
	if serr == "" && raw == nil {
		source = SourceCheapness
	}
	type ranked struct {
		Row
		value float64
	}
	rows := make([]ranked, 0, len(cands))
	for _, c := range cands {
		row := rowOf(c.disc)
		var value float64
		switch {
		case serr != "":
			if r.pricing == byCost {
				value = -row.Price
			} else {
				value = -row.Pct
			}
		case raw == nil:
			row.Score, row.Scored = cheap[row.ModelID], true
			value = row.Score
		default:
			row.Score, row.Scored = raw[row.ModelID], true
			price := cheap[row.ModelID]
			if r.pricing == byDiscount {
				price = -row.Pct
			}
			q := admechQualityWeight[r.sector]
			value = q*row.Score + (1-q)*price
		}
		if taken[row.ModelID] && cheap[row.ModelID] < cheapEnough {
			value -= diversityPenalty
		}
		rows = append(rows, ranked{row, value})
	}
	slices.SortStableFunc(rows, func(a, b ranked) int {
		if c := cmp.Compare(b.value, a.value); c != 0 {
			return c
		}
		if r.pricing == byCost {
			return cmp.Compare(a.Price, b.Price)
		}
		return byPct(a.Row, b.Row)
	})
	ordered := make([]Row, len(rows))
	for i, row := range rows {
		ordered[i] = row.Row
	}
	return SectorResult{Sector: r.sector, Source: source, Err: serr, Rows: Pinnable(ordered, roleTopN)}
}

// admechScores resolves one role's quality scores: the source label, the
// kept candidates, their score per id, and a source error when the role
// must rank price-only. A nil raw with no error marks cheap mode (rank
// and score by cheapness): scout, and delegate with too few code scores.
func admechScores(sec Sector, cands []candidate, scores map[Sector]SectorScores) (string, []candidate, map[string]float64, string) {
	switch sec {
	case AdmechOracle:
		return oracleScores(cands, scores)
	case AdmechScout:
		return scoutScores(cands, scores)
	case AdmechDelegate:
		return delegateScores(cands, scores)
	case AdmechAuditor:
		src, by, err, ok := roleSource(LCR, cands, scores)
		if !ok {
			return src, cands, nil, err
		}
		return src, floorKept(cands, by, p50(mapValues(by))), by, ""
	case AdmechVision:
		// cheap mode: raw nil, no error, no quality floor — the LCR
		// popularity floor is about long-context reasoning and must not
		// gate a vision looker
		return "", cands, nil, ""
	case AdmechReviewer:
		src, by, err, ok := roleSource(Code, cands, scores)
		if !ok {
			return src, cands, nil, err
		}
		return src, scoredKept(cands, by), by, ""
	case AdmechResearcher:
		src, by, err, ok := roleSource(LCR, cands, scores)
		if !ok {
			return src, cands, nil, err
		}
		return src, scoredKept(cands, by), by, ""
	default:
		return NoSourceName, cands, nil, noScoresMessage
	}
}

// roleSource resolves one sector's scores for a role's candidates. When
// ok, source names the source and by holds the candidates' scores. When
// not — the sector is missing, its source errored, or none of the
// candidates is scored — source and err are the price-only fallback's
// notice pair and by is nil.
func roleSource(sec Sector, cands []candidate, scores map[Sector]SectorScores) (source string, by map[string]float64, err string, ok bool) {
	sc, found := scores[sec]
	if !found {
		return NoSourceName, nil, noScoresMessage, false
	}
	if sc.Err != "" {
		return sc.Source, nil, sc.Err, false
	}
	by = make(map[string]float64, len(cands))
	for _, c := range cands {
		if s, has := sc.ByModel[c.disc.ModelID]; has {
			by[c.disc.ModelID] = s
		}
	}
	if len(by) == 0 {
		return sc.Source, nil, noScoresMessage, false
	}
	return sc.Source, by, "", true
}

// scoredKept drops the candidates the source does not rate: a model
// without a score cannot hold a quality-weighted slot.
func scoredKept(cands []candidate, by map[string]float64) []candidate {
	kept := make([]candidate, 0, len(by))
	for _, c := range cands {
		if _, ok := by[c.disc.ModelID]; ok {
			kept = append(kept, c)
		}
	}
	return kept
}

// floorKept keeps only the candidates at or above floor; a floor of 0
// (empty scores) keeps everything.
func floorKept(cands []candidate, by map[string]float64, floor float64) []candidate {
	kept := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if by[c.disc.ModelID] >= floor {
			kept = append(kept, c)
		}
	}
	return kept
}

// oracleScores merges the role's two sources. Each usable source's scores
// are min-max scaled across the role's candidates first (the providers
// scale against their whole populations, so the role-local rescale puts
// the OTIS and WebDev spreads on one footing, see epoch.go), then a
// candidate's score is the max of its available values. When one source
// fails or is absent the other is used alone and no error is reported;
// when both are unusable the role ranks price-only.
func oracleScores(cands []candidate, scores map[Sector]SectorScores) (string, []candidate, map[string]float64, string) {
	mOK, cOK := sourceUsable(Math, scores), sourceUsable(Code, scores)
	if !mOK && !cOK {
		src, err := oracleFallback(scores)
		return src, cands, nil, err
	}
	merged := make(map[string]float64, len(cands))
	for _, sec := range []Sector{Math, Code} {
		if !sourceUsable(sec, scores) {
			continue
		}
		part := make(map[string]float64, len(cands))
		for _, c := range cands {
			if s, ok := scores[sec].ByModel[c.disc.ModelID]; ok {
				part[c.disc.ModelID] = s
			}
		}
		if len(part) == 0 {
			continue
		}
		minMax(part)
		maps.Copy(merged, part)
	}
	kept := scoredKept(cands, merged)
	source := SourceOracle
	if mOK != cOK { // exactly one usable source: name it
		if mOK {
			source = scores[Math].Source
		} else {
			source = scores[Code].Source
		}
	}
	return source, kept, merged, ""
}

// sourceUsable reports whether a sector's scores are present, error-free
// and non-empty. The oracle merges such sources; single-source roles use
// roleSource, which additionally requires a scored candidate.
func sourceUsable(sec Sector, scores map[Sector]SectorScores) bool {
	sc, ok := scores[sec]
	return ok && sc.Err == "" && len(sc.ByModel) > 0
}

// oracleFallback is the notice pair for an oracle whose sources are both
// unusable: the first present source's own error, or the synthetic
// no-scores message.
func oracleFallback(scores map[Sector]SectorScores) (string, string) {
	for _, sec := range []Sector{Math, Code} {
		if sc, ok := scores[sec]; ok {
			if sc.Err != "" {
				return sc.Source, sc.Err
			}
			return sc.Source, noScoresMessage
		}
	}
	return NoSourceName, noScoresMessage
}

// scoutScores applies the popularity floor when the long-context scores
// are usable: candidates below the pool's median score drop. The role
// itself ranks by cheapness (raw nil, cheap mode). When the scores fail,
// the floor cannot be evaluated: the role ranks price-only and carries
// the notice error, so a degraded scout stays visible in the report.
func scoutScores(cands []candidate, scores map[Sector]SectorScores) (string, []candidate, map[string]float64, string) {
	src, by, err, ok := roleSource(LCR, cands, scores)
	if !ok {
		return src, cands, nil, err
	}
	return "", floorKept(cands, by, p50(mapValues(by))), nil, ""
}

// delegateScores is the price-dominant role. When at least minCodeScored
// candidates carry a code score it keeps only those and ranks quality
// against cheapness; with fewer, every candidate ranks by cheapness alone
// (raw nil, cheap mode). A failed code source ranks price-only.
func delegateScores(cands []candidate, scores map[Sector]SectorScores) (string, []candidate, map[string]float64, string) {
	src, by, err, ok := roleSource(Code, cands, scores)
	if !ok {
		return src, cands, nil, err
	}
	if len(by) < minCodeScored {
		return "", cands, nil, ""
	}
	return src, scoredKept(cands, by), by, ""
}

// p50 is the median of vs: the middle value, or the mean of the two
// middle values; 0 when vs is empty.
func p50(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := slices.Clone(vs)
	slices.Sort(s)
	if len(s)%2 == 0 {
		return (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return s[len(s)/2]
}

// mapValues lists a map's values in unspecified order.
func mapValues(m map[string]float64) []float64 {
	out := make([]float64, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
