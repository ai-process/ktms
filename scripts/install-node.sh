#!/bin/sh
# Installs and updates a local KTMS inference node: Ollama, cloudflared and
# the node agent as per-user services. Safe to re-run; see --help.
set -eu

REPO="ai-process/ktms"
GITHUB="${KTMS_GITHUB:-${LLMPROXY_GITHUB:-https://github.com}}"
GITHUB_API="${KTMS_GITHUB_API:-${LLMPROXY_GITHUB_API:-https://api.github.com}}"
BASE="${KTMS_HOME:-${LLMPROXY_HOME:-$HOME/.ktms-node}}"
case $BASE in "" | / | "$HOME") printf 'ERROR: refusing to use %s as the install directory\n' "'$BASE'" >&2; exit 1 ;; esac
OLLAMA_ADDR="${KTMS_OLLAMA_ADDR:-${LLMPROXY_OLLAMA_ADDR:-127.0.0.1:11435}}" # private instance, never clashes with a system Ollama on 11434
LABEL_NODE="com.ktms.node"
LABEL_OLLAMA="com.ktms.ollama"
UNIT_NODE="ktms-node"
UNIT_OLLAMA="ktms-ollama"
# Names of installs made before the rename to KTMS; found and retired on install.
LEGACY_LABEL_PREFIX="com.llm-proxy"
LEGACY_UNIT_PREFIX="llm-proxy"
BACKUP_ROOT="$BASE.backup"

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Usage: install-node.sh --proxy HOST[:PORT] --node-id ID --model MODEL [options]
       install-node.sh --uninstall

Required:
  --proxy HOST[:PORT]    KTMS gRPC address (port defaults to 443, TLS on)
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
  --schedule FILE        load levels by local time, "[days] HH:MM-HH:MM level" per line;
                         copied to ~/.ktms-node/schedule, which a re-run without it keeps
  --upgrade-deps         also update Ollama and cloudflared (a re-run keeps them)
  --dry-run              check this machine and print the plan, change nothing
  --doctor               explain in plain words why the installed node is not working
  --keep-legacy          leave an old ~/.llm-proxy-node in place (its services are still
                         stopped and backed up, or they would clash with this node)
  --uninstall            stop and remove the node, its services and its key
  --purge-backups        with --uninstall: also delete the backups of old installs
EOF
}

# ---- arguments -------------------------------------------------------------

