#!/usr/bin/env bash
# Benchmarks the direct baseline and every gateway in gateways.json, one at a time,
# into results/<run-id>/REPORT.md.
# Local: [PROFILE=quick] ./scripts/run_all.sh. Cloud: run_cloud.sh sets SUT_SSH/RUNNER_SSH/SUT_IP.
set -euo pipefail

BENCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROFILE="${PROFILE:-full}"
TARGETS="${TARGETS:-direct $("${BENCH_DIR}/scripts/gw.py" names)}"
RUN_ID="${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-${PROFILE}}"
OUT="${BENCH_DIR}/results/${RUN_ID}"
SUT_SSH="${SUT_SSH:-}"
RUNNER_SSH="${RUNNER_SSH:-}"
SUT_IP="${SUT_IP:-}"
REMOTE_DIR="${REMOTE_DIR:-/opt/bench}"
read -r -a SSH_OPTS <<<"${SSH_OPTS:-}"
K6_IMAGE=grafana/k6:2.3.0

case "$PROFILE" in
  quick)
    : "${LAT_RATE:=100}" "${LAT_DURATION:=20s}" "${STREAM_VUS:=20}" "${STREAM_DURATION:=20s}"
    : "${SOAK_RATE:=100}" "${SOAK_DURATION:=60s}" "${SAT_STEPS:=100,250,500,1000}"
    : "${SAT_HOLD_S:=10}" "${SAT_RAMP_S:=3}" "${SETTLE_S:=5}" "${COOLDOWN_S:=15}" ;;
  full)
    : "${LAT_RATE:=200}" "${LAT_DURATION:=60s}" "${STREAM_VUS:=50}" "${STREAM_DURATION:=60s}"
    : "${SOAK_RATE:=200}" "${SOAK_DURATION:=5m}" "${SAT_STEPS:=250,500,1000,1500,2000,3000,4000}"
    : "${SAT_HOLD_S:=20}" "${SAT_RAMP_S:=5}" "${SETTLE_S:=10}" "${COOLDOWN_S:=30}" ;;
  *) echo "PROFILE must be quick or full" >&2; exit 2 ;;
esac

log() { printf '\n[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }

sut() {
  if [[ -n "$SUT_SSH" ]]; then
    ssh "${SSH_OPTS[@]}" "$SUT_SSH" "${REMOTE_DIR}/bench/scripts/sut.sh $*"
  else
    "${BENCH_DIR}/scripts/sut.sh" "$@"
  fi
}

# k6 runs as the invoking user so it can write summaries into the results dir. Exit 99
# (thresholds crossed) is a measured result, e.g. errors under load; anything else aborts.
k6run() {
  local name=$1 script=$2; shift 2
  local envs=(-e "TARGET=${target}" -e "BASE_URL=${base}" -e "API_KEY=${key}" -e "MODEL=${model}"
    -e "CHAT_PATH=${path}" -e "HEADERS=${headers}"
    -e "SUMMARY_PATH=/results/${RUN_ID}/${target}.${name}.json")
  for kv in "$@"; do envs+=(-e "$kv"); done
  log "${target}: ${name}"
  local rc=0
  if [[ -n "$RUNNER_SSH" ]]; then
    ssh "${SSH_OPTS[@]}" "$RUNNER_SSH" "mkdir -p ${REMOTE_DIR}/results/${RUN_ID} && docker run --rm --network host -u \$(id -u):\$(id -g) \
      --ulimit nofile=1048576:1048576 -v ${REMOTE_DIR}/bench/k6:/k6:ro -v ${REMOTE_DIR}/results:/results \
      $(printf '%q ' "${envs[@]}") ${K6_IMAGE} run --quiet /k6/scenarios/${script}" || rc=$?
  else
    docker compose -f "${BENCH_DIR}/docker-compose.bench.yaml" --profile k6 run --rm --no-deps \
      --user "$(id -u):$(id -g)" "${envs[@]}" k6 run --quiet "/k6/scenarios/${script}" || rc=$?
  fi
  if [[ $rc -ne 0 && $rc -ne 99 ]]; then
    echo "k6 ${script} failed for ${target} (exit ${rc})" >&2
    exit "$rc"
  fi
}

gw() { "${BENCH_DIR}/scripts/gw.py" "$@"; }

endpoint() {
  local host=$1 port=$2
  [[ -n "$SUT_IP" ]] && host="$SUT_IP"
  echo "http://${host}:${port}"
}

# record PHASE KEY CMD...: appends {"phase":PHASE,KEY:<JSON printed by sut CMD>}.
# Assigning first matters: a failure inside "$(...)" within echo would not trip set -e.
record() {
  local phase=$1 key=$2 val
  shift 2
  val="$(sut "$@")" || return 1
  echo "{\"phase\":\"${phase}\",\"t\":$(date +%s),\"${key}\":${val}}" >>"$res"
}

# Always collect and report what was measured; an abort is marked for report.py.
current=setup
res=/dev/null
sampler=
job=
finish() {
  local rc=$?
  set +e
  trap - EXIT
  [[ -n "$job" ]] && kill "$job" 2>/dev/null
  [[ $rc -ne 0 ]] && echo "{\"target\":\"${current}\",\"exit\":${rc}}" >"${OUT}/aborted.json"
  [[ "$current" != setup ]] && sut stop-gateways
  if [[ -n "$RUNNER_SSH" ]]; then
    scp -q "${SSH_OPTS[@]}" "${RUNNER_SSH}:${REMOTE_DIR}/results/${RUN_ID}/*.json" "${OUT}/"
  fi
  python3 "${BENCH_DIR}/scripts/report.py" "$OUT" && log "report: ${OUT}/REPORT.md"
  exit "$rc"
}

mkdir -p "$OUT"
trap finish EXIT
log "run ${RUN_ID}: profile=${PROFILE} targets=${TARGETS}"
sut build
sut up-deps
sut_env="$(sut env)"
printf '{"run_id":"%s","profile":"%s","mode":"%s","note":"%s","git_sha":"%s","started_at":"%s","sut":%s}\n' \
  "$RUN_ID" "$PROFILE" "$([[ -n "$SUT_SSH" ]] && echo cloud || echo local)" "${ENV_NOTE:-}" \
  "$(git -C "$BENCH_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)" "$(date -u +%FT%TZ)" \
  "$sut_env" >"${OUT}/env.json"

