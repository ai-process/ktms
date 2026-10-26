# Changelog

All notable changes to KTMS are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org); see [API stability](README.md#api-stability) for what
1.x keeps compatible.

## [Unreleased]

## [1.3.6] - 2026-10-27

### Added
- The `/ktms:assess` skill: it surveys the system the assistant runs in (services and their LLM
  calls, deployment, data stores, secrets, observability, GPUs) and explains, without changing
  anything, where Keystone goes, what each service changes, what it brings and costs there, and
  which skill to run next.

## [1.3.5] - 2026-10-26

### Security
- cloudflared 2026.10.3 or newer for the node's built-in tunnel: older releases trust the eMudhra
  intermediates whose keys leaked in October 2026. `install-node.sh` replaces an older one on a
  re-run, without `--upgrade-deps`, and restarts the node onto it; `ktms-node doctor` reports it
  as a problem and the agent warns about it at start.

### Fixed
- `install-node.sh --upgrade-deps` restarts the node after updating cloudflared. The running tunnel
  kept the old binary until the next restart.

## [1.3.4] - 2026-10-26

### Fixed
- The default rules of the auto-seed and `ktms init` try the model made for the effort first: on
  high, `gemini-2.5-pro` and `claude-sonnet-5-5` before `gpt-4o`, and `gpt-4o` before
  `gpt-4o-mini`; on medium, `gpt-4o` before the low models. They tried the models in list order,
  so high went to `gpt-4o-mini` first. A Keystone seeded already keeps its rules.

## [1.3.3] - 2026-10-26

### Added
- `ktms init [CONTEXT]` sets up a Keystone step by step: it asks for the address and the admin
  key, saves a context with the key in the keychain or a 0600 file, then on a Keystone without
  models asks for vendor keys and a client key name, writes `ktms.yaml` with the default models
  and rules of those vendors, shows the plan and applies it. The new client key is kept in the
  context for `ktms chat`; vendor keys go to Keystone only. On a Keystone with models it writes
  the live configuration, without vendor keys, to the file instead.

## [1.3.2] - 2026-10-24

### Fixed
- The release publishes the images, the chart and the Homebrew formulas again: GoReleaser takes a
  tap token only as exactly `{{ .Env.HOMEBREW_TAP_TOKEN }}`. 1.3.0 and 1.3.1 have binaries on
  GitHub but no images, chart or formulas; use 1.3.2.

## [1.3.1] - 2026-10-24

### Fixed
- Not fixed in this release: with `HOMEBREW_TAP_TOKEN` set, the Homebrew formula step stopped the
  1.3.0 release after its GitHub release, before the images, the chart and the formulas. 1.3.1
  rewrote the tap token template but still not as GoReleaser requires, and stopped the same way;
  1.3.2 fixes it.

## [1.3.0] - 2026-10-24

A usage log for local nodes: the gateway records each request it sends to a node, the node keeps
the log, and its owner reads it with `ktms-node stats`. The log needs Redis 6.2 or newer; with an
older Redis set `NODE_USAGE_RETENTION_HOURS=0`.

### Added
- Node usage log: every request the proxy sends to a local node is logged per node in Redis
  (time, model, kind, tokens, outcome, level and cooldown; no user, key or text) for
  `NODE_USAGE_RETENTION_HOURS` (default 168, `0` turns it off). The node pulls it with
  `NodeRegistryService.PullNodeUsage` by cursor; a cursor older than the log gets `gap_until`.
  Needs Redis 6.2 or newer. Revoking a node key deletes the node's log.
- The node agent keeps that log locally for the machine's owner (`--stats-dir`, default
  `~/.ktms-node/stats`): requests pulled every minute, plus its own minute samples (level, state,
  load, GPU), as JSON Lines per day, closed days gzipped, kept `--stats-retention` (default 90d).
  `--stats=false` turns it off; `doctor` checks it.
- `ktms-node stats` shows that log offline: requests, errors, tokens, busy share, cooldown, time
  paused, minutes without data and TTFT/TPS by hour, day or model (`--since`, `--until`, `--by`,
  `--format table|json|csv`), or new requests as they arrive (`--tail`). The installer passes
  `--stats-dir` and prints the command.

