#!/bin/sh
# Upgrade test: the previous release writes a database, this build takes it
# over, and test/upgrade checks that keys, config, limits and a real call
# survive. Needs docker and go. PREV picks the old release (default: the newest
# stable v* tag); NEW_IMAGE skips building this checkout.
set -eu

cd "$(dirname "$0")/.."
PREV=${PREV:-$(git tag --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -n 1)}
[ -n "$PREV" ] || { echo "no previous release tag found" >&2; exit 1; }
OLD_IMAGE="coyl/ktms:${PREV#v}"
NEW_IMAGE=${NEW_IMAGE:-ktms:upgrade-test}
P=ktms-upgrade
NET=$P-net
GRPC_PORT=${GRPC_PORT:-19095}
HTTP_PORT=${HTTP_PORT:-18085}
KEYS="1:$(printf 'upgrade-test-only-key-32-bytes!!' | base64)"
ADMIN_KEY=upgrade-admin
STATE=$(mktemp)

cleanup() {
	docker rm -f "$P-old" "$P-new" "$P-pg" "$P-mock" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	rm -f "$STATE"
}
trap cleanup EXIT
cleanup

wait_ready() {
	i=0
	until curl -fsS "http://127.0.0.1:$HTTP_PORT/readyz" >/dev/null 2>&1; do
		i=$((i + 1))
		if [ "$i" -gt 90 ]; then
			echo "$1 did not become ready" >&2
			docker logs "$1" 2>&1 | tail -n 40 >&2
			exit 1
		fi
		sleep 1
	done
}

# run_gateway NAME IMAGE: both releases share the database, the network and
# the ports, so the test client talks to whichever one is up.
run_gateway() {
	docker pull -q "$2" >/dev/null 2>&1 || true
	docker run -d --name "$1" --network "$NET" \
		-p "127.0.0.1:$GRPC_PORT:9090" -p "127.0.0.1:$HTTP_PORT:8080" \
		-e DATABASE_URL="postgres://ktms:ktms@$P-pg:5432/ktms?sslmode=disable" \
		-e KTMS_ENCRYPTION_KEYS="$KEYS" -e LLMPROXY_ENCRYPTION_KEYS="$KEYS" \
		-e BOOTSTRAP_ADMIN_KEY="$ADMIN_KEY" -e ROUTING_RELOAD_SECONDS=1 \
		"$2" >/dev/null
	wait_ready "$1"
}

echo "upgrade test: $PREV -> this checkout"
docker network create "$NET" >/dev/null
docker run -d --name "$P-pg" --network "$NET" \
	-e POSTGRES_USER=ktms -e POSTGRES_PASSWORD=ktms -e POSTGRES_DB=ktms postgres:17-alpine >/dev/null
docker build -q -t "$P-mock" bench/mock-upstream >/dev/null
docker run -d --name "$P-mock" --network "$NET" -e MOCK_REQUIRE_KEY=vk-upgrade -e MOCK_CHUNKS=3 "$P-mock" >/dev/null
until docker exec "$P-pg" pg_isready -U ktms -d ktms >/dev/null 2>&1; do sleep 1; done
if [ "$NEW_IMAGE" = ktms:upgrade-test ]; then
	docker build -q --target slim -t "$NEW_IMAGE" . >/dev/null
fi

export KTMS_ADDR="127.0.0.1:$GRPC_PORT" ADMIN_KEY UPGRADE_STATE="$STATE" MOCK_URL="http://$P-mock:8000/v1"

run_gateway "$P-old" "$OLD_IMAGE"
go test -count=1 -tags upgrade -run '^TestSeed$' ./test/upgrade/
docker stop -t 30 "$P-old" >/dev/null

run_gateway "$P-new" "$NEW_IMAGE"
go test -count=1 -tags upgrade -run '^TestVerify$' ./test/upgrade/
echo "upgrade test passed: $PREV -> this checkout"
