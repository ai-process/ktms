#!/usr/bin/env bash
# SUT-side helper for run_all.sh. Runs next to docker-compose.bench.yaml, either
# locally or on the SUT VM over SSH, so timing and cgroup reads never cross the WAN.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
if [[ -f .env ]]; then set -a; . ./.env; set +a; fi
BIND_ADDR="${BIND_ADDR:-127.0.0.1}"
read -r -a GATEWAYS <<<"$(scripts/gw.py names)"
gw() { scripts/gw.py "$@"; }

compose() { docker compose -f docker-compose.bench.yaml "$@"; }

port_of() {
  if [[ "$1" == mock-upstream ]]; then echo 8000; else gw "$1" port; fi
}

ready_url() {
  echo "http://${BIND_ADDR}:$(port_of "$1")$(gw "$1" ready)"
}

wait_ready() {
  local url; url="$(ready_url "$1")"
  for _ in $(seq 1 600); do
    curl -sf -o /dev/null "$url" && return 0
    sleep 0.2
  done
  echo "$1 not ready at $url" >&2
  compose logs --tail 50 "$1" >&2
  return 1
}

# Points every seeded OpenAI model at the mock and lifts RPM/token limits,
# which would otherwise throttle the benchmark itself.
prepare_ktms() {
  compose up -d ktms >/dev/null
  wait_ready ktms
  local updated
  updated="$(compose exec -T db psql -U ktms -d ktms -v ON_ERROR_STOP=1 -tA -c "
    WITH u AS (
      UPDATE model SET endpoint = 'http://mock-upstream:8000/v1/', rpm = 0,
             daily_tokens_per_key = 0, daily_tokens_per_user = 0, updated_at = NOW()
      WHERE vendor = 'openai' RETURNING 1
    ), b AS (
      UPDATE config_state SET version = version + 1, updated_at = NOW() WHERE id = 1 RETURNING 1
    )
    SELECT (SELECT count(*) FROM u) || ' ' || (SELECT count(*) FROM b);")"
  if [[ "$updated" != *" 1" || "$updated" == "0 "* ]]; then
    echo "prepare_ktms: unexpected update result '$updated'" >&2
    return 1
  fi
  compose stop ktms >/dev/null
}

# Prints milliseconds from `docker start` of a freshly created container to the
# first successful chat completion through it. Health endpoints are not used: on
# some gateways they turn green before routes are loaded.
cold_start() {
  compose rm -sf "$1" >/dev/null 2>&1
  compose create "$1" >/dev/null 2>&1
  local cid; cid="$(compose ps -aq "$1")"
  python3 - "$cid" "http://${BIND_ADDR}:$(port_of "$1")$(gw "$1" path)" "$(gw "$1" key)" "$(gw "$1" model)" \
    "$(gw "$1" headers)" "${READY_TIMEOUT_S:-300}" <<'PY'
import json, subprocess, sys, time, urllib.request
cid, url, key, model, headers, timeout = sys.argv[1:6] + [int(sys.argv[6])]
body = json.dumps({"model": model, "messages": [{"role": "user", "content": "ready [mock latency_ms=0]"}]}).encode()
hdrs = {"Authorization": f"Bearer {key}", "Content-Type": "application/json", **json.loads(headers)}
t0 = time.monotonic()
subprocess.run(["docker", "start", cid], check=True, stdout=subprocess.DEVNULL)
while time.monotonic() - t0 < timeout:
    try:
        with urllib.request.urlopen(urllib.request.Request(url, body, hdrs), timeout=1) as r:
            if r.status == 200 and json.load(r).get("choices"):
                print(int((time.monotonic() - t0) * 1000))
                sys.exit(0)
    except Exception:
        pass
    time.sleep(0.005)
sys.exit(f"{url} not serving after {timeout}s")
PY
}

# Checked from the image, before start: some arm64 variants ship amd64 binaries (emulated).
# Distroless images have no uname, so those fall back to the image's declared platform.
arch_of() {
  local m
  m="$(compose run --rm --no-deps -T --entrypoint uname "$1" -m 2>/dev/null | tr -d '\r')" || m=
  if [[ -z "$m" ]]; then
    m="$(docker image inspect -f '{{.Architecture}}' "$(compose config --images "$1" | head -1)" 2>/dev/null)"
    m="${m/amd64/x86_64}" m="${m/arm64/aarch64}"
  fi
  printf '"%s"\n' "$m"
}

