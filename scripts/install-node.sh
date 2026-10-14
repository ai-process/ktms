#!/bin/sh
# Installs and updates a local llm-proxy inference node: Ollama, cloudflared and
# the node agent as per-user services. Safe to re-run; see --help.
set -eu

REPO="ai-process/llm-proxy"
GITHUB="${LLMPROXY_GITHUB:-https://github.com}"
GITHUB_API="${LLMPROXY_GITHUB_API:-https://api.github.com}"
BASE="${LLMPROXY_HOME:-$HOME/.llm-proxy-node}"
case $BASE in "" | / | "$HOME") printf 'ERROR: refusing to use %s as the install directory\n' "'$BASE'" >&2; exit 1 ;; esac
OLLAMA_ADDR="${LLMPROXY_OLLAMA_ADDR:-127.0.0.1:11435}" # private instance, never clashes with a system Ollama on 11434
LABEL_NODE="com.llm-proxy.node"
LABEL_OLLAMA="com.llm-proxy.ollama"
UNIT_NODE="llm-proxy-node"
UNIT_OLLAMA="llm-proxy-ollama"

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Usage: install-node.sh --proxy HOST[:PORT] --node-id ID --model MODEL [options]
       install-node.sh --uninstall

Required:
  --proxy HOST[:PORT]    llm-proxy gRPC address (port defaults to 443, TLS on)
  --node-id ID           node id the key was minted for
  --model MODEL          Ollama model to serve, e.g. gemma4:26b-a4b-it-qat

Options:
  --name NAME            human-readable node name (default: node id)
  --max-concurrency N    parallel requests this machine serves (default 1)
  --context N            context tokens per request (default 8192)
  --version vX.Y.Z       node agent release to install (default: latest)
  --key-file PATH        read the node key from a file instead of asking
  --new-key              ask for the node key again even if one is stored
  --plaintext            talk to the proxy without TLS
  --endpoint URL         you expose the node yourself: skip the built-in tunnel
  --upgrade-deps         also update Ollama and cloudflared (a re-run keeps them)
  --dry-run              check this machine and print the plan, change nothing
  --doctor               explain in plain words why the installed node is not working
  --uninstall            stop and remove the node, its services and its key
EOF
}

# ---- arguments -------------------------------------------------------------

parse_args() {
	PROXY="" NODE_ID="" MODEL="" NAME="" MAXC=1 CTX=8192 VERSION="latest"
	KEY_SRC="" NEW_KEY=0 PLAINTEXT=0 ENDPOINT="" UPGRADE=0 DRY_RUN=0 UNINSTALL=0 DOCTOR=0
	while [ $# -gt 0 ]; do
		opt=$1 val="" shift_n=1
		case $1 in --*=*) opt=${1%%=*} val=${1#*=} ;; esac
		case $opt in
		--proxy | --node-id | --model | --name | --max-concurrency | --context | --version | --key-file | --endpoint)
			if [ "$opt" = "$1" ]; then
				[ $# -ge 2 ] || die "$opt needs a value"
				val=$2 shift_n=2
			fi
			case $opt in
			--proxy) PROXY=$val ;; --node-id) NODE_ID=$val ;; --model) MODEL=$val ;; --name) NAME=$val ;;
			--max-concurrency) MAXC=$val ;; --context) CTX=$val ;; --version) VERSION=$val ;;
			--key-file) KEY_SRC=$val ;; --endpoint) ENDPOINT=$val ;;
			esac
			;;
		--new-key) NEW_KEY=1 ;;
		--plaintext) PLAINTEXT=1 ;;
		--upgrade-deps) UPGRADE=1 ;;
		--dry-run) DRY_RUN=1 ;;
		--uninstall) UNINSTALL=1 ;;
		--doctor) DOCTOR=1 ;;
		-h | --help) usage; exit 0 ;;
		*) die "unknown option: $1 (see --help)" ;;
		esac
		shift "$shift_n"
	done
	[ "$UNINSTALL" = 1 ] && return 0
	[ -n "$PROXY" ] || die "--proxy is required (see --help)"
	[ -n "$NODE_ID" ] || die "--node-id is required"
	[ -n "$MODEL" ] || die "--model is required"
	case $NODE_ID in *[!A-Za-z0-9._-]*) die "--node-id may only contain letters, digits, . _ -" ;; esac
	case $MODEL in *[!A-Za-z0-9._:/-]*) die "--model contains characters Ollama does not allow" ;; esac
	case $MAXC in '' | *[!0-9]* | 0) die "--max-concurrency must be a positive number" ;; esac
	case $CTX in '' | *[!0-9]* | 0) die "--context must be a positive number" ;; esac
	case $PROXY in *:*) ;; *) PROXY="$PROXY:443" ;; esac
	[ -n "$NAME" ] || NAME=$NODE_ID
	return 0
}

