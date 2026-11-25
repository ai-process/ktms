# Nodes

## Node agent

`bin/ktms-node` runs next to a local inference engine (`ollama`, `vllm`, `tgi`, `llamacpp`, `freetoken`),
registers it with Keystone and sends heartbeats with the loaded models, GPU metrics and the
engine's load: running and waiting requests from the `/metrics` of vLLM, TGI and llama.cpp
(start `llama-server` with `--metrics`), and from FreeToken's `/v1/stats`. Ollama has no such API, so its nodes report no queue. It
authenticates with a node key minted with the `MintNodeKey` admin RPC. KTMS dials the node,
so the node must be reachable; the agent can do that itself:

```bash
export NODE_KEY=...                       # or --node-key-file
./bin/ktms-node --proxy-addr proxy.example.com:443 --tls \
  --engine ollama --expose=cloudflared    # needs cloudflared 2026.10.3 or newer
```

With `--expose=cloudflared` the agent starts a localhost gateway in front of the engine and a
Cloudflare quick tunnel in front of the gateway, registers the tunnel address, and registers
again whenever the tunnel restarts and its address changes. The gateway checks a per-process
access key (sent to the proxy as the node auth token, never printed) and forwards only
`POST /v1/chat/completions` and `GET /v1/models`; pulling or deleting models is not reachable.
Slow responses are kept alive with padding so the tunnel's 100 s idle cutoff does not drop them.
Quick tunnels have no SLA; for a stable address use `--expose=none` and your own `--endpoint`.
cloudflared older than 2026.10.3 trusts the eMudhra intermediates whose keys leaked in October
2026: the agent warns about it at start and `ktms-node doctor` reports it; re-running
`install-node.sh` updates it.

Flags: `--proxy-addr` (default `127.0.0.1:50051`), `--tls` (dial over TLS with the system CA
roots, for a proxy behind a TLS load balancer; off by default), `--node-id` (defaults to the
hostname), `--node-name`, `--engine` (`ollama`, `vllm`, `tgi`, `llamacpp`, `freetoken`, `decisions`, `generic`),
`--base-weight`, `--max-concurrent`.
`--judge-model` (repeat or comma-separate) tags specific models on the node with the `judge` capability
for local JEPA / decision evaluation, routing `/v1/decisions` and `Judge` calls to them.
`--only-model` (repeat it or comma-separate) reports only the named models to the proxy, for an
engine that shares its model directory with other tools; by default every model the engine has is
reported. If the node key restricts `allowed_models`, the proxy ignores (and logs) other reported
models and registers the node with the allowed ones; it refuses only when none is allowed.
`--engine-url` is where the agent itself polls the engine (default `http://127.0.0.1:11434`);
`--endpoint` is the address reported to the proxy, which dials it (default: `--engine-url`).
`--expose` is `none` (default; you supply `--endpoint`) or `cloudflared`, with
`--cloudflared-path` to pick the binary. `NODE_AUTH_TOKEN` is the bearer the engine itself
requires, if any.

The agent keeps a usage log for the machine's owner in `--stats-dir` (default
`~/.ktms-node/stats`, or `$KTMS_HOME/stats`): every minute it pulls the requests the proxy sent
here with `PullNodeUsage` (time, model, tokens, outcome, level; no user, key or text) into
`requests-YYYY-MM-DD.jsonl`, and writes its own sample (schedule level, state, proxy and engine
load, GPU) to `samples-YYYY-MM-DD.jsonl`. Closed days are gzipped and kept for
`--stats-retention` (default `90d`, `0` = forever); `--stats=false` turns it off. Against a
proxy without the log it keeps only the samples. `doctor` checks that the log moves.

The owner reads it with the same binary, offline and without the node key:

```bash
ktms-node stats                      # last 24 hours by hour
ktms-node stats --since 7d           # by day
ktms-node stats --since 2026-10-01 --by model --format csv
ktms-node stats --tail               # requests as they arrive
```

Columns: requests, errors (the node failed or timed out; refused and abandoned requests are not
counted), tokens in and out, busy (the share of time at least one request ran;
parallel requests count once), cooldown given, paused (the share of minutes at 0%), minutes
without data (the agent was not running), TTFT p50/p95 and output tokens per second. Stretches
the proxy no longer had when the agent pulled are listed as missing. `--format json|csv`,
`--until`, and `--dir` for a log elsewhere.

