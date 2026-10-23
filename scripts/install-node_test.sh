#!/bin/sh
# shellcheck disable=SC1090,SC2016,SC2034,SC2086
# (sources the script by path, splits $ARGS on purpose, writes stubs whose $1 is theirs)
# Tests for install-node.sh. Nothing touches the real machine: a throwaway HOME,
# stubbed launchctl/systemctl/uname and a fake GitHub on localhost.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/install-node.sh"
PASS=0 FAIL=0

ok() { PASS=$((PASS + 1)); printf 'ok   %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf 'FAIL %s\n' "$1"; }
eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want '$2', got '$3')"; fi; }
has() { case $3 in *"$2"*) ok "$1" ;; *) bad "$1 (missing '$2' in: $(printf '%s' "$3" | head -c 300))" ;; esac; }
hasnt() { case $3 in *"$2"*) bad "$1 (unexpected '$2')" ;; *) ok "$1" ;; esac; }

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }

# ---------------------------------------------------------------- unit tests
unit() { ( INSTALL_NODE_SOURCED=1; . "$SCRIPT"; "$@" ); }

eq "model size 26b qat is 4-bit" "13.0" "$(unit model_weights_gb gemma4:26b-a4b-it-qat)"
eq "model size q8" "8.4" "$(unit model_weights_gb llama3.1:8b-instruct-q8_0)"
eq "model size fp16" "16.4" "$(unit model_weights_gb llama3.1:8b-instruct-fp16)"
eq "model size 8b" "4.8" "$(unit model_weights_gb llama3.1:8b)"
eq "model size moe tag" "18.0" "$(unit model_weights_gb qwen3:30b-a3b)"
eq "model size latest tag" "" "$(unit model_weights_gb llama3:latest)"
eq "model size no tag" "" "$(unit model_weights_gb gemma)"

parse() { unit parse_args "$@" 2>&1; }
has "missing --proxy" "--proxy is required" "$(parse --node-id n --model m)"
has "missing --node-id" "--node-id is required" "$(parse --proxy p --model m)"
has "bad node id" "--node-id may only contain" "$(parse --proxy p --node-id 'a b' --model m)"
has "bad concurrency" "--max-concurrency" "$(parse --proxy p --node-id n --model m --max-concurrency 0)"
has "unknown option" "unknown option" "$(parse --bogus)"
has "option without value" "needs a value" "$(parse --proxy)"
eq "uninstall needs nothing" "" "$(parse --uninstall)"
eq "= form, default port, spaces in name" "p.example.com:443|n1|m:1b|My Node" \
	"$(INSTALL_NODE_SOURCED=1; . "$SCRIPT"; parse_args --proxy=p.example.com --node-id=n1 --model=m:1b --name="My Node"; echo "$PROXY|$NODE_ID|$MODEL|$NAME")"

eq "xml escaping" "a&amp;b &lt;c&gt;" "$(unit xml_esc 'a&b <c>')"
eq "systemd quoting" '"a \"b\" 100%%"' "$(unit sd_quote 'a "b" 100%')"