# ---- platform and resources ------------------------------------------------

detect_platform() {
	case $(uname -s) in
	Darwin) OS=darwin ;;
	Linux) OS=linux ;;
	*) die "unsupported OS $(uname -s): this installer supports macOS and Linux" ;;
	esac
	RUNNER=""
	if [ "$OS" = darwin ]; then
		if [ "$(sysctl -n hw.optional.arm64 2>/dev/null || echo 0)" = 1 ]; then
			ARCH=arm64
			# A shell started under Rosetta reports x86_64; force native for everything we run.
			RUNNER="/usr/bin/arch -arm64"
		else
			ARCH=amd64
			warn "This is an Intel Mac: no GPU acceleration, expect slow generation."
		fi
	else
		case $(uname -m) in
		x86_64 | amd64) ARCH=amd64 ;;
		aarch64 | arm64) ARCH=arm64 ;;
		*) die "unsupported CPU $(uname -m)" ;;
		esac
	fi
}

mem_total_mb() {
	if [ "$OS" = darwin ]; then
		bytes=$(sysctl -n hw.memsize 2>/dev/null || true)
		echo $((${bytes:-0} / 1048576))
	else
		kb=$(awk '/^MemTotal:/ { print $2 }' "${LLMPROXY_MEMINFO:-/proc/meminfo}" 2>/dev/null || true)
		echo $((${kb:-0} / 1024))
	fi
}

# gpu_budget_mb: memory the model can live in. A Mac GPU may use about three quarters of RAM.
gpu_budget_mb() {
	if [ "$OS" = darwin ]; then
		echo $(($(mem_total_mb) * 3 / 4))
	elif command -v nvidia-smi >/dev/null 2>&1; then
		nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits 2>/dev/null | awk 'NR == 1 { print int($1); exit } END { if (NR == 0) print 0 }'
	else
		echo $(($(mem_total_mb) * 3 / 4))
	fi
}

# model_weights_gb prints the rough weight size from a tag like name:26b-..., or nothing.
model_weights_gb() {
	printf '%s\n' "$1" | sed -n 's/^[^:]*:\([0-9][0-9.]*\)b.*/\1/p' | awk '$1 != "" { printf "%.1f\n", $1 * 0.6 }'
}

free_disk_gb() {
	df -Pk "$HOME" | awk 'NR == 2 { printf "%d\n", $4 / 1048576 }'
}

check_resources() {
	GB=$(model_weights_gb "$MODEL")
	if [ -z "$GB" ]; then
		say "Model size unknown from the tag; skipping the memory check."
		return 0
	fi
	budget=$(($(gpu_budget_mb) / 1024))
	if [ "$budget" -le 0 ]; then
		say "Could not read this machine's memory; skipping the memory check."
		return 0
	fi
	need=$(awk -v g="$GB" 'BEGIN { printf "%d\n", g * 1.2 + 2 }')
	say "Model $MODEL: about $GB GB of weights, roughly $need GB with context; this machine offers about $budget GB."
	if [ "$need" -gt "$budget" ]; then
		warn "Not enough memory for $MODEL: needs about $need GB, about $budget GB is usable. It may not load or may swap heavily. Pick a smaller model or continue at your own risk."
	fi
	if [ "$OS" = linux ] && ! command -v nvidia-smi >/dev/null 2>&1; then
		warn "No NVIDIA GPU found: the model runs on the CPU and will be slow."
	fi
}

