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
	res := Rank(t.Context(), discs(), nil, p)
	if res.GlobalErr != "" {
		t.Fatalf("GlobalErr = %q, want empty", res.GlobalErr)
	}
	math := res.Sectors[0]
	if math.Sector != Math || len(math.Rows) != 3 || math.Err != "" {
		t.Fatalf("Math sector = %+v, want 3 rows, no error", math)
	}
	// composite = 0.65×score + 0.35×(−Δ%): a = 57.28, b = 47.48; unscored c last
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
	res := Rank(t.Context(), discs(), nil, p)
	if res.GlobalErr != "429 rate limit, retries exhausted" {
		t.Fatalf("GlobalErr = %q, want the provider error", res.GlobalErr)
	}
	for _, sr := range res.Sectors {
		if sr.Sector == CodeFree {
			continue
		}
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
	res := Rank(t.Context(), discs(), nil, p)
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
	res := Rank(t.Context(), many, nil, fakeProvider{})
	for _, sr := range res.Sectors {
		if sr.Sector == CodeFree {
			continue
		}
		if len(sr.Rows) != 10 {
			t.Errorf("%s rows = %d, want 10", sr.Sector, len(sr.Rows))
		}
	}
}

func TestCodeFreeRankedByScore(t *testing.T) {
	free := []string{
		"meta/llama-4-scout:free",
		"google/gemini-2.5-flash:free",
		"openai/gpt-oss-120b:free",
	}
	p := fakeProvider{scores: map[Sector]map[string]float64{
		CodeFree: {
			"google/gemini-2.5-flash:free": 88.4,
			"openai/gpt-oss-120b:free":     72.1,
		},
	}}
	res := Rank(t.Context(), nil, free, p)
	cf := res.Sectors[4]
	if cf.Sector != CodeFree {
		t.Fatalf("sector = %s, want code-free", cf.Sector)
	}
	if len(cf.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(cf.Rows))
	}
	if cf.Err != "" {
		t.Errorf("Err = %q, want empty", cf.Err)
	}
	// scored first by score desc: gemini (88.4), gpt-oss (72.1), then unscored llama
	want := []struct {
		id     string
		scored bool
	}{
		{"google/gemini-2.5-flash:free", true},
		{"openai/gpt-oss-120b:free", true},
		{"meta/llama-4-scout:free", false},
	}
	for i, w := range want {
		if got := cf.Rows[i]; got.ModelID != w.id || got.Scored != w.scored {
			t.Errorf("Rows[%d] = %+v, want id=%s scored=%v", i, got, w.id, w.scored)
		}
	}
}

func TestCodeFreeNoScoresFallback(t *testing.T) {
	free := []string{"z/model-z:free", "a/model-a:free"}
	res := Rank(t.Context(), nil, free, fakeProvider{})
	cf := res.Sectors[4]
	if cf.Err != noScoresMessage {
		t.Errorf("Err = %q, want %q", cf.Err, noScoresMessage)
	}
	if len(cf.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(cf.Rows))
	}
	// alphabetical
	if cf.Rows[0].ModelID != "a/model-a:free" || cf.Rows[1].ModelID != "z/model-z:free" {
		t.Errorf("order = %s, %s, want alphabetical", cf.Rows[0].ModelID, cf.Rows[1].ModelID)
	}
}

func TestCodeFreeTopTenCap(t *testing.T) {
	free := make([]string, 12)
	for i := range free {
		free[i] = string(rune('a'+i)) + "/m:free"
	}
	res := Rank(t.Context(), nil, free, fakeProvider{})
	cf := res.Sectors[4]
	if len(cf.Rows) != 10 {
		t.Errorf("code-free rows = %d, want 10", len(cf.Rows))
	}
}

func TestCodeFreeGlobalError(t *testing.T) {
	p := fakeProvider{err: errText("AA down")}
	res := Rank(t.Context(), nil, []string{"a/b:free"}, p)
	cf := res.Sectors[4]
	if cf.Err != "" {
		t.Errorf("Err = %q, want empty (global error)", cf.Err)
	}
	if res.GlobalErr != "AA down" {
		t.Errorf("GlobalErr = %q, want AA down", res.GlobalErr)
	}
}

type errText string

func (e errText) Error() string { return string(e) }
