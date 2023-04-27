#!/usr/bin/env bash

# For deploying Prometheus and Jaeger tracing all-in-one.

SCRIPT_DIR="$(dirname "$(realpath "${BASH_SOURCE[0]}")")"
BASE_DIR="$(realpath "${SCRIPT_DIR}/..")"

USE_PROMETHEUS="true"

usage () {
    echo "usage: $0 -p <use prometheus: \"true\" or \"false\">"
    exit 1
}

while getopts ":n:p:" option; do
    case ${option} in
        p)
            USE_PROMETHEUS="${OPTARG}"
            if [ "${OPTARG}" != "true" ] && [ "${OPTARG}" != "false" ]; then
                usage
            fi
            ;;
        \?)
            usage
    esac
done

kubectl create namespace monitoring
kubectl apply -f "${BASE_DIR}/manifests/jaeger-deployment.yaml"

if [ "${USE_PROMETHEUS}" == "true" ]; then
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
    helm install prometheus prometheus-community/prometheus --version 19.7.2 -f prometheus-values.yaml --namespace monitoring --create-namespace
fi

# Wait for deployments to be ready.
kubectl rollout status deployment -n monitoring prometheus-server
kubectl rollout status deployment -n monitoring jaeger
