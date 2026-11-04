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
final = 0.65 × score + 0.35 × (−Δ%)
```

The score counts ~2× the price. Prices are USD per Mtok prompt; Δ% is
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
composite ranking as the report (0.65 benchmark + 0.35 discount), scored on
the category's score. `expensive-long` routes the 3 strongest
thinking frontier models (catalog AA Intelligence Index, discount-resorted),
independent of discounts. `strongest-discount-half-light` routes to the top-3 strongest-thinking
(catalog AA Intelligence Index) discounted chat models, carries the HALF-LIGHT persona
prompt with reasoning effort max and verbosity low, and adds sampling settings
(temperature 1.15, top_p 0.95, frequency_penalty 0.4, presence_penalty 0.4) for more
varied, less mechanical replies than the other presets.
Unscored models are skipped; if none of a category's models scored, that
preset is left untouched.

Free presets (`admech-code-free`, `*-free` personas) skip `:free` endpoints
restricted to agentic harnesses, which fail in the OpenRouter web chat. The
API exposes no flag for this; the denylist `harnessOnly` in
`internal/rank/rank.go` is curated from each model page's warning
("only available for use with agentic harnesses") and needs a manual update
when OpenRouter adds such endpoints.

Each update POSTs the new model list plus the shared system prompt and
`verbosity: low` to `api/v1/presets/{slug}/chat/completions`, which creates
a new server-side designated version — nothing is stored or committed here.
A preset whose designated version already matches is not re-posted. Failures
and missing `OPENROUTER_API_KEY` are logged and never block the Telegram
report.

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