## [1.2.0] - 2026-10-24

Tools for people and AI assistants: the `ktms` CLI, `ktms mcp`, a dry run of routing, the
`admin:read` scope and a Claude Code plugin. Works with light-tokenmeter 1.1 for usage reports.

### Added
- The `ktms` CLI beyond `config`: contexts in `~/.config/ktms/config.yaml` with keys from an
  environment variable, a 0600 file or the system keychain; `models`, `rules`, `keys`,
  `vendor-keys`, `orgs`, `teams`, `spend`, `limits`, `nodes`, `node-keys`, `rotate-encryption`;
  `chat` for a test call showing the rule, model and vendor; `health`; `-o json|yaml`; exit codes
  by error class; completion for bash, zsh and fish. Released as archives, the `coyl/ktms-cli`
  image and a Homebrew formula (published once the tap token is set).
  A context named with `--context` or `KTMS_CONTEXT` wins over `KTMS_ADDR`, `KTMS_ADMIN_KEY` and
  the other variables.
- `ktms mcp`: an MCP server for AI assistants over stdio or streamable HTTP (`--listen`, bearer
  token): read tools by default; with `--allow-write`, `config_plan` and `config_apply` (a shown
  plan applies once and only if nothing changed since), key, limit and node tools; the live
  configuration and the file schema as resources; prompts for routing, integration, local nodes
  and a production review.
