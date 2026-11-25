# openrouter-discounter

Daily Telegram report of discounted OpenRouter models, ranked per category
(math, long-context, finance, coding) by free benchmark and popularity
scores. Runs on GitHub Actions daily at 07:00 JST; presets are also
repointed every 2 hours in between (no report).

```
tracker (GitHub Actions, daily 07:00 JST; presets-only every 2 h)
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
   - `OPENROUTER_MANAGEMENT_KEY` — a read-write **management** key from
     openrouter.ai/settings/management-keys; it both reads and writes
     presets (an inference key from openrouter.ai/keys gets
     `401 Invalid management key` on every write). Optional; updates
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
final = performance% × score + price% × (−Δ%)
```

Weights live in `internal/rank/weights.go` (`pricePercent`,
`performancePercent`, summing to 100). Top 10 per category are sent.

Prices are blended USD per Mtok for one agent request — 30k input tokens,
80% served from the prompt cache, 1.5k output (half reasoning) — averaged
over the 4 hours the run's picks stay pinned (`discounts.Horizon`;
presets are repointed every 2 h, so one missed run is covered). Cache reads bill at
`input_cache_read` only on endpoints with implicit caching (no
`cache_control` breakpoints are assumed; flip `discounts.ExplicitCaching`
if your client sends them), misses at `max(prompt, input_cache_write)`,
reasoning at `internal_reasoning` when listed. Δ% is the change vs the
provider's list price. A provider's `discount` and `pricing.overrides` time
windows (`utc_start`/`utc_end`, HHMM UTC, may wrap midnight; optional
`utc_days`) both count: a window covering 2 of the next 4 hours at half
price shows as −25%. The run logs every window discount with its hours.

Each model is priced at its single cheapest endpoint by that blended
price, and the `provider` column names it. A model counts as discounted only
when that endpoint is discounted: a discount undercut by an undiscounted
provider is no saving. Flex service-tier endpoints (`…/flex`: cheaper, but
queued or rejected under load) are never used. Endpoints at ≤4-bit
quantization are skipped when another provider serves the model at higher
precision; uptime is not filtered. `:batch` variants (asynchronous jobs) are
not reported.
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

Paid presets pin each model to its cheapest endpoint: `provider.only` lists
the endpoint tags (`gmicloud/fp8`) with `allow_fallbacks: false`, so a failed
request falls through to the next of the 3 models, never to another
provider. `provider.only` applies to the whole request, so a model is passed
over for the next-ranked one when one of its endpoints is another picked
model's pin (it could route there). `provider.max_price` caps prompt and
completion at the highest rate any pinned endpoint bills within the 24
hours (+1%), so a discount ending early sends requests to the next model
instead of billing list price; the cap is shared, so it only protects
models pinned below the most expensive one. Free presets route unpinned.

Each preset re-picks every model's endpoint for its own needs: the
cheapest endpoint that supports what the preset sends, priced under the
preset's request shape. Tool support (`tools` listed and `tool_choice: auto`
not refused) and context length are checked per endpoint, not per model.

| preset | endpoint must | request shape (in / cached / out / reasoning) |
|---|---|---|
| admech-* (report tables) | — | 30k / 80% / 1.5k / 50% |
| expensive-long | fit 304k tokens | 300k / 80% / 4k / 50% |
| strongest-discount-* | call tools | 8k / 70% / 3k / 60% |
| hermes-delegate | call tools | 40k / 85% / 2k / 50% |
| hermes-review | call tools | 60k / 50% / 1.5k / 50% |
| hermes-compress | hold 1M tokens | 200k / 10% / 4k / 10% |
| hermes-vision | — | 5k / 0% / 0.5k / 0% |
| hermes-light | — | 2k / 0% / 0.2k / 0% |

Shapes live in `internal/rank/needs.go`; tune them to observed usage.