up_deps() {
  local deps=() g d
  for g in "${GATEWAYS[@]}"; do
    for d in $(gw "$g" deps); do deps+=("$d"); done
  done
  compose up -d --wait mock-upstream "${deps[@]}" >/dev/null
}

# Bifrost downloads pricing datasheets at boot and fails without them; serving
# local copies keeps the network out of cold start.
prepare_bifrost() {
  local run=gateways/bifrost/run
  mkdir -p "$run" && chmod 777 "$run"
  cp gateways/bifrost/config.json "$run/config.json"
  [[ -s "$run/pricing.json" ]] || curl -fsSL https://getbifrost.ai/datasheet -o "$run/pricing.json"
  [[ -s "$run/model-parameters.json" ]] ||
    curl -fsSL https://getbifrost.ai/datasheet/model-parameters -o "$run/model-parameters.json"
}

# Higress keeps its control-plane state in the mounted dir, so each run starts from seed.
prepare_higress() {
  rm -rf gateways/higress/run
  cp -R gateways/higress/seed gateways/higress/run
  chmod -R a+rwX gateways/higress/run
}

# New API is configured through its admin API: a channel to the mock, the
# overload guard and per-request consume logs off (KTMS records no usage
# here either), and a token whose key is then pinned to the one in gateways.json.
prepare_newapi() {
  compose exec -T db psql -U ktms -d ktms -qc 'DROP DATABASE IF EXISTS newapi' -c 'CREATE DATABASE newapi' >/dev/null
  compose exec -T redis redis-cli -n 1 FLUSHDB >/dev/null
  compose up -d newapi >/dev/null
  wait_ready newapi
  local b="http://${BIND_ADDR}:3000" p='BenchRoot#2026' t key
  key="$(gw newapi key)"
  ok() { python3 -c "import json,sys; d=json.load(sys.stdin); assert d.get('success'), d; print($1)"; }
  curl -s "$b/api/setup" -H 'Content-Type: application/json' -d "{\"username\":\"root\",\"password\":\"$p\",
    \"confirmPassword\":\"$p\",\"SelfUseModeEnabled\":true,\"DemoSiteEnabled\":false}" | ok '""' >/dev/null
  t="$(curl -s "$b/api/user/login" -H 'Content-Type: application/json' \
    -d "{\"username\":\"root\",\"password\":\"$p\"}" | ok 'd["data"]["access_token"]')"
  local h=(-H "Authorization: Bearer $t" -H "New-Api-User: 1" -H 'Content-Type: application/json')
  for opt in performance_setting.monitor_enabled LogConsumeEnabled; do
    curl -s -X PUT "${h[@]}" "$b/api/option/" -d "{\"key\":\"$opt\",\"value\":\"false\"}" | ok '""' >/dev/null
  done
  # base_url has no /v1: the OpenAI channel appends /v1/chat/completions itself.
  curl -s "${h[@]}" "$b/api/channel/" -d '{"mode":"single","channel":{"type":1,"name":"mock","key":"mock-key",
    "base_url":"http://mock-upstream:8000","models":"gpt-4o-mini","group":"default","status":1}}' | ok '""' >/dev/null
  curl -s "${h[@]}" "$b/api/token/" -d '{"name":"bench","remain_quota":0,"unlimited_quota":true,"expired_time":-1}' |
    ok '""' >/dev/null
  compose exec -T db psql -U ktms -d newapi -v ON_ERROR_STOP=1 -qc \
    "UPDATE tokens SET key = '${key#sk-}' WHERE name = 'bench'" >/dev/null
  # Stopped here; cold-start restarts it, which also drops the in-memory caches.
  compose stop newapi >/dev/null
}

