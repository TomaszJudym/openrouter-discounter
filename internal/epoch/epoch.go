// Package epoch scores models from the Epoch AI Benchmarking Hub bulk
// download (https://epoch.ai/data/benchmark_data.zip, CC BY, no key): OTIS
// Mock AIME for Math and WebDev Arena for Code/CodeFree. It implements
// rank.Provider.
package epoch

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
	"github.com/tomaszjudym/openrouter-discounter/internal/match"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// DataURL is the Epoch AI benchmark bulk download.
const DataURL = "https://epoch.ai/data/benchmark_data.zip"

// SourceName labels Epoch-scored sectors in the price-only notice.
const SourceName = "Epoch AI"

// Benchmark files and columns inside the zip.
const (
	mathFile   = "otis_mock_aime_2024_2025.csv"
	mathCol    = "mean_score" // 0-1 fraction
	codeFile   = "webdev_arena_external.csv"
	codeCol    = "Arena Score" // Elo, min-max scaled to 0-100
	modelCol   = "Model version"
	maxCSVSize = 8 << 20
)

// Provider fetches the Epoch zip once and maps its scores to the OpenRouter
// ids given at construction.
type Provider struct {
	get   fetch.Getter
	url   string
	table match.Table
}

// New builds a provider for the OpenRouter ids.
func New(g fetch.Getter, orIDs []string) *Provider {
	return &Provider{get: g, url: DataURL, table: match.New(orIDs)}
}

// Scores implements rank.Provider for Math, Code, and CodeFree. A download
// failure marks all three sectors; a missing or unreadable benchmark file
// marks only its sectors.
func (p *Provider) Scores(ctx context.Context) map[rank.Sector]rank.SectorScores {
	out := make(map[rank.Sector]rank.SectorScores, 3)
	zr, err := p.download(ctx)
	if err != nil {
		for _, s := range []rank.Sector{rank.Math, rank.Code, rank.CodeFree} {
			out[s] = rank.SectorScores{Source: SourceName, Err: err.Error()}
		}
		return out
	}
	out[rank.Math] = p.sector(zr, mathFile, mathCol, func(v float64, _, _ float64) float64 { return v * 100 })
	code := p.sector(zr, codeFile, codeCol, func(v, lo, hi float64) float64 {
		if hi <= lo {
			return 100
		}
		return 100 * (v - lo) / (hi - lo)
	})
	out[rank.Code] = code
	out[rank.CodeFree] = code
	return out
}

func (p *Provider) download(ctx context.Context) (*zip.Reader, error) {
	body, err := p.get.Get(ctx, p.url, nil, "")
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, errors.New(fetch.Sanitize("unreadable zip: "+err.Error(), ""))
	}
	return zr, nil
}

// sector reads one benchmark CSV. scale maps a raw value to 0-100 given the
// file's min and max raw values. Each OpenRouter id keeps its best score
// across reasoning-effort variants.
func (p *Provider) sector(zr *zip.Reader, file, col string, scale func(v, lo, hi float64) float64) rank.SectorScores {
	recs, err := readCSV(zr, file, col)
	if err != nil {
		return rank.SectorScores{Source: SourceName, Err: fetch.Sanitize(err.Error(), "")}
	}
	lo, hi := 0.0, 0.0
	for i, r := range recs {
		if i == 0 || r.value < lo {
			lo = r.value
		}
		if i == 0 || r.value > hi {
			hi = r.value
		}
	}
	by := make(map[string]float64)
	for _, r := range recs {
		match.SetMax(by, p.table.Lookup(candidates(r.model)...), scale(r.value, lo, hi))
	}
	return rank.SectorScores{Source: SourceName, ByModel: by}
}

type record struct {
	model string
	value float64
}

// readCSV returns the (model, value) pairs of file; rows with an empty or
// non-numeric value are skipped.
func readCSV(zr *zip.Reader, file, col string) ([]record, error) {
	data, err := readFile(zr, file)
	if err != nil {
		return nil, err
	}
	cr := csv.NewReader(bytes.NewReader(data))
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s is empty", file)
	}
	mi, vi := index(rows[0], modelCol), index(rows[0], col)
	if mi < 0 || vi < 0 {
		return nil, fmt.Errorf("%s lacks %q or %q column", file, modelCol, col)
	}
	recs := make([]record, 0, len(rows)-1)
	for _, row := range rows[1:] {
		if mi >= len(row) || vi >= len(row) {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(row[vi]), 64)
		if err != nil || row[mi] == "" {
			continue
		}
		recs = append(recs, record{model: row[mi], value: v})
	}
	return recs, nil
}

// readFile returns the bytes of the zip entry whose base name is file,
// refusing entries above maxCSVSize rather than parsing a cut-off file.
func readFile(zr *zip.Reader, file string) ([]byte, error) {
	var zf *zip.File
	for _, f := range zr.File {
		if path.Base(f.Name) == file {
			zf = f
			break
		}
	}
	if zf == nil {
		return nil, fmt.Errorf("%s missing from zip", file)
	}
	rc, err := zf.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", file, err)
	}
	defer func() { _ = rc.Close() }() // read path: a close error is not actionable
	data, err := io.ReadAll(io.LimitReader(rc, maxCSVSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	if len(data) > maxCSVSize {
		return nil, fmt.Errorf("%s exceeds %d bytes", file, maxCSVSize)
	}
	return data, nil
}

func index(header []string, name string) int {
	for i, h := range header {
		if strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")) == name {
			return i
		}
	}
	return -1
}

// dateSuffix matches release-date tails: -2026-10-27, -20261027, -260628, -0528.
var dateSuffix = regexp.MustCompile(`-(\d{4}-\d{2}-\d{2}|\d{8}|\d{6}|\d{4})$`)

// candidates lists match names for an Epoch "Model version", most specific
// first: as is, without the "_effort" tail, then also without a date tail.
func candidates(version string) []string {
	out := []string{version}
	base := version
	if i := strings.LastIndexByte(version, '_'); i > 0 {
		base = version[:i]
		out = append(out, base)
	}
	if trimmed := dateSuffix.ReplaceAllString(base, ""); trimmed != base {
		out = append(out, trimmed)
	}
	return out
}
