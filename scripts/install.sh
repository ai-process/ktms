#!/bin/sh
# Runs Keystone (KTMS) on this machine with Docker: published images, no clone, no build.
#   curl -fsSL https://ai-process.dev/install.sh | sh
# Safe to re-run: keeps its secrets and data, applies new vendor keys or a new version.
set -eu

REPO="ai-process/ktms"
IMAGE="${KTMS_IMAGE:-coyl/ktms}"
GITHUB_API="${KTMS_GITHUB_API:-https://api.github.com}"

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Usage: install.sh [options]
       curl -fsSL https://ai-process.dev/install.sh | sh -s -- [options]

Starts Keystone (KTMS) with Postgres and Redis in Docker, listening on localhost only.
Vendor keys come from OPENAI_API_KEY, ANTHROPIC_API_KEY and GEMINI_API_KEY, or are asked for.

Options:
  --dir DIR          where compose.yaml and .env live (default: ~/ktms)
  --version vX.Y.Z   release to run (default: latest)
  --port N           HTTP / OpenAI-compatible port (default: 8080)
  --grpc-port N      gRPC port (default: 9090)
  --no-prompt        never ask for a vendor key
  --no-start         write the files, do not start anything
  --uninstall        stop the stack and delete its data and files
  -h, --help         show this help
EOF
}

parse_args() {
	DIR="${KTMS_DIR:-$HOME/ktms}" VERSION="latest" PORT=8080 GRPC_PORT=9090
	PROMPT=1 START=1 UNINSTALL=0
	while [ $# -gt 0 ]; do
		opt=$1 val="" shift_n=1
		case $1 in --*=*) opt=${1%%=*} val=${1#*=} ;; esac
		case $opt in
		--dir | --version | --port | --grpc-port)
			if [ "$opt" = "$1" ]; then
				[ $# -ge 2 ] || die "$opt needs a value"
				val=$2 shift_n=2
			fi
			case $opt in
			--dir) DIR=$val ;; --version) VERSION=$val ;; --port) PORT=$val ;; --grpc-port) GRPC_PORT=$val ;;
			esac
			;;
		--no-prompt) PROMPT=0 ;;
		--no-start) START=0 ;;
		--uninstall) UNINSTALL=1 ;;
		-h | --help) usage; exit 0 ;;
		*) die "unknown option: $1 (see --help)" ;;
		esac
		shift "$shift_n"
	done
	case $DIR in "" | / | "$HOME") die "refusing to use '$DIR' as the install directory" ;; esac
	for p in "$PORT" "$GRPC_PORT"; do
		case $p in '' | *[!0-9]*) die "ports must be numbers, got '$p'" ;; esac
	done
	case $VERSION in latest | v[0-9]*.[0-9]*.[0-9]*) ;; *) die "--version must look like v1.2.3, got '$VERSION'" ;; esac
}

# The image tag for a release; "latest" asks GitHub and falls back to the latest tag.
resolve_tag() {
	if [ "$VERSION" != latest ]; then
		printf '%s\n' "${VERSION#v}"
		return
	fi
	tag=$(curl -fsSL "$GITHUB_API/repos/$REPO/releases/latest" 2>/dev/null |
		sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1) || true
	printf '%s\n' "${tag:-latest}"
}

random_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }
random_b64() { head -c "$1" /dev/urandom | base64 | tr -d '\n'; }

# Value of KEY in an existing .env, so a re-run keeps its secrets.
env_get() { [ -f "$DIR/.env" ] && sed -n "s/^$1=//p" "$DIR/.env" | tail -n 1 || true; }

# Asks on the terminal even when the script itself arrives on stdin from curl.
ask() {
	[ "$PROMPT" = 1 ] && [ -r /dev/tty ] && [ -w /dev/tty ] || return 1
	printf '%s' "$1" >/dev/tty
	stty -echo </dev/tty 2>/dev/null || true
	IFS= read -r answer </dev/tty || answer=""
	stty echo </dev/tty 2>/dev/null || true
	printf '\n' >/dev/tty
	printf '%s\n' "$answer"
}

check_docker() {
	command -v docker >/dev/null 2>&1 || die "Docker is not installed: https://docs.docker.com/get-docker/"
	docker compose version >/dev/null 2>&1 || die "the Docker Compose plugin is required: https://docs.docker.com/compose/install/"
	docker info >/dev/null 2>&1 || die "Docker is installed but not running; start Docker Desktop or the docker service"
}

write_compose() {
	cat >"$DIR/compose.yaml" <<'EOF'
# Written by https://ai-process.dev/install.sh; a re-run rewrites it, so keep changes in .env.
name: keystone
services:
  db:
    image: postgres:16-alpine
    restart: unless-stopped
    environment:
      POSTGRES_USER: ktms
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?run install.sh}
      POSTGRES_DB: ktms
    volumes:
      - db:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -h 127.0.0.1 -U ktms -d ktms"]
      interval: 2s
      timeout: 3s
      retries: 30

  redis:
    image: redis:7-alpine
    restart: unless-stopped

  ktms:
    image: ${KTMS_IMAGE:-coyl/ktms}:${KTMS_TAG:-latest}
    restart: unless-stopped
    depends_on:
      db:
        condition: service_healthy
    environment:
      DATABASE_URL: postgres://ktms:${POSTGRES_PASSWORD}@db:5432/ktms?sslmode=disable
      KTMS_ENCRYPTION_KEYS: ${KTMS_ENCRYPTION_KEYS:?run install.sh}
      BOOTSTRAP_ADMIN_KEY: ${BOOTSTRAP_ADMIN_KEY:?run install.sh}
      REDIS_ADDR: redis:6379
      AUTO_SEED: "true"
      SEED_CLIENT_KEY: ${KTMS_CLIENT_KEY:?run install.sh}
      SEED_OPENAI_KEY: ${OPENAI_API_KEY:-}
      SEED_ANTHROPIC_KEY: ${ANTHROPIC_API_KEY:-}
      SEED_GEMINI_KEY: ${GEMINI_API_KEY:-}
    ports:
      - "127.0.0.1:${KTMS_HTTP_PORT:-8080}:8080"
      - "127.0.0.1:${KTMS_GRPC_PORT:-9090}:9090"

