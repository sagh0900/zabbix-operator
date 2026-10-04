#!/bin/sh
# Runs the end-to-end suite on a dedicated kind cluster: creates the cluster (or reuses it),
# installs CloudNativePG, builds and loads the operator image, installs the operator and
# runs test/e2e. Only ever talks to that cluster, through its own kubeconfig.
#
#   hack/e2e-kind.sh            # E2E_RUN=TestJourney/Upgrade limits the tests run
set -eu
CLUSTER=${E2E_CLUSTER:-zabbix-e2e}
CNPG_VERSION=${CNPG_VERSION:-1.30.1}
KUBECONFIG_FILE=$PWD/bin/e2e-kubeconfig
IMG=zabbix-operator:e2e-$(git rev-parse --short HEAD)
mkdir -p bin

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --config hack/e2e-kind.yaml --kubeconfig "$KUBECONFIG_FILE" --wait 5m
fi
kind get kubeconfig --name "$CLUSTER" > "$KUBECONFIG_FILE"
k() { kubectl --kubeconfig "$KUBECONFIG_FILE" --context "kind-$CLUSTER" "$@"; }

k apply --server-side -f "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${CNPG_VERSION%.*}/releases/cnpg-$CNPG_VERSION.yaml"
k -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=5m

docker build -t "$IMG" .
# Stream the image into every node's containerd (works where "kind load" cannot read the
# Docker image store, for example with a snap-packaged Docker).
for node in $(kind get nodes --name "$CLUSTER"); do
  docker save "$IMG" | docker exec -i "$node" ctr --namespace k8s.io images import --all-platforms - >/dev/null
done

make build-installer IMG="$IMG" >/dev/null
k apply --server-side --force-conflicts -f dist/install.yaml
k -n zabbix-operator rollout restart deployment/zabbix-operator-controller-manager
k -n zabbix-operator rollout status deployment/zabbix-operator-controller-manager --timeout=3m

E2E_KUBECONFIG="$KUBECONFIG_FILE" E2E_CONTEXT="kind-$CLUSTER" \
  go test -tags e2e ./test/e2e/ -v -count=1 -timeout 90m ${E2E_RUN:+-run "$E2E_RUN"}
