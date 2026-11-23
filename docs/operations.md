# Operations

- **Health:** `GET :8080/healthz` (process up, with its `version`) and `GET :8080/readyz` (database reachable).
- **Logs:** structured JSON via zerolog. Vendor keys, client keys and the keyring are never logged.
- **Config changes** are picked up by all replicas within `ROUTING_RELOAD_SECONDS`; the replica that
  handled the admin call applies them immediately.
- **Upgrades:** migrations run at startup under a database advisory lock, so rolling deploys are
  safe. Roll forward by deploying a newer image tag; see [Upgrading](stability.md#upgrading).
- **Graceful shutdown:** on SIGTERM in-flight calls get `SHUTDOWN_TIMEOUT` to finish.

## Backups and the encryption keyring

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

## When a dependency fails

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

## Metrics and alerts

Prometheus metrics are served on `METRICS_ADDR` (`:9091`) at `/metrics`; the Helm chart can add a
`ServiceMonitor`. KTMS's own metrics start with `ktms_`; the standard Go runtime and process
metrics are served next to them.

| Metric | Labels | What it tells you |
| --- | --- | --- |
| `ktms_requests_total` | `vendor`, `model`, `effort`, `status` (`success`, `error`, `canceled`) | Upstream calls and their outcome; `canceled` is a caller that went away. |
| `ktms_request_duration_seconds` | `vendor`, `model`, `effort` | Upstream call duration, histogram; for a stream, the whole stream. |
| `ktms_time_to_first_token_seconds` | `vendor`, `model` | Time to the first streamed chunk, histogram. |
| `ktms_tokens_total` | `vendor`, `model`, `type` (`input`, `output`) | Tokens consumed. |
| `ktms_cache_tokens_total` | `vendor`, `model`, `type` (`read`, `write`) | The part of input read from or written to the prompt cache. |
| `ktms_active_requests` | `vendor`, `model` | Upstream calls in flight. |
| `ktms_throttled_total` | `reason` (`rpm`, `key_rpm`, `key_tpm`, `mcp_tool_rpm`, `budget_key`, `budget_user`, `spend_key_daily`, `spend_key_monthly`, `spend_user_daily`, `spend_user_monthly`, `spend_team_monthly`, `spend_unavailable`) | Calls refused by a limit; `spend_unavailable` is Redis down with `SPEND_LIMITS_FAIL_MODE=closed`. |
| `ktms_fallbacks_total` | `from_model`, `to_model`, `reason` | Moves down a fallback chain. |
| `ktms_circuit_breaker_state` | `vendor`, `model` | `0` closed, `1` open, `2` half-open. |
| `ktms_circuit_breaker_transitions_total`, `ktms_circuit_breaker_rejections_total` | `vendor`, `model` (+ `from`, `to`) | Breaker flaps and the calls it refused. |
| `ktms_vendor_cooldowns_total` | `vendor`, `model` | Pauses started from a vendor `Retry-After`. |
| `ktms_dependency_errors_total` | `dependency` (`redis`, `postgres`, `usage_collector`, `keyring`, `mcp`), `operation` | Failed calls to a dependency; `keyring` is a stored vendor key that no keyring version decrypts. |
| `ktms_usage_events_dropped_total` | `reason` (`queue_full`, `rejected`, `shutdown`, `closed`) | Usage events that never reached light-tokenmeter. |
| `ktms_batch_jobs_total`, `ktms_batch_items_total` | `vendor`, `state` | Batch progress. |
| `ktms_batch_poll_duration_seconds` | `vendor` | Vendor batch polling, histogram. |
| `ktms_shadow_requests_total`, `ktms_shadow_judge_evaluations_total`, `ktms_shadow_judge_scores` | | Shadow traffic and its judged quality. |
| `ktms_legacy_headers_requests_total` | `header` | Clients still sending `X-LLM-Proxy-*` headers. |
| `ktms_mcp_tool_calls_total` | `server`, `tool`, `status` (`success`, `tool_error`, `denied`, `throttled`, `timeout`, `too_large`, `upstream_error`, `cancelled`) | Tool calls through the [MCP gateway](mcp.md#mcp-gateway). |
| `ktms_mcp_tool_call_duration_seconds` | `server` | Tool call duration, histogram. |
| `ktms_mcp_response_bytes` | `server` | Tool result size, histogram. |
| `ktms_agent_messages_total` | `outcome` (`sent`, `replied`, `held`, `denied_<reason>`) | Messages between [agents](mcp.md#agents); `held` is a task a guardrail holds for a person. |
| `ktms_agent_tasks` | `status` | [Agent](mcp.md#agents) tasks in each status, read from Postgres every minute; every replica reports the same total, so aggregate with `max`. |
| `ktms_agent_reply_seconds` | `role` (`agent` — the recipient, `user` — the sender), `status` | How long a task waited for a reply since its last change. |
| `ktms_agent_journal_events_total` | `result` (`written`, `error`) | Writes to the agents' journal. |

The `throttled` and `usage_events_dropped` reasons are exported at 0 from startup, so `increase()`
catches the first event after a restart. Suggested alerts (tune thresholds to your traffic; `job`
is your scrape job, with the chart's `ServiceMonitor` the Service name, e.g. `prod-ktms`):

```yaml
groups:
  - name: ktms
    rules:
      - alert: KTMSUpstreamErrorRate
        expr: sum by (vendor, model) (rate(ktms_requests_total{status="error"}[5m]))
          / sum by (vendor, model) (rate(ktms_requests_total{status!="canceled"}[5m])) > 0.05
        for: 10m
      - alert: KTMSTimeToFirstTokenP99
        expr: histogram_quantile(0.99, sum by (le, vendor, model) (rate(ktms_time_to_first_token_seconds_bucket[5m]))) > 10
        for: 15m
      - alert: KTMSCircuitOpen
        expr: max by (vendor, model) (ktms_circuit_breaker_state) == 1
        for: 5m
      - alert: KTMSDependencyErrors
        expr: sum by (dependency) (rate(ktms_dependency_errors_total[5m])) > 0
        for: 5m
      - alert: KTMSUsageEventsDropped
        expr: sum by (reason) (increase(ktms_usage_events_dropped_total[15m])) > 0
      - alert: KTMSSpendLimitReached
        expr: sum by (reason) (increase(ktms_throttled_total{reason=~"spend_(key|user|team)_.+"}[15m])) > 0
      - alert: KTMSDown
        expr: up{job="ktms"} == 0
        for: 2m
```

`KTMSDependencyErrors` and `KTMSUsageEventsDropped` matter most: Redis and the collector fail
open, so the calls keep succeeding and only these metrics show that limits or accounting stopped.
A long stream is not slow, so latency alerts use the time to the first token. Also probe
`/readyz`: it turns `degraded`, then 503, during a Postgres outage.

## Sizing

From the [benchmark](install.md#resource-footprint): about 0.65 s of CPU per 1,000 proxied requests, under
50 MiB of memory at 4,000 RPS, no drift at a steady load. Upstream time does not cost CPU, so size
by request rate, not by how long vendors take to answer.

- **CPU:** plan 1 vCPU per 1,000 RPS, which leaves headroom for TLS, logging and streaming.
  Guardrails, the semantic cache and the latent encoder add CPU per call; measure them with
  `go test -bench` (see [Semantic cache and latent guardrails](concepts.md#semantic-cache-and-latent-guardrails)).
- **Memory:** memory follows the bodies in flight, not RPS. The chart's default (256 MiB request,
  512 MiB limit) covers thousands of concurrent short calls; raise it for large prompts, images
  or audio (`GRPC_MAX_RECV_MSG_SIZE` is 16 MiB per message).
- **Replicas:** run at least two behind a load balancer; they share nothing but Postgres and
  Redis. Each replica keeps its own circuit breakers and vendor pauses.
- **Postgres:** replicas × `DB_MAX_CONNS` (25 by default) must fit under `max_connections` with
  room for migrations and the other services. Routing serves from a snapshot reloaded every
  `ROUTING_RELOAD_SECONDS`, so the hot path costs one indexed key lookup per call; size
  Postgres for that query rate.
- **Redis:** a few commands per call when limits, caches or the node pool are on, each bounded by
  `REDIS_TIMEOUT`. Keep it in the same zone as the gateway; one small instance serves every
  replica.
