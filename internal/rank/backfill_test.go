package rank

import (
	"context"
	"fmt"
	"math"
	"testing"
)

// pairs builds primary = 2·fallback − 10 over n shared ids.
func pairs(n int) (primary, fallback map[string]float64) {
	primary, fallback = map[string]float64{}, map[string]float64{}
	for i := range n {
		id := fmt.Sprintf("v/m%d", i)
		x := 20 + float64(i)*3
		fallback[id] = x
		primary[id] = 2*x - 10
	}
	return primary, fallback
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestBackfillCalibratesMissingModels(t *testing.T) {
	primary, fallback := pairs(minFit)
	fallback["v/new"] = 30 // 2·30−10 = 50
	fallback["v/top"] = 90 // 170, clamped to 100
	fallback["v/low"] = 1  // −8, clamped to 0
	b := Backfill{
		Provider: fakeProvider{scores: map[Sector]map[string]float64{Code: primary}},
		Sectors:  []Sector{Code},
		Fallback: fallback,
	}
	got := b.Scores(context.Background())[Code].ByModel
	for id, want := range map[string]float64{"v/new": 50, "v/top": 100, "v/low": 0} {
		if !near(got[id], want) {
			t.Errorf("%s = %v, want %v", id, got[id], want)
		}
	}
}

func TestBackfillKeepsPrimaryScore(t *testing.T) {
	primary, fallback := pairs(minFit)
	primary["v/m0"] = 99
	if got := backfill(primary, fallback)["v/m0"]; got != 99 {
		t.Errorf("v/m0 = %v, want primary 99", got)
	}
}

func TestBackfillTooFewPairsUsesRawValues(t *testing.T) {
	primary, fallback := pairs(minFit - 1)
	fallback["v/new"] = 42
	got := backfill(primary, fallback)
	if got["v/new"] != 42 {
		t.Errorf("v/new = %v, want raw 42", got["v/new"])
	}
}

func TestBackfillLeavesFailedSectorPriceOnly(t *testing.T) {
	b := Backfill{
		Provider: fakeProvider{errs: map[Sector]string{Code: "boom"}},
		Sectors:  []Sector{Code, CodeFree},
		Fallback: map[string]float64{"v/a": 50},
	}
	got := b.Scores(context.Background())
	if sc := got[Code]; sc.Err != "boom" || sc.ByModel != nil {
		t.Errorf("Code = %+v, want untouched error", sc)
	}
	if _, ok := got[CodeFree]; ok {
		t.Error("CodeFree added though the primary did not cover it")
	}
}
