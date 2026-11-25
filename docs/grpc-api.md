# Calling Keystone

Authenticate every call with `authorization: Bearer ktms_…` (legacy `llm_…` keys supported) gRPC metadata.

You can import the committed Go stubs directly from this repository:
```bash
go get github.com/ai-process/ktms/gen/ktms/v1
```

## Client SDKs

Generate a client for your language from `proto/ktms/v1` (below). Pre-built SDKs on the Buf
Schema Registry are planned and will be announced in the CHANGELOG when they are published.

## Local Client Generation

Generate client stubs with `buf`, without installing local compiler plugins:
```bash
make proto     # Go stubs in gen/
make proto-py  # Python stubs in gen/python/
make proto-ts  # TypeScript stubs in gen/ts/
```

## GenerateText

```json
{
  "needs": {"general": "proficient", "legal": "basic"},
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
| `needs` | What the request needs: topic → `basic`, `proficient` or `expert`; see [Needs and competencies](concepts.md#needs-and-competencies). |
| `effort` | Deprecated: `EFFORT_LOW`, `EFFORT_MEDIUM`, `EFFORT_HIGH` are `general` at `basic`, `proficient`, `expert`; `needs.general` wins over it. |
| `attributes` | Opaque key/values matched by rules and copied to usage events. `user_id` charges the per-user daily budget (falls back to `svc:<key_name>` for service callers without `user_id`). `usage_project` overrides the usage project (see below). |
| `base_system_instruction`, `request_instruction` | System content never travels as a message. |
| `response_schema` | Unset = free text. Set = JSON output in that shape: types `string`, `integer`, `number`, `boolean`, `object`, `array`, plus `required`, `nullable`, `enum`, `format` and `additional_properties`. Vendors with structured output enforce it; for the others the proxy checks the reply (types, required keys, enum values, extra keys when `additional_properties` is false), retries once and then falls back. A `format` a vendor rejects is passed to the model in the description instead. |
| `tools`, `tool_choice` | Function/tool declarations with JSON Schema parameters. `tool_choice` specifies auto, required, none, or a specific named function. In non-streaming calls, arguments of `strict` tools are checked against their parameters, since not every vendor enforces strict; a call outside them is retried once and then falls back. Streamed tool calls reach the caller as they arrive and are not checked. |
| `messages` | Chat history. Supports `MESSAGE_ROLE_USER`, `MESSAGE_ROLE_ASSISTANT`, and `MESSAGE_ROLE_TOOL` (with `tool_call_id`). User messages support multimodal `parts` (`ContentPart`: `text`, `image`, `audio`, `document`). Assistant messages can carry `tool_calls`. |
| `max_output_tokens` | Ceiling, `0` = none. Output tokens are the expensive ones; set it. |
| `temperature`, `top_p`, `stop`, `seed` | Sampling, passed to the model that serves the call; unset leaves its default. `temperature` 0..2, `top_p` 0..1, up to 16 non-empty `stop` sequences; other values get `INVALID_ARGUMENT`. See [Sampling](#sampling). |
| `enable_google_search` | Only Google models declaring `google_search` can serve it. |
| `action_id` | Free-form label for usage attribution. |
| `model_override` | Exact model id; bypasses rules but **not** throttles. |
| `placement` | Compute locality preference: `nodes_first`, `nodes_only`, `cloud_first`, `cloud_only`. |
| `data_classification` | `restricted` keeps the call and the judge calls it makes on self-hosted nodes, with no cloud fallback; see [Restricted data](concepts.md#restricted-data). |
| `auto_route` | Enables smart model auto-selection; without needs or a pinned effort, a judge picks the general level. |
| `effort_specified` | Deprecated: the caller pinned the effort (skips the judge). Needs count as pinned. |

The response carries `choices`, token `usage`, `resolved_model`, `resolved_vendor`,
`matched_rule` (empty on override), structured `details` (`Choice` objects with text, tool calls, and finish reasons),
`cached` status, routing metadata: `route_mode`, `placement`, `judge_score`, and `judge_model`, and
`ignored_parameters`: the sampling fields the resolved model did not get.

## Sampling

`temperature`, `top_p`, `stop` and `seed` reach the model that serves the call, as far as its API
takes them. A field the model does not take is left out of its request rather than failing the call,
and the response names it, as it names a value the model got changed: `ignored_parameters` in gRPC (on the first chunk of a stream), the
`X-KTMS-Ignored-Params` header over HTTP. Fallback models are judged one by one, so the list
describes the model that answered.

| Vendor | Takes |
| --- | --- |
| `openai`, `azure` | All four, up to 4 `stop` sequences. Reasoning models (an id starting with `o<digit>`, `gpt-5`, `codex-mini` or `computer-use`, as a whole or after a `/`, `-` or `_`, except `gpt-5-chat`) take none: they reject `temperature`, `top_p` and `stop`, and `seed` is not confirmed for them. |
| `groq` | All four, up to 4 `stop` sequences. |
| `google` | All four, up to 5 `stop` sequences. |
| `ollama`, `local` | All four. |
| `deepseek` | `temperature`, `top_p`, `stop` (up to 16). |
| `mistral` | `temperature` (capped at 1.5), `top_p`, `stop`. |
| `anthropic`, Claude on `bedrock` | `stop`, without sequences of whitespace alone. Claude 4.6 and earlier also take `temperature` (capped at 1) and `top_p`; from Claude 4 on, not both: with both set, only `temperature`. Claude 4.7 and later accept only the default sampling, so they get neither. |
| Other `bedrock` models | `temperature` (capped at 1), `top_p`, up to 4 `stop` sequences. |
| `bedrock` application inference profiles (ARNs) | Up to 4 `stop` sequences: the ARN does not say which model is behind it. With `sampling` declared, also `temperature` (capped at 1) or `top_p`, not both, which every family takes. |

A temperature above a vendor's cap is lowered to it and reported as `temperature`. When a vendor
takes fewer stop sequences than the call has, it gets the first ones and `stop` is reported. The model is told apart
by its id. When the id misleads, declare a capability: `no_sampling` for a model whose API rejects a
field this table sends, `sampling` for one that takes what its vendor's table row allows, such as
an Azure deployment of a chat model named like a reasoning one. Sampling is part of the response
cache key. Batch results do not carry `ignored_parameters`. Over HTTP, empty stop sequences are
dropped before the call.

## GenerateTextStream

Server-streaming RPC `rpc GenerateTextStream(GenerateTextStreamRequest) returns (stream GenerateTextChunk)`.

- Takes a `GenerateTextStreamRequest` containing the standard `GenerateTextRequest`.
- Streams incremental `GenerateTextChunk` items containing `delta` (token text), `tool_call_chunks`, `resolved_model`, `resolved_vendor`, `matched_rule`, `cached`, `route_mode`, `placement`, `judge_score`, `judge_model`, `ignored_parameters`, and final `finish_reason` with token `usage`.
- **Fallback guarantee:** If upstream models fail before the first chunk is sent, the router falls back to the next model in the rule's chain. Once the first chunk is transmitted to the caller, the stream is locked to that model to guarantee integrity.
- Usage events and daily token budgets are settled on stream completion.

## Batch text generation

For offline or bulk workflows (such as page generation, backfills, or data pipelines) that prioritize cost over latency, batch generation processes thousands of items asynchronously through vendor Batch APIs (e.g., Google Gemini Batch API) at a **50% discount** compared to interactive calls. Jobs typically finish within 24 hours (vendor timeout at 48 hours).

- **Route resolution:** Resolved once upon submission. The proxy selects the first model in the rule's chain (or, without a matching rule, in the [selection](concepts.md#needs-and-competencies)) that has the `batch` capability, a vendor key for the client (not needed for `local` models), and available daily budget. Once submitted, there is no vendor fallback; per-item outcomes are returned to the client.
- **Privacy:** Prompts are streamed/submitted to the vendor and never persisted in the proxy database.
- **Throttling & Accounting:** RPM limits do not apply to batches. Daily token budgets are checked at submission (rejecting if already exhausted) and tokens are deducted when final item results arrive. Results are retained for `RESULTS_RETENTION_DAYS` (default 7 days; accepts integer days or Go duration strings).

### SubmitBatch

Takes a list of items (`custom_id` unique within the batch, and standard `GenerateTextRequest`), batch-level `needs` (or the deprecated `effort`) and `attributes`, and an optional `client_batch_id` for idempotency (resubmitting with the same client batch ID returns the existing batch):

```bash
grpcurl -plaintext -H "authorization: Bearer ktms_<key>" -d '{
  "needs": {"general": "basic"},
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