parse_args() {
	PROXY="" NODE_ID="" MODEL="" NAME="" MAXC=1 CTX=8192 VERSION="latest"
	KEY_SRC="" NEW_KEY=0 PLAINTEXT=0 ENDPOINT="" UPGRADE=0 DRY_RUN=0 UNINSTALL=0 DOCTOR=0 KEEP_LEGACY=0 PURGE_BACKUPS=0 SCHEDULE_SRC=""
	while [ $# -gt 0 ]; do
		opt=$1 val="" shift_n=1
		case $1 in --*=*) opt=${1%%=*} val=${1#*=} ;; esac
		case $opt in
		--proxy | --node-id | --model | --name | --max-concurrency | --context | --version | --key-file | --endpoint | --schedule)
			if [ "$opt" = "$1" ]; then
				[ $# -ge 2 ] || die "$opt needs a value"
				val=$2 shift_n=2
			fi
			case $opt in
			--proxy) PROXY=$val ;; --node-id) NODE_ID=$val ;; --model) MODEL=$val ;; --name) NAME=$val ;;
			--max-concurrency) MAXC=$val ;; --context) CTX=$val ;; --version) VERSION=$val ;;
			--key-file) KEY_SRC=$val ;; --endpoint) ENDPOINT=$val ;; --schedule) SCHEDULE_SRC=$val ;;
			esac
			;;
		--new-key) NEW_KEY=1 ;;
		--plaintext) PLAINTEXT=1 ;;
		--upgrade-deps) UPGRADE=1 ;;
		--dry-run) DRY_RUN=1 ;;
		--uninstall) UNINSTALL=1 ;;
		--doctor) DOCTOR=1 ;;
		--keep-legacy) KEEP_LEGACY=1 ;;
		--purge-backups) PURGE_BACKUPS=1 ;;
		-h | --help) usage; exit 0 ;;
		*) die "unknown option: $1 (see --help)" ;;
		esac
		shift "$shift_n"
	done
	[ "$PURGE_BACKUPS" = 0 ] || [ "$UNINSTALL" = 1 ] || die "--purge-backups only goes with --uninstall"
	[ "$UNINSTALL" = 1 ] && return 0
	[ -n "$PROXY" ] || die "--proxy is required (see --help)"
	[ -n "$NODE_ID" ] || die "--node-id is required"
	[ -n "$MODEL" ] || die "--model is required"
	case $NODE_ID in *[!A-Za-z0-9._-]*) die "--node-id may only contain letters, digits, . _ -" ;; esac
	case $MODEL in *[!A-Za-z0-9._:/-]*) die "--model contains characters Ollama does not allow" ;; esac
	case $MAXC in '' | *[!0-9]* | 0) die "--max-concurrency must be a positive number" ;; esac
	case $CTX in '' | *[!0-9]* | 0) die "--context must be a positive number" ;; esac
	[ -z "$SCHEDULE_SRC" ] || [ -r "$SCHEDULE_SRC" ] || die "cannot read --schedule $SCHEDULE_SRC"
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
# Bytes per parameter follow the quantization named in the tag; unnamed tags assume a typical 4-bit build.
model_weights_gb() {
	params=$(printf '%s\n' "$1" | sed -n 's/^[^:]*:\([0-9][0-9.]*\)b.*/\1/p')
	[ -n "$params" ] || return 0
	case $1 in
	*fp16* | *f16*) bpp=2.05 ;; # bf16 included
	*q8* | *int8* | *fp8*) bpp=1.05 ;;
	*q6*) bpp=0.85 ;;
	*q5*) bpp=0.7 ;;
	*qat* | *int4* | *q4_0* | *mxfp4* | *nvfp4*) bpp=0.5 ;;
	*) bpp=0.6 ;;
	esac
	awk -v p="$params" -v b="$bpp" 'BEGIN { printf "%.1f\n", p * b }'
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
	# Weights plus runtime buffers (1.5 GB) plus a rough 1 GB of KV cache per 8192 context tokens.
	need=$(awk -v g="$GB" -v c="$CTX" 'BEGIN { printf "%d\n", g + 1.5 + c / 8192 }')
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
		has_tty || die "no terminal to ask for the node key; pass --key-file PATH"
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
	case $key in ktms_node_* | llm_node_*) ;; *) warn "That does not look like a node key (they start with ktms_node_); continuing." ;; esac
	write_key "$key"
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

svc_stop() { stop_label "$(svc_label "$1")"; }

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

# ---- ports and installs from before the rename -------------------------------

label_pid() { # label: its PID under the service manager, or nothing
	if [ "$OS" = darwin ]; then
		launchctl print "$(mac_domain)/$1" 2>/dev/null | awk '$1 == "pid" && $2 == "=" { print $3; exit }'
	else
		p=$(systemctl --user show -p MainPID --value "$1.service" 2>/dev/null || true)
		[ "${p:-0}" = 0 ] || echo "$p"
	fi
}

