#!/bin/bash

set -e -o pipefail

CHECK_IMAGES="\
  nri-resource-policy-balloons \
  nri-resource-policy-template \
  nri-resource-policy-topology-aware \
  nri-config-manager \
  nri-memory-qos \
  nri-memtierd \
  nri-sgx-epc \
  nri-resource-annotator \
"

CHECK_CHARTS="\
  nri-resource-policy-balloons \
  nri-resource-policy-template \
  nri-resource-policy-topology-aware \
  nri-memory-qos \
  nri-memtierd \
  nri-sgx-epc \
  nri-resource-annotator \
"

CHECKS="all"

IMAGE_URL="ghcr.io/containers/nri-plugins"
INDEX_URL="https://containers.github.io/nri-plugins/index.yaml"

info() {
    echo "$*"
}

error() {
    echo "ERROR: $*" >&2
}

check-images() {
    local status=0 pkg img

    info "Checking images for version $VERSION..."
    for pkg in $CHECK_IMAGES; do \
        img="$IMAGE_URL/$pkg"
        info "  $img..."
        if ! result=$(skopeo inspect --format "$pkg: {{.Digest}}" "docker://$img:$VERSION"); then
            info "  FAIL: $img:$VERSION NOT FOUND"
            status=1
        else
            info "  OK: $result"
        fi
    done

    return $status
}

check-helm-charts() {
    local status=0 pkg

    rm -f helm-index
    info "Downloading Helm index..."
    if ! wget -q $INDEX_URL -Ohelm-index; then
        error "Failed to download Helm index"
        return 1
    fi

    info "Checking Helm charts for version $VERSION..."
    for pkg in $CHECK_CHARTS; do
        info "  $pkg:$VERSION..."
        if [ "$(cat helm-index | yq ".entries.$pkg[] | select(.version == \"$VERSION\") | length > 0")" != "true" ]; then
            info "  FAIL: Helm chart $pkg:$VERSION NOT FOUND"
            status=1
        else
            info "  OK: $pkg"
        fi
    done
    rm -f helm-index

    return $status
}

while [ $# -gt 0 ]; do
    case $1 in
        -h|--help)
            echo "Usage: $0 [--images] [--charts] <version-tag>"
            exit 0
            ;;
        --images|-i)
            CHECKS="images"
            ;;
        --charts|--helm|-c)
            CHECKS="charts"
            ;;
        *)
            if [ -n "$VERSION" ]; then
               error "Needs a single version but multiple given ($VERSION, $1)"
               exit 1
            fi
            VERSION="$1"
            ;;
    esac
    shift
done

if [ -z "$VERSION" ]; then
    error "Usage: $0 <version>"
    exit 1
fi

status=0

if [ "$CHECKS" = "all" ] || [ "$CHECKS" = "images" ]; then
    if ! check-images; then
        status=1
    else
        info "All expected images found."
    fi
else
    info "Skipping image checks."
fi

if [ "$CHECKS" = "all" ] || [ "$CHECKS" = "charts" ]; then
    if ! check-helm-charts; then
        status=1
    else
        info "All expected Helm charts found."
    fi
else
    info "Skipping Helm chart checks."
fi

if [ $status -eq 0 ]; then
    info "SUCCESS: All expected artifacts for $VERSION found."
else
    error "FAILED: Some artifacts for release $VERSION not found."
fi

exit $status