# note_foreign_ollama explains why an Ollama found on this machine is not reused.
note_foreign_ollama() {
	[ "$OS" = darwin ] && [ "$ARCH" = arm64 ] || return 0
	found=$(command -v ollama 2>/dev/null || true)
	[ -n "$found" ] || return 0
	kind=$(file -bL "$found" 2>/dev/null || true)
	case $kind in
	*arm64*) ;;
	*x86_64*) warn "Found an Intel-only Ollama at $found: on Apple Silicon it runs through Rosetta without the GPU. It is left alone; this node uses its own native build." ;;
	esac
	return 0
}

# ---- downloads -------------------------------------------------------------

fetch() { # url dest
	progress="-sS"
	[ -t 2 ] && progress="--progress-bar"
	curl -fL --retry 3 --retry-delay 2 $progress -o "$2" "$1" || die "download failed: $1"
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{ print $1 }'; else shasum -a 256 "$1" | awk '{ print $1 }'; fi
}

verify_sha() { # file expected-hash
	got=$(sha256_of "$1")
	[ -n "$2" ] || die "no checksum available for $(basename "$1"); refusing to install an unverified download"
	[ "$got" = "$2" ] || die "checksum mismatch for $(basename "$1"): expected $2, got $got"
}

latest_tag() { # owner/repo
	url=$(curl -fsSIL -o /dev/null -w '%{url_effective}' "$GITHUB/$1/releases/latest") || die "cannot reach $GITHUB"
	printf '%s\n' "${url##*/}"
}

asset_digest() { # owner/repo tag asset
	body=$(curl -fsS "$GITHUB_API/repos/$1/releases/tags/$2") || die "cannot read release $2 of $1 (GitHub API rate limit? try again later)"
	printf '%s\n' "$body" | awk -v n="$3" '/"name":/ { v = $2; gsub(/[",]/, "", v); cur = v } /"digest":/ && cur == n { v = $2; gsub(/[",]/, "", v); sub(/^sha256:/, "", v); print v; exit }'
}

# ---- node key --------------------------------------------------------------

store_key() {
	KEY_FILE="$BASE/key"
	if [ -s "$KEY_FILE" ] && [ "$NEW_KEY" = 0 ] && [ -z "$KEY_SRC" ]; then
		say "Using the node key already stored in $KEY_FILE."
		return 0
	fi
	if [ -n "$KEY_SRC" ]; then
		[ -r "$KEY_SRC" ] || die "cannot read --key-file $KEY_SRC"
		key=$(tr -d ' \t\r\n' <"$KEY_SRC")
	else
		[ -r /dev/tty ] || die "no terminal to ask for the node key; pass --key-file PATH"
		trap 'stty echo </dev/tty 2>/dev/null || true' EXIT INT TERM
		stty -echo </dev/tty # before the prompt, so a fast paste is never echoed
		printf 'Paste the node key (input is hidden): ' >/dev/tty
		IFS= read -r key </dev/tty || true
		stty echo </dev/tty
		printf '\n' >/dev/tty
		trap - EXIT INT TERM
		key=$(printf '%s' "$key" | tr -d ' \t\r\n')
	fi
	[ -n "$key" ] || die "empty node key"
	case $key in llm_node_*) ;; *) warn "That does not look like a node key (they start with llm_node_); continuing." ;; esac
	(umask 077 && printf '%s\n' "$key" >"$KEY_FILE.tmp") && chmod 600 "$KEY_FILE.tmp" && mv "$KEY_FILE.tmp" "$KEY_FILE"
	key=""
}

# ---- services --------------------------------------------------------------

xml_esc() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }

# render_plist label logfile env-lines program args...  (env-lines: "K=V" newline separated)
render_plist() {
	label=$1 log=$2 envs=$3
	shift 3
	printf '<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n<plist version="1.0">\n<dict>\n'
	printf '\t<key>Label</key><string>%s</string>\n\t<key>ProgramArguments</key>\n\t<array>\n' "$label"
	for a in "$@"; do printf '\t\t<string>%s</string>\n' "$(xml_esc "$a")"; done
	printf '\t</array>\n\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>PATH</key><string>/usr/bin:/bin:/usr/sbin:/sbin</string>\n'
	printf '%s\n' "$envs" | while IFS="=" read -r k v; do
		if [ -n "$k" ]; then printf '\t\t<key>%s</key><string>%s</string>\n' "$k" "$(xml_esc "$v")"; fi
	done
	printf '\t</dict>\n\t<key>RunAtLoad</key><true/>\n\t<key>KeepAlive</key><true/>\n\t<key>ThrottleInterval</key><integer>10</integer>\n'
	printf '\t<key>StandardOutPath</key><string>%s</string>\n\t<key>StandardErrorPath</key><string>%s</string>\n</dict>\n</plist>\n' "$(xml_esc "$log")" "$(xml_esc "$log")"
}

sd_quote() { printf '"%s"' "$(printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/%/%%/g')"; }

# render_unit description logfile env-lines program args...
render_unit() {
	desc=$1 log=$2 envs=$3
	shift 3
	printf '[Unit]\nDescription=%s\nAfter=network-online.target\n\n[Service]\n' "$desc"
	printf '%s\n' "$envs" | while IFS="=" read -r k v; do
		if [ -n "$k" ]; then printf 'Environment=%s\n' "$(sd_quote "$k=$v")"; fi
	done
	printf 'ExecStart='
	for a in "$@"; do printf '%s ' "$(sd_quote "$a")"; done
	printf '\nRestart=always\nRestartSec=10\nStandardOutput=append:%s\nStandardError=append:%s\n\n[Install]\nWantedBy=default.target\n' "$log" "$log"
}

UID_NUM=$(id -u)
svc_dir() {
	if [ "$OS" = darwin ]; then echo "${LLMPROXY_SERVICE_DIR:-$HOME/Library/LaunchAgents}"; else echo "${LLMPROXY_SERVICE_DIR:-$HOME/.config/systemd/user}"; fi
}
svc_file() { # id: node|ollama
	if [ "$OS" = darwin ]; then
		[ "$1" = node ] && echo "$(svc_dir)/$LABEL_NODE.plist" || echo "$(svc_dir)/$LABEL_OLLAMA.plist"
	else
		[ "$1" = node ] && echo "$(svc_dir)/$UNIT_NODE.service" || echo "$(svc_dir)/$UNIT_OLLAMA.service"
	fi
}
svc_label() { if [ "$OS" = darwin ]; then [ "$1" = node ] && echo "$LABEL_NODE" || echo "$LABEL_OLLAMA"; else [ "$1" = node ] && echo "$UNIT_NODE" || echo "$UNIT_OLLAMA"; fi; }

mac_domain() {
	if launchctl print "gui/$UID_NUM" >/dev/null 2>&1; then echo "gui/$UID_NUM"; else echo "user/$UID_NUM"; fi
}

svc_running() {
	if [ "$OS" = darwin ]; then
		launchctl print "$(mac_domain)/$(svc_label "$1")" 2>/dev/null | grep -q 'state = running'
	else
		systemctl --user is-active --quiet "$(svc_label "$1").service"
	fi
}

svc_stop() {
	if [ "$OS" = darwin ]; then
		dom=$(mac_domain) lbl=$(svc_label "$1")
		launchctl bootout "$dom/$lbl" 2>/dev/null || true
		# bootstrap right after bootout fails with EIO while the old process is still unloading.
		i=0
		while launchctl print "$dom/$lbl" >/dev/null 2>&1 && [ $i -lt 40 ]; do sleep 0.5; i=$((i + 1)); done
	else
		systemctl --user stop "$(svc_label "$1").service" 2>/dev/null || true
	fi
}