With `OPENROUTER_MANAGEMENT_KEY` set, each run also repoints the OpenRouter presets
`admech-math`, `admech-long`, `admech-finance`, `admech-code`, and
`admech-code-free` at the 3 best of the category's top-10 table — the same
composite ranking as the report (weights.go), scored on
the category's score. `expensive-long` routes the 3 strongest discounted
thinking models (catalog AA Intelligence Index, no price ceilings), ordered
deepest discount first. `strongest-discount-half-light` routes to the top-3 strongest-thinking
(catalog AA Intelligence Index) discounted chat models, carries the HALF-LIGHT persona
prompt with reasoning effort high and verbosity low, and adds sampling settings
(temperature 1.0, top_p 0.95, min_p 0.05, frequency_penalty 0,
presence_penalty 0.1, max_tokens 65536); the other strongest-discount-*
presets share them. Hermes'
main model sends no max_tokens, so 65536 must fit long tool calls (file
writes); the pinned endpoints must allow it (`max_completion_tokens`). Frequency penalty stays 0 and temperature
at 1: higher values (previously 1.15 / 0.4) penalize every common token over a
long reply, reasoning included, and the output collapses into word salad that
then poisons the rest of the session. min_p cuts the low-probability tail that
salad is drawn from; max_tokens bounds a runaway reply. Every persona prompt
states that the persona shapes tone only.
Unscored models are skipped; if none of a category's models scored, that
preset is left untouched.

Free presets (`admech-code-free`) skip `:free` endpoints
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
A preset whose designated version already matches is not re-posted. Presets
update after the Telegram send, so they never block the report. Every preset
is attempted; any failure (e.g. `401 Invalid management key`) is logged and
then exits the run nonzero, so the CI job goes red. A missing
`OPENROUTER_MANAGEMENT_KEY` only skips the updates.

## Hermes role presets

Five presets carry one [Hermes Agent](https://github.com/NousResearch/hermes-agent)
task slot each. Hermes points at them once and follows the daily update with
no local cron. The main (talking) model is not managed here; point
`model.default` at any other preset. None of the task slots talks to the
user, so the presets send no prompt and summaries, titles, verdicts and
skill files stay unstyled. No verbosity and no sampling overrides (tool
calls need default temperature). The 3 listed models double as OpenRouter
fallback order.

| preset | Hermes slot | filter | quality | effort |
|---|---|---|---|---|
| `hermes-delegate` | `delegation.model` | tools | Epoch WebDev Arena (≥ 3 scored), else II | medium |
| `hermes-vision` | `auxiliary.vision` | image input | II | none |
| `hermes-compress` | `auxiliary.compression` | ctx ≥ 1M | II | low |
| `hermes-review` | `auxiliary.background_review`, `curator` | tools | 80% II + 20% cheapness | medium |
| `hermes-light` | `auxiliary.title_generation`, `session_search`, `approval`, `goal_judge` | II ≥ 20, reasoning not mandatory | cheapness `100·(1 − blended/ceiling)` | none |

Pool: no `:free`/`:batch`, positive catalog II, within the price ceilings
(≤ $0.5 prompt / $1 completion per Mtok). Delegate, vision and compress draw
from discounted rows and rank by performance% × quality + price% × (−Δ%).
Review and light rank by absolute cost, so they draw from every priced
model, discounted or not: a list-price model that costs less beats a
pricier discounted one. Quality is min–max scaled within each role;
cheapness is scaled to the ceiling (the blended price of $0.5/$1 uncached
under the role's request shape); prices are the role's re-pick (see the
table above). Roles pick in table order; a model already #1 in an earlier
role loses 15 points, so it repeats only when clearly best — unless its
cheapness is ≥ 80 (blended ≤ 20% of the ceiling), then it may serve any
number of roles. Hermes' own per-request `reasoning_effort` overrides the
preset effort.

Discount end: a promo `pricing.discount` carries no end time, so it can
only be detected by rescanning. Each preset's `provider.max_price` caps
requests at the pinned endpoints' peak rate, so an ended discount never
bills list price — that model is skipped for the next one in the preset.
The presets-only reruns every 2 hours (`TRACKER_REPORT=false`, no Telegram)
then replace it. Scheduled time windows (`pricing.overrides`) are known in
advance and priced over the 4 h Horizon.

`~/.hermes/config.yaml`:

```yaml
model:
  provider: openrouter
  default: "@preset/<your main preset>"
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

GitHub disables scheduled workflows after 60 days without repository
activity. The `keepalive` job (`liskin/gh-workflow-keepalive@v1`,
`actions: write`) re-enables the workflow via the API on scheduled runs —
no dummy commits.
