package main

import (
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

func TestIntelligenceVariantsInheritBase(t *testing.T) {
	mk := func(id string, ii float64) discounts.Model {
		m := discounts.Model{ID: id}
		m.Benchmarks.ArtificialAnalysis.IntelligenceIndex = ii
		return m
	}
	got := intelligence([]discounts.Model{
		mk("v/a", 50), mk("v/a:free", 0), mk("v/a:thinking", 0), mk("v/b:free", 30), mk("v/c", 0),
	})
	want := map[string]float64{"v/a": 50, "v/a:free": 50, "v/b:free": 30}
	if len(got) != len(want) {
		t.Errorf("intelligence = %v, want %v", got, want)
	}
	for id, v := range want {
		if got[id] != v {
			t.Errorf("intelligence[%s] = %v, want %v", id, got[id], v)
		}
	}
}