### GetBatch

Polls batch state (`PENDING`, `RUNNING`, `SUCCEEDED`, `FAILED`, `CANCELLED`, `EXPIRED`) and progress counts (`total`, `done`, `failed`):

```bash
grpcurl -plaintext -H "authorization: Bearer ktms_<key>" -d '{
  "batch_id": "0199c0a0-6228-7e10-91b5-685d996cbfa1"
}' localhost:9090 ktms.v1.GatewayService/GetBatch
```

### ListBatchResults

Pages through completed items with `page_token` (and `next_page_token`). Each item contains its `custom_id` and either a successful `response` (`GenerateTextResponse` with choices, usage, resolved_model, resolved_vendor, matched_rule) or an `error` (`BatchItemError` with gRPC code, message, and error reason):

```bash
grpcurl -plaintext -H "authorization: Bearer ktms_<key>" -d '{
  "batch_id": "0199c0a0-6228-7e10-91b5-685d996cbfa1",
  "page_size": 100
}' localhost:9090 ktms.v1.GatewayService/ListBatchResults
```

### CancelBatch

Requests cancellation of an unfinished batch job at the vendor. The batch remains `RUNNING` until vendor confirmation is polled: already completed items preserve their results, while remaining unfinished items fail with reason `batch_cancelled`.

