# OpenRouter discount tracker

Stateless daily report of active OpenRouter price discounts, ranked per sector
with Artificial Analysis benchmark scores, delivered to Telegram by a GitHub
Actions cron. No server, no storage, no commits: every run computes from that
day's API responses.

Schedule: `0 22 * * *` UTC = 07:00 JST daily. JST has no DST, so the fixed UTC
cron is exact; GitHub delivers within ±15 min under load.

## Discount detection

The catalog is fetched from `https://openrouter.ai/api/v1/models`, then every
non-free model's per-provider pricing from
`GET /api/v1/models/{author}/{slug}/endpoints` (444+ models, concurrency 8,
retry on 429/5xx). This is the same data the
[collections/discounted-models](https://openrouter.ai/collections/discounted-models)
page is built from; there is no bulk or collections API, so the scan walks
the models.

A model is **discounted** iff at least one provider endpoint advertises
`pricing.discount > 0`. On such an endpoint `pricing.prompt` is already the
discounted price; the list price is `prompt/(1-discount)` — e.g. StreamLake's
`0.0000006426` = `0.0000014` × (1−0.541) against the standard $1.40/Mtok
list. The reported row uses the cheapest effective prompt price among the
model's discounted endpoints; Δ% = −discount.

As a secondary signal, time-limited catalog windows on the model's own
`pricing.overrides` (`utc_days` weekday windows priced below the top-level
base price; `utc_start`/`utc_end` are prompt-size buckets in Ktok,
`min_prompt_tokens` volume tiers ignored — they have no time window) are
reported too. Endpoint rows take precedence for the same model.

Models with a `:free` suffix are listed separately as free-tier info, never
as discounts. Models whose endpoint fetch fails are skipped and logged.

## Sector scoring

| Sector | Header | AA source field |
|---|---|---|
| Math | `MATH — top 10 discounted (AA Math Index)` | `evaluations.artificial_analysis_math_index` |
| Long-context reasoning | `LONG-CONTEXT REASONING — top 10 discounted (AA-LCR)` | `evaluations.lcr` (0–1 → ×100) |
| Finance | `FINANCE — top 10 discounted (τ³-Banking)` | `evaluations.tau_banking` (0–1 → ×100) |

Scores come from one request to
`https://artificialanalysis.ai/api/v2/data/llms/models` (`x-api-key` header).
AA model slugs/names are matched to OpenRouter ids by normalized identifier.
Fetching sits behind the `rank.Provider` interface, so the data source can be
swapped without touching ranking logic. All OpenRouter and AA calls are plain
JSON APIs; no HTML is parsed anywhere.

**Fallback**: if AA fails for any reason — missing key, network error, non-200,
unparseable response, no scores for a sector — that sector ranks by Δ%
(discount magnitude) and omits the score column, with a notice under its
header:

```
FINANCE — top 10 discounted (τ³-Banking)
⚠️ scored only by price — Artificial Analysis error: status 403: REDACTED, ke…
```

A fetch-level failure puts one notice at the top of the message and makes all
sectors price-only. Error text is sanitized before display: the `AA_API_KEY`
value is removed, then truncated to 120 chars.

## Message format

HTML `parse_mode`. One message unless >4000 chars, then one per sector (each
message re-carries the timestamp header and any global notice). Fixed-width
tables inside `<pre>`; model names truncated with `…` at 24 chars; HTML
escaping on model names and error text; prices per Mtok, 2 decimals; JST
timestamp via a fixed zone. Zero discounts anywhere → one short
"no discounts today." message (plus the free-tier block).

```
🪙 OpenRouter discounts — 17 Sep 2026 07:00 JST

MATH — top 10 discounted (AA Math Index)
<pre>
#  model                       price      was   Δ% score
1  z-ai/glm-4.7                $0.40    $0.55  -27  95.0
2  minimax/minimax-m2          $0.26    $0.30  -15  78.3
3  inception/mercury-2.5       $0.04    $0.20  -80   n/a
</pre>
```

The `price` column is the cheapest discounted provider's effective prompt
price; `was` is that provider's list price.

## Setup

1. Create your own repo and push this code. Scheduled workflows are disabled
   by default on forks — run it in a repo you own.
2. Settings → Secrets and variables → Actions, add:
   - `TELEGRAM_BOT_TOKEN` — bot token from @BotFather
   - `TELEGRAM_CHANNEL_ID` — channel id (`@name` or numeric `-100…`)
   - `AA_API_KEY` — optional; free key from the Artificial Analysis Insights
     Platform. Without it every sector is scored by price only.
3. Enable Actions on the repo.
4. Trigger a manual run: Actions → daily → Run workflow (`workflow_dispatch`).

## Keepalive

GitHub disables a schedule after 60 days of repo inactivity. The separate
`keepalive` job (`liskin/gh-workflow-keepalive@v1`) re-enables the workflow
via the API — no dummy commits — and is the only activity guard in this
setup; nothing else commits. It holds `actions: write`; every other
workflow permission is explicitly empty. It only runs on scheduled events.
(Original plan used `gautamkrishnar/keepalive-workflow@v2`, which GitHub
ToS-blocked in April 2025.)

## Security

- Secrets live only in GitHub Actions secrets. Nothing is hardcoded.
- The Telegram token is used solely in the sendMessage URL; error messages
  are redacted before any logging, and the token never appears in output.
- The AA key is sent as the `x-api-key` header and is stripped from any error
  text that reaches logs or Telegram.
- A Telegram API failure exits nonzero with the API error body in the logs.

## Local development

Go 1.27.1 via Docker (matches the workflow):

```sh
docker run --rm -v "$PWD":/app -w /app golang:1.27.1-alpine sh -c \
  "gofmt -l . && go vet ./... && go test ./..."
```

Stdlib only (`encoding/json/v2` included); no module dependencies, no
`go.sum`. `git status` stays clean after every run — the `tracker` binary from
the CI build step is git-ignored.
