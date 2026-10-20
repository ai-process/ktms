#!/usr/bin/env bash
# GCP_PROJECT=... ./scripts/run_cloud.sh: creates the stand, ships HEAD, runs the suite
# from the runner VM against the SUT over the private subnet, collects the report and
# always destroys the stand (KEEP=1 keeps it for debugging).
set -euo pipefail

BENCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_DIR="$(cd "${BENCH_DIR}/.." && pwd)"
TF=(terraform -chdir="${BENCH_DIR}/terraform")
REMOTE_DIR=/opt/bench
: "${GCP_PROJECT:?set GCP_PROJECT to the GCP project id}"
export TF_VAR_project="$GCP_PROJECT"
# SSH is opened only to this machine; set SSH_CIDR when egress differs (VPN, CI).
SSH_CIDR="${SSH_CIDR:-$(curl -fsS --max-time 10 https://checkip.amazonaws.com | tr -d '[:space:]')/32}"
[[ "$SSH_CIDR" =~ ^[0-9.]+/[0-9]+$ ]] || { echo "cannot detect public IP; set SSH_CIDR" >&2; exit 1; }
export TF_VAR_ssh_allowed_cidrs="[\"${SSH_CIDR}\"]"
# One workspace per prefix, so stands with different TF_VAR_prefix can run in parallel.
export TF_VAR_prefix="${TF_VAR_prefix:-ktms-bench}"

if [[ -n "$(git -C "$REPO_DIR" status --porcelain -- . ':!bench/results')" ]]; then
  echo "warning: uncommitted changes are not shipped; benchmarking HEAD $(git -C "$REPO_DIR" rev-parse --short HEAD)" >&2
fi

"${TF[@]}" init -input=false >/dev/null
# TF_WORKSPACE instead of `workspace select`: selection is a shared file in .terraform/.
TF_WORKSPACE='' "${TF[@]}" workspace new "$TF_VAR_prefix" >/dev/null 2>&1 || true
export TF_WORKSPACE="$TF_VAR_prefix"

destroy() {
  if [[ "${KEEP:-0}" == 1 ]]; then
    echo "KEEP=1: stand left running; destroy with: TF_WORKSPACE=${TF_WORKSPACE} TF_VAR_prefix=${TF_VAR_prefix}" \
      "TF_VAR_project=${GCP_PROJECT} TF_VAR_ssh_allowed_cidrs='[]' terraform -chdir=${BENCH_DIR}/terraform destroy" >&2
    return
  fi
  "${TF[@]}" destroy -auto-approve -input=false
}
trap destroy EXIT

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
  # Exit 2 means "done with recoverable warnings"; docker must work either way. A new
  # session is needed for docker: cloud-init adds the user to its group after login works.
  ssh "${ssh_opts[@]}" "$host" 'cloud-init status --wait >/dev/null' || true
  ssh "${ssh_opts[@]}" "$host" 'docker version -f "{{.Server.Version}}"'
  git -C "$REPO_DIR" archive --format=tar HEAD | ssh "${ssh_opts[@]}" "$host" "tar -x -C ${REMOTE_DIR}"
done
ssh "${ssh_opts[@]}" "$SUT" "printf 'BIND_ADDR=%s\nSUT_CPUS=%s\n' ${SUT_IP} \$(nproc) > ${REMOTE_DIR}/bench/.env"

SUT_SSH="$SUT" RUNNER_SSH="$RUNNER" SUT_IP="$SUT_IP" SSH_OPTS="$SSH_OPTS" REMOTE_DIR="$REMOTE_DIR" \
  ENV_NOTE="$(out description)" "${BENCH_DIR}/scripts/run_all.sh"