```bash
grpcurl -plaintext -H "authorization: Bearer ktms_<key>" -d '{
  "batch_id": "0199c0a0-6228-7e10-91b5-685d996cbfa1"
}' localhost:9090 ktms.v1.GatewayService/CancelBatch
```

## SynthesizeSpeech, GenerateImage

`SynthesizeSpeech` takes `text` (read verbatim) and an optional `language` and returns OGG/Opus
`audio`. `GenerateImage` takes a `prompt` and returns `image` bytes with a `mime_type`.
Both accept `attributes`, `action_id` and `model_override`, and need the rules in
[Speech, image and judgment rules](configuration.md#speech-image-and-judgment-rules).

## Judge

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

## Translation

`Translate` (HTTP `POST /v1/translate`, same JSON) translates a batch of texts. The client always
makes the same call; which engine translates is the translate rule's choice — a local MT model on
a node, any chat model by prompt, a cloud MT vendor — with the usual fallback, limits, guardrails
and usage reporting. Comparing engines is a config change, not a code change.

```json
{"texts": ["Great location, friendly staff.", "Chambre trop petite."],
 "source_language": "",          // BCP-47; empty detects each text
 "target_language": "de",
 "format": "text",               // or "html": tags and attributes are kept
 "attributes": {"pipeline": "reviews"}, "action_id": "reviews.translate"}
```

The reply holds one `translations[]` entry per text, in order (`text`,
`detected_source_language`), with `resolved_model`, `matched_rule` and `usage` (tokens as the
engine counts them, and `characters`). Limits: 128 texts and 256 KiB per call. Blank texts come
back as they are, and a `source_language` equal to the target returns the texts without a call.

Models that translate declare the `translate` capability and the pairs they serve; a model
without the request's pair is skipped, not tried. `languages` is required on a translate model (`"*"` for
any). A tag covers its longer variants: `"de"` takes `"de-AT"`, `"zh-Hant"` takes `"zh-Hant-TW"`.

```yaml
models:
  - id: opus-mt-de              # a node MT model (ktms-mt, see Run a node)
    vendor: local
    efforts: [EFFORT_LOW]
    capabilities: [translate]
    languages: {source: [en, fr, es, it, nl, pt, pl, ru, tr], target: [de]}
  - id: gemma4:26b              # any chat model, by prompt
    vendor: local
    efforts: [EFFORT_LOW]
    capabilities: [translate]
    languages: {source: ["*"], target: ["*"]}
  - id: gemini-2.5-flash
    vendor: google
    efforts: [EFFORT_LOW, EFFORT_MEDIUM]
    capabilities: [translate]
    languages: {source: ["*"], target: ["*"]}
    price_in_per_mtok: 0.30
    price_out_per_mtok: 2.50
rules:
  - name: translate             # local MT first, then the local LLM, then the cloud
    priority: 170
    attributes: {modality: translate}
    use: [opus-mt-de, gemma4:26b, gemini-2.5-flash]
  - name: translate-gemma       # a pipeline that asks for Gemma by an attribute
    priority: 171
    attributes: {modality: translate, engine: gemma}
    use: [gemma4:26b, gemini-2.5-flash]
```

A chat model translates through a prompt Keystone writes: the texts go as JSON data, the reply
is structured output with one item per text, and Keystone checks the count and the ids before
answering. A reply that does not line up is retried once on the same model, then the chain moves
on. Batches are split into chunks of up to 32 texts.

Usage goes to light-tokenmeter like any call: `action_id` defaults to `translate`, tokens are
reported as usual, the character count goes in `meta.characters`, and `cost_nanousd` adds
`price_per_million_chars` (for engines billed by character) to the token price. A failed
attempt bills its tokens only; the characters are billed once, by the translation delivered.
Spend limits count both. `ktms translate --to de [--from en] [FILE|-]` translates a file line by line, and
`ktms route explain KEY --modality translate --from fr --to de` shows which models a pair reaches.

## Embeddings

Vector representations of text. Vector space isolation is strictly enforced: `auto` model routing and cross-model fallbacks are disallowed to prevent corrupting downstream vector indices. Models must declare the `embedding` capability.

```bash
grpcurl -plaintext -H "authorization: Bearer ktms_<key>" -d '{
  "model": "text-embedding-3-small",
  "inputs": ["Hello world", "Search document"]
}' localhost:9090 ktms.v1.GatewayService/GetEmbeddings
```

## ListModels

Returns the models the calling key can use (those whose vendor it holds a credential for), with
competencies (efforts read as `general`), the deprecated efforts, capabilities, rpm and prices.