svc_start() {
	if [ "$OS" = darwin ]; then
		svc_stop "$1"
		dom=$(mac_domain) f=$(svc_file "$1")
		i=0
		until launchctl bootstrap "$dom" "$f" 2>/dev/null; do
			i=$((i + 1))
			[ $i -lt 8 ] || die "launchctl could not load $f (the previous instance is still unloading?); re-run in a minute"
			sleep 1
		done
	else
		systemctl --user daemon-reload
		systemctl --user enable "$(svc_label "$1").service" >/dev/null 2>&1 || true
		systemctl --user restart "$(svc_label "$1").service"
	fi
}

# install_service id content-file: returns 0 when it changed (caller restarts), 1 when identical.
install_service() {
	f=$(svc_file "$1")
	mkdir -p "$(dirname "$f")"
	if [ -f "$f" ] && cmp -s "$2" "$f"; then rm -f "$2"; return 1; fi
	svc_stop "$1"
	mv "$2" "$f"
	return 0
}

ollama_env() {
	printf 'OLLAMA_HOST=%s\nOLLAMA_KEEP_ALIVE=-1\nOLLAMA_CONTEXT_LENGTH=%s\nOLLAMA_NUM_PARALLEL=%s\n' "$OLLAMA_ADDR" "$CTX" "$MAXC"
}

# ---- Ollama, cloudflared, node agent ----------------------------------------

find_ollama_bin() {
	for p in "$BASE/ollama/ollama" "$BASE/ollama/bin/ollama"; do
		[ -x "$p" ] && { echo "$p"; return 0; }
	done
	return 1
}

ensure_ollama() {
	OLLAMA_BIN=$(find_ollama_bin || true)
	if [ -n "$OLLAMA_BIN" ] && [ "$UPGRADE" = 0 ]; then
		say "Ollama already installed: $OLLAMA_BIN (kept; --upgrade-deps updates it)."
		return 0
	fi
	tag=$(latest_tag ollama/ollama)
	if [ "$OS" = darwin ]; then asset=ollama-darwin.tgz; else asset="ollama-linux-$ARCH.tar.zst"; fi
	if [ "$OS" = linux ] && ! command -v zstd >/dev/null 2>&1; then
		die "zstd is needed to unpack Ollama: install it (Ubuntu: sudo apt install zstd) and re-run"
	fi
	step "Installing Ollama $tag"
	tmp=$(mktemp -d "$BASE/dl.XXXXXX")
	fetch "$GITHUB/ollama/ollama/releases/download/$tag/$asset" "$tmp/$asset"
	verify_sha "$tmp/$asset" "$(asset_digest ollama/ollama "$tag" "$asset")"
	rm -rf "${BASE:?}/ollama.new" && mkdir -p "$BASE/ollama.new"
	if [ "$OS" = darwin ]; then tar -xzf "$tmp/$asset" -C "$BASE/ollama.new"; else zstd -dc "$tmp/$asset" | tar -x -C "$BASE/ollama.new"; fi
	rm -rf "$tmp"
	svc_stop ollama
	rm -rf "${BASE:?}/ollama" && mv "$BASE/ollama.new" "$BASE/ollama"
	OLLAMA_BIN=$(find_ollama_bin) || die "the Ollama archive had no ollama binary"
	OLLAMA_CHANGED=1
}

ensure_cloudflared() {
	[ -z "$ENDPOINT" ] || return 0
	CF_BIN="$BASE/bin/cloudflared"
	if [ -x "$CF_BIN" ] && [ "$UPGRADE" = 0 ]; then return 0; fi
	tag=$(latest_tag cloudflare/cloudflared)
	if [ "$OS" = darwin ]; then asset="cloudflared-darwin-$ARCH.tgz"; else asset="cloudflared-linux-$ARCH"; fi
	step "Installing cloudflared $tag"
	tmp=$(mktemp -d "$BASE/dl.XXXXXX")
	fetch "$GITHUB/cloudflare/cloudflared/releases/download/$tag/$asset" "$tmp/$asset"
	verify_sha "$tmp/$asset" "$(asset_digest cloudflare/cloudflared "$tag" "$asset")"
	if [ "$OS" = darwin ]; then tar -xzf "$tmp/$asset" -C "$tmp" cloudflared && mv "$tmp/cloudflared" "$CF_BIN.new"; else mv "$tmp/$asset" "$CF_BIN.new"; fi
	chmod 755 "$CF_BIN.new"
	"$CF_BIN.new" --version >/dev/null 2>&1 || die "the downloaded cloudflared does not run on this machine"
	mv "$CF_BIN.new" "$CF_BIN"
	rm -rf "$tmp"
}

