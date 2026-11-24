# Error contract

## RPC status codes

| gRPC status | Reason | What to do |
| --- | --- | --- |
| `NOT_FOUND` | – | Unknown batch ID. |
| `INVALID_ARGUMENT` | `guardrail_violation` | Blocked due to detected prompt injection, PII, or a latent-space threat match. Clean input before retrying. |
| `INVALID_ARGUMENT` | `bad_request: <message>` | Empty batch, duplicate `custom_id`, too many items (> `BATCH_MAX_ITEMS`), or invalid parameters. |
| `UNAVAILABLE` | `vendors_unavailable` | The whole chain failed: retry with backoff. When vendors paused every model in the chain with `Retry-After`, details carry `RetryInfo` (HTTP: `Retry-After` header): wait that long first. |
| `UNAVAILABLE` | `auth_unavailable`, `storage_unavailable` | Postgres is unreachable for a key check (past `AUTH_STALE_IF_ERROR`) or a batch or admin write. Retry with backoff. |
| `RESOURCE_EXHAUSTED` | `throttled:rpm`, `throttled:budget_key`, `throttled:budget_user` | Back off; don't hot-retry. Daily budget exhausted before batch submission. |
| `RESOURCE_EXHAUSTED` | `throttled:key_rpm`, `throttled:key_tpm` | The key's own `rpm_limit` or `tpm_limit`. Details carry `RetryInfo`: wait that long. |
| `FAILED_PRECONDITION` | `request_too_large` | The call needs more tokens than a minute of the key's `tpm_limit`; it never fits. Shrink it or raise the limit. |
| `RESOURCE_EXHAUSTED` | `throttled:spend_key_daily`, `throttled:spend_key_monthly`, `throttled:spend_user_daily`, `throttled:spend_user_monthly`, `throttled:spend_team_monthly` | Spend limit reached. Details carry `ErrorInfo{reason: "insufficient_quota"}` and `RetryInfo` until the period resets. |
| `UNAVAILABLE` | `throttled:spend_unavailable` | Spend counters unreachable with `SPEND_LIMITS_FAIL_MODE=closed`. Retry with backoff. |
| `PERMISSION_DENIED` | `model "<id>" not allowed for <key\|team>`, `no models in chain allowed for <key\|team>`, `model access denied by key or team policy` (batch) | The key's or its team's `allowed_models` excludes that model. |
| `FAILED_PRECONDITION` | `output_truncated` | Raise `max_output_tokens`; don't retry as-is. |
| `FAILED_PRECONDITION` | `no_capable_model` | No model in the matched chain can serve this request, modality, or batch capability. |
| `FAILED_PRECONDITION` | `unsupported_language_pair` | `Translate`: models of the chain can translate, but none declares this pair. Add a model for it or a `*` fallback. |
| `FAILED_PRECONDITION` | `no_vendor_key` | The client key holds no credential for the chain's vendors. `local` models never cause this. |
| `FAILED_PRECONDITION` | `not_configured` | No models/rules configured yet. |
| `FAILED_PRECONDITION` | `batch not configured` | Batch processing is not enabled/configured on the proxy (no batch store). |
| `UNIMPLEMENTED` | `ffmpeg_missing` | Gemini speech synthesis invoked without ffmpeg on PATH or running slim container. Install ffmpeg or switch to `:audio` image. |
| `INTERNAL` | `vendor_error` | Terminal vendor failure. |
| `UNAUTHENTICATED` / `PERMISSION_DENIED` | – | Missing/invalid key, or key lacks the scope. |

When to retry: `UNAVAILABLE` with backoff, and `RESOURCE_EXHAUSTED` after the wait it carries
(`RetryInfo`, HTTP `Retry-After`) or with backoff when it carries none. Every other code fails the
same way until the request or the configuration changes.

## Batch item error reasons

Items returned in `ListBatchResults` that failed have an `error` containing a gRPC code, a message, and one of these reasons:

| Reason | When | What to do |
| --- | --- | --- |
| `output_truncated` | Item reached `max_output_tokens` or candidate finish limit. | Re-submit item with higher `max_output_tokens`. |
| `vendor_error` | Vendor error occurred during processing or structured schema validation failed. | Retry synchronously or resubmit in another batch. |
| `batch_expired` | Vendor batch expired (after 48 h) before the item finished. | Re-submit the item. |
| `batch_cancelled` | Batch was cancelled before the item completed. | Re-submit if still needed. |
