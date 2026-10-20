#!/usr/bin/env bash
# Full cloud cycle: create the stand on GCP, ship the current commit, run the
# suite from the runner VM against the SUT over the private subnet, collect the
# report and destroy everything (set KEEP=1 to keep the stand for debugging).
#
#   GCP_PROJECT=my-project ./scripts/run_cloud.sh
set -euo pipefail

BENCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_DIR="$(cd "${BENCH_DIR}/.." && pwd)"
TF=(terraform -chdir="${BENCH_DIR}/terraform")
REMOTE_DIR=/opt/bench
: "${GCP_PROJECT:?set GCP_PROJECT to the GCP project id}"
export TF_VAR_project="$GCP_PROJECT"

if [[ -n "$(git -C "$REPO_DIR" status --porcelain -- . ':!bench/results')" ]]; then
  echo "warning: uncommitted changes are not shipped; benchmarking HEAD $(git -C "$REPO_DIR" rev-parse --short HEAD)" >&2
fi

destroy() {
  if [[ "${KEEP:-0}" == 1 ]]; then
    echo "KEEP=1: stand left running; destroy with: GCP_PROJECT=${GCP_PROJECT} terraform -chdir=${BENCH_DIR}/terraform destroy" >&2
    return
  fi
  "${TF[@]}" destroy -auto-approve -input=false
}
trap destroy EXIT

"${TF[@]}" init -input=false >/dev/null
"${TF[@]}" apply -auto-approve -input=false
out() { "${TF[@]}" output -raw "$1"; }
user="$(out ssh_user)"
SUT="${user}@$(out sut_public_ip)"
RUNNER="${user}@$(out runner_public_ip)"
SUT_IP="$(out sut_private_ip)"
SSH_OPTS="-i $(out ssh_key_path) -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=10 -o ServerAliveInterval=30"
read -r -a ssh_opts <<<"$SSH_OPTS"

for host in "$SUT" "$RUNNER"; do
  for _ in $(seq 1 60); do ssh "${ssh_opts[@]}" "$host" true 2>/dev/null && break; sleep 5; done
  # Exit 2 means "done with recoverable warnings"; docker must work either way.
  ssh "${ssh_opts[@]}" "$host" 'cloud-init status --wait >/dev/null; docker version -f "{{.Server.Version}}"'
  git -C "$REPO_DIR" archive --format=tar HEAD | ssh "${ssh_opts[@]}" "$host" "tar -x -C ${REMOTE_DIR}"
done
ssh "${ssh_opts[@]}" "$SUT" "printf 'BIND_ADDR=%s\nLITELLM_WORKERS=%s\n' ${SUT_IP} \$(nproc) > ${REMOTE_DIR}/bench/.env"

SUT_SSH="$SUT" RUNNER_SSH="$RUNNER" SUT_IP="$SUT_IP" SSH_OPTS="$SSH_OPTS" REMOTE_DIR="$REMOTE_DIR" \
  ENV_NOTE="$(out description)" "${BENCH_DIR}/scripts/run_all.sh"
