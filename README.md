# openrouter-discounter

Daily Telegram report of discounted OpenRouter models, ranked per category
(math, long-context, finance, coding) by free benchmark and popularity
scores. Runs on GitHub Actions daily at 07:00 JST.

```
tracker (GitHub Actions, daily 07:00 JST)
  │
  │ GET  openrouter.ai/api/v1/models                         ← catalog, base prices, AA Intelligence Index
  │ GET  openrouter.ai/api/v1/models/{slug}/endpoints        ← per-provider pricing → discounts
  │ GET  epoch.ai/data/benchmark_data.zip                    ← math, code scores
  │ GET  artificialanalysis.ai/api/v2/language/models/free   ← finance score
  │ GET  openrouter.ai/api/frontend/v1/rankings/models       ← popularity (long-context)
  ▼
rank top 10 per category (math, long-context, finance, code)
  │
  ▼
Telegram ◄── POST api.telegram.org/bot…/sendMessage
presets  ◄── POST openrouter.ai/api/v1/presets/{slug}/chat/completions
```

## Setup

1. Create a bot with @BotFather, add it to your channel as admin.
2. Enable Actions on this repo, then add secrets (Settings → Secrets and
   variables → Actions):
   - `TELEGRAM_BOT_TOKEN` — bot token from @BotFather
   - `TELEGRAM_CHANNEL_ID` — `@name` or `-100…`
   - `AA_API_KEY` — free key from artificialanalysis.ai (Insights Platform →
     API key). Optional; without it, the finance category ranks by discount
     only. The free tier is enough (the `/free` model list).
   - `OPENROUTER_API_KEY` — key from openrouter.ai/keys. Optional; updates
     the `admech-math` / `admech-long` / `admech-finance` / `admech-code`
     / `admech-code-free` / `expensive-long` presets with each category's
     top 3.
3. Actions → daily → Run workflow.

## Scoring

Each category has one score per model, 0–100:

| category | source | scale |
|---|---|---|
| math | Epoch AI — OTIS Mock AIME (`mean_score`) | fraction × 100 |
| long-context | OpenRouter popularity — tokens over the rankings window | log10, min–max |
| finance | Artificial Analysis — Finance & Accounting index (free tier) | as published |
| code, code-free | Epoch AI — WebDev Arena (`Arena Score`, Elo) | min–max over the file |

Ranking:

```
final = 0.75 × score + 0.25 × (−Δ%)
```

Score weighs 75%, discount 25%. Prices are USD per Mtok prompt; Δ% is
the discount vs the provider's list price. Top 10 per category are sent.
Unscored models rank last, by discount. A model keeps its best score across
reasoning-effort variants; a `:free` id shares its base model's benchmark
score, other variants (`:batch`) stay unscored.

Epoch data is CC BY (epoch.ai, no key). The OpenRouter rankings feed is
undocumented; if it changes, only the long-context category degrades. AA's
Coding and Agentic indexes are discontinued, and its per-benchmark fields
(`aa_lcr`, `tau_banking`, `scicode`) are Pro-only.

If a category's source fails, that category ranks by discount only and the
report shows, under its header:

```
⚠️ scored only by price — Epoch AI error: status 503: …
```

The error text has the AA key removed and is truncated to 120 chars.

## Preset updates

With `OPENROUTER_API_KEY` set, each run also repoints the OpenRouter presets
`admech-math`, `admech-long`, `admech-finance`, `admech-code`, and
`admech-code-free` at the 3 best of the category's top-10 table — the same
composite ranking as the report (0.75 benchmark + 0.25 discount), scored on
the category's score. `expensive-long` routes the 3 strongest discounted
thinking models (catalog AA Intelligence Index, no price ceilings), ordered
deepest discount first. `strongest-discount-half-light` routes to the top-3 strongest-thinking
(catalog AA Intelligence Index) discounted chat models, carries the HALF-LIGHT persona
prompt with reasoning effort max and verbosity low, and adds sampling settings
(temperature 1.15, top_p 0.95, frequency_penalty 0.4, presence_penalty 0.4) for more
varied, less mechanical replies than the other presets.
Unscored models are skipped; if none of a category's models scored, that
preset is left untouched.

