# HTTP APIs

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
| `POST /v1/translate` | `Translate` | Protobuf JSON of `TranslateRequest` / `TranslateResponse` (see [Translation](grpc-api.md#translation)). |
| `GET /v1/models`, `GET /v1/models/{id}` | `ListModels` | Only models the key holds a vendor credential for. |

Authentication is `Authorization: Bearer ktms_…` (legacy `llm_…` keys are also supported). Response headers provide full observability:
- `X-KTMS-Vendor` and `X-KTMS-Rule`: vendor and matching rule that served the request.
- `X-KTMS-Placement`: infrastructure placement (`nodes_first`, `nodes_only`, `cloud_first`, `cloud_only`).
- `X-KTMS-Route-Mode`: routing mechanism (`direct`, `override`, `auto-judge`, `auto-default`, `auto-effort`).
- `X-KTMS-Judge-Score` and `X-KTMS-Judge-Model`: evaluation confidence and judge model ID when judge evaluates task complexity.
- `X-KTMS-Ignored-Params`: the sampling fields the serving model did not get, comma-separated (see [Sampling](grpc-api.md#sampling)). It has no `X-LLM-Proxy-*` twin.

> [!NOTE]
> **Backwards Compatibility**: All legacy `X-LLM-Proxy-*` response headers are dual-emitted alongside `X-KTMS-*` with standard RFC 8594 `Deprecation: true`, `Sunset`, and `Warning` headers to provide seamless zero-downtime migration for existing clients.

### Choosing a model and placement

The `model` parameter supports smart auto-routing and infrastructure placement:

| `model` | Behaviour |
| --- | --- |
| `auto` (or empty) | Smart auto-selection with default `nodes_first` placement. Evaluates prompt complexity via Judge (or uses `reasoning_effort` if provided). |
| `auto:<topic>=<level>,…` (e.g. `auto:medical=basic,legal=proficient`) | Routes by these [needs](concepts.md#needs-and-competencies) with `nodes_first` placement; combines with a placement by `:`, e.g. `auto:medical=basic:cloud-first`. |
| `auto:low`, `auto:medium`, `auto:high` | Pinned general need (`basic`, `proficient`, `expert`) with `nodes_first` placement. Deprecated in favour of `auto:general=<level>`. |
| `auto:nodes-first`, `auto:nodes-only`, `auto:cloud-first`, `auto:cloud-only` | Auto-routing with explicit compute locality policy. |
| `auto:<placement>:<effort>` (e.g. `auto:nodes-only:high`) | Combined placement policy and pinned effort level. |
| anything else | Exact registry model ID (`model_override`): bypasses rules but not throttles. |

#### Infrastructure Placement Policies
- `nodes_first` (default): Prefers registered self-hosted compute nodes (`VendorLocal`, `VendorOllama`), falling back to cloud providers if unavailable or busy.
- `nodes_only` (aliases: `nodes`, `private`, `self_hosted`): Restricts routing strictly to self-hosted nodes. Fails if no capable node is available. A preference of the request; for a guarantee that also covers the judge and holds per key or team, see [Restricted data](concepts.md#restricted-data).
- `cloud_first`: Prefers managed cloud providers, falling back to registered nodes.
- `cloud_only`: Excludes self-hosted nodes completely.

Placement precedence:
1. `X-KTMS-Placement` (or legacy `X-LLM-Proxy-Placement`) HTTP header
2. Request body `"placement": "..."`
3. Model suffix (e.g. `model: "auto:nodes-only"`)
4. Default: `nodes_first`

#### Fast Judge Auto-Selection
When `model: "auto"` is requested without an explicit effort level, the proxy evaluates prompt complexity using the configured Judge model (such as `jev-latest` or OpenAI Decisions) with a 250ms deadline. If the judge is not configured or times out, it safely falls back to standard medium effort without dropping the request.

Needs and effort precedence:
1. Needs in the model string (e.g. `auto:medical=basic`), then the `X-KTMS-Needs` header
   (`medical=basic, legal=proficient`) for the topics the model leaves out. The header is for SDKs
   that cannot add a body field. Needs pin the route: no judge.
2. Pinned effort in the model string (e.g. `auto:low`): the general need, unless the needs name `general`.
3. `reasoning_effort` request field (`low`, `medium`, `high`): in 1.x it keeps meaning the general
   need for `auto` and skips the judge; deprecated. It is not passed to the vendor.
4. Dynamic Judge evaluation of the general level (fails open to `medium`).

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
| `temperature`, `top_p`, `stop`, `seed` | Passed to the model as far as it takes them; the rest is named in `X-KTMS-Ignored-Params` (see [Sampling](grpc-api.md#sampling)). `stop` is a string or an array. |
| `logprobs`, penalties… | Accepted and ignored. |

The response is a standard `chat.completion` with `finish_reason` (`"stop"`, `"tool_calls"`, `"length"`, or `"content_filter"`) and token `usage`.

### Speech and images

`POST /v1/audio/speech` takes `model`, `input` and an optional `language` extension (pronunciation
hint, e.g. `"Serbian"`); `voice`, `response_format` and `speed` are ignored. The body of the response
is OGG/Opus whatever format was requested; check the `Content-Type`.

`POST /v1/images/generations` takes `model` and `prompt`; `n` must be 1 and `response_format: "url"`
is rejected (the image is returned as `data[0].b64_json`).

Both route by a rule for the `tts` / `image` [modality](configuration.md#speech-image-and-judgment-rules), or, once models declare competencies, by selection among the models with that capability. `X-KTMS-Needs` narrows them; an effort in the model value (`auto:high`) is ignored here, as before.

### Errors

Errors use OpenAI's shape, `{"error": {"message", "type", "param", "code"}}`, and the proxy's
[error contract](errors.md#error-contract) shows up in `code`:

| HTTP | `type` | `code` examples |
| --- | --- | --- |
| 400 | `invalid_request_error` | `output_truncated`, `no_capable_model`, `unsupported_language_pair`, `no_vendor_key`, `not_configured`; validation messages |
| 401 / 403 | `authentication_error` / `permission_error` | missing or invalid key, missing scope |
| 429 | `rate_limit_error` | `throttled:rpm`, `throttled:key_rpm`, `throttled:key_tpm`, `throttled:budget_key`, `throttled:budget_user` (with `Retry-After`) |
| 429 | `insufficient_quota` | `insufficient_quota`; the message names the cap, e.g. `throttled:spend_key_daily`. `Retry-After` is the seconds until the period resets (UTC midnight or month start). |
| 501 | `server_error` | `ffmpeg_missing` (Gemini speech synthesis invoked without ffmpeg on PATH or in container) |
| 502 | `server_error` | `vendor_error` |
| 503 | `server_error` | `vendors_unavailable`, `auth_unavailable`, `storage_unavailable`, `throttled:spend_unavailable` (with `SPEND_LIMITS_FAIL_MODE=closed`): retry with backoff |

OpenAI SDKs retry 429 and 5xx automatically; for `throttled:*` and `vendor_error` you may want to
disable that (`max_retries=0`) and handle them yourself.

## Anthropic Messages API

`POST /v1/messages` serves the [Anthropic Messages API](https://docs.anthropic.com/en/api/messages)
on `HTTP_ADDR`, so Claude Code and the Anthropic SDKs run through Keystone. It is the same pipeline
as `/v1/chat/completions`: the same keys (scope `generate`), rules, fallback, limits, guardrails,
cache, `X-KTMS-*` headers and usage events. The model that answers may be any vendor's, and the
answer is still a Messages response. The Messages API is [experimental](stability.md#experimental) in 1.x.

Claude Code, with a KTMS key:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=ktms_<your key>        # or ANTHROPIC_API_KEY
export ANTHROPIC_MODEL=auto                        # or a registry model id
export ANTHROPIC_DEFAULT_HAIKU_MODEL=auto:low      # Claude Code's background calls
claude
```

Claude Code warns once that it does not recognize the model `auto`; that is expected. It asks for
up to 32,000-64,000 output tokens, which Keystone passes on as `max_tokens`; if a model your rules
pick caps output lower, set `CLAUDE_CODE_MAX_OUTPUT_TOKENS` to fit it.

The Anthropic SDKs need only the base URL and the key:

```python
import anthropic

client = anthropic.Anthropic(base_url="http://localhost:8080", api_key="ktms_<your key>")
reply = client.messages.create(model="auto", max_tokens=1024,
                               messages=[{"role": "user", "content": "Say hi in three words."}])
print(reply.content[0].text, reply.model)   # `model` is the one that answered
```

| Request field | Support |
| --- | --- |
| `model` | As on chat completions: `auto`, `auto:<topic>=<level>,…`, `auto:<effort>`, `auto:<placement>`, or a registry model id. `X-KTMS-Needs` applies too. |
| `system` | A string or `text` blocks, joined into the system instruction. |
| `messages` | `user` and `assistant` turns with `text`, `image` and `document` blocks (`base64` or `https` `url` sources; `text` and `content` documents become text), `tool_use` and `tool_result` (text and images). `thinking`, `redacted_thinking` and server-tool blocks in assistant turns are dropped: another model cannot continue them. |
| `tools`, `tool_choice` | Client tools with `input_schema` (the same JSON Schema subset as `response_format`); `tool_choice` `auto`, `any`, `tool` and `none`. Server tools such as `web_search` are rejected with `400`. |
| `max_tokens` | Required, the output ceiling. |
| `stream` | SSE events `message_start`, `ping`, `content_block_start`, `content_block_delta` (`text_delta`, `input_json_delta`), `content_block_stop`, `message_delta` and `message_stop`. `message_start` carries an input token estimate; `message_delta` carries the vendor's counts. |
| `metadata.user_id` | `attributes["user_id"]`, as `user` on chat completions; other `metadata` keys are attributes, and `action_id` is the usage action label. Claude Code's `user_id` names a session: Keystone keeps its account (or, without one, its device) as the user and puts the session in `attributes["session_id"]`, so per-user limits and reports span sessions. |
| `enable_cache`, `placement` | Extensions, as on chat completions; so are the `X-KTMS-Cache`, `X-KTMS-Placement` and `X-KTMS-Needs` request headers (set them in Claude Code with `ANTHROPIC_CUSTOM_HEADERS`). |
| `temperature`, `top_p`, `stop_sequences` | Passed to the model as on chat completions (see [Sampling](grpc-api.md#sampling)). A stop sequence ends the answer with `stop_reason` `end_turn`. |
| `top_k` | Accepted, sent to no model, and named in `X-KTMS-Ignored-Params`. |
| `thinking`, `service_tier` | Accepted and ignored. |

The key goes in `x-api-key` or `Authorization: Bearer`. The response's `stop_reason` is `end_turn`,
`tool_use`, `max_tokens` or `refusal`, and `usage` splits `cache_read_input_tokens` and
`cache_creation_input_tokens` out of `input_tokens`. Tool call ids are the vendor's, or `toolu_…`
when the vendor gives none.

`POST /v1/messages/count_tokens` answers with the estimate the token budgets reserve, not a
tokenizer's count: about four characters per token, tool definitions included, a flat allowance
per image or document (`url` sources are not fetched), and the `X-KTMS-Token-Count: estimate`
header.

Errors use the Messages shape, `{"type": "error", "error": {"type", "message"}}`, with the HTTP
statuses of [the table above](#errors): `invalid_request_error` (400), `authentication_error`
(401), `permission_error` (403), `not_found_error` (404), `rate_limit_error` (429, with
`Retry-After`), `overloaded_error` (503), `timeout_error` (504) and `api_error` for the rest. The
machine-readable reason, such as `throttled:rpm`, is the `message`. An error after the stream has
started arrives as an `error` event.
