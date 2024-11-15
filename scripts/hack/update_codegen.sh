#!/bin/bash -e

set -o errexit
set -o nounset
set -o pipefail

trap cleanup EXIT
GO_CMD="${1:-go}"
BUILD_AUX="${2:-${REPO_ROOT}/build-aux}"
REPO_ROOT="$(git rev-parse --show-toplevel)"
CURRENT_DIR="$(dirname "${BASH_SOURCE[0]}")"
TEMP_VENDOR_DIR="${BUILD_AUX}/vendor"

function cleanup() {
  echo "Cleaning up temporary vendor directory..."
  rm -rf "${TEMP_VENDOR_DIR}"
}

pushd "$REPO_ROOT" > /dev/null
"${GO_CMD}" mod vendor
mv vendor "${TEMP_VENDOR_DIR}"

CODEGEN_PKG=$(cd "${TEMP_VENDOR_DIR}"; "${GO_CMD}" list -m -mod=readonly -f "{{.Dir}}" k8s.io/code-generator)
popd > /dev/null

# shellcheck source=/dev/null
source "${CODEGEN_PKG}/kube_codegen.sh"

cd "${REPO_ROOT}/pkg/apis"

kube::codegen::gen_client \
    --output-dir "${REPO_ROOT}/pkg/generated" \
    --output-pkg github.com/containers/nri-plugins/pkg/generated \
    --boilerplate "${REPO_ROOT}/scripts/hack/boilerplate.go.txt" \
    "${REPO_ROOT}/pkg/apis"