Free presets (`admech-code-free`, `halflight-free`) skip `:free` endpoints
restricted to agentic harnesses, which fail in the OpenRouter web chat. The
API exposes no flag for this; the denylist `harnessOnly` in
`internal/rank/rank.go` is curated from each model page's warning
("only available for use with agentic harnesses") and needs a manual update
when OpenRouter adds such endpoints. They also skip vision models (image or
video input) and models with non-text output, and put tool-calling models
(`tools` in `supported_parameters`) ahead of the rest.

`strongest-discount-*` presets require tool calling: a model qualifies only
when the catalog lists `tools` in its `supported_parameters`.

Each update POSTs the new model list plus the shared system prompt and
`verbosity: low` to `api/v1/presets/{slug}/chat/completions`, which creates
a new server-side designated version — nothing is stored or committed here.
A preset whose designated version already matches is not re-posted. Failures
and missing `OPENROUTER_API_KEY` are logged and never block the Telegram
report.

## Hermes role presets

Six presets carry one [Hermes Agent](https://github.com/NousResearch/hermes-agent)
model slot each. Hermes points at them once and follows the daily update with
no local cron. Only `hermes-main` talks to the user, so only it carries a
system prompt (HALF-LIGHT persona); OpenRouter merges it with the one Hermes
sends. The five task presets send no prompt, so summaries, titles, verdicts
and skill files stay unstyled. No verbosity and no sampling overrides (tool
calls need default temperature). The 3 listed models double as OpenRouter
fallback order.

| preset | Hermes slot | filter | quality | effort |
|---|---|---|---|---|
| `hermes-main` | `model.default` | tools, thinking, ctx ≥ 256k | AA Intelligence Index | high |
| `hermes-delegate` | `delegation.model` | tools | Epoch WebDev Arena (≥ 3 scored), else II | medium |
| `hermes-vision` | `auxiliary.vision` | image input | II | none |
| `hermes-compress` | `auxiliary.compression` | ctx ≥ 1M | II | low |
| `hermes-review` | `auxiliary.background_review`, `curator` | tools | II | medium |
| `hermes-light` | `auxiliary.title_generation`, `session_search`, `approval`, `goal_judge` | II ≥ 20 | cheapness `100·(1 − blended/0.6)` | none |

Pool: affordable discounted rows (≤ $0.5 prompt / $1 completion per Mtok),
no `:free`/`:batch`, positive catalog II. Quality is min–max scaled within
each role (except cheapness, scaled to the ceiling); blended price is
0.8 × prompt + 0.2 × completion. Rank = 0.75 × quality + 0.25 × (−Δ%).
Roles pick in table order; a model already #1 in an earlier role loses 15
points, so it repeats only when clearly best. Hermes' own per-request
`reasoning_effort` overrides the preset effort.

`~/.hermes/config.yaml`:

```yaml
model:
  provider: openrouter
  default: "@preset/hermes-main"
  context_length: 256000   # preset names defeat auto-detection
delegation:
  provider: openrouter
  model: "@preset/hermes-delegate"
auxiliary:
  vision:            { provider: openrouter, model: "@preset/hermes-vision" }
  compression:       { provider: openrouter, model: "@preset/hermes-compress" }
  background_review: { provider: openrouter, model: "@preset/hermes-review" }
  curator:           { provider: openrouter, model: "@preset/hermes-review" }
  title_generation:  { provider: openrouter, model: "@preset/hermes-light" }
  session_search:    { provider: openrouter, model: "@preset/hermes-light" }
  approval:          { provider: openrouter, model: "@preset/hermes-light" }
  goal_judge:        { provider: openrouter, model: "@preset/hermes-light" }
```

## Development

```sh
just      # build, vet, race-test, lint (golangci-lint, config in .golangci.yml)
just test # build, vet, race-test only
just fmt  # format in place
```

`justfile` sets `GOTOOLCHAIN=auto`, so a system Go older than the `go` line
in `go.mod` downloads the pinned toolchain on first run. Plain `go` commands
work too, if `GOTOOLCHAIN` allows.

## Keepalive

GitHub disables scheduled workflows after 60 days without commits; a separate
job re-enables the workflow via API — no dummy commits.
