#!/usr/bin/env bash

# For plotting graphs.

SCRIPT_DIR="$(dirname "$(realpath "${BASH_SOURCE[0]}")")"
BASE_DIR="$(realpath "${SCRIPT_DIR}/..")"

if [ ! -z "$PREFIX" ]; then
    PREFIX="-p $PREFIX"
fi

python3 ${SCRIPT_DIR}/plot-graphs.py "$PREFIX" -o "${BASE_DIR}/output/traces.png" -l "topology_aware-jaeger,balloons-jaeger,template-jaeger,baseline-jaeger" "${BASE_DIR}/output"
python3 ${SCRIPT_DIR}/plot-graphs.py "$PREFIX" -o "${BASE_DIR}/output/resource_usage.png" -l "topology_aware-prometheus,balloons-prometheus,template-prometheus" "${BASE_DIR}/output"
