# Quick start (Docker Compose)

**The problem.** Before you wire a gateway into anything, you want to see one call go through it,
on your own machine, in minutes.

**The approach.** One command runs the published images of Keystone, Postgres and Redis with
Docker Compose on localhost, asks for one vendor key and prints a first request with a ready client
key. Nothing is cloned or built.

<p align="center"><img src="img/quickstart-stack.svg" alt="install.sh starts Keystone, Postgres and Redis in ~/ktms with Docker Compose; your first call goes to Keystone, which calls your vendor with your key" width="800"></p>

**Contents:** [One command](#one-command) · [From a clone](#from-a-clone) ·
[First call](#2-make-your-first-call) · [With an AI assistant](#with-an-ai-assistant) ·
[From scratch with `ktms init`](#setting-it-up-from-scratch-with-ktms-init)

## One command

Needs Docker with the Compose plugin.

```bash
curl -fsSL https://ai-process.dev/install.sh | sh
```

| What | Where |
| --- | --- |
| Compose file and `.env` (generated keyring, admin and client keys) | `~/ktms` |
| HTTP and OpenAI-compatible API | `localhost:8080` |
| gRPC | `localhost:9090` |
| Vendor key | Asked for, unless `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` or `GEMINI_API_KEY` is set |

A re-run keeps the files, adds new vendor keys and moves to a new release. Options (`sh -s -- --help`):

| Option | Default | Meaning |
| --- | --- | --- |
| `--dir DIR` | `~/ktms` | Where `compose.yaml` and `.env` live. |
| `--version vX.Y.Z` | latest | Release to run. |
| `--port N`, `--grpc-port N` | `8080`, `9090` | Ports on localhost. |
| `--no-prompt` | — | Never ask for a vendor key. |
| `--no-start` | — | Write the files, start nothing. |
| `--uninstall` | — | Stop the stack and delete its data and files. |

The script is [`scripts/install.sh`](../scripts/install.sh).

## From a clone

Compose builds the image from source. With `AUTO_SEED` (on in Compose), Keystone creates default
models, rules and a client key `ktms_dev_default_key` on first start.

### 1. Launch the stack

```bash
git clone https://github.com/ai-process/ktms.git && cd ktms
OPENAI_API_KEY=sk-... docker compose up -d --build     # or GEMINI_API_KEY, ANTHROPIC_API_KEY
curl -s http://localhost:8080/readyz                    # {"status":"ready"}
```

- `./scripts/quickstart.sh` does the same interactively: it generates the encryption and bootstrap
  keys, writes `.env` and starts the stack.
- On every start, `AUTO_SEED` copies `OPENAI_API_KEY`, `GEMINI_API_KEY` and `ANTHROPIC_API_KEY`
  to the client key `dev-default` (or `$SEED_CLIENT_NAME`).

### 2. Make your first call

With `curl`:

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer ktms_dev_default_key" \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Say hi in three words."}]}'
```

With the OpenAI Python SDK:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="ktms_dev_default_key")
response = client.chat.completions.create(
    model="auto",
    messages=[{"role": "user", "content": "Say hi in three words."}],
)
print(response.choices[0].message.content)
```

The response headers say how it was routed:

| Header | Meaning |
| --- | --- |
| `X-KTMS-Vendor` | The vendor that answered. |
| `X-KTMS-Rule` | The rule that matched. |
| `X-KTMS-Placement` | Cloud or your own nodes. |

The old `X-LLM-Proxy-*` headers are sent too, for older clients. Next:
[HTTP APIs](http-api.md#openai-compatible-http-api) and [Configuration](configuration.md#setting-it-up-keys-models-rules).

## With an AI assistant

In Claude Code, the [plugin](console-and-cli.md#claude-code-plugin-and-skills) deploys and
configures the stack for you:

```bash
claude plugin marketplace add ai-process/ktms && claude plugin install ktms@ai-process
```

| Skill | What it does |
| --- | --- |
| `/ktms:assess` | Looks at your system and says where Keystone fits and what to do first. |
| `/ktms:deploy` | Compose or Helm with Keystone, light-tokenmeter and ktms-admin. |
| `/ktms:integrate` | Moves a service's LLM calls onto Keystone. |

Other assistants: [Prompts for AI assistants](https://ai-process.dev/docs/prompts/), starting with
[Deploy the stack](https://ai-process.dev/docs/prompts/deploy-stack/).

## Setting it up from scratch with `ktms init`

To start without auto-seeding, set `AUTO_SEED=false` and let the
[`ktms` command](console-and-cli.md#command-line-tool) ask for the rest:

```bash
ktms init                       # a context named local; ktms init prod for another one
```

It asks, in order:

1. the Admin API address and the admin key (`BOOTSTRAP_ADMIN_KEY`, `dev-bootstrap` in Compose),
   which it keeps in the keychain or a `0600` file under a new current context;
2. the vendor keys (OpenAI, Gemini, Anthropic; `OPENAI_API_KEY` and the like are offered when set);
3. a client key name.

Then it writes `ktms.yaml` with the default models and rules of those vendors, shows the plan and
applies it:

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

Vendor keys go to Keystone and stay out of the file. The client key is kept in the context too, so
a test call needs nothing more. From then on the file is the configuration:

```bash
ktms chat "Say hi"              # the answer, then rule, model and vendor
ktms config plan -f ktms.yaml   # after editing it
ktms config apply -f ktms.yaml
```

- On a Keystone that has models already, `ktms init` writes them to `ktms.yaml` instead.
- The same steps one by one: `ktms context set`, `ktms keys mint`, `ktms vendor-keys set` and
  [`ktms config apply`](configuration.md#configuration-as-a-file).
- Once real keys exist in production, unset `BOOTSTRAP_ADMIN_KEY`.
