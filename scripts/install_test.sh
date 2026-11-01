#!/bin/sh
# shellcheck disable=SC1090,SC2016,SC2034
# (sources the script by path; matches literal ${...}; sets the globals its functions read)
# Tests for install.sh. No Docker, no network: functions run in a throwaway directory.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/install.sh"
PASS=0 FAIL=0
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

ok() { PASS=$((PASS + 1)); printf 'ok   %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf 'FAIL %s\n' "$1"; }
eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want '$2', got '$3')"; fi; }
has() { case $3 in *"$2"*) ok "$1" ;; *) bad "$1 (missing '$2' in: $(printf '%s' "$3" | head -c 300))" ;; esac; }
hasnt() { case $3 in *"$2"*) bad "$1 (unexpected '$2')" ;; *) ok "$1" ;; esac; }

unit() { ( KTMS_INSTALL_SOURCED=1; . "$SCRIPT"; "$@" ); }

# ---------------------------------------------------------------- arguments
parse() { unit parse_args "$@" 2>&1; }
has "unknown option" "unknown option" "$(parse --bogus)"
has "option without value" "needs a value" "$(parse --dir)"
has "bad port" "ports must be numbers" "$(parse --port abc)"
has "bad version" "--version must look like" "$(parse --version 1.2)"
has "home refused" "refusing to use" "$(parse --dir "$HOME")"
eq "defaults" "$HOME/ktms|latest|8080|9090|1|1|0" \
	"$(KTMS_INSTALL_SOURCED=1; unset KTMS_DIR; . "$SCRIPT"; parse_args; echo "$DIR|$VERSION|$PORT|$GRPC_PORT|$PROMPT|$START|$UNINSTALL")"
eq "= form and flags" "/x/k|v1.4.0|18080|19090|0|0" \
	"$(KTMS_INSTALL_SOURCED=1; . "$SCRIPT"; parse_args --dir=/x/k --version v1.4.0 --port=18080 --grpc-port 19090 --no-prompt --no-start; echo "$DIR|$VERSION|$PORT|$GRPC_PORT|$PROMPT|$START")"
has "help" "Usage: install.sh" "$(parse --help)"

eq "pinned version drops the v" "1.4.0" "$(KTMS_INSTALL_SOURCED=1; . "$SCRIPT"; VERSION=v1.4.0; resolve_tag)"
eq "latest without GitHub falls back to latest" "latest" \
	"$(KTMS_INSTALL_SOURCED=1; KTMS_GITHUB_API=http://127.0.0.1:9; . "$SCRIPT"; VERSION=latest; resolve_tag)"

# ---------------------------------------------------------------- files
# write_env with no terminal and the given vendor variables; prints the resulting .env.
run_env() {
	dir=$1
	shift
	(
		KTMS_INSTALL_SOURCED=1
		unset OPENAI_API_KEY ANTHROPIC_API_KEY GEMINI_API_KEY
		for kv in "$@"; do export "${kv?}"; done
		. "$SCRIPT"
		DIR=$dir TAG=1.4.0 PORT=8080 GRPC_PORT=9090 PROMPT=0
		mkdir -p "$DIR"
		write_compose
		write_env >/dev/null
		echo "HAS_VENDOR=$HAS_VENDOR"
		cat "$DIR/.env"
	)
}

D=$TMP/a
first=$(run_env "$D")
has "no vendor key reported" "HAS_VENDOR=0" "$first"
has "client key generated" "KTMS_CLIENT_KEY=ktms_local_" "$first"
has "keyring generated" "KTMS_ENCRYPTION_KEYS=1:" "$first"
has "tag pinned" "KTMS_TAG=1.4.0" "$first"
eq ".env is private" "600" "$(stat -f %Lp "$D/.env" 2>/dev/null || stat -c %a "$D/.env")"

compose=$(cat "$D/compose.yaml")
hasnt "compose does not build" "build:" "$compose"
has "compose uses the image" 'image: ${KTMS_IMAGE:-coyl/ktms}:${KTMS_TAG:-latest}' "$compose"
has "ports bound to localhost" '"127.0.0.1:${KTMS_HTTP_PORT:-8080}:8080"' "$compose"
has "data on a volume" "db:/var/lib/postgresql/data" "$compose"

second=$(run_env "$D" OPENAI_API_KEY=sk-test)
key() { printf '%s\n' "$2" | sed -n "s/^$1=//p"; }
eq "re-run keeps the keyring" "$(key KTMS_ENCRYPTION_KEYS "$first")" "$(key KTMS_ENCRYPTION_KEYS "$second")"
eq "re-run keeps the client key" "$(key KTMS_CLIENT_KEY "$first")" "$(key KTMS_CLIENT_KEY "$second")"
eq "re-run keeps the database password" "$(key POSTGRES_PASSWORD "$first")" "$(key POSTGRES_PASSWORD "$second")"
eq "vendor key from the environment" "sk-test" "$(key OPENAI_API_KEY "$second")"
has "vendor key reported" "HAS_VENDOR=1" "$second"

third=$(run_env "$D")
eq "stored vendor key survives a run without it" "sk-test" "$(key OPENAI_API_KEY "$third")"

other=$(run_env "$TMP/b")
if [ "$(key KTMS_CLIENT_KEY "$first")" != "$(key KTMS_CLIENT_KEY "$other")" ]; then ok "installs get different keys"; else bad "installs get different keys"; fi

has "uninstall without an install" "no Keystone install" "$(unit main --uninstall --dir "$TMP/none" 2>&1)"

printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
