# Quick start (Docker Compose)

The fastest way to try Keystone is one command. It needs Docker with the Compose plugin, runs the
published images (no clone, no build) with Postgres and Redis on localhost only, asks for an
OpenAI, Anthropic or Gemini key unless one is in the environment, and prints your first request:

```bash
curl -fsSL https://ai-process.dev/install.sh | sh
```

It keeps its files in `~/ktms` (a generated keyring, admin and client keys in `.env`), and a re-run
keeps them while it adds new vendor keys or moves to a new release. Options such as `--port`,
`--version` and `--uninstall` are in `sh -s -- --help`; the script is
[`scripts/install.sh`](../scripts/install.sh).

From a clone instead, Docker Compose builds the image from source. With `AUTO_SEED` enabled (default in compose), the proxy automatically initializes default models, routing rules, and a ready-to-use client key (`ktms_dev_default_key`).

## 1. Launch the stack

```bash
git clone https://github.com/ai-process/ktms.git
cd ktms

# Pass your vendor API key (OpenAI, Gemini, or Anthropic)
OPENAI_API_KEY=sk-... docker compose up -d --build

# Verify readiness
curl -s http://localhost:8080/readyz     # {"status":"ready"}
```

Alternatively, run `./scripts/quickstart.sh` for an interactive setup that generates master encryption and bootstrap keys, configures `.env`, and launches the stack.

> [!NOTE]
> With `AUTO_SEED=true` (enabled by default in Compose), the proxy automatically syncs vendor API keys from environment variables (`OPENAI_API_KEY`, `GEMINI_API_KEY`, `ANTHROPIC_API_KEY`) to the client key named `dev-default` (or `$SEED_CLIENT_NAME`) on every startup.

## 2. Make your first call

You can immediately query the proxy using the OpenAI-compatible HTTP endpoint:

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer ktms_dev_default_key" \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Say hi in three words."}]}'
```

Or using the official OpenAI Python SDK:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="ktms_dev_default_key")
response = client.chat.completions.create(
    model="auto",
    messages=[{"role": "user", "content": "Say hi in three words."}],
)
print(response.choices[0].message.content)
```

The response includes canonical observability headers (`X-KTMS-Vendor`, `X-KTMS-Rule`, `X-KTMS-Placement`) along with backwards-compatible `X-LLM-Proxy-*` headers.

## With an AI assistant

In Claude Code, the [plugin](console-and-cli.md#claude-code-plugin-and-skills) deploys and configures the stack for
you: install it, run `/ktms:assess` to see where Keystone fits your system and what to do first,
`/ktms:deploy` for Compose or Helm with Keystone, light-tokenmeter and ktms-admin, and
`/ktms:integrate` to connect a service.

```bash
claude plugin marketplace add ai-process/ktms && claude plugin install ktms@ai-process
```

For other assistants, [Prompts for AI assistants](https://ai-process.dev/docs/prompts/) covers the
same tasks, starting with [Deploy the stack](https://ai-process.dev/docs/prompts/deploy-stack/).

---

## Setting it up from scratch with `ktms init`

To start without auto-seeding, set `AUTO_SEED=false` and let the [`ktms` command](console-and-cli.md#command-line-tool)
ask for the rest:

```bash
ktms init                       # a context named local; ktms init prod for another one
```

It asks for the Admin API address and the admin key (`BOOTSTRAP_ADMIN_KEY`, `dev-bootstrap` in
Compose), keeps the key in the keychain or a `0600` file under a context it makes current, then for
the vendor keys (OpenAI, Gemini, Anthropic; `OPENAI_API_KEY` and the like are offered when set) and
a client key name. It writes `ktms.yaml` with the default models and rules of those vendors, shows
the plan and applies it:

```text
+ model gpt-4o-mini
+ api key my-app
+ vendor key my-app/openai
~ rules (4 rules, replaced together)
Plan: 5 to create, 1 to change, 0 to delete.
Apply these changes? [y/N] y

New client keys. They are shown only now; store them in your secret manager:
  my-app: ktms_…
```

The vendor keys go to Keystone and stay out of the file; the client key is kept in the context too,
so a test call needs nothing more. From then on the file is the configuration:

```bash
ktms chat "Say hi"              # the answer, then rule, model and vendor
ktms config plan -f ktms.yaml   # after editing it
ktms config apply -f ktms.yaml
```

On a Keystone that has models already, `ktms init` writes them to `ktms.yaml` instead. The same
steps one by one are `ktms context set`, `ktms keys mint`, `ktms vendor-keys set` and
[`ktms config apply`](configuration.md#configuration-as-a-file). Once real keys exist in production, unset
`BOOTSTRAP_ADMIN_KEY`.