If a node does not show up or serves badly, `ktms-node doctor` takes the same flags (plus
`--model`) and says in plain words what is wrong: engine not answering, model missing or not
loaded, Ollama on the CPU (an Intel build under Rosetta on Apple Silicon), proxy unreachable or
needing/not needing `--tls`, a node key that is wrong or bound to another node id, or a tunnel
that cannot come up. It also asks the proxy to vet the exact models the agent would report
against the key's `allowed_models` (nothing is stored). It exits 1 if it finds a problem.

A node on someone's own machine can follow a load schedule in local time with
`--schedule-file FILE`, one rule per line:

```
# [days] HH:MM-HH:MM level
23:00-07:00 100
07:00-10:00 50
mon-fri 10:00-23:00 0
```

Days are `mon`..`sun`, ranges (`mon-fri`) and lists (`sat,sun`); without them a rule applies daily.
A window past midnight belongs to the day it starts on, `24:00` ends a day, uncovered time is 100%,
and overlapping rules are an error. The level is the share of time the node spends on proxy
requests: the proxy gives it `ceil(max_concurrent × level)` slots and, where that is more than the
level, holds the slot idle after each request (one slot at 50%: a 20 s request, then 20 s idle).
At 0% the proxy sends nothing, the gateway answers 503 after a 5 s grace, and once the proxy has no
requests left on the node the agent unloads the served models from Ollama; it loads them again
`--warmup` (default 5m) before the next window. The tunnel and heartbeats keep running, so a pause
ends at once. The agent reads the file every 30 s and on a change, keeps the last valid schedule if
an edit breaks it, and runs at 100% without the file. A proxy that predates capacity levels
routes at 100% (the agent logs a warning). `ktms-node schedule --file FILE` checks a
file and prints the level now and the next switch; `doctor` reports it too. Engines other than
Ollama only stop being routed to.

The owner can override the schedule for a while from the same machine:

```
ktms-node status                     # level, where it comes from, requests, models, next switch
ktms-node pause 10m                  # 0% for 10 minutes (default 10m), then the schedule again
ktms-node pause --until-next         # until the schedule's next switch (at most 24 h)
ktms-node pause 10m --now            # also cut the requests running now; their callers get an error
ktms-node level 30 --for 1h          # 30% for an hour
ktms-node resume                     # back to the schedule now
```

