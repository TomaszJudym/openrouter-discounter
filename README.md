# openrouter-discounter

Daily Telegram report of discounted OpenRouter models, ranked per category
(math, long-context reasoning, finance) by Artificial Analysis benchmark
scores. Runs on GitHub Actions daily at 07:00 JST.

```
tracker (GitHub Actions, daily 07:00 JST)
  │
  │ GET  openrouter.ai/api/v1/models                    ← catalog, base prices
  │ GET  openrouter.ai/api/v1/models/{slug}/endpoints   ← per-provider pricing → discounts
  │ GET  artificialanalysis.ai/api/v2/data/llms/models  ← benchmark scores
  ▼
rank top 10 per category (math, long-context, finance)
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
     API key). Optional; without it, categories rank by discount only.
   - `OPENROUTER_API_KEY` — key from openrouter.ai/keys. Optional; updates
     the `admech-math` / `admech-long` / `admech-finance` presets with each
     category's top 3.
3. Actions → daily → Run workflow.

## Scoring

Each category has one AA benchmark per model, 0–100. Ranking:

```
final = 0.5 × benchmark + 0.5 × (−Δ%)
```

Performance and price count equally. Prices are USD per Mtok prompt; Δ% is
the discount vs the provider's list price. Top 10 per category are sent.
Unscored models rank last, by discount. Source repos: OpenRouter
`/models/{slug}/endpoints` pricing and `artificialanalysis.ai/api/v2/data/llms/models`.

## Keepalive

GitHub disables scheduled workflows after 60 days without commits; a separate
job re-enables the workflow via API — no dummy commits.
