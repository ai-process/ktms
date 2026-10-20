#!/usr/bin/env bash
# SUT-side helper for run_all.sh. Runs next to docker-compose.bench.yaml, either
# locally or on the SUT VM over SSH, so timing and cgroup reads never cross the WAN.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
if [[ -f .env ]]; then set -a; . ./.env; set +a; fi
BIND_ADDR="${BIND_ADDR:-127.0.0.1}"
GATEWAYS=(ktms litellm)

compose() { docker compose -f docker-compose.bench.yaml "$@"; }

port_of() {
  case "$1" in
    ktms) echo 8080 ;; litellm) echo 4000 ;; mock-upstream) echo 8000 ;;
    *) echo "unknown service $1" >&2; return 1 ;;
  esac
}

ready_url() {
  local path=/readyz
  [[ "$1" == litellm ]] && path=/health/readiness
  echo "http://${BIND_ADDR}:$(port_of "$1")${path}"
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
# first 200 from its readiness endpoint. Container creation is excluded.
cold_start() {
  compose rm -sf "$1" >/dev/null 2>&1
  compose create "$1" >/dev/null 2>&1
  local cid; cid="$(compose ps -aq "$1")"
  python3 - "$cid" "$(ready_url "$1")" "${READY_TIMEOUT_S:-300}" <<'PY'
import subprocess, sys, time, urllib.request
cid, url, timeout = sys.argv[1], sys.argv[2], int(sys.argv[3])
t0 = time.monotonic()
subprocess.run(["docker", "start", cid], check=True, stdout=subprocess.DEVNULL)
while time.monotonic() - t0 < timeout:
    try:
        with urllib.request.urlopen(url, timeout=1) as r:
            if r.status == 200:
                print(int((time.monotonic() - t0) * 1000))
                sys.exit(0)
    except Exception:
        pass
    time.sleep(0.005)
sys.exit(f"{url} not ready after {timeout}s")
PY
}

# Prints the service's cgroup v2 counters as one JSON object (bytes, microseconds).
metrics() {
  compose exec -T "$1" sh -c 'cd /sys/fs/cgroup && cat memory.stat cpu.stat &&
    echo "current $(cat memory.current)" && echo "peak $(cat memory.peak 2>/dev/null || echo 0)"' |
    awk '$1=="anon"{a=$2} $1=="usage_usec"{u=$2} $1=="current"{c=$2} $1=="peak"{p=$2}
      END{if(u==""){exit 1}; printf "{\"anon\":%d,\"current\":%d,\"peak\":%d,\"cpu_usec\":%d}\n",a,c,p,u}'
}

# One real request through the target; fails loudly instead of benchmarking errors.
smoke() {
  local svc=$1 key=$2 before after body
  before="$(curl -sf "http://${BIND_ADDR}:8000/stats")"
  body="$(curl -sS -w '\n%{http_code}' "http://${BIND_ADDR}:$(port_of "$svc")/v1/chat/completions" \
    -H "Authorization: Bearer ${key}" -H 'Content-Type: application/json' \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"smoke [mock latency_ms=0]"}]}')"
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
for svc in ("mock-upstream", "ktms", "litellm"):
    # Built services have no image field; compose names them <project>-<service>.
    img = cfg["services"][svc].get("image") or f"{project}-{svc}"
    size = subprocess.run(["docker", "image", "inspect", "-f", "{{.Size}}", img], capture_output=True, text=True).stdout.strip()
    images[svc] = {"image": img, "bytes": int(size or 0)}
print(json.dumps({"cpus": info["NCPU"], "mem_bytes": info["MemTotal"], "arch": info["Architecture"],
                  "kernel": info["KernelVersion"], "docker": info["ServerVersion"], "images": images}))'
}

case "${1:-}" in
  build) compose build -q mock-upstream ktms && compose pull -q db redis litellm ;;
  up-deps) compose up -d --wait db redis mock-upstream >/dev/null ;;
  stop-gateways) compose rm -sf "${GATEWAYS[@]}" >/dev/null 2>&1 ;;
  prepare-ktms) prepare_ktms ;;
  cold-start) cold_start "$2" ;;
  metrics) metrics "$2" ;;
  # Checked from the image, before start: some arm64 variants ship amd64 binaries (emulated).
  arch) printf '"%s"\n' "$(compose run --rm --no-deps -T --entrypoint uname "$2" -m 2>/dev/null | tr -d '\r')" ;;
  smoke) smoke "$2" "$3" ;;
  upstream-stats) curl -sf "http://${BIND_ADDR}:8000/stats" ;;
  env) environment ;;
  down) compose --profile k6 down -v --remove-orphans ;;
  *) echo "usage: $0 build|up-deps|stop-gateways|prepare-ktms|cold-start SVC|metrics SVC|arch SVC|smoke SVC KEY|upstream-stats|env|down" >&2; exit 2 ;;
esac
