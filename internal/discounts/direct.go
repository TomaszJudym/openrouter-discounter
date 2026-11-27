package discounts

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// Darkbloom (api.darkbloom.dev) is priced twice: its OpenRouter endpoints
// (provider DarkbloomProvider, tags "darkbloom/…") are ordinary OpenRouter
// offers billed to OpenRouter credits, and its own API is a separate,
// "direct" offer billed to Darkbloom credits and routed by cmd/dbproxy. Both
// count as discounted when they undercut every other provider (see
// Offer.was): Darkbloom lists no discounts of its own.
const (
	// DarkbloomProvider is Darkbloom's provider_name on OpenRouter endpoints.
	DarkbloomProvider = "Darkbloom"
	// DirectProvider names direct offers in the report's provider column.
	DirectProvider = "db-direct"
	// DirectPrefix prefixes a direct offer's Endpoint, the Darkbloom model id
	// after it, so a direct row can never be mistaken for an OpenRouter tag.
	DirectPrefix = "db:"
)

// IsDirect reports whether an endpoint tag names a Darkbloom-direct offer.
func IsDirect(tag string) bool { return strings.HasPrefix(tag, DirectPrefix) }

// DirectID is the Darkbloom model id of a direct endpoint tag; ok is false
// for any other tag.
func DirectID(tag string) (id string, ok bool) { return strings.CutPrefix(tag, DirectPrefix) }

// DirectModel is one Darkbloom model as an offer for an OpenRouter model:
// its own id, prices (USD per token, as OpenRouter lists them) and limits.
type DirectModel struct {
	ID        string // Darkbloom model id
	BaseID    string // OpenRouter model id it serves
	Pricing   Pricing
	Tools     bool
	Vision    bool // takes image input
	Context   int
	MaxOutput int
}

// directOffer prices one Darkbloom model over the Horizon from `from`.
// Darkbloom bills cache reads automatically (OpenRouter marks its endpoints
// supports_implicit_caching), so cache rates count.
func directOffer(m DirectModel, from time.Time) (Offer, bool) {
	o, ok := newOffer(DirectProvider, DirectPrefix+m.ID, m.Pricing, true, from)
	if !ok {
		return Offer{}, false
	}
	o.Direct, o.Tools, o.Blind, o.Context, o.MaxOutput = true, m.Tools, !m.Vision, m.Context, m.MaxOutput
	return o, true
}

// Direct builds the Darkbloom-direct side: one row per OpenRouter model
// that Darkbloom serves, holding only direct offers, priced against that
// model's other OpenRouter providers in priced (see Priced). Several
// Darkbloom builds of one model are offers of one row. discs are the rows
// whose cheapest direct offer undercuts every other provider, picked for
// Require{} under DefaultWorkload; pricedOut every row, discounted or not.
func Direct(models []DirectModel, priced []Discount, at time.Time) (discs, pricedOut []Discount) {
	refs := make(map[string][]Offer, len(priced))
	for _, d := range priced {
		refs[d.ModelID] = d.Offers
	}
	rows := make(map[string]*Discount)
	for _, m := range models {
		o, ok := directOffer(m, at)
		if !ok {
			continue
		}
		d := rows[m.BaseID]
		if d == nil {
			// non-nil Refs: never fall back to the row's own (direct) offers
			d = &Discount{ModelID: m.BaseID, Refs: append([]Offer{}, refs[m.BaseID]...)}
			rows[m.BaseID] = d
		}
		d.Offers = append(d.Offers, o)
		d.Serves = append(d.Serves, o.Endpoint)
	}
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		d := *rows[id]
		if p, ok := d.ForAny(Require{}, DefaultWorkload); ok {
			pricedOut = append(pricedOut, p)
		}
		if p, ok := d.For(Require{}, DefaultWorkload); ok {
			discs = append(discs, p)
		}
	}
	return discs, pricedOut
}
