# Changelog

All notable changes to KTMS are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org); see [API stability](README.md#api-stability) for what
1.x keeps compatible.

## [Unreleased]

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

### Changed
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

[Unreleased]: https://github.com/ai-process/ktms/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/ai-process/ktms/compare/v0.14.0...v1.0.0
[0.14.0]: https://github.com/ai-process/ktms/compare/v0.13.1...v0.14.0
[0.13.1]: https://github.com/ai-process/ktms/compare/v0.13.0...v0.13.1
[0.13.0]: https://github.com/ai-process/ktms/compare/v0.12.0...v0.13.0
[0.12.1]: https://github.com/ai-process/ktms/compare/v0.12.0...v0.12.1
[0.12.0]: https://github.com/ai-process/ktms/compare/v0.11.1...v0.12.0
[0.11.1]: https://github.com/ai-process/ktms/compare/v0.11.0...v0.11.1
[0.11.0]: https://github.com/ai-process/ktms/compare/v0.10.3...v0.11.0
[0.10.x]: https://github.com/ai-process/ktms/compare/0.9.0...v0.10.3
