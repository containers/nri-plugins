#!/usr/bin/env bash

SCRIPT_DIR="$(dirname "$(realpath "${BASH_SOURCE[0]}")")"
BASE_DIR="$(realpath "${SCRIPT_DIR}/..")"

# This scripts simply deletes all possible deployments.
# Deployments that do not exist will result in errors but they can be ignored.

kubectl delete -f "${BASE_DIR}/manifests/"

helm uninstall -n monitoring prometheus