# stage_agent downloads, verifies and smoke-tests the agent, leaving it in $BASE/bin/llm-proxy-node.new.
# The running binary is only replaced later, so a bad download or a blocked binary changes nothing.
stage_agent() {
	if [ "$VERSION" = latest ]; then
		tag=$(latest_tag "$REPO")
	else
		tag=$VERSION
		case $tag in v*) ;; *) tag="v$tag" ;; esac
	fi
	ver=${tag#v}
	if [ -x "$BASE/bin/llm-proxy-node" ] && [ "$("$BASE/bin/llm-proxy-node" --version 2>/dev/null || true)" = "$ver" ]; then
		say "Node agent $ver is already installed."
		AGENT_STAGED=0
		return 0
	fi
	step "Downloading node agent $ver"
	asset="llm-proxy-node_${ver}_${OS}_${ARCH}.tar.gz"
	tmp=$(mktemp -d "$BASE/dl.XXXXXX")
	curl -fsSL -o "$tmp/checksums.txt" "$GITHUB/$REPO/releases/download/$tag/checksums.txt" ||
		die "release $tag has no node agent binaries (they exist from v0.10.0 on); pass --version with a newer release"
	want=$(awk -v a="$asset" '$2 == a { print $1 }' "$tmp/checksums.txt")
	fetch "$GITHUB/$REPO/releases/download/$tag/$asset" "$tmp/$asset"
	verify_sha "$tmp/$asset" "$want"
	tar -xzf "$tmp/$asset" -C "$tmp" llm-proxy-node
	mv "$tmp/llm-proxy-node" "$BASE/bin/llm-proxy-node.new"
	rm -rf "$tmp"
	chmod 755 "$BASE/bin/llm-proxy-node.new"
	[ "$OS" = darwin ] && xattr -dr com.apple.quarantine "$BASE/bin" 2>/dev/null || true
	got=$("$BASE/bin/llm-proxy-node.new" --version 2>&1) || {
		rm -f "$BASE/bin/llm-proxy-node.new"
		die "the new node agent does not start on this machine ($got). The installed one was left untouched."
	}
	say "Downloaded and verified node agent $got."
	AGENT_STAGED=1
}

run_ollama() {
	if [ -n "$RUNNER" ]; then /usr/bin/arch -arm64 "$OLLAMA_BIN" "$@"; else "$OLLAMA_BIN" "$@"; fi
}

wait_http() { # url seconds
	i=0
	while [ $i -lt "$2" ]; do
		curl -fsS -m 3 "$1" >/dev/null 2>&1 && return 0
		sleep 1
		i=$((i + 1))
	done
	return 1
}

ensure_model() {
	if curl -fsS "http://$OLLAMA_ADDR/api/tags" | grep -qF "\"name\":\"$MODEL\""; then
		say "Model $MODEL is already downloaded."
	else
		if [ -n "${GB:-}" ]; then
			free=$(free_disk_gb)
			[ "$free" -ge "${GB%.*}" ] || die "not enough disk space for $MODEL: needs about $GB GB, $free GB free in $HOME"
		fi
		step "Downloading model $MODEL (this can take a while)"
		OLLAMA_HOST="$OLLAMA_ADDR" run_ollama pull "$MODEL" || die "could not download $MODEL (check the name with: ollama list / the Ollama library)"
	fi
	step "Loading $MODEL into memory"
	curl -fsS -m 900 "http://$OLLAMA_ADDR/api/generate" -d "{\"model\":\"$MODEL\",\"keep_alive\":-1}" >/dev/null || warn "could not preload $MODEL; the first request will be slow"
}

