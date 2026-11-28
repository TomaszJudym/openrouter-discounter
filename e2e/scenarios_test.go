package e2e

// The suite proves the two routing surfaces the repo emits end to end:
// cmd/pipresets writes models.json/settings.json from live stub prices, and
// cmd/dbproxy serves the same presets over loopback, forwarding each
// @preset/<slug> request to whichever side wins on price. Scenario A prices
// Darkbloom below OpenRouter, B above it, C flips the price under a running
// proxy and watches the route table follow.
//
// The headless pi check in each scenario is opt-in: set E2E_PI=1 (and have
// pi plus pi-subagents installed) to run one real pi turn through the
// emitted settings. Without it every Go-side assertion still runs.

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const routeWait = 30 * time.Second

func TestDarkbloomWinsRoutesThroughProxy(t *testing.T) {
	t.Parallel()
	s := newScenario(t, orExpensive, dbDirect)
	s.runPipresets()
	checkSurfaces(t, s, "dbproxy")

	p := s.startProxy("1h")
	if route := s.waitRoute(p, presetOracle, "darkbloom/", routeWait); route != "darkbloom/"+dbModelID {
		t.Errorf("route = %q, want darkbloom/%s", route, dbModelID)
	}
	if calls := s.db.chatsSnapshot(); !hasModel(calls, dbModelID) {
		t.Errorf("Darkbloom stub saw %v, want a completion for %s", calls, dbModelID)
	}
	if calls := s.or.chatsSnapshot(); len(calls) != 0 {
		t.Errorf("OpenRouter stub served %d completions, want none: %v", len(calls), calls)
	}

	t.Run("pi", func(t *testing.T) {
		if os.Getenv("E2E_PI") != "1" {
			t.Skip("set E2E_PI=1 to run the headless pi routing check")
		}
		s.patchForPi("dbproxy", p.url)
		before := len(s.db.chatsSnapshot())
		events := s.runPi(delegateMarker + ": ask the oracle subagent to reply with one word.")
		if !subagentRan(events) {
			t.Fatalf("pi did not run the oracle subagent: %v", events)
		}
		if answer := subagentResult(events); !strings.Contains(answer, "hello from "+dbModelID) {
			t.Errorf("oracle answered %q, want Darkbloom's %s to have served it", answer, dbModelID)
		}
		if used := assistantProviders(events); slices.Contains(used, "openrouter") {
			t.Errorf("pi used the openrouter surface %v, want only dbproxy", used)
		}
		if calls := s.db.chatsSnapshot()[before:]; len(calls) == 0 {
			t.Errorf("dbproxy forwarded no completion to Darkbloom during the pi run")
		}
		if calls := s.or.chatsSnapshot(); len(calls) != 0 {
			t.Errorf("OpenRouter stub served %d completions during the pi run, want none", len(calls))
		}
	})
}

func TestOpenRouterWinsWhenCheaper(t *testing.T) {
	t.Parallel()
	s := newScenario(t, orCheap, dbDirect)
	s.runPipresets()
	checkSurfaces(t, s, "openrouter")

	// The byDiscount roles (oracle, reviewer, researcher) have no Darkbloom
	// row at all once Darkbloom is dearer; the byCost roles keep a dearer
	// row and must still pick OpenRouter — the surface rule's comparison
	// runs either way.
	p := s.startProxy("1h")
	if route := s.waitRoute(p, presetOracle, "openrouter", routeWait); route != "openrouter" {
		t.Errorf("route = %q, want openrouter", route)
	}
	if calls := s.or.chatsSnapshot(); !hasModel(calls, presetOracle) {
		t.Errorf("OpenRouter stub saw %v, want a completion for %s", calls, presetOracle)
	}
	if calls := s.db.chatsSnapshot(); len(calls) != 0 {
		t.Errorf("Darkbloom stub served %d completions, want none: %v", len(calls), calls)
	}

	t.Run("pi", func(t *testing.T) {
		if os.Getenv("E2E_PI") != "1" {
			t.Skip("set E2E_PI=1 to run the headless pi routing check")
		}
		s.patchForPi("openrouter", p.url)
		events := s.runPi(delegateMarker + ": ask the oracle subagent to reply with one word.")
		if !subagentRan(events) {
			t.Fatalf("pi did not run the oracle subagent: %v", events)
		}
		if answer := subagentResult(events); !strings.Contains(answer, "hello from "+presetOracle) {
			t.Errorf("oracle answered %q, want the OpenRouter stub's %s to have served it", answer, presetOracle)
		}
		if !assistantUsed(events, "openrouter", presetWorker) {
			t.Errorf("no parent turn from openrouter/%s; providers seen: %v", presetWorker, assistantProviders(events))
		}
		if calls := s.db.chatsSnapshot(); len(calls) != 0 {
			t.Errorf("dbproxy forwarded %d completions to Darkbloom during the pi run, want none", len(calls))
		}
	})
}

