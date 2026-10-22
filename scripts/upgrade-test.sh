#!/bin/sh
# Upgrade test: a previous release writes a database, this build takes it over,
# and test/upgrade checks that keys, config, limits and a real call survive.
# Then the old release runs once more on the migrated database, as during a
# rolling upgrade. Needs docker, curl and go.
#
# PREV lists the releases to upgrade from (default: the previous published
# release in this branch's history and the oldest published 1.x release);
# --print-prev prints it and exits. CUR is the tag being released, never its
# own predecessor. NEW_IMAGE skips building this checkout.
set -eu

cd "$(dirname "$0")/.."

# stable_tags prints published-looking tags reachable from HEAD, newest first,
# so a backport branch compares against its own line, not a newer release.
stable_tags() {
	git tag --merged HEAD --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | grep -vx "${CUR:-none}" || true
}
has_image() { docker manifest inspect "coyl/ktms:${1#v}" >/dev/null 2>&1; }

# A tag whose release failed has no image; the one before it is the real
# predecessor.
pick_prev() {
	for t in $(stable_tags); do
		if has_image "$t"; then
			echo "$t"
			return
		fi
	done
}
pick_oldest_1x() {
	for t in $(stable_tags | grep '^v1\.' | sort -t. -k2,2n -k3,3n); do
		if has_image "$t"; then
			echo "$t"
			return
		fi
	done
}

if [ -z "${PREV:-}" ]; then
	prev=$(pick_prev)
	oldest=$(pick_oldest_1x)
	PREV=$prev
	[ -n "$oldest" ] && [ "$oldest" != "$prev" ] && PREV="$prev $oldest"
fi
[ -n "$PREV" ] || { echo "no published previous release found" >&2; exit 1; }
if [ "${1:-}" = --print-prev ]; then
	echo "$PREV"
	exit 0
fi

P=ktms-upgrade-$$
NET=$P-net
NEW_IMAGE=${NEW_IMAGE:-}
BUILT=""
KEYS="1:$(printf 'upgrade-test-only-key-32-bytes!!' | base64)"
export ADMIN_KEY=upgrade-admin
STATE=$(mktemp)
export UPGRADE_STATE="$STATE"

cleanup() {
	docker rm -f -v "$P-old" "$P-new" "$P-old-again" "$P-pg" "$P-mock" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	docker rmi -f "$P-mock" $BUILT >/dev/null 2>&1 || true
	rm -f "$STATE"
}
trap cleanup EXIT

wait_ready() {
	http=$(docker port "$1" 8080/tcp | head -n 1)
	i=0
	until curl -fsS "http://$http/readyz" >/dev/null 2>&1; do
		i=$((i + 1))
		if [ "$i" -gt 90 ]; then
			echo "$1 did not become ready" >&2
			docker logs "$1" 2>&1 | tail -n 40 >&2
			exit 1
		fi
		sleep 1
	done
}

# run_gateway NAME IMAGE starts a gateway on the shared database with random
# host ports and points the test client at it.
run_gateway() {
	docker run -d --name "$1" --network "$NET" -p 127.0.0.1::9090 -p 127.0.0.1::8080 \
		-e DATABASE_URL="postgres://ktms:ktms@$P-pg:5432/ktms?sslmode=disable" \
		-e KTMS_ENCRYPTION_KEYS="$KEYS" -e BOOTSTRAP_ADMIN_KEY="$ADMIN_KEY" -e ROUTING_RELOAD_SECONDS=1 \
		"$2" >/dev/null
	wait_ready "$1"
	KTMS_ADDR=$(docker port "$1" 9090/tcp | head -n 1)
	export KTMS_ADDR
}

phase() { go test -count=1 -tags upgrade -run "^$1\$" ./test/upgrade/; }

docker build -q -t "$P-mock" bench/mock-upstream >/dev/null
if [ -z "$NEW_IMAGE" ]; then
	NEW_IMAGE="$P-new-image"
	BUILT=$NEW_IMAGE
	docker build -q --target slim -t "$NEW_IMAGE" . >/dev/null
fi
export MOCK_URL="http://$P-mock:8000/v1"

for from in $PREV; do
	echo "upgrade test: $from -> this checkout"
	docker rm -f -v "$P-old" "$P-new" "$P-old-again" "$P-pg" "$P-mock" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	docker network create "$NET" >/dev/null
	docker run -d --name "$P-pg" --network "$NET" \
		-e POSTGRES_USER=ktms -e POSTGRES_PASSWORD=ktms -e POSTGRES_DB=ktms postgres:17-alpine >/dev/null
	docker run -d --name "$P-mock" --network "$NET" -e MOCK_REQUIRE_KEY=vk-upgrade -e MOCK_CHUNKS=3 "$P-mock" >/dev/null
	until docker exec "$P-pg" pg_isready -U ktms -d ktms >/dev/null 2>&1; do sleep 1; done

	old="coyl/ktms:${from#v}"
	run_gateway "$P-old" "$old"
	phase TestSeed
	docker stop -t 30 "$P-old" >/dev/null

	run_gateway "$P-new" "$NEW_IMAGE"
	phase TestVerify
	docker stop -t 30 "$P-new" >/dev/null

	# Rolling upgrade: the old release must still serve on the migrated schema.
	run_gateway "$P-old-again" "$old"
	phase TestServe
	echo "upgrade test passed: $from -> this checkout"
done
