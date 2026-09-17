# Task: OpenRouter Discount Tracker — GitHub Actions Cron + Go + Telegram

Build a fully working system, not pseudocode. Commit-ready code, config files, setup docs. No server, no public endpoints, no storage — one repo, stateless daily run.

## Architecture
- Single workflow `.github/workflows/daily.yml`: `schedule: 0 22 * * *` (UTC) = 07:00 Asia/Tokyo daily. JST has no DST, so a fixed UTC cron is exact. Plus `workflow_dispatch` for manual runs.
- Runner job: checkout → `actions/setup-go@v5` with `go-version: '1.27.1'` → `go build -o tracker ./cmd/tracker && ./tracker`.
- Go 1.27.1, stdlib only (`net/http`, `encoding/json`, `time`; `encoding/json/v2` is fine). No third-party modules, no go.sum.
- Stateless: nothing persisted, nothing committed. Every run computes from that day's API responses only; `git status` is clean after every run.
- Separate keepalive job in the same workflow: `gautamkrishnar/keepalive-workflow@v2` (default API mode, no dummy commits), own job with `permissions: actions: write`. Required, not optional: with no daily commits, GitHub disables the schedule after 60 days of repo inactivity.
- Top-level `permissions: {}`; only the keepalive job gets `actions: write`. `concurrency: { group: daily-discount, cancel-in-progress: false }`.

## Secrets / Security (hard requirements)
- GitHub Actions secrets only: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHANNEL_ID`, optional `AA_API_KEY`. No other credentials, no hardcoded values.
- Telegram: server-side call to `https://api.telegram.org/bot<TOKEN>/sendMessage`. Token never logged or echoed; rely on GitHub's automatic secret masking.
- AA error strings must be sanitized before use in the Telegram notice or logs: exact-match removal of the `AA_API_KEY` value, then truncate to 120 chars. No other secrets appear in any output.

## Discount detection (stateless)
1. GET https://openrouter.ai/api/v1/models daily.
2. Current price = `pricing.prompt` / `pricing.completion` (USD per token; report per Mtok).
3. A model is "discounted" iff it has an active `pricing.overrides` time-window (now ∈ window). Extract discount % and the UTC window; derive `was = price / (1 − discount%)`.
4. No state exists, so permanent price-drop detection is out of scope — discounts are override windows only. Do not invent a baseline.
5. Flag `:free`-suffixed models separately (free tier, not discounts). Exclude models with zero/null pricing.

## Sector ranking
Three sectors, top 10 each:
- Math — Artificial Analysis Math Index
- Long-context reasoning — AA-LCR (~100k-token long-context reasoning index)
- Finance — τ³-Banking (banking/finance agent tasks)

AA data via one `internal/aa` fetch. Fallback: if AA fails for any reason — missing key, network error, non-200, unparseable response, no scores for a sector — that sector scores by price only: rank by Δ% descending, omit the score column, and put a notice line under that sector's header: `⚠️ scored only by price — Artificial Analysis error: <redacted, truncated error>`. If AA fails globally (fetch-level error), one notice at the top of the message and all sectors go price-only. If AA works, models without a score rank after scored ones by Δ% with score `n/a`. Keep benchmark fetching behind a single interface so the data source can be swapped later without touching ranking logic.

## Telegram message format
HTML `parse_mode`, not MarkdownV2. One message unless >4000 chars, then one per sector. Monospace tables inside `<pre>`, fixed-width via padding, model names truncated with `…`, HTML-escape model names and error text (`&` `<` `>`), `n/a` right-aligned. Header timestamp in JST via `time.FixedZone("JST", 9*3600)`, not tzdata. Notice line appears in every message containing price-only tables. Zero discounted models anywhere → one short "no discounts today" message. Any single message <4096 chars.

Layout:

```
🪙 OpenRouter discounts — 17 Sep 2026 07:00 JST

MATH — top 10 discounted (AA Math Index)
<pre>
#  model                   price     was       Δ%    score
1  deepseek/deepseek-chat   $0.14     $0.28     -50   68.2
2  qwen/qwen3-235b-a22b     $0.09     $0.18     -50    n/a
</pre>

FINANCE — top 10 discounted (τ³-Banking)
⚠️ scored only by price — Artificial Analysis error: 429 rate limit, retries exhausted
<pre>
#  model                   price     was       Δ%
1  google/gemini-2.5-pro    $0.65     $1.30     -50
2  openai/gpt-5.2           $0.80     $0.88     -9
</pre>

Free tier (info only):
<pre>
:free models today — 12
openai/gpt-oss-120b:free, meta/llama-4-scout:free, …
</pre>
```

Column widths: rank 2, model 24, price 8, was 8, Δ% 4, score 5 (adjust as needed; keep lines ≤80 chars). Prices per Mtok, 2 decimals.

## Deliverables
- `.github/workflows/daily.yml` — schedule + workflow_dispatch, concurrency, permissions, runner job, separate keepalive job.
- `go.mod` (`go 1.27.1`), `cmd/tracker/main.go`, `internal/discounts`, `internal/rank`, `internal/format`, `internal/telegram`, `internal/aa`.
- `README.md` — setup: create repo, add the three secrets under Settings → Secrets → Actions, enable Actions; scheduled workflows are disabled by default on forks (use your own repo); keepalive explanation (it is the only activity guard now); 07:00 JST delivery ±15 min under GitHub load; what the price-only fallback notice looks like.
- Tests, `go test ./...`: override/window parsing and `was` derivation, table alignment/truncation, HTML escaping, 4096-char split, AA-failure fallback (Δ% ranking, score column omitted, notice present, error redacted and truncated), zero-discount message.

## Acceptance
- `workflow_dispatch` run goes green end-to-end; Telegram message arrives with aligned tables.
- Simulated AA failure → price-only tables, notice with redacted error present.
- Runs leave the repo untouched (no commits, clean `git status`).
- Telegram API failure → nonzero exit; API error body in logs, token never shown.
