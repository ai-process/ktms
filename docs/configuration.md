# Configuration

## Configuration reference

Everything is an environment variable; see [`env.example`](../env.example).

| Variable | Default | Description |
| --- | --- | --- |
| `DATABASE_URL` | – | **Required.** Postgres connection string. |
| `KTMS_ENCRYPTION_KEYS` / `LLMPROXY_ENCRYPTION_KEYS` | – | **Required.** Versioned keyring `1:<base64 32 bytes>[,2:…]`. The highest version encrypts new writes; all listed versions can decrypt. |
| `GRPC_ADDR` | `:9090` | gRPC listen address (data and admin planes). |
| `GRPC_MAX_RECV_MSG_SIZE` | `16777216` | Maximum gRPC message receive size in bytes (16MB). |
| `HTTP_ADDR` | `:8080` | HTTP listen address (health probes and the OpenAI-compatible API). |
| `HTTP_API_ENABLED` | `true` | Serve the `/v1` API on `HTTP_ADDR`; `false` leaves only the health probes. |
| `MCP_GATEWAY_ENABLED` | `true` | Serve the [MCP gateway](mcp.md#mcp-gateway) (`/mcp`, `/mcp/{server}`) on `HTTP_ADDR`. |
| `AGENTS_ENABLED` | `true` | Serve the [agents' inbox](mcp.md#agents), the gateway's built-in server `agents`. |
| `AGENTS_MAX_DEPTH` | `5` | How deep a chain of tasks in one conversation may go. |
| `AGENTS_MAX_MESSAGES_PER_CONTEXT` | `200` | Messages in one conversation. |
| `AGENTS_MAX_MESSAGE_BYTES` | `65536` | Size of one message, at most `1048576`. |
| `AGENTS_MAX_TASKS_PER_PAIR_HOUR` | `100` | Tasks one agent may send another within an hour, in any conversation; `0` is unlimited. |
| `AGENTS_TOOL_RPM` | `60` | Calls of each agents tool per key and minute; `0` is unlimited. Needs Redis. |
| `AGENTS_RETENTION` | `720h` | Tasks unchanged this long are deleted; `0` keeps them. |
| `AGENTS_CORRECTION_WINDOW` | `168h` | How long after its last change an ended task takes a correction of its answer. |
| `AGENTS_MAX_PINNED` | `200` | Tasks one agent may pin with `pin_task`; pinned tasks outlive `AGENTS_RETENTION`. |
| `AGENTS_RECORDING` | `metadata` | What the journal keeps by default: `off`, `metadata` or `full`. |
| `AGENTS_JOURNAL_MAXLEN` | `100000` | Events the journal's Redis stream keeps; the oldest go first. |
| `AGENTS_JOURNAL_MAX_EVENT_BYTES` | `16384` | Text of one `full` event; longer text is cut and hashed. |
| `AGENTS_JOURNAL_REDIS_URL` | – | A separate Redis for the journal (`redis://...`); empty uses `REDIS_ADDR`. |
| `HTTP_API_TIMEOUT` | `5m` | Read/write timeout for one HTTP API call (it waits on an upstream model). |
| `METRICS_ADDR` | `:9091` | Internal HTTP listen address for Prometheus `/metrics` and `/healthz`. Empty disables the metrics server. |
| `LOG_LEVEL` | `info` | zerolog level. |
| `SHUTDOWN_TIMEOUT` | `30s` | Graceful drain time on SIGTERM. |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `25` / `5` | Postgres pool bounds. |
| `DB_TIMEOUT` | `3s` | Bounds connecting to Postgres (unless the DSN sets `connect_timeout`) and each per-request query such as key checks, so a hung database fails calls fast. |
| `AUTH_STALE_IF_ERROR` | `5m` | While Postgres is unreachable, a key that verified within this window keeps working (a revocation made just before the outage can lag by at most this much). `0` refuses every call during an outage. |
| `ROUTING_RELOAD_SECONDS` | `60` | How often replicas poll for config changed elsewhere. |
| `AUTO_MIGRATE` | `true` | Apply embedded migrations at startup (guarded by an advisory lock, safe for rolling deploys). Off means applying `migrations/*.sql` by hand. |
| `MIGRATE_TIMEOUT` | `3m` | Bound for the whole migration run. |
| `AUTO_SEED` | `false` | Automatically populate initial models, rules, and client key when starting against an unconfigured database (enabled in docker-compose). |
| `SEED_CLIENT_KEY` | empty | Pre-configured client API key (e.g. `ktms_dev_default_key`) for auto-seed; generated if unset. |
| `SEED_CLIENT_NAME` | `dev-default` | Name assigned to the auto-seeded client API key. |
| `SEED_OPENAI_KEY` / `OPENAI_API_KEY` | empty | OpenAI API key to encrypt and register during auto-seeding. |
| `SEED_GEMINI_KEY` / `GEMINI_API_KEY` | empty | Google Gemini API key to encrypt and register during auto-seeding. |
| `SEED_ANTHROPIC_KEY` / `ANTHROPIC_API_KEY` | empty | Anthropic API key to encrypt and register during auto-seeding. |
| `REDIS_ADDR` | empty | Redis for throttling. Empty disables limits. |
| `REDIS_PASSWORD` / `REDIS_DB` | empty / `0` | Redis auth and database. |
| `REDIS_KEY_PREFIX` | empty | Namespace for counters when Redis is shared (compose uses `dev`). |
| `REDIS_TIMEOUT` | `500ms` | Per-command Redis read/write timeout (dials get twice as long, one retry). With the fail-fast guard, about what a hung Redis adds to a request before limits fail open. |
| `BUDGET_GRACE_MARGIN` | `0.05` | Fraction of daily token budget allowed as grace overdraft (0.05 = 5%, 0.0 or negative disables grace overdraft). |
| `SPEND_LIMITS_FAIL_MODE` | `open` | What spend limits do when Redis errors: `open` lets calls through unmetered (logged), `closed` refuses calls under a spend limit with `UNAVAILABLE throttled:spend_unavailable`. |
| `SPEND_DEFAULT_OUTPUT_RESERVE_TOKENS` | `4096` | Output tokens priced into a spend reservation when the call sets no max tokens. |
| `CIRCUIT_BREAKER_ENABLED` | `true` | Enable upstream Circuit Breaker to fail-fast during provider outages without network IO. |
| `CIRCUIT_BREAKER_FAIL_THRESHOLD` | `5` | Consecutive upstream failures (5xx, network timeouts) before tripping circuit to Open. |
| `CIRCUIT_BREAKER_COOLDOWN` | `30s` | Cooldown period before allowing a single probe request in Half-Open state. |
| `UPSTREAM_REQUEST_TIMEOUT` | `0` (disabled) | Upstream vendor timeout (total per-attempt deadline for unary/media; per-chunk idle watchdog for streaming). Stalls trip breaker and advance fallback chains. |
| `VENDOR_COOLDOWN_MAX` | `5m` | Longest pause honoured when a vendor answers `429`/`503` with `Retry-After`. `0` disables pauses. |
| `BATCH_POLL_SECONDS` | `60` | Background polling interval in seconds for checking unfinished batch jobs. |
| `BATCH_MAX_ITEMS` | `10000` | Maximum number of items allowed in a single batch submission. |
| `RESULTS_RETENTION_DAYS` | `7` | Retention period for completed batch results before cleanup (accepts integer days, e.g. `7`, or Go duration string, e.g. `168h`). |
| `NODE_HEARTBEAT_INTERVAL_SECONDS` | `5` | Node agent heartbeat reporting interval in seconds. |
| `NODE_HEARTBEAT_TTL_SECONDS` | `15` | Heartbeat TTL in seconds before an idle node transitions to `OFFLINE`. |
| `NODE_RETENTION_TTL_DAYS` | `1` | Retention period in days for inactive/offline nodes before garbage collection. |
| `NODE_ALLOW_LOCALHOST_ENDPOINTS` | `false` | Allow loopback/link-local endpoints for local inference nodes (useful in tests/dev). |
| `NODE_USAGE_RETENTION_HOURS` | `168` | Hours each node's request log (time, model, tokens, outcome; no user, key or text) is kept in Redis (6.2+) for the node's agent to pull with `PullNodeUsage`; a node with no requests for that long loses its log, and revoking its key deletes it. `0` turns the log off. |
| `BOOTSTRAP_ADMIN_KEY` | empty | Implicit admin key for creating the first real key. Unset afterwards. |
| `USAGE_COLLECTOR_HOST` | empty | `host:port` of a gRPC usage sink. Empty disables reporting. |
| `USAGE_COLLECTOR_TLS` | `false` | Reach the sink over TLS, e.g. light-tokenmeter on Cloud Run (`host:443`). |
| `USAGE_COLLECTOR_TOKEN` | empty | Bearer token sent to the sink on every call; for light-tokenmeter, a token with the `write` scope. Empty sends none. |
| `USAGE_PROJECT_ID` | `ktms` | Default project name on usage events. |
| `SENTRY_DSN` | empty | Error reporting; empty disables it. |
| `SENTRY_ENVIRONMENT` / `SENTRY_RELEASE` | `development` / empty | Sentry metadata. |
| `SENTRY_ENABLE_TRACING` | `false` | Sentry performance tracing. |
| `SENTRY_DEBUG` | `false` | Sentry SDK debug logging. |
| `SENTRY_WITH_BREADCRUMBS` | `false` | Attach recent log lines to Sentry events as breadcrumbs. |
| `OTEL_SERVICE_NAME` | `ktms` | Service name reported in OpenTelemetry traces. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | OTLP gRPC endpoint (`host:port`) for distributed tracing; empty disables remote export. |
| `CACHE_ENABLED` | `false` | Enable prompt and completion exact-match cache in Redis. |
| `CACHE_TTL` | `1h` | Expiration time for cached responses. |
| `CACHE_MAX_BYTES` | `1048576` | Maximum size in bytes of a single cached response (1MB). Responses exceeding this are not cached. |
| `GUARDRAILS_ENABLED` | `false` | Enable input/output security guardrails (PII masking and prompt injection detection). |
| `GUARDRAILS_PII_ACTION` | `block` | Action on detected PII: `block` (reject with `guardrail_violation`) or `redact` (mask with placeholders and desanitize in output). |
| `GUARDRAILS_PROMPT_INJECTION_ACTION` | `block` | Action on detected prompt injection: `block` or `log`. |
| `GUARDRAILS_LATENT_ENABLED` | `false` | Experimental. With `GUARDRAILS_ENABLED`, also screen prompts in latent space against injection, jailbreak, prompt-leak and exfiltration centroids (see [Latent guardrails](concepts.md#semantic-cache-and-latent-guardrails)). |
| `GUARDRAILS_LATENT_THRESHOLD` | `0.70` | Cosine similarity to a malicious centroid at which a prompt is flagged. Tuned for the local encoder; recalibrate for a JEPA encoder. |
| `GUARDRAILS_LATENT_ACTION` | `block` | `block` (reject with `guardrail_violation`) or `log`. |
| `GUARDRAILS_LATENT_CATEGORIES_FILE` | empty | JSON array of extra categories (`name`, `phrases`, and/or precomputed `centroids` of `{model, vector}`), added to the built-in set. |
| `SEMANTIC_CACHE_ENABLED` | `false` | Experimental. Serve `enable_cache` requests whose final user message is semantically close to a cached one. Needs Redis; uses `CACHE_TTL` and `CACHE_MAX_BYTES`. |
| `SEMANTIC_CACHE_SIMILARITY_THRESHOLD` | `0.95` | Minimum cosine similarity for a semantic hit. |
| `SEMANTIC_CACHE_MAX_ENTRIES` | `256` | Vectors kept (and scanned per lookup) for one exact context; older entries are trimmed. |
| `LATENT_ENCODER_URL` | empty | Experimental. OpenAI-compatible `/v1/embeddings` endpoint, e.g. a node serving a JEPA encoder. Empty uses the local hash encoder; failures fall back to it. |
| `LATENT_ENCODER_MODEL` | `jepa-text` | Model name sent to the encoder; also namespaces cached vectors. |
| `LATENT_ENCODER_API_KEY` | empty | Bearer token for the encoder, if it needs one. |
| `LATENT_ENCODER_TIMEOUT` | `50ms` | Bound for one encoder call before falling back to the local encoder. |
| `LATENT_LOCAL_ENCODER_DIMS` | `512` | Width of the local hash encoder's vectors. |
| `LATENT_ENCODER_CACHE_SIZE` | `4096` | In-memory LRU of recent embeddings, so guardrails and cache encode a prompt once. `0` disables it. |


## Setting it up: keys, models, rules

All of this is done with the admin service (`AdminService`) using a key that has
the `admin` scope. Mutations are validated in a transaction together with the existing
config, so an invalid state can never become active. Prefer a browser or a terminal to
`grpcurl`? See the [web console](console-and-cli.md#web-console) and the [command-line tool](console-and-cli.md#command-line-tool).

### Configuration as a file

Everything below can also live in a YAML file in your repository and be applied with the
[`ktms` command](console-and-cli.md#command-line-tool), which plans the changes first, like `terraform plan`:

```bash
export KTMS_ADDR=localhost:9090 KTMS_ADMIN_KEY=ktms_...   # admin; admin:read is enough for plan and export
ktms config export -o ktms.yaml      # start from what is running
ktms config plan -f ktms.yaml        # what would change; --detailed-exitcode exits 3 on drift
ktms config apply -f ktms.yaml       # asks before applying; --yes for scripts
```

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/ai-process/ktms/main/schema/ktms-config.schema.json
api_version: ktms.ai-process.dev/v1alpha1
models:
  - {id: gpt-4o-mini, vendor: openai, efforts: [EFFORT_LOW, EFFORT_MEDIUM, EFFORT_HIGH],
     price_in_per_mtok: 0.15, price_out_per_mtok: 0.6, price_cache_read_per_mtok: 0.075}
rules:
  - {name: default, priority: 1, use: [gpt-4o-mini]}
organizations:
  - name: acme
    teams: [{name: search, monthly_budget_usd: 100, allowed_models: [gpt-4o-mini]}]
api_keys:
  - {name: billing, policy: {rpm_limit: 120, tpm_limit: 200000}}
  - {name: search-bot, organization: acme, team: search}
vendor_keys:
  - {api_key_name: billing, vendor: openai, plain_key_env: OPENAI_KEY_BILLING}
spend_limits:
  - {subject_type: key, subject_id: billing, period: day, limit_usd: 5}
user_rate_limits:
  - {user_id: "*", model_id: gpt-4o-mini, rpm: 30}
```

- Models, rules, user rate limits and key policies are the Admin API's own messages
  (`ModelSpec`, `RuleSpec`, …) with the same field names; a model or rule without `enabled`
  is enabled. The JSON Schema is [`schema/ktms-config.schema.json`](../schema/ktms-config.schema.json)
  (also `ktms config schema`), a full example [`schema/example.yaml`](../schema/example.yaml).
- Only the sections in the file are managed; a missing section is left alone. `--prune` deletes
  what a managed section does not list, except `admin` and `admin:read` keys, which it only
  reports.
- Secrets never go in the file. A vendor key is read from the variable named in
  `plain_key_env` and compared with the stored one by fingerprint (`VendorKeyInfo.fingerprint`);
  a new client key is printed once, after apply. Exported vendor keys point at
  `KTMS_VENDOR_KEY_<KEY>_<VENDOR>`. MCP credentials (`mcp_credentials`) work the same way with
  `value_env`, exported as `KTMS_MCP_CREDENTIAL_<SERVER>`.
- What the Admin API cannot change is refused before anything is applied: a key's scopes or
  team, or a revoked key named again. Mint a key under another name instead.
- A team's cap is its `monthly_budget_usd`; `spend_limits` holds key and user caps.
- `node_schedules` lists the schedules KTMS sends to nodes that hand theirs to it (`node_id` and
  `schedule`, the rules as text); temporary overrides are not configuration.
- Steps run in dependency order (organizations, models, keys, vendor keys, rules, limits, then
  deletes). If one fails, the earlier ones stay applied; plan again to see what is left.

### 1. API keys

```bash
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" \
  -d '{"name":"billing-service","scopes":["generate"]}' \
  localhost:9090 ktms.v1.AdminService/MintAPIKey
```

Keys look like `ktms_<hex>` (keys minted before the rename start with `llm_` and keep working). Only a hash is stored, and the plaintext is returned once.
Scopes: `generate` (data plane), `mcp` (the [MCP gateway](mcp.md#mcp-gateway)), `admin` (implies
everything) and `admin:read` (the Admin API reads: `GetConfig`, `GetSpend`, `GetTeam`,
`GetTeamSpend`, `ExplainRoute` and every `List*`; nothing that changes state) and `agents:read`
(the [agents](mcp.md#agents)' cards and conversations, messages included: `ListAgents`,
`ListAgentTasks`, `GetAgentTask`; nothing else). A read key sees every tenant, so it cannot be bound to an organization or team. `GetConfig`
never returns a vendor key itself, only its fingerprint.
`ListAPIKeys` and `RevokeAPIKey` do what they say.

A key can carry its own policy, on top of its team's and each model's `rpm`, at minting
(`"policy": {...}`) or later with `SetAPIKeyPolicy` (it replaces the policy; the key's next call
uses it):

```bash
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" \
  -d '{"name":"billing-service","policy":{"allowed_models":["gpt-4o-mini"],"rpm_limit":120,"tpm_limit":200000}}' \
  localhost:9090 ktms.v1.AdminService/SetAPIKeyPolicy
```

- `allowed_mcp`: the MCP servers and tools a key with the `mcp` scope may use; see
  [MCP gateway](mcp.md#mcp-gateway).
- `data_classification`: `restricted` keeps every call of the key on self-hosted nodes; see
  [Restricted data](concepts.md#restricted-data).
- `allowed_models`: the models the key may call; empty allows any. A chain keeps only the models
  both the key's and its team's lists allow; an override outside them, or a chain with none left,
  is `PERMISSION_DENIED` (the message names `key` or `team`).
- `rpm_limit`: calls per minute over all models, a minute of them as burst.
- `tpm_limit`: tokens per minute over all models (GCRA weighted by tokens). A call books its input
  estimate when admitted and its real input + output when it ends, so an underestimate is paid
  for by the next calls; a call larger than a minute of tokens never fits and is refused with
  `FAILED_PRECONDITION request_too_large` (HTTP 400). A cache hit counts as a call, with no tokens.
- Both rates count text generation, embeddings, judgments, speech and images (media calls by
  their real tokens only), not batches. Refusals are `RESOURCE_EXHAUSTED` `throttled:key_rpm` /
  `throttled:key_tpm` with `RetryInfo` (HTTP 429 with `Retry-After`). Like every limit they need
  Redis and fail open without it.

### 2. Vendor keys (per client)

A vendor credential is assigned to **one** API key and encrypted at rest. A client can
only use models of vendors it holds a credential for, and `ListModels` reflects that. The
exception is the `local` vendor: models served by registered nodes need no vendor key, so every
client can call them.

```bash
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" \
  -d '{"api_key_name":"billing-service","vendor":"google","plain_key":"AIza..."}' \
  localhost:9090 ktms.v1.AdminService/SetVendorKey
```

Vendors: `openai`, `anthropic`, `google`, `deepseek` (OpenAI-compatible API, default endpoint
`https://api.deepseek.com`), `groq` (OpenAI-compatible API, default endpoint `https://api.groq.com/openai/v1`),
`mistral` (OpenAI-compatible API, default endpoint `https://api.mistral.ai/v1`), `azure` (Azure OpenAI Service;
endpoint is required, e.g. `https://<resource>.openai.azure.com` with optional `?api-version=...`; model id maps
to deployment name; content filter violations automatically advance to the next fallback candidate), `bedrock`
(Amazon Bedrock Converse/ConverseStream API; key format is `<access_key_id>:<secret_access_key>[:<region>[:<session_token>]]`,
defaulting region to `us-east-1`; model id maps to Bedrock foundation model or inference profile ARN/ID; optional `endpoint`
overrides the base URL; content filter and guardrail interventions automatically advance to fallback candidate), `ollama` (any
self-hosted OpenAI-compatible server such as Ollama or llama.cpp; the model's `endpoint` is required, e.g.
`https://host/v1`, and the vendor key is the server's own) and `typesafe`
(judgments only). Use `DeleteVendorKey` to remove one.

### 3. Models

```json
{ "model": {
    "id": "gemini-2.5-flash",              // referenced by rules and model_override; also sent to the vendor
    "vendor": "google",
    "endpoint": "",                         // optional base-URL override (Azure, self-hosted gateways…)
    "efforts": ["EFFORT_LOW", "EFFORT_MEDIUM"],
    "capabilities": ["google_search"],
    "rpm": 600,                             // per client key per minute, 0 = unlimited
    "price_in_per_mtok": 0.30,              // for usage reporting
    "price_out_per_mtok": 2.50,
    "price_in_peak_per_mtok": 0,            // for vendors with peak-hour pricing, else 0
    "price_out_peak_per_mtok": 0,
    "price_in_batch_per_mtok": 0,           // batch pricing; 0 defaults to half normal price
    "price_out_batch_per_mtok": 0,
    "price_cache_read_per_mtok": 0.075,     // cached input; 0 = vendor default (see below)
    "price_cache_write_per_mtok": 0,
    "daily_tokens_per_key": 50000000,       // per API key per UTC day, 0 = unlimited
    "daily_tokens_per_user": 500000,        // per attributes["user_id"] (or svc:<key_name> fallback) per UTC day
    "languages": {"source": ["*"], "target": ["*"]}, // required on translate models: the pairs served
    "price_per_million_chars": 0,           // translate: per million source characters, added to tokens
    "enabled": true } }
```

(The `//` comments are for illustration; strip them from real JSON.) Send it to `UpsertModel`.

When `price_in_batch_per_mtok` and `price_out_batch_per_mtok` are `0`, the proxy automatically defaults them to half the regular price (`price_in_per_mtok / 2`, `price_out_per_mtok / 2`).

**Prompt caching.** Input served from or written to the vendor's prompt cache is priced at
`price_cache_read_per_mtok` and `price_cache_write_per_mtok` (standard rates; peak and batch calls
scale them like input). At `0` the vendor default applies, chosen never to undercharge: for
`anthropic`, 0.1x input to read and 1.25x to write (the 5-minute cache); for `bedrock`, 0.25x
and 1.25x (Claude reads at 0.1x and Nova at 0.25x there); for every other vendor, the input
price. Set the read rate for OpenAI, Gemini and DeepSeek models to get their discount; the seeded
OpenAI and Gemini models carry theirs. KTMS reads the cached part from each vendor (`cache_read_input_tokens` and
`cache_creation_input_tokens`, `prompt_tokens_details.cached_tokens`, DeepSeek's
`prompt_cache_hit_tokens`, Gemini's `cachedContentTokenCount`, Bedrock's `cacheRead/WriteInputTokens`),
bills spend limits by it, sends it to light-tokenmeter as `cache_read_tokens` and
`cache_write_tokens`, and returns it in `TokensUsage.cache_read` / `cache_write` and, over HTTP,
`usage.prompt_tokens_details.cached_tokens`.

Capabilities a model may declare:

| Capability | Meaning |
| --- | --- |
| `batch` | Can serve asynchronous batch text generation requests (`SubmitBatch`). |
| `google_search` | Can serve requests with `enable_google_search`. |
| `no_thinking` | The model reasons by default and bills the reasoning as output (DeepSeek V4, Gemma 4 on Ollama). The proxy turns it off: `thinking: {"type": "disabled"}` for DeepSeek, `reasoning_effort: "none"` for Ollama, and for `local` models when the serving node runs Ollama (other engines get no switch). OpenAI-compatible vendors only. |
| `no_sampling` | The model's API rejects sampling fields Keystone would otherwise send it (see [Sampling](grpc-api.md#sampling)): it gets none of `temperature`, `top_p`, `stop` and `seed`, and responses report them as ignored. |
| `sampling` | The model takes its vendor's sampling fields although its id says otherwise (see [Sampling](grpc-api.md#sampling)): an Azure deployment name, a Claude alias, a reasoning model run without reasoning. |
| `prompt_cache` | Anthropic only: puts `cache_control` breakpoints after the tools, the system prompt and, in a multi-turn conversation, the latest turn, so repeated prefixes are read at 0.1x input. A new prefix is written at 1.25x, so set it on models whose instructions or conversations repeat. |
| `vision` | Can process image parts in multimodal messages (`ImagePart`). |
| `audio_input` | Can process audio parts in multimodal messages (`AudioPart`). |
| `pdf_input` | Can process PDF document parts in multimodal messages (`DocumentPart`). |
| `tts` | Can serve `SynthesizeSpeech`. |
| `image` | Can serve `GenerateImage`. |
| `judge` | Can serve `Judge` (TypeSafe `jev-latest`, OpenAI `gpt-6-luna`). |
| `translate` | Can serve `Translate`: a chat model by prompt, a node MT model natively. Declares `languages` (see [Translation](grpc-api.md#translation)). |
| `embedding_input` | A judge model that scores raw latent vectors in `JudgeRequest.state_media` (`EmbeddingPart`). |

### 4. Rules

```bash
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" -d '{"rules":[
  {"name":"spanish-high",  "priority":30, "effort":"EFFORT_HIGH",
   "attributes":{"language":"es"}, "use":["gemini-2.5-pro","gpt-4o"], "enabled":true},
  {"name":"low-default",   "priority":20, "effort":"EFFORT_LOW",
   "use":["deepseek-chat","gpt-4o-mini"], "enabled":true},
  {"name":"anything-else", "priority":1,
   "use":["gpt-4o-mini"], "enabled":true}
]}' localhost:9090 ktms.v1.AdminService/ReplaceRules
```

- The **highest priority** matching rule wins; priorities and names are unique.
- A rule matches when its `effort` equals the request's (unspecified = any effort) **and**
  every attribute in the rule equals the request's attribute.
- `use` is the ordered **fallback chain**, cheaper-first by convention. If the first model
  is throttled or its vendor is unavailable, the next is tried.
- `weights` (optional) enables **weighted traffic routing** (Canary or A/B testing). If specified, `weights` must contain exactly one positive integer per model in `use` (e.g. `[90, 10]`). When an explicit `canary_key` or `user_id` attribute is provided, distribution is deterministic and sticky per key (salted per rule name). If omitted or when `canary_mode: "random"` is passed, routing uses weighted random distribution. When `weights` is omitted, routing is strictly deterministic (100% on `use[0]`).
- `shadow_model` and `shadow_percentage` (optional) enable **Shadow Routing (Request Mirroring)**: asynchronously duplicates a sample (0.0–100.0%) of incoming traffic to a candidate shadow model in the background without affecting caller latency or responses. Shadow calls are non-billable to end-users (recorded at 0 nano-USD cost in usage events), carry `is_shadow: "true"` and `shadow: "true"` metadata, do not count against caller spend limits or daily token budgets, respect model RPM limits and global concurrency semaphores (512 for shadow, 256 for evaluation) to protect proxy resources and upstream providers, and never trip circuit breakers for primary traffic.
- `shadow_eval` (optional boolean, requires `shadow_model`): enables automated quality evaluation comparing primary vs shadow model outputs using an internal `Judge` model asynchronously in the background. The judge evaluates preference (`primary`, `shadow`, or `tie`) and quality scores without adding latency to the primary caller, respects judge model RPM and circuit breakers without consuming caller spend limits, and exports results via Prometheus metrics (`ktms_shadow_judge_evaluations_total` and `ktms_shadow_judge_scores`).
- Validation requires a **catch-all** (no attribute predicates) for every effort, so no
  request can ever fall through. `anything-else` above covers all three at once.
- `ReplaceRules` swaps the whole set atomically; there is no per-rule CRUD.

Inspect the live configuration with `GetConfig`.

### 5. Spend limits (optional)

Hard caps in USD across all models, kept in nano-USD (1 USD = 1 000 000 000) to avoid float
drift. Calls are priced from the model's `price_*_per_mtok` (peak and batch rates included):

```bash
# $50 a day for the "support-bot" key, and $20 a month for every end user by default.
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" -d '{"spend_limit":
  {"subject_type":"key","subject_id":"support-bot","period":"day","limit_nanousd":50000000000}}' \
  localhost:9090 ktms.v1.AdminService/SetSpendLimit
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" -d '{"spend_limit":
  {"subject_type":"user","subject_id":"*","period":"month","limit_nanousd":20000000000}}' \
  localhost:9090 ktms.v1.AdminService/SetSpendLimit
```

- `subject_type` is `key` (API key name) or `user` (`attributes["user_id"]`, or
  `svc:<key_name>`); `subject_id` `*` is the default for every subject of that type.
  `period` is a UTC `day` or `month`.
- Before each model in the chain is called, its worst case
  (`estimated input × the dearer of the input and cache-write price + max tokens × output price`,
  or `SPEND_DEFAULT_OUTPUT_RESERVE_TOKENS` without max tokens) is reserved atomically on every
  applicable counter by one Redis Lua script; when the call ends the reservation is settled
  to the actual cost. A call that would cross any cap is refused, so a cheaper model further
  down the chain may still be tried.
- Batches are checked at submission (refused once a cap is reached) and charged at the
  batch rate when results arrive.
- Refusals are `RESOURCE_EXHAUSTED throttled:spend_key_daily`, `throttled:spend_key_monthly`,
  `throttled:spend_user_daily` or `throttled:spend_user_monthly`, with a `RetryInfo` until the
  period resets (HTTP: 429 `insufficient_quota` plus `Retry-After`).
- `ListSpendLimits`, `DeleteSpendLimit` and `GetConfig` show and lift caps. Every usage event
  carries its `cost_nanousd`, which light-tokenmeter sums per day, model, user or job.
- `GetSpend` shows where a subject stands now, from the counters that refuse calls: for a key
  (`{"subject_type":"key","subject_id":"billing-service"}`), user or team, one window per period
  with `spent_nanousd`, `limit_nanousd`, `remaining_nanousd` and `resets_at`. Spend is counted
  only under a cap (a `*` limit counts for every subject): an uncapped window says
  `metered: false`, and a cap set mid-period counts from then on. A new cap shows once the config
  reloads: right away on the replica that set it, within `ROUTING_RELOAD_SECONDS` on the others.

### 6. Organizations and teams (optional)

Group keys into organizations and teams to give each team its own models and monthly budget:

```bash
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" -d '{"name":"acme"}' \
  localhost:9090 ktms.v1.AdminService/CreateOrganization
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" -d '{"org_id":"<org id>","name":"search",
  "monthly_budget_usd":500,"allowed_models":["gpt-5.1","local-qwen3-32b"]}' \
  localhost:9090 ktms.v1.AdminService/CreateTeam
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" -d '{"name":"search-service",
  "scopes":["generate"],"org_id":"<org id>","team_id":"<team id>"}' \
  localhost:9090 ktms.v1.AdminService/MintAPIKey
```

- A team key may call only the team's `allowed_models` (empty means any). A model override
  outside them is `PERMISSION_DENIED`; a chain is filtered to them, and a chain with none
  left is `PERMISSION_DENIED`. The proxy's own judge calls (auto effort, shadow evaluation)
  are not restricted. `ListModels` (and `/v1/models`) lists only the allowed models.
- `UpdateTeam` replaces the team: send every field, since an omitted budget or model list
  lifts the cap or the restriction.
- `monthly_budget_usd` is a spend limit on all the team's keys together (UTC month), refused
  with `RESOURCE_EXHAUSTED throttled:spend_team_monthly`. It is set only through
  `CreateTeam`/`UpdateTeam` (`0` lifts it); `SetSpendLimit` refuses `subject_type: "team"`.
  `GetTeamSpend` reads the enforced counter, which runs only while a budget is set.
- `data_classification: restricted` keeps every call of the team's keys on self-hosted nodes; see
  [Restricted data](concepts.md#restricted-data).
- Rules can match `org_id` and `team_id`. The proxy sets them from the key; callers cannot.
- Deleting a team or organization detaches revoked keys and is `FAILED_PRECONDITION` while
  active keys remain: revoke or move them first.

### Speech, image and judgment rules

These modalities carry no effort. The proxy routes them at `EFFORT_LOW` with a reserved
`attributes["modality"]` set to `tts`, `image`, `judge` or `translate`, and only rules that name that
modality can match, over models that declare the matching capability:

```json
{"name":"speech", "priority":100, "attributes":{"modality":"tts"},
 "use":["gemini-2.5-flash-preview-tts"], "enabled":true}
```

Without such a rule the call fails with `FAILED_PRECONDITION no_capable_model`.

### Rotating the encryption key

New writes use the highest version in `KTMS_ENCRYPTION_KEYS`; every listed version can decrypt.

1. Add a new version and deploy it to **every** replica: `1:<old>,2:<new>`. Until all replicas run
   the new keyring, do not add or change vendor keys (or the `SEED_*_KEY` values with
   `AUTO_SEED`): a replica still on `1:<old>` cannot read a key written with version 2. It logs
   an error, counts `ktms_dependency_errors_total{dependency="keyring"}`, and keeps the
   credential it already had for that client and vendor; a replica that starts in that state
   has none, so that client's requests skip that vendor's models (the next model in the chain
   serves, or `no_vendor_key` if none can). The rest of the config keeps loading either way. A
   keyring that can read no vendor key at all is refused at startup: that is a wrong keyring,
   not a rotation.
2. Call `RotateEncryption` once, on any replica. It re-encrypts every vendor key with version 2
   (in one transaction that locks the rows it rewrites, so a key set at the same moment is not
   lost) and the auth tokens of registered nodes, and returns how many it changed; calling it
   again changes nothing. If a vendor key needs a version the keyring lacks, it fails with
   `FAILED_PRECONDITION` naming the key and changes nothing.
3. Remove version 1 and redeploy, but only when no database backup you may restore still holds
   keys encrypted with it (see [Backups](operations.md#backups-and-the-encryption-keyring)). Until then,
   leaving it listed is harmless. Cached responses encrypted with a removed version are
   treated as cache misses.

`TestRotateEncryptionLetsTheOldKeyGo` runs this sequence against Postgres in CI.
