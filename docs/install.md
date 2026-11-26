# Installation and footprint

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
except LiteLLM p99. The stand, every gateway config and the run scripts are in [`bench/`](../bench/).*

## Installation

### Docker image

Release images are published to Docker Hub for `linux/amd64` and `linux/arm64` in two variants:

- **`coyl/ktms:latest`** (also tagged **`:slim`** or pinned `:<version>` / `:<version>-slim`): default lightweight image (~67 MB uncompressed, ~21 MB download). A clean Alpine container without `ffmpeg`, optimized for text generation, chat, reasoning, embeddings, and OpenAI TTS.
- **`coyl/ktms:audio`** (or pinned `:<version>-audio`): extended image (~195 MB uncompressed, ~69 MB download) with `ffmpeg` installed for Gemini speech synthesis (transcoding raw PCM into OGG/Opus voice messages).

```bash
docker pull coyl/ktms:latest      # default lightweight image (~67 MB)
docker pull coyl/ktms:audio       # with ffmpeg for Gemini speech synthesis (~195 MB)
```

`X.Y` (and `X.Y-slim`, `X.Y-audio`) follows the newest release of its line; `latest`, `slim` and
`audio` follow the newest release of all. The [changelog](../CHANGELOG.md) lists what each release
changed.

If Gemini TTS is requested without ffmpeg available (e.g. on the default slim image), the proxy returns gRPC `Codes.Unimplemented` / HTTP 501 Not Implemented: `ffmpeg_missing: ffmpeg is required for Gemini speech synthesis: install ffmpeg or use the 'coyl/ktms:audio' container image`. OpenAI TTS produces native Opus directly and works on both variants without ffmpeg.

The image runs as a non-root user (`appuser`, UID 10001), exposes `8080` (HTTP) and `9090` (gRPC), and is configured entirely through environment variables. From 1.0.0, images are signed with cosign and carry an SBOM and provenance, and the release's `checksums.txt` is signed: see [Verifying a Release](../SECURITY.md#verifying-a-release).

You need a Postgres database (required; 16 is what the compose file uses) and a Redis (optional, see below;
6.2 or newer for the local node pool's usage log):

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

An official Helm v3 chart is published with every release at `oci://ghcr.io/ai-process/charts/ktms`
(source in [`deploy/helm/ktms`](../deploy/helm/ktms)). The chart version is the release version, and
it runs the image of the same version:

```bash
# 1. Create a secret with your database URL and encryption keys
kubectl create secret generic ktms-secrets \
  --from-literal=DATABASE_URL='postgres://ktms:pass@db:5432/ktms?sslmode=require' \
  --from-literal=KTMS_ENCRYPTION_KEYS="1:$(head -c 32 /dev/urandom | base64)" \
  --from-literal=BOOTSTRAP_ADMIN_KEY="$(head -c 24 /dev/urandom | base64 | tr -d '/+=')"

# 2. Install the release
helm install ktms oci://ghcr.io/ai-process/charts/ktms --version 1.7.0 \
  --set secrets.existingSecret=ktms-secrets \
  --set ingress.enabled=true \
  --set "ingress.hosts[0].host=proxy.example.com" \
  --set "ingress.hosts[0].paths[0].path=/" \
  --set "ingress.hosts[0].paths[0].pathType=Prefix"
```

The chart supports non-root security context (`appuser:10001`), HorizontalPodAutoscaler, Prometheus Operator `ServiceMonitor`, and separate gRPC Ingress configuration. See [`deploy/helm/ktms/README.md`](../deploy/helm/ktms/README.md) for full configuration parameters.

### Building from source

Requires Go 1.26+ (and `ffmpeg` on `PATH` if you use speech).

```bash
make build                       # → bin/ktms-server and bin/ktms-node
export DATABASE_URL=postgres://ktms:ktms@localhost:5432/ktms?sslmode=disable
export KTMS_ENCRYPTION_KEYS="1:$(head -c 32 /dev/urandom | base64)"
./bin/ktms-server
```

Or build the image yourself: `docker build -t ktms .`

### Production checklist

- **TLS.** The server speaks plaintext gRPC. Terminate TLS in front of it (Envoy, nginx
  with `grpc_pass`, a service mesh, a cloud load balancer) or keep it on a private network.
  Node daemons that reach the proxy through that TLS endpoint run with `--tls`.
- **Back up the encryption keyring**, apart from the database: without `KTMS_ENCRYPTION_KEYS` the
  stored vendor keys are unreadable. See [Backups](operations.md#backups-and-the-encryption-keyring).
- **Mint real keys, then unset `BOOTSTRAP_ADMIN_KEY`.** It is accepted as an implicit admin key.
- **Run Redis if you want limits.** With `REDIS_ADDR` empty, RPM, token budgets and spend
  limits are **not enforced** (the proxy fails open by design, and does the same if Redis
  errors; set `SPEND_LIMITS_FAIL_MODE=closed` to refuse spend-limited calls instead).
- **Give each client its own API key** (and its own vendor credentials), so spend, models
  and revocation are per client.
- **Mind your Postgres connection count.** Each replica holds up to `DB_MAX_CONNS`
  (default 25) connections.
- **Back up Postgres.** All configuration lives there; see [Backups](operations.md#backups-and-the-encryption-keyring).
- **Keep internal ports internal.** Expose gRPC (`:9090`, clients and node daemons) and HTTP
  (`:8080`). Metrics (`:9091`) belong on the cluster network. `/healthz` and `/readyz` share `:8080`
  with the API, and `/readyz` reports database state, so block both paths at a public ingress.
- **Network policies.** Ingress: clients, node daemons, the console, Prometheus (`:9091`) and the
  kubelet probes. Egress: the vendors, Postgres, Redis, the collector, the nodes, and what you
  configured (`OTEL_EXPORTER_OTLP_ENDPOINT`, `SENTRY_DSN`, `LATENT_ENCODER_URL`).
- **TLS to dependencies.** Use `sslmode=require` for Postgres and `USAGE_COLLECTOR_TLS` for the
  collector. The Redis client has no TLS: keep Redis on a private network with `REDIS_PASSWORD`.
- **Least-privilege keys.** `generate` for clients; `admin:read` for dashboards, assistants and
  anything that only looks; `admin` only for the console and operators.
- **Spend limits on every client key** ([Spend limits](configuration.md#5-spend-limits-optional)).
- **Secrets.** Keep `KTMS_ENCRYPTION_KEYS`, `DATABASE_URL` and `USAGE_COLLECTOR_TOKEN` in a secret
  manager; the collector token needs only light-tokenmeter's `write` scope. Rotate the keyring
  with [Rotating the encryption key](configuration.md#rotating-the-encryption-key).
- **Nodes.** Leave `NODE_ALLOW_LOCALHOST_ENDPOINTS=false` outside development.
- **Alerts.** Wire up the [suggested alerts](operations.md#metrics-and-alerts); Redis and the collector fail
  open, so without them a dead limiter or lost accounting goes unnoticed.