func TestProxyRefreshesRoutes(t *testing.T) {
	t.Parallel()
	s := newScenario(t, orExpensive, dbDirect)
	p := s.startProxy("1s")
	if route := s.waitRoute(p, presetOracle, "darkbloom/", routeWait); route != "darkbloom/"+dbModelID {
		t.Fatalf("initial route = %q, want darkbloom/%s", route, dbModelID)
	}
	orBefore := len(s.or.chatsSnapshot())

	// Flip OpenRouter below Darkbloom; the running proxy's next refresh
	// must rebuild its table and prefer OpenRouter.
	s.or.setPrice(orCheap)
	if route := s.waitRoute(p, presetOracle, "openrouter", routeWait); route != "openrouter" {
		t.Fatalf("route after the price flip = %q, want openrouter", route)
	}
	if orBefore != 0 {
		t.Errorf("OpenRouter served %d completions before the flip, want none (Darkbloom was cheaper)", orBefore)
	}
	if calls := s.or.chatsSnapshot(); !hasModel(calls, presetOracle) {
		t.Errorf("OpenRouter stub saw %v after the flip, want %s", calls, presetOracle)
	}

	// Flip back: the table follows in both directions.
	s.or.setPrice(orExpensive)
	if route := s.waitRoute(p, presetOracle, "darkbloom/", routeWait); route != "darkbloom/"+dbModelID {
		t.Fatalf("route after the second flip = %q, want darkbloom/%s", route, dbModelID)
	}
}

// checkSurfaces asserts the emitted config for a run where surface wins:
// both providers carry the managed ids, the base URLs are the documented
// production ones, and every overridden agent points at that surface.
func checkSurfaces(t *testing.T, s *scenario, surface string) {
	t.Helper()
	for _, provider := range []string{"openrouter", "dbproxy"} {
		if ids := s.providerModelIDs(provider); !slices.Contains(ids, presetOracle) || !slices.Contains(ids, presetWorker) {
			t.Errorf("%s models = %v, want %s and %s", provider, ids, presetOracle, presetWorker)
		}
	}
	if got := s.provider("openrouter")["baseUrl"]; got != "https://openrouter.ai/api/v1" {
		t.Errorf("openrouter baseUrl = %v, want the production root", got)
	}
	if got := s.provider("dbproxy")["baseUrl"]; got != "http://127.0.0.1:8787/v1" {
		t.Errorf("dbproxy baseUrl = %v, want the loopback preset proxy", got)
	}
	for _, agent := range []string{"worker", "oracle", "reviewer", "researcher", "evidence-auditor", "delegate", "scout"} {
		if got, want := s.agentOverride(agent), surface+"/"+presetSlug(agent); got != want {
			t.Errorf("agentOverrides.%s.model = %q, want %q (%s wins on price)", agent, got, want, surface)
		}
	}
}

// presetSlug maps a pi-subagents agent to its managed preset slug, mirroring
// internal/piconfig's agentFor table.
func presetSlug(agent string) string {
	slug := map[string]string{
		"worker":           presetWorker,
		"oracle":           presetOracle,
		"reviewer":         "@preset/admech-code-reviewer",
		"researcher":       "@preset/admech-code-researcher",
		"evidence-auditor": "@preset/admech-code-auditor",
		"delegate":         "@preset/admech-code-delegate",
		"scout":            "@preset/admech-code-scout",
	}[agent]
	if slug == "" {
		panic("unknown agent " + agent)
	}
	return slug
}

func hasModel(calls []chatCall, model string) bool {
	return slices.ContainsFunc(calls, func(c chatCall) bool { return c.Model == model })
}

// subagentResult is the text the oracle tool returned to the parent: the
// child's answer, carried back through dbproxy or OpenRouter.
func subagentResult(events []piEvent) string {
	for _, ev := range events {
		if ev.Type != "tool_execution_end" || ev.ToolName != "subagent" || ev.IsError {
			continue
		}
		for _, c := range ev.Result.Content {
			return c.Text
		}
	}
	return ""
}

func subagentRan(events []piEvent) bool {
	for _, ev := range events {
		if ev.Type == "tool_execution_end" && ev.ToolName == "subagent" && !ev.IsError {
			return true
		}
	}
	return false
}

func assistantUsed(events []piEvent, provider, model string) bool {
	for _, ev := range events {
		if ev.Type != "message_end" || ev.Message.Role != "assistant" {
			continue
		}
		if ev.Message.Provider == provider && ev.Message.Model == model {
			return true
		}
	}
	return false
}

func assistantProviders(events []piEvent) []string {
	seen := make([]string, 0, 4)
	for _, ev := range events {
		if ev.Type != "message_end" || ev.Message.Role != "assistant" {
			continue
		}
		seen = append(seen, ev.Message.Provider+"/"+ev.Message.Model)
	}
	return seen
}
