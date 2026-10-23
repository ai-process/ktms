#!/bin/sh
# Installs the Helm chart the way users do, from an OCI registry, on a fresh kind
# cluster with this tree's image and an in-cluster Postgres, and waits until the
# pods are ready (/readyz: the database answers and migrations ran).
set -eu

cd "$(dirname "$0")/.."
P=ktms-chart-$$
REG_PORT=${REG_PORT:-5055}
VERSION=0.0.0-ci
WORK=$(mktemp -d)

cleanup() {
	kind delete cluster --name "$P" >/dev/null 2>&1 || true
	docker rm -f "$P-registry" >/dev/null 2>&1 || true
	docker rmi -f "coyl/ktms:$VERSION" >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

fail() {
	echo "helm-kind: $*" >&2
	kubectl --context "kind-$P" get pods -A >&2 || true
	kubectl --context "kind-$P" logs -l app.kubernetes.io/name=ktms --tail=30 >&2 || true
	exit 1
}

echo "helm-kind: building the image and a kind cluster"
docker build -q --target slim -t "coyl/ktms:$VERSION" . >/dev/null || fail "building the image"
kind create cluster --name "$P" --wait 120s >/dev/null 2>&1 || fail "creating the kind cluster"
kind load docker-image "coyl/ktms:$VERSION" --name "$P" >/dev/null 2>&1 || fail "loading the image"
K="kubectl --context kind-$P"

docker pull -q registry:2 >/dev/null
docker run -d --name "$P-registry" -p "127.0.0.1:$REG_PORT:5000" registry:2 >/dev/null
helm package deploy/helm/ktms --version "$VERSION" --app-version "$VERSION" -d "$WORK" >/dev/null
i=0
until helm push "$WORK/ktms-$VERSION.tgz" "oci://127.0.0.1:$REG_PORT/charts" --plain-http >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -gt 30 ] && fail "pushing the chart to the registry"
	sleep 1
done
echo "helm-kind: chart pushed to oci://127.0.0.1:$REG_PORT/charts/ktms"

$K run postgres --image=postgres:17-alpine --env=POSTGRES_PASSWORD=pg --env=POSTGRES_DB=ktms --port=5432 >/dev/null
$K expose pod postgres --port=5432 >/dev/null
$K wait --for=condition=Ready pod/postgres --timeout=180s >/dev/null || fail "Postgres did not start"
$K create secret generic ktms-secrets \
	--from-literal=DATABASE_URL="postgres://postgres:pg@postgres:5432/ktms?sslmode=disable" \
	--from-literal=KTMS_ENCRYPTION_KEYS="1:$(openssl rand -base64 32)" >/dev/null

# No --set image.tag: the chart's appVersion must name the image on its own.
helm --kube-context "kind-$P" install ktms "oci://127.0.0.1:$REG_PORT/charts/ktms" --version "$VERSION" \
	--plain-http --set secrets.existingSecret=ktms-secrets --set image.pullPolicy=Never \
	--wait --timeout 5m >/dev/null 2>&1 || fail "helm install did not become ready"
want="coyl/ktms:$VERSION"
got=$($K get deploy -l app.kubernetes.io/name=ktms -o jsonpath='{.items[0].spec.template.spec.containers[0].image}')
[ "$got" = "$want" ] || fail "the chart runs $got, want $want"
ready=$($K get deploy -l app.kubernetes.io/name=ktms -o jsonpath='{.items[0].status.readyReplicas}')
echo "helm-kind: installed from OCI, $ready replicas ready on $got"
echo "helm-kind passed"
