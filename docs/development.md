# Development

```bash
make test       # go test -race ./...
make build      # binary in bin/
make proto      # regenerate gen/ after editing proto/ (needs buf)
make proto-py   # generate Python client stubs in gen/python/
make proto-ts   # generate TypeScript client stubs in gen/ts/
make clients    # regenerate the usage-sink stubs from usage-proto/
docker compose up --build   # local stack with Postgres + Redis
```

`usage-proto/usage/v1/usage.proto` is a byte-for-byte copy of light-tokenmeter's, which owns it.
CI fails when the copy or the stubs generated from it drift; copy the file over and run
`make clients`.

Layout: `cmd/server` (entrypoint), `internal/router` (rule matching, fallback, admission),
`internal/registry` (validated config snapshots), `internal/llm` (vendor-neutral chat model and
adapters), `internal/throttle` (Redis limits), `internal/grpcapi` (RPC handlers),
`internal/crypto` (keyring), `migrations/` (embedded SQL).

Pull requests are welcome; read [`CONTRIBUTING.md`](../CONTRIBUTING.md) first. Maintainers release
as described in [`RELEASING.md`](../RELEASING.md).