PLIST=$(unit render_plist com.x.y "/tmp/log & more.log" "OLLAMA_HOST=127.0.0.1:1
OLLAMA_KEEP_ALIVE=-1" /usr/bin/arch -arm64 "/Users/a b/ollama" serve)
has "plist label" "<key>Label</key><string>com.x.y</string>" "$PLIST"
has "plist arg with space" "<string>/Users/a b/ollama</string>" "$PLIST"
has "plist env" "<key>OLLAMA_KEEP_ALIVE</key><string>-1</string>" "$PLIST"
has "plist escaped log path" "/tmp/log &amp; more.log" "$PLIST"
has "plist keepalive" "<key>KeepAlive</key><true/>" "$PLIST"
if command -v plutil >/dev/null 2>&1; then
	printf '%s' "$PLIST" >"${TMPDIR:-/tmp}/it.$$.plist"
	if plutil -lint "${TMPDIR:-/tmp}/it.$$.plist" >/dev/null 2>&1; then ok "plist lints"; else bad "plist lints"; fi
	rm -f "${TMPDIR:-/tmp}/it.$$.plist"
fi
UNIT=$(unit render_unit "desc" /tmp/n.log "A=1" systemd-inhibit "--why=Serving LLM requests" "/opt/my dir/node" --x)
has "unit exec quoting" 'ExecStart="systemd-inhibit" "--why=Serving LLM requests" "/opt/my dir/node" "--x"' "$UNIT"
has "unit env" 'Environment="A=1"' "$UNIT"
has "unit restart" "Restart=always" "$UNIT"

# legacy services are read back from what render_plist / render_unit wrote
legacy_plist=$(unit render_plist com.llm-proxy.node /tmp/n.log "OLLAMA_MODELS=/Users/a b/.llm-proxy-node/models" /usr/bin/caffeinate -is /x/llm-proxy-node --proxy-addr p:443 --node-id old-node --tls)
legacy_unit=$(unit render_unit d /tmp/n.log "OLLAMA_MODELS=/home/a/.llm-proxy-node/models" systemd-inhibit "--why=Serving LLM requests" /x/llm-proxy-node --node-id old-node --tls)
printf '%s' "$legacy_plist" >"${TMPDIR:-/tmp}/it.$$.plist"
printf '%s' "$legacy_unit" >"${TMPDIR:-/tmp}/it.$$.service"
eq "plist node id read back" "old-node" "$(unit service_arg "${TMPDIR:-/tmp}/it.$$.plist" --node-id)"
eq "unit node id read back" "old-node" "$(unit service_arg "${TMPDIR:-/tmp}/it.$$.service" --node-id)"
eq "plist env read back" "/Users/a b/.llm-proxy-node/models" "$(unit service_env "${TMPDIR:-/tmp}/it.$$.plist" OLLAMA_MODELS)"
eq "unit env read back" "/home/a/.llm-proxy-node/models" "$(unit service_env "${TMPDIR:-/tmp}/it.$$.service" OLLAMA_MODELS)"
eq "absent flag reads empty" "" "$(unit service_arg "${TMPDIR:-/tmp}/it.$$.service" --model)"
rm -f "${TMPDIR:-/tmp}/it.$$.plist" "${TMPDIR:-/tmp}/it.$$.service"
has "--purge-backups needs --uninstall" "only goes with --uninstall" "$(parse --proxy p --node-id n --model m --purge-backups)"

# print_status shows the newest address: re-registration follows a tunnel restart
psdir="${TMPDIR:-/tmp}/it.$$.status"
mkdir -p "$psdir/logs"
printf 'INF node registered successfully endpoint=https://old.trycloudflare.com interval=5s\nINF tunnel address changed endpoint=https://new.trycloudflare.com\nINF node re-registered endpoint=https://new.trycloudflare.com\n' >"$psdir/logs/node.log"
status() { ( INSTALL_NODE_SOURCED=1; . "$SCRIPT"; BASE=$psdir OS=linux ARCH=amd64 NODE_ID=n PROXY=p OLLAMA_ADDR=a MODEL=m; print_status ); }
has "status uses the re-registered address" "Address: https://new.trycloudflare.com" "$(status)"
printf 'INF node re-registered endpoint=https://new.trycloudflare.com\nINF node registered successfully endpoint=https://first.trycloudflare.com interval=5s\n' >"$psdir/logs/node.log"
has "status uses the latest line of either kind" "Address: https://first.trycloudflare.com" "$(status)"
rm -rf "$psdir"

sumfile="${TMPDIR:-/tmp}/it.$$.bin"
printf 'x' >"$sumfile"
good=$(sha "$sumfile")
if unit verify_sha "$sumfile" "$good"; then ok "verify_sha accepts match"; else bad "verify_sha accepts match"; fi
has "verify_sha rejects mismatch" "checksum mismatch" "$(unit verify_sha "$sumfile" deadbeef 2>&1)"
has "verify_sha refuses empty" "refusing to install" "$(unit verify_sha "$sumfile" '' 2>&1)"
rm -f "$sumfile"

# -------------------------------------------------------------- fake world
if ! command -v python3 >/dev/null 2>&1; then
	printf 'python3 missing: skipping end-to-end tests\n'
	printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
	[ "$FAIL" = 0 ]
	exit
fi

W=$(mktemp -d "${TMPDIR:-/tmp}/install-node-test.XXXXXX")
SRV_PID=""
cleanup() {
	[ -z "$SRV_PID" ] || kill "$SRV_PID" 2>/dev/null
	rm -rf "${W:?}"
}
trap cleanup EXIT INT TERM

cat >"$W/server.py" <<'PY'
import hashlib, http.server, json, os, sys, threading
FX, STATE, P1, P2 = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])

class GH(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_HEAD(self): self.do_GET()
    def send(self, code, body=b"", ctype="text/plain", extra=None):
        self.send_response(code)
        self.send_header("Content-Type", ctype); self.send_header("Content-Length", str(len(body)))
        for k, v in (extra or {}).items(): self.send_header(k, v)
        self.end_headers()
        if self.command != "HEAD": self.wfile.write(body)
    def do_GET(self):
        p = self.path.strip("/").split("/")
        if len(p) == 4 and p[2] == "releases" and p[3] == "latest":
            tag = open(f"{FX}/{p[0]}/{p[1]}/LATEST").read().strip()
            return self.send(302, extra={"Location": f"/{p[0]}/{p[1]}/releases/tag/{tag}"})
        if len(p) == 5 and p[2] == "releases" and p[3] == "tag": return self.send(200, b"release")
        if len(p) == 6 and p[0] == "repos" and p[3] == "releases" and p[4] == "tags":
            d = f"{FX}/{p[1]}/{p[2]}/{p[5]}"
            if not os.path.isdir(d): return self.send(404)
            assets = [{"name": n, "digest": "sha256:" + hashlib.sha256(open(f"{d}/{n}", "rb").read()).hexdigest()} for n in sorted(os.listdir(d))]
            return self.send(200, json.dumps({"tag_name": p[5], "assets": assets}, indent=2).encode(), "application/json")
        if len(p) == 6 and p[2] == "releases" and p[3] == "download":
            f = f"{FX}/{p[0]}/{p[1]}/{p[4]}/{p[5]}"
            if not os.path.isfile(f): return self.send(404)
            return self.send(200, open(f, "rb").read(), "application/octet-stream")
        self.send(404)

class Engine(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, body):
        b = json.dumps(body, separators=(",", ":")).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def up(self):
        return any(os.path.exists(f"{STATE}/{m}") for m in ("running.ktms-ollama.service", "loaded.com.ktms.ollama")) or os.path.exists(f"{STATE}/foreign-ollama")
    def do_GET(self):
        if not self.up():
            self.send_response(503); self.send_header("Content-Length", "0"); self.end_headers(); return
        if self.path == "/api/version": return self.send({"version": "0.0.0"})
        if self.path == "/api/tags":
            names = [l.strip() for l in open(f"{STATE}/models")] if os.path.exists(f"{STATE}/models") else []
            return self.send({"models": [{"name": n, "model": n} for n in names if n]})
        self.send({})
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0))); self.send({})

for port, h in ((P1, GH), (P2, Engine)):
    s = http.server.ThreadingHTTPServer(("127.0.0.1", port), h)
    threading.Thread(target=s.serve_forever, daemon=True).start()
print("ready", flush=True)
threading.Event().wait()
PY

