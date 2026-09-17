# OpenRouter discount tracker

Stateless daily report of active OpenRouter price discounts, ranked per sector
with Artificial Analysis benchmark scores, delivered to Telegram by a GitHub
Actions cron. No server, no storage, no commits: every run computes from that
day's API responses.

Schedule: `0 22 * * *` UTC = 07:00 JST daily. JST has no DST, so the fixed UTC
cron is exact; GitHub delivers within ±15 min under load.

## Discount detection

The catalog is fetched from `https://openrouter.ai/api/v1/models` (444+ models
as of Sep 2026). The live `pricing.overrides` schema is an array of tiered
pricing entries:

```json
"pricing": {
  "prompt": "0.00000066",
  "overrides": [
    { "utc_days": ["saturday","sunday"],
      "prompt": "0.00000066" },
    { "utc_days": ["monday","tuesday","wednesday","thursday","friday"],
      "utc_start": 400, "utc_end": 600,
      "prompt": "0.00000132" },
    { "min_prompt_tokens": 272000, "prompt": "0.00002" }
  ]
}
```

- `utc_days` is the time window (UTC weekdays); `utc_start`/`utc_end` are
  prompt-size bucket bounds in Ktok (`utc_end: 0` = open-ended) — a token
  dimension, not a time-of-day window.
- A model is **discounted** iff an override whose `utc_days` contains the
  current UTC weekday prices prompt below the top-level base price. The
  reported price is the cheapest prompt price among today's windows; `was` is
  the base price; Δ% = (price − was)/was. The API has no discount-percentage
  field, so no inverse `was` derivation is needed.
- Volume-only tiers (`min_prompt_tokens`, no `utc_days`) have no time window
  and are ignored. Zero or null base pricing is excluded.
- Models with a `:free` suffix are listed separately as free-tier info, never
  as discounts.

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
swapped without touching ranking logic.

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
1  deepseek/deepseek-chat      $0.14    $0.28  -50  68.2
2  qwen/qwen3-235b-a22b        $0.09    $0.18  -50   n/a
</pre>
```

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
`keepalive` job (`gautamkrishnar/keepalive-workflow@v2`, API mode, no dummy
commits) counts as activity and is the only activity guard in this setup —
nothing else commits. It holds `actions: write`; every other workflow
permission is explicitly empty.

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
