# openrouter-discounter

Daily Telegram report of discounted OpenRouter models, ranked per category
(math, long-context, finance, coding) by free benchmark and popularity
scores. Runs on GitHub Actions daily at 07:00 JST; presets are also
repointed every 2 hours in between (no report).

```text
tracker (GitHub Actions, daily 07:00 JST; presets-only every 2 h)
  │
  │ GET  openrouter.ai/api/v1/models                         ← catalog, base prices, AA Intelligence Index
  │ GET  openrouter.ai/api/v1/models/{slug}/endpoints        ← per-provider pricing → discounts
  │ GET  epoch.ai/data/benchmark_data.zip                    ← math, code scores
  │ GET  artificialanalysis.ai/api/v2/language/models/free   ← finance score
  │ GET  openrouter.ai/api/frontend/v1/rankings/models       ← popularity (long-context)
  │ GET  api.darkbloom.dev/v1/models                         ← Darkbloom-direct prices (optional)
  ▼
rank top 10 per category (math, long-context, finance, code)
  │
  ▼
Telegram ◄── POST api.telegram.org/bot…/sendMessage
presets  ◄── POST openrouter.ai/api/v1/presets/{slug}/chat/completions
```

## Setup

Scheduled workflows are disabled by default on forks — use your own repo,
not a fork, or re-enable the daily workflow under Actions after forking.

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
   - `DARKBLOOM_API_KEY` — key from console.darkbloom.dev. Optional; adds
     the Darkbloom-direct price table to the report (see Darkbloom below).
3. Actions → daily → Run workflow.
4. Expect the report around 07:00 JST, ±15 min: GitHub schedules cron runs
   under load, so the delivery can be delayed but never skipped; the
   fixed-UTC crons are exact in JST (no DST).

## Scoring

Each category has one score per model, 0–100:

| category | source | scale |
| --- | --- | --- |
| math | Epoch AI — OTIS Mock AIME (`mean_score`) | fraction × 100 |
| long-context | OpenRouter popularity — tokens over the rankings window | log10, min–max |
| finance | Artificial Analysis — Finance & Accounting index (free tier) | as published |
| code, code-free | Epoch AI — WebDev Arena (`Arena Score`, Elo) | min–max over the file |

Ranking:

```text
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

```text
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
| --- | --- | --- |
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
deepest discount first. `strongest-discount-half-light` routes to the top-3
discounted chat models with a catalog AA Intelligence Index, ranked by the
same composite equation at the persona pair — price 80 / quality 20
(`personaPricePercent` / `personaPerformancePercent` in weights.go); the
global pair stays 50/50 for the report, `expensive-long`, `admech-*` and
hermes-*. All four Disco Elysium persona presets (strongest-discount-*) share
that top-3; `auto-shivers` is not managed by this repo. It carries the HALF-LIGHT persona
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
| --- | --- | --- | --- | --- |
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

## Darkbloom

[Darkbloom](https://docs.darkbloom.dev) (prepaid credits, OpenAI-compatible,
shared Apple-silicon Macs) is priced twice:

- **On OpenRouter** — its `darkbloom/*` endpoints are ordinary OpenRouter
  offers, billed to OpenRouter credits, pinned in the OpenRouter presets.
- **Direct** — `api.darkbloom.dev`, billed to Darkbloom credits. One
  `GET /v1/models` gives prices, context, max output, image input, tool
  support and live capacity; models no Mac can take right now are skipped.
  Each model is mapped to its OpenRouter id (Hugging Face id, then name
  with build suffixes like `-4bit`/`-mtp` dropped) and uses that model's
  benchmark scores.

Darkbloom lists no discounts, so a Darkbloom offer counts as discounted when
it undercuts every other provider of the model: `was` is the cheapest other
provider. Its fp4/int4 endpoints are exempt from the ≤4-bit filter.

Every preset is selected twice over the same scores: the OpenRouter side
(top 3 → the OpenRouter preset, as before) and the Darkbloom-direct side
(top 3, by the same rule). Darkbloom stores no presets and has no model
fallback (`models` is ignored; one `model` per request), so the direct side
is served by a local proxy:

```sh
DARKBLOOM_API_KEY=sk-db-… AA_API_KEY=… go run ./cmd/dbproxy   # 127.0.0.1:8787
```

Point the client's OpenAI base URL at `http://127.0.0.1:8787/v1`, keep the
model names `@preset/<slug>` and your OpenRouter key as the API key (it is
forwarded to OpenRouter only). For Hermes Agent, use a custom provider
with that `base_url` in place of `provider: openrouter`. Per request:

