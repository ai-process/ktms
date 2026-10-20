# Contributing to KTMS

Thank you for your interest in contributing to `KTMS`! `KTMS (Keystone: The Model Switchboard)` is a high-performance, self-hosted LLM gateway providing unified routing, fail-fast fallbacks, rate and spend limits, and centralized credential encryption for production AI workflows.

We welcome bug reports, documentation enhancements, feature proposals, and code contributions.

---

## 1. Getting Started

### Prerequisites

- **Go:** `1.26+` installed locally.
- **Docker & Docker Compose:** for local PostgreSQL and Redis dependencies.
- **Buf CLI:** for building and linting Protobuf contracts (`brew install bufbuild/buf/buf` or via official installer).

### Local Development Setup

1. **Clone the repository:**
   ```bash
   git clone https://github.com/ai-process/ktms.git
   cd ktms
   ```

2. **Start backing dependencies (PostgreSQL & Redis):**
   ```bash
   # Starts PostgreSQL on localhost:5435 and Redis on localhost:6380
   docker compose up -d db redis
   ```

3. **Run database migrations and verify connectivity:**
   ```bash
   export DATABASE_URL="postgres://ktms:ktms@localhost:5435/ktms?sslmode=disable"
   export KTMS_ENCRYPTION_KEYS="1:LS1kZXYtb25seS1rZXktLWRldi1vbmx5LWtleS0tISE="
   export REDIS_ADDR="localhost:6380"
   AUTO_SEED=true go run cmd/server/main.go
   ```

4. **Run tests with the race detector:**
   ```bash
   # Provide DATABASE_URL so postgres-backed integration tests execute rather than skip
   DATABASE_URL="postgres://ktms:ktms@localhost:5435/ktms?sslmode=disable" go test ./... -race
   ```

5. **Run static analysis:**
   ```bash
   go vet ./...
   buf lint
   ```

---

## 2. Core Architectural Invariants & Coding Rules

Every pull request is expected to adhere to our core engineering invariants:

- **Comment brevity:** Keep comments to 3 lines maximum — normally one line, ideally inline (`// ...` on the code line). Comments explain *why*, never *what*. Delete redundant comments instead of restating the code.
- **Zero secret leakage:** Never log, echo, or write a vendor LLM key, client API key, or the encryption keyring to stdout, Sentry, an error message, OpenTelemetry attributes, or a gRPC response. Vendor keys exist in plaintext only inside the in-memory registry snapshot.
- **Key lookup by ID:** Look up by key ID, not key name. Vendor keys, adapters and anything stored per client are looked up by `APIKeyID`. `KeyName` is only a label, used for budgets, logs and usage. Tests must give the ID and the name different values so a mix-up fails the test.
- **Guarded SQL state transitions:** Guard state changes in SQL. Every `UPDATE` that changes a row's state includes the states it may move from (`AND state IN (…)`) and checks the number of rows affected. A row in a final state never changes.
- **Commit before side effects:** Usage events, budget charges and other outside effects happen only after the DB transaction commits, and are written so that repeating them doesn't charge twice.
- **Vendor cleanup on DB failure:** Clean up vendor work when the DB write fails. When a vendor job is created before the DB row, a failed or losing insert must cancel that vendor job, or a pending row is written first and then filled in.
- **No silent error dropping:** Don't throw away errors when writing data. `_ =` and `_, _ =` are not allowed on DB writes or vendor calls. At minimum, log the error with the batch or request ID.
- **Config parity:** Config names match the docs. Every env var is spelled the same in `internal/config/config.go`, `env.example` and the README. The `TestEnvVarDocumentationConsistency` test verifies this.
- **Proto field hygiene:** Every response field is filled or marked `reserved`. Don't add a proto field the server never sets.
- **Standardized error contracts:** One error format. Per-item errors always include `code`, `reason` and `message`, built by one shared helper with codes from `google.golang.org/grpc/codes`.
- **Single identity helper:** One helper for user identity. Budget checks, budget charges and usage events all get the user ID from the same helper (including the `svc:<name>` fallback).
- **Vendor state mapping:** Map vendor states carefully. In-between vendor states such as `CANCELLING` or `UPDATING` map to an unfinished state, never to a final one.
- **Postgres transactions:** After any error inside a transaction, the transaction is aborted. Don't run more queries on the same `tx`; use `ON CONFLICT`, a savepoint, or a new query after rollback. Test idempotency paths against a real database, not mocks.
- **Vendor cancellation semantics:** Cancelling vendor jobs is a request, not a result: a local row moves to `CANCELLED` only after the vendor confirms it and its results have been fetched.
- **Prune dead code:** When changing architecture or state transitions, remove unused methods from store implementations, worker interfaces, RPC handler interfaces, test fakes, and tests.
- **Structured vendor errors:** Inspect SDK error types (e.g. `errors.As(err, &genai.APIError{})` with status codes) for idempotency or terminal checks instead of brittle string matching on error text.
- **Lockstep documentation:** Keep documentation in lockstep with behavioral changes. Any change in runtime behavior (e.g. identity fallback affecting quotas) must update proto comments, generated code, README tables, and PR descriptions.
- **Concise communication:** Keep communication, explanations, and PR descriptions concise and direct (no walls of text).
- **No AI attribution:** Never add `Co-Authored-By` trailers or AI attribution lines to git commits or PR descriptions, and never name branches after a model or tool.
- **Generated code sync:** `gen/` is committed; regenerate with `make proto` after editing `proto/`.
- **Pre-commit verification:** Always run `go vet ./...` and `go test ./... -race` before committing.

---

## 3. Submitting Issues & Pull Requests

### Reporting Issues

- **Bug Reports:** Use the [Bug Report Template](https://github.com/ai-process/ktms/issues/new?template=bug_report.yml). Please include reproduction steps, environment, and sanitized logs.
- **Feature Requests:** Use the [Feature Request Template](https://github.com/ai-process/ktms/issues/new?template=feature_request.yml).
- **New Vendor Adapters:** Use the [Model Provider Template](https://github.com/ai-process/ktms/issues/new?template=model_provider.yml).
- **Security Vulnerabilities:** Follow our [Security Policy](SECURITY.md). Do not file public issues for vulnerabilities.

### Pull Request Workflow

1. Fork or branch from `main`:
   ```bash
   git checkout -b feat/your-feature-name
   # or
   git checkout -b fix/your-bugfix-name
   ```
2. Keep commits atomic and focused. Use Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, `perf:`).
3. Verify all checks pass locally:
   ```bash
   go vet ./...
   go test ./... -race
   buf lint
   ```
4. Submit your pull request using the provided [Pull Request Template](.github/PULL_REQUEST_TEMPLATE.md).

---

## 4. Community Standards

All contributors and participants in the `KTMS` project are expected to uphold the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md). Please report any violations to [security@vasiliy.pro](mailto:security@vasiliy.pro).
