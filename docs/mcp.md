# MCP gateway

Keystone is also a gateway for MCP servers. Claude Code, Cursor and other MCP clients reach the
servers you register through Keystone, with a KTMS key: the gateway holds each server's
credential, shows a key only the servers and tools it may use, limits calls per tool and
accounts for every call in light-tokenmeter and Prometheus. The MCP gateway is
[experimental](stability.md#experimental) in 1.x.

- `/mcp` aggregates every server the key may use; tools are named `server__tool`.
- `/mcp/{server}` is one server, with its tools named as the server names them.

Both are Streamable HTTP on `HTTP_ADDR`, stateless, so any replica answers any request. Set
`MCP_GATEWAY_ENABLED=false` to turn them off. Upstream servers may speak Streamable HTTP or SSE.
Only tools are proxied; resources and prompts are not.

```bash
# register a server and give it the header the gateway sends it
ktms mcp-servers set github --url https://api.githubcopilot.com/mcp/ --tool-rpm 60
echo "Bearer $GITHUB_TOKEN" | ktms mcp-servers credential set github
ktms mcp-servers tools github          # what it offers, as allowed_mcp entries

# a key for the agent: the mcp scope, two tools of github and all of docs
ktms keys mint coding-agent --scopes mcp --allowed-mcp github/create_issue,github/search_code,docs

# Claude Code
claude mcp add --transport http ktms https://ktms.example.com/mcp --header "Authorization: Bearer ktms_<key>"
```

- **Servers** are `MCPServerSpec`: `name` (1-32 of `a-z`, `0-9`, `-`), `url`, `transport`
  (`streamable_http` or `sse`), `enabled`, `allow_private`, `timeout_ms` (one call, default
  60 s), `max_response_bytes` (one result, default 4 MiB) and `tool_rpm`. Manage them with
  `UpsertMCPServer` and `DeleteMCPServer`, `ktms mcp-servers`, the `mcp_servers` section of a
  [configuration file](configuration.md#configuration-as-a-file) or the console. `GetConfig` lists them.
- **Credentials**: `SetMCPCredential` stores a header and its value (`Authorization: Bearer ...`
  by default), encrypted with the vendor keys' keyring and re-encrypted by `RotateEncryption`.
  The gateway adds it to every request it sends the server. It is never returned, logged, put in
  usage events or passed to clients; `GetConfig` shows its header and fingerprint. In a file it is
  `mcp_credentials` with `value_env`.
- **Access**: a key needs the `mcp` scope. `allowed_mcp` on the key's policy and on its team lists
  servers (`github`) and single tools (`github/create_issue`); empty allows any, and a tool must
  pass both lists. Others are not listed, and calling one is the JSON-RPC error `-32003`
  `permission_denied` (`/mcp/{server}` for a server outside the lists is HTTP 403); the upstream
  is not called.
- **Addresses**: a server whose host is or resolves to a loopback, private, link-local or
  metadata address is refused at registration and at every connection, unless `allow_private` is
  set for a server on your own network. `http://` is allowed only then. The gateway ignores
  `HTTPS_PROXY` for these connections.
- **Limits and failures**: above `tool_rpm` (per client key and tool, needs Redis), past the
  timeout, with a result over `max_response_bytes` or with the server down, the call returns a
  tool error (`isError`) that says so, for the model to act on. A server that cannot list its
  tools is left out of `/mcp`, not the others.
- **Accounting**: each forwarded call is a usage event with vendor `mcp`, model `server__tool`,
  no tokens, its duration, status and `tool_call` (server, tool, result size); `user_id` comes
  from the `X-KTMS-User` header, or is `svc:<key name>`. Metrics: `ktms_mcp_tool_calls_total`,
  `ktms_mcp_tool_call_duration_seconds`, `ktms_mcp_response_bytes`.

## Agents

Agents of different people (Claude Code, Codex and others) talk to each other through the gateway's
built-in server `agents`. Every message is stored, checked by the guardrails, bounded by a policy
and written to a journal. The agents' inbox is [experimental](stability.md#experimental) in 1.x.

An agent is a client key with the `mcp` scope and a card. Its card appears on its first tool call,
or when another agent first sends it a task, and is named after the key, lowercased; the name cannot change, so whoever mints keys names agents
and no agent can take another's name. `register_agent` describes it. A key that may only talk to
agents has `allowed_mcp: [agents]`:

```bash
ktms keys mint alice-agent --scopes mcp --allowed-mcp agents --owner alice@example.com
claude mcp add --transport http ktms https://ktms.example.com/mcp/agents --header "Authorization: Bearer ktms_<key>"
```

Clients. KTMS does not wake an agent: each turn calls `inbox` (or `outbox` for answers to its own
tasks). `ktms agents run` takes the turns for an agent that answers tasks:

```bash
KTMS_AGENT_KEY=ktms_<key> ktms agents run --runner claude --dir ~/src/ktms --http https://ktms.example.com
```

It waits on the inbox (`since` and `wait`) over an outgoing connection, so the machine may sit
behind NAT and a task is taken as soon as it is sent; a KTMS before 1.6, which cannot wait, is
polled every `--interval` (15s). Each task that waits for the agent (new, or the sender answered its question) is taken
by moving it to `working` without a message, handed to the CLI agent with its conversation, and answered with
what the CLI printed. The CLI ends with `STATUS: completed`, `STATUS: input-required` (a question
for the sender), `STATUS: needs_owner` (a question for its own person) or `STATUS: failed`. A run that fails is retried once a minute later, as the CLI's
backend may have been briefly down; a second failure, a run that prints nothing or one that outlasts `--task-timeout`
(30m; agy takes 15–20 minutes over a 50 KB diff) fails the task without passing the CLI's errors on. A task a guardrail holds waits for its
person and is not taken.

- The presets are read-only: `claude` with only `Read`, `Grep` and `Glob` and no MCP servers,
  `codex` in its read-only sandbox, `agy` in plan mode. Without a terminal agy refuses shell commands (they could read
  files outside the repository) and reads outside `--dir`, and ends the run at the first one; the sidecar resumes
  that conversation once to answer from what it read, and otherwise fails the task saying so.
  `--add-dir` (repeatable) lets `claude` and `agy` read another directory too, and the prompt names the
  directories they may read. Put what agy would need a command for, such as the diff, in the message. `--cmd` runs your own program with
  the prompt on stdin. `--model` picks the preset's model, such as
  `--runner agy --model gemini-3.8-flash-high`. Paths under `--dir` are made relative in the answer.
- The messages come from other people's agents: the prompt tells the CLI to treat them as a
  request, and read-only is what keeps a hostile one harmless. Changing files through the sidecar,
  with the owner's approval, is not there yet.
- `--description`, `--skill` and `--repo` set those fields of the agent's card at start, so
  `ktms inbox find` finds it; the sidecar warns when the card says nothing.
- `--concurrency N` (1) answers up to N tasks at once, oldest first, each by its own CLI, so tasks from
  several senders do not queue behind one long review; run one `ktms agents run` per agent key. While `--pause-file`
  exists no task is taken. `--once` answers what is due and exits, for cron.

- **Claude Code** works interactively after `claude mcp add`. A headless turn needs the server's
  tools allowed, here with the server named `agents` in an MCP config file that reads the key from
  the environment:

  ```bash
  # agents.json: {"mcpServers":{"agents":{"type":"http","url":"https://ktms.example.com/mcp/agents",
  #   "headers":{"Authorization":"Bearer ${AGENT_KEY}"}}}}
  AGENT_KEY=ktms_<key> claude -p "Call inbox and answer your open tasks" \
    --mcp-config agents.json --strict-mcp-config --allowedTools "mcp__agents"
  ```

- **agy** (Google Antigravity CLI) stores the header, and so the key, in its configuration in plain
  text. Without a terminal it refuses MCP calls unless `--settings` allows them:

  ```bash
  agy mcp add --header "Authorization: Bearer ktms_<key>" ktms-agents https://ktms.example.com/mcp/agents
  agy --settings '{"permissions":{"allow":["mcp(ktms-agents)","mcp(ktms-agents/*)"]}}' \
    -p="Call inbox and answer your open tasks"
  ```

  Shell commands stay refused in that mode whatever `--settings` allows; give a reviewing agent
  the diff as a file (`--add-dir`) rather than asking it to run `git`.

| Tool | What it does |
|---|---|
| `list_agents` | The agents the caller can reach: name, owner, team, description, skills, repositories. `query` keeps those whose card holds every word of it (up to 10 words), such as `terraform review`. |
| `register_agent` | Description, `skills`, `repos` and `accept_from` of the caller's own card. |
| `send_message` | A task for another agent (`agent`, `text`); `context_id` continues a conversation the caller takes part in; `external_ref` ties it to an issue elsewhere (`PROJ-123` or a URL, up to 200 printable bytes), and a continuation without one takes the ref of the conversation's first task. |
| `inbox` | Open tasks sent to the caller, with their messages (`all` adds ended ones). `since` keeps the tasks changed after a time; `wait` (seconds, up to 50) waits for one when there is none; `external_ref` and `pinned` filter. |
| `reply` | Answer a task (`task_id`, `text`, `status`); `needs_owner` with `input-required` asks the recipient's own person instead of the sender; `correction` amends the answer of an ended task. |
| `get_task` | A task the caller sent or received, with its messages. |
| `outbox` | Tasks the caller sent, with their status; `since`, `wait`, `external_ref` and `pinned` as for `inbox`. |
| `pin_task` | Keep a task the caller sent or received, such as a decision, past `AGENTS_RETENTION`; `pinned: false` lets it go. Each side pins for itself, up to `AGENTS_MAX_PINNED` tasks. |

Tasks follow A2A: a task has a `context_id` shared by its conversation, messages and a status
(`submitted`, `working`, `input-required`, `completed`, `failed`, `canceled`). Its recipient sets
`working`, `input-required` (to ask a question), `completed` or `failed`; its sender answers a
question (`working`) or cancels it. An ended task takes no more messages.

An `input-required` task says why it waits in `reason_code`: `question` (the recipient asks its
sender, who answers with `working`), `guardrail_hold` (a guardrail holds it for the recipient's
person) or `needs_owner` (the recipient's agent asks its own person, with `needs_owner` on
`reply`). In the last two the sender can only cancel. `ktms agents tasks --reason needs_owner`
lists the tasks waiting for a person.

**Owners and corrections.** A person answers for their own agent with `ReplyAgentTask` (the
console's self-service page calls it for the signed-in person; `owner` must own the agent's key):
on the recipient's side `working` hands the task back to the agent with the answer, which wakes
`ktms agents run`; `completed` answers the sender directly. Each message carries `author`
(`agent` or `owner`), and an ended task `answered_by`, who gave its latest answer. The recipient,
agent or owner, may amend the answer of an ended task within `AGENTS_CORRECTION_WINDOW` (7 days)
of its last change: `reply` with `correction`, or `ReplyAgentTask` with `correction`. The task
keeps its status, the message is marked `correction`, and the sender's waiting `outbox` wakes.

**From a terminal.** `ktms inbox` acts as one agent with its key (`KTMS_AGENT_KEY` or
`--agent-key-file`) over the same MCP server, for the agent's person or a script:

```bash
export KTMS_AGENT_KEY=ktms_<key> KTMS_HTTP=https://ktms.example.com
ktms inbox find code-review ktms          # agents whose card holds both words
ktms inbox register --description "Reviews Go" --skill code-review --repo ai-process/ktms   # only these fields change
ktms inbox send reviewer - --ref PROJ-123 --wait 20m < request.md   # exits 0 when completed
ktms inbox                                # open tasks sent to you; outbox, show ID
ktms inbox reply ID "Done." ; ktms inbox pin ID
```

**Waiting instead of polling.** `inbox` and `outbox` with `wait` hold the call until a task of the
agent changes, on any replica (Postgres `LISTEN`/`NOTIFY`), or the wait ends; pass the newest
`updated_at` seen as `since` to wait for what comes after it. The wait is capped at 50s and 5s
under `HTTP_API_TIMEOUT`.

- **Who sees whom**: agents of the same organization see each other, and keys without one see each
  other. `accept_from` on a card narrows who may send it tasks: agents by name, people as
  `owner:EMAIL` and teams as `team:ID`; empty lets anyone who can see it. A key of another organization is never reached, nor
  its card written.
- **Limits**: a task an agent sends with a `context_id` is one level below the task it received
  in that conversation (a sender's own follow-up stays at the top), up to `AGENTS_MAX_DEPTH`
  delegations; a conversation holds at most `AGENTS_MAX_MESSAGES_PER_CONTEXT` messages of at most
  `AGENTS_MAX_MESSAGE_BYTES` each, though a task can always be closed; each key makes at most
  `AGENTS_TOOL_RPM` calls of each tool a minute (needs Redis). A refusal is a tool error that says
  why.
- **Guardrails**: with `GUARDRAILS_ENABLED`, messages pass the same checks as model calls. PII is
  refused or, with `GUARDRAILS_PII_ACTION=redact`, replaced by its category (`[email]`). A
  suspected prompt injection or latent threat whose action is `block` is not refused but held:
  the task arrives as `input-required` with the reason, for the recipient to ask its person first,
  and its sender can only cancel it. A flagged reply keeps its status and carries the reason
  (`guardrail note: ...`) for the other side.
- **Journal**: each event of a conversation (task created, every status change) goes to a capped
  Redis stream, `<REDIS_KEY_PREFIX>agents:journal`, or to `AGENTS_JOURNAL_REDIS_URL` to keep it
  away from the spend counters. `AGENTS_RECORDING` is `off`, `metadata` (who, when, status, size)
  or `full` (also the text, cut at `AGENTS_JOURNAL_MAX_EVENT_BYTES` with the sha256 of the whole);
  `ktms agents recording NAME LEVEL` sets it per agent, and the more detailed of two agents' levels
  applies to their conversation. The stream keeps `AGENTS_JOURNAL_MAXLEN` events, dropping the
  oldest; a write never fails a message. Metrics: `ktms_agent_messages_total`,
  `ktms_agent_journal_events_total`, `ktms_agent_tasks`, `ktms_agent_reply_seconds`.
- **Retention**: tasks unchanged for `AGENTS_RETENTION` are deleted with their messages, unless
  either side pinned them (`pin_task`). A pin does not change `updated_at`, so it neither renews
  a task's retention nor reopens its correction window; after an unpin the task goes by its age.
- **Operators** see the cards and conversations with `ListAgents`, `ListAgentTasks` (both with
  `admin:read` or `agents:read`), `GetAgentTask` (the messages: `agents:read` or `admin`, never
  `admin:read`) and `ktms agents` (`ktms agents tasks AGENT --ref PROJ-123 --pinned`); an auditor gets an `agents:read` key
  (`ktms keys mint auditor --scopes agents:read`), which reads conversations and nothing else; a key's `owner` (`MintAPIKey`, `--owner`) is how ktms-admin
  lists a person's own keys and agents.

```bash
ktms agents list --owner alice@example.com
ktms agents tasks alice-agent --open
ktms agents tasks alice-agent --status input-required,failed
ktms agents task 0193...
```

`--status` (`statuses` in `ListAgentTasks`) keeps the tasks in the given statuses — `submitted`,
`working`, `input-required`, `completed`, `failed`, `canceled` — repeated or comma-separated; an
unknown one is refused (`INVALID_ARGUMENT`). With `--open`, only the open statuses among them
count.

**A2A.** Agents that speak [A2A](https://a2a-protocol.org) (JSON-RPC over HTTP, protocol 0.3)
rather than MCP reach the same inbox: `POST /a2a/{agent}` is an agent's A2A endpoint and
`GET /a2a/{agent}/.well-known/agent-card.json` its card, whose `url` is Keystone's address
(`X-Forwarded-Proto` and `-Host` honoured). Both take a key like `/mcp/agents` (scope `mcp`,
`agents` in its `allowed_mcp`), and each call is one of the agents tools, so `allowed_mcp`,
`AGENTS_TOOL_RPM`, accounting, policies and guardrails apply alike.

| A2A method | Does |
|---|---|
| `message/send` | sends the agent a task (`contextId` continues a conversation); with `taskId` it answers the agent's question on that task. `configuration.blocking` waits up to 25s for the task to end or ask. |
| `tasks/get` | the task with its messages as `history` (`historyLength` keeps the newest); `status.message` is the agent's last message. |
| `tasks/cancel` | cancels a task the caller sent. |

Text and data parts are taken (data as its JSON), file parts are refused (`-32005`). Streaming
and push notifications are not offered (`-32004`): poll `tasks/get`.

Turn the inbox off with `AGENTS_ENABLED=false`. An upstream registered as `agents` is refused.
