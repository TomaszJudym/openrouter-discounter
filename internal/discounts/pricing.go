package discounts

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Rates is one price point in USD per million tokens. A zero CacheRead,
// CacheWrite or Reasoning means the price is not listed.
type Rates struct {
	Prompt     float64
	Completion float64
	CacheRead  float64
	CacheWrite float64
	Reasoning  float64
}

func (r Rates) scale(f float64) Rates {
	return Rates{r.Prompt * f, r.Completion * f, r.CacheRead * f, r.CacheWrite * f, r.Reasoning * f}
}

// Workload is the request profile blended prices are computed for.
type Workload struct {
	Input     float64 // prompt tokens per request
	Output    float64 // completion tokens per request, reasoning included
	CacheHit  float64 // share of Input served from the prompt cache
	Reasoning float64 // share of Output that is reasoning
}

// DefaultWorkload is a multi-turn agent request: a long, mostly cached
// prompt and a short answer. Every reported price and Δ% uses it.
var DefaultWorkload = Workload{Input: 30_000, Output: 1_500, CacheHit: 0.8, Reasoning: 0.5}

// LongWorkload is a long-context request, past the 272k-token tier at which
// several providers raise prices.
var LongWorkload = Workload{Input: 300_000, Output: 4_000, CacheHit: 0.8, Reasoning: 0.5}

// String describes the workload for the report header.
func (w Workload) String() string {
	return fmt.Sprintf("%s in (%.0f%% cached) + %s out", kTokens(w.Input), w.CacheHit*100, kTokens(w.Output))
}

func kTokens(n float64) string {
	return strconv.FormatFloat(n/1000, 'f', -1, 64) + "k"
}

// Blended is the cost of one request under w divided by its total tokens,
// USD per Mtok. Cache hits bill at the cache-read rate when listed; misses
// bill at the cache-write rate when it exceeds the prompt rate (a write is
// never cheaper than plain input); reasoning bills at its own rate when
// listed, else as completion.
func (r Rates) Blended(w Workload) float64 {
	hit := r.Prompt
	if r.CacheRead > 0 {
		hit = min(r.CacheRead, r.Prompt)
	}
	miss := max(r.Prompt, r.CacheWrite)
	reason := r.Completion
	if r.Reasoning > 0 {
		reason = r.Reasoning
	}
	in := w.Input * (w.CacheHit*hit + (1-w.CacheHit)*miss)
	out := w.Output * ((1-w.Reasoning)*r.Completion + w.Reasoning*reason)
	return (in + out) / (w.Input + w.Output)
}

// Tier is a context-length price tier: Rates apply from MinPrompt prompt
// tokens up.
type Tier struct {
	MinPrompt int
	Rates     Rates
}

// Tariff is an endpoint's prices at one moment: base rates plus context
// tiers in ascending MinPrompt order.
type Tariff struct {
	Base  Rates
	Tiers []Tier
}

// For returns the rates billed for a request of w's prompt size.
func (t Tariff) For(w Workload) Rates {
	r := t.Base
	for _, tr := range t.Tiers {
		if float64(tr.MinPrompt) <= w.Input {
			r = tr.Rates
		}
	}
	return r
}

// Blended is the blended USD per Mtok under w, tier included.
func (t Tariff) Blended(w Workload) float64 { return t.For(w).Blended(w) }

func (t Tariff) scale(f float64) Tariff {
	return t.mapRates(func(r Rates) Rates { return r.scale(f) })
}

// Override is one pricing.overrides entry. Entries with utc_days or
// utc_start/utc_end are time windows (utc_start/utc_end are HHMM, UTC; a
// window may wrap past midnight, utc_end 0 means midnight); entries with
// min_prompt_tokens are context tiers. Unset prices inherit the base.
type Override struct {
	Days       []string `json:"utc_days"`
	Start      *int     `json:"utc_start"`
	End        *int     `json:"utc_end"`
	MinPrompt  int      `json:"min_prompt_tokens"`
	Prompt     string   `json:"prompt"`
	Completion string   `json:"completion"`
	CacheRead  string   `json:"input_cache_read"`
	CacheWrite string   `json:"input_cache_write"`
	Reasoning  string   `json:"internal_reasoning"`
}

