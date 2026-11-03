#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
STATE="$ROOT/.state"
KUBECTL=kubectl
KUBECONFIG="$STATE/kubeconfig"
export KUBECONFIG
CLUSTER=agent-sandbox-test
CONTEXT=kind-agent-sandbox-test
NODE_IMAGE=kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5
RELEASE=https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.8
RAW=https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/v1.0.8
TEMP=

fail() { printf '%s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "Ferramenta necessária: $1"; }
k() { "$KUBECTL" --kubeconfig "$KUBECONFIG" --context "$CONTEXT" "$@"; }
verify() { printf '%s  %s\n' "$2" "$1" | sha256sum -c -; }
fetch() { curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 "$1" -o "$2"; }
finish() {
    if [ -n "$TEMP" ]; then rm -rf "$TEMP"; fi
    rmdir "$STATE/lock"
}

mkdir -p "$STATE"
mkdir "$STATE/lock" 2>/dev/null || fail 'Outra operação em andamento (.state/lock).'
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
umask 077
TEMP=$(mktemp -d)

check_client() {
    need kubectl
    client_version=$("$KUBECTL" version --client -o yaml)
    actual=$(printf '%s\n' "$client_version" | awk '/gitVersion:/ {print $2; exit}')
    case "$actual" in v1.35.*|v1.36.*|v1.37.*) ;; *) fail "kubectl $actual incompatível: esta POC usa Kubernetes 1.36 e aceita kubectl 1.35, 1.36 ou 1.37." ;; esac
}

check_kind() {
    need kind
    actual=$(kind version)
    printf '%s\n' "$actual" | awk '
        $1 == "kind" && $2 ~ /^v[0-9]+\.[0-9]+\.[0-9]+$/ {
            split(substr($2, 2), version, ".")
            supported = version[1] > 0 || (version[1] == 0 && version[2] >= 32)
        }
        END { exit !supported }
    ' || fail "kind incompatível ou versão não reconhecida: $actual. Esta POC requer uma versão estável do kind >= v0.32.0 para Kubernetes 1.36.1; atualize manualmente se necessário. Nenhuma ferramenta será instalada."
}

managed_cluster() {
    check_client
    check_kind
    [ -s "$STATE/cluster-uid" ] && [ -s "$KUBECONFIG" ] || fail 'Cluster sem registro local de propriedade; operação recusada.'
    clusters=$(kind get clusters)
    printf '%s\n' "$clusters" | has_cluster || fail 'Cluster dedicado ausente.'
    uid=$(k get namespace kube-system -o jsonpath='{.metadata.uid}')
    [ "$uid" = "$(cat "$STATE/cluster-uid")" ] || fail 'UID do cluster diverge; operação recusada.'
    if [ "${1:-}" != cleanup ]; then
        server_version=$(k version -o yaml)
        actual=$(printf '%s\n' "$server_version" | awk '/^serverVersion:/ {server=1; next} server && /gitVersion:/ {print $2; exit}')
        case "$actual" in
            v1.36.*) ;;
            *) fail "Cluster gerenciado em ${actual:-versão desconhecida}; esperado Kubernetes 1.36. Para recriar, execute explicitamente make cleanup (apaga os recursos da POC) e depois make up." ;;
        esac
    fi
}

has_cluster() {
    grep -Fx "$CLUSTER" >/dev/null
}

case "${1:-}" in
doctor)
    for tool in make curl sha256sum docker; do need "$tool"; done
    check_kind
    check_client
    kind version
    "$KUBECTL" version --client
    docker info --format 'Docker {{.ServerVersion}} / cgroup v{{.CgroupVersion}}'
    ;;
cluster-up)
    check_client
    check_kind
    need docker
    docker info >/dev/null
    clusters=$(kind get clusters)
    if printf '%s\n' "$clusters" | has_cluster; then
        managed_cluster
        printf '%s\n' 'Cluster gerenciado já existe.'
        exit 0
    fi
    [ ! -e "$STATE/cluster-uid" ] && [ ! -e "$KUBECONFIG" ] || fail 'Estado anterior encontrado; investigue antes de recriar o cluster.'
    kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --config "$ROOT/manifests/kind.yaml" --kubeconfig "$KUBECONFIG" --wait 90s
    uid=$(k get namespace kube-system -o jsonpath='{.metadata.uid}')
    [ -n "$uid" ] || fail 'UID do cluster ausente.'
    printf '%s\n' "$uid" > "$STATE/cluster-uid"
    k version
    ;;
controller)
    managed_cluster
    need curl
    need sha256sum
    fetch "$RELEASE/sandbox-with-extensions.yaml" "$TEMP/controller.yaml"
    verify "$TEMP/controller.yaml" e68bfae0bd9b6ff717745a4be04bcc909594ba32011b6a0891b77c4d7b5bf58a
    k apply -f "$TEMP/controller.yaml"
    k wait --for=condition=Established crd/sandboxtemplates.extensions.agents.x-k8s.io crd/sandboxwarmpools.extensions.agents.x-k8s.io crd/sandboxclaims.extensions.agents.x-k8s.io --timeout=90s
    k -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s
    ;;
