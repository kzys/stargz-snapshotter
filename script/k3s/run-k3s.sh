#!/bin/bash

#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

set -euo pipefail

CONTEXT="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )/"
REGISTRY_HOST=k3s-private-registry

# Arguments
K3S_CLUSTER_NAME="${1}"
K3S_USER_KUBECONFIG="${2}"
K3S_REGISTRY_CA="${3}"
REGISTRY_NETWORK="${4}"
DOCKERCONFIGJSON_DATA="${5}"

TMP_BUILTIN_CONF=$(mktemp)
TMP_CONTEXT=$(mktemp -d)
SN_KUBECONFIG=$(mktemp)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm "${SN_KUBECONFIG}"
    rm -rf "${TMP_CONTEXT}"
    rm -rf "${TMP_BUILTIN_CONF}"
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

# K3S_NODE_IMAGE names a prebuilt node image (see build-node-image.sh) already
# loaded in the local Docker daemon; CI builds it once and shares it between
# the k3s jobs.
if [ -z "${K3S_NODE_IMAGE:-}" ] ; then
    echo "Preparing node image..."
    K3S_NODE_IMAGE="$("${CONTEXT}"/build-node-image.sh)"
fi

cat <<EOF > "${TMP_BUILTIN_CONF}"
mirrors:
  ${REGISTRY_HOST}:5000:
    endpoint:
      - ${REGISTRY_HOST}:5000
      - ${REGISTRY_HOST}:5001 # dummy
configs:
  ${REGISTRY_HOST}:5000:
    tls:
      ca_file: /registry.crt
EOF

echo "Createing k3s cluster"
k3d cluster create "${K3S_CLUSTER_NAME}" --image="${K3S_NODE_IMAGE}" \
    --registry-config="${TMP_BUILTIN_CONF}" -v "${K3S_REGISTRY_CA}":/registry.crt:ro \
    --k3s-arg='--snapshotter=stargz@server:*;agent:*' \
    --env "CONTAINERD_LOG_LEVEL=debug@server:*;agent:*"
k3d kubeconfig get "${K3S_CLUSTER_NAME}" > "${K3S_USER_KUBECONFIG}"
K3S_NODENAME="$(k3d node list | grep ${K3S_CLUSTER_NAME}-server-0 | cut -d " " -f 1 | tr -d '\n')"
docker network connect "${REGISTRY_NETWORK}" "${K3S_NODENAME}"

echo "Configuring kubernetes cluster..."
CONFIGJSON_BASE64="$(cat ${DOCKERCONFIGJSON_DATA} | base64 -i -w 0)"
cat <<EOF | KUBECONFIG="${K3S_USER_KUBECONFIG}" kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: ns1
---
apiVersion: v1
kind: Secret
metadata:
  name: testsecret
  namespace: ns1
data:
  .dockerconfigjson: ${CONFIGJSON_BASE64}
type: kubernetes.io/dockerconfigjson
EOF