# One target per subshell: a gateway that fails is recorded in failed.jsonl and the
# run moves on. It runs in the background because set -e is ignored in a subshell
# that is tested with || or if.
bench_target() {
  trap '[[ -n "$sampler" ]] && kill "$sampler" 2>/dev/null' EXIT
  case "$target" in
    direct) svc=mock-upstream key=mock-key model=gpt-4o-mini path=/v1/chat/completions headers='{}'
      base="$(endpoint mock-upstream 8000)" ;;
    *) svc="$target" key="$(gw "$target" key)" model="$(gw "$target" model)" path="$(gw "$target" path)"
      headers="$(gw "$target" headers)" base="$(endpoint "$target" "$(gw "$target" port)")" ;;
  esac
  res="${OUT}/${target}.resources.jsonl"
  : >"$res"
  sut stop-gateways

  if [[ "$target" != direct ]]; then
    log "${target}: cold start"
    record arch arch arch "$svc"
    prepare="$(gw "$target" prepare)"
    [[ -n "$prepare" ]] && sut "$prepare"
    record cold_start ms cold-start "$svc"
    sut smoke "$svc"
    sleep "$SETTLE_S"
    record idle m metrics "$svc"
  fi

  k6run latency 01_latency_overhead.js "RATE=${LAT_RATE}" "DURATION=${LAT_DURATION}"
  k6run streaming 02_streaming_ttft.js "VUS=${STREAM_VUS}" "DURATION=${STREAM_DURATION}"

  if [[ "$target" != direct ]]; then
    record soak_start m metrics "$svc"
    record upstream_start u upstream-stats
    (
      while sleep 5; do record soak_sample m metrics "$svc" 2>/dev/null || true; done
    ) &
    sampler=$!
    k6run soak 04_soak.js "RATE=${SOAK_RATE}" "DURATION=${SOAK_DURATION}"
    kill "$sampler" 2>/dev/null || true
    wait "$sampler" 2>/dev/null || true
    sampler=
    record soak_end m metrics "$svc"
    record upstream_end u upstream-stats
    sleep "$COOLDOWN_S"
    record cooled m metrics "$svc"
  fi

  # Last: saturation can leave the process at peak heap, skewing later samples.
  k6run saturation 03_concurrency.js "STEPS=${SAT_STEPS}" "HOLD_S=${SAT_HOLD_S}" "RAMP_S=${SAT_RAMP_S}"
  if [[ "$target" != direct ]]; then
    record after_saturation m metrics "$svc"
  fi
}

for target in $TARGETS; do
  current="$target"
  rc=0
  bench_target &
  job=$!
  wait "$job" || rc=$?
  job=
  if [[ $rc -ne 0 ]]; then
    echo "{\"target\":\"${target}\",\"exit\":${rc}}" >>"${OUT}/failed.jsonl"
    log "${target}: failed (exit ${rc}), continuing with the next target"
    sut stop-gateways || true
  fi
done
current=finished
