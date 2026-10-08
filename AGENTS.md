# Agent Guidelines & Rules for llm-proxy

Guidelines and invariants for AI agents working in this codebase, synthesized from architecture decisions and code review findings.

## 1. Database & Transactions
- **Postgres transaction aborts:** After any error in a PostgreSQL transaction, the transaction block is aborted and unusable. Never execute more queries on an errored `tx`. Use `ON CONFLICT DO NOTHING`, savepoints, or rollback before running subsequent queries on the connection pool (`d.pool`). Test idempotency paths against real PostgreSQL, not mocks.
- **SQL state guards:** Every `UPDATE` that transitions state must include `AND state IN (...)` and check rows affected. Terminal states (`SUCCEEDED`, `FAILED`, `CANCELLED`, `EXPIRED`) must never transition again.
- **Commit before side effects:** Emit usage events, charge budgets, and trigger external effects only after the database transaction successfully commits.

## 2. Vendor Integrations & Async Workflows
- **Vendor cancellations are requests, not results:** Cancelling an external job is an asynchronous request. Keep the local row in `RUNNING` until the poller confirms vendor cancellation, preserves any finished item results, and fails remaining pending items.
- **Structured error matching:** Inspect structured SDK error types (e.g. `errors.As(err, &genai.APIError{})` with `Status == "FAILED_PRECONDITION"`) for idempotency or terminal checks instead of fragile string matching.
- **Clean up orphaned vendor work:** If an external job is created before the DB record, cancel the external job if the DB write fails or loses an insertion race.

## 3. Architecture, Refactoring & Cleanup
- **Prune dead code during refactors:** When refactoring flows or interfaces, eliminate unused methods from store implementations (`proxydb`), background worker interfaces (`poller`), RPC handler interfaces (`grpcapi`), test fakes, and unit tests.
- **Look up by Key ID, not Key Name:** Use `APIKeyID` for vendor keys, adapters, and tenant-scoped lookups. `KeyName` is purely a human-readable label for budgets, logs, and usage.
- **Single helper for user identity:** All budget checks, budget reservations, adapter invocations, and usage events must obtain user identity from `router.UserID(attrs, keyName)`.

## 4. Error Handling & Observability
- **Never swallow writes or side-effect errors:** `_ =` and `_, _ =` are forbidden on database writes, vendor calls, and usage reporting. Log non-fatal errors with full context (`batch_id`, `custom_id`).
- **Standardized error responses:** Per-item batch errors must always contain `code`, `reason`, and `message`, constructed via shared helpers using gRPC codes.

## 5. Contracts & Documentation
- **Keep contracts in lockstep:** Any behavioral change (such as fallback identities affecting quotas) must be updated across proto comments, generated code (`make proto`), README tables, and PR descriptions.
- **Config parity:** Every configuration environment variable must match across `config.go`, `env.example`, and `README.md`.

## 6. Code & Communication Hygiene
- **Code comments:** Keep comments to 3 lines maximum explaining *why*, never *what*.
- **Concise messages:** Keep PR explanations and chat responses short and direct — no walls of text.
- **No AI attribution:** Never add `Co-Authored-By` trailers or AI attribution lines to git commits or PR descriptions.
- **Account separation:** Never post PR comments using `gh` under a user's personal account credentials.
- **Verification:** Always run `go vet ./...` and `go test ./... -race` before committing.