# port_holder addr: "pid name" of what listens where addr would bind: the same
# address or a wildcard. Another user's process shows no PID, so an address that
# answers HTTP while nothing of ours should be there counts as "unknown".
port_holder() {
	host=${1%:*} port=${1##*:} pid=""
	if command -v lsof >/dev/null 2>&1; then
		pid=$(lsof -nP -iTCP:"$port" -sTCP:LISTEN -Fpn 2>/dev/null | awk -v h="$host:$port" -v p=":$port" '
			/^p/ { pid = substr($0, 2) } /^n/ { a = substr($0, 2); if (a == h || a == "*" p || a == "0.0.0.0" p || a == "[::]" p) { print pid; exit } }')
	elif command -v ss >/dev/null 2>&1; then
		pid=$(ss -ltnpH 2>/dev/null | awk -v h="$host:$port" -v p=":$port" '
			($4 == h || $4 == "*" p || $4 == "0.0.0.0" p || $4 == "[::]" p) { if (match($0, /pid=[0-9]+/)) print substr($0, RSTART + 4, RLENGTH - 4); else print "?"; exit }')
	fi
	if [ -z "$pid" ] && [ "${2:-}" = idle ] && curl -fsS -m 2 "http://$1/api/version" >/dev/null 2>&1; then pid="?"; fi
	[ -n "$pid" ] || return 0
	if [ "$pid" = "?" ]; then echo "? another user's process"; else printf '%s %s\n' "$pid" "$(ps -o comm= -p "$pid" 2>/dev/null | head -n 1)"; fi
}

# check_ollama_port: the model would otherwise be pulled into, and served by,
# someone else's Ollama that happens to hold the node's address.
check_ollama_port() {
	ours=$(label_pid "$(svc_label ollama)")
	if [ -n "$ours" ]; then held=$(port_holder "$OLLAMA_ADDR"); else held=$(port_holder "$OLLAMA_ADDR" idle); fi
	[ -n "$held" ] || return 0
	[ "${held%% *}" = "$ours" ] && return 0
	[ "${held%% *}" = "?" ] && [ -n "$ours" ] && return 0 # unreadable PID with ours running: it is ours
	if [ "${held%% *}" = "?" ]; then what=${held#* }; else what="${held#* } (pid ${held%% *})"; fi
	die "$OLLAMA_ADDR is taken by $what, not by this node's Ollama. Stop it, or run this node's Ollama elsewhere with KTMS_OLLAMA_ADDR=127.0.0.1:PORT."
}

legacy_label() { if [ "$OS" = darwin ]; then echo "$LEGACY_LABEL_PREFIX.$1"; else echo "$LEGACY_UNIT_PREFIX-$1"; fi; }
legacy_file() { if [ "$OS" = darwin ]; then echo "$(svc_dir)/$(legacy_label "$1").plist"; else echo "$(svc_dir)/$(legacy_label "$1").service"; fi; }

# service_arg and service_env read back what render_plist / render_unit wrote,
# the only producers of legacy services, so their layout is known exactly.
service_arg() { # file flag
	case $1 in
	*.plist) awk -v f="<string>$2</string>" 'want { sub(/^[ \t]*<string>/, ""); sub(/<\/string>.*/, ""); print; exit } index($0, f) { want = 1 }' "$1" ;;
	*) sed -n 's/^ExecStart=//p' "$1" | awk -v f="$2" '{ n = split($0, a, /" "/); for (i = 1; i <= n; i++) gsub(/^ *"|" *$/, "", a[i]); for (i = 1; i < n; i++) if (a[i] == f) { print a[i + 1]; exit } }' ;;
	esac
}
service_env() { # file name
	case $1 in
	*.plist) awk -v k="<key>$2</key><string>" 'index($0, k) { s = substr($0, index($0, k) + length(k)); sub(/<\/string>.*/, "", s); print s; exit }' "$1" ;;
	*) sed -n "s/^Environment=\"$2=\(.*\)\"\$/\1/p" "$1" | head -n 1 ;;
	esac
}

real_path() { (cd "$1" 2>/dev/null && pwd -P) || printf '%s' "${1%/}"; }

