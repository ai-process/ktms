#!/bin/sh
# Installs the Helm chart the way users do, from an OCI registry, on a fresh kind
# cluster with this tree's image and an in-cluster Postgres, and waits until the
# pods are ready (/readyz: the database answers and migrations ran).
# IMAGE=repo:tag uses an image built beforehand instead of building one.
set -eu

cd "$(dirname "$0")/.."
P=ktms-chart-$$
VERSION=0.0.0-ci.$$
WORK=$(mktemp -d)

cleanup() {
	kind delete cluster --name "$P" >/dev/null 2>&1 || true
	docker rm -f "$P-registry" >/dev/null 2>&1 || true
	[ -n "${BUILT:-}" ] && docker rmi -f "$BUILT" >/dev/null 2>&1
	rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# run STEP CMD...: keeps the output in a log, shown only if the step fails.
run() {
	step=$1; shift
	"$@" >"$WORK/step.log" 2>&1 || { tail -n 40 "$WORK/step.log" >&2; fail "$step"; }
}

fail() {
	echo "helm-kind: $*" >&2
	kubectl --context "kind-$P" get pods -A >&2 2>/dev/null || true
	kubectl --context "kind-$P" logs -l app.kubernetes.io/name=ktms --tail=30 >&2 2>/dev/null || true
	exit 1
}

if [ -z "${IMAGE:-}" ]; then
	IMAGE=coyl/ktms:$VERSION
	BUILT=$IMAGE
	echo "helm-kind: building $IMAGE"
	run "building the image" docker build --target slim -t "$IMAGE" .
fi
REPO=${IMAGE%:*}
TAG=${IMAGE##*:}

echo "helm-kind: creating a kind cluster"
run "creating the kind cluster" kind create cluster --name "$P" --wait 120s
run "loading the image" kind load docker-image "$IMAGE" --name "$P"
K="kubectl --context kind-$P"

# Port 0: the registry gets a free port, so parallel runs do not collide.
run "pulling the registry" docker pull -q registry:2
run "starting the registry" docker run -d --name "$P-registry" -p 127.0.0.1:0:5000 registry:2
REG=$(docker port "$P-registry" 5000/tcp | head -n 1)
run "packaging the chart" helm package deploy/helm/ktms --version "$VERSION" --app-version "$TAG" -d "$WORK"
i=0
until helm push "$WORK/ktms-$VERSION.tgz" "oci://$REG/charts" --plain-http >"$WORK/push.log" 2>&1; do
	i=$((i + 1))
	[ "$i" -gt 30 ] && { cat "$WORK/push.log" >&2; fail "pushing the chart to the registry"; }
	sleep 1
done
echo "helm-kind: chart pushed to oci://$REG/charts/ktms"

run "starting Postgres" $K run postgres --image=postgres:17-alpine --env=POSTGRES_PASSWORD=pg --env=POSTGRES_DB=ktms --port=5432
run "exposing Postgres" $K expose pod postgres --port=5432
run "waiting for Postgres" $K wait --for=condition=Ready pod/postgres --timeout=180s
run "creating the secret" $K create secret generic ktms-secrets \
	--from-literal=DATABASE_URL="postgres://postgres:pg@postgres:5432/ktms?sslmode=disable" \
	--from-literal=KTMS_ENCRYPTION_KEYS="1:$(openssl rand -base64 32)"

# No image.tag: the chart's appVersion must name the image on its own.
run "helm install (pods not ready)" helm --kube-context "kind-$P" install ktms "oci://$REG/charts/ktms" \
	--version "$VERSION" --plain-http --set secrets.existingSecret=ktms-secrets \
	--set image.repository="$REPO" --set image.pullPolicy=Never --wait --timeout 5m
got=$($K get deploy -l app.kubernetes.io/name=ktms -o jsonpath='{.items[0].spec.template.spec.containers[0].image}')
[ "$got" = "$IMAGE" ] || fail "the chart runs $got, want $IMAGE"
ready=$($K get deploy -l app.kubernetes.io/name=ktms -o jsonpath='{.items[0].status.readyReplicas}')
echo "helm-kind: installed from OCI, $ready replicas ready on $got"
echo "helm-kind passed"
