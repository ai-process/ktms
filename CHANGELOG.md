# Changelog

All notable changes to KTMS are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org); see [API stability](README.md#api-stability) for what
1.x keeps compatible.

## [Unreleased]

### Added
- Organizations and teams: team keys share a model allowlist and a monthly budget enforced
  by the gateway (`CreateOrganization`, `CreateTeam`, `UpdateTeam`, `GetTeamSpend`, ...).
  `ListModels` lists only the models a team may call.
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

[Unreleased]: https://github.com/ai-process/ktms/compare/v0.13.1...HEAD
[0.13.1]: https://github.com/ai-process/ktms/compare/v0.13.0...v0.13.1
[0.13.0]: https://github.com/ai-process/ktms/compare/v0.12.0...v0.13.0
[0.12.1]: https://github.com/ai-process/ktms/compare/v0.12.0...v0.12.1
[0.12.0]: https://github.com/ai-process/ktms/compare/v0.11.1...v0.12.0
[0.11.1]: https://github.com/ai-process/ktms/compare/v0.11.0...v0.11.1
[0.11.0]: https://github.com/ai-process/ktms/compare/v0.10.3...v0.11.0
[0.10.x]: https://github.com/ai-process/ktms/compare/0.9.0...v0.10.3
