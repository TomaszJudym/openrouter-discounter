package rank

// Composite ranking weights in percent, summing to 100: price weights the
// discount (−Δ%), performance the benchmark score. Both terms are 0-100.
// Edit both lines together; the openrouter-price-ratio skill rewrites them.
const (
	pricePercent       = 50
	performancePercent = 50
)

// Persona composite weights in percent, summing to 100, for the Disco
// Elysium persona presets (strongest-discount-*, see SelectStrongestDiscount):
// same composite equation, price-heavy. Report sectors, expensive-long and
// the hermes-* presets keep the global pair above.
const (
	personaPricePercent       = 80
	personaPerformancePercent = 20
)

// scoreWeight and priceWeight are the composite factors (0-1), the persona*
// pair likewise; vars only so tests can pin them (see TestMain).
var (
	scoreWeight        = performancePercent / 100.0
	priceWeight        = pricePercent / 100.0
	personaScoreWeight = personaPerformancePercent / 100.0
	personaPriceWeight = personaPricePercent / 100.0
)

// Admech role quality weights in percent; the rest of each role's
// composite prices it (discount depth for byDiscount roles, absolute
// cheapness for byCost ones — see admech.go). The oracle's hard problems
// prize ability; review output is acted on, so the reviewer is
// quality-heavy; the researcher drafts at half and half; the auditor
// reads logs and the delegate executes well-specified tasks, so price
// leads there.
const (
	oracleQualityPercent     = 80
	reviewerQualityPercent   = 70
	researcherQualityPercent = 50
	auditorQualityPercent    = 40
	delegateQualityPercent   = 20
)

// admechQualityWeight maps each scored admech role to its quality factor
// (0-1); vars only so tests can pin it (see TestMain).
var admechQualityWeight = map[Sector]float64{
	AdmechOracle:     oracleQualityPercent / 100.0,
	AdmechReviewer:   reviewerQualityPercent / 100.0,
	AdmechResearcher: researcherQualityPercent / 100.0,
	AdmechAuditor:    auditorQualityPercent / 100.0,
	AdmechDelegate:   delegateQualityPercent / 100.0,
}

// Compile-time guards: each admech weight is in 0-100.
var (
	_ [oracleQualityPercent]struct{}
	_ [100 - oracleQualityPercent]struct{}
	_ [reviewerQualityPercent]struct{}
	_ [100 - reviewerQualityPercent]struct{}
	_ [researcherQualityPercent]struct{}
	_ [100 - researcherQualityPercent]struct{}
	_ [auditorQualityPercent]struct{}
	_ [100 - auditorQualityPercent]struct{}
	_ [delegateQualityPercent]struct{}
	_ [100 - delegateQualityPercent]struct{}
)

// Compile-time guards (negative array length fails the build): each weight
// is in 0-100 and both sum to 100.
var (
	_ [pricePercent]struct{}
	_ [100 - pricePercent]struct{}
	_ [performancePercent]struct{}
	_ [100 - performancePercent]struct{}
	_ [1 - 2*((pricePercent+performancePercent-100)*(pricePercent+performancePercent-100))]struct{}
	_ [personaPricePercent]struct{}
	_ [100 - personaPricePercent]struct{}
	_ [personaPerformancePercent]struct{}
	_ [100 - personaPerformancePercent]struct{}
	_ [1 - 2*((personaPricePercent+personaPerformancePercent-100)*(personaPricePercent+personaPerformancePercent-100))]struct{}
)