detect_legacy() {
	LEGACY_IDS="" LEGACY_DIR="" LEGACY_NODE_ID="" LEGACY_MODELS=""
	for id in node ollama; do
		if [ -f "$(legacy_file "$id")" ] || [ -n "$(label_pid "$(legacy_label "$id")")" ]; then LEGACY_IDS="$LEGACY_IDS $id"; fi
	done
	f=$(legacy_file node)
	[ ! -f "$f" ] || LEGACY_NODE_ID=$(service_arg "$f" --node-id)
	f=$(legacy_file ollama)
	[ ! -f "$f" ] || LEGACY_MODELS=$(service_env "$f" OLLAMA_MODELS)
	base=$(real_path "$BASE")
	for d in "${LLMPROXY_HOME:-}" "$HOME/.llm-proxy-node"; do
		if [ -z "$d" ] || [ ! -d "$d" ]; then continue; fi
		r=$(real_path "$d")
		# Never the current install (kept under its old name), $HOME, / or a parent of the install.
		case $r in / | "$(real_path "$HOME")") continue ;; esac
		case "$base/" in "$r"/*) continue ;; esac
		LEGACY_DIR=${d%/}
		break
	done
}

legacy_work() { [ -n "$LEGACY_IDS" ] || { [ -n "$LEGACY_DIR" ] && [ "$KEEP_LEGACY" = 0 ]; }; }

legacy_key_plan() {
	[ -n "$LEGACY_DIR" ] && [ -s "$LEGACY_DIR/key" ] || return 0
	if [ -s "$BASE/key" ] || [ -n "$KEY_SRC" ] || [ "$NEW_KEY" = 1 ]; then
		echo "not needed: this node has its own key"
	elif [ "$LEGACY_NODE_ID" = "$NODE_ID" ]; then
		echo "reused: it was minted for node $NODE_ID too"
	else
		echo "offered: it was minted for node ${LEGACY_NODE_ID:-unknown}, not $NODE_ID, so you are asked first"
	fi
}

legacy_plan() {
	legacy_work || return 0
	say "Found an install from before the rename to KTMS. Installing will:"
	for id in $LEGACY_IDS; do say "  - stop $(legacy_label "$id") and move its service file to $BACKUP_ROOT/"; done
	if [ -n "$LEGACY_DIR" ]; then
		if [ "$KEEP_LEGACY" = 1 ]; then say "  - leave $LEGACY_DIR in place (--keep-legacy)"; else say "  - move $LEGACY_DIR to $BACKUP_ROOT/"; fi
		plan=$(legacy_key_plan)
		[ -z "$plan" ] || say "  - its node key: $plan"
	fi
}

# stop_label stops a service; bootstrap right after bootout fails with EIO
# while the old process is still unloading, so it waits for that.
stop_label() {
	if [ "$OS" = darwin ]; then
		dom=$(mac_domain)
		launchctl bootout "$dom/$1" 2>/dev/null || true
		i=0
		while launchctl print "$dom/$1" >/dev/null 2>&1 && [ $i -lt 40 ]; do sleep 0.5; i=$((i + 1)); done
	else
		systemctl --user stop "$1.service" 2>/dev/null || true
	fi
}

# stop_legacy makes sure an old service is gone even when its manager failed to
# stop it. Only what the manager ran is killed, the wrapper (caffeinate,
# systemd-inhibit) and its children: a llama-server by name may be another's.
stop_legacy() {
	lbl=$(legacy_label "$1")
	pid=$(label_pid "$lbl")
	kids=""
	[ -z "$pid" ] || kids=$(pgrep -P "$pid" 2>/dev/null | tr '\n' ' ')
	stop_label "$lbl"
	[ "$OS" = darwin ] || systemctl --user disable "$lbl.service" >/dev/null 2>&1 || true
	for p in $kids $pid; do
		i=0
		while kill -0 "$p" 2>/dev/null && [ $i -lt 20 ]; do sleep 0.5; i=$((i + 1)); done
		if kill -0 "$p" 2>/dev/null; then
			kill "$p" 2>/dev/null || true
			sleep 2
			! kill -0 "$p" 2>/dev/null || kill -9 "$p" 2>/dev/null || true
		fi
	done
	say "Stopped $lbl."
}

has_tty() { (: </dev/tty) 2>/dev/null; }

write_key() { # key: stored 0600, written whole or not at all
	(umask 077 && printf '%s\n' "$1" >"$BASE/key.tmp") && chmod 600 "$BASE/key.tmp" && mv "$BASE/key.tmp" "$BASE/key"
}

# import_legacy_key runs before store_key and only reads: if the install stops
# later, the old node still has its key and keeps working.
import_legacy_key() {
	[ -n "$LEGACY_DIR" ] && [ -s "$LEGACY_DIR/key" ] || return 0
	[ ! -s "$BASE/key" ] && [ -z "$KEY_SRC" ] && [ "$NEW_KEY" = 0 ] || return 0
	if [ "$LEGACY_NODE_ID" != "$NODE_ID" ]; then
		if ! has_tty; then
			say "The old install's node key (node ${LEGACY_NODE_ID:-unknown}) was not reused: no terminal to ask."
			return 0
		fi
		printf 'The old install has a node key for node %s, not %s. Use it for this node? [y/N] ' "${LEGACY_NODE_ID:-unknown}" "$NODE_ID" >/dev/tty
		IFS= read -r answer </dev/tty || answer=""
		case $answer in y | Y | yes | YES) ;; *) return 0 ;; esac
	fi
	write_key "$(tr -d ' \t\r\n' <"$LEGACY_DIR/key")"
	say "Reused the old install's node key."
}

# adopt_legacy_ollama copies the old install's Ollama build instead of
# downloading one; cleanup_legacy then drops the original from the backup.
adopt_legacy_ollama() {
	[ -n "$LEGACY_DIR" ] && [ "$UPGRADE" = 0 ] && [ -z "$(find_ollama_bin || true)" ] || return 0
	for p in "$LEGACY_DIR/ollama/ollama" "$LEGACY_DIR/ollama/bin/ollama"; do
		[ -x "$p" ] || continue
		rm -rf "${BASE:?}/ollama.new" && cp -Rp "$LEGACY_DIR/ollama" "$BASE/ollama.new" && mv "$BASE/ollama.new" "$BASE/ollama"
		say "Reusing the old install's Ollama build (--upgrade-deps updates it)."
		return 0
	done
}

# cleanup_legacy retires the old install right before this node's Ollama takes
# its port, once everything else is in place: services stopped for good, their
# files and its directory moved to a dated backup outside $BASE.
cleanup_legacy() {
	legacy_work || return 0
	step "Retiring the install from before the rename"
	bk="$BACKUP_ROOT/$(date +%Y%m%d-%H%M%S)"
	mkdir -p "$bk"
	chmod 700 "$BACKUP_ROOT"
	for id in $LEGACY_IDS; do
		stop_legacy "$id"
		f=$(legacy_file "$id")
		if [ -f "$f" ]; then mv "$f" "$bk/" && say "Moved $f to $bk/."; fi
	done
	if [ "$OS" = linux ] && [ -n "$LEGACY_IDS" ]; then systemctl --user daemon-reload 2>/dev/null || true; fi
	if [ -z "$LEGACY_DIR" ] || [ "$KEEP_LEGACY" = 1 ]; then return 0; fi
	[ ! -d "$BASE/ollama" ] || rm -rf "${LEGACY_DIR:?}/ollama" # this node has its own copy
	moved="$bk/$(basename "$LEGACY_DIR")"
	mv "$LEGACY_DIR" "$moved"
	say "Moved $LEGACY_DIR to $moved."
	case $LEGACY_MODELS in "$LEGACY_DIR"/*) say "The old Ollama kept its models in $LEGACY_MODELS; they are now in $moved${LEGACY_MODELS#"$LEGACY_DIR"}." ;; esac
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

# Older cloudflared trusts the eMudhra intermediates whose keys leaked in October 2026.
CF_MIN=2026.10.3

# cf_older BIN: true when BIN reports a version below $CF_MIN; an unknown version is not.
cf_older() {
	v=$("$1" --version 2>/dev/null | sed -n 's/.*version \([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p' | head -n 1)
	[ -n "$v" ] || return 1
	[ "$(printf '%s\n%s\n' "$v" "$CF_MIN" | sort -t. -k1,1n -k2,2n -k3,3n | head -n 1)" != "$CF_MIN" ]
}

ensure_cloudflared() {
	[ -z "$ENDPOINT" ] || return 0
	CF_BIN="$BASE/bin/cloudflared"
	if [ -x "$CF_BIN" ] && [ "$UPGRADE" = 0 ]; then
		cf_older "$CF_BIN" || return 0
		say "cloudflared is older than $CF_MIN and trusts leaked eMudhra certificates: updating it."
	fi
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
	CF_CHANGED=1 # the running tunnel still uses the old binary until the node restarts
}

# stage_agent downloads, verifies and smoke-tests the agent, leaving it in $BASE/bin/ktms-node.new.
# The running binary is only replaced later, so a bad download or a blocked binary changes nothing.
stage_agent() {
	if [ "$VERSION" = latest ]; then
		tag=$(latest_tag "$REPO")
	else
		tag=$VERSION
		case $tag in v*) ;; *) tag="v$tag" ;; esac
	fi
	ver=${tag#v}
	if [ -x "$BASE/bin/ktms-node" ] && [ "$("$BASE/bin/ktms-node" --version 2>/dev/null || true)" = "$ver" ]; then
		say "Node agent $ver is already installed."
		AGENT_STAGED=0
		return 0
	fi
	step "Downloading node agent $ver"
	asset="ktms-node_${ver}_${OS}_${ARCH}.tar.gz"
	legacy_asset="llm-proxy-node_${ver}_${OS}_${ARCH}.tar.gz"
	tmp=$(mktemp -d "$BASE/dl.XXXXXX")
	curl -fsSL -o "$tmp/checksums.txt" "$GITHUB/$REPO/releases/download/$tag/checksums.txt" ||
		die "release $tag has no node agent binaries (they exist from v0.10.0 on); pass --version with a newer release"
	want=$(awk -v a="$asset" '$2 == a { print $1 }' "$tmp/checksums.txt")
	if [ -z "$want" ]; then
		want=$(awk -v a="$legacy_asset" '$2 == a { print $1 }' "$tmp/checksums.txt")
		asset="$legacy_asset"
	fi
	fetch "$GITHUB/$REPO/releases/download/$tag/$asset" "$tmp/$asset"
	verify_sha "$tmp/$asset" "$want"
	if tar -tzf "$tmp/$asset" 2>/dev/null | grep -q 'ktms-node'; then
		tar -xzf "$tmp/$asset" -C "$tmp" ktms-node
		mv "$tmp/ktms-node" "$BASE/bin/ktms-node.new"
	else
		tar -xzf "$tmp/$asset" -C "$tmp" llm-proxy-node
		mv "$tmp/llm-proxy-node" "$BASE/bin/ktms-node.new"
	fi
	rm -rf "$tmp"
	chmod 755 "$BASE/bin/ktms-node.new"
	if [ "$OS" = darwin ]; then xattr -dr com.apple.quarantine "$BASE/bin" 2>/dev/null || true; fi
	got=$("$BASE/bin/ktms-node.new" --version 2>&1) || {
		rm -f "$BASE/bin/ktms-node.new"
		die "the new node agent does not start on this machine ($got). The installed one was left untouched."
	}
	say "Downloaded and verified node agent $got."
	AGENT_STAGED=1
}

# agent_bin is the agent this run will leave installed: the staged one, else the current one.
agent_bin() {
	if [ "${AGENT_STAGED:-0}" = 1 ]; then echo "$BASE/bin/ktms-node.new"; else echo "$BASE/bin/ktms-node"; fi
}

# agent_has_schedule tells whether an agent binary knows schedules; older ones
# exit on the unknown --schedule-file flag, so it is only passed to newer ones.
# An empty NODE_KEY keeps an older agent from starting for real on the probe.
agent_has_schedule() { # binary
	[ -x "$1" ] && NODE_KEY="" "$1" schedule --file /dev/null --quiet </dev/null >/dev/null 2>&1
}

# agent_has_stats tells whether an agent binary keeps a usage log (`stats`).
agent_has_stats() { # binary
	[ -x "$1" ] && NODE_KEY="" "$1" stats --dir "$BASE/stats" --since 1m </dev/null >/dev/null 2>&1
}

# check_schedule validates a schedule file with the agent's own parser, or dies.
check_schedule() { # binary file what
	out=$("$1" schedule --file "$2" 2>&1) || die "invalid $3: ${out#"schedule: $2: "}"
}

# install_schedule checks --schedule (or the schedule already installed) and
# copies it into place, keeping the old one for a rollback. It runs before any
# service is touched, so a bad file changes nothing.
install_schedule() {
	bin=$(agent_bin)
	if [ -z "$SCHEDULE_SRC" ]; then
		[ -f "$BASE/schedule" ] && agent_has_schedule "$bin" || return 0
		check_schedule "$bin" "$BASE/schedule" "$BASE/schedule (fix it, or pass a new one with --schedule FILE)"
		return 0
	fi
	agent_has_schedule "$bin" || die "this node agent version does not support --schedule; install a newer one with --version"
	check_schedule "$bin" "$SCHEDULE_SRC" "--schedule $SCHEDULE_SRC"
	rm -f "$BASE/schedule.prev"
	[ -f "$BASE/schedule" ] && cp -p "$BASE/schedule" "$BASE/schedule.prev"
	cp "$SCHEDULE_SRC" "$BASE/schedule.tmp" && chmod 644 "$BASE/schedule.tmp" && mv "$BASE/schedule.tmp" "$BASE/schedule"
	SCHEDULE_REPLACED=1
	say "Schedule installed in $BASE/schedule: $out."
}

# restore_schedule puts back the schedule a failed install replaced.
restore_schedule() {
	[ "$SCHEDULE_REPLACED" = 1 ] || return 0
	if [ -f "$BASE/schedule.prev" ]; then mv "$BASE/schedule.prev" "$BASE/schedule"; else rm -f "$BASE/schedule"; fi
	warn "Restored the previous schedule; the new one was not applied."
}

# schedule_status prints the level now and the next switch, or nothing when unknown.
schedule_status() { # binary
	[ -f "$BASE/schedule" ] && agent_has_schedule "$1" || return 0
	"$1" schedule --file "$BASE/schedule" 2>/dev/null || true
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
	case $(schedule_status "$(agent_bin)") in
	"now 0%"*)
		say "The schedule pauses the node now: $MODEL is not loaded; the agent loads it before the next window."
		return 0
		;;
	esac
	step "Loading $MODEL into memory"
	curl -fsS -m 900 "http://$OLLAMA_ADDR/api/generate" -d "{\"model\":\"$MODEL\",\"keep_alive\":-1}" >/dev/null || warn "could not preload $MODEL; the first request will be slow"
}

write_service() { # id program args... (stdin: nothing)
	id=$1
	shift
	out=$(mktemp "$BASE/svc.XXXXXX")
	if [ "$id" = node ]; then log="$BASE/logs/node.log" envs="" desc="KTMS node agent"; else log="$BASE/logs/ollama.log" envs=$(ollama_env) desc="KTMS Ollama"; fi
	if [ "$OS" = darwin ]; then render_plist "$(svc_label "$id")" "$log" "$envs" "$@" >"$out"; else render_unit "$desc" "$log" "$envs" "$@" >"$out"; fi
	SVC_TMP=$out
}

# write_node_service renders the agent's service for the flags the given binary knows.
write_node_service() { # binary
	bin=$1
	set -- "$BASE/bin/ktms-node" --proxy-addr "$PROXY" --node-id "$NODE_ID" --node-name "$NAME" \
		--engine ollama --engine-url "http://$OLLAMA_ADDR" --only-model "$MODEL" --max-concurrent "$MAXC" --node-key-file "$BASE/key"
	[ "$PLAINTEXT" = 1 ] || set -- "$@" --tls
	if [ -n "$ENDPOINT" ]; then
		set -- "$@" --endpoint "$ENDPOINT" --expose none
	else
		set -- "$@" --expose cloudflared --cloudflared-path "$BASE/bin/cloudflared"
	fi
	# Always passed, file or not: a schedule created later needs no reinstall.
	agent_has_schedule "$bin" && set -- "$@" --schedule-file "$BASE/schedule"
	# Explicit: the agent's own default follows $KTMS_HOME, which its service does not set.
	agent_has_stats "$bin" && set -- "$@" --stats-dir "$BASE/stats"
	# Keep the machine awake while the node runs.
	if [ "$OS" = darwin ]; then
		set -- /usr/bin/caffeinate -is "$@"
	elif command -v systemd-inhibit >/dev/null 2>&1; then
		set -- systemd-inhibit --what=idle:sleep --who=ktms-node --why="Serving LLM requests" --mode=block "$@"
	fi
	write_service node "$@"
}

# run_doctor explains why the node is not working, in plain language. Returns the agent's status.
run_doctor() {
	step "Diagnosing"
	set -- doctor --proxy-addr "$PROXY" --node-id "$NODE_ID" --node-key-file "$BASE/key" \
		--engine ollama --engine-url "http://$OLLAMA_ADDR" --only-model "$MODEL" --model "$MODEL"
	[ "$PLAINTEXT" = 1 ] || set -- "$@" --tls
	agent_has_schedule "$BASE/bin/ktms-node" && set -- "$@" --schedule-file "$BASE/schedule"
	if [ -n "$ENDPOINT" ]; then
		set -- "$@" --endpoint "$ENDPOINT" --expose none
	else
		set -- "$@" --expose cloudflared --cloudflared-path "$BASE/bin/cloudflared"
	fi
	"$BASE/bin/ktms-node" "$@"
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
	# Old services would keep serving under this node's id; the old directory is
	# the user's to remove, so it stays.
	KEEP_LEGACY=1
	detect_legacy
	cleanup_legacy
	step "Removing the node"
	for id in node ollama; do
		svc_stop "$id"
		if [ "$OS" = linux ]; then systemctl --user disable "$(svc_label "$id").service" >/dev/null 2>&1 || true; fi
		rm -f "$(svc_file "$id")"
	done
	if [ "$OS" = linux ]; then systemctl --user daemon-reload 2>/dev/null || true; fi
	rm -rf "${BASE:?}"
	say "Removed services, binaries, logs, the usage log and the stored node key."
	if [ -d "$BACKUP_ROOT" ]; then
		if [ "$PURGE_BACKUPS" = 1 ]; then
			rm -rf "${BACKUP_ROOT:?}"
			say "Removed the backups of old installs in $BACKUP_ROOT."
		else
			say "Backups of old installs stay in $BACKUP_ROOT (--purge-backups removes them)."
		fi
	fi
	say "Downloaded models stay in ~/.ollama/models; delete them there if you want the space back."
}

print_status() {
	# Re-registration after a tunnel restart logs the newer address, so take the latest of both.
	ep=$(sed -n -e 's/.*node registered successfully.*endpoint=\([^ ]*\).*/\1/p' -e 's/.*node re-registered.*endpoint=\([^ ]*\).*/\1/p' "$BASE/logs/node.log" 2>/dev/null | tail -1 | sed "s/$(printf '\033')\[[0-9;]*m//g")
	engine="Ollama"
	[ "$OS" = darwin ] && [ "$ARCH" = arm64 ] && engine="Ollama (Apple GPU)"
	sched=$(schedule_status "$BASE/bin/ktms-node")
	if [ -z "$sched" ]; then
		sched="none, 100% all the time (add one with --schedule FILE)"
		[ -f "$BASE/schedule" ] && sched="see $BASE/schedule"
	fi
	stats="needs a newer node agent"
	agent_has_stats "$BASE/bin/ktms-node" && stats="$BASE/bin/ktms-node stats --since 24h (log in $BASE/stats)"
	cat <<EOF

