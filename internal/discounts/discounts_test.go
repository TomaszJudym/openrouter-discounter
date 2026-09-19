package discounts

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func thursday(t *testing.T) time.Time {
	t.Helper()
	day, err := time.Parse(time.DateOnly, "2026-09-17") // a Thursday
	if err != nil {
		t.Fatalf("parse date: %v", err)
	}
	return day
}

func mustParse(t *testing.T, raw string) []Model {
	t.Helper()
	var r response
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("Unmarshal(%q) error = %v, want nil", raw, err)
	}
	return r.Data
}

func TestDetectWeekdayWindowDiscount(t *testing.T) {
	data := mustParse(t, `{"data":[
		{"id":"vendor/weekend-model","name":"Weekend Model","pricing":{
			"prompt":"0.0000020","completion":"0.0000100",
			"overrides":[{"utc_days":["saturday","sunday"],"prompt":"0.0000010","completion":"0.0000050"},
			             {"utc_days":["thursday"],"utc_start":400,"utc_end":600,"prompt":"0.0000014","completion":"0.0000070"}]}},
		{"id":"vendor/surcharge-model","name":"Surcharge","pricing":{
			"prompt":"0.0000010","completion":"0.0000050",
			"overrides":[{"utc_days":["thursday"],"prompt":"0.0000020","completion":"0.0000100"}]}},
		{"id":"vendor/flat-model","name":"Flat","pricing":{
			"prompt":"0.0000020","completion":"0.0000100",
			"overrides":[{"utc_days":["thursday"],"prompt":"0.0000020","completion":"0.0000100"}]}}
	]}`)
	got := Detect(data, thursday(t))
	if len(got) != 1 {
		t.Fatalf("Detect() = %+v, want exactly one discount", got)
	}
	d := got[0]
	if d.ModelID != "vendor/weekend-model" {
		t.Errorf("ModelID = %q, want %q", d.ModelID, "vendor/weekend-model")
	}
	// cheapest of the two Thursday windows wins: 1.4/Mtok vs base 2.0/Mtok
	if d.Price != 1.4 || d.Was != 2.0 {
		t.Errorf("Price/Was = %v/%v, want 1.4/2.0", d.Price, d.Was)
	}
	if want := (1.4/2.0 - 1) * 100; !floatEq(d.Pct, want) {
		t.Errorf("Pct = %v, want %v", d.Pct, want)
	}
}

func TestDetectIgnoresVolumeTiersAndNullPricing(t *testing.T) {
	data := mustParse(t, `{"data":[
		{"id":"vendor/volume-only","name":"Volume","pricing":{
			"prompt":"0.0000020","completion":"0.0000100",
			"overrides":[{"min_prompt_tokens":272000,"prompt":"0.0000010","completion":"0.0000050"}]}},
		{"id":"vendor/zero-price","name":"Zero","pricing":{"prompt":"0","completion":"0"}},
		{"id":"vendor/no-pricing","name":"NoPricing"},
		{"id":"vendor/no-overrides","name":"Plain","pricing":{"prompt":"0.0000020","completion":"0.0000100"}}
	]}`)
	if got := Detect(data, thursday(t)); len(got) != 0 {
		t.Errorf("Detect() = %+v, want none (volume tiers have no time window)", got)
	}
}

func TestDetectCheapestWindowAcrossBuckets(t *testing.T) {
	data := mustParse(t, `{"data":[
		{"id":"vendor/bucketed","name":"Bucketed","pricing":{
			"prompt":"0.0000020","completion":"0.0000100",
			"overrides":[
				{"utc_days":["thursday"],"utc_start":0,"utc_end":400,"prompt":"0.0000018","completion":"0.0000090"},
				{"utc_days":["thursday"],"utc_start":400,"utc_end":0,"prompt":"0.0000012","completion":"0.0000060"}]}}
	]}`)
	got := Detect(data, thursday(t))
	if len(got) != 1 || got[0].Price != 1.2 {
		t.Fatalf("Detect() = %+v, want price 1.2/Mtok (cheapest Thursday bucket)", got)
	}
}

