# Concepts

**Effort** is the only routing field the proxy understands; everything else about a request's
purpose travels in free-form attributes, so the proxy knows nothing about your product.

**Throttling & Wait Buffer:**
- **GCRA Rate Limiting:** Enforces requests-per-minute (`rpm`) via the Generic Cell Rate Algorithm in Redis, with configurable burst capacity (`rpm_burst`, defaulting to `rpm - 1` to preserve classic fixed-window behavior, `< 0` to disable burst). Key limits and user limits are evaluated atomically.
- **User & Model Overrides:** Configure fine-grained limits per user and model with `SetUserRateLimit` (supports `*` wildcards for user-wide or model-wide defaults).
- **Spend Limits:** Hard USD caps per API key and per end user (UTC day or month), reserved atomically before each call and settled to the actual cost; see [Spend limits](configuration.md#5-spend-limits-optional).
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
gRPC service implementing [`usage-proto/usage/v1/usage.proto`](../usage-proto/usage/v1/usage.proto).
Events are attributed to the project named by the `usage_project` attribute, falling back to the
API key's name. Any service implementing `RecordUsageEvent(s)` works as a sink. When
light-tokenmeter has authentication on, give KTMS a token with the `write` scope in
`USAGE_COLLECTOR_TOKEN`; a refused token is logged as an error and events are held and retried.

## Restricted data

`data_classification: restricted` keeps a call on your self-hosted nodes: no cloud vendor sees it,
and when the nodes fail there is no fallback to the cloud. Set it on a request, or on a key's policy
or a team, where it holds for every call of the key whatever the request says:

```bash
ktms keys mint scribe --team clinic/notes --data-classification restricted
ktms keys policy set billing --data-classification restricted   # unrestricted lifts it
```

```yaml
organizations:
  - name: clinic
    teams:
      - name: notes
        data_classification: restricted   # every key of the team
```

`SetAPIKeyPolicy` and `UpdateTeam` replace the rest of the policy or team, but keep the current
`data_classification` when the field is empty: a client that predates it, such as an older
console, cannot lift a restriction by leaving it out. `unrestricted` lifts it, and the
configuration file does that when the field is removed. `ktms keys list` shows a key's
restriction, or its team's as `restricted (team)`.

For a single call, set `data_classification: restricted` in the request (`GenerateTextRequest`,
`data_classification` on `/v1/chat/completions`), or send the `X-KTMS-Data-Classification:
restricted` header over HTTP or `x-ktms-data-classification` gRPC metadata, which also covers
embeddings, judgments, speech and images. A request can restrict a call, not lift its key's or
team's restriction; a value other than `restricted` or empty is `INVALID_ARGUMENT`. A restricted call:

- routes as `placement: nodes_only`: the chain keeps the node models (`VendorLocal`,
  `VendorOllama`) and is `FAILED_PRECONDITION no_capable_model` without one; when every node fails
  it is `UNAVAILABLE vendors_unavailable`. `nodes_first` and `cloud_first` become `nodes_only`,
  `cloud_only` is `INVALID_ARGUMENT`, and a `model_override` of a cloud model is
  `FAILED_PRECONDITION`.
- asks the auto-effort judge and the shadow evaluation judge only on nodes, and falls back to the
  default effort, or skips the evaluation, when no node can judge.
- skips the response cache, so its answer cannot be served to another key.
- for embeddings, judgments, speech and images is `FAILED_PRECONDITION` without a node model;
  batches go to vendors' batch APIs and need a node model too, or are refused.
- carries `data_classification: restricted` in its usage event's meta; callers cannot set that
  attribute themselves.

Nodes are models of vendor `local` (registered `ktms-node`s) and `ollama`, an Ollama endpoint you
configure: point `ollama` models only at servers you run. Restricted data also does not cover what
is sent outside the model chain: `LATENT_ENCODER_URL` (latent guardrails and the semantic cache)
and MCP servers. Point the encoder at your own infrastructure.

## Semantic cache and latent guardrails

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