func (o Override) window() bool {
	return len(o.Days) > 0 || o.Start != nil || o.End != nil
}

// bounds is the window as HHMM [start, end); end 0 or unset is 2400.
func (o Override) bounds() (start, end int) {
	start, end = 0, 2400
	if o.Start != nil {
		start = *o.Start
	}
	if o.End != nil && *o.End != 0 {
		end = *o.End
	}
	return start, end
}

// activeAt reports whether the window covers at. In a window that wraps
// midnight, the hours after midnight belong to the previous day's window,
// so utc_days is checked against that day.
func (o Override) activeAt(at time.Time) bool {
	at = at.UTC()
	t := at.Hour()*100 + at.Minute()
	start, end := o.bounds()
	day := at
	switch {
	case start < end:
		if t < start || t >= end {
			return false
		}
	case start > end:
		if t < end {
			day = at.AddDate(0, 0, -1)
		} else if t < start {
			return false
		}
	}
	if len(o.Days) == 0 {
		return true
	}
	wd := day.Weekday().String()
	return slices.ContainsFunc(o.Days, func(d string) bool { return strings.EqualFold(d, wd) })
}

// label renders the window as "15:00–01:00 UTC", prefixed by its days; a
// days-only window is just its days.
func (o Override) label() string {
	days := strings.Join(o.Days, ",")
	if o.Start == nil && o.End == nil {
		return days
	}
	start, end := o.bounds()
	s := fmt.Sprintf("%02d:%02d–%02d:%02d UTC", start/100, start%100, end/100%24, end%100)
	if days != "" {
		s = days + " " + s
	}
	return s
}

// rates overlays the override's listed prices on base.
func (o Override) rates(base Rates) Rates {
	r := base
	for _, f := range []struct {
		dst *float64
		src string
	}{
		{&r.Prompt, o.Prompt}, {&r.Completion, o.Completion}, {&r.CacheRead, o.CacheRead},
		{&r.CacheWrite, o.CacheWrite}, {&r.Reasoning, o.Reasoning},
	} {
		if f.src != "" {
			*f.dst = perMtok(f.src)
		}
	}
	return r
}

// Pricing is a model's or endpoint's pricing, decimal USD per token.
// Discount is set on provider endpoints only: the listed prices are already
// discounted by that fraction.
type Pricing struct {
	Prompt     string     `json:"prompt"`
	Completion string     `json:"completion"`
	CacheRead  string     `json:"input_cache_read"`
	CacheWrite string     `json:"input_cache_write"`
	Reasoning  string     `json:"internal_reasoning"`
	Discount   float64    `json:"discount"`
	Overrides  []Override `json:"overrides"`
}

func (p Pricing) rates() Rates {
	return Rates{perMtok(p.Prompt), perMtok(p.Completion), perMtok(p.CacheRead), perMtok(p.CacheWrite), perMtok(p.Reasoning)}
}

// tariff is the base tariff: base rates plus the context tiers.
func (p Pricing) tariff() Tariff {
	t := Tariff{Base: p.rates()}
	for _, o := range p.Overrides {
		if o.MinPrompt > 0 && !o.window() {
			t.Tiers = append(t.Tiers, Tier{o.MinPrompt, o.rates(t.Base)})
		}
	}
	slices.SortStableFunc(t.Tiers, func(a, b Tier) int { return a.MinPrompt - b.MinPrompt })
	return t
}