router)
    managed_cluster
    need curl
    need sha256sum
    fetch "$RAW/clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml" "$TEMP/router-source.yaml"
    verify "$TEMP/router-source.yaml" 9662645e1f2bebd77e318636618ffc029c15565c0dace72ac8a8b40c5e0253f1
    sed -e 's|${ROUTER_IMAGE}|us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/sandbox-router:latest-main|g' -e '/ALLOW_UNAUTHENTICATED_ROUTER/{n;s/value: "false"/value: "true"/;}' "$TEMP/router-source.yaml" > "$TEMP/router.yaml"
    k -n agent-sandbox-system apply -f "$TEMP/router.yaml"
    k -n agent-sandbox-system rollout status deployment/sandbox-router-deployment --timeout=180s
    ;;
pool)
    managed_cluster
    k apply -f "$ROOT/manifests/pool.yaml"
    ;;
smoke)
    managed_cluster
    k apply -f "$ROOT/manifests/smoke.yaml"
    k -n default wait --for=condition=Ready sandboxclaim/poc-smoke --timeout=180s
    sandbox=$(k -n default get sandboxclaim poc-smoke -o jsonpath='{.status.sandbox.name}')
    [ -n "$sandbox" ] || fail 'Claim Ready sem nome de sandbox.'
    printf 'Sandbox provisionado: %s\n' "$sandbox"
    k -n default get sandbox "$sandbox"
    k -n default delete sandboxclaim poc-smoke --wait=true --timeout=90s
    ;;
status)
    managed_cluster
    k get nodes
    k -n agent-sandbox-system get deployments,pods,services
    k -n default get sandboxtemplates,sandboxwarmpools,sandboxclaims,sandboxes,pods
    ;;
dev-up)
    managed_cluster
    need docker
    need sha256sum
    docker info >/dev/null
    k get crd sandboxes.agents.x-k8s.io >/dev/null
    BUILDX_CONFIG="$STATE/buildx" docker build --tag agent-sandbox-go-dev:local "$ROOT/dev"
    image_id=$(docker image inspect agent-sandbox-go-dev:local --format '{{.Id}}')
    image="agent-sandbox-go-dev:${image_id#sha256:}"
    docker tag agent-sandbox-go-dev:local "$image"
    kind load docker-image --name "$CLUSTER" "$image"
    printf '%s\n' "$image" > "$STATE/dev-image"
    secret=$(k -n default get secret go-dev-auth --ignore-not-found -o name)
    if [ -z "$secret" ]; then
        dd if=/dev/urandom of="$TEMP/random" bs=32 count=1 2>/dev/null
        od -An -tx1 "$TEMP/random" | tr -d ' \n' > "$TEMP/password"
        k -n default create secret generic go-dev-auth --from-file=password="$TEMP/password" >/dev/null
    fi
    k apply -f "$ROOT/manifests/dev-storage.yaml"
    sed "s|DEV_IMAGE|$image|" "$ROOT/manifests/dev.yaml" > "$TEMP/dev.yaml"
    spec_hash=$(sha256sum "$TEMP/dev.yaml" | awk '{print $1}')
    existing=$(k -n default get sandbox go-dev --ignore-not-found -o name)
    current_hash=$(k -n default get sandbox go-dev --ignore-not-found -o jsonpath='{.metadata.annotations.poc\.agent-sandbox\.local/spec-hash}')
    if [ -n "$existing" ] && [ "$current_hash" != "$spec_hash" ]; then
        fail 'Imagem ou manifesto da IDE mudou. Execute make dev-down e make dev-up para aplicar; o PVC será preservado.'
    fi
    sed "s|DEV_SPEC_HASH|$spec_hash|" "$TEMP/dev.yaml" > "$TEMP/dev-final.yaml"
    k apply -f "$TEMP/dev-final.yaml"
    k -n default wait --for=condition=Ready sandbox/go-dev --timeout=180s
    k -n default wait --for=condition=Ready pod/go-dev --timeout=180s
    printf '%s\n' 'IDE pronta. Execute make dev-password e, em outro terminal, make dev-open.'
    ;;
dev-open)
    managed_cluster
    k -n default wait --for=condition=Ready pod/go-dev --timeout=90s
    printf '%s\n' 'IDE: http://127.0.0.1:8080 — senha: make dev-password. Ctrl+C encerra o túnel.'
    finish
    trap - EXIT INT TERM
    exec "$KUBECTL" --kubeconfig "$KUBECONFIG" --context "$CONTEXT" -n default port-forward --address 127.0.0.1 service/go-dev-ide 8080:8080
    ;;
dev-password)
    managed_cluster
    need base64
    encoded=$(k -n default get secret go-dev-auth -o jsonpath='{.data.password}')
    [ -n "$encoded" ] || fail 'Secret da IDE sem senha.'
    printf '%s' "$encoded" | base64 -d
    printf '\n'
    ;;
dev-down)
    managed_cluster
    k -n default delete service/go-dev-ide sandbox/go-dev --ignore-not-found --cascade=foreground --wait=true --timeout=90s
    printf '%s\n' 'IDE parada; PVC go-dev-home e Secret go-dev-auth preservados.'
    ;;
cleanup)
    managed_cluster cleanup
    kind delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
    clusters=$(kind get clusters)
    if printf '%s\n' "$clusters" | has_cluster; then fail 'Cluster ainda existe; estado preservado.'; fi
    rm -f "$STATE/cluster-uid" "$KUBECONFIG"
    ;;
*) fail 'Operação inválida; consulte make help.' ;;
esac
