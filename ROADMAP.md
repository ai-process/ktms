# Roadmap: Keystone. The Model Switchboard. (KTMS)

This is what we plan to build next in Keystone, the KTMS console and light-tokenmeter. It shows
direction, not commitments: items can move between horizons, and dates and version numbers are
assigned only when a feature ships (see the [changelog](CHANGELOG.md)). Within each horizon, items
higher up come first.

- **Now** — in active work.
- **Next** — planned once the current work lands.
- **Later** — wanted, not scheduled yet.
- **Not planned** — out of scope for the project.

Every 1.x release is additive: new RPCs, fields, endpoints and settings. Anything that would break
`ktms.v1`, the HTTP `/v1` API, environment variables, Helm values or the database schema waits for
[2.0](#planned-for-20).

## Now

### Coding agents and API

- OpenAI Responses API at `/v1/responses` for Codex CLI and the Agents SDK.

### Routing and providers

- A pool of keys per vendor, spread across requests and rotated around vendor rate limits.
- An explicit `openai_compatible` vendor for any OpenAI-compatible endpoint.

### Security and operations

- An audit log of configuration changes: who changed what and when, through the Admin API or the
  console.
- Secrets read from files (`*_FILE` variants for `KTMS_ENCRYPTION_KEYS`, `DATABASE_URL` and service
  tokens), with Helm recipes for Vault Agent, External Secrets Operator and the Secrets Store CSI
  driver.
- A ready Grafana dashboard and a `compose.observability.yaml` to run it locally.
- Node usage in the console: load and spend per GPU node.
- A published 24-hour soak test and CPU and memory profiles of Keystone under load.
- light-tokenmeter: retention settings for Postgres and ClickHouse, and published memory and
  throughput measurements.

## Next

### Agent tools: MCP, second stage

- OAuth 2.1 for MCP clients, and OAuth to upstream servers with token refresh.
- Guardrails on tool responses, including prompt injection arriving through tool output.
- Pinned tool descriptions: an alert when a server changes a tool it already published.
- OpenAPI to MCP: expose a REST service as a set of tools.
- MCP tools executed by the gateway inside `/v1/responses`.

### Agent-to-agent: A2A proxy

- A registry of A2A agents, with Agent Cards rewritten to point at Keystone.
- JSON-RPC, SSE and REST transports, under KTMS keys, limits and usage accounting.

### API and providers

- Speech to text at `/v1/audio/transcriptions`.
- OpenAI-compatible `/v1/files` and `/v1/batches` on top of the existing Batch API.
- `/v1/rerank` and `/v1/moderations`.
- Vertex AI: Gemini and Claude through Google Cloud with a service account.
- Presets for OpenAI-compatible clouds: xAI, OpenRouter, Together, Fireworks, Cerebras.

### Spend, security and data

- Budget alerts at thresholds such as 80% and 100%: webhook, Slack and e-mail.
- Expiry dates and rotation for client keys.
- Guardrails per key and per routing rule, not only globally.
- External guardrails: Presidio, Amazon Bedrock Guardrails, Azure AI Content Safety, and the
  judge model as a guardrail.
- The encryption keyring unwrapped at startup from HashiCorp Vault Transit, AWS KMS or Google Cloud
  KMS.
- Opt-in logging of request and response bodies, with PII redaction and a retention period.
- Usage export to Langfuse, Datadog and S3 or GCS.
- Passkey sign-in for the console.

### Routing

- Hedged requests for streaming: a second attempt when the first is slow to start.
- A corrective retry when a response fails its JSON schema.
- Cloud routing strategies by latency, load and price.

### Documentation

- Cookbooks for common setups, and a shorter README that points to the
  [documentation site](https://ai-process.dev/docs).

## Later

- Vendor keys kept in Vault and referenced from Keystone (`vault://…`), cached with a TTL and
  rotated in Vault.
- Local stdio MCP servers published through `ktms-node`, without a public IP.
- Tools as code ("code mode") to save agent context.
- Native Gemini API (`generateContent`) and passthrough routes to vendor APIs with key injection and
  accounting.
- Cohere: chat, embeddings and rerank.
- `n > 1` and `logprobs`.
- Realtime API over WebSocket, video and image edits.
- Routing by client tags and headers; priority queues for cloud vendors.
- Prompt management: versioned templates called by id.
- Plugins: webhooks before and after a call, or WASM.
- A feedback API that ties a rating to a request.

## Not planned

- **A hosted service.** Keystone is self-hosted.
- **A paid or enterprise edition.** Everything ships under Apache 2.0.
- **A native adapter for every provider.** The long tail is covered by `openai_compatible`.

## Planned for 2.0

Breaking changes are collected for 2.0 and not shipped in 1.x. So far:

- removal of the deprecated `LLM_PROXY_*` variables, `X-LLM-Proxy-*` headers, the console's
  `GOOGLE_*` variables and other aliases marked "until 2.0".

## Feedback

Tell us what matters to you and what is missing: open an
[issue](https://github.com/ai-process/ktms/issues). Requests for the console and
light-tokenmeter are welcome there too.
