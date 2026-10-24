#!/bin/sh
# The ktms CLI against a built Keystone with Postgres, Redis and a mock vendor:
# config apply, keys mint, vendor-keys set, chat, spend get.
# Env: IMAGE (a built server image; default builds this checkout).
set -eu

cd "$(dirname "$0")/.."

P=ktms-cli-e2e-$$
NET=$P-net
IMAGE=${IMAGE:-}
BUILT=""
TMP=$(mktemp -d)

cleanup() {
	docker rm -f -v "$P-gw" "$P-pg" "$P-redis" "$P-mock" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	docker rmi -f "$P-mock" $BUILT >/dev/null 2>&1 || true
	rm -rf "$TMP"
}
trap cleanup EXIT

fail() {
	echo "cli e2e: $*" >&2
	docker logs "$P-gw" 2>&1 | tail -n 40 >&2 || true
	exit 1
}

# retry N CMD... runs CMD until it succeeds, once a second.
retry() {
	n=$1
	shift
	i=0
	until "$@"; do
		i=$((i + 1))
		[ "$i" -lt "$n" ] || return 1
		sleep 1
	done
}

docker build -q -t "$P-mock" bench/mock-upstream >/dev/null
if [ -z "$IMAGE" ]; then
	IMAGE="$P-image"
	BUILT=$IMAGE
	docker build -q --target slim -t "$IMAGE" . >/dev/null
fi
go build -o "$TMP/ktms" ./cmd/ktms
PATH="$TMP:$PATH"

docker network create "$NET" >/dev/null
docker run -d --name "$P-pg" --network "$NET" \
	-e POSTGRES_USER=ktms -e POSTGRES_PASSWORD=ktms -e POSTGRES_DB=ktms postgres:17-alpine >/dev/null
docker run -d --name "$P-redis" --network "$NET" redis:7-alpine >/dev/null
docker run -d --name "$P-mock" --network "$NET" -e MOCK_REQUIRE_KEY=vk-e2e -e MOCK_CHUNKS=3 "$P-mock" >/dev/null
retry 60 docker exec "$P-pg" pg_isready -U ktms -d ktms >/dev/null 2>&1 || fail "postgres did not start"

docker run -d --name "$P-gw" --network "$NET" -p 127.0.0.1::9090 -p 127.0.0.1::8080 \
	-e DATABASE_URL="postgres://ktms:ktms@$P-pg:5432/ktms?sslmode=disable" \
	-e REDIS_ADDR="$P-redis:6379" \
	-e KTMS_ENCRYPTION_KEYS="1:$(printf 'cli-e2e-test-only-key-32-bytes!!' | base64)" \
	-e BOOTSTRAP_ADMIN_KEY=e2e-admin -e ROUTING_RELOAD_SECONDS=1 \
	"$IMAGE" >/dev/null

export KTMS_CONFIG="$TMP/config.yaml"
export KTMS_ADDR="$(docker port "$P-gw" 9090/tcp | head -n 1)"
export KTMS_HTTP="http://$(docker port "$P-gw" 8080/tcp | head -n 1)"
export KTMS_ADMIN_KEY=e2e-admin
retry 90 ktms health >/dev/null 2>&1 || fail "keystone did not become ready"
ktms health

cat >"$TMP/ktms.yaml" <<EOF
api_version: ktms.ai-process.dev/v1alpha1
models:
  - id: mock-model
    vendor: openai
    endpoint: http://$P-mock:8000/v1
    efforts: [EFFORT_LOW, EFFORT_MEDIUM, EFFORT_HIGH]
    price_in_per_mtok: 1000
    price_out_per_mtok: 2000
rules:
  - {name: default, priority: 1, use: [mock-model]}
spend_limits:
  - {subject_type: key, subject_id: "*", period: day, limit_usd: 10}
EOF

echo "--- config apply"
ktms config apply --yes -f "$TMP/ktms.yaml"
ktms config plan --detailed-exitcode -f "$TMP/ktms.yaml" || fail "plan after apply is not empty"

echo "--- keys mint"
KTMS_API_KEY=$(ktms keys mint e2e)
export KTMS_API_KEY
case $KTMS_API_KEY in ktms_*) ;; *) fail "mint printed '$KTMS_API_KEY'" ;; esac
echo vk-e2e | ktms vendor-keys set e2e openai
ktms keys list | grep -q '^e2e ' || fail "keys list misses e2e"
ktms keys list -o json | grep -q "$KTMS_API_KEY" && fail "keys list shows the key"

echo "--- route explain"
ktms route explain e2e --effort low | tee "$TMP/explain"
grep -q "tries: mock-model" "$TMP/explain" || fail "route explain does not route e2e to mock-model"

echo "--- chat"
# The gateway picks up the new configuration within ROUTING_RELOAD_SECONDS.
retry 15 ktms chat "Say hi" >"$TMP/answer" 2>"$TMP/route" || { cat "$TMP/route" >&2; fail "chat failed"; }
cat "$TMP/answer" "$TMP/route"
grep -q "rule: default" "$TMP/route" || fail "chat did not report the rule"
grep -q "vendor: openai" "$TMP/route" || fail "chat did not report the vendor"

echo "--- spend get"
spent() { ktms spend get key e2e -o json | grep -Eq '"spent_nanousd": "[1-9]'; }
retry 15 spent || { ktms spend get key e2e -o json >&2; fail "no spend recorded for e2e"; }
ktms spend get key e2e

echo "--- read key"
READ_KEY=$(ktms keys mint viewer --scopes admin:read)
KTMS_ADMIN_KEY=$READ_KEY ktms models list >/dev/null || fail "read key cannot list models"
code=0
KTMS_ADMIN_KEY=$READ_KEY ktms keys revoke e2e --yes 2>/dev/null || code=$?
[ "$code" = 4 ] || fail "read key on a mutation exited $code, want 4"

echo "--- mcp over stdio"
KTMS_E2E_BIN="$TMP/ktms" KTMS_E2E_MOCK="http://$P-mock:8000/v1" go test -count=1 -tags e2e -run '^TestMCPStdio$' ./cmd/ktms/ ||
	fail "mcp scenario failed"
# The rule added through MCP serves a call that carries its attribute, and
# route explain says so before the call.
ktms route explain e2e --effort low --attr tier=mcp | grep -q "rule: from-mcp" || fail "route explain misses the MCP rule"
retry 15 sh -c 'ktms chat --attr tier=mcp "Say hi" 2>&1 >/dev/null | grep -q "rule: from-mcp"' || fail "the MCP rule does not route"

echo "cli e2e: ok"
