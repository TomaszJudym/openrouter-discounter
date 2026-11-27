# Task: OpenRouter Discount Tracker — GitHub Actions Cron + Go + Telegram

Build a fully working system, not pseudocode. Commit-ready code, config files, setup docs. No server, no public endpoints, no storage — one repo, stateless daily run.

## Architecture

- Single workflow `.github/workflows/daily.yml` with four triggers: `schedule: 0 22 * * *` (UTC) = 07:00 Asia/Tokyo daily (report + presets); `schedule: 7 0-20/2 * * *` — every 2 h, presets only (`TRACKER_REPORT=false`, no Telegram send; off the hour because GitHub delays and drops top-of-hour schedules); `push: master`; `workflow_dispatch`. JST has no DST, so fixed UTC crons are exact.
- `report` job: checkout → `actions/setup-go@v5` with `go-version: '1.27.1'` → build (`go build -o tracker ./cmd/tracker`) then run (`./tracker`), secrets passed as env; `timeout-minutes: 15`. Presets update after the Telegram send, so a preset failure never costs the report but fails the run.
- `checks` job (skipped on schedules): `go build ./...`, `go vet ./...`, `go test -race -count=1 ./...`, `golangci/golangci-lint-action@v9`.
- Go 1.27.1, stdlib only (`net/http`, `encoding/json`, `time`; `encoding/json/v2` is fine). No third-party modules, no go.sum.
- Stateless: nothing persisted, nothing committed. Every run computes from that day's API responses only; `git status` is clean after every run.
- Separate `keepalive` job in the same workflow: `liskin/gh-workflow-keepalive@v1` (re-enables the schedule via the API, no dummy commits; gautamkrishnar/keepalive-workflow is ToS-blocked upstream), only on scheduled runs, own job with `permissions: actions: write`. Required, not optional: with no daily commits, GitHub disables the schedule after 60 days of repo inactivity.
- Top-level `permissions: {}`; only the keepalive job gets `actions: write`. `concurrency: { group: daily-discount, cancel-in-progress: false }`.

## Darkbloom (added 2026-11)

- Optional `DARKBLOOM_API_KEY` secret: `internal/darkbloom` reads `api.darkbloom.dev/v1/models`; `discounts.Direct` prices Darkbloom-direct offers against the model's other OpenRouter providers.
- `internal/pipeline` runs every selection twice (OpenRouter side → OpenRouter presets; Darkbloom-direct side → `cmd/dbproxy`).
- The Darkbloom-direct table is shown even on days with zero OpenRouter discounts (user-approved behavior).
- `cmd/dbproxy` is the one exception to "no server": a local loopback proxy, never run in CI, holding picks in memory only. Darkbloom has no presets and no model fallback, so the proxy injects preset prompts/params and walks the 3 models itself.

## Secrets / Security (hard requirements)

- GitHub Actions secrets only: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHANNEL_ID` (required for report runs), optional `AA_API_KEY` (finance scores), optional `OPENROUTER_MANAGEMENT_KEY` (preset updates; a read-write *management* key — an inference key gets `401 Invalid management key` on writes), optional `DARKBLOOM_API_KEY`. No hardcoded values. `TRACKER_REPORT` is a non-secret env flag (`false` = presets-only run, no Telegram secrets needed).
- Telegram: server-side call to `https://api.telegram.org/bot<TOKEN>/sendMessage`. Token never logged or echoed; rely on GitHub's automatic secret masking.
- Error strings from keyed sources (AA, Darkbloom) must be sanitized before use in the Telegram notice or logs: the secret value is removed by exact match, then truncated to 120 runes (`internal/fetch`). No other secrets appear in any output.

## Discount detection (stateless)

1. GET `https://openrouter.ai/api/v1/models` daily (catalog: base prices, endpoints, benchmarks).
2. Reported price = blended USD per Mtok for one agent request (30k input, 80% cached, 1.5k output, half reasoning — `discounts.DefaultWorkload`), averaged over the 4-hour `discounts.Horizon` the picks stay pinned; presets are repointed every 2 h, so one missed run is covered.
3. A model is "discounted" iff its cheapest fitting endpoint is discounted: either `pricing.discount > 0` (list = price/(1 − discount%)) or an active `pricing.overrides` time window (now ∈ window; `utc_start`/`utc_end` HHMM UTC, may wrap midnight, optional `utc_days`). A discount undercut by an undiscounted provider is no saving. Flex service-tier endpoints are never used; ≤4-bit-quantized endpoints are skipped when a higher-precision provider exists. Endpoint fetch failures degrade (affected models are missing from the report), never fail the run.
4. No state exists, so permanent price-drop detection is out of scope — discounts are listed discounts and override windows only. Do not invent a baseline.
5. `:free` ids are ranked in their own free sectors (Code-free, FreeHalflight); `:batch` variants are never reported.

## Sector ranking

Five report sectors, top 10 each (see `internal/rank` labels):