Node $NODE_ID is running and registered.
  Proxy:   $PROXY
  Engine:  $engine on $OLLAMA_ADDR, model $MODEL
  Address: ${ep:-see $BASE/logs/node.log}
  Schedule: $sched
  Stats:   $stats
  Logs:    $BASE/logs/node.log
  Update:  run the same command again
  Remove:  re-run with --uninstall
EOF
	if [ "$OS" = darwin ]; then
		say "  Note:    on a Mac the node starts when you log in. For an unattended Mac turn on automatic login (System Settings > Users & Groups)."
	fi
}

main() {
	parse_args "$@"
	[ "$UNINSTALL" = 1 ] && { do_uninstall; return 0; }
	detect_platform
	if [ "$DOCTOR" = 1 ]; then
		[ -x "$BASE/bin/ktms-node" ] || die "no node is installed in $BASE; run the installer without --doctor first"
		run_doctor
		return $?
	fi
	step "Checking this machine ($OS/$ARCH)"
	note_foreign_ollama
	check_resources
	detect_legacy
	legacy_plan
	if [ "$DRY_RUN" = 1 ]; then
		if [ -n "$SCHEDULE_SRC" ]; then
			if agent_has_schedule "$BASE/bin/ktms-node"; then
				check_schedule "$BASE/bin/ktms-node" "$SCHEDULE_SRC" "--schedule $SCHEDULE_SRC"
				say "Schedule $SCHEDULE_SRC is valid: $out."
			else
				say "Schedule $SCHEDULE_SRC will be checked by the downloaded agent before anything changes."
			fi
		fi
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
	OLLAMA_CHANGED=0 CF_CHANGED=0 AGENT_STAGED=0 SCHEDULE_REPLACED=0
	import_legacy_key
	store_key
	stage_agent
	install_schedule
	adopt_legacy_ollama
	ensure_ollama
	ensure_cloudflared

	step "Starting the inference engine"
	if [ -n "$RUNNER" ]; then write_service ollama /usr/bin/arch -arm64 "$OLLAMA_BIN" serve; else write_service ollama "$OLLAMA_BIN" serve; fi
	cleanup_legacy
	check_ollama_port
	if install_service ollama "$SVC_TMP" || [ "$OLLAMA_CHANGED" = 1 ] || ! svc_running ollama; then svc_start ollama; fi
	wait_http "http://$OLLAMA_ADDR/api/version" 60 || die "Ollama did not start; see $BASE/logs/ollama.log"
	check_ollama_port # whoever answered must be this node's Ollama
	ensure_model

	step "Starting the node"
	node_changed=$CF_CHANGED
	if [ "$AGENT_STAGED" = 1 ]; then
		svc_stop node
		[ -x "$BASE/bin/ktms-node" ] && cp -p "$BASE/bin/ktms-node" "$BASE/bin/ktms-node.prev"
		mv "$BASE/bin/ktms-node.new" "$BASE/bin/ktms-node"
		node_changed=1
	fi
	write_node_service "$BASE/bin/ktms-node"
	install_service node "$SVC_TMP" && node_changed=1
	if [ "$node_changed" = 1 ] || ! svc_running node; then
		if ! start_node_and_wait; then
			run_doctor || true # best effort: an older agent may not have it
			if [ "$AGENT_STAGED" = 1 ] && [ -x "$BASE/bin/ktms-node.prev" ]; then
				warn "The new node agent did not come up; restoring the previous one."
				svc_stop node
				mv "$BASE/bin/ktms-node.prev" "$BASE/bin/ktms-node"
				restore_schedule
				write_node_service "$BASE/bin/ktms-node" # its flags, not the new agent's
				install_service node "$SVC_TMP" || true
				svc_start node || true
			fi
			tail -n 15 "$BASE/logs/node.log" 2>/dev/null >&2 || true
			die "the node did not register; the diagnosis above says why. Run it again any time: $BASE/bin/ktms-node doctor --help. Log: $BASE/logs/node.log"
		fi
	fi
	rm -f "$BASE/schedule.prev"
	print_status
}

[ "${INSTALL_NODE_SOURCED:-0}" = 1 ] || main "$@"
