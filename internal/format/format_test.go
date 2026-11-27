package format

import (
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
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
				{ModelID: "deepseek/deepseek-chat", Provider: "GMICloud", Price: 0.14, Was: 0.28, Pct: -50, Score: 68.2, Scored: true},
				{ModelID: "qwen/qwen3-235b-a22b", Price: 0.09, Was: 0.18, Pct: -50},
			},
		}, {
			Sector: rank.Finance,
			Source: "Artificial Analysis",
			Err:    "429 rate limit, retries exhausted",
			Rows: []rank.Row{
				{ModelID: "google/gemini-2.5-pro", Price: 0.65, Was: 1.30, Pct: -50},
				{ModelID: "openai/gpt-5.2", Price: 0.80, Was: 0.88, Pct: -9},
			},
		}},
	}
}

func TestMessagesLayoutAndAlignment(t *testing.T) {
	msgs := Messages(scoredResult(), noon())
	if len(msgs) != 1 {
		t.Fatalf("Messages() produced %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	// 07:00 UTC renders as 16:00 JST the same day
	if !strings.Contains(m, "🪙 OpenRouter discounts — 17 Sep 2026 16:00 JST") {
		t.Errorf("header missing or wrong timestamp:\n%s", m)
	}
	for _, want := range []string{
		"MATH — top 10 discounted (Epoch OTIS Mock AIME)",
		"FINANCE — top 10 discounted (AA Finance &amp; Accounting)",
		"⚠️ scored only by price — Artificial Analysis error: 429 rate limit, retries exhausted",
		"prices: 4h-avg blended $/Mtok, 30k in (80% cached) + 1.5k out",
		"#  model                    provider        was→now      Δ% score",
		"1  deepseek/deepseek-chat   GMICloud      0.280→0.140   -50  68.2",
		"2  qwen/qwen3-235b-a22b     any           0.180→0.090   -50   n/a",
		"1  google/gemini-2.5-pro    any            1.30→0.65    -50",
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
	msgs := Messages(scoredResult(), noon())
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
	m := Messages(res, noon())[0]
	if strings.Contains(m, "evil/<b>") {
		t.Errorf("model name not escaped correctly:\n%s", m)
	}
	if !strings.Contains(m, "evil/&lt;b&gt;&amp;amp;&lt;/b&gt;") {
		t.Errorf("escaped name missing:\n%s", m)
	}
}

// A model/provider name containing & < > must keep the same visible column
// positions as an ASCII name: the cell is padded to its width on the raw
// truncated text and escaped only afterwards, so entities like &amp; (1
// visible rune, 5 bytes) never shift the row's columns left.
func TestMessagesEscapedCellsStayAligned(t *testing.T) {
	ascii := rank.Result{Sectors: []rank.SectorResult{sectorRows("vendor/plain-model-1", "prov-a")}}
	esc := rank.Result{Sectors: []rank.SectorResult{sectorRows("vendor/plain-model-2", "prov-b")}}
	// same visible rune width as "vendor/plain-model-1" but with escapable
	// runes, so escaping must not shrink the row
	esc.Sectors[0].Rows[0].ModelID = "vendor/p<i>&amp;>-model"
	esc.Sectors[0].Rows[0].Provider = "prov<&>b"

	asciiLines := dataLines(Messages(ascii, noon())[0])
	escLines := dataLines(Messages(esc, noon())[0])
	if len(asciiLines) != 1 || len(escLines) != 1 {
		t.Fatalf("expected 1 data row per message, got %d and %d", len(asciiLines), len(escLines))
	}
	if runeLen(visible(escLines[0])) != runeLen(visible(asciiLines[0])) {
		t.Errorf("escaped row visible width %d != ascii row width %d:\n%q\n%q",
			runeLen(visible(escLines[0])), runeLen(visible(asciiLines[0])), asciiLines[0], escLines[0])
	}
	// visible rune offsets of the price arrow and the pct column match, and
	// no raw escapable rune survives in the rendered line
	visEsc, visAscii := visible(escLines[0]), visible(asciiLines[0])
	for _, sub := range []string{"\u2192", "-50"} {
		if got, want := runeIndex(visEsc, sub), runeIndex(visAscii, sub); got != want {
			t.Errorf("column %q shifted: escaped at %d, ascii at %d", sub, got, want)
		}
	}
	for _, raw := range []string{"<", ">"} {
		if strings.Contains(escLines[0], raw) {
			t.Errorf("escaped row contains raw %q: %q", raw, escLines[0])
		}
	}
	if !strings.Contains(escLines[0], "&amp;") {
		t.Errorf("escaped row missing entity escape: %q", escLines[0])
	}
}

func sectorRows(model, provider string) rank.SectorResult {
	return rank.SectorResult{
		Sector: rank.Math,
		Rows:   []rank.Row{{ModelID: model, Provider: provider, Price: 0.65, Was: 1.30, Pct: -50, Score: 68.2, Scored: true}},
	}
}

func dataLines(m string) []string {
	var out []string
	for l := range strings.SplitSeq(m, "\n") {
		if strings.HasPrefix(l, "1 ") {
			out = append(out, l)
		}
	}
	return out
}

func TestMessagesModelTruncation(t *testing.T) {
	long := "vendor/" + strings.Repeat("m", 40)
	res := rank.Result{Sectors: []rank.SectorResult{{
		Sector: rank.Math,
		Rows:   []rank.Row{{ModelID: long, Price: 1, Was: 2, Pct: -50, Score: 1, Scored: true}},
	}}}
	m := Messages(res, noon())[0]
	if !strings.Contains(m, "vendor/"+strings.Repeat("m", 16)+"…") {
		t.Errorf("truncated model name missing:\n%s", m)
	}
	for l := range strings.SplitSeq(m, "\n") {
		if strings.Contains(l, long) {
			t.Errorf("untruncated model name present: %q", l)
		}
	}
}

func TestMessagesNoticeEscaped(t *testing.T) {
	res := scoredResult()
	res.Sectors[1].Source = "A&B"
	res.Sectors[1].Err = "status 403: <forbidden> & key leaked"
	msgs := Messages(res, noon())
	if !strings.Contains(msgs[0], noticePrefix+"A&amp;B error: status 403: &lt;forbidden&gt; &amp; key leaked") {
		t.Errorf("notice missing or unescaped:\n%s", msgs[0])
	}
}

func TestMessagesNoticeOnlyOnFailedSector(t *testing.T) {
	m := Messages(scoredResult(), noon())[0]
	if n := strings.Count(m, noticePrefix); n != 1 {
		t.Errorf("notice count = %d, want 1 (Finance only):\n%s", n, m)
	}
}

func TestMessagesSplitAboveLimit(t *testing.T) {
	failed := func(s rank.Sector) rank.SectorResult {
		return rank.SectorResult{Sector: s, Source: "Epoch AI", Err: "Epoch down", Rows: manyRows()}
	}
	res := rank.Result{
		Sectors: []rank.SectorResult{
			failed(rank.Math), failed(rank.LCR), failed(rank.Finance),
			failed(rank.Math), failed(rank.LCR), failed(rank.Finance),
		},
	}
	msgs := Messages(res, noon())
	if len(msgs) < 2 {
		t.Fatalf("Messages() produced %d messages, want >= 2 (split above limit)", len(msgs))
	}
	for i, m := range msgs {
		if len(m) > 4096 {
			t.Errorf("message %d exceeds 4096 chars: %d", i, len(m))
		}
		if !strings.Contains(m, noticePrefix+"Epoch AI error: Epoch down") {
			t.Errorf("message %d missing price-only notice:\n%s", i, m)
		}
	}
}

// The split threshold counts runes: sector blocks full of multibyte runes
// whose combined byte count exceeds the limit but whose rune count stays
// below it must stay in one message, never split early.
func TestMessagesSplitCountsRunes(t *testing.T) {
	sector := func(s rank.Sector) rank.SectorResult {
		rows := make([]rank.Row, 10)
		for i := range rows {
			rows[i] = rank.Row{
				ModelID: "vendor/model-" + strings.Repeat("水", 13) + string(rune('a'+i)),
				Price:   float64(i), Was: float64(i) * 2, Pct: -50,
			}
		}
		return rank.SectorResult{Sector: s, Rows: rows}
	}
	res := rank.Result{Sectors: []rank.SectorResult{
		sector(rank.Math), sector(rank.LCR), sector(rank.Finance), sector(rank.Code),
		sector(rank.Math),
	}}
	msgs := Messages(res, noon())
	if len(msgs) != 1 {
		t.Fatalf("Messages() produced %d messages, want 1 (split counts runes, not bytes)", len(msgs))
	}
	m := msgs[0]
	// the byte count exceeds the limit: byte-based counting would have split
	if len(m) <= 4000 {
		t.Fatalf("test content below 4000 bytes (%d); it no longer discriminates byte vs rune counting", len(m))
	}
	if n := utf8.RuneCountInString(m); n > 4000 {
		t.Errorf("message exceeds 4000 runes (%d)", n)
	}
	if strings.Contains(strings.Join(msgs, "\n"), "\n\n\n") && len(msgs) > 1 {
		t.Error("unexpected extra split")
	}
}

func TestNoDiscountsMessage(t *testing.T) {
	m := NoDiscounts(noon())
	if !strings.Contains(m, "no discounts today.") {
		t.Errorf("NoDiscounts() = %q, want short no-discount message", m)
	}
	if strings.Count(m, "\n\n") > 1 {
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

// directRows mirrors rank.SelectDirect output: one row per OpenRouter model
// Darkbloom serves, provider "db-direct", endpoint tagged "db:<darkbloom id>"
// (see discounts.DirectPrefix), was = cheapest other provider.
func directRows() []rank.Row {
	return []rank.Row{
		{ModelID: "vendor/model-a", Provider: discounts.DirectProvider, Endpoint: discounts.DirectPrefix + "model-a", Price: 0.14, Was: 0.28, Pct: -50, Score: 71.5, Scored: true},
		{ModelID: "evil/<b>&x", Provider: discounts.DirectProvider, Endpoint: discounts.DirectPrefix + "evil", Price: 0.09, Was: 0.18, Pct: -50},
	}
}

func directSector(rows []rank.Row) rank.SectorResult {
	return rank.SectorResult{Sector: rank.DarkbloomDirect, Rows: rows}
}

func TestMessagesDarkbloomDirectTable(t *testing.T) {
	res := rank.Result{Sectors: []rank.SectorResult{directSector(directRows())}}
	msgs := Messages(res, noon())
	if len(msgs) != 1 {
		t.Fatalf("Messages() produced %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	lines := strings.Split(m, "\n")
	want := []string{
		"DARKBLOOM — direct, cheapest 10 (via local dbproxy)",
		"#  model                    provider        was→now      Δ% score",
		"1  vendor/model-a           db-direct     0.280→0.140   -50  71.5",
		// the model cell is padded to 24 raw runes and escaped after, so the
		// escaped entities do not shrink the column and the row stays aligned
		"2  evil/&lt;b&gt;&amp;x               db-direct     0.180→0.090   -50   n/a",
	}
	for _, w := range want {
		if !slices.Contains(lines, w) {
			t.Errorf("direct table missing line %q:\n%s", w, m)
		}
	}
	// the escaped model name must not appear raw
	if strings.Contains(m, "evil/<b>") {
		t.Errorf("direct model name not escaped:\n%s", m)
	}
	// the score column is right-aligned: "71.5" and "n/a" share the same
	// right edge and both data lines are the same width
	var data []string
	for _, l := range lines {
		if strings.HasPrefix(l, "1 ") || strings.HasPrefix(l, "2 ") {
			data = append(data, l)
		}
	}
	if len(data) != 2 {
		t.Fatalf("expected 2 direct data rows, got %d:\n%s", len(data), m)
	}
	if runeLen(visible(data[0])) != runeLen(visible(data[1])) {
		t.Errorf("direct rows differ in visible width: %d vs %d",
			runeLen(visible(data[0])), runeLen(visible(data[1])))
	}
	// escaped entities sit inside the padded cell, so the visible widths match
	// even though the raw line lengths differ
	if runeLen(data[0]) == runeLen(data[1]) {
		t.Errorf("expected escaped row to be longer in raw runes: %d vs %d", runeLen(data[0]), runeLen(data[1]))
	}
	if !strings.HasSuffix(data[0], "71.5") || !strings.HasSuffix(data[1], "n/a") {
		t.Errorf("score column not right-aligned: %q / %q", data[0], data[1])
	}
	for _, row := range data {
		if n := runeLen(row); n > 80 {
			t.Errorf("direct row exceeds 80 chars (%d): %q", n, row)
		}
	}
}

func TestMessagesSplitIncludesDarkbloomDirect(t *testing.T) {
	failed := func(s rank.Sector) rank.SectorResult {
		return rank.SectorResult{Sector: s, Source: "Epoch AI", Err: "Epoch down", Rows: manyRows()}
	}
	res := rank.Result{
		Sectors: []rank.SectorResult{
			failed(rank.Math), failed(rank.LCR), failed(rank.Finance),
			failed(rank.Math), failed(rank.LCR), failed(rank.Finance),
			directSector(manyDirectRows()),
		},
	}
	msgs := Messages(res, noon())
	if len(msgs) < 2 {
		t.Fatalf("Messages() produced %d messages, want >= 2 (split above limit):\n%v", len(msgs), msgs)
	}
	last := msgs[len(msgs)-1]
	if !strings.Contains(last, "DARKBLOOM — direct, cheapest 10 (via local dbproxy)") {
		t.Errorf("last message missing the direct sector label:\n%s", last)
	}
	if !strings.Contains(last, "db-direct") || !strings.Contains(last, "vendor/direct-model-a") {
		t.Errorf("last message missing the direct table rows:\n%s", last)
	}
	// the direct sector is rendered exactly once across the split
	if n := strings.Count(strings.Join(msgs, "\n"), "DARKBLOOM —"); n != 1 {
		t.Errorf("DARKBLOOM label appears %d times across %d messages, want 1", n, len(msgs))
	}
	for i, m := range msgs {
		if len(m) > 4096 {
			t.Errorf("message %d exceeds 4096 chars: %d", i, len(m))
		}
		if !strings.Contains(m, "🪙 OpenRouter discounts — 17 Sep 2026 16:00 JST") {
			t.Errorf("message %d missing the repeated header:\n%s", i, m)
		}
	}
}

func TestMessagesDirectOnlyIsFullReport(t *testing.T) {
	// a day whose only priced rows are Darkbloom-direct must still render a
	// full sector message, never the short no-discount text
	res := rank.Result{Sectors: []rank.SectorResult{directSector(directRows())}}
	m := Messages(res, noon())[0]
	if strings.Contains(m, "no discounts today") {
		t.Errorf("direct-only report rendered as no-discounts message:\n%s", m)
	}
	if !strings.Contains(m, "DARKBLOOM — direct, cheapest 10 (via local dbproxy)") {
		t.Errorf("direct-only report missing the direct table:\n%s", m)
	}
}

func TestNoDiscountsOmitsDarkbloomDirect(t *testing.T) {
	m := NoDiscounts(noon())
	for _, forbidden := range []string{"DARKBLOOM", "<pre>", "db-direct", "db:"} {
		if strings.Contains(m, forbidden) {
			t.Errorf("no-discounts message contains %q:\n%s", forbidden, m)
		}
	}
}

func runeLen(s string) int { return len([]rune(s)) }

// visible decodes the HTML entities used by the escaper; inside Telegram's
// <pre> each entity renders as one character, so this is the column width a
// reader sees.
var visibleReplacer = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")

func visible(s string) string { return visibleReplacer.Replace(s) }

func manyDirectRows() []rank.Row {
	rows := make([]rank.Row, 10)
	for i := range rows {
		rows[i] = rank.Row{
			ModelID:  "vendor/direct-model-" + string(rune('a'+i)),
			Provider: discounts.DirectProvider,
			Endpoint: discounts.DirectPrefix + "direct-model-" + string(rune('a'+i)),
			Price:    float64(i), Was: float64(i) * 2, Pct: float64(-50 + i),
		}
	}
	return rows
}

func TestCodeFreeTableLayout(t *testing.T) {
	res := rank.Result{
		Sectors: []rank.SectorResult{{
			Sector: rank.CodeFree,
			Rows: []rank.Row{
				{ModelID: "google/gemini-2.5-flash:free", Score: 88.4, Scored: true},
				{ModelID: "meta/llama-4-scout:free", Score: 0, Scored: false},
			},
		}},
	}
	msgs := Messages(res, noon())
	if len(msgs) != 1 {
		t.Fatalf("Messages() produced %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	for _, want := range []string{
		"CODE-FREE — top 10 free coding models (Epoch WebDev Arena)",
		"google/gemini-2.5-flash…",
		"88.4",
		"meta/llama-4-scout:free",
		"n/a",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message missing %q:\n%s", want, m)
		}
	}
	// no price columns
	if strings.Contains(m, "→") {
		t.Error("code-free table contains price arrow")
	}
}

func TestMessagesStrongestDiscountSector(t *testing.T) {
	res := rank.Result{
		Sectors: []rank.SectorResult{{
			Sector: rank.StrongestDiscountHalflight,
			Rows: []rank.Row{
				{ModelID: "anthropic/claude-opus-4", Price: 5.00, Was: 10.00, Pct: -50, Score: 92.3, Scored: true},
				{ModelID: "google/gemini-3.0-pro", Price: 1.25, Was: 2.50, Pct: -50, Score: 88.1, Scored: true},
				{ModelID: "deepseek/deepseek-r2", Price: 0.27, Was: 0.30, Pct: -10, Score: 84.7, Scored: true},
			},
		}},
	}
	msgs := Messages(res, noon())
	if len(msgs) != 1 {
		t.Fatalf("Messages() produced %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if !strings.Contains(m, "STRONGEST-DISCOUNT-HALFLIGHT — top 3 strongest-thinking discounted") {
		t.Errorf("strongest-discount label missing:\n%s", m)
	}
	for _, want := range []string{
		"anthropic/claude-opus-4",
		"google/gemini-3.0-pro",
		"deepseek/deepseek-r2",
		"92.3",
		"88.1",
		"84.7",
		"10.00\u21925.00",
		"2.50\u21921.25",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message missing %q:\n%s", want, m)
		}
	}
}

func runeIndex(s, sub string) int {
	before, _, ok := strings.Cut(s, sub)
	if !ok {
		return -1
	}
	return len([]rune(before))
}