// priceAt is the tariff billed at at: the base, or the cheapest time window
// active then (a window above the base is a surcharge and applies too),
// with that window's label. Context tiers keep their base prices.
func (p Pricing) priceAt(base Tariff, at time.Time) (Tariff, string) {
	var best Tariff
	label, found := "", false
	for _, o := range p.Overrides {
		if !o.window() || o.MinPrompt > 0 || !o.activeAt(at) {
			continue
		}
		t := Tariff{Base: o.rates(base.Base), Tiers: base.Tiers}
		if t.Base.Prompt <= 0 {
			continue
		}
		if !found || t.Blended(DefaultWorkload) < best.Blended(DefaultWorkload) {
			best, label, found = t, o.label(), true
		}
	}
	if !found {
		return base, ""
	}
	return best, label
}

func (p Pricing) hasWindows() bool {
	return slices.ContainsFunc(p.Overrides, func(o Override) bool { return o.window() && o.MinPrompt == 0 })
}

// Horizon is how long one run's picks stay pinned: an offer's price is its
// average over the Horizon from the run, and max_price caps at its peak.
// Presets are repointed every 2 h, so 4 h covers one missed run; a short
// Horizon prices a time-window discount by the hours it is actually used,
// not diluted over a day. A var only so tests can pin it (see TestMain).
var Horizon = 4 * time.Hour

// sampleStep is the Horizon's sampling interval.
const sampleStep = 5 * time.Minute

// ExplicitCaching reports whether clients mark prompt-cache breakpoints
// (cache_control). Without them only endpoints with implicit caching bill
// cache hits; the others bill every prompt token at the prompt rate.
var ExplicitCaching = false

// Offer is one usable endpoint priced over the Horizon from the run. Avg is
// the time-average tariff, Peak the highest rates billed (for max_price),
// List the price before the endpoint's discount. Window names the windows
// priced below the base within the Horizon. Tools, Context and MaxOutput
// are the endpoint's own support (Context, MaxOutput 0: unknown).
type Offer struct {
	Provider  string
	Endpoint  string // tag for provider.only; empty for catalog offers
	Window    string
	Avg       Tariff
	Peak      Tariff
	List      Tariff
	Tools     bool
	Context   int
	MaxOutput int // max_completion_tokens
}

