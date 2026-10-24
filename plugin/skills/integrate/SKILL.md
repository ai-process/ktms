---
name: integrate
description: Move a service's LLM calls onto Keystone (KTMS), the self-hosted LLM gateway - one client module, an effort per call site, attribution, the error policy and tests. Use when a codebase calls OpenAI, Anthropic, Gemini or other vendor SDKs and should go through Keystone, or when asked to connect a service to KTMS.
---

# Connect a service to Keystone

Keystone speaks the OpenAI wire format on HTTP `/v1` and its own gRPC API on `:9090`. A service
needs a client key (`ktms_…`, scope `generate`) and the gateway's address; it never names vendor
models, it asks for an effort and Keystone's rules pick the chain.

## Before you change code

1. List every LLM call site (SDK or raw HTTP) with file:line. Change nothing yet.
2. Ask what the gateway is (base URL, gRPC address) unless the project already says.
3. If the `ktms` MCP server is connected, call `health` and `list_models` to see what is
   served; otherwise `ktms health` and `ktms models list`.
4. A key for the service: the user mints it (`ktms keys mint NAME --team org/team`, or the
   MCP tool `mint_key` with write tools on). It is shown once: it goes into the secret store,
   never into the repository or this chat.

## The change

- One small client module owns the base URL, key, timeouts, retries and attribution. Read
  `KTMS_BASE_URL` (or `KTMS_GRPC_ADDR`) and `KTMS_API_KEY` from the environment; add both to the
  example env file and the deployment config.
- Model values: `auto` (a judge picks the effort), `auto:low|medium|high`, or
  `auto:<placement>:<effort>` with placement `nodes-first`, `nodes-only`, `cloud-first`,
  `cloud-only`. In gRPC set `effort`. An exact model id bypasses the rules; embeddings always need
  one.
- Effort per call site: low for classification, extraction and short answers, medium for ordinary
  generation, high for reasoning and long documents. Justify each in one line.
- Attribution: HTTP `user` becomes `user_id` (per-user limits and reports); `metadata.action_id`
  names the call site as `<service>.<feature>` in usage reports. gRPC has `attributes` and
  `action_id`.
- Always set `max_tokens` (gRPC `max_output_tokens`); keep structured output with
  `response_format` json_schema.
- Turn off the SDK's own retries and apply the [error contract](https://github.com/ai-process/ktms/blob/main/README.md#error-contract) once: wait
  out `Retry-After` on 429, back off on 503, never retry configuration errors
  (`no_capable_model`, `no_vendor_key`, `not allowed for key`).
- Log `X-KTMS-Rule`, `X-KTMS-Vendor` and the response's `model` (the model that answered) at
  debug level.

## Check

- Tests against a fake Keystone: success, a 429 with `Retry-After`, a 503 and a 502.
- `ktms route explain NAME --effort low --attr k=v` (or the MCP tool `explain_route`) for each
  effort the service uses: the chain must not end in `no_vendor_key`.
- `ktms chat --effort low "hello"` with the service's key shows the rule and vendor that served it.

Sources: [OpenAI-compatible HTTP API](https://github.com/ai-process/ktms/blob/main/README.md#openai-compatible-http-api),
[Calling Keystone](https://github.com/ai-process/ktms/blob/main/README.md#calling-keystone), [Error contract](https://github.com/ai-process/ktms/blob/main/README.md#error-contract).
