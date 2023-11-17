#!/usr/bin/env bash

SCRIPT_DIR="$(dirname "$(realpath "${BASH_SOURCE[0]}")")"
BASE_DIR="$(realpath "${SCRIPT_DIR}/..")"

LOG_DIR="$BASE_DIR/output"
RUNTIME=${RUNTIME:-containerd}
OUTPUT_PREFIX=""

PREREQUISITES="python3 nc envsubst"

mkdir -p "$LOG_DIR"

PARAMS="$*"
if [ -z "$PARAMS" ]; then
    PARAMS="-n 10 -i 9"
fi

if [ ! -z "$PREFIX" ]; then
    PARAMS="$PARAMS -p $PREFIX"
    OUTPUT_PREFIX="${PREFIX}-"
fi

get_pod_name() {
    local pod
    local timeout=20

    pod=$(until kubectl get pods -n kube-system | awk '/nri-resource-policy-/ { print $1 }'
        do
            timeout=$(( $timeout - 1 ))
            if [ "$timeout" == "0" ]; then
            echo "Timeout while waiting nri resource policy plugin to start" > /dev/tty
            exit 1
            fi
            sleep 1
        done)

    if [ -z "$pod" ]; then
        echo "Pod not found" > /dev/tty
        exit 1
    fi

    kubectl wait --timeout=10s --for=condition=Ready -n kube-system pod/$pod >/dev/null 2>&1
    if [ $? -ne 0 ]; then
        echo "Pod $pod not ready" > /dev/tty
        exit 1
    fi

    echo $pod
}

check_prereqs() {
    local p='' t=''

    for p in $@; do
        if ! t="$(type -f $p)"; then
            echo "missing prerequisite $p"
            fail=1
        fi
    done

    if [ -z "$fail" ]; then
        return 0
    fi

    echo "missing dependencies/prerequisites, aborting tests"
    exit 1
}

resolve_helm_chart() {
    local name=$1
    local chart="${!name}"

    if [ "$name" == "baseline" ]; then
	return 0
    fi
    if [ -z "$chart" -o -d "$chart" ]; then
	return 0
    fi

    case $chart in
	1|yes)
	    chart="$BASE_DIR/../../deployment/helm/${name//_/-}"
	    if [ -d "$chart" ]; then
		eval "$name=$chart"
		return 0
	    fi
    esac

    echo 2>&1 "$name helm chart \"$chart\" not found"
    exit 1
}

pod=""

START_TIME=$(date +%s)

run_test() {
    local test=$1

    # Let resource policy plugin to start
    sleep 1

    pod=$(get_pod_name)

    local prefix=${OUTPUT_PREFIX}$(date -u +"%Y%m%dT%H%M%SZ" -d "@${START_TIME}")

    kubectl -n kube-system logs "$pod" -f > "$LOG_DIR/$prefix-$test.log" 2>&1 &

    echo "Executing: ${SCRIPT_DIR}/run-test.sh $PARAMS -l $test"
    echo "Log file: $LOG_DIR/$prefix-$test.log"

    local current_time=$(date +"%Y-%m-%d %H:%M:%S")

    RUNTIME="$RUNTIME" ${SCRIPT_DIR}/run-test.sh $PARAMS -l $test

    journalctl --since="$current_time" -u $RUNTIME > "$LOG_DIR/$prefix-$RUNTIME-$test.log"
}

cleanup_resource_policy() {
    # Remove all deployments of nri-plugins
    helm uninstall -n kube-system test-plugin
}

cleanup_all() {
    ${SCRIPT_DIR}/destroy-deployment.sh
    cleanup_resource_policy
}

