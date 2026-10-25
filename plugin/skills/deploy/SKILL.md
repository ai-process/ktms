---
name: deploy
description: Write deployment configuration for Keystone (KTMS) with light-tokenmeter and ktms-admin - Docker Compose or the Helm chart - with secrets from a secret store and a first-start runbook. Use when asked to deploy, install or containerize KTMS or its stack.
---

# Deploy the stack

## Ask first

Platform (Compose on one host, or Kubernetes with Helm and which ingress), managed or in-cluster
Postgres and Redis, the secret store, public hostnames (HTTP API, gRPC, console), console sign-in,
monitoring.

## Facts

- Images, pinned to a release (never `latest`): `coyl/ktms:<v>` (`-slim`, or `-audio` with
  ffmpeg), `coyl/light-tokenmeter:<v>`, `coyl/ktms-admin:<v>`.
- Keystone: gRPC `:9090`, HTTP `:8080` (`/v1`, `/healthz`, `/readyz`), metrics `:9091`.
  Needs `DATABASE_URL`, `KTMS_ENCRYPTION_KEYS` (`1:` + base64 of 32 random bytes, backed up
  apart from the database), `REDIS_ADDR` (without Redis no limit is enforced),
  `USAGE_COLLECTOR_HOST` and `USAGE_COLLECTOR_TOKEN`, and only on first start
  `BOOTSTRAP_ADMIN_KEY`.
- Helm: `helm install ktms oci://ghcr.io/ai-process/charts/ktms --version <v>` with
  `secrets.existingSecret`; the gRPC ingress is its own Ingress and host. light-tokenmeter and
  ktms-admin have no chart.
- light-tokenmeter: `TOKENMETER_TOKENS` holds hashes only; `tokenmeter token new --name ktms
  --scope write` makes the token for Keystone and its entry, a read token for the console.
- Keystone and light-tokenmeter speak plaintext: TLS at the ingress, `:9091`, Postgres and Redis
  off the public network, `/readyz` probed inside only.

## Deliver

1. The files, every secret as a reference to the store, probes on `/readyz`, two or more
   Keystone replicas on Kubernetes, resources from [Sizing](https://github.com/ai-process/ktms/blob/main/README.md#sizing).
2. A runbook: generate the keyring and tokens (commands, not values); start with
   `BOOTSTRAP_ADMIN_KEY`; on an empty Keystone the user runs `ktms init` in their own terminal
   (it asks for the admin and vendor keys without echo, so they never reach the chat);
   `ktms keys mint console --scopes admin` and a viewer key with `--scopes admin:read`; put them
   in the store; remove the bootstrap key; restart.
3. NetworkPolicies or a firewall note; the alert rules from [Metrics and alerts](https://github.com/ai-process/ktms/blob/main/README.md#metrics-and-alerts).
4. Check after start: `ktms health` shows both probes 200 and the version; `ktms config plan`
   against the repository's `ktms.yaml` shows no surprise.

Use only environment variables the sources name. Sources: [Installation](https://github.com/ai-process/ktms/blob/main/README.md#installation),
[Configuration reference](https://github.com/ai-process/ktms/blob/main/README.md#configuration-reference), [Production checklist](https://github.com/ai-process/ktms/blob/main/README.md#production-checklist),
light-tokenmeter [Authentication](https://github.com/ai-process/light-tokenmeter/blob/main/README.md#authentication).
