#!/bin/bash
# Usage: run-all.sh [--image TAR] [--prepare-node] [TEST...]
#
# Deploy nri-dra-driver-cxl in the local configuration (deploy.sh) and run
# the tests (default: all test*.sh in this directory), each with its output
# in $RESULTS_DIR/<test>.log (default /tmp/dra-driver-cxl-local). The
# driver is left deployed local.
DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
RESULTS_DIR=${RESULTS_DIR:-/tmp/dra-driver-cxl-local}
deploy_args=() tests=()
while [ $# -gt 0 ]; do
    case "$1" in
        --image) deploy_args+=(--image "$2"); shift ;;
        --prepare-node) deploy_args+=(--prepare-node) ;;
        *) tests+=("$1") ;;
    esac
    shift
done
[ ${#tests[@]} -gt 0 ] || tests=("$DIR"/test[0-9]*.sh)
mkdir -p "$RESULTS_DIR"
"$DIR/deploy.sh" "${deploy_args[@]}" local > "$RESULTS_DIR/deploy.log" 2>&1 ||
    { cat "$RESULTS_DIR/deploy.log"; exit 1; }
rc=0
for t in "${tests[@]}"; do
    name=$(basename "$t" .sh)
    echo "=== running $name"
    "$DIR/$name.sh" > "$RESULTS_DIR/$name.log" 2>&1 || rc=1
    sed -n '/^=== /,$p' "$RESULTS_DIR/$name.log"
done
exit $rc