write_service() { # id program args... (stdin: nothing)
	id=$1
	shift
	out=$(mktemp "$BASE/svc.XXXXXX")
	if [ "$id" = node ]; then log="$BASE/logs/node.log" envs="" desc="llm-proxy node agent"; else log="$BASE/logs/ollama.log" envs=$(ollama_env) desc="llm-proxy Ollama"; fi
	if [ "$OS" = darwin ]; then render_plist "$(svc_label "$id")" "$log" "$envs" "$@" >"$out"; else render_unit "$desc" "$log" "$envs" "$@" >"$out"; fi
	SVC_TMP=$out
}

# run_doctor explains why the node is not working, in plain language. Returns the agent's status.
run_doctor() {
	step "Diagnosing"
	set -- doctor --proxy-addr "$PROXY" --node-id "$NODE_ID" --node-key-file "$BASE/key" \
		--engine ollama --engine-url "http://$OLLAMA_ADDR" --model "$MODEL"
	[ "$PLAINTEXT" = 1 ] || set -- "$@" --tls
	if [ -n "$ENDPOINT" ]; then
		set -- "$@" --endpoint "$ENDPOINT" --expose none
	else
		set -- "$@" --expose cloudflared --cloudflared-path "$BASE/bin/cloudflared"
	fi
	"$BASE/bin/llm-proxy-node" "$@"
}

registered_in_log() { # start-line
	tail -n +"$(($1 + 1))" "$BASE/logs/node.log" 2>/dev/null | grep -q 'node registered successfully'
}

log_lines() {
	if [ -f "$BASE/logs/node.log" ]; then wc -l <"$BASE/logs/node.log" | tr -d ' '; else echo 0; fi
}

start_node_and_wait() {
	start=$(log_lines)
	svc_start node
	say "Waiting for the node to register (the tunnel can take up to two minutes)..."
	i=0
	while [ $i -lt "${LLMPROXY_REGISTER_WAIT:-150}" ]; do
		registered_in_log "$start" && return 0
		svc_running node || { [ $i -gt 5 ] && return 1; }
		sleep 1
		i=$((i + 1))
	done
	return 1
}

# ---- main ------------------------------------------------------------------

do_uninstall() {
	detect_platform
	step "Removing the node"
	for id in node ollama; do
		svc_stop "$id"
		rm -f "$(svc_file "$id")"
	done
	[ "$OS" = linux ] && systemctl --user daemon-reload 2>/dev/null || true
	rm -rf "${BASE:?}"
	say "Removed services, binaries, logs and the stored node key."
	say "Downloaded models stay in ~/.ollama/models; delete them there if you want the space back."
}

print_status() {
	ep=$(sed -n 's/.*node registered successfully.*endpoint=\([^ ]*\).*/\1/p' "$BASE/logs/node.log" 2>/dev/null | tail -1 | sed "s/$(printf '\033')\[[0-9;]*m//g")
	engine="Ollama"
	[ "$OS" = darwin ] && [ "$ARCH" = arm64 ] && engine="Ollama (Apple GPU)"
	cat <<EOF

Node $NODE_ID is running and registered.
  Proxy:   $PROXY
  Engine:  $engine on $OLLAMA_ADDR, model $MODEL
  Address: ${ep:-see $BASE/logs/node.log}
  Logs:    $BASE/logs/node.log
  Update:  run the same command again
  Remove:  re-run with --uninstall
EOF
}

