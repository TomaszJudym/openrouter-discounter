package discounts

import (
	"encoding/json/v2"
	"errors"
	"net/http"
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
	got, free := Detect(data, thursday(t))
	if len(free) != 0 {
		t.Errorf("Detect() free = %v, want none", free)
	}
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

func floatEq(a, b float64) bool { return absF(a-b) < 1e-9 }

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
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
	got, free := Detect(data, thursday(t))
	if len(got) != 0 {
		t.Errorf("Detect() discounts = %+v, want none (volume tiers have no time window)", got)
	}
	if len(free) != 0 {
		t.Errorf("Detect() free = %v, want none", free)
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
	got, _ := Detect(data, thursday(t))
	if len(got) != 1 || got[0].Price != 1.2 {
		t.Fatalf("Detect() = %+v, want price 1.2/Mtok (cheapest Thursday bucket)", got)
	}
}

func TestDetectFreeTier(t *testing.T) {
	data := mustParse(t, `{"data":[
		{"id":"openai/gpt-oss-120b:free","name":"Free Model","pricing":{"prompt":"0","completion":"0"}},
		{"id":"openai/gpt-oss-120b","name":"Paid Model","pricing":{"prompt":"0.0000020","completion":"0.0000100"}}
	]}`)
	got, free := Detect(data, thursday(t))
	if len(free) != 1 || free[0] != "openai/gpt-oss-120b:free" {
		t.Errorf("free = %v, want [openai/gpt-oss-120b:free]", free)
	}
	if len(got) != 0 {
		t.Errorf("discounts = %+v, want none", got)
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
	if got := minTodayPrice([]Override{{Days: []string{"THURSDAY"}, Prompt: "0.0000010"}}, "thursday"); got != 0.000001 {
		t.Errorf("minTodayPrice() = %v, want 0.000001", got)
	}
}