# Prints the service's cgroup v2 counters as one JSON object (bytes, microseconds).
# Reads from the host so the probe is not charged to the measured cgroup; Docker
# Desktop hides host cgroups, so there it falls back to exec inside the container.
metrics() {
  local cid dir
  cid="$(docker ps -q --no-trunc --filter label=com.docker.compose.project=ktms-bench \
    --filter "label=com.docker.compose.service=$1" | head -1)"
  [[ -n "$cid" ]] || { echo "metrics: $1 is not running" >&2; return 1; }
  for dir in "/sys/fs/cgroup/system.slice/docker-${cid}.scope" "/sys/fs/cgroup/docker/${cid}" ""; do
    [[ -z "$dir" || -r "$dir/memory.stat" ]] && break
  done
  if [[ -n "$dir" ]]; then
    cat "$dir/memory.stat" "$dir/cpu.stat"
    echo "current $(cat "$dir/memory.current")"
    echo "peak $(cat "$dir/memory.peak" 2>/dev/null || echo 0)"
  else
    docker exec "$cid" sh -c 'cd /sys/fs/cgroup && cat memory.stat cpu.stat &&
      echo "current $(cat memory.current)" && echo "peak $(cat memory.peak 2>/dev/null || echo 0)"'
  fi | awk '$1=="anon"{a=$2} $1=="usage_usec"{u=$2} $1=="current"{c=$2} $1=="peak"{p=$2}
    END{if(u==""){exit 1}; printf "{\"anon\":%d,\"current\":%d,\"peak\":%d,\"cpu_usec\":%d}\n",a,c,p,u}'
}

# One real request through the target; fails loudly instead of benchmarking errors.
smoke() {
  local svc=$1 before after body hdrs=()
  while IFS= read -r h; do [[ -n "$h" ]] && hdrs+=(-H "$h"); done < <(gw "$svc" headers |
    python3 -c 'import json,sys; [print(f"{k}: {v}") for k, v in json.load(sys.stdin).items()]')
  before="$(curl -sf "http://${BIND_ADDR}:8000/stats")"
  body="$(curl -sS -w '\n%{http_code}' "http://${BIND_ADDR}:$(port_of "$svc")$(gw "$svc" path)" \
    -H "Authorization: Bearer $(gw "$svc" key)" -H 'Content-Type: application/json' ${hdrs[@]+"${hdrs[@]}"} \
    -d "{\"model\":\"$(gw "$svc" model)\",\"messages\":[{\"role\":\"user\",\"content\":\"smoke [mock latency_ms=0]\"}]}")"
  after="$(curl -sf "http://${BIND_ADDR}:8000/stats")"
  if [[ "${body##*$'\n'}" != 200 || "$before" == "$after" ]]; then
    echo "smoke $svc failed: ${body}" >&2
    return 1
  fi
}

environment() {
  compose config --format json | python3 -c '
import json, subprocess, sys
cfg = json.load(sys.stdin)
info = json.loads(subprocess.run(["docker", "info", "-f", "{{json .}}"], capture_output=True, check=True).stdout)
images, project = {}, cfg["name"]
for svc in ["mock-upstream"] + sys.argv[1].split():
    # Built services have no image field; compose names them <project>-<service>.
    img = cfg["services"][svc].get("image") or f"{project}-{svc}"
    size = subprocess.run(["docker", "image", "inspect", "-f", "{{.Size}}", img], capture_output=True, text=True).stdout.strip()
    images[svc] = {"image": img, "bytes": int(size or 0)}
print(json.dumps({"cpus": info["NCPU"], "mem_bytes": info["MemTotal"], "arch": info["Architecture"],
                  "kernel": info["KernelVersion"], "docker": info["ServerVersion"], "images": images}))' "${GATEWAYS[*]}"
}

case "${1:-}" in
  build) compose build -q && compose pull -q --ignore-buildable ;;
  up-deps) up_deps ;;
  stop-gateways) compose rm -sf "${GATEWAYS[@]}" >/dev/null 2>&1 ;;
  prepare-ktms) prepare_ktms ;;
  prepare-bifrost) prepare_bifrost ;;
  prepare-higress) prepare_higress ;;
  prepare-newapi) prepare_newapi ;;
  cold-start) cold_start "$2" ;;
  metrics) metrics "$2" ;;
  arch) arch_of "$2" ;;
  smoke) smoke "$2" ;;
  upstream-stats) curl -sf "http://${BIND_ADDR}:8000/stats" ;;
  env) environment ;;
  down) compose --profile k6 down -v --remove-orphans ;;
  *) echo "usage: $0 build|up-deps|stop-gateways|prepare-ktms|prepare-bifrost|prepare-higress|prepare-newapi|cold-start SVC|metrics SVC|arch SVC|smoke SVC|upstream-stats|env|down" >&2; exit 2 ;;
esac
