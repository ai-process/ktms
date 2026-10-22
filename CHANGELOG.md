# Changelog

All notable changes to KTMS are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org); see [API stability](README.md#api-stability) for what
1.x keeps compatible.

## [Unreleased]

### Added
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

### Deprecated
- `LLMPROXY_ENCRYPTION_KEYS`: use `KTMS_ENCRYPTION_KEYS`. It is removed in 2.0.
- `X-LLM-Proxy-*` headers: use `X-KTMS-*`. They are removed on their announced `Sunset` date,
  2027-03-01, the one removal inside 1.x.
