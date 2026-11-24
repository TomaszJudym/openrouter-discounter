package rank

// Composite ranking weights in percent, summing to 100: price weights the
// discount (−Δ%), performance the benchmark score. Both terms are 0-100.
// Edit both lines together; the openrouter-price-ratio skill rewrites them.
const (
	pricePercent       = 50
	performancePercent = 50
)

// scoreWeight and priceWeight are the composite factors (0-1); vars only so
// tests can pin them (see TestMain).
var (
	scoreWeight = performancePercent / 100.0
	priceWeight = pricePercent / 100.0
)

// Weights sum to 100: a compile error otherwise (negative array length).
var _ [1 - 2*((pricePercent+performancePercent-100)*(pricePercent+performancePercent-100))]struct{}
