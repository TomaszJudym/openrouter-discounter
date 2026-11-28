package pipeline

import (
	"cmp"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tomaszjudym/openrouter-discounter/internal/darkbloom"
	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
	"github.com/tomaszjudym/openrouter-discounter/internal/format"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// runStub serves one pipeline.Run's HTTP surface: the OpenRouter catalog
// and endpoint pricing, the Darkbloom model list, and 404s for the score
// sources (see Run: they degrade the run). client rewrites every request
// onto the stub, so the packages' URL constants need no overrides.
type runStub struct {
	catalogStatus   int               // /api/v1/models status; 0 → 200
	catalog         string            // /api/v1/models body
	endpoints       map[string]string // model id → endpoint body; missing id → 404
	darkbloomStatus int               // /v1/models status; 0 → 200
	darkbloom       string            // /v1/models body
}

func (s runStub) client(t *testing.T) *http.Client {
	t.Helper()
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/models":
			w.WriteHeader(cmp.Or(s.catalogStatus, http.StatusOK))
			_, _ = fmt.Fprint(w, s.catalog)
		case r.URL.Path == "/v1/models":
			w.WriteHeader(cmp.Or(s.darkbloomStatus, http.StatusOK))
			_, _ = fmt.Fprint(w, s.darkbloom)
		case strings.HasPrefix(r.URL.Path, "/api/v1/models/") && strings.HasSuffix(r.URL.Path, "/endpoints"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/models/"), "/endpoints")
			body, ok := s.endpoints[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprint(w, body)
		default:
			// a non-200 fails the score sources on their first attempt,
			// without the Getter's retry backoff
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	hc := srv.Client()
	inner := hc.Transport
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %s: %v", srv.URL, err)
	}
	hc.Transport = rewriter{next: inner, target: target}
	return hc
}

// rewriter sends every request to the stub whatever host the URL constants
// name (the catalog, Darkbloom and score URLs are consts).
type rewriter struct {
	next   http.RoundTripper
	target *url.URL
}

func (rw rewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.Host = rw.target.Host
	r2.URL.Scheme, r2.URL.Host = rw.target.Scheme, rw.target.Host
	return rw.next.RoundTrip(r2)
}

// catalogEntry is one chat model at 0.2/0.8 $/Mtok with the given
// intelligence index; the endpoint bodies price the same model.
func catalogEntry(id string, ii float64) string {
	return fmt.Sprintf(`{"id":%q,"hugging_face_id":%q,"pricing":{"prompt":"0.0000002","completion":"0.0000008"},`+
		`"benchmarks":{"artificial_analysis":{"intelligence_index":%v}},`+
		`"architecture":{"input_modalities":["text","image"]},`+
		`"supported_parameters":["tools"],"context_length":2000000}`, id, id, ii)
}

// endpointsBody is one provider endpoint for id at the given discount off
// the catalog list price.
func endpointsBody(id string, discount float64) string {
	return fmt.Sprintf(`{"data":{"id":%q,"endpoints":[{"provider_name":"List",`+
		`"pricing":{"prompt":"0.0000002","completion":"0.0000008","discount":%v},`+
		`"supported_parameters":["tools"],"context_length":2000000,`+
		`"max_completion_tokens":1048576,"supports_tool_choice":{}}]}}`, id, discount)
}

// darkbloomBody is one routable Darkbloom model serving the catalog model
// id at half its list price, direct.
func darkbloomBody(id string) string {
	return fmt.Sprintf(`{"data":[{"id":%q,"pricing":{"prompt":"0.0000001","completion":"0.0000004"},`+
		`"input_modalities":["text","image"],`+
		`"context_length":2000000,"max_output_length":1048576,`+
		`"metadata":{"can_accept":true,"routable_providers":2}}]}`, id)
}

// catalogAll discounts vendor/alphachat's endpoint by a quarter;
// vendor/betachat sells only list price.
var catalogAll = `{"data":[` + catalogEntry("vendor/alphachat", 50) + `,` + catalogEntry("vendor/betachat", 40) + `]}`

// catalogListOnly has no discounted endpoint at all: vendor/betachat only.
var catalogListOnly = `{"data":[` + catalogEntry("vendor/betachat", 40) + `]}`

// run calls Run over a stub, failing the test on the catalog error path.
func run(t *testing.T, hc *http.Client, keys Keys) Result {
	t.Helper()
	res, err := Run(t.Context(), hc, keys, time.Now())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return res
}

// assertDirectRoutes fails unless every row of sr routes via db:.
func assertDirectRoutes(t *testing.T, where string, sr rank.SectorResult) {
	t.Helper()
	for _, r := range sr.Rows {
		if !discounts.IsDirect(r.Endpoint) {
			t.Errorf("%s: row %s routes via %q, want a db: endpoint", where, r.ModelID, r.Endpoint)
		}
	}
}

// assertOpenRouterRoutes fails unless no row of sr routes via db:.
func assertOpenRouterRoutes(t *testing.T, sr rank.SectorResult) {
	t.Helper()
	for _, r := range sr.Rows {
		if discounts.IsDirect(r.Endpoint) {
			t.Errorf("%s: OpenRouter preset routes %s via %s", sr.Sector, r.ModelID, r.Endpoint)
		}
	}
}

// TestRunSelectsBothSides: a full day ranks the OpenRouter discounts and
// appends the Darkbloom-direct price table, and every preset side keeps to
// its own endpoints.
func TestRunSelectsBothSides(t *testing.T) {
	hc := runStub{
		catalog: catalogAll,
		endpoints: map[string]string{
			"vendor/alphachat": endpointsBody("vendor/alphachat", 0.25),
			"vendor/betachat":  endpointsBody("vendor/betachat", 0),
		},
		darkbloom: darkbloomBody("alphachat"),
	}.client(t)
	res := run(t, hc, Keys{Darkbloom: "db-key"})
	if res.NoDiscounts {
		t.Fatal("NoDiscounts = true, want false: both sides rank")
	}
	if res.DirectErr != "" {
		t.Errorf("DirectErr = %q, want none", res.DirectErr)
	}
	if len(res.Report.Sectors) == 0 {
		t.Fatal("empty report")
	}
	last := res.Report.Sectors[len(res.Report.Sectors)-1]
	if last.Sector != rank.DarkbloomDirect {
		t.Fatalf("report ends with %s, want %s", last.Sector, rank.DarkbloomDirect)
	}
	if len(last.Rows) == 0 || last.Rows[0].ModelID != "vendor/alphachat" {
		t.Errorf("direct table rows = %+v, want vendor/alphachat first", last.Rows)
	}
	assertDirectRoutes(t, "direct table", last)
	if len(res.OpenRouter) == 0 {
		t.Fatal("OpenRouter presets = nil, want the preset sectors")
	}
	for _, sr := range res.OpenRouter {
		assertOpenRouterRoutes(t, sr)
	}
	if len(res.Direct) == 0 {
		t.Fatal("Direct presets = nil, want the direct preset sectors")
	}
	for _, sr := range res.Direct {
		assertDirectRoutes(t, string(sr.Sector), sr)
	}
}

// TestRunNoDiscountsWhenBothSidesEmpty: no affordable OpenRouter discount
// and no Darkbloom side — the short message, no report and no presets.
func TestRunNoDiscountsWhenBothSidesEmpty(t *testing.T) {
	hc := runStub{
		catalog:   catalogListOnly,
		endpoints: map[string]string{"vendor/betachat": endpointsBody("vendor/betachat", 0)},
	}.client(t) // no Darkbloom key
	res := run(t, hc, Keys{})
	if !res.NoDiscounts {
		t.Fatal("NoDiscounts = false, want true: both sides empty")
	}
	if res.Report.Sectors != nil || res.OpenRouter != nil || res.Direct != nil {
		t.Errorf("Run() = %+v, want no report and no presets", res)
	}
}

// TestRunDirectTableWhenOpenRouterSideEmpty: with no affordable OpenRouter
// discount but priced Darkbloom-direct rows, the report is the direct table
// and the OpenRouter presets stay as they are.
func TestRunDirectTableWhenOpenRouterSideEmpty(t *testing.T) {
	hc := runStub{
		catalog:   catalogListOnly,
		endpoints: map[string]string{"vendor/betachat": endpointsBody("vendor/betachat", 0)},
		darkbloom: darkbloomBody("betachat"),
	}.client(t)
	res := run(t, hc, Keys{Darkbloom: "db-key"})
	if res.NoDiscounts {
		t.Fatal("NoDiscounts = true, want false: the direct side has priced rows")
	}
	if len(res.Report.Sectors) != 1 {
		t.Fatalf("report sectors = %d, want only the direct table: %+v", len(res.Report.Sectors), res.Report.Sectors)
	}
	sr := res.Report.Sectors[0]
	if sr.Sector != rank.DarkbloomDirect || len(sr.Rows) == 0 || sr.Rows[0].ModelID != "vendor/betachat" {
		t.Errorf("report = %+v, want the vendor/betachat direct table", sr)
	}
	if res.OpenRouter != nil {
		t.Errorf("OpenRouter presets = %+v, want nil: nothing affordable to preset", res.OpenRouter)
	}
	if len(res.Direct) == 0 {
		t.Error("Direct presets = nil, want the direct preset sectors")
	}
}

// TestRunDirectErrSanitized: a Darkbloom failure degrades the run with a
// sanitized error — the key removed and the text truncated.
func TestRunDirectErrSanitized(t *testing.T) {
	const key = "sk-darkbloom-secret"
	hc := runStub{
		catalog:   catalogListOnly,
		endpoints: map[string]string{"vendor/betachat": endpointsBody("vendor/betachat", 0)},
		// 403 is not retried; the body carries the key first so it stays
		// inside fetch's %.200s window and the sanitized text also exercises
		// the truncation
		darkbloomStatus: http.StatusForbidden,
		darkbloom:       `{"error":"` + key + ` ` + strings.Repeat("x", 200) + `"}`,
	}.client(t)
	res := run(t, hc, Keys{Darkbloom: key})
	if res.DirectErr == "" {
		t.Fatal("DirectErr = \"\", want the sanitized Darkbloom failure")
	}
	if strings.Contains(res.DirectErr, key) {
		t.Errorf("DirectErr %q leaks the key", res.DirectErr)
	}
	if !strings.Contains(res.DirectErr, "REDACTED") {
		t.Errorf("DirectErr %q lacks REDACTED, want the key replaced", res.DirectErr)
	}
	if n := utf8.RuneCountInString(res.DirectErr); n > fetch.MaxErrRunes {
		t.Errorf("DirectErr = %d runes, want ≤ %d", n, fetch.MaxErrRunes)
	}
	// a failed Darkbloom side is reported, not silenced: the otherwise empty
	// day's message is the DARKBLOOM error section, not "no discounts today."
	if res.NoDiscounts {
		t.Error("NoDiscounts = true, want false: the failed side is reported")
	}
	if len(res.Report.Sectors) != 1 {
		t.Fatalf("report sectors = %d, want only the Darkbloom error section", len(res.Report.Sectors))
	}
	sr := res.Report.Sectors[0]
	if sr.Sector != rank.DarkbloomDirect || sr.Source != darkbloom.SourceName || len(sr.Rows) != 0 {
		t.Errorf("report = %+v, want the empty Darkbloom error section", sr)
	}
	// nothing else ranks, so the notice carries that fact before the error
	if want := "no discounts found today; " + res.DirectErr; sr.Err != want {
		t.Errorf("notice = %q, want %q", sr.Err, want)
	}
}

// TestRunDirectErrNoticeWithDiscounts: with affordable OpenRouter discounts
// still ranked, a Darkbloom failure appends the error section after the
// ranked ones — sanitized error, no rows, no empty-day qualifier.
func TestRunDirectErrNoticeWithDiscounts(t *testing.T) {
	const key = "sk-darkbloom-secret"
	hc := runStub{
		catalog: catalogAll,
		endpoints: map[string]string{
			"vendor/alphachat": endpointsBody("vendor/alphachat", 0.25),
			"vendor/betachat":  endpointsBody("vendor/betachat", 0),
		},
		darkbloomStatus: http.StatusForbidden,
		darkbloom:       `{"error":"` + key + `"}`,
	}.client(t)
	res := run(t, hc, Keys{Darkbloom: key})
	if res.NoDiscounts {
		t.Fatal("NoDiscounts = true, want false: the OpenRouter side ranks")
	}
	last := res.Report.Sectors[len(res.Report.Sectors)-1]
	if last.Sector != rank.DarkbloomDirect {
		t.Fatalf("report ends with %s, want %s", last.Sector, rank.DarkbloomDirect)
	}
	if len(last.Rows) != 0 {
		t.Errorf("error section has %d rows, want none", len(last.Rows))
	}
	if last.Source != darkbloom.SourceName {
		t.Errorf("source = %q, want %q", last.Source, darkbloom.SourceName)
	}
	if last.Err != res.DirectErr {
		t.Errorf("notice = %q, want the sanitized DirectErr %q", last.Err, res.DirectErr)
	}
	if strings.Contains(last.Err, "no discounts") {
		t.Errorf("notice = %q, want no empty-day qualifier: discounts rank above", last.Err)
	}
	if strings.Contains(last.Err, key) {
		t.Errorf("notice %q leaks the key", last.Err)
	}
}

// TestRunDirectErrDayRendersNotice: the message built from a Darkbloom
// failure day (no affordable discounts, direct side down) carries both the
// empty-day wording and the sanitized error, and never the key.
func TestRunDirectErrDayRendersNotice(t *testing.T) {
	const key = "sk-darkbloom-secret"
	hc := runStub{
		catalog:         catalogListOnly,
		endpoints:       map[string]string{"vendor/betachat": endpointsBody("vendor/betachat", 0)},
		darkbloomStatus: http.StatusForbidden,
		darkbloom:       `{"error":"` + key + `"}`,
	}.client(t)
	res := run(t, hc, Keys{Darkbloom: key})
	msgs := format.Messages(res.Report, time.Now())
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want one", len(msgs))
	}
	msg := msgs[0]
	if !strings.Contains(msg, "no discounts found today") {
		t.Errorf("message %q lacks the empty-day wording", msg)
	}
	if !strings.Contains(msg, darkbloom.SourceName+" error: ") {
		t.Errorf("message %q lacks the Darkbloom error notice", msg)
	}
	if strings.Contains(msg, key) {
		t.Errorf("message %q leaks the key", msg)
	}
}

// TestRunCatalogFailure: only a catalog failure is an error, and the result
// stays zero.
func TestRunCatalogFailure(t *testing.T) {
	hc := runStub{catalogStatus: http.StatusInternalServerError, catalog: "boom"}.client(t)
	res, err := Run(t.Context(), hc, Keys{}, time.Now())
	if err == nil {
		t.Fatal("Run() error = nil, want the catalog failure")
	}
	if res.NoDiscounts || res.Report.Sectors != nil || res.OpenRouter != nil || res.Direct != nil {
		t.Errorf("Run() = %+v on failure, want zero", res)
	}
}
