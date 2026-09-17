// Package format renders the Telegram HTML message(s): a header, per-sector
// top-10 tables, and the free-tier block. Any message above the limit is
// split into one message per sector; every message is self-contained, so the
// timestamp header and the global price-only notice repeat on each one.
package format

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// Limits and layout widths (runes). Keep rendered lines <= 80 chars.
const (
	messageLimit = 4000
	widthModel   = 24
	widthPair    = 12 // "was→now", per Mtok, 2 decimals
	widthPct     = 4
	widthScore   = 5

	headerTimeLayout = "2 Jan 2006 15:04 JST"
	noticePrefix     = "⚠️ scored only by price — Artificial Analysis error: "
	freeTitle        = "Free tier:"
)

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// jst is Japan Standard Time; JST has no DST so a fixed zone is exact.
func jst() *time.Location { return time.FixedZone("JST", 9*3600) }

// Messages renders one HTML message for the ranking, or one per sector when
// the combined message would exceed the per-message limit. The free-tier
// block rides the last message when it fits, its own message otherwise.
func Messages(res rank.Result, free []string, now time.Time) []string {
	header := "🪙 OpenRouter discounts — " + now.In(jst()).Format(headerTimeLayout)
	start := func(b *strings.Builder) {
		b.WriteString(header)
		if res.GlobalErr != "" {
			b.WriteString("\n" + notice(res.GlobalErr))
		}
	}
	msgs := make([]string, 0, 4)
	var b strings.Builder
	start(&b)
	for _, sr := range res.Sectors {
		block := sectorBlock(sr, res.GlobalErr)
		if b.Len() > 0 && b.Len()+2+len(block) > messageLimit {
			msgs = append(msgs, b.String())
			b.Reset()
			start(&b)
		}
		b.WriteString("\n\n" + block)
	}
	freeBlock := freeTierBlock(free)
	if freeBlock != "" {
		if b.Len() > 0 && b.Len()+2+len(freeBlock) > messageLimit {
			msgs = append(msgs, b.String())
			b.Reset()
			start(&b)
		}
		b.WriteString("\n\n" + freeBlock)
	}
	msgs = append(msgs, b.String())
	return msgs
}

// NoDiscounts renders the short message for a day with no active discounts.
func NoDiscounts(free []string, now time.Time) string {
	var b strings.Builder
	b.WriteString("🪙 OpenRouter discounts — " + now.In(jst()).Format(headerTimeLayout))
	b.WriteString("\n\nno discounts today.")
	if block := freeTierBlock(free); block != "" {
		b.WriteString("\n\n" + block)
	}
	return b.String()
}

func sectorBlock(sr rank.SectorResult, globalErr string) string {
	var b strings.Builder
	b.WriteString(sr.Sector.Label())
	if sr.Err != "" || globalErr != "" {
		errText := sr.Err
		if errText == "" {
			errText = globalErr
		}
		b.WriteString("\n" + notice(errText))
	}
	b.WriteString("\n<pre>\n")
	b.WriteString(table(sr))
	b.WriteString("</pre>")
	return b.String()
}

func notice(errText string) string {
	return noticePrefix + htmlEscaper.Replace(errText)
}

// table renders the fixed-width ranking inside <pre>. The score column is
// omitted entirely when no row has a score.
func table(sr rank.SectorResult) string {
	scored := false
	for _, r := range sr.Rows {
		if r.Scored {
			scored = true
			break
		}
	}
	var rows []string
	if scored {
		rows = append(rows, tableRow("#", "model", "was→now", "Δ%", "score"))
	} else {
		rows = append(rows, tableRow("#", "model", "was→now", "Δ%", ""))
	}
	for i, r := range sr.Rows {
		pair := fmt.Sprintf("%.2f→%.2f", r.Was, r.Price)
		pct := fmt.Sprintf("%.0f", r.Pct)
		model := htmlEscaper.Replace(truncateRunes(r.ModelID, widthModel))
		var line string
		if scored {
			score := "n/a"
			if r.Scored {
				score = fmt.Sprintf("%.1f", r.Score)
			}
			line = tableRow(fmt.Sprint(i+1), model, pair, pct, score)
		} else {
			line = tableRow(fmt.Sprint(i+1), model, pair, pct, "")
		}
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n") + "\n"
}

// tableRow joins fixed-width columns; the score column is dropped when empty.
func tableRow(rankCol, modelCol, pairCol, pctCol, scoreCol string) string {
	cells := []string{
		padRight(rankCol, 2, false),
		padRight(modelCol, widthModel, false),
		padRight(pairCol, widthPair, true),
		padRight(pctCol, widthPct, true),
	}
	if scoreCol != "" {
		cells = append(cells, padRight(scoreCol, widthScore, true))
	}
	return strings.TrimRight(strings.Join(cells, " "), " ")
}

// padRight pads a rune-counted column; truncation happens at the call site.
func padRight(s string, width int, right bool) string {
	n := width - utf8.RuneCountInString(s)
	if n <= 0 {
		return s
	}
	if right {
		return strings.Repeat(" ", n) + s
	}
	return s + strings.Repeat(" ", n)
}

// truncateRunes cuts s to width runes, marking the cut with an ellipsis.
func truncateRunes(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	r := []rune(s)
	r = append(r[:width-1], '…')
	return string(r)
}

func freeTierBlock(free []string) string {
	if len(free) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(freeTitle + "\n<pre>\n")
	b.WriteString(fmt.Sprintf(":free models today — %d\n", len(free)))
	b.WriteString(htmlEscaper.Replace(strings.Join(free, ", ")))
	b.WriteString("\n</pre>")
	return b.String()
}