- Math — Epoch AI OTIS Mock AIME (`internal/epoch`)
- Long-context reasoning — OpenRouter popularity, rankings feed, log10 min–max (`internal/popularity`)
- Finance — Artificial Analysis Finance & Accounting index, free tier (`internal/aa`)
- Code and Code-free — Epoch AI WebDev Arena; models it does not rate fall back to the catalog's AA Coding Index, calibrated onto the Epoch scale

Rank = performance% × score + price% × (−Δ%); weights in `internal/rank/weights.go`. Unscored models rank after scored ones by Δ%, score `n/a`. A model keeps its best score across reasoning-effort variants; a `:free` id inherits its base model's score.

Score sources implement `rank.Provider`, are fetched concurrently (`rank.Providers`, first listed wins), and sit behind one interface so a source can be swapped without touching ranking logic. Fallback: if a source fails for a sector — missing key, network error, non-200, unparseable response, no scores for the sector — that sector ranks by price only: Δ% descending, score column omitted, notice under the header: `⚠️ scored only by price — <source> error: <redacted, truncated error>`.

Preset sectors (presets only, not in the report): expensive-long (top 3 strongest thinking discounted, deepest discount first, ignores price ceilings), `strongest-discount-*` personas (top 3 strongest-thinking tool-capable chat models; composite price 80 / performance 20 — only the halflight one is shown in Telegram), `hermes-*` roles (per-role need-based picks, by-cost). Plus the Darkbloom-direct table in the report: `DARKBLOOM — direct, cheapest 10 (via local dbproxy)`.

## Telegram message format

HTML `parse_mode`, not MarkdownV2. One message unless >4000 chars, then one per sector; the timestamp header repeats on every message. Monospace tables inside `<pre>`, fixed-width via padding, model names truncated with `…`, HTML-escape model names and error text (`&` `<` `>`), `n/a` right-aligned. Header timestamp in JST via `time.FixedZone("JST", 9*3600)`, not tzdata. The notice line appears in every message containing price-only tables. Zero OpenRouter discounted models → one short "no discounts today." message — but the Darkbloom-direct table is still shown on such days when it has rows (user-approved). Any single message <4096 chars.

Layout (header carries a price note: `prices: 4h-avg blended $/Mtok, 30k in (80% cached) + 1.5k out`):

```text
🪙 OpenRouter discounts — 17 Sep 2026 07:00 JST
prices: 4h-avg blended $/Mtok, 30k in (80% cached) + 1.5k out

FINANCE — top 10 discounted (AA Finance & Accounting)
⚠️ scored only by price — Artificial Analysis error: 429 rate limit, retries exhausted
<pre>
#  model                    provider        was→now      Δ% score
1  google/gemini-2.5…       gmicloud/fp8   1.30→0.65    -50  68.2
</pre>
```

Column widths: rank 2, model 24, provider 12, was→now 13 (blended per Mtok; 2 decimals, 3 below $1), Δ% 4, score 5 right-aligned — the score column is omitted when no row is scored; Code-free omits the price columns. Keep lines ≤80 chars.

## Deliverables

- `.github/workflows/daily.yml` — the four triggers, concurrency, permissions, `report`/`checks`/`keepalive` jobs.
- `go.mod` (`go 1.27.1`), `cmd/tracker/main.go` (report + presets, `TRACKER_REPORT` presets-only mode), `cmd/dbproxy` (local loopback proxy for Darkbloom-direct picks), `internal/discounts`, `internal/rank`, `internal/format`, `internal/telegram`, `internal/aa`, `internal/epoch`, `internal/popularity`, `internal/pipeline`, `internal/darkbloom`, `internal/preset`, `internal/match`, `internal/fetch`.
- `README.md` — setup: create repo, add the secrets (`TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHANNEL_ID`, `AA_API_KEY`, `OPENROUTER_MANAGEMENT_KEY`, `DARKBLOOM_API_KEY`) under Settings → Secrets → Actions, enable Actions; scheduled workflows are disabled by default on forks (use your own repo); keepalive explanation (`liskin/gh-workflow-keepalive@v1` is the only activity guard); 07:00 JST delivery ±15 min under GitHub load; what the price-only fallback notice looks like.
- Tests, `go test ./...`: discount/endpoint pricing and `was` derivation, override-window parsing, table alignment/truncation, HTML escaping, 4000-char split, sector-source-failure fallback (Δ% ranking, score column omitted, notice present, error redacted and truncated), zero-discount message.

## Acceptance

- `workflow_dispatch` run goes green end-to-end; Telegram message arrives with aligned tables.
- Simulated score-source failure → price-only tables, notice with redacted error present.
- Runs leave the repo untouched (no commits, clean `git status`).
- Telegram API failure → nonzero exit; API error body in logs, token never shown. Preset update failure → nonzero exit; a missing `OPENROUTER_MANAGEMENT_KEY` only skips the preset updates.

## Lint

- Always run just-lint to verify any code change