func TestDetectFreeTier(t *testing.T) {
	data := mustParse(t, `{"data":[
		{"id":"openai/gpt-oss-120b:free","name":"Free Model","pricing":{"prompt":"0","completion":"0"}},
		{"id":"openai/gpt-oss-120b","name":"Paid Model","pricing":{"prompt":"0.0000020","completion":"0.0000100"}}
	]}`)
	if free := freeModels(data); len(free) != 1 || free[0] != "openai/gpt-oss-120b:free" {
		t.Errorf("freeModels() = %v, want [openai/gpt-oss-120b:free]", free)
	}
	if got := Detect(data, thursday(t)); len(got) != 0 {
		t.Errorf("Detect() = %+v, want none", got)
	}
}

func TestEndpointDiscountMath(t *testing.T) {
	byID := map[string][]Endpoint{
		"a/model-a": {
			{Provider: "Cheap", Prompt: "0.0000006426", Discount: 0.541},
			{Provider: "Full", Prompt: "0.0000014", Discount: 0},
		},
		"b/model-b": {
			{Provider: "HalfOff", Prompt: "0.0000005", Discount: 0.5},
			{Provider: "Small", Prompt: "0.0000008", Discount: 0.2},
		},
		"c/model-c": {{Provider: "None", Prompt: "0.000001", Discount: 0}},
	}
	got := endpointDiscounts(byID)
	if len(got) != 2 {
		t.Fatalf("endpointDiscounts() = %+v, want 2 discounts", got)
	}
	a := got[0]
	if a.ModelID != "a/model-a" || a.Provider != "Cheap" {
		t.Errorf("model-a row = %+v, want provider Cheap", a)
	}
	// 0.0000006426/Mtok = $0.6426; list = 0.6426/(1-0.541) = 1.4
	if !floatEq(a.Price, 0.6426) || !floatEq(a.Was, 1.4) {
		t.Errorf("Price/Was = %v/%v, want 0.6426/1.4", a.Price, a.Was)
	}
	if !floatEq(a.Pct, -54.1) {
		t.Errorf("Pct = %v, want -54.1", a.Pct)
	}
	b := got[1]
	// cheapest effective price among discounted endpoints: 0.5 < 0.8
	if b.Provider != "HalfOff" || !floatEq(b.Price, 0.5) || !floatEq(b.Was, 1.0) {
		t.Errorf("model-b row = %+v, want HalfOff 0.5/1.0", b)
	}
}

