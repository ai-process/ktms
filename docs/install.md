# Installation and footprint

**The problem.** A gateway sits in front of every LLM call, so it must be cheap to run, quick to
start, and easy to put next to your services: Docker, Kubernetes or a plain binary.

**The approach.** Keystone is one Go process. It needs Postgres, and Redis for limits and caches.
Replicas share nothing else, so you run as many as you like behind a load balancer that ends TLS.

<p align="center"><img src="img/install-deployment.svg" alt="Services, node daemons and ktms-admin reach Keystone replicas through a TLS load balancer; the replicas use Postgres, Redis, light-tokenmeter and the vendors; Prometheus scrapes them on the internal port 9091" width="860"></p>

**Contents:** [Glossary](#glossary) · [Resource footprint](#resource-footprint) ·
[Docker image](#docker-image) · [Helm chart](#helm-chart-kubernetes) ·
[From source](#building-from-source) · [Production checklist](#production-checklist)

## Glossary

| Term | Meaning |
| --- | --- |
| **Keyring** | `KTMS_ENCRYPTION_KEYS`, the versioned AES keys that encrypt vendor keys in Postgres. Lose it and the vendor keys are lost. |
| **Bootstrap admin key** | `BOOTSTRAP_ADMIN_KEY`, an implicit admin key for the first setup. Unset it once real keys exist. |
| **Replica** | One Keystone process. Replicas are stateless; Postgres and Redis hold what they share. |
| **Slim, audio image** | `coyl/ktms:latest` without ffmpeg, and `coyl/ktms:audio` with it for Gemini speech. |

## Resource footprint

KTMS adds well under a millisecond per request and needs tens of megabytes of memory.

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

The same stand measured Bifrost, Portkey, Kong AI Gateway, Apache APISIX, Higress, Envoy AI Gateway,
agentgateway, New API and Plano:

- KTMS is in the class of the compiled and Nginx/Envoy-based gateways: all add under 1.3 ms at p50
  and sustain 4,000 RPS.
- Among them, only agentgateway matches KTMS's memory; agentgateway, APISIX and Kong add less
  latency (0.25–0.35 ms) and use less CPU per request.
- The Node.js and Python gateways (Portkey, LiteLLM) saturate at 500 and 250 RPS.

*Measured in October 2026 on Google Cloud: each gateway alone on a dedicated `c3-standard-4` (4 vCPU,
16 GB, x86_64), k6 on a second VM, and a mock OpenAI upstream (0 ms for latency, 10 ms for
throughput). LiteLLM `v1.106.0` ran with master key only and no database. Memory is the gateway
container's cgroup RSS; Postgres and Redis are not counted. Two independent runs agreed within ~10%,
except LiteLLM p99. The stand, every gateway config and the run scripts are in [`bench/`](../bench/).*

## Installation

### Docker image

Images for `linux/amd64` and `linux/arm64` on Docker Hub:

| Tag | Size (uncompressed / pull) | For |
| --- | --- | --- |
| `coyl/ktms:latest`, `:slim`, `:<version>`, `:<version>-slim` | ~67 MB / ~21 MB | Everything except Gemini speech: text, chat, embeddings, OpenAI TTS. |
| `coyl/ktms:audio`, `:<version>-audio` | ~195 MB / ~69 MB | Adds `ffmpeg` for Gemini speech synthesis (PCM to OGG/Opus). |

- `X.Y` (and `X.Y-slim`, `X.Y-audio`) follows the newest release of that line; `latest`, `slim`
  and `audio` follow the newest release of all. The [changelog](../CHANGELOG.md) lists each release.
- Gemini TTS on the slim image fails with gRPC `UNIMPLEMENTED` / HTTP 501 `ffmpeg_missing`. OpenAI
  TTS produces Opus itself and works on both.
- The image runs as `appuser` (UID 10001), exposes `8080` (HTTP) and `9090` (gRPC), and takes all
  settings from environment variables.
- From 1.0.0, images are signed with cosign and carry an SBOM and provenance; `checksums.txt` is
  signed too. See [Verifying a Release](../SECURITY.md#verifying-a-release).

**Run it.** Needs Postgres (16 in the compose file) and, for limits, Redis (6.2 or newer for the
node pool's usage log):

```bash
# generate the keyring once and keep it safe, apart from database backups
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

A Compose service for your own stack:

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

On Kubernetes without the chart:

| Probe | Endpoint |
| --- | --- |
| Liveness | `GET :8080/healthz` |
| Readiness | `GET :8080/readyz` (checks the database) |

Replicas pick up configuration changes within `ROUTING_RELOAD_SECONDS`.

### Helm chart (Kubernetes)

Published with every release at `oci://ghcr.io/ai-process/charts/ktms` (source in
[`deploy/helm/ktms`](../deploy/helm/ktms)). The chart version is the release version and runs the
image of that version.

```bash
# 1. a secret with the database URL and the keys
kubectl create secret generic ktms-secrets \
  --from-literal=DATABASE_URL='postgres://ktms:pass@db:5432/ktms?sslmode=require' \
  --from-literal=KTMS_ENCRYPTION_KEYS="1:$(head -c 32 /dev/urandom | base64)" \
  --from-literal=BOOTSTRAP_ADMIN_KEY="$(head -c 24 /dev/urandom | base64 | tr -d '/+=')"

# 2. the release
helm install ktms oci://ghcr.io/ai-process/charts/ktms --version 1.8.0 \
  --set secrets.existingSecret=ktms-secrets \
  --set ingress.enabled=true \
  --set "ingress.hosts[0].host=proxy.example.com" \
  --set "ingress.hosts[0].paths[0].path=/" \
  --set "ingress.hosts[0].paths[0].pathType=Prefix"
```

The chart has a non-root security context (`appuser:10001`), a HorizontalPodAutoscaler, a
Prometheus Operator `ServiceMonitor` and a separate gRPC Ingress. All values:
[`deploy/helm/ktms/README.md`](../deploy/helm/ktms/README.md).

### Building from source

Requires Go 1.26+ (and `ffmpeg` on `PATH` for Gemini speech).

```bash
make build                       # → bin/ktms-server and bin/ktms-node
export DATABASE_URL=postgres://ktms:ktms@localhost:5432/ktms?sslmode=disable
export KTMS_ENCRYPTION_KEYS="1:$(head -c 32 /dev/urandom | base64)"
./bin/ktms-server
```

Or build the image: `docker build -t ktms .`

### Production checklist

**Network**

| Do | Why |
| --- | --- |
| End TLS in front of Keystone (Envoy, nginx `grpc_pass`, a mesh, a cloud load balancer), or keep it on a private network. Node daemons behind that TLS run with `--tls`. | The server speaks plaintext gRPC. |
| Expose gRPC `:9090` and HTTP `:8080`. Keep metrics `:9091` on the cluster network. Block `/healthz` and `/readyz` at a public ingress. | `/readyz` reports database state, and the probes share `:8080` with the API. |
| Network policies. Ingress: clients, node daemons, the console, Prometheus, kubelet probes. Egress: vendors, Postgres, Redis, the collector, the nodes, and `OTEL_EXPORTER_OTLP_ENDPOINT`, `SENTRY_DSN`, `LATENT_ENCODER_URL` if set. | Least access. |
| `sslmode=require` for Postgres, `USAGE_COLLECTOR_TLS` for the collector. Keep Redis private with `REDIS_PASSWORD`. | The Redis client has no TLS. |
| Leave `NODE_ALLOW_LOCALHOST_ENDPOINTS=false` outside development. | Otherwise a node may register a loopback or link-local address. |

**Keys and secrets**

| Do | Why |
| --- | --- |
| Back up the keyring apart from the database. See [Backups](operations.md#backups-and-the-encryption-keyring). | Without `KTMS_ENCRYPTION_KEYS` the stored vendor keys are unreadable. |
| Mint real keys, then unset `BOOTSTRAP_ADMIN_KEY`. | It is accepted as an admin key. |
| One client key, and its own vendor credentials, per client. | Spend, models and revocation are per key. |
| Least privilege: `generate` for clients, `admin:read` for dashboards and assistants, `admin` only for the console and operators. | A leaked key does less. |
| Keep `KTMS_ENCRYPTION_KEYS`, `DATABASE_URL` and `USAGE_COLLECTOR_TOKEN` (light-tokenmeter's `write` scope only) in a secret manager. Rotate the keyring as in [Rotating the encryption key](configuration.md#rotating-the-encryption-key). | |

**Money and reliability**

| Do | Why |
| --- | --- |
| Run Redis if you want limits. | Without `REDIS_ADDR`, RPM, token budgets and spend limits are **not enforced**; on Redis errors they fail open. `SPEND_LIMITS_FAIL_MODE=closed` refuses spend-limited calls instead. |
| A [spend limit](configuration.md#5-spend-limits-optional) on every client key. | Caps what a leaked key or a runaway client can spend. |
| Fit replicas × `DB_MAX_CONNS` (25) under Postgres's `max_connections`. | Each replica holds up to that many connections. |
| Back up Postgres. See [Backups](operations.md#backups-and-the-encryption-keyring). | All configuration lives there. |
| Wire up the [suggested alerts](operations.md#metrics-and-alerts). | Redis and the collector fail open: without alerts a dead limiter or lost accounting goes unnoticed. |