main() {
	parse_args "$@"
	[ "$UNINSTALL" = 1 ] && { do_uninstall; return 0; }
	detect_platform
	if [ "$DOCTOR" = 1 ]; then
		[ -x "$BASE/bin/llm-proxy-node" ] || die "no node is installed in $BASE; run the installer without --doctor first"
		run_doctor
		return $?
	fi
	step "Checking this machine ($OS/$ARCH)"
	note_foreign_ollama
	check_resources
	if [ "$DRY_RUN" = 1 ]; then
		say "Dry run: would install into $BASE, run Ollama on $OLLAMA_ADDR and register node $NODE_ID with $PROXY. Nothing was changed."
		return 0
	fi
	command -v curl >/dev/null 2>&1 || die "curl is required"
	mkdir -p "$BASE/bin" "$BASE/logs"
	chmod 700 "$BASE"
	rm -rf "${BASE:?}"/dl.* "${BASE:?}"/svc.* "${BASE:?}/ollama.new" # leftovers of an interrupted run
	if [ "$OS" = linux ]; then
		systemctl --user show-environment >/dev/null 2>&1 || die "systemd user services are not available in this session; log in normally (not via su) and retry"
		loginctl enable-linger "$(id -un)" 2>/dev/null || warn "could not enable lingering: the node will stop when you log out (run: sudo loginctl enable-linger $(id -un))"
	fi
	OLLAMA_CHANGED=0 AGENT_STAGED=0
	store_key
	ensure_ollama
	ensure_cloudflared
	stage_agent

	step "Starting the inference engine"
	if [ -n "$RUNNER" ]; then write_service ollama /usr/bin/arch -arm64 "$OLLAMA_BIN" serve; else write_service ollama "$OLLAMA_BIN" serve; fi
	if install_service ollama "$SVC_TMP" || [ "$OLLAMA_CHANGED" = 1 ] || ! svc_running ollama; then svc_start ollama; fi
	wait_http "http://$OLLAMA_ADDR/api/version" 60 || die "Ollama did not start; see $BASE/logs/ollama.log"
	ensure_model

	step "Starting the node"
	set -- "$BASE/bin/llm-proxy-node" --proxy-addr "$PROXY" --node-id "$NODE_ID" --node-name "$NAME" \
		--engine ollama --engine-url "http://$OLLAMA_ADDR" --max-concurrent "$MAXC" --node-key-file "$BASE/key"
	[ "$PLAINTEXT" = 1 ] || set -- "$@" --tls
	if [ -n "$ENDPOINT" ]; then
		set -- "$@" --endpoint "$ENDPOINT" --expose none
	else
		set -- "$@" --expose cloudflared --cloudflared-path "$BASE/bin/cloudflared"
	fi
	# Keep the machine awake while the node runs.
	if [ "$OS" = darwin ]; then
		set -- /usr/bin/caffeinate -is "$@"
	elif command -v systemd-inhibit >/dev/null 2>&1; then
		set -- systemd-inhibit --what=idle:sleep --who=llm-proxy-node --why="Serving LLM requests" --mode=block "$@"
	fi
	node_changed=0
	if [ "$AGENT_STAGED" = 1 ]; then
		svc_stop node
		[ -x "$BASE/bin/llm-proxy-node" ] && cp -p "$BASE/bin/llm-proxy-node" "$BASE/bin/llm-proxy-node.prev"
		mv "$BASE/bin/llm-proxy-node.new" "$BASE/bin/llm-proxy-node"
		node_changed=1
	fi
	write_service node "$@"
	install_service node "$SVC_TMP" && node_changed=1
	if [ "$node_changed" = 1 ] || ! svc_running node; then
		if ! start_node_and_wait; then
			run_doctor || true # best effort: an older agent may not have it
			if [ "$AGENT_STAGED" = 1 ] && [ -x "$BASE/bin/llm-proxy-node.prev" ]; then
				warn "The new node agent did not come up; restoring the previous one."
				svc_stop node
				mv "$BASE/bin/llm-proxy-node.prev" "$BASE/bin/llm-proxy-node"
				svc_start node || true
			fi
			tail -n 15 "$BASE/logs/node.log" 2>/dev/null >&2 || true
			die "the node did not register; the diagnosis above says why. Run it again any time: $BASE/bin/llm-proxy-node doctor --help. Log: $BASE/logs/node.log"
		fi
	fi
	print_status
}

[ "${INSTALL_NODE_SOURCED:-0}" = 1 ] || main "$@"
