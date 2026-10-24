# Releasing Keystone. The Model Switchboard. (KTMS)

For maintainers. Versions follow [API stability](README.md#api-stability): a fix is a patch, a
feature is a minor, a break of the public API is a major.

## Steps

1. In a pull request (`chore(release): X.Y.Z`):
   - move the `[Unreleased]` entries of [`CHANGELOG.md`](CHANGELOG.md) under
     `## [X.Y.Z] - YYYY-MM-DD` and add its compare link at the bottom;
   - set `version` and `appVersion` in [`deploy/helm/ktms/Chart.yaml`](deploy/helm/ktms/Chart.yaml)
     and `version` in [`plugin/.claude-plugin/plugin.json`](plugin/.claude-plugin/plugin.json) to
     `X.Y.Z`; the release stops when either differs from the tag;
   - update the pinned versions in examples: `helm install … --version` in the README and
     [`deploy/helm/ktms/README.md`](deploy/helm/ktms/README.md), `V=` in
     [`SECURITY.md`](SECURITY.md#verifying-a-release).
2. After it is merged, tag main and push the tag:

   ```bash
   git tag vX.Y.Z && git push origin vX.Y.Z
   ```

The `v` matters: it makes the tag a Go module version, so
`go install github.com/ai-process/ktms/cmd/ktms@vX.Y.Z` works.

## What the workflow does

[`release.yml`](.github/workflows/release.yml), in order:

1. **Compatibility** ([`compat.yml`](.github/workflows/compat.yml)): `govulncheck` for linux and
   darwin, `buf breaking` against the previous published release in the tag's history, and the
   upgrade test from it and from the oldest release of the same major. If it fails, nothing is
   published.
2. **Binaries** (GoReleaser, [`.goreleaser.yaml`](.goreleaser.yaml)): `ktms`, `ktms-server` and
   `ktms-node` archives for darwin and linux, amd64 and arm64, a cosign-signed `checksums.txt` and
   an SBOM per archive, on a GitHub Release with the commit list as notes.
3. **Images** ([`docker-publish.yml`](.github/workflows/docker-publish.yml)): signed multi-arch
   `coyl/ktms` (`:<tag>`, `-slim` and `-audio` variants), `coyl/ktms-node` and `coyl/ktms-cli`.
   `X.Y` moves only for the newest release of its line, and `latest`, `slim` and `audio` only
   for the newest release of all, so a backport moves neither.
4. **Helm chart**: `oci://ghcr.io/ai-process/charts/ktms` with the tag as `version` and
   `appVersion`, signed with cosign.

An image on Docker Hub marks a complete release: that is how the next release finds its
predecessor, so re-run a failed image job before tagging again. A failed chart job can be re-run
at any time and keeps a version already pushed.

A release that breaks the proto on purpose (an Experimental change, or a new major) sets the
repository variable `ALLOW_BREAKING_TAG` to its tag. That skips the proto checks; the upgrade
test still runs.

## Outside the tag

The Buf Schema Registry is fed from `main`, not from tags: each push that passes lint and the
breaking check publishes `ktms.v1` to `buf.build/ai-process/ktms`, which generates the SDKs
([`buf.yml`](.github/workflows/buf.yml); it can also be run by hand on `main`).

## Secrets

| Secret | For |
| --- | --- |
| `DOCKER_USERNAME`, `DOCKER_PASSWORD` (an access token) | Pushing the images |
| `BUF_TOKEN` (a buf.build token of the `ai-process` organization) | Publishing to the Buf Schema Registry; without it `bsr-push` fails, so an unpublished SDK does not go unnoticed |
| `HOMEBREW_TAP_TOKEN` (optional) | The `ktms` and `ktms-node` formulas in `ai-process/homebrew-tap`; the repository must exist. Without it the formulas are skipped |
| `MACOS_SIGN_P12`, `MACOS_SIGN_PASSWORD`, `MACOS_NOTARY_ISSUER_ID`, `MACOS_NOTARY_KEY_ID`, `MACOS_NOTARY_KEY` (optional) | Developer ID signing and notarization of the darwin binaries. Without them they are unsigned; binaries fetched with `curl` or installed by `brew` carry no quarantine flag and run, and only browser downloads trigger Gatekeeper |

The chart and the cosign signatures need no secret: they use the workflow's `GITHUB_TOKEN` and
OIDC identity, which [Verifying a release](SECURITY.md#verifying-a-release) pins. After the first
release, the `charts/ktms` package has to be made public once in the organization's package
settings.

## Locally

```bash
./scripts/upgrade-test.sh   # the upgrade test (needs Docker)
scripts/helm-kind.sh        # the chart installed from an OCI registry on kind
go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean --skip=publish,sign,sbom,notarize
```
