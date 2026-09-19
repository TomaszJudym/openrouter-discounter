package format

import (
	"strings"
	"testing"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

func noon() time.Time {
	day, _ := time.Parse(time.DateTime, "2026-09-17 07:00:00")
	return day // UTC 07:00 = JST 16:00
}

func scoredResult() rank.Result {
	return rank.Result{
		Sectors: []rank.SectorResult{{
			Sector: rank.Math,
			Rows: []rank.Row{
				{ModelID: "deepseek/deepseek-chat", Price: 0.14, Was: 0.28, Pct: -50, Score: 68.2, Scored: true},
				{ModelID: "qwen/qwen3-235b-a22b", Price: 0.09, Was: 0.18, Pct: -50},
			},
		}, {
			Sector: rank.Finance,
			Err:    "429 rate limit, retries exhausted",
			Rows: []rank.Row{
				{ModelID: "google/gemini-2.5-pro", Price: 0.65, Was: 1.30, Pct: -50},
				{ModelID: "openai/gpt-5.2", Price: 0.80, Was: 0.88, Pct: -9},
			},
		}},
	}
}

func TestMessagesLayoutAndAlignment(t *testing.T) {
	msgs := Messages(scoredResult(), []string{"openai/gpt-oss-120b:free"}, noon())
	if len(msgs) != 1 {
		t.Fatalf("Messages() produced %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	// 07:00 UTC renders as 16:00 JST the same day
	if !strings.Contains(m, "🪙 OpenRouter discounts — 17 Sep 2026 16:00 JST") {
		t.Errorf("header missing or wrong timestamp:\n%s", m)
	}
	for _, want := range []string{
		"MATH — top 10 discounted (AA Math Index)",
		"FINANCE — top 10 discounted (τ³-Banking)",
		noticePrefix + "429 rate limit, retries exhausted",
		"1  deepseek/deepseek-chat      0.28→0.14  -50  68.2",
		"2  qwen/qwen3-235b-a22b        0.18→0.09  -50   n/a",
		"Free tier:",
		":free models today — 1",
		"openai/gpt-oss-120b:free",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message missing %q:\n%s", want, m)
		}
	}
	for line := range strings.SplitSeq(m, "\n") {
		// the 80-char budget applies to table lines; the notice prefix is
		// fixed wording and can exceed it (spec example is 86 chars)
		if !strings.HasPrefix(line, "⚠️") {
			if n := len([]rune(line)); n > 80 {
				t.Errorf("line exceeds 80 chars (%d): %q", n, line)
			}
		}
	}
}

func TestMessagesColumnAlignment(t *testing.T) {
	msgs := Messages(scoredResult(), nil, noon())
	lines := strings.Split(msgs[0], "\n")
	var data []string
	for _, l := range lines {
		if strings.HasPrefix(l, "1 ") || strings.HasPrefix(l, "2 ") {
			data = append(data, l)
		}
	}
	if len(data) < 4 {
		t.Fatalf("expected 4 data rows, got %d:\n%s", len(data), msgs[0])
	}
	// the was→now pair must start at the same rune offset in every row
	col := runeIndex(data[0], "→")
	for _, row := range data {
		if got := runeIndex(row, "→"); got != col {
			t.Errorf("price column misaligned: %q arrow at %d, want %d", row, got, col)
		}
	}
}

func TestMessagesEscapesHTML(t *testing.T) {
	res := rank.Result{Sectors: []rank.SectorResult{{
		Sector: rank.Math,
		Rows: []rank.Row{
			{ModelID: "evil/<b>&amp;</b>", Price: 1, Was: 2, Pct: -50, Score: 1, Scored: true},
		},
	}}}
	m := Messages(res, nil, noon())[0]
	if strings.Contains(m, "evil/<b>") {
		t.Errorf("model name not escaped correctly:\n%s", m)
	}
	if !strings.Contains(m, "evil/&lt;b&gt;&amp;amp;&lt;/b&gt;") {
		t.Errorf("escaped name missing:\n%s", m)
	}
}

func TestMessagesModelTruncation(t *testing.T) {
	long := "vendor/" + strings.Repeat("m", 40)
	res := rank.Result{Sectors: []rank.SectorResult{{
		Sector: rank.Math,
		Rows:   []rank.Row{{ModelID: long, Price: 1, Was: 2, Pct: -50, Score: 1, Scored: true}},
	}}}
	m := Messages(res, nil, noon())[0]
	if !strings.Contains(m, "vendor/"+strings.Repeat("m", 16)+"…") {
		t.Errorf("truncated model name missing:\n%s", m)
	}
	for l := range strings.SplitSeq(m, "\n") {
		if strings.Contains(l, long) {
			t.Errorf("untruncated model name present: %q", l)
		}
	}
}

func TestMessagesGlobalErrorNotice(t *testing.T) {
	res := scoredResult()
	res.GlobalErr = "status 403: <forbidden> & key leaked"
	msgs := Messages(res, nil, noon())
	if !strings.Contains(msgs[0], noticePrefix+"status 403: &lt;forbidden&gt; &amp; key leaked") {
		t.Errorf("global notice missing or unescaped:\n%s", msgs[0])
	}
}

func TestMessagesSplitAboveLimit(t *testing.T) {
	res := rank.Result{
		GlobalErr: "AA down",
		Sectors: []rank.SectorResult{
			{Sector: rank.Math, Rows: manyRows()},
			{Sector: rank.LCR, Rows: manyRows()},
			{Sector: rank.Finance, Rows: manyRows()},
		},
	}
	free := make([]string, 80)
	for i := range free {
		free[i] = "vendor/model-" + strings.Repeat("f", 12) + "-:free"
	}
	msgs := Messages(res, free, noon())
	if len(msgs) < 2 {
		t.Fatalf("Messages() produced %d messages, want >= 2 (split above limit)", len(msgs))
	}
	for i, m := range msgs {
		if len(m) > 4096 {
			t.Errorf("message %d exceeds 4096 chars: %d", i, len(m))
		}
		if !strings.Contains(m, noticePrefix+"AA down") {
			t.Errorf("message %d missing price-only notice:\n%s", i, m)
		}
	}
	// the free-tier block must survive the split
	found := false
	for _, m := range msgs {
		if strings.Contains(m, ":free models today — 80") {
			found = true
		}
	}
	if !found {
		t.Error("free-tier block lost in split")
	}
}

func TestNoDiscountsMessage(t *testing.T) {
	m := NoDiscounts([]string{"openai/gpt-oss-120b:free"}, noon())
	if !strings.Contains(m, "no discounts today.") {
		t.Errorf("NoDiscounts() = %q, want short no-discount message", m)
	}
	if strings.Contains(m, "<pre>") && !strings.Contains(m, ":free models today — 1") {
		t.Errorf("free block malformed:\n%s", m)
	}
	if strings.Count(m, "\n\n") > 2 {
		t.Errorf("message not short:\n%s", m)
	}
}

func manyRows() []rank.Row {
	rows := make([]rank.Row, 10)
	for i := range rows {
		rows[i] = rank.Row{
			ModelID: "vendor/model-with-a-long-name-" + string(rune('a'+i)),
			Price:   float64(i), Was: float64(i) * 2, Pct: float64(-50 + i),
		}
	}
	return rows
}

func runeIndex(s, sub string) int {
	before, _, ok := strings.Cut(s, sub)
	if !ok {
		return -1
	}
	return len([]rune(before))
}