- `AdminService/ExplainRoute`: a dry run of routing for a client key on the same code as
  `GenerateText` (the router's chain filter is now built on it): the matched rule, every model of
  the chain kept or dropped with the reason, what a call would try, canary weights and the shadow
  model, or the error a call would get. No vendor or judge is called and nothing is spent; it
  takes `admin:read`. `ktms route explain` and the MCP tool `explain_route` use it.
- A Claude Code plugin in `plugin/` (marketplace `ai-process` in this repository): skills to
  integrate a service, configure routing, deploy, add a local node, migrate, report on usage and
  review for production, and the `ktms` and `tokenmeter` MCP servers. CI checks that every
  command, flag, MCP tool and README section a skill names exists.
- `/healthz` reports the server's `version`.
- Configuration as a file: `ktms config validate|plan|apply|export|schema` keeps models, rules,
  organizations and teams, client keys and their policies, vendor keys (from environment
  variables), spend limits and user rate limits in YAML, plans the changes against the live
  configuration and applies them in order; `--prune` deletes what the file dropped. JSON Schema
  in `schema/ktms-config.schema.json`. Experimental (`v1alpha1`).
- `VendorKeyInfo.fingerprint` in `GetConfig`: the first 16 hex characters of the vendor key's
  SHA-256, so a client can tell whether the stored key equals one it holds.
- `admin:read` scope: a key with it may call the Admin API reads (`GetConfig`, `GetSpend`,
  `GetTeam`, `GetTeamSpend`, every `List*`) and is refused on everything that changes state, for
  dashboards, CLI and MCP clients that only look.

## [1.1.0] - 2026-10-23

### Added
- Node capacity levels: `HeartbeatRequest.capacity_percent` (0-100, unset = 100) limits a local
  node to a share of its time. Slots shrink to `ceil(max_concurrent × level)`, and where that
  overshoots the level a slot stays held for a cooldown after each request. At 0% the node is
  `NODE_STATE_PAUSED` and gets no requests. `ListNodes` reports `capacity_percent` and
  `effective_slots`; `active_requests` counts running requests only.
- Node agent load schedule: `--schedule-file` sets levels by local time (`[days] HH:MM-HH:MM level`),
  re-read on change; the level goes out in every heartbeat, and a change at once. At 0% the gateway
  refuses requests and, once the proxy has none left on the node, the agent unloads the served
  Ollama models, loading them again `--warmup` (default 5m) before the next window.
  `ktms-node schedule --file F` checks a file; `doctor` reports the current level.
- `install-node.sh --schedule FILE` validates the schedule with the agent and installs it as
  `~/.ktms-node/schedule` (a re-run without it keeps the file); the status shows the level now and
  the next switch, and the model is not preloaded during a pause.

### Fixed
- The release signs the Helm chart again: the 1.0.0 job pushed the chart, then stopped before
  signing it, because `helm pull` needs its destination directory to exist. Chart 1.0.0 stays
  unsigned; the images and binaries of 1.0.0 are signed.
- The release's `usage.v1` compatibility check no longer counts the Go package of the copy, which
  `buf.gen.usage.yaml` sets, as a break (the pull-request check already did not).

## [1.0.0] - 2026-10-23

The first stable release of Keystone. The Model Switchboard. (KTMS). From here on, the public API
listed in [API stability](README.md#api-stability) changes incompatibly only in a new major
version. Upgrade from 0.14.x in one step; from older 0.x releases, upgrade to 0.14.x first.

### Added
- `USAGE_COLLECTOR_TOKEN` (Helm: `secrets.usageCollectorToken`): a bearer token sent to the
  usage sink on every call, for light-tokenmeter 1.0 with authentication on. A refused token is
  logged as an error; events are held and retried.
- CI fails when `usage-proto/` or the stubs generated from it drift from light-tokenmeter, which
  owns `usage.v1`.
- The Helm chart is published with every release at `oci://ghcr.io/ai-process/charts/ktms`,
  signed with cosign; its version and `appVersion` are the release version. CI installs it from
  an OCI registry on a kind cluster.
- Prompt-cache pricing: `ModelSpec.price_cache_read_per_mtok` and `price_cache_write_per_mtok`
  (0 = a vendor default that never undercharges, e.g. Anthropic's 0.1x read and 1.25x write).
  Spend reservations book input at the dearer of the input and cache-write rate. Cached input from Anthropic,
  OpenAI, DeepSeek, Gemini and Bedrock is priced at these rates in spend limits and usage costs,
  sent to light-tokenmeter as `cache_read_tokens` / `cache_write_tokens`, returned in
  `TokensUsage.cache_read` / `cache_write` (HTTP: `prompt_tokens_details.cached_tokens`), and
  counted in `ktms_cache_tokens_total`.
- Key policies: `APIKeyPolicy` (`allowed_models`, `rpm_limit`, `tpm_limit`) on `MintAPIKey`,
  `SetAPIKeyPolicy` and `ListAPIKeys`. The allowlist narrows chains on top of the team's; the rates
  cover all models (tokens by GCRA, booked by estimate and then by fact) and refuse with
  `throttled:key_rpm` / `throttled:key_tpm` and `RetryInfo`.
- `prompt_cache` model capability: Anthropic calls put `cache_control` breakpoints after the
  tools, the system prompt and a conversation's latest turn, so repeated prefixes are read from
  the cache at 0.1x input.
- `GetSpend`: a key's, user's or team's current spend, cap and remaining amount per UTC day and
  month, from the counters that enforce the caps.

- `install-node.sh` takes over a node installed before the rename: it reuses the old node key
  (for the same node id) and Ollama build, then, right before its own Ollama starts, stops the
  `llm-proxy-*` services for good and moves their files and `~/.llm-proxy-node` to
  `~/.ktms-node.backup/<time>/`. `--dry-run` shows the plan, `--keep-legacy` keeps the old
  directory, `--uninstall` retires old services too, `--purge-backups` deletes the backups.

### Changed
- Publishing `ktms.v1` to the Buf Schema Registry fails the `bsr-push` job when `BUF_TOKEN` is
  missing, instead of being skipped without a word.
- Allowlist refusals name the list that refused: `not allowed for <key|team>`, `no models in chain
  allowed for <key|team>`, and for batches `model access denied by key or team policy`.
- Relicensed under Apache 2.0.
- The README names the product Keystone. The Model Switchboard. (KTMS).
- The README no longer offers SDK installs from the Buf Schema Registry, which is not published
  yet; generate a client from `proto/ktms/v1` instead.
- Usage events keep their metadata within light-tokenmeter's 16 KiB limit: entries that do not
  fit are left out, largest first, with a warning, instead of the event being refused.
- `ktms_requests_total` labels a call whose caller went away `status="canceled"` instead of
  `error`, so client aborts stay out of vendor error rates.
- `ktms_throttled_total` and `ktms_usage_events_dropped_total` export every reason at 0 from
  startup, so `increase()` alerts also catch the first event after a restart.
- The README's Operations section lists every metric with suggested alerts and sizing, and the
  production checklist covers ports, network policies, key scopes and secrets.

### Fixed
- `install-node.sh` no longer pulls the model into another process's Ollama that happens to listen
  on the node's address; it stops and names that process.
- Prompt-cache tokens were mispriced, in spend limits and usage costs alike: Anthropic cache
  reads were billed 10x too high and cache writes 20% too low, and Bedrock left cached input out
  of the bill altogether.
- `install-node.sh` no longer warns that a `ktms_node_` key does not look like a node key.
- CONTRIBUTING and `compose.yaml` use the KTMS database and `KTMS_ENCRYPTION_KEYS` names.

## [0.14.0] - 2026-10-22

### Added
- Organizations and teams: team keys share a model allowlist and a monthly budget enforced
  by the gateway (`CreateOrganization`, `CreateTeam`, `UpdateTeam`, `GetTeamSpend`, ...).
  `ListModels` lists only the models a team may call.
- Operations: backups and the keyring, and a safe key rotation procedure, tested against
  Postgres; CI now runs the Postgres-backed tests (they used to skip).
- An upgrade test on every pull request and before each release: the previous release and the
  oldest release of the same major write a database, this build must keep and serve it, and the
  old release must still serve on the migrated database (rolling upgrades).
- Releases are gated: binaries and images are published only after the proto and upgrade checks
  against the previous release pass, and images only after the binaries.
- Signed releases: images and `checksums.txt` are signed with keyless cosign; images carry an
  SBOM and SLSA provenance, archives an SPDX SBOM. See SECURITY.md, Verifying a Release.
- `govulncheck` for linux and darwin on every pull request and before every release.
- The public API for 1.x is defined in the README ([API stability](README.md#api-stability)),
  with experimental elements marked `Experimental:` in the proto.
- A startup warning when the deprecated `LLMPROXY_ENCRYPTION_KEYS` is used instead of
  `KTMS_ENCRYPTION_KEYS`.

### Changed
- `USAGE_PROJECT_ID` now defaults to `ktms`, as the README already documented (it was
  `llm-proxy`). Set it explicitly to keep usage under the old project name; while it is unset
  and usage reporting is on, the gateway logs this at startup.
- Logs carry `service=ktms` instead of `service=llm-proxy`.
- The `ffmpeg_missing` message names the `coyl/ktms:audio` image.
- CI also checks `usage.v1` for breaking changes. Adding the `buf skip breaking` label re-runs the check.
- A cache hit refused by the rpm limit returns the documented `throttled:rpm` reason instead of
  `resource_exhausted: rpm limit exceeded`.
- `SENTRY_ENVIRONMENT` defaults to `development`, as documented; it was sent empty.
- Postgres outages return the reasons `auth_unavailable` and `storage_unavailable` (were
  `auth backend unavailable` and `storage unavailable`), so the HTTP API puts them in `code`.
- `DrainNode` without node support returns `FAILED_PRECONDITION` instead of `UNAVAILABLE`.
- Docker images are built only for `v*` release tags (any tag used to produce images), and
  `latest`, `slim` and `audio` follow only the newest stable release.

### Fixed
- `RotateEncryption` also re-encrypts the auth tokens of registered nodes, so dropping the old
  key version no longer breaks calls to nodes, and it locks the vendor key rows, so a key set
  during a rotation is no longer overwritten with the previous one.
- A vendor key the replica's keyring cannot read no longer stops that replica from loading
  config: it keeps the credential it had (or skips the key), logs and counts it. A keyring that
  reads no vendor key at all still refuses to start. `RotateEncryption` over such a key fails
  with `FAILED_PRECONDITION` instead of `UNAVAILABLE storage_unavailable`.
- With `AUTO_SEED`, starting a replica no longer rewrites unchanged seeded vendor keys, or keys
  a newer keyring already rotated, which broke rotations.

### Deprecated
- `LLMPROXY_ENCRYPTION_KEYS`: use `KTMS_ENCRYPTION_KEYS`. It is removed in 2.0.
- `X-LLM-Proxy-*` headers: use `X-KTMS-*`. They are removed on their announced `Sunset` date,
  2027-03-01, the one removal inside 1.x.

## [0.13.1] - 2026-10-21

### Fixed
- Postgres outage: keys that verified within `AUTH_STALE_IF_ERROR` keep working.
- Redis is bounded by `REDIS_TIMEOUT` and a fail-fast guard; usage events queue while the
  collector is down; node admission follows the engine's real queue.
- Vendor `Retry-After` is honoured, and the vendor SDKs' own retries are off.
- JSON Schema keeps `enum`, number limits, `format` and `additionalProperties`.
- Vendor connections are reused under load.

### Security
- Go 1.27.3, Node 24.22.1 and Alpine 3.23 with patched `ca-certificates`.

## [0.13.0] - 2026-10-18

### Added
- Canary weights and shadow traffic with judge evaluation; a vendor circuit breaker and a
  per-attempt upstream timeout.
- Helm chart, slim and audio images, zero-config quickstart with auto-seed.

### Changed
- The project is renamed from llm-proxy to KTMS: gRPC package `ktms.v1`, `KTMS_*` settings and
  `X-KTMS-*` headers. The old names keep working where listed under Deprecated in the README.

## [0.12.1] - 2026-10-22

Backport on the 0.12 line.

### Security
- Go 1.27.3, Alpine 3.23 and `ca-certificates` >= 20261020-r0.

## [0.12.0] - 2026-10-16

### Added
- Latent semantic cache and latent guardrails; `GetEmbeddings`; multimodal judge state;
  `EvaluateRollouts` and `JudgeStream`; USD spend limits per key and user; judge models on nodes.

## [0.11.1] - 2026-10-15

### Added
- Buf Schema Registry module and client generation templates.

## [0.11.0] - 2026-10-15

### Added
- Smart auto routing and node placement policies.

## [0.10.x] - 2026-10-14 to 2026-10-15

### Added
- Node agent with a built-in gateway and tunnel (`--expose`), `doctor`, `install-node.sh` and
  GoReleaser releases (0.10.0); guardrails, Azure, Bedrock, Groq and Mistral, Prometheus and
  OpenTelemetry, exact-match cache, weighted routing, multimodal input (0.10.2).

### Fixed
- Node registration drops disallowed models (0.10.1); local models need no vendor key (0.10.3).

Releases before 0.10.0 are described on the
[GitHub releases page](https://github.com/ai-process/ktms/releases).

[Unreleased]: https://github.com/ai-process/ktms/compare/v1.3.6...HEAD
[1.3.6]: https://github.com/ai-process/ktms/compare/v1.3.5...v1.3.6
[1.3.5]: https://github.com/ai-process/ktms/compare/v1.3.4...v1.3.5
[1.3.4]: https://github.com/ai-process/ktms/compare/v1.3.3...v1.3.4
[1.3.3]: https://github.com/ai-process/ktms/compare/v1.3.2...v1.3.3
[1.3.2]: https://github.com/ai-process/ktms/compare/v1.3.1...v1.3.2
[1.3.1]: https://github.com/ai-process/ktms/compare/v1.3.0...v1.3.1
[1.3.0]: https://github.com/ai-process/ktms/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/ai-process/ktms/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/ai-process/ktms/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/ai-process/ktms/compare/v0.14.0...v1.0.0
[0.14.0]: https://github.com/ai-process/ktms/compare/v0.13.1...v0.14.0
[0.13.1]: https://github.com/ai-process/ktms/compare/v0.13.0...v0.13.1
[0.13.0]: https://github.com/ai-process/ktms/compare/v0.12.0...v0.13.0
[0.12.1]: https://github.com/ai-process/ktms/compare/v0.12.0...v0.12.1
[0.12.0]: https://github.com/ai-process/ktms/compare/v0.11.1...v0.12.0
[0.11.1]: https://github.com/ai-process/ktms/compare/v0.11.0...v0.11.1
[0.11.0]: https://github.com/ai-process/ktms/compare/v0.10.3...v0.11.0
[0.10.x]: https://github.com/ai-process/ktms/compare/0.9.0...v0.10.3