free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
P1=$(free_port)
P2=$(free_port)
FX="$W/fx"
STATE="$W/state"
mkdir -p "$FX" "$STATE" "$W/bin" "$W/home"
python3 "$W/server.py" "$FX" "$STATE" "$P1" "$P2" >"$W/server.log" 2>&1 &
SRV_PID=$!
i=0
while ! grep -q ready "$W/server.log" 2>/dev/null && [ $i -lt 50 ]; do
	sleep 0.1
	i=$((i + 1))
done

# agent release fixtures. modes: good | broken_start (--version fails) | never_registers
make_agent() { # version mode os arch
	d="$FX/ai-process/ktms/v$1"
	mkdir -p "$d" "$W/pack"
	case $2 in
	broken_start) body='[ "$1" = --version ] && exit 1; exit 0' ;;
	never_registers) body="[ \"\$1\" = doctor ] && echo DOCTOR-RAN:\$*
[ \"\$1\" = --version ] && { echo $1; exit 0; } # BROKEN
exit 0" ;;
	*) body="[ \"\$1\" = --version ] && { echo $1; exit 0; }; exit 0" ;;
	esac
	printf '#!/bin/sh\n%s\n' "$body" >"$W/pack/ktms-node"
	chmod +x "$W/pack/ktms-node"
	asset="ktms-node_$1_$3_$4.tar.gz"
	tar -czf "$d/$asset" -C "$W/pack" ktms-node
	printf '%s  %s\n' "$(sha "$d/$asset")" "$asset" >>"$d/checksums.txt"
	printf 'v%s\n' "$1" >"$FX/ai-process/ktms/LATEST"
}

fake_ollama_bin() {
	printf '#!/bin/sh\ncase "$1" in\nserve) exec sleep 300;;\npull) echo "$2" >> "$FAKE_STATE/models";;\n--version) echo 0.0.0;;\nesac\n' >"$1"
	chmod +x "$1"
}
fake_cloudflared_bin() {
	printf '#!/bin/sh\n[ "$1" = --version ] && echo cloudflared\nexit 0\n' >"$1"
	chmod +x "$1"
}

# dependency releases (Ollama only ships a darwin tarball here; Linux tests pre-seed it)
make_deps() { # arch
	o="$FX/ollama/ollama/v9.9.9"
	c="$FX/cloudflare/cloudflared/2099.1.0"
	mkdir -p "$o" "$c" "$W/pack2"
	printf 'v9.9.9\n' >"$FX/ollama/ollama/LATEST"
	printf '2099.1.0\n' >"$FX/cloudflare/cloudflared/LATEST"
	fake_ollama_bin "$W/pack2/ollama"
	tar -czf "$o/ollama-darwin.tgz" -C "$W/pack2" ollama
	fake_cloudflared_bin "$W/pack2/cloudflared"
	tar -czf "$c/cloudflared-darwin-$1.tgz" -C "$W/pack2" cloudflared
	cp "$W/pack2/cloudflared" "$c/cloudflared-linux-$1"
}

make_stubs() {
	b="$W/bin"
	cat >"$b/uname" <<'S'
#!/bin/sh
case "$1" in -s) echo "$FAKE_OS" ;; -m) echo "$FAKE_ARCH" ;; *) echo "$FAKE_OS" ;; esac
S
	cat >"$b/sysctl" <<'S'
#!/bin/sh
case "$2" in hw.optional.arm64) echo 1 ;; hw.memsize) echo "${FAKE_MEMSIZE:-34359738368}" ;; *) exit 1 ;; esac
S
	cat >"$b/systemctl" <<'S'
#!/bin/sh
echo "systemctl $*" >>"$FAKE_STATE/calls"
[ "$1" = --user ] && shift
case "$1" in
show) # show -p MainPID --value UNIT
	u=$5
	if [ ! -f "$FAKE_STATE/running.$u" ]; then echo 0
	elif [ "$u" = ktms-ollama.service ]; then echo 4242
	else cat "$FAKE_STATE/pid.$u" 2>/dev/null || echo 0; fi ;;
stop) rm -f "$FAKE_STATE/running.$2" ;;
restart)
	touch "$FAKE_STATE/running.$2"
	if [ "$2" = ktms-node.service ] && ! grep -q BROKEN "$LLMPROXY_HOME/bin/ktms-node"; then
		echo "INF node registered successfully endpoint=https://fake-abc.trycloudflare.com interval=5s" >>"$LLMPROXY_HOME/logs/node.log"
	fi ;;
is-active) [ -f "$FAKE_STATE/running.$3" ] || exit 3 ;;
esac
exit 0
S
	cat >"$b/launchctl" <<'S'
#!/bin/sh
echo "launchctl $*" >>"$FAKE_STATE/calls"
case "$1" in
print)
	case "$2" in
	*/*/*)
		l=${2##*/}
		[ -f "$FAKE_STATE/loaded.$l" ] || exit 113
		echo "state = running"
		if [ "$l" = com.ktms.ollama ]; then echo "pid = 4242"; elif [ -f "$FAKE_STATE/pid.$l" ]; then echo "pid = $(cat "$FAKE_STATE/pid.$l")"; fi
		exit 0 ;;
	*) exit 0 ;;
	esac ;;
bootout) rm -f "$FAKE_STATE/loaded.${2##*/}" ;;
bootstrap)
	l=$(basename "$3" .plist)
	touch "$FAKE_STATE/loaded.$l"
	if [ "$l" = com.ktms.node ] && ! grep -q BROKEN "$LLMPROXY_HOME/bin/ktms-node"; then
		echo "INF node registered successfully endpoint=https://fake-abc.trycloudflare.com interval=5s" >>"$LLMPROXY_HOME/logs/node.log"
	fi ;;
esac
exit 0
S
	# The listener on the Ollama port: a foreign process when a test says so,
	# else this node's Ollama once its service runs (the fake engine plays it).
	cat >"$b/lsof" <<'S'
