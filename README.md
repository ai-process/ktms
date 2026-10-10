# llm-proxy

[![Test](https://github.com/ai-process/llm-proxy/actions/workflows/test.yml/badge.svg)](https://github.com/ai-process/llm-proxy/actions/workflows/test.yml)
[![Docker image](https://img.shields.io/docker/v/coyl/llm-proxy?label=docker%20hub&sort=semver)](https://hub.docker.com/r/coyl/llm-proxy)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A self-hosted LLM gateway. Your services talk to one gRPC endpoint; the proxy holds
the vendor API keys, decides which model answers each request, falls back when a
vendor is down, enforces rate and spend limits, and records usage.

- **One place for vendor keys.** OpenAI, Google Gemini, DeepSeek and TypeSafe credentials
  live encrypted in Postgres (AES-256-GCM). Client services never see them.
- **Routing by rules, not code.** Clients send an *effort* (`LOW` / `MEDIUM` / `HIGH`) and
  free-form *attributes*; an ordered rule list maps them to a model chain with fallbacks.
  Rules are changed at runtime over an admin RPC, with no redeploy.
- **Throttling.** Per-model requests-per-minute and daily token budgets, per API key and
  per end user, backed by Redis.
- **More than text.** Text generation with structured (JSON-schema) output, asynchronous
  batch generation at 50% discount, speech synthesis, image generation and typed judgments.
- **OpenAI-compatible HTTP API.** Point any OpenAI SDK or tool at `http://host:8080/v1`
  and get routing, fallbacks and limits for free.
- **Usage reporting.** Optionally forwards one event per call to a gRPC usage sink.
- **Web console.** [llm-proxy-admin](#web-console) manages models, rules and keys in the browser.

## Contents

- [How it works](#how-it-works)
- [Quick start (Docker Compose)](#quick-start-docker-compose)
- [Installation](#installation)
  - [Docker image](#docker-image)
  - [Building from source](#building-from-source)
  - [Production checklist](#production-checklist)
- [Configuration reference](#configuration-reference)
- [Setting it up: keys, models, rules](#setting-it-up-keys-models-rules)
- [Web console](#web-console)
- [Calling the proxy](#calling-the-proxy)
- [OpenAI-compatible HTTP API](#openai-compatible-http-api)
- [Concepts](#concepts)
- [Error contract](#error-contract)
- [Operations](#operations)
- [Development](#development)
- [License](#license)

## How it works

```
 your services ──gRPC (Bearer llm_…)──▶ llm-proxy ──▶ OpenAI / Gemini / DeepSeek / TypeSafe
                                          │  │
                                 Postgres ┘  └ Redis (limits)      └─▶ usage sink (optional)
                                 config, keys, encrypted vendor keys
```

Two gRPC services share port `9090`:

| Service | Purpose | Needed scope |
| --- | --- | --- |
| `llmproxy.v1.LLMProxyService` | `GenerateText`, `SubmitBatch`, `GetBatch`, `ListBatchResults`, `CancelBatch`, `SynthesizeSpeech`, `GenerateImage`, `Judge`, `ListModels` | `generate` |
| `llmproxy.v1.LLMProxyAdminService` | models, rules, vendor keys, API keys, key rotation | `admin` |

Port `8080` serves the health probes (`/healthz`, `/readyz`) and the
[OpenAI-compatible HTTP API](#openai-compatible-http-api) under `/v1`. gRPC server
reflection is on, so `grpcurl` works without the `.proto` files.

## Quick start (Docker Compose)

Requirements: Docker with the Compose plugin, and [`grpcurl`](https://github.com/fullstorydev/grpcurl).

```bash
git clone https://github.com/ai-process/llm-proxy.git
cd llm-proxy
docker compose up --build -d
curl -s localhost:8080/readyz     # {"status":"ready"}
```

This starts the proxy, Postgres 16 and Redis 7. The compose file ships a **development-only**
encryption key and the bootstrap admin key `dev-bootstrap`; never reuse them anywhere real.
The database schema applies itself on first start.

Mint a client key, give it a vendor credential, register a model and a catch-all rule:

```bash
export ADMIN="authorization: Bearer dev-bootstrap"
G="grpcurl -plaintext -H"

# 1. A client API key (the plaintext is shown exactly once).
$G "$ADMIN" -d '{"name":"my-app","scopes":["generate"]}' \
  localhost:9090 llmproxy.v1.LLMProxyAdminService/MintAPIKey

# 2. Your OpenAI key, assigned to that client key.
$G "$ADMIN" -d '{"api_key_name":"my-app","vendor":"openai","plain_key":"sk-..."}' \
  localhost:9090 llmproxy.v1.LLMProxyAdminService/SetVendorKey

# 3. A model, and 4. rules (one catch-all per effort is mandatory).
$G "$ADMIN" -d '{"model":{"id":"gpt-4o-mini","vendor":"openai",
  "efforts":["EFFORT_LOW","EFFORT_MEDIUM","EFFORT_HIGH"],
  "price_in_per_mtok":0.15,"price_out_per_mtok":0.60,"enabled":true}}' \
  localhost:9090 llmproxy.v1.LLMProxyAdminService/UpsertModel

$G "$ADMIN" -d '{"rules":[
  {"name":"default","priority":1,"use":["gpt-4o-mini"],"enabled":true}]}' \
  localhost:9090 llmproxy.v1.LLMProxyAdminService/ReplaceRules

# 5. Ask something with the key from step 1.
$G "authorization: Bearer llm_<your key>" -d '{
  "effort":"EFFORT_LOW",
  "messages":[{"role":"MESSAGE_ROLE_USER","text":"Say hi in three words."}]}' \
  localhost:9090 llmproxy.v1.LLMProxyService/GenerateText
```

The response names the model and rule that served it (`resolved_model`, `matched_rule`).
Once your real keys exist, unset `BOOTSTRAP_ADMIN_KEY`.

## Installation

### Docker image

Release images are published to Docker Hub for `linux/amd64` and `linux/arm64`:

```bash
docker pull coyl/llm-proxy:latest      # or pin a release tag, e.g. coyl/llm-proxy:0.1.0
```

The image is a static Go binary on Alpine plus `ffmpeg` (needed to turn Gemini's raw
PCM speech into OGG/Opus). It runs as a non-root user, exposes `8080` (HTTP) and
`9090` (gRPC) and is configured entirely through environment variables.

You need a Postgres database (required; 16 is what the compose file uses) and a Redis (optional, see below):

```bash
# generate the at-rest encryption keyring once and keep it safe
export LLMPROXY_ENCRYPTION_KEYS="1:$(head -c 32 /dev/urandom | base64)"
export BOOTSTRAP_ADMIN_KEY="$(head -c 24 /dev/urandom | base64 | tr -d '/+=')"

docker run -d --name llm-proxy \
  -p 9090:9090 -p 8080:8080 \
  -e DATABASE_URL='postgres://user:pass@db-host:5432/llmproxy?sslmode=require' \
  -e LLMPROXY_ENCRYPTION_KEYS \
  -e BOOTSTRAP_ADMIN_KEY \
  -e REDIS_ADDR=redis-host:6379 \
  coyl/llm-proxy:latest
```

A minimal Compose service for your own stack:

```yaml
services:
  llm-proxy:
    image: coyl/llm-proxy:latest
    restart: unless-stopped
    environment:
      DATABASE_URL: postgres://llmproxy:${DB_PASSWORD}@db:5432/llmproxy?sslmode=disable
      LLMPROXY_ENCRYPTION_KEYS: ${LLMPROXY_ENCRYPTION_KEYS}
      REDIS_ADDR: redis:6379
    ports:
      - "9090:9090"
    depends_on: [db, redis]
```

On Kubernetes, point the liveness probe at `GET :8080/healthz` and the readiness probe
at `GET :8080/readyz` (checks the database). Run as many replicas as you like; they share
Postgres and Redis and pick up configuration changes within `ROUTING_RELOAD_SECONDS`.

### Building from source

Requires Go 1.25+ (and `ffmpeg` on `PATH` if you use speech).

```bash
make build                       # → bin/llm-proxy-server
export DATABASE_URL=postgres://llmproxy:llmproxy@localhost:5432/llmproxy?sslmode=disable
export LLMPROXY_ENCRYPTION_KEYS="1:$(head -c 32 /dev/urandom | base64)"
./bin/llm-proxy-server
```

Or build the image yourself: `docker build -t llm-proxy .`

### Production checklist

- **TLS.** The server speaks plaintext gRPC. Terminate TLS in front of it (Envoy, nginx
  with `grpc_pass`, a service mesh, a cloud load balancer) or keep it on a private network.
- **Back up the encryption keyring.** Without `LLMPROXY_ENCRYPTION_KEYS` the stored vendor
  keys are unreadable. Treat it like any other root secret.
- **Mint real keys, then unset `BOOTSTRAP_ADMIN_KEY`.** It is accepted as an implicit admin key.
- **Run Redis if you want limits.** With `REDIS_ADDR` empty, RPM and token budgets are
  **not enforced** (the proxy fails open by design, and does the same if Redis errors).
- **Give each client its own API key** (and its own vendor credentials), so spend, models
  and revocation are per client.
- **Mind your Postgres connection count.** Each replica holds up to `DB_MAX_CONNS`
  (default 25) connections.
- **Back up Postgres.** All configuration lives there.

## Configuration reference

Everything is an environment variable; see [`env.example`](env.example).

| Variable | Default | Description |
| --- | --- | --- |
| `DATABASE_URL` | – | **Required.** Postgres connection string. |
| `LLMPROXY_ENCRYPTION_KEYS` | – | **Required.** Versioned keyring `1:<base64 32 bytes>[,2:…]`. The highest version encrypts new writes; all listed versions can decrypt. |
| `GRPC_ADDR` | `:9090` | gRPC listen address (data and admin planes). |
| `HTTP_ADDR` | `:8080` | HTTP listen address (health probes and the OpenAI-compatible API). |
| `HTTP_API_ENABLED` | `true` | Serve the `/v1` API on `HTTP_ADDR`; `false` leaves only the health probes. |
| `HTTP_API_TIMEOUT` | `5m` | Read/write timeout for one HTTP API call (it waits on an upstream model). |
| `LOG_LEVEL` | `info` | zerolog level. |
| `SHUTDOWN_TIMEOUT` | `30s` | Graceful drain time on SIGTERM. |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `25` / `5` | Postgres pool bounds. |
| `ROUTING_RELOAD_SECONDS` | `60` | How often replicas poll for config changed elsewhere. |
| `AUTO_MIGRATE` | `true` | Apply embedded migrations at startup (guarded by an advisory lock, safe for rolling deploys). Off means applying `migrations/*.sql` by hand. |
| `MIGRATE_TIMEOUT` | `3m` | Bound for the whole migration run. |
| `REDIS_ADDR` | empty | Redis for throttling. Empty disables limits. |
| `REDIS_PASSWORD` / `REDIS_DB` | empty / `0` | Redis auth and database. |
| `REDIS_KEY_PREFIX` | empty | Namespace for counters when Redis is shared (compose uses `dev`). |
| `BATCH_POLL_SECONDS` | `60` | Background polling interval in seconds for checking unfinished batch jobs. |
| `BATCH_MAX_ITEMS` | `10000` | Maximum number of items allowed in a single batch submission. |
| `RESULTS_RETENTION_DAYS` | `7` | Retention period in days for completed batch results before cleanup. |
| `BOOTSTRAP_ADMIN_KEY` | empty | Implicit admin key for creating the first real key. Unset afterwards. |
| `USAGE_COLLECTOR_HOST` | empty | `host:port` of a gRPC usage sink. Empty disables reporting. |
| `USAGE_PROJECT_ID` | `llm-proxy` | Default project name on usage events. |
| `SENTRY_DSN` | empty | Error reporting; empty disables it. |
| `SENTRY_ENVIRONMENT` / `SENTRY_RELEASE` | `development` / empty | Sentry metadata. |
| `SENTRY_ENABLE_TRACING` | `false` | Sentry performance tracing. |

## Setting it up: keys, models, rules

All of this is done with the admin service (`LLMProxyAdminService`) using a key that has
the `admin` scope. Mutations are validated in a transaction together with the existing
config, so an invalid state can never become active. Prefer a browser to `grpcurl`? See the
[web console](#web-console).

### 1. API keys

```bash
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" \
  -d '{"name":"billing-service","scopes":["generate"]}' \
  localhost:9090 llmproxy.v1.LLMProxyAdminService/MintAPIKey
```

Keys look like `llm_<hex>`. Only a hash is stored, and the plaintext is returned once.
Scopes: `generate` (data plane) and `admin` (implies everything).
`ListAPIKeys` and `RevokeAPIKey` do what they say.

### 2. Vendor keys (per client)

A vendor credential is assigned to **one** API key and encrypted at rest. A client can
only use models of vendors it holds a credential for, and `ListModels` reflects that.

```bash
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" \
  -d '{"api_key_name":"billing-service","vendor":"google","plain_key":"AIza..."}' \
  localhost:9090 llmproxy.v1.LLMProxyAdminService/SetVendorKey
```

Vendors: `openai`, `google`, `deepseek` (OpenAI-compatible API, default endpoint
`https://api.deepseek.com`) and `typesafe` (judgments only). Use `DeleteVendorKey` to remove one.

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
    "daily_tokens_per_key": 50000000,       // per API key per UTC day, 0 = unlimited
    "daily_tokens_per_user": 500000,        // per attributes["user_id"] (or svc:<key_name> fallback) per UTC day
    "enabled": true } }
```

(The `//` comments are for illustration; strip them from real JSON.) Send it to `UpsertModel`.

When `price_in_batch_per_mtok` and `price_out_batch_per_mtok` are `0`, the proxy automatically defaults them to half the regular price (`price_in_per_mtok / 2`, `price_out_per_mtok / 2`).

Capabilities a model may declare:

| Capability | Meaning |
| --- | --- |
| `batch` | Can serve asynchronous batch text generation requests (`SubmitBatch`). |
| `google_search` | Can serve requests with `enable_google_search`. |
| `no_structured_output` | The vendor API can't take a JSON schema (e.g. DeepSeek). The shape is requested in the prompt and verified by the proxy instead. |
| `no_thinking` | The model reasons by default and bills the reasoning as output (DeepSeek V4). The proxy sends `thinking: {"type": "disabled"}`. OpenAI-compatible vendors only. |
| `tts` | Can serve `SynthesizeSpeech`. |
| `image` | Can serve `GenerateImage`. |
| `judge` | Can serve `Judge` (TypeSafe models). |

### 4. Rules

```bash
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" -d '{"rules":[
  {"name":"spanish-high",  "priority":30, "effort":"EFFORT_HIGH",
   "attributes":{"language":"es"}, "use":["gemini-2.5-pro","gpt-4o"], "enabled":true},
  {"name":"low-default",   "priority":20, "effort":"EFFORT_LOW",
   "use":["deepseek-chat","gpt-4o-mini"], "enabled":true},
  {"name":"anything-else", "priority":1,
   "use":["gpt-4o-mini"], "enabled":true}
]}' localhost:9090 llmproxy.v1.LLMProxyAdminService/ReplaceRules
```

- The **highest priority** matching rule wins; priorities and names are unique.
- A rule matches when its `effort` equals the request's (unspecified = any effort) **and**
  every attribute in the rule equals the request's attribute.
- `use` is the ordered **fallback chain**, cheaper-first by convention. If the first model
  is throttled or its vendor is unavailable, the next is tried.
- Validation requires a **catch-all** (no attribute predicates) for every effort, so no
  request can ever fall through. `anything-else` above covers all three at once.
- `ReplaceRules` swaps the whole set atomically; there is no per-rule CRUD.

Inspect the live configuration with `GetConfig`.

### Speech, image and judgment rules

These modalities carry no effort. The proxy routes them at `EFFORT_LOW` with a reserved
`attributes["modality"]` set to `tts`, `image` or `judge`, and only rules that name that
modality can match, over models that declare the matching capability:

```json
{"name":"speech", "priority":100, "attributes":{"modality":"tts"},
 "use":["gemini-2.5-flash-preview-tts"], "enabled":true}
```

Without such a rule the call fails with `FAILED_PRECONDITION no_capable_model`.

### Rotating the encryption key

Add a higher version to the keyring (`1:…,2:…`), redeploy, call `RotateEncryption` to
re-encrypt every stored vendor key, then drop the old version.

## Web console

[**llm-proxy-admin**](https://github.com/ai-process/llm-proxy-admin) is a small web UI for everything in
the previous section: models, routing rules, client API keys, vendor credentials and a playground for
trying a prompt through your rules. It can also show usage and spend per project, user and model when
paired with [light-tokenmeter](https://github.com/ai-process/light-tokenmeter). Sign-in is Google, limited to
an e-mail allowlist with wildcards (`*@example.com`).

```bash
# 1. a key for the console
grpcurl -plaintext -H "authorization: Bearer $BOOTSTRAP_ADMIN_KEY" \
  -d '{"name":"console","scopes":["admin"]}' localhost:9090 llmproxy.v1.LLMProxyAdminService/MintAPIKey

# 2. run it (see its README for the Google OAuth setup)
docker run -d -p 8081:8080 \
  -e PUBLIC_URL=https://admin.example.com \
  -e GOOGLE_CLIENT_ID=… -e GOOGLE_CLIENT_SECRET=… -e ALLOWED_EMAILS='*@example.com' \
  -e SESSION_SECRET="$(openssl rand -base64 36)" \
  -e LLM_PROXY_ADDR=llm-proxy:9090 -e LLM_PROXY_ADMIN_KEY=llm_… \
  coyl/llm-proxy-admin:latest
```

## Calling the proxy

Authenticate every call with `authorization: Bearer llm_…` gRPC metadata. Generate Go,
Python, TypeScript… clients from [`proto/llmproxy/v1`](proto/llmproxy/v1) with `buf` or
`protoc`, or use the committed Go stubs in [`gen/llmproxy/v1`](gen/llmproxy/v1)
(`github.com/ai-process/llm-proxy/gen/llmproxy/v1`).

### GenerateText

```json
{
  "effort": "EFFORT_MEDIUM",
  "attributes": {"language": "es", "user_id": "u-42", "task": "summary"},
  "base_system_instruction": "You are a concise assistant.",
  "request_instruction": "Answer in one paragraph.",
  "messages": [{"role": "MESSAGE_ROLE_USER", "text": "Summarize …"}],
  "response_schema": {"type": "object",
    "properties": {"summary": {"type": "string"}}, "required": ["summary"]},
  "max_output_tokens": 800,
  "action_id": "app.summarize"
}
```

| Field | Notes |
| --- | --- |
| `effort` | Routing tier. |
| `attributes` | Opaque key/values matched by rules and copied to usage events. `user_id` charges the per-user daily budget (falls back to `svc:<key_name>` for service callers without `user_id`). `usage_project` overrides the usage project (see below). |
| `base_system_instruction`, `request_instruction` | System content never travels as a message. |
| `response_schema` | Unset = free text. Set = JSON output validated against the schema; a vendor that can't enforce it is verified by the proxy. |
| `max_output_tokens` | Ceiling, `0` = none. Output tokens are the expensive ones; set it. |
| `enable_google_search` | Only Google models declaring `google_search` can serve it. |
| `action_id` | Free-form label for usage attribution. |
| `model_override` | Exact model id; bypasses rules but **not** throttles. |

The response carries `choices`, token `usage`, `resolved_model`, `resolved_vendor` and
`matched_rule` (empty on override).

### Batch text generation

For offline or bulk workflows (such as page generation, backfills, or data pipelines) that prioritize cost over latency, batch generation processes thousands of items asynchronously through vendor Batch APIs (e.g., Google Gemini Batch API) at a **50% discount** compared to interactive calls. Jobs typically finish within 24 hours (vendor timeout at 48 hours).

- **Route resolution:** Resolved once upon submission. The proxy selects the first model in the rule's chain that has the `batch` capability, a valid vendor key for the client, and available daily budget. Once submitted, there is no vendor fallback; per-item outcomes are returned to the client.
- **Privacy:** Prompts are streamed/submitted to the vendor and never persisted in the proxy database.
- **Throttling & Accounting:** RPM limits do not apply to batches. Daily token budgets are checked at submission (rejecting if already exhausted) and tokens are deducted when final item results arrive. Results are retained for `RESULTS_RETENTION_DAYS` (default 7 days).

#### SubmitBatch

Takes a list of items (`custom_id` unique within the batch, and standard `GenerateTextRequest`), batch-level `effort` and `attributes`, and an optional `client_batch_id` for idempotency (resubmitting with the same client batch ID returns the existing batch):

```bash
grpcurl -plaintext -H "authorization: Bearer llm_<key>" -d '{
  "effort": "EFFORT_LOW",
  "attributes": {"task": "catalog_enrichment"},
  "client_batch_id": "job-2026-10-07-001",
  "items": [
    {
      "custom_id": "item-1",
      "request": {
        "messages": [{"role": "MESSAGE_ROLE_USER", "text": "Extract attributes from product 1"}],
        "response_schema": {"type": "object", "properties": {"color": {"type": "string"}}, "required": ["color"]}
      }
    },
    {
      "custom_id": "item-2",
      "request": {
        "messages": [{"role": "MESSAGE_ROLE_USER", "text": "Extract attributes from product 2"}],
        "response_schema": {"type": "object", "properties": {"color": {"type": "string"}}, "required": ["color"]}
      }
    }
  ]
}' localhost:9090 llmproxy.v1.LLMProxyService/SubmitBatch
```

Returns a `Batch` with status `BATCH_STATE_PENDING` (or `BATCH_STATE_RUNNING`), total item count, model, and creation timestamp.

#### GetBatch

Polls batch state (`PENDING`, `RUNNING`, `SUCCEEDED`, `FAILED`, `CANCELLED`, `EXPIRED`) and progress counts (`total`, `done`, `failed`):

```bash
grpcurl -plaintext -H "authorization: Bearer llm_<key>" -d '{
  "batch_id": "0199c0a0-6228-7e10-91b5-685d996cbfa1"
}' localhost:9090 llmproxy.v1.LLMProxyService/GetBatch
```

#### ListBatchResults

Pages through completed items with `page_token` (and `next_page_token`). Each item contains its `custom_id` and either a successful `response` (`GenerateTextResponse` with choices, usage, resolved_model, resolved_vendor, matched_rule) or an `error` (`BatchItemError` with gRPC code, message, and error reason):

```bash
grpcurl -plaintext -H "authorization: Bearer llm_<key>" -d '{
  "batch_id": "0199c0a0-6228-7e10-91b5-685d996cbfa1",
  "page_size": 100
}' localhost:9090 llmproxy.v1.LLMProxyService/ListBatchResults
```

#### CancelBatch

Requests cancellation of an unfinished batch job at the vendor. The batch remains `RUNNING` until vendor confirmation is polled: already completed items preserve their results, while remaining unfinished items fail with reason `batch_cancelled`.

```bash
grpcurl -plaintext -H "authorization: Bearer llm_<key>" -d '{
  "batch_id": "0199c0a0-6228-7e10-91b5-685d996cbfa1"
}' localhost:9090 llmproxy.v1.LLMProxyService/CancelBatch
```

### SynthesizeSpeech, GenerateImage

`SynthesizeSpeech` takes `text` (read verbatim) and an optional `language` and returns OGG/Opus
`audio`. `GenerateImage` takes a `prompt` and returns `image` bytes with a `mime_type`.
Both accept `attributes`, `action_id` and `model_override`, and need the rules described
[above](#speech-image-and-judgment-rules).

### Judge

`Judge` answers typed questions about one state instead of writing text, so you get
calibrated probabilities to branch on rather than prose to parse. All questions in a call
are answered against the same state in parallel.

| Type | Question | Answer |
| --- | --- | --- |
| `JUDGE_TYPE_NOUL` | Does this hold? | probability of yes (`noul`, 0–1) |
| `JUDGE_TYPE_CHOICE` | Which one? | `choice` plus full `probabilities` and `confidence` |
| `JUDGE_TYPE_SCORE` | How much? | probability-weighted position on ordered `levels` |

```json
{
  "state_json": "{\"message\":\"My card was charged twice!\"}",
  "questions": {
    "angry": {"type": "JUDGE_TYPE_NOUL", "instructions": "Is the author upset?"},
    "topic": {"type": "JUDGE_TYPE_CHOICE", "instructions": "What is this about?",
              "options": {"billing": "payments and charges", "bug": "software defects"}}
  }
}
```

The vendor is [TypeSafe](https://typesafe.ai) (`jev-latest`), which bills input only and
writes no text, so its models must declare the `judge` capability and refuse `GenerateText`.

### ListModels

Returns the models the calling key can use (those whose vendor it holds a credential for), with
efforts, capabilities, rpm and prices.

## OpenAI-compatible HTTP API

The data plane is also available as HTTP/JSON in the OpenAI wire format, so existing
OpenAI SDKs, LangChain, `curl` and similar tools work by changing the base URL and key.
It runs on `HTTP_ADDR` (default `:8080`) and shares everything with gRPC: the same
`llm_…` API keys (scope `generate`), vendor credentials, rules, fallbacks, throttling and
usage reporting. Set `HTTP_API_ENABLED=false` to turn it off.

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="llm_<your key>")
reply = client.chat.completions.create(
    model="auto",                                # let the rules choose
    messages=[{"role": "user", "content": "Say hi in three words."}],
)
print(reply.choices[0].message.content, reply.model)   # `model` is the one that answered
```

```bash
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer llm_<your key>" -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Say hi in three words."}]}'
```

### Endpoints

| Endpoint | Maps to | Notes |
| --- | --- | --- |
| `POST /v1/chat/completions` | `GenerateText` | Text and structured output. |
| `POST /v1/audio/speech` | `SynthesizeSpeech` | Returns OGG/Opus audio. |
| `POST /v1/images/generations` | `GenerateImage` | Returns `b64_json`. |
| `GET /v1/models`, `GET /v1/models/{id}` | `ListModels` | Only models the key holds a vendor credential for. |
| `POST /v1/judge` | `Judge` | Not part of OpenAI's API. Body and response are the protobuf JSON of `JudgeRequest` / `JudgeResponse`. |

Authentication is `Authorization: Bearer llm_…`. The response headers `X-LLM-Proxy-Vendor`
and `X-LLM-Proxy-Rule` (and `X-LLM-Proxy-Model` for speech and images) tell you which
vendor and rule served the call; for chat the answering model is the `model` field.

### Choosing a model

The `model` field decides routing:

| `model` | Behaviour |
| --- | --- |
| `auto` (or empty) | Rules decide. Effort comes from `reasoning_effort` (`low` / `medium` / `high`), default `medium`. |
| `auto:low`, `auto:medium`, `auto:high` | Rules decide, at that effort. |
| anything else | An exact registry model id (`model_override`): bypasses rules but not throttles. |

Routing attributes (what rules match on, and what usage events carry) come from OpenAI's own
fields: `metadata` is passed through as attributes, `user` becomes `attributes["user_id"]`
(unless `metadata.user_id` is set), and `metadata.action_id` is used as the usage action label.

```json
{"model": "auto:low", "user": "u-42", "metadata": {"subject": "french", "action_id": "app.quiz"},
 "messages": [{"role": "user", "content": "Translate: good morning"}]}
```

### Chat completions: what is supported

| Request field | Support |
| --- | --- |
| `messages` | `system`/`developer` messages become the system instruction, `user`/`assistant` the conversation. Content is a string or text parts. |
| `max_tokens`, `max_completion_tokens` | Output ceiling (`max_output_tokens`). Set one: an unbounded answer is the costliest failure. |
| `response_format` | `json_schema` is enforced (converted to the proxy's schema; keywords such as `enum` and `additionalProperties` are dropped). `json_object` asks for JSON in the prompt. |
| `stream` | Accepted. The proxy retries and verifies whole answers, so the full reply arrives as a single SSE chunk followed by `[DONE]`; `stream_options.include_usage` is honoured. |
| `reasoning_effort`, `user`, `metadata` | Routing, as above. |
| `enable_google_search` | Extension: let a Google model ground the answer in search. |
| `n` | Must be 1. |
| `tools`, `tool_choice`, `functions` | Rejected with `400`. |
| Image/audio content parts, `tool`/`function` roles | Rejected with `400`. |
| `temperature`, `top_p`, `stop`, `seed`, `logprobs`, penalties… | Accepted and ignored. |

The response is a standard `chat.completion` with `finish_reason: "stop"` and token `usage`.

### Speech and images

`POST /v1/audio/speech` takes `model`, `input` and an optional `language` extension (pronunciation
hint, e.g. `"Serbian"`); `voice`, `response_format` and `speed` are ignored. The body of the response
is OGG/Opus whatever format was requested; check the `Content-Type`.

`POST /v1/images/generations` takes `model` and `prompt`; `n` must be 1 and `response_format: "url"`
is rejected (the image is returned as `data[0].b64_json`).

Both need rules for the `tts` / `image` [modality](#speech-image-and-judgment-rules).

### Errors

Errors use OpenAI's shape, `{"error": {"message", "type", "param", "code"}}`, and the proxy's
[error contract](#error-contract) shows up in `code`:

| HTTP | `type` | `code` examples |
| --- | --- | --- |
| 400 | `invalid_request_error` | `output_truncated`, `no_capable_model`, `no_vendor_key`, `not_configured`; validation messages |
| 401 / 403 | `authentication_error` / `permission_error` | missing or invalid key, missing scope |
| 429 | `rate_limit_error` | `throttled:rpm`, `throttled:budget_key`, `throttled:budget_user` (with `Retry-After`) |
| 502 | `server_error` | `vendor_error` |
| 503 | `server_error` | `vendors_unavailable` (the only error worth retrying) |

OpenAI SDKs retry 429 and 5xx automatically; for `throttled:*` and `vendor_error` you may want to
disable that (`max_retries=0`) and handle them yourself.

## Concepts

**Effort** is the only routing field the proxy understands; everything else about a request's
purpose travels in free-form attributes, so the proxy knows nothing about your product.

**Throttling** has two layers per model: a fixed per-minute request window
(`rpm`, per API key) and daily token budgets (`daily_tokens_per_key`, `daily_tokens_per_user`,
UTC day). A throttled model is skipped in the fallback chain; if none of the chain is
available the caller gets `RESOURCE_EXHAUSTED`.

**Usage reporting.** When `USAGE_COLLECTOR_HOST` is set, every call emits an event (vendor,
model, tokens, duration, status, `action_id`, rule name and your attributes as metadata) to a
gRPC service implementing [`usage-proto/usage/v1/usage.proto`](usage-proto/usage/v1/usage.proto).
Events are attributed to the project named by the `usage_project` attribute, falling back to the
API key's name. Any service implementing `RecordUsageEvent(s)` works as a sink.

## Error contract

### RPC status codes

| gRPC status | Reason | What to do |
| --- | --- | --- |
| `NOT_FOUND` | – | Unknown batch ID. |
| `INVALID_ARGUMENT` | – | Empty batch, duplicate `custom_id`, too many items (> `BATCH_MAX_ITEMS`), or invalid parameters. |
| `UNAVAILABLE` | `vendors_unavailable` | The whole chain failed. **The only retryable error.** |
| `RESOURCE_EXHAUSTED` | `throttled:rpm`, `throttled:budget_key`, `throttled:budget_user` | Back off; don't hot-retry. Daily budget exhausted before batch submission. |
| `FAILED_PRECONDITION` | `output_truncated` | Raise `max_output_tokens`; don't retry as-is. |
| `FAILED_PRECONDITION` | `no_capable_model` | No model in the matched chain can serve this request, modality, or batch capability. |
| `FAILED_PRECONDITION` | `no_vendor_key` | The client key holds no credential for the chain's vendors. |
| `FAILED_PRECONDITION` | `not_configured` | No models/rules configured yet. |
| `INTERNAL` | `vendor_error` | Terminal vendor failure. |
| `UNAUTHENTICATED` / `PERMISSION_DENIED` | – | Missing/invalid key, or key lacks the scope. |

### Batch item error reasons

Items returned in `ListBatchResults` that failed have an `error` containing a gRPC code, a message, and one of these reasons:

| Reason | When | What to do |
| --- | --- | --- |
| `output_truncated` | Item reached `max_output_tokens` or candidate finish limit. | Re-submit item with higher `max_output_tokens`. |
| `vendor_error` | Vendor error occurred during processing or structured schema validation failed. | Retry synchronously or resubmit in another batch. |
| `batch_expired` | Vendor batch expired (after 48 h) before the item finished. | Re-submit the item. |

## Operations

- **Health:** `GET :8080/healthz` (process up) and `GET :8080/readyz` (database reachable).
- **Logs:** structured JSON via zerolog. Vendor keys, client keys and the keyring are never logged.
- **Config changes** are picked up by all replicas within `ROUTING_RELOAD_SECONDS`; the replica that
  handled the admin call applies them immediately.
- **Upgrades:** migrations run at startup under a database advisory lock, so rolling deploys are
  safe. Roll forward by deploying a newer image tag.
- **Graceful shutdown:** on SIGTERM in-flight calls get `SHUTDOWN_TIMEOUT` to finish.

## Development

```bash
make test       # go test -race ./...
make build      # binary in bin/
make proto      # regenerate gen/ after editing proto/ (needs buf)
make clients    # regenerate the usage-sink stubs from usage-proto/
docker compose up --build   # local stack with Postgres + Redis
```

Layout: `cmd/server` (entrypoint), `internal/router` (rule matching, fallback, admission),
`internal/registry` (validated config snapshots), `internal/llm` (vendor-neutral chat model and
adapters), `internal/throttle` (Redis limits), `internal/grpcapi` (RPC handlers),
`internal/crypto` (keyring), `migrations/` (embedded SQL).

Pull requests are welcome; read [`CONTRIBUTING.md`](CONTRIBUTING.md) first.

### Releasing

Pushing a git tag builds and publishes a multi-arch image to Docker Hub as
`coyl/llm-proxy:<tag>` (and `latest` for semver tags). The repository needs the secrets
`DOCKER_USERNAME` and `DOCKER_PASSWORD` (a Docker Hub access token).

```bash
git tag 0.1.0 && git push origin 0.1.0
```

## License

[MIT](LICENSE)
