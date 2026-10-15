#!/bin/bash

SUMMARY="licenses-summary.txt"
CSVFILE="licenses.csv"
IGNORED="--ignore github.com/containers/nri-plugins/pkg/topology"
PROGRESS="${PROGRESS:---progress}"

# For dockerized runs pin license extraction tool image to :main.
# Without docker we fall back to a binary on $PATH.
GO_LICENSES_IMAGE="${GO_LICENSES_IMAGE:-ghcr.io/klihub/go-and-maybe-cache-licenses:main}"
GO_LICENSES_BINARY="${GO_LICENSES_BINARY:-go-and-maybe-cache-licenses}"

if [ -z "$GO_LICENSES_DOCKER" ]; then
    if command -v docker >/dev/null 2>&1; then
        GO_LICENSES_DOCKER=1
    else
        GO_LICENSES_DOCKER=0
    fi
fi

if [ "$#" -lt 2 ]; then
    echo "Usage: $0 <output-directory> <name>=<source-directory>..."
    exit 1
fi

OUT_ROOT="$1"; shift
TARGETS=("$@")

TOP_DIR=$(cd "$(dirname "$0")/../.." && pwd)
GO_LICENSES_CACHE="${GO_LICENSES_CACHE:-$TOP_DIR/build/cache/licenses}"
# A warm go build cache speeds up "go list", so we'll mount it in our
# build containers. Used for resolving licenses.
GO_LICENSES_GOCACHE="${GO_LICENSES_GOCACHE:-$TOP_DIR/build/cache/go-build}"
OUT_ROOT_ABS=$(readlink -m "$OUT_ROOT")

names=()
for target in "${TARGETS[@]}"; do
    names+=("${target%%=*}")
done

echo "Extracting/checking licenses for ${names[*]}..."

# Pre-create directories for all targets. The extraction tool now
# handles all targets in a single go.
mkdir -p "$OUT_ROOT_ABS"
for name in "${names[@]}"; do
    rm -fr "$OUT_ROOT_ABS/$name"
done

if [ "$GO_LICENSES_DOCKER" = 1 ]; then
    GOMODCACHE="${GOMODCACHE:-$(go env GOMODCACHE 2>/dev/null)}"
    if [ -z "$GOMODCACHE" ]; then
        echo "Cannot determine GOMODCACHE; is go installed?"
        exit 1
    fi
    mkdir -p "$GO_LICENSES_CACHE" "$GO_LICENSES_GOCACHE"

    # Mount the module cache writable so missing modules can be fetched.
    # HOME and GOCACHE must be writable for the mapped user.
    go_licenses() {
        docker run --rm \
            --user "$(id -u):$(id -g)" \
            -v "$TOP_DIR":/work:ro \
            -v "$GOMODCACHE":/go/pkg/mod \
            -v "$GO_LICENSES_CACHE":/cache \
            -v "$OUT_ROOT_ABS":/out \
            -v "$GO_LICENSES_GOCACHE":/gocache \
            -e GO_LICENSES_CACHE_DIR=/cache \
            -e HOME=/tmp \
            -e GOCACHE=/gocache \
            -e GOFLAGS= \
            "$GO_LICENSES_IMAGE" "$@"
    }
    SAVE_ROOT="/out"
    REPORT_PATH="/out/{{.Name}}/$CSVFILE"
else
    go_licenses() {
        "$GO_LICENSES_BINARY" "$@"
    }
    SAVE_ROOT="$OUT_ROOT_ABS"
    REPORT_PATH="$OUT_ROOT_ABS/{{.Name}}/$CSVFILE"
fi

go_licenses check $IGNORED $PROGRESS --target_path '{{.Name}}' "${TARGETS[@]}" && \
    go_licenses save $IGNORED $PROGRESS --save_path "$SAVE_ROOT" \
        --target_path '{{.Name}}' "${TARGETS[@]}" && \
        go_licenses report $IGNORED $PROGRESS --target_path "$REPORT_PATH" \
            "${TARGETS[@]}"
status="$?"

if [ "$status" != "0" ]; then
    echo "License check FAILED, status $status"
    exit $status
fi

for name in "${names[@]}"; do
    cat "$OUT_ROOT_ABS/$name/$CSVFILE" | sed 's/^.*,//g' | sort -u \
        > "$OUT_ROOT_ABS/$name/$SUMMARY"
done
