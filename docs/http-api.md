# HTTP APIs

**The problem.** Most code and tools already speak the OpenAI or Anthropic API. Rewriting them for
a gateway's own protocol is work nobody wants to do.

**The approach.** Keystone serves both APIs on `HTTP_ADDR` (`:8080`). You change the base URL and the
key, and get Keystone's routing, fallback, limits and usage accounting. Everything is shared with
gRPC: the same `ktms_…` keys (scope `generate`), vendor keys, rules and usage events. The `model`
field tells Keystone what the call needs instead of naming a model:

<p align="center"><img src="img/http-api-model.svg" alt="The model string auto:medical=basic,legal=proficient:cloud-first has three parts: auto lets Keystone choose, then the needs as topic=level pairs, then the placement" width="800"></p>

**Contents:** [Glossary](#glossary) · [Try it](#openai-compatible-http-api) · [Endpoints](#endpoints) ·
[Choosing a model and placement](#choosing-a-model-and-placement) ·
[Chat completions fields](#chat-completions-what-is-supported) · [Speech and images](#speech-and-images) ·
[Errors](#errors) · [Anthropic Messages API](#anthropic-messages-api)

## Glossary

| Term | Meaning |
| --- | --- |
| **`auto`** | Let Keystone choose the model by rules or selection. |
| **Needs** | `topic=level` pairs, such as `medical=basic`; see [Needs and competencies](concepts.md#needs-and-competencies). |
| **Placement** | Where a call may run: `nodes_first`, `nodes_only`, `cloud_first`, `cloud_only`. |
| **Judge** | A fast model that guesses how hard a prompt is when `auto` comes without needs. |
| **Attributes** | Key–value context for rules and usage, taken from `metadata` and `user`. |

## OpenAI-compatible HTTP API

Point an OpenAI SDK, LangChain or `curl` at Keystone. `HTTP_API_ENABLED=false` turns the API off.

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
| `POST /v1/messages` | `GenerateText` / `GenerateTextStream` | The [Anthropic Messages API](#anthropic-messages-api). |
| `POST /v1/embeddings` | `GetEmbeddings` | Vector embeddings with `float` or `base64` encoding format and optional dimensionality. |
| `POST /v1/audio/speech` | `SynthesizeSpeech` | Returns OGG/Opus audio. |
| `POST /v1/images/generations` | `GenerateImage` | Returns `b64_json`. |
| `POST /v1/decisions` | `Judge` | OpenAI Decisions API format (`predicate`, `choice`, `score`). `input` is JSON state, or an array of `input_text`/`text`, `input_image`/`image_url`, `input_audio` and `input_embedding` parts for multimodal state. |
| `POST /v1/decisions/rollouts` | `EvaluateRollouts` | Batch trajectory and rollout candidate evaluation for agentic workflows with candidate ranking. |
| `POST /v1/judge` | `Judge` | Protobuf JSON of `JudgeRequest` / `JudgeResponse`. |
| `POST /v1/translate` | `Translate` | Protobuf JSON of `TranslateRequest` / `TranslateResponse` (see [Translation](grpc-api.md#translation)). |
| `GET /v1/models`, `GET /v1/models/{id}` | `ListModels` | Only models the key holds a vendor credential for. |

Authentication: `Authorization: Bearer ktms_…` (legacy `llm_…` keys work too).

**Response headers** say how the call was routed:

| Header | Meaning |
| --- | --- |
| `X-KTMS-Vendor`, `X-KTMS-Rule` | The vendor and the rule that served the call. |
| `X-KTMS-Placement` | `nodes_first`, `nodes_only`, `cloud_first` or `cloud_only`. |
| `X-KTMS-Route-Mode` | `direct`, `override`, `auto-judge`, `auto-default` or `auto-effort`. |
| `X-KTMS-Judge-Score`, `X-KTMS-Judge-Model` | The judge's confidence and model, when a judge picked the level. |
| `X-KTMS-Ignored-Params` | Sampling fields the serving model did not get, comma-separated (see [Sampling](grpc-api.md#sampling)). No `X-LLM-Proxy-*` twin. |

> [!NOTE]
> The legacy `X-LLM-Proxy-*` response headers are sent next to `X-KTMS-*`, with RFC 8594
> `Deprecation: true`, `Sunset` and `Warning` headers, so older clients keep working while they move.

### Choosing a model and placement

| `model` | Behaviour |
| --- | --- |
| `auto` (or empty) | Keystone chooses, with `nodes_first` placement. A judge guesses the general level, unless `reasoning_effort` is given. |
| `auto:<topic>=<level>,…` (e.g. `auto:medical=basic,legal=proficient`) | Routes by these [needs](concepts.md#needs-and-competencies) with `nodes_first` placement; combines with a placement by `:`, e.g. `auto:medical=basic:cloud-first`. |
| `auto:low`, `auto:medium`, `auto:high` | Pinned general need (`basic`, `proficient`, `expert`) with `nodes_first`. Deprecated: use `auto:general=<level>`. |
| `auto:nodes-first`, `auto:nodes-only`, `auto:cloud-first`, `auto:cloud-only` | Auto routing with a placement. |
| `auto:<placement>:<effort>` (e.g. `auto:nodes-only:high`) | A placement and a pinned effort. |
| anything else | An exact registry model id (`model_override`): bypasses rules, not limits. |

**Placement policies.**

| Placement | Aliases | Behaviour |
| --- | --- | --- |
| `nodes_first` (default) | | Your nodes (`local`, `ollama`) first, cloud vendors when they are unavailable or busy. |
| `nodes_only` | `nodes`, `private`, `self_hosted` | Only your nodes; fails when none can serve. A preference of the request: for a guarantee that also covers the judge and holds per key or team, see [Restricted data](concepts.md#restricted-data). |
| `cloud_first` | | Cloud vendors first, then your nodes. |
| `cloud_only` | | Never your nodes. |

Where the placement comes from, first wins:

1. the `X-KTMS-Placement` header (or legacy `X-LLM-Proxy-Placement`);
2. `"placement"` in the body;
3. the model suffix, e.g. `auto:nodes-only`;
4. the default, `nodes_first`.

**Needs and effort**, first wins:

1. Needs in the model string (`auto:medical=basic`), then the `X-KTMS-Needs` header
   (`medical=basic, legal=proficient`) for topics the model string leaves out. The header is for SDKs
   that cannot add a body field. Needs pin the route: no judge.
2. A pinned effort in the model string (`auto:low`): the general need, unless the needs name `general`.
3. `reasoning_effort` (`low`, `medium`, `high`): in 1.x the general need for `auto`, skipping the
   judge; deprecated, and not passed to the vendor.
4. The judge: for `model: "auto"` without any of the above, the configured judge model (such as
   `jev-latest` or OpenAI Decisions) rates the prompt within 250 ms. Without a judge, or when it is
   late, the level is `medium`.

**Attributes** (what rules match and usage events carry) come from OpenAI's own fields:

| Field | Becomes |
| --- | --- |
| `metadata` | Attributes, as they are. |
| `user` | `attributes["user_id"]`, unless `metadata.user_id` is set. |
| `metadata.action_id` | The usage action label. |

```json
{"model": "auto:nodes-first", "user": "u-42", "metadata": {"subject": "french", "action_id": "app.quiz"},
 "messages": [{"role": "user", "content": "Translate: good morning"}]}
```

### Chat completions: what is supported

| Request field | Support |
| --- | --- |
| `messages` | `system`/`developer` messages become the system instruction, `user`/`assistant` the conversation. Content is a string or parts: text, `image_url`, `input_audio`, `file`. |
| `max_tokens`, `max_completion_tokens` | Output ceiling (`max_output_tokens`). Set one: an unbounded answer is the costliest failure. |
| `response_format` | `json_schema` is enforced: `type` (incl. `number` and `["string","null"]`), `properties`, `items`, `required`, `enum`, `format` and boolean `additionalProperties` are carried. Enums on non-string types, and formats a vendor rejects, reach the model as a hint in the description. Other keywords (`minimum`, `pattern`, `oneOf`, ...) are dropped. `json_object` asks for JSON in the prompt. |
| `stream` | Real incremental SSE. Once the first chunk is sent, the stream stays on that model; `stream_options.include_usage` is honoured. |
| `reasoning_effort`, `user`, `metadata` | Routing, as above. |
| `enable_google_search` | Extension: let a Google model ground the answer in search. |
| `n` | Must be 1. |
| `tools`, `tool_choice` | Function declarations with JSON Schema parameters. `tool_choice`: `"auto"`, `"none"`, `"required"` or a function name. |
| `role: "tool"` | Tool responses with `tool_call_id`. |
| `temperature`, `top_p`, `stop`, `seed` | Passed to the model as far as it takes them; the rest is named in `X-KTMS-Ignored-Params` (see [Sampling](grpc-api.md#sampling)). `stop` is a string or an array. |
| `logprobs`, penalties… | Accepted and ignored. |

The response is a standard `chat.completion` with `finish_reason` (`"stop"`, `"tool_calls"`,
`"length"` or `"content_filter"`) and token `usage`.

### Speech and images

| Endpoint | Takes | Ignores or refuses | Returns |
| --- | --- | --- | --- |
| `POST /v1/audio/speech` | `model`, `input`, optional `language` (a pronunciation hint, e.g. `"Serbian"`) | `voice`, `response_format` and `speed` are ignored | OGG/Opus whatever format was asked; check `Content-Type`. |
| `POST /v1/images/generations` | `model`, `prompt` | `n` must be 1; `response_format: "url"` is refused | `data[0].b64_json` |

Both route by a rule for the `tts` / `image` [modality](configuration.md#speech-image-and-judgment-rules),
or, once models declare competencies, by selection among the models with that capability.
`X-KTMS-Needs` narrows them; an effort in the model value (`auto:high`) is ignored here.

### Errors

Errors use OpenAI's shape, `{"error": {"message", "type", "param", "code"}}`, with the reason from
the [error contract](errors.md#error-contract) in `code`:

| HTTP | `type` | `code` examples |
| --- | --- | --- |
| 400 | `invalid_request_error` | `output_truncated`, `no_capable_model`, `unsupported_language_pair`, `no_vendor_key`, `not_configured`; validation messages |
| 401 / 403 | `authentication_error` / `permission_error` | missing or invalid key, missing scope |
| 429 | `rate_limit_error` | `throttled:rpm`, `throttled:key_rpm`, `throttled:key_tpm`, `throttled:budget_key`, `throttled:budget_user` (with `Retry-After`) |
| 429 | `insufficient_quota` | `insufficient_quota`; the message names the cap, e.g. `throttled:spend_key_daily`. `Retry-After` is the seconds until the period resets (UTC midnight or month start). |
| 501 | `server_error` | `ffmpeg_missing` (Gemini speech synthesis invoked without ffmpeg on PATH or in container) |
| 502 | `server_error` | `vendor_error` |
| 503 | `server_error` | `vendors_unavailable`, `auth_unavailable`, `storage_unavailable`, `throttled:spend_unavailable` (with `SPEND_LIMITS_FAIL_MODE=closed`): retry with backoff |

OpenAI SDKs retry 429 and 5xx by themselves. For `throttled:*` and `vendor_error` you may want to turn
that off (`max_retries=0`) and handle them yourself.

## Anthropic Messages API

`POST /v1/messages` serves the [Anthropic Messages API](https://docs.anthropic.com/en/api/messages),
so Claude Code and the Anthropic SDKs run through Keystone. It is the same pipeline as
`/v1/chat/completions`: keys, rules, fallback, limits, guardrails, cache, `X-KTMS-*` headers and
usage events. The model that answers may be any vendor's; the answer is still a Messages response.
[Experimental](stability.md#experimental) in 1.x.

**Claude Code**, with a KTMS key:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=ktms_<your key>        # or ANTHROPIC_API_KEY
export ANTHROPIC_MODEL=auto                        # or a registry model id
export ANTHROPIC_DEFAULT_HAIKU_MODEL=auto:low      # Claude Code's background calls
claude
```

- Claude Code warns once that it does not know the model `auto`; that is expected.
- It asks for 32,000–64,000 output tokens, passed on as `max_tokens`. If a model your rules pick
  caps output lower, set `CLAUDE_CODE_MAX_OUTPUT_TOKENS` to fit it.

**The Anthropic SDKs** need only the base URL and the key:

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

**The response.**

- The key goes in `x-api-key` or `Authorization: Bearer`.
- `stop_reason` is `end_turn`, `tool_use`, `max_tokens` or `refusal`.
- `usage` splits `cache_read_input_tokens` and `cache_creation_input_tokens` out of `input_tokens`.
- Tool call ids are the vendor's, or `toolu_…` when the vendor gives none.

**`POST /v1/messages/count_tokens`** returns the estimate the token budgets reserve, not a
tokenizer's count: about four characters per token, tool definitions included, a flat allowance per
image or document (`url` sources are not fetched), with the header `X-KTMS-Token-Count: estimate`.

**Errors** use the Messages shape, `{"type": "error", "error": {"type", "message"}}`, with the HTTP
statuses of [the table above](#errors):

| HTTP | `error.type` |
| --- | --- |
| 400 | `invalid_request_error` |
| 401 | `authentication_error` |
| 403 | `permission_error` |
| 404 | `not_found_error` |
| 429 | `rate_limit_error`, with `Retry-After` |
| 503 | `overloaded_error` |
| 504 | `timeout_error` |
| other | `api_error` |

The machine-readable reason, such as `throttled:rpm`, is the `message`. An error after the stream has
started arrives as an `error` event.