volumes:
  db:
EOF
}

# Keeps secrets from an earlier run; vendor keys from the environment win over stored ones.
write_env() {
	enc=$(env_get KTMS_ENCRYPTION_KEYS)
	[ -n "$enc" ] || enc="1:$(random_b64 32)"
	boot=$(env_get BOOTSTRAP_ADMIN_KEY)
	[ -n "$boot" ] || boot="ktms_admin_$(random_hex 24)"
	pg=$(env_get POSTGRES_PASSWORD)
	[ -n "$pg" ] || pg=$(random_hex 16)
	client=$(env_get KTMS_CLIENT_KEY)
	[ -n "$client" ] || client="ktms_local_$(random_hex 24)"
	openai=${OPENAI_API_KEY:-$(env_get OPENAI_API_KEY)}
	anthropic=${ANTHROPIC_API_KEY:-$(env_get ANTHROPIC_API_KEY)}
	gemini=${GEMINI_API_KEY:-$(env_get GEMINI_API_KEY)}

	if [ -z "$openai$anthropic$gemini" ]; then
		say "Keystone needs at least one vendor key to answer (you can add one later by re-running)."
		if key=$(ask "Paste an OpenAI, Anthropic or Gemini API key (Enter to skip): "); then
			case $key in
			sk-ant-*) anthropic=$key ;;
			AIza*) gemini=$key ;;
			?*) openai=$key ;;
			esac
		fi
	fi

	umask 077
	cat >"$DIR/.env" <<EOF
# Secrets of this Keystone install. Back it up: without KTMS_ENCRYPTION_KEYS the stored vendor keys are lost.
KTMS_TAG=$TAG
KTMS_IMAGE=$IMAGE
KTMS_HTTP_PORT=$PORT
KTMS_GRPC_PORT=$GRPC_PORT
KTMS_ENCRYPTION_KEYS=$enc
BOOTSTRAP_ADMIN_KEY=$boot
POSTGRES_PASSWORD=$pg
KTMS_CLIENT_KEY=$client
OPENAI_API_KEY=$openai
ANTHROPIC_API_KEY=$anthropic
GEMINI_API_KEY=$gemini
EOF
	CLIENT_KEY=$client
	HAS_VENDOR=0
	[ -z "$openai$anthropic$gemini" ] || HAS_VENDOR=1
}

wait_ready() {
	i=0
	while [ $i -lt 90 ]; do
		if curl -fsS "http://127.0.0.1:$PORT/readyz" >/dev/null 2>&1; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	return 1
}

compose() { docker compose --project-directory "$DIR" -f "$DIR/compose.yaml" --env-file "$DIR/.env" "$@"; }

uninstall() {
	[ -f "$DIR/compose.yaml" ] || die "no Keystone install in $DIR"
	check_docker
	step "Stopping Keystone and deleting its data"
	compose down --volumes --remove-orphans
	rm -f "$DIR/compose.yaml" "$DIR/.env"
	rmdir "$DIR" 2>/dev/null || true
	say "Removed. The Docker images stay cached; remove them with: docker image rm $IMAGE"
}

main() {
	parse_args "$@"
	if [ "$UNINSTALL" = 1 ]; then
		uninstall
		return
	fi

	step "Checking Docker"
	check_docker
	TAG=$(resolve_tag)
	say "Keystone $TAG, files in $DIR"

	mkdir -p "$DIR"
	write_compose
	write_env

	if [ "$START" = 0 ]; then
		say "Files written. Start with: docker compose --project-directory $DIR up -d"
		return
	fi

	step "Starting Keystone, Postgres and Redis"
	compose pull --quiet
	compose up -d --remove-orphans
	wait_ready || {
		compose logs --tail 40 ktms >&2 || true
		die "Keystone did not become ready on port $PORT; the log is above"
	}

	cat <<EOF

Keystone is running on http://localhost:$PORT (gRPC on localhost:$GRPC_PORT).

Try it:
  curl -s http://localhost:$PORT/v1/chat/completions \\
    -H "Authorization: Bearer $CLIENT_KEY" \\
    -H "Content-Type: application/json" \\
    -d '{"model":"auto","messages":[{"role":"user","content":"Say hi in three words."}]}'

Any OpenAI SDK works too: base_url="http://localhost:$PORT/v1", api_key="$CLIENT_KEY".
EOF
	[ "$HAS_VENDOR" = 1 ] || cat <<EOF

No vendor key yet, so requests fail with no_vendor_key. Add one by re-running:
  OPENAI_API_KEY=sk-... sh -c "\$(curl -fsSL https://ai-process.dev/install.sh)"
EOF
	cat <<EOF

Secrets are in $DIR/.env. Stop: docker compose --project-directory $DIR down
Remove everything: curl -fsSL https://ai-process.dev/install.sh | sh -s -- --uninstall --dir $DIR
Next: route by effort, add GPU nodes, go to production: https://ai-process.dev/docs/
EOF
}

[ -n "${KTMS_INSTALL_SOURCED:-}" ] || main "$@"
