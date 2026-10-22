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
  `llm-proxy`). Set it explicitly to keep usage under the old project name.
- Logs carry `service=ktms` instead of `service=llm-proxy`.
- The `ffmpeg_missing` message names the `coyl/ktms:audio` image.
- CI checks proto compatibility against the latest release tag instead of `main`.

### Deprecated
- `LLMPROXY_ENCRYPTION_KEYS`: use `KTMS_ENCRYPTION_KEYS`. It is removed in 2.0.
