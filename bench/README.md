# Gateway benchmarks

A reproducible stand that compares KTMS with the notable self-hosted LLM gateways and with calling
the upstream directly. Every number in a report is measured during that run. Nothing is hardcoded or
estimated.

```
 runner VM                     SUT VM (private network)
 ┌──────────┐   HTTP    ┌───────────────────────────────────────┐
 │   k6     │ ────────▶ │ KTMS, LiteLLM, ─┐                     │
 │ (2.3.0)  │           │ Bifrost, ...    ┼──▶ mock-upstream     │
 └──────────┘           │ (direct) ───────┘    Postgres, Redis   │
                        └───────────────────────────────────────┘
```

- **mock-upstream** (`mock-upstream/`, Go, stdlib only) is an OpenAI-compatible `/v1/chat/completions`
  that returns fixed bodies, either as JSON or as SSE. Its timing comes from a directive inside the
  prompt, e.g. `[mock latency_ms=10 chunks=20 chunk_delay_ms=15]`. Proxies forward prompts verbatim
  but drop custom headers, so every target gets byte-identical requests. `GET /stats` counts the
  requests that actually reached it.
- Only **one gateway runs at a time**. The others are removed before its turn, so they never compete
  for CPU.
- Images are pinned in `docker-compose.bench.yaml`; KTMS is built from the checked-out commit
  (`slim` target).

## Gateways

| Gateway | Image | Config | Client auth |
|---|---|---|---|
| KTMS | built from this repo | auto-seed, then `prepare-ktms` lifts the seeded limits | key |
| LiteLLM | `ghcr.io/berriai/litellm:v1.106.0` | `gateways/litellm/` | master key |
| Bifrost | `maximhq/bifrost:v2.3.1` | `gateways/bifrost/` | none |
| Portkey | `portkeyai/gateway:1.15.2` | request headers | none |
| Kong AI Gateway (OSS) | `kong:3.9.4` | `gateways/kong/` | key-auth |
| Apache APISIX | `apache/apisix:3.19.0-debian` | `gateways/apisix/` | key-auth |
| Higress | `higress/all-in-one:2.2.5` | `gateways/higress/seed/` | key-auth |
| Envoy AI Gateway | `envoyproxy/ai-gateway-cli:v1.2.0` | `gateways/aigw/` | none |
| agentgateway | `cr.agentgateway.dev/agentgateway:v1.6.0` | `gateways/agentgateway/` | API key |
| New API | `calciumion/new-api:v1.0.0-rc.45` | `prepare-newapi` (admin API) | token |
| Plano | `katanemo/plano:0.4.37` | `gateways/plano/` | none |

Each config routes `gpt-4o-mini` to the mock. Retries and caching are off, and so are access logs and
telemetry wherever the gateway allows it. `gateways.json` holds each gateway's port, client key,
headers, optional `prepare` hook and the notes printed in the report. To add a gateway, add a service
to the compose file and an entry to `gateways.json`; `TARGETS="direct ktms bifrost"` runs a subset.

## Scenarios

| # | Script | Measures |
|---|---|---|
| 1 | `k6/scenarios/01_latency_overhead.js` | Added latency (p50/p95/p99) over the direct baseline at a fixed, unsaturated rate with a 0 ms upstream. |
| 2 | `k6/scenarios/02_streaming_ttft.js` | TTFT and total stream time with concurrent SSE streams; lost chunks reveal buffering or coalescing. |
| 3 | `k6/scenarios/03_concurrency.js` | Stepped arrival rate (up to 4000 RPS). Reports the highest step sustained with <1% errors. Runs last, because it can leave the process at peak heap. |
| 4 | `k6/scenarios/04_soak.js` | Steady load. Gives RSS under load, drift between the first and last quarter (leaks), and CPU time per 1k requests. |
| – | `scripts/sut.sh cold-start` | Time from `docker start` of a fresh container to the first successful chat completion through it. Health endpoints are not used: on some gateways they turn green before routes load. |

Memory is the gateway container's cgroup v2 `anon` (RSS); peak is `memory.peak`. KTMS and New API
also need Postgres and Redis, which are not counted. Multi-process gateways get one worker per SUT
core (`SUT_CPUS`, 4 locally); Portkey ships as a single Node.js process and runs as shipped.

## Cloud run (published numbers)

Needs `terraform` ≥ 1.5 and GCP Application Default Credentials
(`gcloud auth application-default login`) for a project with the Compute API enabled.

```bash
cd bench
GCP_PROJECT=my-project ./scripts/run_cloud.sh                 # full profile, all gateways, ~2.5 h
PROFILE=quick GCP_PROJECT=my-project ./scripts/run_cloud.sh
TARGETS="direct ktms kong" GCP_PROJECT=my-project ./scripts/run_cloud.sh
# Parallel stands need distinct prefixes (each gets its own Terraform workspace):
TF_VAR_prefix=ktms-bench-a RUN_ID=run-a GCP_PROJECT=my-project ./scripts/run_cloud.sh &
```

The script:

1. Creates a dedicated VPC `ktms-bench-net` with subnet `10.77.1.0/24` in `europe-west3-a`, firewall
   rules that allow only SSH from outside, and two `c3-standard-4` VMs (4 vCPU, not overcommitted):
   SUT at `10.77.1.10` and runner at `10.77.1.20`. Project-wide SSH keys are blocked; a throwaway
   ED25519 key is generated per stand.
2. Ships `git archive HEAD` to both machines. Uncommitted changes are not benchmarked.
3. Runs the suite: k6 runs on the runner; cold start and cgroup reads run on the SUT.
4. Copies results to `results/<run-id>/` and always runs `terraform destroy`, even on failure.
   Set `KEEP=1` to keep the stand for debugging.

Every resource is prefixed `ktms-bench-` (override with `TF_VAR_prefix`) and labeled
`managed-by=ktms-bench`; Terraform state only tracks the stand, so other resources in the project are
never touched. Region, zone and machine types are variables in `terraform/variables.tf`.

## Local run (regressions)

```bash
cd bench
PROFILE=quick ./scripts/run_all.sh   # ~15 min
./scripts/run_all.sh                 # full profile
./scripts/sut.sh down                # remove containers and volumes
```

Here k6 and the gateways share one machine, so use local runs to compare commits, not gateways.
On Apple Silicon, the `linux/arm64` LiteLLM image ships x86_64 binaries. It runs emulated and is
roughly 10x slower. The report detects this and warns that those numbers are not comparable.

## Output

`results/<run-id>/` contains `REPORT.md`, `results.json` (machine-readable), the raw k6 summaries
`<target>.<scenario>.json`, `<gateway>.resources.jsonl` (cgroup samples) and `env.json`
(hardware, kernel, image sizes, commit). Durations and rates can be overridden through the
variables at the top of `scripts/run_all.sh`, e.g. `LAT_RATE=500 SAT_STEPS=500,1000,3000`.

To replace some targets with a rerun (for example after fixing one gateway), rerun them together with
`direct` and merge: `scripts/merge_runs.py results/final results/<base> ktms=results/<rerun>`. The
report then marks every replaced target with its run, commit and direct baseline.

`go test ./bench/...` covers the mock upstream.
