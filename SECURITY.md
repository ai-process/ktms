# Security Policy

The security of `KTMS` and the integrity of upstream provider credentials stored within it are paramount. We appreciate the responsible disclosure of any potential vulnerabilities.

## Supported Versions

We provide security updates and patches for the following versions:

| Version | Supported          |
| ------- | ------------------ |
| `1.x`, latest minor release | :white_check_mark: |
| `0.13.x` | :white_check_mark: until 1.0.0 is released |
| `< 0.13` | :x:                |

## Verifying a Release

From 1.0.0 on, release artifacts are signed with [Sigstore cosign](https://docs.sigstore.dev/)
using GitHub Actions' OIDC identity (keyless: there is no key to leak), and carry an SBOM.

Docker images (signature, SBOM and SLSA provenance are attached to the image):

```bash
cosign verify coyl/ktms:1.0.0 \
  --certificate-identity-regexp '^https://github\.com/ai-process/ktms/\.github/workflows/docker-publish\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
docker buildx imagetools inspect coyl/ktms:1.0.0 --format '{{ json .SBOM }}'
docker buildx imagetools inspect coyl/ktms:1.0.0 --format '{{ json .Provenance }}'
```

Binaries on the GitHub release (`checksums.txt` is signed; each archive has an `.sbom.json`):

```bash
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/ai-process/ktms/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt
```

Every pull request also runs [`govulncheck`](https://go.dev/doc/security/vuln/): a known
vulnerability that KTMS code can reach fails the build.

## Reporting a Vulnerability

**Please do not report security vulnerabilities via public GitHub issues, discussions, or pull requests.**

Instead, please report potential security vulnerabilities through one of the following private channels:

1. **GitHub Private Vulnerability Reporting:**  
   Navigate to the [Security Advisories tab](https://github.com/ai-process/ktms/security/advisories/new) of this repository and click **Report a vulnerability**. This allows secure, confidential coordination and tracking.

2. **Email Disclosure:**  
   Send an encrypted or confidential email to [security@vasiliy.pro](mailto:security@vasiliy.pro) with the subject prefix `[SECURITY] KTMS: <Brief Description>`.

### What to Include in Your Report

To help us triage and resolve the issue quickly, please provide:
- A clear description of the vulnerability and its potential impact.
- Detailed step-by-step reproduction instructions or a minimal proof of concept (PoC).
- The version, environment, and configuration where the vulnerability was observed.
- Any suggested mitigations or remediation steps if available.

### Response Timeline

- **Initial Acknowledgment:** Within 48 hours of receipt.
- **Triage & Assessment:** Within 5 business days with an assessment of severity and priority.
- **Remediation & Patch Release:** Coordinated security advisory and release within a reasonable timeframe (typically 14–30 days depending on complexity).
- **Public Disclosure:** Following the release of the patched version or mutually agreed disclosure date.

---

## Security Architecture & Design Guarantees

`KTMS` is designed with defense-in-depth principles:

1. **Envelope Encryption for Vendor Credentials:**  
   All upstream provider API keys (OpenAI, Anthropic, Gemini, DeepSeek, Bedrock, etc.) are encrypted at rest in PostgreSQL using AES-256-GCM with versioned keyrings (`KTMS_ENCRYPTION_KEYS` / `LLMPROXY_ENCRYPTION_KEYS`). Plaintext keys are decrypted only into volatile in-memory snapshot caches.

2. **Zero-Log Secret Guarantee:**  
   The codebase strictly enforces that vendor API keys, client API keys, and encryption master keys are never logged to stdout, file logs, OpenTelemetry traces, Prometheus metrics, Sentry, or gRPC error details.

3. **Key ID Isolation:**  
   Client credentials and vendor allocations are looked up internally by immutable UUIDs (`APIKeyID`), never by human-readable names or unhashed credentials.

4. **Non-Root & Network Isolation:**  
   Official Docker images run as an unprivileged non-root user (`appuser`, uid 10001). Quickstart Docker Compose configurations bind all service ports to `127.0.0.1` by default to prevent unintentional network exposure.