A manual level lasts from 1 minute to 24 hours, ends by the wall clock (a Mac that slept past the
end resumes on waking) and survives an agent restart (`~/.ktms-node/override.json`, see
`--state-dir`). A manual pause does not warm the models before it ends, so the memory stays free
for the owner; they load right after it, and the proxy falls back meanwhile (`--warm` keeps the
schedule's behaviour). These commands, and the desktop tray, talk to the running agent over a Unix
socket, `--control-socket` (default `~/.ktms-node/control.sock`, empty turns it off). It is
owner-only: mode 0600, and a caller running as another user is refused. Its HTTP/JSON API
(`GET /v1/status`, `GET /v1/events` as server-sent events, `POST`/`DELETE /v1/override`,
`GET`/`PUT /v1/schedule`, `POST /v1/schedule/preview`, `GET /v1/stats`, `GET`/`PUT /v1/settings`)
never returns the node key or any token.

**Desktop tray.** `install-node.sh --tray` adds `ktms-node-tray`: a menu bar app on macOS
(`~/Applications/Keystone Node.app`, started at login, no Dock icon) or a tray icon on Linux
(StatusNotifierItem; GNOME needs the AppIndicator extension). The icon is a circle filled to the level
(the exact percentage and where it comes from are in the menu and tooltip), two bars with a
countdown during a manual pause, and a warning when the agent or KTMS does not answer. Each request
the node is serving, on any engine, is a small square beside it: a new one flies in from the right,
one that ends leaves upward from its own place and the rest close up; they stack in two rows, and
past four run off the icon's edge. The shape stays small for 3 seconds after the last request, so
short gaps between requests do not make it jump. The count is the status's `requests.inflight`, and
`requests.running` lists the ids of the oldest eight, both live from the node's gateway; without
the gateway the count is the engine's at the last heartbeat. Its menu pauses for 5, 10 or 30 minutes or until the next switch, limits the node to 50, 30
or 10% for an hour, cuts running requests, resumes the schedule and turns "Schedule set by KTMS" on
or off. Its window, a page served on `127.0.0.1` that only the tray's one-time link opens, edits the
schedule as a week grid or as text with a live preview, shows the usage log for 24 hours, 7 and 30
days, and turns notifications (pause over, agent silent, schedule invalid) on or off. The tray is a
client of the control socket and keeps no state of its own beyond `~/.ktms-node/tray.json`;
quitting it leaves the node running.

**Schedule set by KTMS.** With `--schedule-source=ktms`, or `ktms-node settings
--schedule-source=ktms` (the tray's "Schedule set by KTMS") at run time, the node takes its
schedule and load from KTMS and ignores its local file, which stays in place for when the setting
is turned off. KTMS keeps one schedule per node id (`ktms nodes schedule set|show|list|delete`,
the `node_schedules` file section, `SetNodeSchedule` in the Admin API), validated with the agent's
parser, plus an optional override of 1 minute to 24 hours (`ktms nodes schedule override`,
`SetNodeCapacityOverride`). It reaches the node with a heartbeat within about 15 seconds (KTMS
re-reads a node's schedule at most every 10 s). The node stores the last
one it got in `~/.ktms-node/managed-schedule`, so a restart without the proxy keeps it; before the
first one it runs at 100%. The owner's own pause or level on the machine still wins over both.
`ListNodes` shows each node's `schedule_source`, `capacity_source` and `capacity_until`.

While the proxy refuses the node (wrong key, other node id, no allowed model) the agent does not
exit and does not open a tunnel: it checks with the proxy first, then retries with a pause that
grows to five minutes and says why in the log, so a supervisor never restarts it into a fresh
Cloudflare tunnel per attempt.

### FreeToken

[FreeToken](https://github.com/FlashML-org/FreeToken) runs large MoE models on one consumer
NVIDIA RTX (30 to 50 series, Linux or Windows; not on a Mac, AMD or Intel): expert weights stay in
RAM and the GPU caches the experts in use. `ft serve` runs one model per process:

```bash
export FREETOKEN_API_KEY=...              # optional; ft serve and the agent both read it
ft serve --model <hf-id or path> --served-model-name qwen3.6-35b
./bin/ktms-node --proxy-addr proxy.example.com:443 --tls --engine freetoken
```

With `--engine freetoken` the agent polls `http://127.0.0.1:1919`, FreeToken's default, and
sends `FREETOKEN_API_KEY` as its bearer; the proxy uses it too unless `NODE_AUTH_TOKEN` is set. The model is
reported with its context length from `/v1/models` and as loaded once `/health` says `ok`; while
the weights load it is reported cold, and a server whose `/health` says `error` reports no models.
The load comes from `requests.active` in `/v1/stats`, which counts every admitted request: those
above `--engine-max-running` (FreeToken's `--max-running-requests`, default 4) count as waiting, so
a node busy with other clients hands calls to the next model in the chain. `--max-concurrent`,
what the proxy sends here, defaults to 4 too. FreeToken rejects `response_format`, so its nodes get
the schema in the prompt; declare `no_structured_output` on the model as well, and the router
checks the reply and retries a wrong shape. `no_thinking` sends `reasoning_effort: none`.
`ktms-node doctor` checks that `ft` is installed, the server answers and takes the key, and the
model is loaded. `install-node.sh` installs Ollama only; install FreeToken yourself.

## Run a node

Lend a machine's GPU to the proxy: an Apple Silicon Mac, or Linux with an NVIDIA card (or just a
CPU, slowly). Three steps; no tunnels or service files to write by hand, and no admin rights on macOS.

**1. Get a key.** Whoever runs the proxy mints one key per machine, bound to a node id, and gives
you the key (`ktms_node_...`), the node id and the proxy address. They also need a `local` model
whose `id` is exactly the Ollama tag the node serves (`G` and `ADMIN` as in the quick start;
drop `-plaintext` for a proxy behind TLS):

```bash
# admin, once per machine
$G "$ADMIN" -d '{"name":"anna-mac","node_id":"anna-mac"}' \
  proxy.example.com:443 ktms.v1.AdminService/MintNodeKey      # plain_key shown once

# admin, once per model; no_thinking turns Gemma's reasoning off
$G "$ADMIN" -d '{"model":{"id":"gemma4:26b-a4b-it-qat","vendor":"local",
  "efforts":["EFFORT_LOW","EFFORT_MEDIUM"],"capabilities":["no_thinking"],
  "queue_timeout_ms":2000,"enabled":true}}' \
  proxy.example.com:443 ktms.v1.AdminService/UpsertModel
```

**2. Run one command** on the machine (replace the three values), and paste the key when asked.
The input is hidden and the key is stored in a file only you can read:

```bash
curl -fsSL https://raw.githubusercontent.com/ai-process/ktms/vX.Y.Z/scripts/install-node.sh | sh -s -- \
  --proxy proxy.example.com --node-id anna-mac --model gemma4:26b-a4b-it-qat
```

It downloads Ollama, the model and the node agent, starts them as background services and keeps
the machine awake, and waits until the node has registered. Linux services start at boot. On a
Mac they are login items: after a reboot the node comes back when its user logs in, so turn on
automatic login for an unattended Mac. The model is large, so the first run takes a while. Add
`--max-concurrency N` for a machine that can serve more than one request at a time (default 1).

**3. Check.** The last lines say `Node anna-mac is running and registered`. The proxy admin
confirms with `ListNodes` that the node is `HEALTHY` and has the model.

To keep the machine free while its owner works, add `--schedule FILE` with load levels by local
time (format above, under the node agent). The installer checks the file and copies it to
`~/.ktms-node/schedule`; edit that file later and the agent picks it up, no re-run needed. The
last lines then show the level now and the next switch (`Schedule: now 0%, next 23:00 100%`), and
during a pause the installer does not load the model into memory. The `Stats:` line gives the
command that shows how the machine was used (`~/.ktms-node/bin/ktms-node stats`).

Add `--tray` for the menu bar (macOS) or tray (Linux) app that pauses the node for a while and
edits its schedule without touching the file (see Desktop tray above). A re-run without `--tray`
keeps it.

Run the same command again to update; a re-run only restarts what changed, and a new agent that
fails to start is rolled back. Remove everything with the same command plus `--uninstall`
(downloaded models stay in `~/.ollama`).

**A machine that ran the node before the rename** (`llm-proxy-node`, in `~/.llm-proxy-node`) is
taken over by the same command. Its node key is reused when it was minted for the same
`--node-id` (for another id you are asked first), and its Ollama build instead of a download.
Once everything else is in place, right before the new Ollama starts, its services
(`com.llm-proxy.*` launch agents, `llm-proxy-*` user units) are stopped for good and their files
and the old directory moved to `~/.ktms-node.backup/<time>/`; an install that fails before that
leaves the old node running. `--dry-run` shows this plan and changes nothing; `--keep-legacy`
leaves the old directory in place, though its services still go, or two nodes would share one id
and port. `--uninstall` retires old services too; backups survive it unless you add
`--purge-backups`.

The installer also refuses to run Ollama on an address another process already listens on
(another Ollama, say), since the model would be pulled into that one; it names the process. Stop
it, or set `KTMS_OLLAMA_ADDR=127.0.0.1:PORT`.

### A translation node

`--engine mt` installs [ktms-mt](../mt/README.md) instead of Ollama: a Python venv with
CTranslate2, the Opus-MT models converted to int8, and a service on `127.0.0.1:11436`; the node
agent then runs with `--engine mt`. Its models declare only `translate`, so they serve `Translate`
and never a chat. Python 3.11–3.13 must be installed.

```bash
curl -fsSL https://raw.githubusercontent.com/ai-process/ktms/vX.Y.Z/scripts/install-node.sh | sh -s -- \
  --proxy proxy.example.com --node-id anna-mt --engine mt --model opus-mt-de
```

The admin registers the model like any local one, with its pairs:

```yaml
- id: opus-mt-de
  vendor: local
  efforts: [EFFORT_LOW]
  capabilities: [translate]
  languages: {source: ["*"], target: [de]}
```

`opus-mt-de` translates English, French, Spanish and Italian with direct Opus-MT pairs and every
other language through a multilingual model; `ktms-mt catalog` lists the others (single pairs,
M2M100, MADLAD-400). Any server that speaks the same `/v1/models` and `/v1/translate` contract
can take its place behind `--engine mt`. When the node stops, the translate rule falls through to
the next model of its chain, a chat model translating by prompt.

**Chat and translation on one machine.** `--engine ollama,mt` runs both engines behind one node:
one key, one tunnel, one schedule and one tray. Ollama stays the primary engine, and the node's
gateway sends each request to the engine of its model. KTMS counts the node's requests together, so the
installer gives such a node two slots by default and Ollama one of them: a long chat does not hold
up translation.

```bash
curl -fsSL https://raw.githubusercontent.com/ai-process/ktms/vX.Y.Z/scripts/install-node.sh | sh -s -- \
  --proxy proxy.example.com --node-id anna --engine ollama,mt --model gemma4:26b-a4b-it-qat --mt-model opus-mt-de
```

The agent behind it runs `--engine ollama --extra-engine mt=http://127.0.0.1:11436`. An engine that
stops leaves the others serving. The owner can pause one engine and keep the rest: `ktms-node
engine pause mt` (or the engine's line in the tray) stops offering its models to KTMS until
`ktms-node engine resume mt`, and the pause survives a restart. Pausing the node pauses all its
engines; the schedule unloads and warms only Ollama's models, since ktms-mt keeps its small models
loaded.

Measured on an Apple M1 Pro (10 cores, 32 GB), `opus-mt-de`, int8 on the CPU, greedy decoding
(`beam_size = 1`, the default), hotel reviews of 183 characters on average in English, French,
Spanish, Italian and Dutch:

| Path | Reviews per minute | Characters per second |
| --- | --- | --- |
| `ktms-mt bench`, batches of 128 | 1,869 | 5,670 |
| Through Keystone: `ktms translate`, 128 texts per call, one client | 1,475 | 4,480 |
| The same, two clients at once | 1,097 | 3,330 |
| `ktms-mt bench`, `beam_size = 2` | 900 | 2,730 |
| `ktms-mt bench`, `device = "mps"` (CTranslate2's experimental Apple GPU backend), beam 2 | 536 | 1,630 |

Throughput follows characters: reviews of 400 characters go at about 670 a minute through
Keystone. One stream of large batches is fastest; CTranslate2 already uses every core.

### If the node does not show up

Run the same command with `--doctor`. It says in plain words what is wrong: Ollama not answering,
the model missing or not loaded, Ollama on the CPU (an Intel build running through Rosetta), the
proxy unreachable or `--tls` wrong, a key that is wrong or belongs to another node id, or a tunnel
that cannot come up (outbound port 7844 blocked). Logs are in `~/.ktms-node/logs/`.

| Symptom | Cause and fix |
| --- | --- |
| `Killed: 9`, or "Apple could not verify..." | The agent came through a browser download, which macOS quarantines. Use the `curl` command above; `curl` and `brew` files are not quarantined. |
| Very slow, GPU idle | Intel Ollama through Rosetta, or no NVIDIA driver. The installer uses its own native Ollama and says when it ignores an Intel one. |
| `key belongs to node "x"` | The key is bound to one machine. Use the node id it was minted for, or ask for a new key. |
| `model "x" not permitted for this node key` | The key's `allowed_models` excludes a model on the machine. The installer reports only `--model`; for a hand-run agent pass `--only-model`. |
| Needs a new key | Re-run with `--new-key`. |
| `127.0.0.1:11435 is taken by …` | Another process holds the node's Ollama port. Stop it, or pick another address with `KTMS_OLLAMA_ADDR`. |
| Address changed after a restart | Expected: quick tunnel addresses change, and the node re-registers by itself. |
| Node is offline after a Mac reboot | macOS starts the services at login, not at boot. Log in, or enable automatic login (System Settings > Users & Groups). |
| The machine sleeps | The services keep it awake while they run, on a Mac only while plugged in. Closing a laptop lid still sleeps it. |

Installs on Linux need `zstd` (Ubuntu: `sudo apt install zstd`) and a normal login session
(systemd user services). The installer script's other options are in `install-node.sh --help`.
