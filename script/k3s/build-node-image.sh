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

# Builds a k3s node image with this repository's stargz snapshotter built in
# and prints its reference. The image stays in the local Docker daemon, so the
# caller can run it with k3d or export it with `docker save`. Everything but
# the reference goes to stderr.

set -euo pipefail

CONTEXT="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )/"
REPO="${CONTEXT}../../"

K3S_VERSION=main
K3S_REPO=https://github.com/k3s-io/k3s
K3S_NODE_REPO=ghcr.io/stargz-containers
K3S_NODE_IMAGE_NAME=k3s

exec 3>&1 1>&2

TMP_K3S_REPO=$(mktemp -d)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm -rf "${TMP_K3S_REPO}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

git clone -b ${K3S_VERSION} --depth 1 "${K3S_REPO}" "${TMP_K3S_REPO}"

mkdir -p "${TMP_K3S_REPO}/scripts/vendor.tmp/github.com/containerd/stargz-snapshotter"
( cd "${REPO}" && git archive HEAD | tar -x -C "${TMP_K3S_REPO}/scripts/vendor.tmp/github.com/containerd/stargz-snapshotter" )

echo "replace github.com/containerd/stargz-snapshotter => ./scripts/vendor.tmp/github.com/containerd/stargz-snapshotter" >> "${TMP_K3S_REPO}/go.mod"
echo "replace github.com/containerd/stargz-snapshotter/estargz => ./scripts/vendor.tmp/github.com/containerd/stargz-snapshotter/estargz" >> "${TMP_K3S_REPO}/go.mod"

cat "${TMP_K3S_REPO}/go.mod"

# K3s's script to create the image requires specific name for the tag determined by the k8s.io/kubernetes go module's version name (see scripts/version.sh)
REQUIRED_VERSION_NAME=$(go -C "${TMP_K3S_REPO}" list -mod=readonly -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' k8s.io/kubernetes)
K3S_NODE_IMAGE="${K3S_NODE_REPO}/${K3S_NODE_IMAGE_NAME}:${REQUIRED_VERSION_NAME}"

(
    cd "${TMP_K3S_REPO}" && \
        export GOTOOLCHAIN=auto && \
        git config user.email "dummy@example.com" && \
        git config user.name "dummy" && \
        sed -i 's/timeout:.*$/timeout: 10m/' ./.golangci.yml && \
        go mod tidy && \
        make deps && \
        git add . && \
        git commit -m tmp && \
        REPO="${K3S_NODE_REPO}" IMAGE_NAME="${K3S_NODE_IMAGE_NAME}" TAG="${REQUIRED_VERSION_NAME}" make image
)

echo "${K3S_NODE_IMAGE}" >&3
