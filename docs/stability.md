# API stability

From 1.0, KTMS follows [semantic versioning](https://semver.org): within 1.x nothing in the public
API below changes incompatibly. Breaking changes wait for 2.0, except the one removal announced
before 1.0 and listed under [Deprecated](#deprecated). Minor releases add features; patch releases
fix bugs.

## What is public

| Surface | What stays compatible in 1.x |
| --- | --- |
| gRPC `ktms.v1` (`GatewayService`, `AdminService`, `NodeRegistryService`) | No RPC, field, enum value or message is removed or renamed, and no field number or type changes. New RPCs and fields may appear. `buf breaking` checks every pull request. |
| HTTP `/v1` (OpenAI-compatible) and `X-KTMS-*` headers | Paths, request and response fields, and header names, except the experimental ones listed below. New fields and endpoints may appear. |
| Probes and metrics endpoints | `/healthz` and `/readyz` on `HTTP_ADDR`, `/metrics` and `/healthz` on `METRICS_ADDR`. |
| Error contract | The status codes and reason strings in [Error contract](errors.md#error-contract) and the HTTP [Errors](http-api.md#errors) table keep their meaning. New reasons may appear, so treat unknown ones like their status code. |
| Environment variables | Names, defaults and meaning of every variable in [Configuration reference](configuration.md#configuration-reference). |
| Database | Any 1.x release upgrades the database of an earlier one by itself (`AUTO_MIGRATE`, forward-only). No manual SQL steps. The tables are not an API: read and write through the RPCs. |
| Prometheus metrics | Names and labels of `ktms_*` metrics, except the experimental ones listed below. The `effort` label stays: for a request with needs it is the `general` need as `low`, `medium` or `high`, and `default` without one (as speech and images always had). Needs get no label: topics are unbounded. |
| Usage events (`usage.v1`) | Field numbers and meaning, checked by `buf breaking` like `ktms.v1`. The meta keys KTMS writes keep their format, `effort` and `needs` (1.7) included. |
| Needs and competencies (1.7) | `ModelSpec.competencies`, `needs` on requests and rules, `ModelInfo.competencies`, the HTTP model `auto:<topic>=<level>,…` and the `X-KTMS-Needs` header, the levels `basic` < `proficient` < `expert` and their meaning: a model meets a need at its level or below. |
| Node agent | A 1.x agent works with any 1.x gateway, in both directions. |
| Helm chart (`oci://ghcr.io/ai-process/charts/ktms`) | Value names and meaning; new values may appear. The chart version is the release version. |

Not public, and free to change in any release: Redis key layout, log message text, the
`internal/` Go packages, CLI output of the tools in `cmd/` other than their flags, and benchmark
numbers.

## Experimental

These work and are supported, but may change in a minor release. The change is then listed in the
[CHANGELOG](../CHANGELOG.md). In the proto they carry an `Experimental:` comment.

- **The configuration file format** (`api_version: ktms.ai-process.dev/v1alpha1`) and the
  `ktms` command, until the format reaches `v1`.

- **The judge's choice of the general level:** when a request is auto-routed without needs or a
  pinned effort (`model: "auto"` or `auto:<placement>` over HTTP without `reasoning_effort` or
  `X-KTMS-Needs`; `auto_route` without `needs` and with `effort_specified` false in gRPC), a judge
  picks the `general` level. Its choice, the `auto-judge` and `auto-default` values of
  `route_mode`, `judge_score`, `judge_model` and the `X-KTMS-Judge-Score` and
  `X-KTMS-Judge-Model` headers are experimental. Auto-routing with needs or a pinned effort
  (`auto:<topic>=<level>`, `auto:<effort>`, `auto:<placement>:<effort>`, route mode
  `auto-effort`), placement policies, `model_override` and the `X-KTMS-Route-Mode` header are
  stable.
- **Selection without a rule (1.7):** its order (placement class, then excess, then the blended
  price `(3 × price_in + price_out) / 4` per million tokens, or the price per million characters
  for translation, then the model id) and the chain of three, and the `excess` and
  `blended_price_per_mtok` fields of `ExplainRoute`. Which models qualify (needs met, capabilities,
  vendor key, `allowed_models`) is stable.
- **Shadow traffic and its evaluation:** the `shadow_model`, `shadow_percentage` and `shadow_eval`
  rule fields, and the `ktms_shadow_*` metrics.
- **Latent features:** the semantic cache (`SEMANTIC_CACHE_*`), latent guardrails
  (`GUARDRAILS_LATENT_*`), the encoder settings (`LATENT_*`), and embeddings in judge state
  (`EmbeddingPart`, `input_embedding` over HTTP).
- **`JudgeStream` and `EvaluateRollouts`**, and `POST /v1/decisions/rollouts` over HTTP.
- **The MCP gateway:** `/mcp`, the `MCPServerSpec` RPCs, `allowed_mcp`, the `mcp_servers` and
  `mcp_credentials` file sections, the `ktms_mcp_*` metrics and `ToolCall` in usage events.
- **The agents' inbox:** the `agents` MCP server and its tools, `ListAgents`, `SetAgentRecording`,
  `ListAgentTasks`, `GetAgentTask`, `AGENTS_*`, the journal's event format and the
  `ktms_agent_*` metrics.
- **The Anthropic Messages API:** `POST /v1/messages`, `POST /v1/messages/count_tokens` and the
  `X-KTMS-Token-Count` header.

## Compatibility with ktms-admin and light-tokenmeter

| KTMS | ktms-admin | light-tokenmeter | Notes |
| --- | --- | --- | --- |
| 1.x | 1.x | 1.x | With light-tokenmeter's authentication on, set `USAGE_COLLECTOR_TOKEN` to a `write` token. |
| 1.x | 0.4.x | 0.1.x | Works without authentication; no compatibility promise before 1.0. |

`ktms.v1` and `usage.v1` stay compatible within 1.x, so any 1.x release of each works with any
1.x release of the others.

## Upgrading

- **Within 1.x, upgrade in one step** from any earlier 1.x release: migrations run forward at
  startup and cover every release in between. `scripts/upgrade-test.sh` upgrades from the
  previous release and from the oldest release of the same major on every pull request and
  before each release.
- **Rolling upgrades work within 1.x:** migrations only add, so replicas of the previous release
  keep serving on the migrated database while new ones start; the upgrade test runs the old
  release again after the new one migrated. A change that cannot be additive waits for 2.0.
  Until the roll finishes, old replicas do not apply what the new release added (for example a
  new kind of limit), so finish rolls promptly.
- **Downgrades are not supported.** Migrations have no down step; to go back, restore the
  database backup taken before the upgrade.
- **From 0.x, go through 0.14.x to 1.0,** then to later 1.x releases: 0.x made no
  compatibility promise, and only the 0.14 → 1.0 step is tested (by the 1.0 release).
- **Read the [CHANGELOG](../CHANGELOG.md)** before upgrading: it lists every change in Added,
  Changed, Deprecated, Removed, Fixed and Security.

## Deprecated

Deprecated names keep working through 1.x, log a warning (or count in
`ktms_legacy_headers_requests_total`), and are removed in 2.0. One exception is announced in
advance:

| Deprecated | Use instead | Removed |
| --- | --- | --- |
| `LLMPROXY_ENCRYPTION_KEYS` | `KTMS_ENCRYPTION_KEYS` | 2.0 |
| `X-LLM-Proxy-*` request and response headers | `X-KTMS-*` | On their `Sunset` date, 2027-03-01, as already announced in the responses |

- **Effort (until 2.0):** the `Effort` enum, `GenerateTextRequest.effort` and `effort_specified`,
  `SubmitBatchRequest.effort`, `ModelSpec.efforts`, `ModelInfo.efforts`, `RuleSpec.effort`,
  `ExplainRoute`'s `effort` fields, the HTTP models `auto:low`, `auto:medium`, `auto:high`, and
  `reasoning_effort` meaning the general need for the model `auto`. They keep working through 1.x
  as the topic `general` (low, medium, high = basic, proficient, expert); a model described by
  efforts keeps serving exactly those. Write `competencies` and `needs` instead; no warning is logged,
  as effort is still how most 1.x clients route.
