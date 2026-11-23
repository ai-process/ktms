# Console, CLI and AI assistants

## Web console

[**ktms-admin**](https://github.com/ai-process/ktms-admin) is a small web UI for everything in
the previous section: models, routing rules, client API keys, vendor credentials and a playground for
trying a prompt through your rules. It can also show usage and spend per project, user and model when
paired with [light-tokenmeter](https://github.com/ai-process/light-tokenmeter). Sign-in is a password,
Google or any OpenID Connect provider, GitHub, or an access proxy (oauth2-proxy, Authelia, Cloudflare
Access, …), limited to an e-mail allowlist with wildcards (`*@example.com`).

```bash
# 1. a key for the console
grpcurl -plaintext -H "authorization: Bearer $BOOTSTRAP_ADMIN_KEY" \
  -d '{"name":"console","scopes":["admin"]}' localhost:9090 ktms.v1.AdminService/MintAPIKey

# 2. a password hash for you (asks twice)
docker run --rm -it coyl/ktms-admin:latest hash-password you@example.com

# 3. run it (its README covers the other sign-in methods)
docker run -d -p 8081:8080 \
  -e PUBLIC_URL=https://admin.example.com \
  -e ADMIN_USERS='you@example.com:$argon2id$…' -e ALLOWED_EMAILS='you@example.com' \
  -e SESSION_SECRET="$(openssl rand -base64 36)" \
  -e KTMS_ADDR=ktms:9090 -e KTMS_ADMIN_KEY=ktms_… \
  coyl/ktms-admin:latest
```

## Command-line tool

`ktms` covers the everyday admin jobs without `grpcurl` and the proto: the configuration as a
file, keys, spend and limits, nodes, a test call and health checks. Keys never go on the command
line, and none is printed except a new one, once.

```bash
# a release archive (darwin/linux, amd64/arm64, signed checksums), or:
go install github.com/ai-process/ktms/cmd/ktms@latest
docker run --rm -e KTMS_ADDR -e KTMS_ADMIN_KEY coyl/ktms-cli:latest models list
```

A context names a Keystone and where its keys live, so a command needs no flags:

```bash
ktms init prod                          # asks for all of it, see Quick start; or by hand:
ktms context set prod --addr ktms.example.com:443 --tls --http https://ktms.example.com
ktms context store-key prod             # reads the admin key without echo, into the keychain
ktms context store-key prod --client    # a generate key for ktms chat
ktms context use prod
```

Contexts live in `~/.config/ktms/config.yaml` (`KTMS_CONFIG`). A key source is `env:VAR`,
`file:PATH` or `keychain` (macOS Keychain, or the secret service through `secret-tool` on Linux);
`store-key --file` keeps the key in a `0600` file instead. Flags win over `KTMS_ADDR`, `KTMS_TLS`,
`KTMS_HTTP`, `KTMS_ADMIN_KEY` and `KTMS_API_KEY`, which win over the current context; a context named
with `--context` or `KTMS_CONTEXT` wins over them, so its keys never go elsewhere.

```bash
ktms health                                   # /healthz, /readyz and the server's version
ktms models list; ktms rules list             # -o json or -o yaml for scripts
ktms keys mint billing --team acme/search --rpm 120
echo "$OPENAI_KEY" | ktms vendor-keys set billing openai
ktms keys policy set billing --allowed-models gpt-4o-mini --tpm 200000
ktms chat "Say hi"                            # the answer, then rule, model and vendor
ktms translate --to de reviews.txt            # one translation per line, see Translation
ktms limits set key billing day 5             # USD per UTC day; * sets the default
ktms spend get key billing                    # or user ID, team org/team
ktms route explain billing --effort high      # where it would go, and why
ktms nodes list; ktms nodes drain gpu-1
ktms nodes schedule set mac --file week.txt   # for nodes that hand their schedule to KTMS
ktms nodes schedule override mac --level 0 --for 2h
ktms node-keys mint gpu-1 --node-id gpu-1 --allowed-cidrs 10.0.0.0/8
ktms keys revoke billing                      # asks first; --yes for scripts
ktms rotate-encryption
ktms mcp-servers list                         # the MCP gateway's servers, see MCP gateway
ktms completion zsh > "${fpath[1]}/_ktms"     # also bash and fish
ktms mcp                                      # tools for AI assistants, see below
```

`ktms route explain KEY --effort low [--attr k=v] [--placement P] [--model M]` asks Keystone where
such a request would go: the rule, every model of its chain kept or dropped with the reason (no
vendor key, key or team policy, capability, placement, circuit open, vendor paused, no node), and
what a call would try. It runs Keystone's own routing (`AdminService/ExplainRoute`), calls no vendor
or judge and spends nothing; rate limits and budgets are not checked. With `--auto` and no effort it
shows the judge's fallback, medium. `--modality translate --from fr --to de` (or `judge`, `tts`,
`image`) explains that RPC's route instead, models without the language pair dropped as `language_pair`.

`admin:read` is enough for everything that only reads (`list`, `get`, `route explain`,
`config plan|export`).
Exit codes follow the [error contract](errors.md#error-contract): 0 done, 1 failed, 2 bad usage, 3 plan
has changes (`--detailed-exitcode`), 4 not authenticated or not allowed, 5 not found, 6 rejected as
invalid, 7 unavailable or throttled (retry later).

### MCP server for AI assistants

`ktms mcp` gives Claude Code, Claude Desktop, Cursor and other MCP clients Keystone's tools, so an
assistant reads and changes the configuration itself instead of writing `grpcurl` scripts. It uses
the same contexts and keys as the CLI; the key never reaches the assistant.

- **Read tools** (the only ones by default): `get_config` (the live configuration as a ktms config
  file), `list_models`, `list_rules`, `list_keys`, `list_vendor_keys`, `list_teams`,
  `list_spend_limits`, `get_spend`, `list_nodes`, `list_node_keys`, `explain_route`, `health`. An `admin:read` key
  is enough.
- **Write tools**, offered only with `--allow-write`: `config_plan` (a ktms config file in, the plan
  and a `plan_id` out; nothing changes) and `config_apply` (applies that plan once, in the session
  that planned it, and only if the live configuration has not changed since), `mint_key` (the key
  comes back once, marked for the user), `revoke_key`, `set_key_policy`, `set_spend_limit`,
  `delete_spend_limit`, `drain_node`. Vendor keys in a plan come from the MCP server's
  environment, never from the chat, and only from variables named `KTMS_VENDOR_KEY_*` (the names
  `ktms config export` writes), so an assistant cannot point `plain_key_env` at the admin key or
  another secret. Each tool carries `readOnlyHint` / `destructiveHint`.
- **Resources** `ktms://config` (the live configuration) and `ktms://schema` (the file's JSON
  Schema); **prompts** `configure_routing`, `integrate_service`, `local_node`, `production_review`.
- Names and values from Keystone are other people's text: the tools say so, and an assistant should
  treat them as data.

Claude Code (`.mcp.json` in a repository, or `claude mcp add ktms -- ktms mcp`), Claude Desktop
(`claude_desktop_config.json`) and Cursor (`.cursor/mcp.json`) take the same entry:

```json
{
  "mcpServers": {
    "ktms": { "command": "ktms", "args": ["mcp"], "env": { "KTMS_CONTEXT": "prod" } }
  }
}
```

Add `"--allow-write"` to `args` to let the assistant plan and apply changes. Over the network,
`ktms mcp --listen 127.0.0.1:8765` serves streamable HTTP at `/mcp`; every request needs
`Authorization: Bearer <token>` with the token from `KTMS_MCP_TOKEN` (16+ characters, e.g.
`openssl rand -hex 32`). It speaks plain HTTP: keep it on localhost or behind a TLS proxy.

```json
{
  "mcpServers": {
    "ktms": { "type": "http", "url": "http://127.0.0.1:8765/mcp",
              "headers": { "Authorization": "Bearer ${KTMS_MCP_TOKEN}" } }
  }
}
```

### Claude Code plugin and skills

[`plugin/`](../plugin) is a Claude Code plugin with both MCP servers (`ktms mcp`, `tokenmeter mcp`,
read-only) and skills for the usual tasks: seeing how Keystone fits a system, connecting a
service, configuring routing, deploying, adding a local node, migrating from LiteLLM, usage
reports and a production review.

```bash
claude plugin marketplace add ai-process/ktms && claude plugin install ktms@ai-process
```

The skills are plain `SKILL.md` folders, so other agents that read Agent Skills can use them too;
see [plugin/README.md](../plugin/README.md). CI fails when a skill names a command, flag, MCP tool or
docs section that does not exist. Assistants without skills or MCP can use the
[prompts](https://ai-process.dev/docs/prompts/), which cover the same tasks.
