package rank

import (
	"fmt"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

// directRow is a hand-built Darkbloom-direct discount: priced at half its
// cheapest reference (see discounts.Direct), so Δ% is -50.
func directRow(id string, price float64) discounts.Discount {
	return discounts.Discount{
		ModelID: id, Provider: discounts.DirectProvider,
		Endpoint: discounts.DirectPrefix + id, Price: price, Was: 2 * price, Pct: -50,
	}
}

// TestSelectDirectSortsByPriceAndScores: the direct table orders rows by
// blended price, cheapest first, and carries the intelligence index only
// where it knows the model and the score is positive.
func TestSelectDirectSortsByPriceAndScores(t *testing.T) {
	got := SelectDirect([]discounts.Discount{
		directRow("vendor/d", 0.8), directRow("vendor/a", 0.2),
		directRow("vendor/c", 0.6), directRow("vendor/b", 0.4),
	}, map[string]float64{"vendor/a": 55, "vendor/c": 0})
	if got.Sector != DarkbloomDirect {
		t.Errorf("sector = %s, want %s", got.Sector, DarkbloomDirect)
	}
	want := []struct {
		id     string
		scored bool
	}{
		{"vendor/a", true}, {"vendor/b", false}, {"vendor/c", false}, {"vendor/d", false},
	}
	if len(got.Rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(got.Rows), len(want))
	}
	for i, w := range want {
		r := got.Rows[i]
		if r.ModelID != w.id || r.Scored != w.scored {
			t.Errorf("Rows[%d] = %s scored=%v, want %s scored=%v", i, r.ModelID, r.Scored, w.id, w.scored)
		}
	}
	if r := got.Rows[0]; r.Score != 55 || r.Provider != discounts.DirectProvider || r.Endpoint != discounts.DirectPrefix+"vendor/a" {
		t.Errorf("first row = %+v, want vendor/a scored 55 via %s", r, discounts.DirectProvider)
	}
}

// TestSelectDirectCapsAtTen: more than 10 direct rows keep the 10 cheapest.
func TestSelectDirectCapsAtTen(t *testing.T) {
	rows := make([]discounts.Discount, 0, 12)
	for i := range 12 {
		rows = append(rows, directRow(fmt.Sprintf("vendor/m%02d", i), float64(i+1)))
	}
	got := SelectDirect(rows, nil)
	if len(got.Rows) != 10 {
		t.Fatalf("rows = %d, want 10", len(got.Rows))
	}
	if got.Rows[0].ModelID != "vendor/m00" || got.Rows[9].ModelID != "vendor/m09" {
		t.Errorf("rows run %s…%s, want the ten cheapest m00…m09", got.Rows[0].ModelID, got.Rows[9].ModelID)
	}
}