func TestFetchDiscountsMergesEndpointAndWindowRows(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	old := endpointsURLFormat
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/models/vendor/promo/"):
			_, _ = fmt.Fprint(w, `{"data":{"id":"vendor/promo","endpoints":[{"provider_name":"Cheap","pricing":{"prompt":"0.0000005","completion":"0.000002","discount":0.5}},{"provider_name":"List","pricing":{"prompt":"0.000001","completion":"0.000004","discount":0}}]}}`)
		case strings.HasPrefix(r.URL.Path, "/models/vendor/windowonly/"):
			_, _ = fmt.Fprint(w, `{"data":{"id":"vendor/windowonly","endpoints":[{"provider_name":"List","pricing":{"prompt":"0.000002","completion":"0.000008","discount":0}}]}}`)
		case strings.HasPrefix(r.URL.Path, "/models/vendor/broken/"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"error":"nope"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() { endpointsURLFormat = old })
	_ = srv.Client() // starts the in-memory server and fills srv.URL
	endpointsURLFormat = srv.URL + "/models/%s/%s/endpoints"

	data := mustParse(t, `{"data":[
		{"id":"vendor/promo","name":"Promo","pricing":{"prompt":"0.000001","completion":"0.000004"}},
		{"id":"vendor/windowonly","name":"Window","pricing":{"prompt":"0.000002","completion":"0.000008",
			"overrides":[{"utc_days":["thursday"],"prompt":"0.0000012","completion":"0.0000050"}]}},
		{"id":"vendor/broken","name":"Broken","pricing":{"prompt":"0.000002","completion":"0.000008"}},
		{"id":"vendor/freem:free","name":"Free","pricing":{"prompt":"0","completion":"0"}}
	]}`)
	// srv.Client(): the in-memory transport routes the example.com host
	discs, free, err := FetchDiscounts(t.Context(), srv.Client(), data, thursday(t))
	if len(free) != 1 || free[0] != "vendor/freem:free" {
		t.Errorf("free = %v, want [vendor/freem:free]", free)
	}
	if len(discs) != 2 {
		t.Fatalf("discounts = %+v, want 2 (promo + windowonly)", discs)
	}
	if discs[0].ModelID != "vendor/promo" || discs[0].Provider != "Cheap" {
		t.Errorf("first row = %+v, want vendor/promo via Cheap", discs[0])
	}
	if discs[1].ModelID != "vendor/windowonly" || discs[1].Provider != "" {
		t.Errorf("second row = %+v, want catalog-window row without provider", discs[1])
	}
	if err == nil || !strings.Contains(err.Error(), "vendor/broken") {
		t.Errorf("err = %v, want failure for vendor/broken", err)
	}
	for _, p := range seen {
		if strings.Contains(p, "freem:free") {
			t.Errorf(":free model was fetched: %s", p)
		}
	}
}

func TestFetchEndpointsNotFoundYieldsNoError(t *testing.T) {
	old := endpointsURLFormat
	t.Cleanup(func() { endpointsURLFormat = old })
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	_ = srv.Client() // starts the in-memory server and fills srv.URL
	endpointsURLFormat = srv.URL + "/models/%s/%s/endpoints"
	eps, err := fetchEndpoints(t.Context(), srv.Client(), "ghost/model")
	if err != nil || eps != nil {
		t.Errorf("fetchEndpoints() = %v, %v, want nil, nil", eps, err)
	}
}

func TestFetchEndpointsParsesPricing(t *testing.T) {
	old := endpointsURLFormat
	t.Cleanup(func() { endpointsURLFormat = old })
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":{"id":"inception/mercury-2.5","endpoints":[
			{"provider_name":"Inception","pricing":{"prompt":"0.00000004","completion":"0.00000015","discount":0.8}},
			{"provider_name":"Ambient","pricing":{"prompt":"0.0000006","completion":"0.000002","discount":0}}]}}`)
	}))
	_ = srv.Client() // starts the in-memory server and fills srv.URL
	endpointsURLFormat = srv.URL + "/models/%s/%s/endpoints"
	eps, err := fetchEndpoints(t.Context(), srv.Client(), "inception/mercury-2.5")
	if err != nil {
		t.Fatalf("fetchEndpoints() error = %v, want nil", err)
	}
	if len(eps) != 2 || eps[0].Provider != "Inception" || eps[0].Discount != 0.8 || eps[0].Prompt != "0.00000004" {
		t.Errorf("endpoints = %+v, want Inception with 0.8 discount first", eps)
	}
}

func TestFetchBadStatus(t *testing.T) {
	hc := &http.Client{Transport: errTransport{}}
	_, err := Fetch(t.Context(), hc)
	if err == nil {
		t.Fatal("Fetch() error = nil, want transport error")
	}
}

type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("connection refused")
}

func TestDayMatchingAcrossCase(t *testing.T) {
	if got := minTodayPrice([]Override{{Days: []string{"THURSDAY"}, Prompt: "0.0000010"}}, "thursday"); got != 0.000001 {
		t.Errorf("minTodayPrice() = %v, want 0.000001", got)
	}
}

func floatEq(a, b float64) bool { return absF(a-b) < 1e-9 }

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
