# Gateway benchmarks

A reproducible stand that compares KTMS with LiteLLM and with calling the upstream directly. Every
number in a report is measured during that run. Nothing is hardcoded or estimated.

```
 runner VM                     SUT VM (private network)
 ┌──────────┐   HTTP    ┌───────────────────────────────────────┐
 │   k6     │ ────────▶ │ KTMS :8080 ─┐                         │
 │ (2.3.0)  │           │ LiteLLM :4000 ┼──▶ mock-upstream :8000 │
 └──────────┘           │ (direct) ───┘     Postgres, Redis     │
                        └───────────────────────────────────────┘
```

- **mock-upstream** (`mock-upstream/`, Go, stdlib only) is an OpenAI-compatible `/v1/chat/completions`
  that returns fixed bodies, either as JSON or as SSE. Its timing comes from a directive inside the
  prompt, e.g. `[mock latency_ms=10 chunks=20 chunk_delay_ms=15]`. Proxies forward prompts verbatim
  but drop custom headers, so every target gets byte-identical requests. `GET /stats` counts the
  requests that actually reached it.
- Only **one gateway runs at a time**. The others are removed before its turn, so they never compete
  for CPU.
- Images are pinned: `grafana/k6:2.3.0`, `ghcr.io/berriai/litellm:v1.83.14-stable`. KTMS is built
  from the checked-out commit (`slim` target).

## Scenarios

| # | Script | Measures |
|---|---|---|
| 1 | `k6/scenarios/01_latency_overhead.js` | Added latency (p50/p95/p99) over the direct baseline at a fixed, unsaturated rate with a 0 ms upstream. |
| 2 | `k6/scenarios/02_streaming_ttft.js` | TTFT and total stream time with concurrent SSE streams; lost chunks reveal buffering or coalescing. |
| 3 | `k6/scenarios/03_concurrency.js` | Stepped arrival rate (up to 2000 RPS). Reports the highest step sustained with <1% errors. Runs last, because it can leave the process at peak heap. |
| 4 | `k6/scenarios/04_soak.js` | Steady load. Gives RSS under load, drift between the first and last quarter (leaks), and CPU time per 1k requests. |
| – | `scripts/sut.sh cold-start` | Time from `docker start` of a fresh container to the first 200 from its readiness endpoint. |

Memory is the gateway container's cgroup v2 `anon` (RSS); peak is `memory.peak`. KTMS also needs
Postgres and Redis, which are not counted. LiteLLM runs in its leanest mode: master key only, no
database. It uses one Uvicorn worker per SUT core (`LITELLM_WORKERS`, 4 locally); KTMS uses every
core from a single process.

## Cloud run (published numbers)

Needs `terraform` ≥ 1.5 and GCP Application Default Credentials
(`gcloud auth application-default login`) for a project with the Compute API enabled.

```bash
cd bench
GCP_PROJECT=my-project ./scripts/run_cloud.sh                 # full profile, ~40 min, ~$0.35
PROFILE=quick GCP_PROJECT=my-project ./scripts/run_cloud.sh
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

`go test ./bench/...` covers the mock upstream.
