package discounts

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
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

// at is the Thursday test day at hh:mm UTC.
func at(t *testing.T, hh, mm int) time.Time {
	t.Helper()
	return thursday(t).Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute)
}

// ep builds an endpoint with flat pricing and a provider discount.
func ep(provider, prompt, completion string, discount float64) Endpoint {
	return Endpoint{Provider: provider, Pricing: Pricing{Prompt: prompt, Completion: completion, Discount: discount}}
}

// flat is the blended DefaultWorkload price of uncached prompt/completion
// rates, USD per Mtok.
func flat(prompt, completion float64) float64 {
	return Rates{Prompt: prompt, Completion: completion}.Blended(DefaultWorkload)
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
	// Thursday 00:00 + 24h: 2h at 1.4, 22h at the 2.0 base
	got := Detect(data, at(t, 0, 0))
	if len(got) != 1 {
		t.Fatalf("Detect(Thu) = %+v, want exactly one discount", got)
	}
	d := got[0]
	if d.ModelID != "vendor/weekend-model" || d.Window != "thursday 04:00–06:00 UTC" {
		t.Errorf("row = %+v, want vendor/weekend-model in the Thursday 04:00 window", d)
	}
	if want := (2*1.4 + 22*2.0) / 24; !floatEq(d.Prompt, want) || !floatEq(d.Price, flat(want, want*5)) || !floatEq(d.Was, flat(2, 10)) {
		t.Errorf("Prompt/Price/Was = %v/%v/%v, want 24h average %v", d.Prompt, d.Price, d.Was, want)
	}
	if !floatEq(d.Pct, -2.5) || !floatEq(d.PeakPrompt, 2) {
		t.Errorf("Pct/PeakPrompt = %v/%v, want -2.5/2", d.Pct, d.PeakPrompt)
	}
	// a Friday run sees no window within 24h; Saturday is half price all day
	if got := Detect(data, at(t, 24, 0)); len(got) != 0 {
		t.Errorf("Detect(Fri) = %+v, want none", got)
	}
	got = Detect(data, at(t, 48, 0))
	if len(got) != 1 || !floatEq(got[0].Pct, -50) || got[0].Window != "saturday,sunday" {
		t.Errorf("Detect(Sat) = %+v, want -50%% in the saturday,sunday window", got)
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

func TestDetectAveragesWindowsOverHorizon(t *testing.T) {
	data := mustParse(t, `{"data":[
		{"id":"vendor/bucketed","name":"Bucketed","pricing":{
			"prompt":"0.0000020","completion":"0.0000100",
			"overrides":[
				{"utc_days":["thursday"],"utc_start":0,"utc_end":400,"prompt":"0.0000018","completion":"0.0000090"},
				{"utc_days":["thursday"],"utc_start":400,"utc_end":0,"prompt":"0.0000012","completion":"0.0000060"}]}}
	]}`)
	for _, tt := range []struct {
		hh           int
		prompt, peak float64
	}{
		{0, (4*1.8 + 20*1.2) / 24, 1.8},  // all of Thursday
		{4, (20*1.2 + 4*2.0) / 24, 2.0},  // 20h of the cheap bucket, then Friday
		{23, (1*1.2 + 23*2.0) / 24, 2.0}, // last Thursday hour, then Friday
	} {
		got := Detect(data, at(t, tt.hh, 0))
		if len(got) != 1 || !floatEq(got[0].Prompt, tt.prompt) || !floatEq(got[0].PeakPrompt, tt.peak) {
			t.Errorf("Detect(%02d:00) = %+v, want prompt %v, peak %v", tt.hh, got, tt.prompt, tt.peak)
		}
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
			ep("Cheap", "0.0000006426", "", 0.541),
			ep("Full", "0.0000014", "", 0),
		},
		"b/model-b": {
			ep("HalfOff", "0.0000005", "", 0.5),
			ep("Small", "0.0000008", "", 0.2),
		},
		"c/model-c": {ep("None", "0.000001", "", 0)},
	}
	got := endpointDiscounts(byID, thursday(t))
	if len(got) != 2 {
		t.Fatalf("endpointDiscounts() = %+v, want 2 discounts", got)
	}
	a := got[0]
	if a.ModelID != "a/model-a" || a.Provider != "Cheap" {
		t.Errorf("model-a row = %+v, want provider Cheap", a)
	}
	// 0.0000006426/token = $0.6426/Mtok; list = 0.6426/(1-0.541) = 1.4
	if !floatEq(a.Prompt, 0.6426) || !floatEq(a.Price, flat(0.6426, 0)) || !floatEq(a.Was, flat(1.4, 0)) {
		t.Errorf("Prompt/Price/Was = %v/%v/%v, want 0.6426 prompt, list 1.4", a.Prompt, a.Price, a.Was)
	}
	if !floatEq(a.Pct, -54.1) {
		t.Errorf("Pct = %v, want -54.1", a.Pct)
	}
	b := got[1]
	// cheapest effective price among discounted endpoints: 0.5 < 0.8
	if b.Provider != "HalfOff" || !floatEq(b.Prompt, 0.5) || !floatEq(b.Pct, -50) {
		t.Errorf("model-b row = %+v, want HalfOff 0.5/1.0", b)
	}
}

// stubEndpoints serves the endpoint fixtures of the merge test and records
// every requested path.
func stubEndpoints(t *testing.T) (*http.Client, func() []string) {
	t.Helper()
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
		case strings.HasPrefix(r.URL.Path, "/models/vendor/beaten/"):
			// window active, but the catalog's own windows yield to endpoint data
			_, _ = fmt.Fprint(w, `{"data":{"id":"vendor/beaten","endpoints":[{"provider_name":"List","pricing":{"prompt":"0.000002","completion":"0.000008","discount":0}}]}}`)
		case strings.HasPrefix(r.URL.Path, "/models/vendor/broken/"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"error":"nope"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() { endpointsURLFormat = old })
	// srv.Client(): the in-memory transport routes the example.com host
	hc := srv.Client() // starts the in-memory server and fills srv.URL
	endpointsURLFormat = srv.URL + "/models/%s/%s/endpoints"
	return hc, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func TestFetchDiscountsMergesEndpointAndWindowRows(t *testing.T) {
	hc, seen := stubEndpoints(t)
	data := mustParse(t, `{"data":[
		{"id":"vendor/promo","name":"Promo","pricing":{"prompt":"0.000001","completion":"0.000004"}},
		{"id":"vendor/windowonly","name":"Window","pricing":{"prompt":"0.000002","completion":"0.000008",
			"overrides":[{"utc_days":["thursday"],"prompt":"0.0000012","completion":"0.0000050"}]}},
		{"id":"vendor/beaten","name":"Beaten","pricing":{"prompt":"0.000002","completion":"0.000008",
			"overrides":[{"utc_days":["thursday"],"prompt":"0.0000012","completion":"0.0000050"}]}},
		{"id":"vendor/promo:batch","name":"Batch","pricing":{"prompt":"0.0000005","completion":"0.000002",
			"overrides":[{"utc_days":["thursday"],"prompt":"0.0000001"}]}},
		{"id":"vendor/broken","name":"Broken","pricing":{"prompt":"0.000002","completion":"0.000008"}},
		{"id":"vendor/freem:free","name":"Free","pricing":{"prompt":"0","completion":"0"}}
	]}`)
	discs, _, free, err := FetchDiscounts(t.Context(), hc, data, thursday(t))
	if len(free) != 1 || free[0] != "vendor/freem:free" {
		t.Errorf("free = %v, want [vendor/freem:free]", free)
	}
	if len(discs) != 2 {
		t.Fatalf("discounts = %+v, want 2 (promo + windowonly; beaten has endpoint data)", discs)
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
	for _, p := range seen() {
		if strings.Contains(p, "freem:free") || strings.Contains(p, ":batch") {
			t.Errorf(":free or :batch model was fetched: %s", p)
		}
	}
}

func TestChatText(t *testing.T) {
	tests := []struct {
		name string
		m    Model
		want bool
	}{
		{
			name: "nil_output_modalities",
			m:    Model{ID: "a/b"},
			want: true,
		},
		{
			name: "empty_output_modalities",
			m:    Model{ID: "a/b", Architecture: Architecture{}},
			want: true,
		},
		{
			name: "text_in_output_modalities",
			m:    Model{ID: "a/b", Architecture: Architecture{OutputModalities: []string{"image", "text"}}},
			want: true,
		},
		{
			name: "missing_text_in_output_modalities",
			m:    Model{ID: "a/b", Architecture: Architecture{OutputModalities: []string{"image"}}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.ChatText(); got != tt.want {
				t.Errorf("ChatText() = %v, want %v", got, tt.want)
			}
		})
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
	if len(eps) != 2 || eps[0].Provider != "Inception" || eps[0].Pricing.Discount != 0.8 || eps[0].Pricing.Prompt != "0.00000004" {
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
	return nil, errors.New("connection refused")
}

func TestDayMatchingAcrossCase(t *testing.T) {
	if !(Override{Days: []string{"THURSDAY"}}).activeAt(thursday(t)) {
		t.Error("activeAt(Thursday) = false for utc_days THURSDAY, want true")
	}
}

func floatEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestAffordableFiltersPriceCeilings(t *testing.T) {
	ds := []Discount{
		{ModelID: "ok", Prompt: 0.5, Completion: 1.0},
		{ModelID: "input-over", Prompt: 0.51, Completion: 0.5},
		{ModelID: "output-over", Prompt: 0.1, Completion: 1.01},
		{ModelID: "unlisted-output", Prompt: 0.25},
	}
	got := Affordable(ds)
	ids := make([]string, 0, len(got))
	for _, d := range got {
		ids = append(ids, d.ModelID)
	}
	if want := []string{"ok", "unlisted-output"}; !slices.Equal(ids, want) {
		t.Errorf("Affordable() ids = %v, want %v", ids, want)
	}
}

func TestDetectCarriesCompletion(t *testing.T) {
	data := mustParse(t, `{"data":[
		{"id":"v/own","pricing":{"prompt":"0.000002","completion":"0.00001",
			"overrides":[{"utc_days":["thursday"],"prompt":"0.0000005","completion":"0.0000015"}]}},
		{"id":"v/inherit","pricing":{"prompt":"0.000002","completion":"0.000003",
			"overrides":[{"utc_days":["thursday"],"prompt":"0.0000005"}]}}
	]}`)
	got := Detect(data, thursday(t))
	if len(got) != 2 {
		t.Fatalf("Detect() = %+v, want 2 rows", got)
	}
	if !floatEq(got[0].Completion, 1.5) {
		t.Errorf("override completion = %v, want 1.5", got[0].Completion)
	}
	if !floatEq(got[1].Completion, 3) {
		t.Errorf("base completion fallback = %v, want 3", got[1].Completion)
	}
}

func TestTextOnly(t *testing.T) {
	tests := []struct {
		name string
		arch Architecture
		want bool
	}{
		{"unlisted", Architecture{}, true},
		{"text", Architecture{InputModalities: []string{"text", "file"}, OutputModalities: []string{"text"}}, true},
		{"image input", Architecture{InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"}}, false},
		{"video input", Architecture{InputModalities: []string{"video", "text"}}, false},
		{"image output", Architecture{InputModalities: []string{"text"}, OutputModalities: []string{"text", "image"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Model{Architecture: tt.arch}).TextOnly(); got != tt.want {
				t.Errorf("TextOnly() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestForAnyPicksUndiscounted(t *testing.T) {
	list := Tariff{Base: Rates{Prompt: 0.1, Completion: 0.2}}
	promoNow := Tariff{Base: Rates{Prompt: 0.3, Completion: 0.6}}
	promoWas := Tariff{Base: Rates{Prompt: 0.6, Completion: 1.2}}
	d := Discount{ModelID: "m/x", Offers: []Offer{
		{Provider: "Promo", Endpoint: "promo", Avg: promoNow, Peak: promoNow, List: promoWas},
		{Provider: "Flat", Endpoint: "flat", Avg: list, Peak: list, List: list},
	}}
	if _, ok := d.For(Require{}, DefaultWorkload); ok {
		t.Errorf("For: ok, want false (cheapest offer is not discounted)")
	}
	got, ok := d.ForAny(Require{}, DefaultWorkload)
	if !ok || got.Endpoint != "flat" || got.Pct != 0 {
		t.Errorf("ForAny = %+v, %v; want flat endpoint, Pct 0", got, ok)
	}
	if _, ok := d.ForAny(Require{Tools: true}, DefaultWorkload); ok {
		t.Errorf("ForAny with unmet Require: ok, want false")
	}
}

func TestFetchDiscountsPricedRows(t *testing.T) {
	hc, _ := stubEndpoints(t)
	data := mustParse(t, `{"data":[
		{"id":"vendor/promo","name":"Promo","pricing":{"prompt":"0.000001","completion":"0.000004"}},
		{"id":"vendor/beaten","name":"Beaten","pricing":{"prompt":"0.000002","completion":"0.000008"}},
		{"id":"vendor/nodata","name":"NoData","pricing":{"prompt":"0.000002","completion":"0.000008"}}
	]}`)
	_, priced, _, err := FetchDiscounts(t.Context(), hc, data, thursday(t))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// every model with endpoint data, discounted or not; nodata has none
	if len(priced) != 2 || priced[0].ModelID != "vendor/beaten" || priced[1].ModelID != "vendor/promo" {
		t.Fatalf("priced = %+v, want [vendor/beaten vendor/promo]", priced)
	}
	if priced[0].Provider != "List" || priced[0].Pct != 0 {
		t.Errorf("beaten = %+v, want the undiscounted List offer, Pct 0", priced[0])
	}
	if priced[1].Provider != "Cheap" || priced[1].Pct >= 0 {
		t.Errorf("promo = %+v, want the discounted Cheap offer", priced[1])
	}
}