#!/bin/sh
port=$(printf '%s\n' "$@" | sed -n 's/^-iTCP://p')
if [ -n "${FAKE_FOREIGN_PID:-}" ]; then printf 'p%s\nn%s:%s\n' "$FAKE_FOREIGN_PID" "${FAKE_FOREIGN_HOST:-127.0.0.1}" "$port"; exit 0; fi
[ -f "$FAKE_STATE/running.ktms-ollama.service" ] || [ -f "$FAKE_STATE/loaded.com.ktms.ollama" ] || exit 1
printf 'p4242\nn*:%s\n' "$port"
S
	for n in loginctl systemd-inhibit xattr; do printf '#!/bin/sh\nexit 0\n' >"$b/$n"; done
	chmod +x "$b"/*
}

run_install() {
	HOME="$W/home" LLMPROXY_HOME="${TEST_BASE:-$W/home/.ktms-node}" LLMPROXY_SERVICE_DIR="$W/svc" \
		LLMPROXY_GITHUB="http://127.0.0.1:$P1" LLMPROXY_GITHUB_API="http://127.0.0.1:$P1" \
		LLMPROXY_OLLAMA_ADDR="127.0.0.1:$P2" LLMPROXY_REGISTER_WAIT=12 LLMPROXY_MEMINFO="$W/meminfo" \
		FAKE_OS="$FAKE_OS" FAKE_ARCH="$FAKE_ARCH" FAKE_STATE="$STATE" FAKE_FOREIGN_PID="${FAKE_FOREIGN_PID:-}" FAKE_FOREIGN_HOST="${FAKE_FOREIGN_HOST:-}" PATH="$W/bin:$PATH" \
		sh "$SCRIPT" "$@" 2>&1
}

reset_world() {
	rm -rf "${W:?}/home" "${W:?}/svc" "${STATE:?}" "${FX:?}" "${W:?}/pack" "${W:?}/pack2"
	mkdir -p "$W/home" "$STATE" "$FX"
	printf 'ktms_node_test_key\n' >"$W/keyfile"
}
BASE="$W/home/.ktms-node"
ARGS="--proxy llm-nodes.example.com --node-id node-9 --model tiny:1b --key-file $W/keyfile"
count_calls() { grep -c -E "$1" "$STATE/calls" 2>/dev/null || true; }
mode_of() { if [ "$(uname -s)" = Darwin ]; then stat -f '%Lp' "$1"; else stat -c '%a' "$1"; fi; }

printf "MemTotal:       33554432 kB\n" >"$W/meminfo"
make_stubs

# ---------------------------------------------------------- linux end to end
FAKE_OS=Linux FAKE_ARCH=x86_64
reset_world
make_deps amd64
make_agent 0.10.0 good linux amd64
mkdir -p "$BASE/ollama/bin"
fake_ollama_bin "$BASE/ollama/bin/ollama" # zstd is not assumed on the test machine
OUT=$(run_install $ARGS --max-concurrency 2 --context 4096)
RC=$?
eq "linux: fresh install exits 0" 0 "$RC"
[ "$RC" = 0 ] || printf '%s\n' "$OUT" | tail -15
has "linux: reports the node running" "Node node-9 is running and registered" "$OUT"
hasnt "linux: no macOS login note on Linux" "automatic login" "$OUT"
has "linux: reports the tunnel address" "https://fake-abc.trycloudflare.com" "$OUT"
eq "linux: key stored with 0600" 600 "$(mode_of "$BASE/key")"
eq "linux: key content" "ktms_node_test_key" "$(cat "$BASE/key")"
eq "linux: agent installed" "0.10.0" "$("$BASE/bin/ktms-node" --version)"
if [ -x "$BASE/bin/cloudflared" ]; then ok "linux: cloudflared installed and verified"; else bad "linux: cloudflared installed"; fi
eq "linux: model pulled" "tiny:1b" "$(cat "$STATE/models")"
NODE_UNIT=$(cat "$W/svc/ktms-node.service")
OLL_UNIT=$(cat "$W/svc/ktms-ollama.service")
has "linux: node keeps machine awake" "systemd-inhibit" "$NODE_UNIT"
has "linux: node uses built-in tunnel" '"--expose" "cloudflared"' "$NODE_UNIT"
has "linux: node reads key from file" "--node-key-file" "$NODE_UNIT"
hasnt "linux: key never in the unit" "ktms_node_test_key" "$NODE_UNIT"
has "linux: tls on by default" '"--tls"' "$NODE_UNIT"
has "linux: node reports only the chosen model" '"--only-model" "tiny:1b"' "$NODE_UNIT"
has "linux: ollama keeps model loaded" 'OLLAMA_KEEP_ALIVE=-1' "$OLL_UNIT"
has "linux: ollama parallel follows concurrency" 'OLLAMA_NUM_PARALLEL=2' "$OLL_UNIT"
has "linux: ollama context" 'OLLAMA_CONTEXT_LENGTH=4096' "$OLL_UNIT"
has "linux: ollama private address" "OLLAMA_HOST=127.0.0.1:$P2" "$OLL_UNIT"

BEFORE=$(count_calls 'restart|stop')
OUT=$(run_install $ARGS --max-concurrency 2 --context 4096)
RC=$?
eq "linux: re-run exits 0" 0 "$RC"
eq "linux: re-run touches no running service" "$BEFORE" "$(count_calls 'restart|stop')"
has "linux: re-run says agent up to date" "already installed" "$OUT"

CALLS=$(count_calls 'restart|stop')
OUT=$(run_install $ARGS --max-concurrency 2 --context 4096 --doctor)
RC=$?
eq "linux: --doctor exits with the doctor's status" 0 "$RC"
eq "linux: --doctor touches no service" "$CALLS" "$(count_calls 'restart|stop')"
OUT=$(run_install --proxy llm-nodes.example.com --node-id other --model tiny:1b --doctor --key-file "$W/keyfile")
RC=$?
eq "linux: --doctor passes the stored settings" 0 "$RC"

make_agent 0.11.0 good linux amd64
OLL_BEFORE=$(count_calls 'restart ktms-ollama')
OUT=$(run_install $ARGS --max-concurrency 2 --context 4096 --version v0.11.0)
RC=$?
eq "linux: upgrade exits 0" 0 "$RC"
eq "linux: upgraded" "0.11.0" "$("$BASE/bin/ktms-node" --version)"
eq "linux: upgrade does not restart Ollama" "$OLL_BEFORE" "$(count_calls 'restart ktms-ollama')"
eq "linux: previous binary kept" "0.10.0" "$("$BASE/bin/ktms-node.prev" --version)"

make_agent 0.12.0 broken_start linux amd64
OUT=$(run_install $ARGS --max-concurrency 2 --context 4096 --version v0.12.0)
RC=$?
if [ "$RC" != 0 ]; then ok "linux: blocked binary fails the install"; else bad "linux: blocked binary fails the install"; fi
has "linux: blocked binary explained" "does not start on this machine" "$OUT"
eq "linux: blocked binary leaves the working one" "0.11.0" "$("$BASE/bin/ktms-node" --version)"

make_agent 0.13.0 never_registers linux amd64
OUT=$(run_install $ARGS --max-concurrency 2 --context 4096 --version v0.13.0)
RC=$?
if [ "$RC" != 0 ]; then ok "linux: agent that never registers fails"; else bad "linux: agent that never registers fails"; fi
has "linux: rollback announced" "restoring the previous one" "$OUT"
has "linux: failed start runs the doctor" "DOCTOR-RAN:doctor --proxy-addr llm-nodes.example.com:443" "$OUT"
has "linux: doctor gets the model and the key file, never the key" "--model tiny:1b" "$OUT"
has "linux: doctor reports only the chosen model" "--only-model tiny:1b" "$OUT"
hasnt "linux: doctor output never shows the key" "ktms_node_test_key" "$OUT"
eq "linux: rolled back to the working version" "0.11.0" "$("$BASE/bin/ktms-node" --version)"

make_agent 0.14.0 good linux amd64
printf 'tampered' >>"$FX/ai-process/ktms/v0.14.0/ktms-node_0.14.0_linux_amd64.tar.gz"
OUT=$(run_install $ARGS --max-concurrency 2 --context 4096 --version v0.14.0)
RC=$?
if [ "$RC" != 0 ]; then ok "linux: tampered download fails"; else bad "linux: tampered download fails"; fi
has "linux: tampered download named" "checksum mismatch" "$OUT"
eq "linux: tampered download changes nothing" "0.11.0" "$("$BASE/bin/ktms-node" --version)"

OUT=$(TEST_BASE="$W/nothing-installed" run_install $ARGS --doctor)
RC=$?
if [ "$RC" != 0 ]; then ok "--doctor without an install fails"; else bad "--doctor without an install fails"; fi
has "--doctor without an install says so" "no node is installed" "$OUT"

OUT=$(run_install --uninstall)
RC=$?
eq "linux: uninstall exits 0" 0 "$RC"
has "linux: uninstall disables the units" "disable ktms-node.service" "$(cat "$STATE/calls")"
has "linux: uninstall disables ollama too" "disable ktms-ollama.service" "$(cat "$STATE/calls")"
if [ ! -e "$BASE" ]; then ok "linux: uninstall removes the install dir and key"; else bad "linux: uninstall removes the install dir"; fi
if [ ! -e "$W/svc/ktms-node.service" ]; then ok "linux: uninstall removes units"; else bad "linux: uninstall removes units"; fi

reset_world
make_deps amd64
make_agent 0.10.0 good linux amd64
OUT=$(run_install $ARGS --dry-run)
RC=$?
eq "dry run exits 0" 0 "$RC"
has "dry run says nothing changed" "Nothing was changed" "$OUT"
if [ ! -e "$BASE" ]; then ok "dry run creates nothing"; else bad "dry run creates nothing"; fi

# memory warning on a small Mac, none on a 32 GB one
FAKE_OS=Darwin FAKE_ARCH=arm64
OUT=$(FAKE_MEMSIZE=8589934592 run_install --proxy p --node-id n --model gemma4:26b-a4b-it-qat --dry-run)
has "small Mac gets a memory warning" "Not enough memory" "$OUT"
OUT=$(run_install --proxy p --node-id n --model gemma4:26b-a4b-it-qat --dry-run)
hasnt "32 GB Mac fits the 26b model" "Not enough memory" "$OUT"
# measured: this model loaded fully on a 24 GB Mac at a 12288 context
OUT=$(FAKE_MEMSIZE=25769803776 run_install --proxy p --node-id n --model gemma4:26b-a4b-it-qat --context 12288 --dry-run)
hasnt "24 GB Mac fits the 26b qat model at 12288 context" "Not enough memory" "$OUT"
OUT=$(FAKE_MEMSIZE=17179869184 run_install --proxy p --node-id n --model gemma4:26b-a4b-it-qat --dry-run)
has "16 GB Mac still gets the warning" "Not enough memory" "$OUT"

# Intel-only Ollama on Apple Silicon is called out and not used
printf '#!/bin/sh\nexit 0\n' >"$W/bin/ollama"
printf '#!/bin/sh\necho "Mach-O 64-bit executable x86_64"\n' >"$W/bin/file"
chmod +x "$W/bin/ollama" "$W/bin/file"
OUT=$(run_install --proxy p --node-id n --model tiny:1b --dry-run)
has "Intel Ollama is called out" "Intel-only Ollama" "$OUT"
rm -f "$W/bin/ollama" "$W/bin/file"

# ---------------------------------------------------------- macOS end to end
if [ "$(uname -s)" = Darwin ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null)" = 1 ]; then
	rm -f "$W/bin/uname" # real uname; only the service manager is faked
	printf '#!/bin/sh\ncase "$2" in hw.memsize) echo 34359738368;; *) exec /usr/sbin/sysctl "$@";; esac\n' >"$W/bin/sysctl"
	chmod +x "$W/bin/sysctl"
	FAKE_OS=Darwin FAKE_ARCH=arm64
	reset_world
	make_deps arm64
	make_agent 0.10.0 good darwin arm64
	OUT=$(run_install $ARGS --name "My Node")
	RC=$?
	eq "mac: fresh install exits 0" 0 "$RC"
[ "$RC" = 0 ] || printf '%s\n' "$OUT" | tail -15
	has "mac: reports running" "Node node-9 is running and registered" "$OUT"
	has "mac: tells an unattended Mac to enable auto-login" "automatic login" "$OUT"
	if [ -x "$BASE/ollama/ollama" ]; then ok "mac: Ollama downloaded and verified"; else bad "mac: Ollama downloaded"; fi
	NP=$(cat "$W/svc/com.ktms.node.plist")
	OP=$(cat "$W/svc/com.ktms.ollama.plist")
	has "mac: node wrapped in caffeinate" "/usr/bin/caffeinate" "$NP"
	has "mac: node name with a space kept" "<string>My Node</string>" "$NP"
	hasnt "mac: key never in the plist" "ktms_node_test_key" "$NP"
	has "mac: Ollama forced to the native slice" "<string>-arm64</string>" "$OP"
	has "mac: Ollama keeps the model loaded" "<key>OLLAMA_KEEP_ALIVE</key><string>-1</string>" "$OP"
	for f in "$W/svc"/*.plist; do
		if plutil -lint "$f" >/dev/null 2>&1; then ok "mac: $(basename "$f") lints"; else bad "mac: $(basename "$f") lints"; fi
	done
	BEFORE=$(count_calls 'bootstrap|bootout')
	OUT=$(run_install $ARGS --name "My Node")
	RC=$?
	eq "mac: re-run exits 0" 0 "$RC"
	eq "mac: re-run does not touch loaded services" "$BEFORE" "$(count_calls 'bootstrap|bootout')"
	make_agent 0.11.0 good darwin arm64
	OUT=$(run_install $ARGS --name "My Node" --version v0.11.0)
	RC=$?
	eq "mac: upgrade exits 0" 0 "$RC"
	ORDER=$(grep -E 'launchctl (bootout|bootstrap) .*com.ktms.node' "$STATE/calls" | tail -2 | awk '{print $2}' | tr '\n' ' ')
	eq "mac: bootout precedes bootstrap on restart" "bootout bootstrap " "$ORDER"
else
	printf 'skip macOS end-to-end (not an Apple Silicon Mac)\n'
fi

# ------------------------------------------------- install from before the rename
make_stubs # the macOS run above takes the uname stub away
FAKE_OS=Linux FAKE_ARCH=x86_64
OLD="$W/home/.llm-proxy-node"
NOKEY_ARGS="--proxy llm-nodes.example.com --node-id node-9 --model tiny:1b"
# legacy_world NODE_ID: an llm-proxy-node install whose node service is running
# as a real process that its stubbed manager does not kill on stop.
legacy_world() {
	reset_world
	make_deps amd64
	make_agent 0.10.0 good linux amd64
	mkdir -p "$OLD/ollama/bin" "$W/svc" "$OLD/models"
	fake_ollama_bin "$OLD/ollama/bin/ollama" # adopted: Linux tests have no Ollama download
	printf 'llm_node_old_key\n' >"$OLD/key"
	( INSTALL_NODE_SOURCED=1; . "$SCRIPT"; render_unit d "$OLD/logs/node.log" "" "$OLD/bin/llm-proxy-node" --proxy-addr p:443 --node-id "$1" --tls ) >"$W/svc/llm-proxy-node.service"
	( INSTALL_NODE_SOURCED=1; . "$SCRIPT"; render_unit d "$OLD/logs/ollama.log" "OLLAMA_MODELS=$OLD/models" "$OLD/ollama/bin/ollama" serve ) >"$W/svc/llm-proxy-ollama.service"
	sh -c 'sleep 300 & echo $! >"$1"; wait' sh "$STATE/legacy-child" 2>/dev/null &
	LEGACY_PID=$!
	i=0
	while [ ! -s "$STATE/legacy-child" ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
	LEGACY_CHILD=$(cat "$STATE/legacy-child")
	echo "$LEGACY_PID" >"$STATE/pid.llm-proxy-node.service"
	touch "$STATE/running.llm-proxy-node.service"
}
# macOS detection, from launchd plists, runs on any machine with the stubs
reset_world
mkdir -p "$W/macsvc" "$OLD/models"
( INSTALL_NODE_SOURCED=1; . "$SCRIPT"; render_plist com.llm-proxy.node /tmp/n.log "" /usr/bin/caffeinate -is "$OLD/bin/llm-proxy-node" --node-id mac-node --tls ) >"$W/macsvc/com.llm-proxy.node.plist"
( INSTALL_NODE_SOURCED=1; . "$SCRIPT"; render_plist com.llm-proxy.ollama /tmp/o.log "OLLAMA_MODELS=$OLD/models" "$OLD/ollama/ollama" serve ) >"$W/macsvc/com.llm-proxy.ollama.plist"
touch "$W/macsvc/com.hotelstory.ollama.plist.disabled"
MAC=$(HOME="$W/home" LLMPROXY_HOME="$BASE" LLMPROXY_SERVICE_DIR="$W/macsvc" FAKE_STATE="$STATE" PATH="$W/bin:$PATH" sh -c \
	'INSTALL_NODE_SOURCED=1; . "$1"; OS=darwin NODE_ID=mac-node KEY_SRC="" NEW_KEY=0 KEEP_LEGACY=0; detect_legacy; echo "ids=$LEGACY_IDS|node=$LEGACY_NODE_ID|models=$LEGACY_MODELS|dir=$LEGACY_DIR"; legacy_plan' sh "$SCRIPT")
has "mac legacy: both launchd services found" "ids= node ollama|" "$MAC"
has "mac legacy: node id read from the plist" "|node=mac-node|" "$MAC"
has "mac legacy: model store read from the plist" "|models=$OLD/models|" "$MAC"
has "mac legacy: directory found" "|dir=$OLD" "$MAC"
has "mac legacy: plan names the launchd label" "stop com.llm-proxy.node" "$MAC"
hasnt "mac legacy: someone else's plist is not ours" "hotelstory" "$MAC"

backups() { find "$W/home/.ktms-node.backup" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l | tr -d ' '; }

legacy_world node-9
OUT=$(run_install $NOKEY_ARGS --dry-run)
has "legacy: dry run lists the old node service" "stop llm-proxy-node" "$OUT"
has "legacy: dry run lists the old directory" "move $OLD to" "$OUT"
has "legacy: dry run says the key is reused" "reused: it was minted for node node-9" "$OUT"
if [ -f "$W/svc/llm-proxy-node.service" ] && [ -d "$OLD" ] && kill -0 "$LEGACY_PID" 2>/dev/null; then ok "legacy: dry run changes nothing"; else bad "legacy: dry run changes nothing"; fi

OUT=$(run_install $NOKEY_ARGS)
RC=$?
eq "legacy: install over an old one exits 0" 0 "$RC"
[ "$RC" = 0 ] || printf '%s\n' "$OUT" | tail -15
if kill -0 "$LEGACY_PID" 2>/dev/null; then bad "legacy: the old node's process is gone"; kill "$LEGACY_PID"; else ok "legacy: the old node's process is gone"; fi
if kill -0 "$LEGACY_CHILD" 2>/dev/null; then bad "legacy: the wrapped agent is gone too"; kill "$LEGACY_CHILD"; else ok "legacy: the wrapped agent is gone too"; fi
has "legacy: old units disabled" "disable llm-proxy-node.service" "$(cat "$STATE/calls")"
if [ ! -e "$W/svc/llm-proxy-node.service" ] && [ ! -e "$W/svc/llm-proxy-ollama.service" ] && [ ! -e "$OLD" ]; then ok "legacy: old units and directory moved away"; else bad "legacy: old units and directory moved away"; fi
BK=$(find "$W/home/.ktms-node.backup" -mindepth 1 -maxdepth 1 -type d | head -n 1)
if [ -f "$BK/llm-proxy-node.service" ] && [ -f "$BK/.llm-proxy-node/key" ]; then ok "legacy: backup outside the install holds units and directory"; else bad "legacy: backup holds units and directory ($BK)"; fi
eq "legacy: key reused for the same node id" "llm_node_old_key" "$(cat "$BASE/key")"
eq "legacy: reused key is 0600" 600 "$(mode_of "$BASE/key")"
has "legacy: the old Ollama build is reused" "Reusing the old install's Ollama build" "$OUT"
if [ ! -e "$BK/.llm-proxy-node/ollama" ]; then ok "legacy: the backup drops the copied Ollama build"; else bad "legacy: the backup drops the copied Ollama build"; fi
hasnt "legacy: the key is never printed" "llm_node_old_key" "$OUT"
has "legacy: the old model store is pointed to" "now in $BK/.llm-proxy-node/models" "$OUT"
has "legacy: the node runs" "Node node-9 is running and registered" "$OUT"

OUT=$(run_install $NOKEY_ARGS)
eq "legacy: re-run exits 0" 0 "$?"
hasnt "legacy: re-run finds nothing to retire" "Retiring" "$OUT"
eq "legacy: re-run leaves the backup alone" 1 "$(backups)"

OUT=$(run_install --uninstall)
has "legacy: uninstall keeps backups by default" "Backups of old installs stay" "$OUT"
eq "legacy: backups survive uninstall" 1 "$(backups)"
OUT=$(run_install --uninstall --purge-backups)
if [ ! -e "$W/home/.ktms-node.backup" ]; then ok "legacy: --purge-backups removes them"; else bad "legacy: --purge-backups removes them"; fi

# another node id: the key is only offered, and without a terminal it stays put
legacy_world old-node
OUT=$(run_install $NOKEY_ARGS)
RC=$?
has "legacy: other node id is not reused without a terminal" "was not reused: no terminal to ask" "$OUT"
if [ ! -s "$BASE/key" ]; then ok "legacy: no key taken from another node"; else bad "legacy: no key taken from another node"; fi
if [ "$RC" != 0 ] && [ -f "$W/svc/llm-proxy-node.service" ] && [ -d "$OLD" ] && kill -0 "$LEGACY_PID" 2>/dev/null; then ok "legacy: a failed install leaves the old node running"; else bad "legacy: a failed install leaves the old node running"; fi
OUT=$(run_install $ARGS)
eq "legacy: with its own key the install completes" 0 "$?"
kill "$LEGACY_PID" "$LEGACY_CHILD" 2>/dev/null

# --keep-legacy keeps the directory but never the services
legacy_world node-9
OUT=$(run_install $ARGS --keep-legacy)
RC=$?
kill "$LEGACY_PID" "$LEGACY_CHILD" 2>/dev/null
eq "legacy: --keep-legacy install exits 0" 0 "$RC"
if [ -d "$OLD" ] && [ ! -e "$W/svc/llm-proxy-node.service" ]; then ok "legacy: --keep-legacy keeps the directory, retires the units"; else bad "legacy: --keep-legacy keeps the directory, retires the units"; fi
OUT=$(run_install $ARGS --keep-legacy)
hasnt "legacy: --keep-legacy re-run has nothing to retire" "Retiring" "$OUT"
eq "legacy: --keep-legacy re-run makes no new backup" 1 "$(backups)"

# --uninstall on a machine with only the old node retires its services
legacy_world node-9
OUT=$(run_install --uninstall)
kill "$LEGACY_PID" "$LEGACY_CHILD" 2>/dev/null
if [ ! -e "$W/svc/llm-proxy-node.service" ] && [ ! -e "$W/svc/llm-proxy-ollama.service" ] && [ -d "$OLD" ]; then ok "legacy: uninstall retires old services, leaves the old directory"; else bad "legacy: uninstall retires old services, leaves the old directory"; fi

# a stale LLMPROXY_HOME of $HOME is never taken for an old install
reset_world
DIR=$(HOME="$W/home" LLMPROXY_HOME="$W/home" KTMS_HOME="$BASE" FAKE_STATE="$STATE" PATH="$W/bin:$PATH" sh -c 'INSTALL_NODE_SOURCED=1; . "$1"; OS=linux; detect_legacy; echo "dir=$LEGACY_DIR"' sh "$SCRIPT")
eq "legacy: \$HOME is not an old install" "dir=" "$DIR"
mkdir -p "$W/home/parent/.ktms-node"
DIR=$(HOME="$W/home" LLMPROXY_HOME="$W/home/parent" KTMS_HOME="$W/home/parent/.ktms-node" FAKE_STATE="$STATE" PATH="$W/bin:$PATH" sh -c 'INSTALL_NODE_SOURCED=1; . "$1"; OS=linux; detect_legacy; echo "dir=$LEGACY_DIR"' sh "$SCRIPT")
eq "legacy: a parent of the install is not an old install" "dir=" "$DIR"

# an install kept under the old name is the current one, not a legacy one
legacy_world node-9
kill "$LEGACY_PID" "$LEGACY_CHILD" 2>/dev/null
rm -f "$W/svc"/llm-proxy-* "$STATE/running.llm-proxy-node.service"
OUT=$(TEST_BASE="$OLD" run_install $ARGS --dry-run)
hasnt "legacy: LLMPROXY_HOME as the install directory is not moved" "move $OLD" "$OUT"

# a port held by another process is refused before anything is pulled into it
legacy_world node-9
kill "$LEGACY_PID" "$LEGACY_CHILD" 2>/dev/null
sleep 300 &
FOREIGN=$!
OUT=$(FAKE_FOREIGN_PID=$FOREIGN run_install $ARGS)
RC=$?
kill "$FOREIGN" 2>/dev/null
if [ "$RC" != 0 ]; then ok "port: a foreign listener fails the install"; else bad "port: a foreign listener fails the install"; fi
has "port: the holder is named" "is taken by sleep (pid $FOREIGN)" "$OUT"
if [ ! -s "$STATE/models" ]; then ok "port: nothing pulled into the foreign Ollama"; else bad "port: nothing pulled into the foreign Ollama"; fi
OUT=$(FAKE_FOREIGN_PID=$$ FAKE_FOREIGN_HOST=192.168.1.5 run_install $ARGS)
eq "port: a listener on another address is no conflict" 0 "$?"
# another user's Ollama: no PID to see, but the address answers before ours runs
run_install --uninstall >/dev/null
reset_world
make_deps amd64
make_agent 0.10.0 good linux amd64
mkdir -p "$BASE/ollama/bin"
fake_ollama_bin "$BASE/ollama/bin/ollama"
touch "$STATE/foreign-ollama"
OUT=$(run_install $ARGS)
RC=$?
rm -f "$STATE/foreign-ollama"
if [ "$RC" != 0 ]; then ok "port: an unseen listener that answers fails the install"; else bad "port: an unseen listener that answers fails the install"; fi
has "port: the unseen holder is described" "taken by another user's process" "$OUT"

# ------------------------------------------------- hidden key prompt via pty
FAKE_OS=Linux
FAKE_ARCH=x86_64
reset_world
make_deps amd64
make_agent 0.10.0 good linux amd64
mkdir -p "$BASE/ollama/bin"
fake_ollama_bin "$BASE/ollama/bin/ollama"
cat >"$W/keydrive.py" <<'PY'
import os, pty, sys, select, time
secret, args = sys.argv[1], sys.argv[2:]
pid, fd = pty.fork()
if pid == 0:
    os.execvp("sh", ["sh"] + args)
buf, sent = b"", False
deadline = time.time() + 90
while time.time() < deadline:
    r, _, _ = select.select([fd], [], [], 0.5)
    if r:
        try: d = os.read(fd, 4096)
        except OSError: break
        if not d: break
        buf += d
        if not sent and b"input is hidden" in buf:
            os.write(fd, (secret + "\n").encode()); sent = True
    else:
        try:
            if os.waitpid(pid, os.WNOHANG)[0]: break
        except ChildProcessError: break
sys.stdout.write(buf.decode(errors="replace"))
sys.exit(3 if secret.encode() in buf else 0)
PY
OUT=$(HOME="$W/home" LLMPROXY_HOME="$BASE" LLMPROXY_SERVICE_DIR="$W/svc" LLMPROXY_GITHUB="http://127.0.0.1:$P1" LLMPROXY_GITHUB_API="http://127.0.0.1:$P1" LLMPROXY_OLLAMA_ADDR="127.0.0.1:$P2" LLMPROXY_REGISTER_WAIT=12 FAKE_OS=Linux FAKE_ARCH=x86_64 FAKE_STATE="$STATE" PATH="$W/bin:$PATH" python3 -I "$W/keydrive.py" llm_node_typed_secret "$SCRIPT" --proxy llm-nodes.example.com --node-id node-9 --model tiny:1b 2>&1)
RC=$?
eq "prompt: key typed at the terminal is not echoed" 0 "$RC"
eq "prompt: key stored from the terminal" "llm_node_typed_secret" "$(cat "$BASE/key" 2>/dev/null)"
eq "prompt: stored key is 0600" 600 "$(mode_of "$BASE/key")"

printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" = 0 ]
