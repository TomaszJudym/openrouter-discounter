package rank

import (
	"context"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

func discs() []discounts.Discount {
	return []discounts.Discount{
		{ModelID: "a/model-a", Price: 1.0, Was: 2.0, Pct: -50},
		{ModelID: "b/model-b", Price: 2.0, Was: 2.2, Pct: -9},
		{ModelID: "c/model-c", Price: 3.0, Was: 6.0, Pct: -50},
	}
}

type fakeProvider struct {
	scores map[Sector]map[string]float64
	err    error
}

func (f fakeProvider) Scores(context.Context) (map[Sector]map[string]float64, error) {
	return f.scores, f.err
}

func TestRankScoredOrderingAndUnscoredLast(t *testing.T) {
	p := fakeProvider{scores: map[Sector]map[string]float64{
		Math: {"a/model-a": 61.2, "b/model-b": 68.2},
	}}
	res := Rank(t.Context(), discs(), p)
	if res.GlobalErr != "" {
		t.Fatalf("GlobalErr = %q, want empty", res.GlobalErr)
	}
	math := res.Sectors[0]
	if math.Sector != Math || len(math.Rows) != 3 || math.Err != "" {
		t.Fatalf("Math sector = %+v, want 3 rows, no error", math)
	}
	// composite = 0.5×score + 0.5×(−Δ%): a = 55.6, b = 38.6; unscored c last
	want := []struct {
		id     string
		score  float64
		scored bool
	}{{"a/model-a", 61.2, true}, {"b/model-b", 68.2, true}, {"c/model-c", 0, false}}
	for i, w := range want {
		if got := math.Rows[i]; got.ModelID != w.id || got.Score != w.score || got.Scored != w.scored {
			t.Errorf("Rows[%d] = %v, want %v", i, got, w)
		}
	}
}

func TestRankPriceOnlyFallbackOnProviderError(t *testing.T) {
	p := fakeProvider{err: errText("429 rate limit, retries exhausted")}
	res := Rank(t.Context(), discs(), p)
	if res.GlobalErr != "429 rate limit, retries exhausted" {
		t.Fatalf("GlobalErr = %q, want the provider error", res.GlobalErr)
	}
	for _, sr := range res.Sectors {
		if sr.Err != "" {
			t.Errorf("%s Err = %q, want empty (global failure)", sr.Sector, sr.Err)
		}
		if len(sr.Rows) != 3 {
			t.Fatalf("%s rows = %d, want 3", sr.Sector, len(sr.Rows))
		}
		if sr.Rows[0].Scored || sr.Rows[0].ModelID != "a/model-a" {
			t.Errorf("%s first row = %+v, want a/model-a by Δ%% ordering", sr.Sector, sr.Rows[0])
		}
		if sr.Rows[1].ModelID != "c/model-c" || sr.Rows[2].ModelID != "b/model-b" {
			t.Errorf("%s order = %s,%s, want c then b (Δ%% ascending)", sr.Sector, sr.Rows[1].ModelID, sr.Rows[2].ModelID)
		}
	}
}

func TestRankPerSectorNoScoresNotice(t *testing.T) {
	p := fakeProvider{scores: map[Sector]map[string]float64{
		Math: {"a/model-a": 60},
	}}
	res := Rank(t.Context(), discs(), p)
	if res.GlobalErr != "" {
		t.Fatalf("GlobalErr = %q, want empty", res.GlobalErr)
	}
	for _, sr := range res.Sectors {
		if sr.Sector == Math {
			if sr.Err != "" {
				t.Errorf("Math Err = %q, want empty", sr.Err)
			}
			continue
		}
		if sr.Err != noScoresMessage {
			t.Errorf("%s Err = %q, want %q", sr.Sector, sr.Err, noScoresMessage)
		}
	}
}

func TestRankTopTenCap(t *testing.T) {
	many := make([]discounts.Discount, 12)
	for i := range many {
		many[i] = discounts.Discount{ModelID: string(rune('a'+i)) + "/m", Price: 1, Was: 2, Pct: float64(-i - 1)}
	}
	res := Rank(t.Context(), many, fakeProvider{})
	for _, sr := range res.Sectors {
		if len(sr.Rows) != 10 {
			t.Errorf("%s rows = %d, want 10", sr.Sector, len(sr.Rows))
		}
	}
}

type errText string

func (e errText) Error() string { return string(e) }
