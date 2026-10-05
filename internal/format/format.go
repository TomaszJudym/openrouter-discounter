// Package format renders the Telegram HTML message(s): a header, per-sector
// top-10 tables, and the free-tier block. Any message above the limit is
// split into one message per sector; every message is self-contained, so the
// timestamp header and the global price-only notice repeat on each one.
package format

import (
	"fmt"
	"slices"
	"strconv"
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
var jst = time.FixedZone("JST", 9*3600)

// Messages renders one HTML message for the ranking, or one per sector when
// the combined message would exceed the per-message limit. The free-tier
// block rides the last message when it fits, its own message otherwise.
func Messages(res rank.Result, free []string, now time.Time) []string {
	header := "🪙 OpenRouter discounts — " + now.In(jst).Format(headerTimeLayout)
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
	b.WriteString("🪙 OpenRouter discounts — " + now.In(jst).Format(headerTimeLayout))
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

// cell is one fixed-width column of a table line.
type cell struct {
	text  string
	width int
	right bool
}

// line renders cells separated by one space, trimming trailing padding.
func line(cells ...cell) string {
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = pad(c.text, c.width, c.right)
	}
	return strings.TrimRight(strings.Join(parts, " "), " ")
}

// table renders the fixed-width ranking inside <pre>. The score column is
// omitted when no row is scored; CodeFree omits the price columns.
func table(sr rank.SectorResult) string {
	withPrice := sr.Sector != rank.CodeFree
	withScore := slices.ContainsFunc(sr.Rows, func(r rank.Row) bool { return r.Scored })
	var b strings.Builder
	hdr := []cell{
		{text: "#", width: 2},
		{text: "model", width: widthModel},
	}
	if withPrice {
		hdr = append(hdr,
			cell{text: "was→now", width: widthPair, right: true},
			cell{text: "Δ%", width: widthPct, right: true})
	}
	if withScore {
		hdr = append(hdr, cell{text: "score", width: widthScore, right: true})
	}
	b.WriteString(line(hdr...))
	b.WriteByte('\n')
	for i, r := range sr.Rows {
		cells := []cell{
			{text: strconv.Itoa(i + 1), width: 2},
			{text: htmlEscaper.Replace(truncateRunes(r.ModelID, widthModel)), width: widthModel},
		}
		if withPrice {
			cells = append(cells,
				cell{text: fmt.Sprintf("%.2f→%.2f", r.Was, r.Price), width: widthPair, right: true},
				cell{text: fmt.Sprintf("%.0f", r.Pct), width: widthPct, right: true})
		}
		if withScore {
			score := "n/a"
			if r.Scored {
				score = fmt.Sprintf("%.1f", r.Score)
			}
			cells = append(cells, cell{text: score, width: widthScore, right: true})
		}
		b.WriteString(line(cells...))
		b.WriteByte('\n')
	}
	return b.String()
}

func truncateRunes(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	r := []rune(s)
	r = append(r[:width-1], '…')
	return string(r)
}

func pad(s string, width int, right bool) string {
	n := width - utf8.RuneCountInString(s)
	if n <= 0 {
		return s
	}
	if right {
		return strings.Repeat(" ", n) + s
	}
	return s + strings.Repeat(" ", n)
}

func freeTierBlock(free []string) string {
	if len(free) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(freeTitle + "\n<pre>\n")
	fmt.Fprintf(&b, ":free models today — %d\n", len(free))
	b.WriteString(htmlEscaper.Replace(strings.Join(free, ", ")))
	b.WriteString("\n</pre>")
	return b.String()
}