- The side whose #1 costs less (blended per Mtok under the preset's
  workload) goes first; a tie goes to Darkbloom (prepaid, no OpenRouter fee).
- Darkbloom: the proxy injects the preset's system prompt (merged before the
  client's own system message) and parameters the client did not send
  (`min_p` dropped: Darkbloom does not take it), passes `tools` and messages
  untouched, and tries the 3 models in order.
- OpenRouter: `@preset/<slug>` is forwarded unchanged; OpenRouter applies
  the preset and its own 3-model fallback.
- Any error before the response starts (non-2xx, error object, SSE error
  event, no response headers within 10 min) moves to the next model, then to
  the other side; nothing is retried once output has started. The
  `X-Dbproxy-Route` response header names the backend used.

Picks are recomputed at start (about a minute; until then every request goes
to OpenRouter) and every `DBPROXY_REFRESH` (default 2 h), held in memory
only: each run builds a new route table and swaps it in whole, so requests
served during a refresh keep the previous table and a failed run leaves it
unchanged. `OPENROUTER_BASE_URL`, `DARKBLOOM_BASE_URL`, `DBPROXY_ADDR` and
`DBPROXY_REFRESH` override the production defaults (staging, tests); the
local services are covered in [Local pi-subagents
integration](#local-pi-subagents-integration). Tool calls, streamed tool
calls and tool results work on Darkbloom (verified 2026-11-27). The
OpenRouter web chat sees only the OpenRouter side.

## Local pi-subagents integration

The same selection that keeps the OpenRouter presets current also drives a
local pi agent and its pi-subagents. The tracker itself can rewrite the
agent's `models.json` and `settings.json` after its preset updates, or
`cmd/pipresets` can do it as a one-off; `cmd/dbproxy` (see Darkbloom above)
serves the Darkbloom-direct picks on loopback. All of it is local and
opt-in: the GitHub Actions run emits no pi config (the tracker's emit is
off unless asked for), so it needs no agent dir, no `$HOME`, and writes
nothing outside its own checkout.

### Presets for pi

The tracker's emit is one gate: it runs only when `--pi-dir <dir>` or a
non-empty `TRACKER_PIPRESETS` names the agent dir, and it runs after the
preset updates (`TRACKER_REPORT=false` reruns included). With neither set
nothing is written and no home dir is looked up, which is what CI relies
on. `pipresets` is the standalone equivalent:

```sh
TRACKER_PIPRESETS="$HOME/.pi/agent" go run ./cmd/tracker   # report + presets + pi config
go run ./cmd/tracker --pi-dir "$HOME/.pi/agent"            # same emit, flag form
go run ./cmd/pipresets --dry-run   # print the changes; write nothing
go run ./cmd/pipresets             # write models.json and settings.json
```

A failing emit exits the tracker nonzero (joined with any preset failure)
but never costs the report: it runs after the Telegram send.

For each managed slug the emit adds a `@preset/<slug>` model under the
`openrouter` provider and the same id under the loopback `dbproxy` provider,
with the cost derived from the winning row, then sets
`subagents.agentOverrides` per builtin pi-subagents agent to
`dbproxy/@preset/<slug>` or `openrouter/@preset/<slug>`, whichever surface's
top row is cheaper. Merge-writes keep unmanaged keys, files are written
atomically and skipped when their content is unchanged, and no secret is
written: the emitted key is the `$OPENROUTER_API_KEY` environment
interpolation.

Managed slugs (`internal/preset.ManagedSlugs`) — the only slugs the emit
ever writes or points an agent at:

| slug | pi-subagents agent |
| --- | --- |
| `admech-code` | worker |
| `admech-code-oracle` | oracle |
| `admech-code-reviewer` | reviewer |
| `admech-code-researcher` | researcher |
| `admech-code-auditor` | evidence-auditor |
| `admech-code-delegate` | delegate |
| `admech-code-scout` | scout |
| `admech-code-vision` | — (companion looker, wire as an auxiliary like `hermes-vision`) |

The list is the guard as well: `preset.UpdateManaged` refuses a preset write
for any slug outside it (the daily tracker run still walks the ranked preset
sectors it reports).

### Environment keys

Secrets are read from the environment only; nothing is written down or
logged. `scripts/local-cron.sh` and the deploy units below rely on whatever
is exported in the invoking shell (systemd: the optional `EnvironmentFile`).

| variable | used by | default | meaning |
| --- | --- | --- | --- |
| `OPENROUTER_MANAGEMENT_KEY` | tracker | — | read-write management key; missing only skips the preset updates |
| `TRACKER_PIPRESETS` | tracker, local-cron | — | pi agent dir; unset (and no `--pi-dir`) skips the pi config emit |
| `AA_API_KEY` | tracker, pipresets, dbproxy | — | optional finance scores |
| `DARKBLOOM_API_KEY` | tracker, pipresets, dbproxy | — | required by dbproxy; optional elsewhere |
| `OPENROUTER_BASE_URL` | dbproxy | `https://openrouter.ai/api/v1` | upstream root |
| `DARKBLOOM_BASE_URL` | dbproxy | `https://api.darkbloom.dev/v1` | upstream root |
| `DBPROXY_ADDR` | dbproxy | `127.0.0.1:8787` | listen address; anything not loopback is refused |
| `DBPROXY_REFRESH` | dbproxy | `2h` | re-discovery interval (Go duration) |

`pipresets` (and the tracker's opt-in emit) writes the dbproxy provider as
`http://127.0.0.1:8787/v1`, so run the proxy on its default address; the
`DBPROXY_ADDR` / base-URL overrides are for staging and the e2e tests.

### The proxy as a user service

dbproxy holds the Darkbloom key and forwards the client's OpenRouter key, so
it binds loopback only and refuses any other `DBPROXY_ADDR`. Run it in your
own user session, not as a root or system-wide service. Linux, a transient
user unit that takes the keys from the invoking shell:

```sh
systemd-run --user --unit openrouter-dbproxy --working-directory "$PWD" \
  --setenv DARKBLOOM_API_KEY="$DARKBLOOM_API_KEY" \
  --setenv AA_API_KEY="$AA_API_KEY" go run ./cmd/dbproxy
```

macOS: keep it in a login shell (`DARKBLOOM_API_KEY=… go run ./cmd/dbproxy`)
or write an equivalent LaunchAgent of your own.

### Local cron every 2 h

`scripts/local-cron.sh` is the local stand-in for the GitHub presets rerun:
it runs the tracker with `TRACKER_REPORT=false` (presets only, no Telegram
secrets, no message) and `TRACKER_PIPRESETS` pointing at `~/.pi/agent`
(override it in the environment to write the config elsewhere). The tracker
is the only writer: one pipeline run repoints the OpenRouter presets and
emits the pi config, so the schedule runs no separate `pipresets` and fetches
the selection once. Everything goes to stderr, so a scheduler capture never
picks up a half-written report.

```sh
scripts/local-cron.sh              # presets + pi config, one tracker run
scripts/local-cron.sh --dry-run    # no pi config written; pipresets --dry-run prints the changes
```

`--dry-run` writes no pi config, and nothing at all under the agent dir: the
tracker runs with `TRACKER_PIPRESETS` unset (its emit off), then
`pipresets --dry-run --dir <dir>` prints the changes the emit would have made.
Preset repointing is not a dry run, here or in the tracker: the presets are
repointed either way.

macOS (launchd, `StartInterval` 7200 s, run at load):

```sh
sed -e "s#__REPO__#$PWD#g" -e "s#__HOME__#$HOME#g" \
  deploy/com.openrouter-discounter.plist \
  > ~/Library/LaunchAgents/com.openrouter-discounter.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.openrouter-discounter.plist
```

Linux (systemd user units, `OnUnitActiveSec=2h` after a 2 min first run):

```sh
mkdir -p ~/.config/systemd/user ~/.config/openrouter-discounter
sed "s#__REPO__#$PWD#g" deploy/openrouter-discounter.service \
  > ~/.config/systemd/user/openrouter-discounter.service
cp deploy/openrouter-discounter.timer ~/.config/systemd/user/
install -m 600 /dev/null ~/.config/openrouter-discounter/env   # KEY=VALUE, then fill in
systemctl --user daemon-reload
systemctl --user enable --now openrouter-discounter.timer
```

The timer's first run is 2 minutes after the user manager starts, then every
2 hours after the previous run finishes; the launchd agent runs at load and
every 2 hours. Plain cron works too:

```cron
0 */2 * * * $HOME/src/openrouter-discounter/scripts/local-cron.sh 2>>/tmp/openrouter-discounter.log
```

Refresh cadence: dbproxy re-discovers and swaps its routes every
`DBPROXY_REFRESH` (2 h); the local cron, launchd agent and systemd timer
repoint the OpenRouter presets and the pi config on the same 2 h cadence.
They are independent of the GitHub Actions schedule and safe to run
concurrently with it — every run recomputes from that moment's API prices
and persists nothing.

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
