# KTMS (Keystone: The Model Switchboard)

[![CI](https://github.com/ai-process/ktms/actions/workflows/test.yml/badge.svg)](https://github.com/ai-process/ktms/actions/workflows/test.yml)
[![Buf](https://github.com/ai-process/ktms/actions/workflows/buf.yml/badge.svg)](https://github.com/ai-process/ktms/actions/workflows/buf.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ai-process/ktms)](https://goreportcard.com/report/github.com/ai-process/ktms)
[![Release](https://img.shields.io/github/v/release/ai-process/ktms?sort=semver)](https://github.com/ai-process/ktms/releases)
[![Docker Image](https://img.shields.io/docker/v/coyl/ktms?label=docker%20hub&sort=semver)](https://hub.docker.com/r/coyl/ktms)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache_2.0-blue.svg)](LICENSE)

A self-hosted LLM gateway and model switchboard. Your services talk to one endpoint; KTMS holds
the vendor API keys, decides which model answers each request, falls back when a
vendor is down, enforces rate and spend limits, and records usage.

- **One place for vendor keys.** OpenAI, Anthropic (Claude), Google Gemini, DeepSeek and TypeSafe credentials
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
- **Small footprint.** Single Go binary: ~67 MB image, ~7 MiB idle RAM, under 1 ms of added latency.
- **Web console.** [ktms-admin](#web-console) manages models, rules and keys in the browser.

## Contents

- [How it works](#how-it-works)
- [Quick start (Docker Compose)](#quick-start-docker-compose)
- [Resource footprint](#resource-footprint)
- [Installation](#installation)
  - [Docker image](#docker-image)
  - [Helm chart (Kubernetes)](#helm-chart-kubernetes)
  - [Building from source](#building-from-source)
  - [Node agent](#node-agent)
  - [Production checklist](#production-checklist)
- [Configuration reference](#configuration-reference)
- [Setting it up: keys, models, rules](#setting-it-up-keys-models-rules)
- [Run a node](#run-a-node)
- [Web console](#web-console)
- [Calling the proxy](#calling-the-proxy)
- [OpenAI-compatible HTTP API](#openai-compatible-http-api)
- [Concepts](#concepts)
- [Error contract](#error-contract)
- [API stability](#api-stability)
- [Operations](#operations)
- [Development](#development)
- [License](#license)

## How it works

```
 your services ──gRPC (Bearer ktms_…)──▶ KTMS ──▶ OpenAI / Anthropic / Gemini / DeepSeek / TypeSafe
                                          │  │
                                 Postgres ┘  └ Redis (limits)      └─▶ usage sink (optional)
                                 config, keys, encrypted vendor keys
```

Two gRPC services share port `9090`:

| Service | Purpose | Needed scope |
| --- | --- | --- |
| `ktms.v1.GatewayService` | `GenerateText`, `GenerateTextStream`, `SubmitBatch`, `GetBatch`, `ListBatchResults`, `CancelBatch`, `SynthesizeSpeech`, `GenerateImage`, `Judge`, `JudgeStream`, `EvaluateRollouts`, `GetEmbeddings`, `ListModels` | `generate` |
| `ktms.v1.AdminService` | models, rules, vendor keys, API keys, key rotation | `admin` |

Port `8080` serves the health probes (`/healthz`, `/readyz`) and the
[OpenAI-compatible HTTP API](#openai-compatible-http-api) under `/v1`. gRPC server
reflection is on, so `grpcurl` works without the `.proto` files.

## Quick start (Docker Compose)

The fastest way to run KTMS locally is with Docker Compose. With `AUTO_SEED` enabled (default in compose), the proxy automatically initializes default models, routing rules, and a ready-to-use client key (`ktms_dev_default_key`).

### 1. Launch the stack

```bash
git clone https://github.com/ai-process/ktms.git
cd ktms

# Pass your vendor API key (OpenAI, Gemini, or Anthropic)
OPENAI_API_KEY=sk-... docker compose up -d --build

# Verify readiness
curl -s http://localhost:8080/readyz     # {"status":"ready"}
```

Alternatively, run `./scripts/quickstart.sh` for an interactive setup that generates master encryption and bootstrap keys, configures `.env`, and launches the stack.

> [!NOTE]
> With `AUTO_SEED=true` (enabled by default in Compose), the proxy automatically syncs vendor API keys from environment variables (`OPENAI_API_KEY`, `GEMINI_API_KEY`, `ANTHROPIC_API_KEY`) to the client key named `dev-default` (or `$SEED_CLIENT_NAME`) on every startup.

### 2. Make your first call

You can immediately query the proxy using the OpenAI-compatible HTTP endpoint:

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer ktms_dev_default_key" \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Say hi in three words."}]}'
```

Or using the official OpenAI Python SDK:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="ktms_dev_default_key")
response = client.chat.completions.create(
    model="auto",
    messages=[{"role": "user", "content": "Say hi in three words."}],
)
print(response.choices[0].message.content)
```

The response includes canonical observability headers (`X-KTMS-Vendor`, `X-KTMS-Rule`, `X-KTMS-Placement`) along with backwards-compatible `X-LLM-Proxy-*` headers.

---

### Manual Setup via Admin RPC (Advanced)

If you prefer to configure everything manually from scratch (without auto-seeding), disable `AUTO_SEED=false` and use [`grpcurl`](https://github.com/fullstorydev/grpcurl) with `BOOTSTRAP_ADMIN_KEY`:

```bash
export ADMIN="authorization: Bearer dev-bootstrap"
G="grpcurl -plaintext -H"

# 1. Mint a client API key (plaintext is returned once)
$G "$ADMIN" -d '{"name":"my-app","scopes":["generate"]}' \
  localhost:9090 ktms.v1.AdminService/MintAPIKey

# 2. Assign vendor credential to the client key
$G "$ADMIN" -d '{"api_key_name":"my-app","vendor":"openai","plain_key":"sk-..."}' \
  localhost:9090 ktms.v1.AdminService/SetVendorKey

# 3. Register a model
$G "$ADMIN" -d '{"model":{"id":"gpt-4o-mini","vendor":"openai",
  "efforts":["EFFORT_LOW","EFFORT_MEDIUM","EFFORT_HIGH"],
  "price_in_per_mtok":0.15,"price_out_per_mtok":0.60,"enabled":true}}' \
  localhost:9090 ktms.v1.AdminService/UpsertModel

# 4. Set routing rules (catch-all for each effort tier is mandatory)
$G "$ADMIN" -d '{"rules":[
  {"name":"default","priority":1,"use":["gpt-4o-mini"],"enabled":true}]}' \
  localhost:9090 ktms.v1.AdminService/ReplaceRules

# 5. Call gRPC GenerateText with your client key
$G "authorization: Bearer ktms_<your key>" -d '{
  "effort":"EFFORT_LOW",
  "messages":[{"role":"MESSAGE_ROLE_USER","text":"Say hi in three words."}]}' \
  localhost:9090 ktms.v1.GatewayService/GenerateText
```

Once real keys exist in production, unset `BOOTSTRAP_ADMIN_KEY`.

## Resource footprint

KTMS is a single Go process with no interpreter. It adds well under a millisecond per request and
needs tens of megabytes where Python gateways such as LiteLLM Proxy need gigabytes.

| Metric | KTMS (Go) | LiteLLM Proxy (Python, 4 workers) | Difference |
|---|---|---|---|
| Docker image, linux/amd64 (uncompressed / pull) | ~67 MB / ~21 MB (slim) | ~1.3 GB / ~420 MB | ~20x smaller |
| Idle memory (RSS) | ~7 MiB | ~2.5 GiB | ~370x less |
| Memory at a steady 200 RPS | ~20 MiB, no drift | ~2.9 GiB | ~145x less |
| Peak memory over the whole run (up to 4,000 RPS) | < 50 MiB | ~3.0 GiB | ~60x less |
| Added latency, p50 / p99 | ~0.6 ms / ~1.1 ms | ~8.5 ms / 27–229 ms | ~13x lower p50 |
| Added time to first token (streaming), p50 | ~0.7 ms | ~160 ms | no chunk buffering in either |
| Max sustained throughput (< 1% errors) | ≥ 4,000 RPS (highest step tested) | ~250 RPS | ≥ 16x higher |
| CPU time per 1,000 requests | ~0.63 s | ~9.7 s | ~15x less |
| Time to readiness (container start → first proxied answer) | ~170 ms | ~32 s | ~190x faster |

The same stand also measured Bifrost, Portkey, Kong AI Gateway, Apache APISIX, Higress, Envoy AI
Gateway, agentgateway, New API and Plano. KTMS is in the same class as the compiled and
Nginx/Envoy-based gateways: all of them add under 1.3 ms at p50 and sustain 4,000 RPS. Among them,
only agentgateway matches KTMS's memory footprint, while agentgateway, APISIX and Kong add less
latency (0.25–0.35 ms) and use less CPU per request. The Node.js and Python gateways (Portkey,
LiteLLM) saturate at 500 and 250 RPS.

*Measured in October 2026 on Google Cloud: each gateway alone on a dedicated `c3-standard-4` (4 vCPU,
16 GB, x86_64), k6 on a second VM, and a mock OpenAI upstream (0 ms for latency, 10 ms for
throughput). LiteLLM `v1.106.0` ran with master key only and no database. Memory is the gateway
container's cgroup RSS; Postgres and Redis are not counted. Two independent runs agreed within ~10%,
except LiteLLM p99. The stand, every gateway config and the run scripts are in [`bench/`](bench/).*

## Installation

### Docker image

Release images are published to Docker Hub for `linux/amd64` and `linux/arm64` in two variants:

- **`coyl/ktms:latest`** (also tagged **`:slim`** or pinned `:<version>` / `:<version>-slim`): default lightweight image (~67 MB uncompressed, ~21 MB download). A clean Alpine container without `ffmpeg`, optimized for text generation, chat, reasoning, embeddings, and OpenAI TTS.
- **`coyl/ktms:audio`** (or pinned `:<version>-audio`): extended image (~195 MB uncompressed, ~69 MB download) with `ffmpeg` installed for Gemini speech synthesis (transcoding raw PCM into OGG/Opus voice messages).

```bash
docker pull coyl/ktms:latest      # default lightweight image (~67 MB)
docker pull coyl/ktms:audio       # with ffmpeg for Gemini speech synthesis (~195 MB)
```

If Gemini TTS is requested without ffmpeg available (e.g. on the default slim image), the proxy returns gRPC `Codes.Unimplemented` / HTTP 501 Not Implemented: `ffmpeg_missing: ffmpeg is required for Gemini speech synthesis: install ffmpeg or use the 'coyl/ktms:audio' container image`. OpenAI TTS produces native Opus directly and works on both variants without ffmpeg.

The image runs as a non-root user (`appuser`, UID 10001), exposes `8080` (HTTP) and `9090` (gRPC), and is configured entirely through environment variables. From 1.0.0, images are signed with cosign and carry an SBOM and provenance, and the release's `checksums.txt` is signed: see [Verifying a Release](SECURITY.md#verifying-a-release).

You need a Postgres database (required; 16 is what the compose file uses) and a Redis (optional, see below):

```bash
# generate the at-rest encryption keyring once and keep it safe
export KTMS_ENCRYPTION_KEYS="1:$(head -c 32 /dev/urandom | base64)"
export BOOTSTRAP_ADMIN_KEY="$(head -c 24 /dev/urandom | base64 | tr -d '/+=')"

docker run -d --name ktms \
  -p 9090:9090 -p 8080:8080 \
  -e DATABASE_URL='postgres://user:pass@db-host:5432/ktms?sslmode=require' \
  -e KTMS_ENCRYPTION_KEYS \
  -e BOOTSTRAP_ADMIN_KEY \
  -e REDIS_ADDR=redis-host:6379 \
  coyl/ktms:latest
```

A minimal Compose service for your own stack:

```yaml
services:
  ktms:
    image: coyl/ktms:latest
    restart: unless-stopped
    environment:
      DATABASE_URL: postgres://ktms:${DB_PASSWORD}@db:5432/ktms?sslmode=disable
      KTMS_ENCRYPTION_KEYS: ${KTMS_ENCRYPTION_KEYS}
      REDIS_ADDR: redis:6379
    ports:
      - "9090:9090"
    depends_on: [db, redis]
```

On Kubernetes, point the liveness probe at `GET :8080/healthz` and the readiness probe
at `GET :8080/readyz` (checks the database). Run as many replicas as you like; they share
Postgres and Redis and pick up configuration changes within `ROUTING_RELOAD_SECONDS`.

### Helm chart (Kubernetes)

An official Helm v3 chart is provided under [`deploy/helm/ktms`](deploy/helm/ktms):

```bash
# 1. Create a secret with your database URL and encryption keys
kubectl create secret generic ktms-secrets \
  --from-literal=DATABASE_URL='postgres://ktms:pass@db:5432/ktms?sslmode=require' \
  --from-literal=KTMS_ENCRYPTION_KEYS="1:$(head -c 32 /dev/urandom | base64)" \
  --from-literal=BOOTSTRAP_ADMIN_KEY="$(head -c 24 /dev/urandom | base64 | tr -d '/+=')"

# 2. Install the release
helm install ktms ./deploy/helm/ktms \
  --set secrets.existingSecret=ktms-secrets \
  --set ingress.enabled=true \
  --set "ingress.hosts[0].host=proxy.example.com" \
  --set "ingress.hosts[0].paths[0].path=/" \
  --set "ingress.hosts[0].paths[0].pathType=Prefix"
```

The chart supports non-root security context (`appuser:10001`), HorizontalPodAutoscaler, Prometheus Operator `ServiceMonitor`, and separate gRPC Ingress configuration. See [`deploy/helm/ktms/README.md`](deploy/helm/ktms/README.md) for full configuration parameters.

### Building from source

Requires Go 1.26+ (and `ffmpeg` on `PATH` if you use speech).

```bash
make build                       # → bin/ktms-server and bin/ktms-node
export DATABASE_URL=postgres://ktms:ktms@localhost:5432/ktms?sslmode=disable
export KTMS_ENCRYPTION_KEYS="1:$(head -c 32 /dev/urandom | base64)"
./bin/ktms-server
```

Or build the image yourself: `docker build -t ktms .`

### Node agent

`bin/ktms-node` runs next to a local inference engine (`ollama`, `vllm`, `tgi`, `llamacpp`),
registers it with KTMS and sends heartbeats with the loaded models, GPU metrics and the
engine's load: running and waiting requests from the `/metrics` of vLLM, TGI and llama.cpp
(start `llama-server` with `--metrics`). Ollama has no such API, so its nodes report no queue. It
authenticates with a node key minted with the `MintNodeKey` admin RPC. KTMS dials the node,
so the node must be reachable; the agent can do that itself:

```bash
export NODE_KEY=...                       # or --node-key-file
./bin/ktms-node --proxy-addr proxy.example.com:443 --tls \
  --engine ollama --expose=cloudflared    # needs the cloudflared binary
```

With `--expose=cloudflared` the agent starts a localhost gateway in front of the engine and a
Cloudflare quick tunnel in front of the gateway, registers the tunnel address, and registers
again whenever the tunnel restarts and its address changes. The gateway checks a per-process
access key (sent to the proxy as the node auth token, never printed) and forwards only
`POST /v1/chat/completions` and `GET /v1/models`; pulling or deleting models is not reachable.
Slow responses are kept alive with padding so the tunnel's 100 s idle cutoff does not drop them.
Quick tunnels have no SLA; for a stable address use `--expose=none` and your own `--endpoint`.

Flags: `--proxy-addr` (default `127.0.0.1:50051`), `--tls` (dial over TLS with the system CA
roots, for a proxy behind a TLS load balancer; off by default), `--node-id` (defaults to the
hostname), `--node-name`, `--engine` (`ollama`, `vllm`, `tgi`, `llamacpp`, `decisions`, `generic`),
`--base-weight`, `--max-concurrent`.
`--judge-model` (repeat or comma-separate) tags specific models on the node with the `judge` capability
for local JEPA / decision evaluation, routing `/v1/decisions` and `Judge` calls to them.
`--only-model` (repeat it or comma-separate) reports only the named models to the proxy, for an
engine that shares its model directory with other tools; by default every model the engine has is
reported. If the node key restricts `allowed_models`, the proxy ignores (and logs) other reported
models and registers the node with the allowed ones; it refuses only when none is allowed.
`--engine-url` is where the agent itself polls the engine (default `http://127.0.0.1:11434`);
`--endpoint` is the address reported to the proxy, which dials it (default: `--engine-url`).
`--expose` is `none` (default; you supply `--endpoint`) or `cloudflared`, with
`--cloudflared-path` to pick the binary. `NODE_AUTH_TOKEN` is the bearer the engine itself
requires, if any.

If a node does not show up or serves badly, `ktms-node doctor` takes the same flags (plus
`--model`) and says in plain words what is wrong: engine not answering, model missing or not
loaded, Ollama on the CPU (an Intel build under Rosetta on Apple Silicon), proxy unreachable or
needing/not needing `--tls`, a node key that is wrong or bound to another node id, or a tunnel
that cannot come up. It also asks the proxy to vet the exact models the agent would report
against the key's `allowed_models` (nothing is stored). It exits 1 if it finds a problem.

While the proxy refuses the node (wrong key, other node id, no allowed model) the agent does not
exit and does not open a tunnel: it checks with the proxy first, then retries with a pause that
grows to five minutes and says why in the log, so a supervisor never restarts it into a fresh
Cloudflare tunnel per attempt.

### Production checklist

- **TLS.** The server speaks plaintext gRPC. Terminate TLS in front of it (Envoy, nginx
  with `grpc_pass`, a service mesh, a cloud load balancer) or keep it on a private network.
  Node agents that reach the proxy through that TLS endpoint run with `--tls`.
- **Back up the encryption keyring**, apart from the database: without `KTMS_ENCRYPTION_KEYS` the
  stored vendor keys are unreadable. See [Backups](#backups-and-the-encryption-keyring).
- **Mint real keys, then unset `BOOTSTRAP_ADMIN_KEY`.** It is accepted as an implicit admin key.
- **Run Redis if you want limits.** With `REDIS_ADDR` empty, RPM, token budgets and spend
  limits are **not enforced** (the proxy fails open by design, and does the same if Redis
  errors; set `SPEND_LIMITS_FAIL_MODE=closed` to refuse spend-limited calls instead).
- **Give each client its own API key** (and its own vendor credentials), so spend, models
  and revocation are per client.
- **Mind your Postgres connection count.** Each replica holds up to `DB_MAX_CONNS`
  (default 25) connections.
- **Back up Postgres.** All configuration lives there; see [Backups](#backups-and-the-encryption-keyring).

## Configuration reference

Everything is an environment variable; see [`env.example`](env.example).

| Variable | Default | Description |
| --- | --- | --- |
| `DATABASE_URL` | – | **Required.** Postgres connection string. |
| `KTMS_ENCRYPTION_KEYS` / `LLMPROXY_ENCRYPTION_KEYS` | – | **Required.** Versioned keyring `1:<base64 32 bytes>[,2:…]`. The highest version encrypts new writes; all listed versions can decrypt. |
| `GRPC_ADDR` | `:9090` | gRPC listen address (data and admin planes). |
| `GRPC_MAX_RECV_MSG_SIZE` | `16777216` | Maximum gRPC message receive size in bytes (16MB). |
| `HTTP_ADDR` | `:8080` | HTTP listen address (health probes and the OpenAI-compatible API). |
| `HTTP_API_ENABLED` | `true` | Serve the `/v1` API on `HTTP_ADDR`; `false` leaves only the health probes. |
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
| `BOOTSTRAP_ADMIN_KEY` | empty | Implicit admin key for creating the first real key. Unset afterwards. |
| `USAGE_COLLECTOR_HOST` | empty | `host:port` of a gRPC usage sink. Empty disables reporting. |
| `USAGE_COLLECTOR_TLS` | `false` | Reach the sink over TLS, e.g. light-tokenmeter on Cloud Run (`host:443`). |
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
| `GUARDRAILS_LATENT_ENABLED` | `false` | Experimental. With `GUARDRAILS_ENABLED`, also screen prompts in latent space against injection, jailbreak, prompt-leak and exfiltration centroids (see [Latent guardrails](#semantic-cache-and-latent-guardrails)). |
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
config, so an invalid state can never become active. Prefer a browser to `grpcurl`? See the
[web console](#web-console).

### 1. API keys

```bash
grpcurl -plaintext -H "authorization: Bearer $ADMIN_KEY" \
  -d '{"name":"billing-service","scopes":["generate"]}' \
  localhost:9090 ktms.v1.AdminService/MintAPIKey
```

Keys look like `ktms_<hex>` (keys minted before the rename start with `llm_` and keep working). Only a hash is stored, and the plaintext is returned once.
Scopes: `generate` (data plane) and `admin` (implies everything).
`ListAPIKeys` and `RevokeAPIKey` do what they say.

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
| `no_thinking` | The model reasons by default and bills the reasoning as output (DeepSeek V4, Gemma 4 on Ollama). The proxy turns it off: `thinking: {"type": "disabled"}` for DeepSeek, `reasoning_effort: "none"` for Ollama, and for `local` models when the serving node runs Ollama (other engines get no switch). OpenAI-compatible vendors only. |
| `vision` | Can process image parts in multimodal messages (`ImagePart`). |
| `audio_input` | Can process audio parts in multimodal messages (`AudioPart`). |
| `pdf_input` | Can process PDF document parts in multimodal messages (`DocumentPart`). |
| `tts` | Can serve `SynthesizeSpeech`. |
| `image` | Can serve `GenerateImage`. |
| `judge` | Can serve `Judge` (TypeSafe `jev-latest`, OpenAI `gpt-6-luna`). |
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
  (`estimated input × input price + max tokens × output price`, or
  `SPEND_DEFAULT_OUTPUT_RESERVE_TOKENS` without max tokens) is reserved atomically on every
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
- Rules can match `org_id` and `team_id`. The proxy sets them from the key; callers cannot.
- Deleting a team or organization detaches revoked keys and is `FAILED_PRECONDITION` while
  active keys remain: revoke or move them first.

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
   keys encrypted with it (see [Backups](#backups-and-the-encryption-keyring)). Until then,
   leaving it listed is harmless. Cached responses encrypted with a removed version are
   treated as cache misses.

`TestRotateEncryptionLetsTheOldKeyGo` runs this sequence against Postgres in CI.

## Run a node

Lend a machine's GPU to the proxy: an Apple Silicon Mac, or Linux with an NVIDIA card (or just a
CPU, slowly). Three steps; no tunnels or service files to write by hand, and no admin rights on macOS.

**1. Get a key.** Whoever runs the proxy mints one key per machine, bound to a node id, and gives
you the key (`ktms_node_...`), the node id and the proxy address. They also need a `local` model
whose `id` is exactly the Ollama tag the node serves (`G` and `ADMIN` as in the quick start;
drop `-plaintext` for a proxy behind TLS):

```bash
# admin, once per machine
$G "$ADMIN" -d '{"name":"anna-mac","node_id":"anna-mac"}' \
  proxy.example.com:443 ktms.v1.AdminService/MintNodeKey      # plain_key shown once

# admin, once per model; no_thinking turns Gemma's reasoning off
$G "$ADMIN" -d '{"model":{"id":"gemma4:26b-a4b-it-qat","vendor":"local",
  "efforts":["EFFORT_LOW","EFFORT_MEDIUM"],"capabilities":["no_thinking"],
  "queue_timeout_ms":2000,"enabled":true}}' \
  proxy.example.com:443 ktms.v1.AdminService/UpsertModel
```

**2. Run one command** on the machine (replace the three values), and paste the key when asked.
The input is hidden and the key is stored in a file only you can read:

```bash
curl -fsSL https://raw.githubusercontent.com/ai-process/ktms/vX.Y.Z/scripts/install-node.sh | sh -s -- \
  --proxy proxy.example.com --node-id anna-mac --model gemma4:26b-a4b-it-qat
```

It downloads Ollama, the model and the node agent, starts them as background services and keeps
the machine awake, and waits until the node has registered. Linux services start at boot. On a
Mac they are login items: after a reboot the node comes back when its user logs in, so turn on
automatic login for an unattended Mac. The model is large, so the first run takes a while. Add
`--max-concurrency N` for a machine that can serve more than one request at a time (default 1).

**3. Check.** The last lines say `Node anna-mac is running and registered`. The proxy admin
confirms with `ListNodes` that the node is `HEALTHY` and has the model.

Run the same command again to update; a re-run only restarts what changed, and a new agent that
fails to start is rolled back. Remove everything with the same command plus `--uninstall`
(downloaded models stay in `~/.ollama`).

### If the node does not show up

Run the same command with `--doctor`. It says in plain words what is wrong: Ollama not answering,
the model missing or not loaded, Ollama on the CPU (an Intel build running through Rosetta), the
proxy unreachable or `--tls` wrong, a key that is wrong or belongs to another node id, or a tunnel
that cannot come up (outbound port 7844 blocked). Logs are in `~/.ktms-node/logs/`.

| Symptom | Cause and fix |
| --- | --- |
| `Killed: 9`, or "Apple could not verify..." | The agent came through a browser download, which macOS quarantines. Use the `curl` command above; `curl` and `brew` files are not quarantined. |
| Very slow, GPU idle | Intel Ollama through Rosetta, or no NVIDIA driver. The installer uses its own native Ollama and says when it ignores an Intel one. |
| `key belongs to node "x"` | The key is bound to one machine. Use the node id it was minted for, or ask for a new key. |
| `model "x" not permitted for this node key` | The key's `allowed_models` excludes a model on the machine. The installer reports only `--model`; for a hand-run agent pass `--only-model`. |
| Needs a new key | Re-run with `--new-key`. |
| Address changed after a restart | Expected: quick tunnel addresses change, and the node re-registers by itself. |
| Node is offline after a Mac reboot | macOS starts the services at login, not at boot. Log in, or enable automatic login (System Settings > Users & Groups). |
| The machine sleeps | The services keep it awake while they run, on a Mac only while plugged in. Closing a laptop lid still sleeps it. |

Installs on Linux need `zstd` (Ubuntu: `sudo apt install zstd`) and a normal login session
(systemd user services). The installer script's other options are in `install-node.sh --help`.

## Web console

[**ktms-admin**](https://github.com/ai-process/ktms-admin) is a small web UI for everything in
the previous section: models, routing rules, client API keys, vendor credentials and a playground for
trying a prompt through your rules. It can also show usage and spend per project, user and model when
paired with [light-tokenmeter](https://github.com/ai-process/light-tokenmeter). Sign-in is Google, limited to
an e-mail allowlist with wildcards (`*@example.com`).

```bash
# 1. a key for the console
grpcurl -plaintext -H "authorization: Bearer $BOOTSTRAP_ADMIN_KEY" \
  -d '{"name":"console","scopes":["admin"]}' localhost:9090 ktms.v1.AdminService/MintAPIKey

# 2. run it (see its README for the Google OAuth setup)
docker run -d -p 8081:8080 \
  -e PUBLIC_URL=https://admin.example.com \
  -e GOOGLE_CLIENT_ID=… -e GOOGLE_CLIENT_SECRET=… -e ALLOWED_EMAILS='*@example.com' \
  -e SESSION_SECRET="$(openssl rand -base64 36)" \
  -e KTMS_ADDR=ktms:9090 -e KTMS_ADMIN_KEY=ktms_… \
  coyl/ktms-admin:latest
```

## Calling the proxy

Authenticate every call with `authorization: Bearer ktms_…` (legacy `llm_…` keys supported) gRPC metadata.

You can import the committed Go stubs directly from this repository:
```bash
go get github.com/ai-process/ktms/gen/ktms/v1
```

### Consuming via Buf Schema Registry (BSR)

The gRPC API is published to the [Buf Schema Registry](https://buf.build/ai-process/ktms) (`buf.build/ai-process/ktms`). Pre-generated client SDKs are available across languages:

- **Go (BSR):**
  ```bash
  go get buf.build/gen/go/ai-process/ktms/protocolbuffers/go
  go get buf.build/gen/go/ai-process/ktms/grpc/go
  ```
- **TypeScript / Node / Web (Connect / gRPC-Web):**
  Configure npm for the `@buf` registry, then install the packages:
  ```bash
  npm config set @buf:registry https://buf.build/gen/npm/v1/
  npm install @buf/ai-process_ktms.bufbuild_es @connectrpc/connect
  ```
- **Python:**
  Install from the Buf Python registry:
  ```bash
  pip install --extra-index-url https://buf.build/gen/python \
    ai-process-ktms-protocolbuffers-python \
    ai-process-ktms-grpc-python
  ```

### Local Client Generation

Or generate client stubs locally with `buf` without installing local compiler plugins:
```bash
make proto     # Go stubs in gen/
make proto-py  # Python stubs in gen/python/
make proto-ts  # TypeScript stubs in gen/ts/
```

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
| `effort` | Routing tier (`EFFORT_LOW`, `EFFORT_MEDIUM`, `EFFORT_HIGH`). |
| `attributes` | Opaque key/values matched by rules and copied to usage events. `user_id` charges the per-user daily budget (falls back to `svc:<key_name>` for service callers without `user_id`). `usage_project` overrides the usage project (see below). |
| `base_system_instruction`, `request_instruction` | System content never travels as a message. |
| `response_schema` | Unset = free text. Set = JSON output in that shape: types `string`, `integer`, `number`, `boolean`, `object`, `array`, plus `required`, `nullable`, `enum`, `format` and `additional_properties`. Vendors with structured output enforce it; for the others the proxy checks the reply (types, required keys, enum values, extra keys when `additional_properties` is false), retries once and then falls back. A `format` a vendor rejects is passed to the model in the description instead. |
| `tools`, `tool_choice` | Function/tool declarations with JSON Schema parameters. `tool_choice` specifies auto, required, none, or a specific named function. In non-streaming calls, arguments of `strict` tools are checked against their parameters, since not every vendor enforces strict; a call outside them is retried once and then falls back. Streamed tool calls reach the caller as they arrive and are not checked. |
| `messages` | Chat history. Supports `MESSAGE_ROLE_USER`, `MESSAGE_ROLE_ASSISTANT`, and `MESSAGE_ROLE_TOOL` (with `tool_call_id`). User messages support multimodal `parts` (`ContentPart`: `text`, `image`, `audio`, `document`). Assistant messages can carry `tool_calls`. |
| `max_output_tokens` | Ceiling, `0` = none. Output tokens are the expensive ones; set it. |
| `enable_google_search` | Only Google models declaring `google_search` can serve it. |
| `action_id` | Free-form label for usage attribution. |
| `model_override` | Exact model id; bypasses rules but **not** throttles. |
| `placement` | Compute locality preference: `nodes_first`, `nodes_only`, `cloud_first`, `cloud_only`. |
| `auto_route` | Enables smart model auto-selection; when effort is unpinned, evaluates task complexity via Judge. |
| `effort_specified` | Signals caller explicitly pinned effort (skips dynamic Judge evaluation). |

The response carries `choices`, token `usage`, `resolved_model`, `resolved_vendor`,
`matched_rule` (empty on override), structured `details` (`Choice` objects with text, tool calls, and finish reasons),
`cached` status, and routing metadata: `route_mode`, `placement`, `judge_score`, and `judge_model`.

### GenerateTextStream

Server-streaming RPC `rpc GenerateTextStream(GenerateTextStreamRequest) returns (stream GenerateTextChunk)`.

- Takes a `GenerateTextStreamRequest` containing the standard `GenerateTextRequest`.
- Streams incremental `GenerateTextChunk` items containing `delta` (token text), `tool_call_chunks`, `resolved_model`, `resolved_vendor`, `matched_rule`, `cached`, `route_mode`, `placement`, `judge_score`, `judge_model`, and final `finish_reason` with token `usage`.
- **Fallback guarantee:** If upstream models fail before the first chunk is sent, the router falls back to the next model in the rule's chain. Once the first chunk is transmitted to the caller, the stream is locked to that model to guarantee integrity.
- Usage events and daily token budgets are settled on stream completion.

### Batch text generation

For offline or bulk workflows (such as page generation, backfills, or data pipelines) that prioritize cost over latency, batch generation processes thousands of items asynchronously through vendor Batch APIs (e.g., Google Gemini Batch API) at a **50% discount** compared to interactive calls. Jobs typically finish within 24 hours (vendor timeout at 48 hours).

- **Route resolution:** Resolved once upon submission. The proxy selects the first model in the rule's chain that has the `batch` capability, a vendor key for the client (not needed for `local` models), and available daily budget. Once submitted, there is no vendor fallback; per-item outcomes are returned to the client.
- **Privacy:** Prompts are streamed/submitted to the vendor and never persisted in the proxy database.
- **Throttling & Accounting:** RPM limits do not apply to batches. Daily token budgets are checked at submission (rejecting if already exhausted) and tokens are deducted when final item results arrive. Results are retained for `RESULTS_RETENTION_DAYS` (default 7 days; accepts integer days or Go duration strings).

#### SubmitBatch

Takes a list of items (`custom_id` unique within the batch, and standard `GenerateTextRequest`), batch-level `effort` and `attributes`, and an optional `client_batch_id` for idempotency (resubmitting with the same client batch ID returns the existing batch):

```bash
grpcurl -plaintext -H "authorization: Bearer ktms_<key>" -d '{
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
}' localhost:9090 ktms.v1.GatewayService/SubmitBatch
```

Returns a `Batch` with status `BATCH_STATE_PENDING` (or `BATCH_STATE_RUNNING`), total item count, model, and creation timestamp.

#### GetBatch

Polls batch state (`PENDING`, `RUNNING`, `SUCCEEDED`, `FAILED`, `CANCELLED`, `EXPIRED`) and progress counts (`total`, `done`, `failed`):

```bash
grpcurl -plaintext -H "authorization: Bearer ktms_<key>" -d '{
  "batch_id": "0199c0a0-6228-7e10-91b5-685d996cbfa1"
}' localhost:9090 ktms.v1.GatewayService/GetBatch
```

#### ListBatchResults

Pages through completed items with `page_token` (and `next_page_token`). Each item contains its `custom_id` and either a successful `response` (`GenerateTextResponse` with choices, usage, resolved_model, resolved_vendor, matched_rule) or an `error` (`BatchItemError` with gRPC code, message, and error reason):

```bash
grpcurl -plaintext -H "authorization: Bearer ktms_<key>" -d '{
  "batch_id": "0199c0a0-6228-7e10-91b5-685d996cbfa1",
  "page_size": 100
}' localhost:9090 ktms.v1.GatewayService/ListBatchResults
```

#### CancelBatch

Requests cancellation of an unfinished batch job at the vendor. The batch remains `RUNNING` until vendor confirmation is polled: already completed items preserve their results, while remaining unfinished items fail with reason `batch_cancelled`.

```bash
grpcurl -plaintext -H "authorization: Bearer ktms_<key>" -d '{
  "batch_id": "0199c0a0-6228-7e10-91b5-685d996cbfa1"
}' localhost:9090 ktms.v1.GatewayService/CancelBatch
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

Multimodal state goes in `state_media` (`text`, `image`, `audio`, `embedding` parts), alongside
or instead of `state_json`. Each media kind narrows the chain to judge models that also declare
`vision`, `audio_input` or `embedding_input`; TypeSafe takes JSON state only. Image URLs must be
`https` and are forwarded, not fetched. Guardrails inspect text parts only. Limits: 32 parts,
16 MiB of media, 16384 embedding dimensions.

Supported decision vendors are [TypeSafe](https://typesafe.ai) (`jev-latest`, billing input only) and [OpenAI](https://openai.com) (`gpt-6-luna`, billing input and output tokens); both write no text and declare the `judge` capability.

### Embeddings

Vector representations of text. Vector space isolation is strictly enforced: `auto` model routing and cross-model fallbacks are disallowed to prevent corrupting downstream vector indices. Models must declare the `embedding` capability.

```bash
grpcurl -plaintext -H "authorization: Bearer ktms_<key>" -d '{
  "model": "text-embedding-3-small",
  "inputs": ["Hello world", "Search document"]
}' localhost:9090 ktms.v1.GatewayService/GetEmbeddings
```

### ListModels

Returns the models the calling key can use (those whose vendor it holds a credential for), with
efforts, capabilities, rpm and prices.

## OpenAI-compatible HTTP API

The data plane is also available as HTTP/JSON in the OpenAI wire format, so existing
OpenAI SDKs, LangChain, `curl` and similar tools work by changing the base URL and key.
It runs on `HTTP_ADDR` (default `:8080`) and shares everything with gRPC: the same
`ktms_…` API keys (scope `generate`), vendor credentials, rules, fallbacks, throttling and
usage reporting. Set `HTTP_API_ENABLED=false` to turn it off.

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="ktms_<your key>")
reply = client.chat.completions.create(
    model="auto",                                # let the rules choose
    messages=[{"role": "user", "content": "Say hi in three words."}],
)
print(reply.choices[0].message.content, reply.model)   # `model` is the one that answered
```

```bash
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer ktms_<your key>" -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Say hi in three words."}]}'
```

### Endpoints

| Endpoint | Maps to | Notes |
| --- | --- | --- |
| `POST /v1/chat/completions` | `GenerateText` / `GenerateTextStream` | Text, multimodal parts (`image_url`, `input_audio`, `file`/`document`), structured output, `tools`, and SSE streaming. |
| `POST /v1/embeddings` | `GetEmbeddings` | Vector embeddings with `float` or `base64` encoding format and optional dimensionality. |
| `POST /v1/audio/speech` | `SynthesizeSpeech` | Returns OGG/Opus audio. |
| `POST /v1/images/generations` | `GenerateImage` | Returns `b64_json`. |
| `POST /v1/decisions` | `Judge` | OpenAI Decisions API format (`predicate`, `choice`, `score`). `input` is JSON state, or an array of `input_text`/`text`, `input_image`/`image_url`, `input_audio` and `input_embedding` parts for multimodal state. |
| `POST /v1/decisions/rollouts` | `EvaluateRollouts` | Batch trajectory and rollout candidate evaluation for agentic workflows with candidate ranking. |
| `POST /v1/judge` | `Judge` | Protobuf JSON of `JudgeRequest` / `JudgeResponse`. |
| `GET /v1/models`, `GET /v1/models/{id}` | `ListModels` | Only models the key holds a vendor credential for. |

Authentication is `Authorization: Bearer ktms_…` (legacy `llm_…` keys are also supported). Response headers provide full observability:
- `X-KTMS-Vendor` and `X-KTMS-Rule`: vendor and matching rule that served the request.
- `X-KTMS-Placement`: infrastructure placement (`nodes_first`, `nodes_only`, `cloud_first`, `cloud_only`).
- `X-KTMS-Route-Mode`: routing mechanism (`direct`, `override`, `auto-judge`, `auto-default`, `auto-effort`).
- `X-KTMS-Judge-Score` and `X-KTMS-Judge-Model`: evaluation confidence and judge model ID when judge evaluates task complexity.

> [!NOTE]
> **Backwards Compatibility**: All legacy `X-LLM-Proxy-*` response headers are dual-emitted alongside `X-KTMS-*` with standard RFC 8594 `Deprecation: true`, `Sunset`, and `Warning` headers to provide seamless zero-downtime migration for existing clients.

### Choosing a model and placement

The `model` parameter supports smart auto-routing and infrastructure placement:

| `model` | Behaviour |
| --- | --- |
| `auto` (or empty) | Smart auto-selection with default `nodes_first` placement. Evaluates prompt complexity via Judge (or uses `reasoning_effort` if provided). |
| `auto:low`, `auto:medium`, `auto:high` | Pinned effort level with `nodes_first` placement. |
| `auto:nodes-first`, `auto:nodes-only`, `auto:cloud-first`, `auto:cloud-only` | Auto-routing with explicit compute locality policy. |
| `auto:<placement>:<effort>` (e.g. `auto:nodes-only:high`) | Combined placement policy and pinned effort level. |
| anything else | Exact registry model ID (`model_override`): bypasses rules but not throttles. |

#### Infrastructure Placement Policies
- `nodes_first` (default): Prefers registered self-hosted compute nodes (`VendorLocal`, `VendorOllama`), falling back to cloud providers if unavailable or busy.
- `nodes_only` (aliases: `nodes`, `private`, `self_hosted`): Restricts routing strictly to self-hosted nodes. Fails if no capable node is available.
- `cloud_first`: Prefers managed cloud providers, falling back to registered nodes.
- `cloud_only`: Excludes self-hosted nodes completely.

Placement precedence:
1. `X-KTMS-Placement` (or legacy `X-LLM-Proxy-Placement`) HTTP header
2. Request body `"placement": "..."`
3. Model suffix (e.g. `model: "auto:nodes-only"`)
4. Default: `nodes_first`

#### Fast Judge Auto-Selection
When `model: "auto"` is requested without an explicit effort level, the proxy evaluates prompt complexity using the configured Judge model (such as `jev-latest` or OpenAI Decisions) with a 250ms deadline. If the judge is not configured or times out, it safely falls back to standard medium effort without dropping the request.

Effort precedence:
1. Pinned effort in model string (e.g. `auto:low`)
2. `reasoning_effort` request field (`low`, `medium`, `high`) — skips judge evaluation
3. Dynamic Judge evaluation (fails open to `medium`)

Routing attributes (what rules match on, and what usage events carry) come from OpenAI's own
fields: `metadata` is passed through as attributes, `user` becomes `attributes["user_id"]`
(unless `metadata.user_id` is set), and `metadata.action_id` is used as the usage action label.

```json
{"model": "auto:nodes-first", "user": "u-42", "metadata": {"subject": "french", "action_id": "app.quiz"},
 "messages": [{"role": "user", "content": "Translate: good morning"}]}
```

### Chat completions: what is supported

| Request field | Support |
| --- | --- |
| `messages` | `system`/`developer` messages become the system instruction, `user`/`assistant` the conversation. Content is a string or text parts. |
| `max_tokens`, `max_completion_tokens` | Output ceiling (`max_output_tokens`). Set one: an unbounded answer is the costliest failure. |
| `response_format` | `json_schema` is enforced: `type` (incl. `number` and `["string","null"]`), `properties`, `items`, `required`, `enum`, `format` and boolean `additionalProperties` are carried. Enums on non-string types, and formats a vendor rejects, reach the model as a hint in the description. Other keywords (`minimum`, `pattern`, `oneOf`, ...) are dropped. `json_object` asks for JSON in the prompt. |
| `stream` | Real incremental SSE streaming backed by gRPC server streaming. Once the first chunk is emitted, stream is locked to the selected model; `stream_options.include_usage` is honoured on completion. |
| `reasoning_effort`, `user`, `metadata` | Routing, as above. |
| `enable_google_search` | Extension: let a Google model ground the answer in search. |
| `n` | Must be 1. |
| `tools`, `tool_choice` | Supported. Function declarations with JSON Schema parameters. `tool_choice` supports `"auto"`, `"none"`, `"required"`, or specific function names. |
| `role: "tool"` | Supported. Tool responses with `tool_call_id`. |
| Image/audio content parts | Rejected with `400`. |
| `temperature`, `top_p`, `stop`, `seed`, `logprobs`, penalties… | Accepted and ignored. |

The response is a standard `chat.completion` with `finish_reason` (`"stop"`, `"tool_calls"`, `"length"`, or `"content_filter"`) and token `usage`.

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
| 429 | `insufficient_quota` | `insufficient_quota`; the message names the cap, e.g. `throttled:spend_key_daily`. `Retry-After` is the seconds until the period resets (UTC midnight or month start). |
| 501 | `server_error` | `ffmpeg_missing` (Gemini speech synthesis invoked without ffmpeg on PATH or in container) |
| 502 | `server_error` | `vendor_error` |
| 503 | `server_error` | `vendors_unavailable`, `auth_unavailable`, `storage_unavailable`, `throttled:spend_unavailable` (with `SPEND_LIMITS_FAIL_MODE=closed`): retry with backoff |

OpenAI SDKs retry 429 and 5xx automatically; for `throttled:*` and `vendor_error` you may want to
disable that (`max_retries=0`) and handle them yourself.

## Concepts

**Effort** is the only routing field the proxy understands; everything else about a request's
purpose travels in free-form attributes, so the proxy knows nothing about your product.

**Throttling & Wait Buffer:**
- **GCRA Rate Limiting:** Enforces requests-per-minute (`rpm`) via the Generic Cell Rate Algorithm in Redis, with configurable burst capacity (`rpm_burst`, defaulting to `rpm - 1` to preserve classic fixed-window behavior, `< 0` to disable burst). Key limits and user limits are evaluated atomically.
- **User & Model Overrides:** Configure fine-grained limits per user and model with `SetUserRateLimit` (supports `*` wildcards for user-wide or model-wide defaults).
- **Spend Limits:** Hard USD caps per API key and per end user (UTC day or month), reserved atomically before each call and settled to the actual cost; see [Spend limits](#5-spend-limits-optional).
- **Daily Token Budgets:** Limits tokens per API key (`daily_tokens_per_key`) and per end-user (`daily_tokens_per_user`, UTC day). A configurable grace margin (`budget_grace_margin`, default 5%, `< 0` to strictly disable) absorbs requests that exceed the budget on their last call without refusing them mid-stream.
- **Inference Pool Wait Buffer:** For local node pools, requests waiting for free slots use jittered backoff polling up to `queue_timeout_ms`. Interactive requests (non-service callers) are automatically capped to 250ms to safeguard Time-To-First-Token (TTFT). Little's Law predictive admission evaluates the queue depth (KTMS's own leases plus other clients' requests waiting in the engine: what the heartbeat saw waiting beyond KTMS's leases on that node; node selection weighs the same load) and estimated duration, immediately falling back to the next model in the chain if expected wait time exceeds the timeout. A slot lease is renewed for as long as its call runs, unary or streaming, so a long generation never frees a slot the node is still using.
- A throttled model is skipped in the fallback chain; if none of the chain is available the caller gets `RESOURCE_EXHAUSTED`.

**Upstream Circuit Breaker:**
- Monitors consecutive upstream failures (5xx server errors, network connection drops, connect/TLS handshake timeouts, and upstream timeouts via `UPSTREAM_REQUEST_TIMEOUT`) per `(vendor, model)`. Caller client aborts and overall client deadline cancellations do not trip the circuit breaker, but vendor stalls beyond `UPSTREAM_REQUEST_TIMEOUT` (total duration for unary/media, per-chunk idle watchdog for streaming) reliably trip it and trigger immediate fallback to the next candidate model.
- When consecutive failures reach `CIRCUIT_BREAKER_FAIL_THRESHOLD` (default 5), the circuit trips to `OPEN`.
- Requests skip degraded upstream models immediately, without a network round trip.
- After `CIRCUIT_BREAKER_COOLDOWN` (default 30s), transitions to `HALF_OPEN` and admits a single probe request. Success restores `CLOSED`; failure re-opens the circuit.
- If all models in the chain have open circuits, the caller receives `UNAVAILABLE vendors_unavailable`.

**Vendor back-off (`Retry-After`):**
- Vendor SDKs run with their built-in retries off, so one router attempt is exactly one vendor call. The router retries a `429`, `5xx` or transport failure (connection reset, EOF on a reused connection) once on the same model before falling back, for text, media and streaming calls (a stream only until its first chunk).
- The vendor's wait is read from `Retry-After` or `retry-after-ms` (OpenAI-compatible APIs, Azure, Anthropic) and from `google.rpc.RetryInfo` (Gemini). Bedrock throttling carries no wait, so it is retried and falls back as before.
- A wait of up to 1s is honoured in place before the same-model retry. A longer one on a `429` or `503` pauses the model for that client's vendor credential, up to `VENDOR_COOLDOWN_MAX`, and the chain moves on. Clients sharing one vendor key share the pause; other credentials are unaffected. With pauses off, the ordinary same-model retry applies. Shadow traffic respects pauses but never starts one.
- Paused models are skipped without a network round trip (`ktms_fallbacks_total{reason="cooldown"}`; pauses started are counted in `ktms_vendor_cooldowns_total`). Pauses live in each replica's memory, like circuit breakers, so each replica learns of a pause from its own first rejected call.
- If every model in the chain is paused, the caller receives `UNAVAILABLE vendors_unavailable` with a gRPC `RetryInfo` for the soonest pause to end; the OpenAI-compatible HTTP API returns it as a `Retry-After` header. When any model failed for another reason, no wait is promised: it may serve the next request.
- Local node models are never paused: the node pool has its own admission control.

**Usage reporting.** When `USAGE_COLLECTOR_HOST` is set, every call emits an event (vendor,
model, tokens, duration, status, `action_id`, rule name and your attributes as metadata) to a
gRPC service implementing [`usage-proto/usage/v1/usage.proto`](usage-proto/usage/v1/usage.proto).
Events are attributed to the project named by the `usage_project` attribute, falling back to the
API key's name. Any service implementing `RecordUsageEvent(s)` works as a sink.

### Semantic cache and latent guardrails

Both project text into a representation space with the encoder at `LATENT_ENCODER_URL` (a JEPA
or any embedding model behind an OpenAI-compatible `/v1/embeddings`), falling back to a local,
lexical hash encoder that folds case, homoglyphs, zero-width and spaced-out letters.

- **Semantic cache.** After an exact-cache miss, a request with `enable_cache` compares its final
  user message (or `request_instruction` when there are no messages) with earlier ones whose
  everything else — key, config version, system prompt, history, tools, effort, attributes and
  every number in the message — matches exactly. A hit at `SEMANTIC_CACHE_SIMILARITY_THRESHOLD`
  returns `cached: true` and usage metadata `cache_match=semantic` with `cache_similarity`.
  Prompts containing PII and responses with tool calls are never cached; with guardrails on, the
  message is screened before a near match is served. Vectors and responses are encrypted at rest.
- **Latent guardrails.** Each prompt is scored against category centroids; the nearest one at
  `GUARDRAILS_LATENT_THRESHOLD` blocks (or logs) with `guardrail_violation: latent <category>
  intent detected`. An unreachable encoder fails open after the regex checks have run.
- **Overhead.** The local encoder runs in-process; measure screening and semantic lookup cost on
  your hardware with `go test -bench . ./internal/latent ./internal/guardrails ./internal/cache`.
  A remote encoder adds its own latency, bounded by `LATENT_ENCODER_TIMEOUT`.

## Error contract

### RPC status codes

| gRPC status | Reason | What to do |
| --- | --- | --- |
| `NOT_FOUND` | – | Unknown batch ID. |
| `INVALID_ARGUMENT` | `guardrail_violation` | Blocked due to detected prompt injection, PII, or a latent-space threat match. Clean input before retrying. |
| `INVALID_ARGUMENT` | `bad_request: <message>` | Empty batch, duplicate `custom_id`, too many items (> `BATCH_MAX_ITEMS`), or invalid parameters. |
| `UNAVAILABLE` | `vendors_unavailable` | The whole chain failed: retry with backoff. When vendors paused every model in the chain with `Retry-After`, details carry `RetryInfo` (HTTP: `Retry-After` header): wait that long first. |
| `UNAVAILABLE` | `auth_unavailable`, `storage_unavailable` | Postgres is unreachable for a key check (past `AUTH_STALE_IF_ERROR`) or a batch or admin write. Retry with backoff. |
| `RESOURCE_EXHAUSTED` | `throttled:rpm`, `throttled:budget_key`, `throttled:budget_user` | Back off; don't hot-retry. Daily budget exhausted before batch submission. |
| `RESOURCE_EXHAUSTED` | `throttled:spend_key_daily`, `throttled:spend_key_monthly`, `throttled:spend_user_daily`, `throttled:spend_user_monthly`, `throttled:spend_team_monthly` | Spend limit reached. Details carry `ErrorInfo{reason: "insufficient_quota"}` and `RetryInfo` until the period resets. |
| `UNAVAILABLE` | `throttled:spend_unavailable` | Spend counters unreachable with `SPEND_LIMITS_FAIL_MODE=closed`. Retry with backoff. |
| `PERMISSION_DENIED` | `model "<id>" not allowed for team`, `no models in chain allowed for team`, `model access denied by team policy` (batch) | The key's team may not use that model. |
| `FAILED_PRECONDITION` | `output_truncated` | Raise `max_output_tokens`; don't retry as-is. |
| `FAILED_PRECONDITION` | `no_capable_model` | No model in the matched chain can serve this request, modality, or batch capability. |
| `FAILED_PRECONDITION` | `no_vendor_key` | The client key holds no credential for the chain's vendors. `local` models never cause this. |
| `FAILED_PRECONDITION` | `not_configured` | No models/rules configured yet. |
| `FAILED_PRECONDITION` | `batch not configured` | Batch processing is not enabled/configured on the proxy (no batch store). |
| `UNIMPLEMENTED` | `ffmpeg_missing` | Gemini speech synthesis invoked without ffmpeg on PATH or running slim container. Install ffmpeg or switch to `:audio` image. |
| `INTERNAL` | `vendor_error` | Terminal vendor failure. |
| `UNAUTHENTICATED` / `PERMISSION_DENIED` | – | Missing/invalid key, or key lacks the scope. |

When to retry: `UNAVAILABLE` with backoff, and `RESOURCE_EXHAUSTED` after the wait it carries
(`RetryInfo`, HTTP `Retry-After`) or with backoff when it carries none. Every other code fails the
same way until the request or the configuration changes.

### Batch item error reasons

Items returned in `ListBatchResults` that failed have an `error` containing a gRPC code, a message, and one of these reasons:

| Reason | When | What to do |
| --- | --- | --- |
| `output_truncated` | Item reached `max_output_tokens` or candidate finish limit. | Re-submit item with higher `max_output_tokens`. |
| `vendor_error` | Vendor error occurred during processing or structured schema validation failed. | Retry synchronously or resubmit in another batch. |
| `batch_expired` | Vendor batch expired (after 48 h) before the item finished. | Re-submit the item. |
| `batch_cancelled` | Batch was cancelled before the item completed. | Re-submit if still needed. |

## API stability

From 1.0, KTMS follows [semantic versioning](https://semver.org): within 1.x nothing in the public
API below changes incompatibly. Breaking changes wait for 2.0, except the one removal announced
before 1.0 and listed under [Deprecated](#deprecated). Minor releases add features; patch releases
fix bugs.

### What is public

| Surface | What stays compatible in 1.x |
| --- | --- |
| gRPC `ktms.v1` (`GatewayService`, `AdminService`, `NodeRegistryService`) | No RPC, field, enum value or message is removed or renamed, and no field number or type changes. New RPCs and fields may appear. `buf breaking` checks every pull request. |
| HTTP `/v1` (OpenAI-compatible) and `X-KTMS-*` headers | Paths, request and response fields, and header names, except the experimental ones listed below. New fields and endpoints may appear. |
| Probes and metrics endpoints | `/healthz` and `/readyz` on `HTTP_ADDR`, `/metrics` and `/healthz` on `METRICS_ADDR`. |
| Error contract | The status codes and reason strings in [Error contract](#error-contract) and the HTTP [Errors](#errors) table keep their meaning. New reasons may appear, so treat unknown ones like their status code. |
| Environment variables | Names, defaults and meaning of every variable in [Configuration reference](#configuration-reference). |
| Database | Any 1.x release upgrades the database of an earlier one by itself (`AUTO_MIGRATE`, forward-only). No manual SQL steps. The tables are not an API: read and write through the RPCs. |
| Prometheus metrics | Names and labels of `ktms_*` metrics, except the experimental ones listed below. |
| Usage events (`usage.v1`) | Field numbers and meaning, checked by `buf breaking` like `ktms.v1`. |
| Node agent | A 1.x agent works with any 1.x gateway, in both directions. |

Not public, and free to change in any release: Redis key layout, log message text, the
`internal/` Go packages, CLI output of the tools in `cmd/` other than their flags, and benchmark
numbers.

### Experimental

These work and are supported, but may change in a minor release. The change is then listed in the
[CHANGELOG](CHANGELOG.md). In the proto they carry an `Experimental:` comment.

- **The judge's effort choice:** when a request is auto-routed without a pinned effort
  (`model: "auto"` or `auto:<placement>` over HTTP without `reasoning_effort`; `auto_route` with
  `effort_specified` false in gRPC), a judge picks the effort. Its choice, the `auto-judge` and
  `auto-default` values of `route_mode`, `judge_score`, `judge_model` and the
  `X-KTMS-Judge-Score` and `X-KTMS-Judge-Model` headers are experimental. Auto-routing with a
  pinned effort (`auto:<effort>`, `auto:<placement>:<effort>`, route mode `auto-effort`),
  placement policies, `model_override` and the `X-KTMS-Route-Mode` header are stable.
- **Shadow traffic and its evaluation:** the `shadow_model`, `shadow_percentage` and `shadow_eval`
  rule fields, and the `ktms_shadow_*` metrics.
- **Latent features:** the semantic cache (`SEMANTIC_CACHE_*`), latent guardrails
  (`GUARDRAILS_LATENT_*`), the encoder settings (`LATENT_*`), and embeddings in judge state
  (`EmbeddingPart`, `input_embedding` over HTTP).
- **`JudgeStream` and `EvaluateRollouts`**, and `POST /v1/decisions/rollouts` over HTTP.
- **The Helm chart** in `deploy/helm/ktms`, until it is published as a versioned OCI chart. Until
  then its values may change.

### Upgrading

- **Within 1.x, upgrade in one step** from any earlier 1.x release: migrations run forward at
  startup and cover every release in between. `scripts/upgrade-test.sh` upgrades from the
  previous release and from the oldest release of the same major on every pull request and
  before each release.
- **Rolling upgrades work within 1.x:** migrations only add, so replicas of the previous release
  keep serving on the migrated database while new ones start; the upgrade test runs the old
  release again after the new one migrated. A change that cannot be additive waits for 2.0.
  Until the roll finishes, old replicas do not apply what the new release added (for example a
  new kind of limit), so finish rolls promptly.
- **Downgrades are not supported.** Migrations have no down step; to go back, restore the
  database backup taken before the upgrade.
- **From 0.x, upgrade to 1.0 first,** then to later 1.x releases: 0.x made no compatibility
  promise, and only the 0.13 → 1.0 step is tested (by the 1.0 release).
- **Read the [CHANGELOG](CHANGELOG.md)** before upgrading: it lists every change in Added,
  Changed, Deprecated, Removed, Fixed and Security.

### Deprecated

Deprecated names keep working through 1.x, log a warning (or count in
`ktms_legacy_headers_requests_total`), and are removed in 2.0. One exception is announced in
advance:

| Deprecated | Use instead | Removed |
| --- | --- | --- |
| `LLMPROXY_ENCRYPTION_KEYS` | `KTMS_ENCRYPTION_KEYS` | 2.0 |
| `X-LLM-Proxy-*` request and response headers | `X-KTMS-*` | On their `Sunset` date, 2027-03-01, as already announced in the responses |

## Operations

- **Health:** `GET :8080/healthz` (process up) and `GET :8080/readyz` (database reachable).
- **Logs:** structured JSON via zerolog. Vendor keys, client keys and the keyring are never logged.
- **Config changes** are picked up by all replicas within `ROUTING_RELOAD_SECONDS`; the replica that
  handled the admin call applies them immediately.
- **Upgrades:** migrations run at startup under a database advisory lock, so rolling deploys are
  safe. Roll forward by deploying a newer image tag; see [Upgrading](#upgrading).
- **Graceful shutdown:** on SIGTERM in-flight calls get `SHUTDOWN_TIMEOUT` to finish.

### Backups and the encryption keyring

- **Back up Postgres.** It holds everything durable: client keys (as hashes), vendor keys
  (encrypted), models, rules, limits, tenants, node keys and batches. Use `pg_dump` or your
  provider's snapshots, and test a restore.
- **Back up the keyring separately.** `KTMS_ENCRYPTION_KEYS` is not in the database. Keep it in a
  secret manager, not next to the dumps: a dump plus the keyring is every vendor key in
  plaintext. **If the keyring is lost, every stored vendor key is lost**, and each has to be set
  again with `SetVendorKey`; client keys, config and limits survive.
- **Keep old key versions as long as the backups that need them.** A dump taken before a
  rotation can only be read with the versions it was written with.
- **Restore:** stop every replica, restore the dump, **flush the gateway's Redis keys** (the
  `REDIS_KEY_PREFIX` namespace), then start a release at least as new as the one that took the
  dump. The flush matters: cached responses are keyed by the config version, which the restore
  rolls back, so later changes would reuse version numbers whose cache entries belong to another
  config. An older release could run on the restored schema but would ignore what was added
  after it, so do not restore with one.
- **Redis is not backed up.** It holds rate, token and spend counters, the response and semantic
  caches, and the node pool: registrations (with encrypted node auth tokens), heartbeats, slot
  leases, the node circuit breaker and drain flags. Losing it resets the current day's and
  month's counters (spend limits then allow up to one more full cap in the current period),
  empties the caches, drops every node from routing until its next heartbeat re-registers it,
  and clears drain flags, so drained nodes take traffic again.

### When a dependency fails

Failed calls to Redis, Postgres and the usage collector are counted in `ktms_dependency_errors_total{dependency,operation}`; alert on it. Nothing below needs a restart: the Redis pool, the Postgres pool and the collector connection reconnect by themselves. After 3 consecutive Redis transport failures, Redis calls fail fast without network IO for 2s, then are tried again, so a hung Redis costs a request about one `REDIS_TIMEOUT`, not one per command.

| Dependency | What breaks | Behaviour | Why |
| --- | --- | --- | --- |
| Redis | RPM limits, daily token budgets | **Fail open**: calls proceed unmetered. | A metering outage should not become a service outage. |
| Redis | Spend limits | `SPEND_LIMITS_FAIL_MODE`: `open` (default) proceeds unmetered; `closed` refuses calls under a limit with `UNAVAILABLE throttled:spend_unavailable`. | Money: the operator chooses. |
| Redis | Settling reservations | The counter keeps the reserved estimate instead of the actual usage until its window resets: a key can hit its limit early, or late when a call used more than was reserved. | Reported, not repaired: the window reset clears it. |
| Redis | Response and semantic cache | **Fail open**: treated as a miss; storing is skipped. | A cache is an optimisation. |
| Redis | Local node pool | The pool's state is unknown, so pool models are **skipped** and the chain falls back (e.g. to cloud models); the vendor circuit breaker is not tripped. A node whose state cannot be read is left out, never assumed idle. Node registration returns `UNAVAILABLE` (the agent retries); the sweep never evicts a node on a read error. | Never overbook a node; keep serving from the rest of the chain. |
| Usage collector (light-tokenmeter) | Usage events | Queued in memory (10,000 events) and sent in batches; while the collector is down they are held and retried, then delivered exactly once (event IDs deduplicate). Events the collector refuses are dropped one by one, not with their batch. Drops are counted in `ktms_usage_events_dropped_total{reason}` (`queue_full`, `rejected`, `shutdown`, `closed`). Shutdown flushes for up to 5s. Requests never wait on it. | Accounting must not slow down or lose traffic. |
| Postgres | API key and node key checks | Keys that verified within `AUTH_STALE_IF_ERROR` (5m) keep working; any other key is refused with `UNAVAILABLE auth_unavailable`. Each check is bounded by `DB_TIMEOUT` (3s). Routing, limits and vendor keys serve from the last snapshot, so traffic of known keys continues. `/readyz` stays 200 with `"status":"degraded"` until `AUTH_STALE_IF_ERROR` has passed since Postgres last answered, then reports 503 (every cached key has expired); with `AUTH_STALE_IF_ERROR=0` it reports 503 at once and every call is refused. | Availability for known keys. Revocation still applies on the next request whenever Postgres answers; only one made just before an outage can lag, by at most the window. |
| Postgres | Routing config reload | The last good snapshot keeps serving; each poll or rebuild is bounded to 30s and retried every `ROUTING_RELOAD_SECONDS`. | |
| Postgres | Admin RPCs, batches | `UNAVAILABLE storage_unavailable` (cause logged). A batch whose idempotency check fails is refused rather than submitted, so it is never paid for twice. | |
| Postgres | Startup | Migrations and the first config load must succeed (the first load within 30s); otherwise the process exits and the orchestrator restarts it. | Nothing to serve without a first snapshot. |

## Development

```bash
make test       # go test -race ./...
make build      # binary in bin/
make proto      # regenerate gen/ after editing proto/ (needs buf)
make proto-py   # generate Python client stubs in gen/python/
make proto-ts   # generate TypeScript client stubs in gen/ts/
make clients    # regenerate the usage-sink stubs from usage-proto/
docker compose up --build   # local stack with Postgres + Redis
```

Layout: `cmd/server` (entrypoint), `internal/router` (rule matching, fallback, admission),
`internal/registry` (validated config snapshots), `internal/llm` (vendor-neutral chat model and
adapters), `internal/throttle` (Redis limits), `internal/grpcapi` (RPC handlers),
`internal/crypto` (keyring), `migrations/` (embedded SQL).

Pull requests are welcome; read [`CONTRIBUTING.md`](CONTRIBUTING.md) first.

### Releasing

1. Move the `[Unreleased]` entries in [CHANGELOG.md](CHANGELOG.md) under the new version and
   date, and pick the version by [API stability](#api-stability): a fix is a patch, a feature is a
   minor, a break of the public API is a major.
2. Push a `vX.Y.Z` tag (the `v` matters: it makes the tag a valid Go module version, so
   `go install github.com/ai-process/ktms/cmd/node-agent@vX.Y.Z` works):

```bash
git tag v1.0.0 && git push origin v1.0.0
```

The release workflow first runs `.github/workflows/compat.yml`: `buf breaking` against the
previous published release in the tag's history, and the upgrade test from it (and from the
oldest release of the same major). If it fails, nothing is published. Then GoReleaser publishes
the binaries and, after it, the Docker images: an image on Docker Hub marks a complete release,
which is how the next release finds its predecessor (re-run a failed image job before tagging
again). A release that breaks the proto on purpose (an Experimental change, or a new major) sets
the repository variable `ALLOW_BREAKING_TAG` to its tag, which skips the proto checks; the
upgrade test still runs. Only the newest stable tag moves `latest`, `slim` and `audio`, so a
backport does not.

- **GitHub Release** (GoReleaser, `.github/workflows/release.yml`): `ktms-node` and
  `ktms-server` archives for darwin and linux, amd64 and arm64, plus `checksums.txt`.
- **Docker Hub** (`.github/workflows/docker-publish.yml`): multi-arch `coyl/ktms` tagged as
  `:<tag>`, `:<tag>-slim`, `:latest`, `:slim` (default lightweight Alpine without ffmpeg),
  and `:<tag>-audio`, `:audio` (with ffmpeg), plus `coyl/ktms-node:<tag>` (node agent).
  The repository needs the secrets `DOCKER_USERNAME` and `DOCKER_PASSWORD` (an access token).
- **Optional, off until their secrets exist:** macOS signing and notarization
  (`MACOS_SIGN_P12`, `MACOS_SIGN_PASSWORD`, `MACOS_NOTARY_ISSUER_ID`, `MACOS_NOTARY_KEY_ID`,
  `MACOS_NOTARY_KEY`) and a Homebrew formula pushed to `ai-process/homebrew-tap`
  (`HOMEBREW_TAP_TOKEN`; the tap repository must exist). Without them the release is unsigned.
  Binaries fetched with `curl` or installed by `brew` carry no quarantine flag and run;
  only browser downloads trigger Gatekeeper.

Dry run: `go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean --skip=publish`.

## License

[Apache 2.0](LICENSE)