plot_graphs() {
    local jaeger_labels="$1"
    local prometheus_labels="$2"
    echo "plotting latency graphs: ${SCRIPT_DIR}/plot-graphs.py -o ${BASE_DIR}/output/${OUTPUT_PREFIX}traces.png -l "$jaeger_labels" ${BASE_DIR}/output $PARAMS"
    ${SCRIPT_DIR}/plot-graphs.py -o "${BASE_DIR}/output/${OUTPUT_PREFIX}traces.png" -l "$jaeger_labels" "${BASE_DIR}/output" $PARAMS
    echo "plotting resource graphs: ${SCRIPT_DIR}/plot-graphs.py -o ${BASE_DIR}/output/${OUTPUT_PREFIX}resource_usage.png -l "$prometheus_labels" ${BASE_DIR}/output $PARAMS"
    ${SCRIPT_DIR}/plot-graphs.py -o "${BASE_DIR}/output/${OUTPUT_PREFIX}resource_usage.png" -l "$prometheus_labels" "${BASE_DIR}/output" $PARAMS
}

baseline="${baseline:-true}"

echo "***********"
echo "Note that you must install nri-resource-policy plugin images manually before running this script."
echo "***********"

baseline="${baseline:-true}"

if [ -z "$topology_aware" -o -z "$template" -o -z "$balloons" ]; then
    echo "Cannot find topology-aware, balloons or template helm charts. Set it before for example like this:"
    echo "topology_aware=<helm dir>/topology-aware topology_aware_overrides=<helm overrides for topology-aware plugin> balloons=<helm dir>/balloons balloons_overrides=<helm overrides for balloons plugin> template=<helm dir>/template template_overrides=<helm overrides for template plugin> ./scripts/run-tests.sh"
    echo
    echo "Using only partial resource policy deployments in the test:"
else
    echo "Using these resource policy deployments in the test:"
fi

echo "baseline       : ${baseline:-skipped}"
echo "topology_aware : ${topology_aware:-skipped}"
echo "  - overrides  : ${topology_aware_overrides:-none}"
echo "balloons       : ${balloons:-skipped}"
echo "  - overrides  : ${balloons_overrides:-none}"
echo "template       : ${template:-skipped}"
echo "  - overrides  : ${template_overrides:-none}"

cleanup_all

# Check that we have all prerequisites and deployment files.
check_prereqs $PREREQUISITES
plot_graphs --test-imports

for test in baseline template topology_aware balloons; do
    resolve_helm_chart $test
    if [ -n "${!test}" ]; then
	echo "$test: ${!test}"
    fi
done



# Note that with this script, we always run the baseline unless user
# sets "baseline=0" when starting the script, and those resource policy
# tests that user has supplied deployment file.
jaeger_labels=""
prometheus_labels=""
sep=""

for test in baseline template topology_aware balloons
do
    case $test in
        baseline)
            if [ -z "$baseline" -o "$baseline" != "true" ]; then
                continue
            fi
            jaeger_labels="baseline-jaeger"; sep=","
            # the baseline setup does not measure resource usage
            ;;
        template)
            if [ -z "$template" ]; then
                continue
            fi
            jaeger_labels="$jaeger_labels${sep}template-jaeger"; sep=","
            prometheus_labels="template-prometheus"; sep=","
            helm install -n kube-system test-plugin "$template" $template_overrides
            ;;
        topology_aware)
            if [ -z "$topology_aware" ]; then
                continue
            fi
            jaeger_labels="$jaeger_labels${sep}topology_aware-jaeger"; sep=","
            prometheus_labels="$prometheus_labels${sep}topology_aware-prometheus"; sep=","
            helm install -n kube-system test-plugin "$topology_aware" $topology_aware_overrides
            ;;
        balloons)
            if [ -z "$balloons" ]; then
                continue
            fi
            jaeger_labels="$jaeger_labels${sep}balloons-jaeger"; sep=","
            prometheus_labels="$prometheus_labels${sep}balloons-prometheus"; sep=","
            helm install -n kube-system test-plugin "$balloons" $balloons_overrides
            ;;
    esac

    # Install necessary deployments with the pre-run.sh script.
    # Unfortunately can not be done once before all tests
    # because some old Prometheus timeseries remain otherwise.
    ${SCRIPT_DIR}/pre-run.sh

    run_test $test
    cleanup_all
done

plot_graphs $jaeger_labels $prometheus_labels