// newOffer prices one endpoint (or the catalog, tag "") over the Horizon
// from `from`. Zero or negative (dynamic) prompt prices yield no offer.
func newOffer(provider, tag string, p Pricing, cached bool, from time.Time) (Offer, bool) {
	base := p.tariff()
	if base.Base.Prompt <= 0 {
		return Offer{}, false
	}
	strip := func(t Tariff) Tariff {
		if cached || ExplicitCaching {
			return t
		}
		return t.uncached()
	}
	o := Offer{Provider: provider, Endpoint: tag, List: strip(base)}
	if p.Discount > 0 && p.Discount < 1 {
		o.List = o.List.scale(1 / (1 - p.Discount))
	}
	if !p.hasWindows() {
		o.Avg, o.Peak = strip(base), strip(base)
		return o, true
	}
	n := int(Horizon / sampleStep)
	samples := make([]Tariff, n)
	var labels []string
	for i := range n {
		t, label := p.priceAt(base, from.Add(time.Duration(i)*sampleStep))
		samples[i] = strip(t)
		if label != "" && t.Blended(DefaultWorkload) < base.Blended(DefaultWorkload) && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	o.Avg, o.Peak = meanTariff(samples), peakTariff(samples)
	o.Window = strings.Join(labels, "; ")
	return o, true
}

// discounted reports whether the offer undercuts its list price under w.
func (o Offer) discounted(w Workload) bool {
	return o.Avg.Blended(w) < o.List.Blended(w)
}

// Require is what a preset needs from its pinned endpoint.
type Require struct {
	Tools      bool // endpoint accepts tools (tool_choice auto)
	MinContext int  // context tokens; the workload must fit regardless
	Capped     bool // average rates within MaxPrompt and MaxCompletion
	// MinOutput is the preset's max_tokens: a pinned endpoint with a lower
	// max_completion_tokens would be filtered out by OpenRouter.
	MinOutput int
}

// meets reports whether the offer satisfies r for requests shaped like w.
func (o Offer) meets(r Require, w Workload) bool {
	if r.Tools && !o.Tools {
		return false
	}
	if need := max(r.MinContext, int(w.Input+w.Output)); o.Context > 0 && o.Context < need {
		return false
	}
	if r.MinOutput > 0 && o.MaxOutput > 0 && o.MaxOutput < r.MinOutput {
		return false
	}
	if r.Capped {
		rt := o.Avg.For(w)
		if rt.Prompt > MaxPrompt || rt.Completion > MaxCompletion {
			return false
		}
	}
	return true
}

func (r Rates) uncached() Rates {
	r.CacheRead, r.CacheWrite = 0, 0
	return r
}

func (t Tariff) uncached() Tariff { return t.mapRates(Rates.uncached) }

func (t Tariff) mapRates(f func(Rates) Rates) Tariff {
	out := Tariff{Base: f(t.Base), Tiers: make([]Tier, len(t.Tiers))}
	for i, tr := range t.Tiers {
		out.Tiers[i] = Tier{tr.MinPrompt, f(tr.Rates)}
	}
	return out
}

// zipRates combines two tariffs of the same tier layout rate by rate.
func zipRates(a, b Tariff, f func(x, y float64) float64) Tariff {
	z := func(x, y Rates) Rates {
		return Rates{f(x.Prompt, y.Prompt), f(x.Completion, y.Completion), f(x.CacheRead, y.CacheRead),
			f(x.CacheWrite, y.CacheWrite), f(x.Reasoning, y.Reasoning)}
	}
	out := Tariff{Base: z(a.Base, b.Base), Tiers: make([]Tier, len(a.Tiers))}
	for i, tr := range a.Tiers {
		out.Tiers[i] = Tier{tr.MinPrompt, z(tr.Rates, b.Tiers[i].Rates)}
	}
	return out
}

// meanTariff averages samples rate by rate; all share one tier layout.
func meanTariff(ts []Tariff) Tariff {
	sum := ts[0]
	for _, t := range ts[1:] {
		sum = zipRates(sum, t, func(x, y float64) float64 { return x + y })
	}
	return sum.scale(1 / float64(len(ts)))
}

// peakTariff is the highest of each rate across samples.
func peakTariff(ts []Tariff) Tariff {
	peak := ts[0]
	for _, t := range ts[1:] {
		peak = zipRates(peak, t, math.Max)
	}
	return peak
}

// quantBits is the weight precision named by an endpoint quantization tag;
// 0 when unknown.
func quantBits(q string) int {
	switch strings.ToLower(q) {
	case "fp32":
		return 32
	case "bf16", "fp16":
		return 16
	case "fp8", "int8":
		return 8
	case "fp6":
		return 6
	case "fp4", "nvfp4", "mxfp4", "int4":
		return 4
	}
	return 0
}

// flexTier is the service tier served after standard traffic: cheaper, but
// queued or rejected under load. It is never pinned.
const flexTier = "flex"

// usable drops flex service-tier endpoints, then endpoints with weights
// quantized to 4 bits or fewer while another remaining endpoint serves the
// same model at higher precision (a natively 4-bit model keeps its 4-bit
// endpoints). Unknown quantization passes. Uptime is not filtered: presets
// fall back across models, not providers.
func usable(eps []Endpoint) []Endpoint {
	std := make([]Endpoint, 0, len(eps))
	top := 0
	for _, e := range eps {
		if strings.EqualFold(e.ServiceTier, flexTier) {
			continue
		}
		std = append(std, e)
		top = max(top, quantBits(e.Quantization))
	}
	out := make([]Endpoint, 0, len(std))
	for _, e := range std {
		if b := quantBits(e.Quantization); b > 0 && b <= 4 && b < top {
			continue
		}
		out = append(out, e)
	}
	return out
}
