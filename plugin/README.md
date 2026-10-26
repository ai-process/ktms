# Keystone (KTMS) plugin for Claude Code

Skills that walk an assistant through the usual Keystone and light-tokenmeter tasks, and both MCP
servers. Install it in one go:

```bash
claude plugin marketplace add ai-process/ktms && claude plugin install ktms@ai-process
```

| Skill | For |
| --- | --- |
| `/ktms:assess` | Where Keystone fits the system you run in, and the plan to get there |
| `/ktms:integrate` | Moving a service's LLM calls onto Keystone |
| `/ktms:configure` | Routing, keys and limits as a ktms config file, planned and applied |
| `/ktms:deploy` | Compose or Helm for the stack, and the first-start runbook |
| `/ktms:node` | A local GPU node with cloud fallback |
| `/ktms:migrate` | Moving from LiteLLM or vendor SDKs |
| `/ktms:usage-reports` | Spend questions, reports and alerts from light-tokenmeter |
| `/ktms:production-review` | Checking a deployment before and after it goes live |

The assistant picks a skill by itself when the task fits; the slash command starts one by name.

## Requirements

Keystone 1.2 or later with its `ktms` CLI, and light-tokenmeter 1.1 or later with its `tokenmeter`
CLI, for the skills' commands and the MCP servers. Until those releases are out, build both CLIs
from `main` (`go install github.com/ai-process/ktms/cmd/ktms@main`,
`go install github.com/ai-process/light-tokenmeter/cmd/tokenmeter@main`).

## MCP servers

`.mcp.json` starts `ktms mcp` and `tokenmeter mcp`, read-only. Both binaries must be on `PATH`
(release archives or `go install`, see above), and they
find Keystone and light-tokenmeter the way the CLIs do: the current context, or `KTMS_CONTEXT`,
`KTMS_ADDR`, `KTMS_ADMIN_KEY`, `TOKENMETER_ADDR`, `TOKENMETER_TOKEN` in the environment Claude
Code runs in. Use an `admin:read` key and a `read` token. To let the assistant change Keystone,
add your own entry with `"args": ["mcp", "--allow-write"]` in the project's `.mcp.json`.

## Other assistants

The skills are plain Agent Skills (a folder with `SKILL.md` that carries `name` and
`description`): copy `skills/*` into the skills directory of Codex, Cursor or another agent that
reads them, and add the two MCP servers to its configuration. The same texts, as prompts to paste,
are at [ai-process.dev/docs/prompts](https://ai-process.dev/docs/prompts/).

## Versions

The plugin's version is Keystone's release; CI fails when a skill names a command, flag, MCP tool
or README section that does not exist.
