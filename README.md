# Keystone. The Model Switchboard. (KTMS)

[![CI](https://github.com/ai-process/ktms/actions/workflows/test.yml/badge.svg)](https://github.com/ai-process/ktms/actions/workflows/test.yml)
[![Buf](https://github.com/ai-process/ktms/actions/workflows/buf.yml/badge.svg)](https://github.com/ai-process/ktms/actions/workflows/buf.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ai-process/ktms)](https://goreportcard.com/report/github.com/ai-process/ktms)
[![Release](https://img.shields.io/github/v/release/ai-process/ktms?sort=semver)](https://github.com/ai-process/ktms/releases)
[![Docker Image](https://img.shields.io/docker/v/coyl/ktms?label=docker%20hub&sort=semver)](https://hub.docker.com/r/coyl/ktms)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache_2.0-blue.svg)](LICENSE)

A self-hosted LLM gateway and model switchboard. Your services talk to one endpoint; Keystone holds
the vendor API keys, decides which model answers each request, falls back when a vendor is down,
enforces rate and spend limits, and records usage. One Go binary: ~67 MB image, ~7 MiB idle RAM,
under 1 ms of added latency.

## Features

- **One place for vendor keys.** OpenAI, Anthropic, Google Gemini, DeepSeek and TypeSafe
  credentials live encrypted in Postgres (AES-256-GCM); clients never see them.
  → [Vendor keys](docs/configuration.md#2-vendor-keys-per-client),
  [rotating the encryption key](docs/configuration.md#rotating-the-encryption-key)
- **Routing by rules, not code.** Clients send an *effort* and free-form *attributes*; ordered
  rules map them to a model chain with fallbacks, changed at runtime without a redeploy.
  → [Models and rules](docs/configuration.md#setting-it-up-keys-models-rules),
  [Concepts](docs/concepts.md#concepts)
- **Configuration as code.** One YAML file, planned and applied like Terraform.
  → [Configuration as a file](docs/configuration.md#configuration-as-a-file)
- **Limits and money.** Requests per minute, daily token budgets and hard USD caps per key and per
  end user; organizations and teams. → [Spend limits](docs/configuration.md#5-spend-limits-optional),
  [Organizations and teams](docs/configuration.md#6-organizations-and-teams-optional)
- **Resilience.** Same-model retry, `Retry-After` back-off, circuit breakers and fallback across
  vendors. → [Concepts](docs/concepts.md#concepts), [When a dependency fails](docs/operations.md#when-a-dependency-fails)
- **More than text.** Streaming, structured (JSON-schema) output, batches at 50% discount, speech,
  images, typed judgments, embeddings and translation. → [Calling Keystone](docs/grpc-api.md#calling-keystone)
- **Drop-in APIs.** OpenAI-compatible `/v1` and the Anthropic Messages API: point an existing SDK
  at Keystone. → [HTTP APIs](docs/http-api.md#openai-compatible-http-api)
- **Your own hardware.** Ollama, vLLM, llama.cpp, FreeToken or a translation engine on a Mac or a
  GPU box, with cloud fallback; restricted data never leaves them.
  → [Nodes](docs/nodes.md#run-a-node), [Restricted data](docs/concepts.md#restricted-data)
- **MCP gateway and agents' inbox.** MCP servers behind one key with per-tool limits, and an inbox
  where agents send each other tasks. *Experimental.* → [MCP gateway](docs/mcp.md#mcp-gateway),
  [Agents](docs/mcp.md#agents)
- **Usage and observability.** One event per call to light-tokenmeter, Prometheus metrics and
  ready-made alerts. → [Operations](docs/operations.md#operations)
- **Console, CLI and assistants.** The ktms-admin web console, the `ktms` CLI, an MCP server and a
  Claude Code plugin. → [Console, CLI and AI assistants](docs/console-and-cli.md#command-line-tool)

## How it works

```
 your services ──gRPC (Bearer ktms_…)──▶ KTMS ──▶ OpenAI / Anthropic / Gemini / DeepSeek / TypeSafe
                                          │  │
                                 Postgres ┘  └ Redis (limits)      └─▶ usage sink (optional)
                                 config, keys, encrypted vendor keys
```

Two gRPC services share port `9090`:

| Service | Purpose | Needed scope |
| --- | --- | --- |
| `ktms.v1.GatewayService` | `GenerateText`, `GenerateTextStream`, `SubmitBatch`, `GetBatch`, `ListBatchResults`, `CancelBatch`, `SynthesizeSpeech`, `GenerateImage`, `Judge`, `JudgeStream`, `EvaluateRollouts`, `Translate`, `GetEmbeddings`, `ListModels` | `generate` |
| `ktms.v1.AdminService` | models, rules, vendor keys, API keys, key rotation, `ExplainRoute` | `admin`; `admin:read` for the `Get*` and `List*` reads and `ExplainRoute`; `agents:read` for the agents' cards and conversations |

Port `8080` serves the health probes (`/healthz`, `/readyz`) and the
[OpenAI-compatible HTTP API](docs/http-api.md#openai-compatible-http-api) under `/v1`. gRPC server
reflection is on, so `grpcurl` works without the `.proto` files.


## Quick start

```bash
curl -fsSL https://ai-process.dev/install.sh | sh
```

It needs Docker with Compose, starts Keystone with Postgres and Redis on localhost, asks for an
OpenAI, Anthropic or Gemini key and prints your first request with a ready client key.

From a clone, with an AI assistant, or from scratch with `ktms init`: [Quick start](docs/quickstart.md).

## Documentation

| Document | What is in it |
| --- | --- |
| [Quick start](docs/quickstart.md) | Docker Compose, the first call, `ktms init`, setting up with an AI assistant |
| [Installation](docs/install.md) | Resource footprint, Docker image, Helm chart, building from source, production checklist |
| [Configuration](docs/configuration.md) | Every environment variable; keys, vendor keys, models, rules, limits, tenants as API calls or a file |
| [Nodes](docs/nodes.md) | The node daemon, running a local or translation node, troubleshooting |
| [Console, CLI and AI assistants](docs/console-and-cli.md) | ktms-admin, the `ktms` command, its MCP server, the Claude Code plugin |
| [Calling Keystone (gRPC)](docs/grpc-api.md) | Client SDKs, text, streaming, sampling, batches, speech, images, judge, translation, embeddings |
| [HTTP APIs](docs/http-api.md) | OpenAI-compatible endpoints, model and placement selection, Anthropic Messages API |
| [MCP gateway and agents](docs/mcp.md) | MCP servers behind Keystone, the agents' inbox |
| [Concepts](docs/concepts.md) | Throttling, circuit breaker, back-off, usage reporting, restricted data, semantic cache |
| [Error contract](docs/errors.md) | RPC status codes and reasons, batch item errors |
| [API stability](docs/stability.md) | What is public, experimental features, compatibility, upgrading, deprecations |
| [Operations](docs/operations.md) | Backups, dependency failures, metrics and alerts, sizing |
| [Development](docs/development.md) | Building, tests, code generation, repository layout |

## Glossary

| Term | Meaning |
| --- | --- |
| **Client key** | A `ktms_…` API key a service calls Keystone with. Has scopes (`generate`, `admin`, `admin:read`, `mcp`, …) and an optional policy with limits. → [API keys](docs/configuration.md#1-api-keys) |
| **Vendor key** | A provider's API key (OpenAI, Anthropic, …), stored encrypted and assigned to one client key. → [Vendor keys](docs/configuration.md#2-vendor-keys-per-client) |
| **Model** | A vendor model registered in Keystone with its prices, limits and capabilities. → [Models](docs/configuration.md#3-models) |
| **Rule** | Maps an effort and attributes to an ordered model chain; the first matching rule by priority wins. → [Rules](docs/configuration.md#4-rules) |
| **Effort** | `LOW`, `MEDIUM` or `HIGH`: how much model a request needs. The only routing field Keystone understands; with the HTTP model `auto` a judge can pick it. → [Concepts](docs/concepts.md#concepts) |
| **Attributes** | Free-form key–value pairs on a request that rules match on and usage records carry. |
| **Modality** | The `modality` attribute (`tts`, `image`, `judge`, `translate`) that sends a request to rules and models for that kind of work. → [Speech, image and judgment rules](docs/configuration.md#speech-image-and-judgment-rules) |
| **Fallback chain** | The models a rule lists; Keystone moves to the next one when a model fails, is throttled or paused. |
| **Circuit breaker** | Skips a `(vendor, model)` after repeated failures until a probe succeeds. → [Concepts](docs/concepts.md#concepts) |
| **Placement** | Where a request may run: cloud vendors, self-hosted nodes, or nodes first. → [Choosing a model and placement](docs/http-api.md#choosing-a-model-and-placement) |
| **Node** | A machine with your own inference engine that Keystone routes to. → [Run a node](docs/nodes.md#run-a-node) |
| **Node daemon** | The process on a node that reports its models and load to Keystone. → [Node daemon](docs/nodes.md#node-daemon) |
| **Node key** | The credential a node daemon authenticates with, optionally limited to some models. |
| **Engine** | The inference server behind a node: Ollama, vLLM, llama.cpp, FreeToken or `ktms-mt` for translation. |
| **Restricted data** | `data_classification: restricted`: a call that stays on your nodes, with no cloud fallback. → [Restricted data](docs/concepts.md#restricted-data) |
| **Spend limit** | A hard USD cap per key or end user per day or month, reserved before a call. → [Spend limits](docs/configuration.md#5-spend-limits-optional) |
| **Organization, team** | Tenants that keys belong to; limits and visibility follow them. → [Organizations and teams](docs/configuration.md#6-organizations-and-teams-optional) |
| **Batch** | An asynchronous set of generations at the vendors' batch discount. → [Batch text generation](docs/grpc-api.md#batch-text-generation) |
| **Judge** | A typed judgment (score, choice, yes/no) by a model, also used for auto effort and shadow evaluation. → [Judge](docs/grpc-api.md#judge) |
| **Shadow routing** | Mirroring a share of traffic to a candidate model, optionally judged against the primary. |
| **Semantic cache, latent guardrails** | Experimental: answers for similar prompts and prompt screening in embedding space. → [Semantic cache and latent guardrails](docs/concepts.md#semantic-cache-and-latent-guardrails) |
| **Usage sink** | A gRPC service that receives one usage event per call; light-tokenmeter is the reference one. → [Concepts](docs/concepts.md#concepts) |
| **light-tokenmeter** | The companion service that stores usage and answers spend questions. [Repository](https://github.com/ai-process/light-tokenmeter) |
| **ktms-admin** | The web console for Keystone. → [Web console](docs/console-and-cli.md#web-console) |
| **MCP gateway** | Keystone as a proxy for MCP servers, with per-key tool access and per-tool limits. → [MCP gateway](docs/mcp.md#mcp-gateway) |
| **Agent, inbox** | An agent's card and the tasks other agents send it through Keystone. → [Agents](docs/mcp.md#agents) |

## Roadmap

What we build now, next and later, and what is not planned, is in [`ROADMAP.md`](ROADMAP.md).
Requests and ideas are welcome as [issues](https://github.com/ai-process/ktms/issues).
Contributions: [`CONTRIBUTING.md`](CONTRIBUTING.md) and [Development](docs/development.md).

## License

[Apache 2.0](LICENSE)
