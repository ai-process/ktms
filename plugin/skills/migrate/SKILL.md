---
name: migrate
description: Plan and prepare a migration to Keystone (KTMS) from LiteLLM Proxy or from direct vendor SDK calls - inventory, a mapping table, a ktms config file and a cut-over plan. Use when asked to replace LiteLLM, OpenRouter-style aliases or scattered vendor SDK calls with KTMS.
---

# Migrate to Keystone

## What maps to what

| From | In Keystone |
| --- | --- |
| A model alias or deployment | A model (id = the vendor's model name, vendor, efforts, prices) |
| Several deployments of one model | One rule's chain, or weights in one rule |
| Fallbacks | The order of a rule's `use` chain; open circuits and vendor pauses are skipped |
| Virtual keys | Client keys (scope `generate`), each with its own vendor keys |
| Budgets | `spend_limits` per key or user, per UTC day or month |
| rpm/tpm per key | A key `policy` (`rpm_limit`, `tpm_limit`, `allowed_models`) |
| Teams | `organizations` with teams; a team budget is monthly only (`monthly_budget_usd`) |
| Spend logs | Usage events in light-tokenmeter |

Clients stop picking aliases: they send an effort (`auto`, `auto:low|medium|high`) and
attributes. On `/v1`, `temperature`, `top_p`, `stop`, `seed` and penalties are accepted and
ignored, and `n` must be 1: flag every client that depends on them.

## Steps

1. Inventory every model, alias, fallback, key, team, budget and limit (or, for SDK users, every
   call site with its model and parameters).
2. The mapping table: old item, Keystone item, and what has no equivalent (a daily team budget,
   sampling parameters) with how to handle it.
3. A `ktms.yaml` (see the configure skill); `ktms config validate`, then `ktms config plan`
   against the new Keystone. Vendor keys come from environment variables named in
   `plain_key_env`.
4. Client changes per call site: base URL, key, model value (effort instead of alias), `user` and
   `metadata.action_id` (see the integrate skill).
5. Cut-over: run Keystone next to the old setup, move one service or a share of traffic first,
   compare errors, latency and spend for a few days (`tokenmeter usage summary --group-by
   model`), then switch, keeping the old setup ready to roll back for a week.

Ask about anything that does not map with confidence. Sources: [Concepts](https://github.com/ai-process/ktms/blob/main/README.md#concepts),
[Setting it up: keys, models, rules](https://github.com/ai-process/ktms/blob/main/README.md#setting-it-up-keys-models-rules),
[OpenAI-compatible HTTP API](https://github.com/ai-process/ktms/blob/main/README.md#openai-compatible-http-api).
