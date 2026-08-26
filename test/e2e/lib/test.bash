# Shared helpers for test cases (code.var.sh files).
#
# This library is not sourced by run.sh. Instead, run_tests.sh seeds the
# *.source.sh chain with it, so that
#
#   - the helpers live in the same subshell as the test code and nowhere else,
#     and cannot clash with the script API of run.sh, and
#   - any *.source.sh file (test suite, policy, topology or test case level) can
#     override a helper defined here, because those are sourced after this file.
#
# Helpers here are documented in the same format as the script API of run.sh,
# that is, a "# script API" marked function followed by indented comment lines.
# Run "./run.sh help" to print the documentation of all available functions.
#
# Add a helper here if more than one test case needs it. Keep policy-specific
# helpers in POLICY/*.source.sh instead.

# Fail if this was sourced without the primitives it builds on.
if ! type -t vm-command >/dev/null; then
    echo "error: lib/test.bash: lib/vm.bash must be sourced first" >&2
    return 1
fi

###
### Waiting for things to happen
###

retry-until() { # script API
    # Usage: retry-until [--timeout SECS] [--interval SECS] [--message MSG] SNIPPET
    #
    # Evaluate SNIPPET on the host repeatedly until it exits with status 0.
    # Give up after SECS seconds (default 30), waiting SECS seconds between
    # the attempts (default 1). Both must be integers. Print MSG before the
    # first attempt and when giving up.
    #
    # Return 0 if SNIPPET succeeded, non-zero if it did not. Never fails the
    # test, so that the caller can choose between error, command-error and
    # ignoring the timeout.
    #
    # SNIPPET is evaluated, so single-quote it to have the values it refers to
    # re-read on every attempt:
    #   retry-until --timeout 10 'vm-command "kubectl get pod $pod"'
    #
    # SNIPPET is evaluated in the scope of this function, so it must not refer
    # to variables named timeout, interval, message or elapsed.
    #
    # This is the host side counterpart of vm-run-until.
    local timeout=30 interval=1 message="" elapsed=0
    while [ "${1#--}" != "$1" ]; do
        case "$1" in
            --timeout)  timeout="$2"; shift 2;;
            --interval) interval="$2"; shift 2;;
            --message)  message="$2"; shift 2;;
            --)         shift; break;;
            *)          error "retry-until: unknown option \"$1\"";;
        esac
    done
    if [ -n "$message" ]; then
        echo "waiting for: $message"
    fi
    while true; do
        if eval "$*"; then
            return 0
        fi
        if [ "$elapsed" -ge "$timeout" ]; then
            break
        fi
        sleep "$interval"
        elapsed=$(( elapsed + interval ))
    done
    echo "timeout after ${timeout}s${message:+ waiting for: $message}" >&2
    return 1
}

container-state() { # script API
    # Usage: container-state POD CONTAINER
    #
    # Print the state object of CONTAINER of POD, and store it in
    # COMMAND_OUTPUT. CONTAINER is a container name, the index of a container
    # in the pod, or empty for all containers of the pod.
    local pod=$1 ctr=$2 jqsel
    if [ -z "$ctr" ]; then
        jqsel='.[]'
    elif [[ "$ctr" =~ ^[0-9]+$ ]]; then
        jqsel=".[$ctr]"
    else
        jqsel="map(select(.name == \"$ctr\")) | .[0]"
    fi
    vm-command "kubectl get pod $pod -ojson | \
        jq '.status.containerStatuses | $jqsel | .state'"
}

wait-container-waiting-reason() { # script API
    # Usage: wait-container-waiting-reason POD CONTAINER REASON [TIMEOUT]
    #
    # Wait until the waiting reason of CONTAINER of POD becomes REASON,
    # TIMEOUT seconds at most, 5 by default. CONTAINER is passed to
    # container-state, so it can also be a container index or empty.
    # Fail the test on timeout.
    local pod=$1 ctr=$2 reason=$3 timeout=${4:-5}
    retry-until --timeout "$timeout" \
        'container-state "$pod" "$ctr" && grep -q "$reason" <<< "$COMMAND_OUTPUT"' || {
        error "container ${ctr:-*} of pod $pod did not enter the $reason state"
    }
}

verify-container-error() { # script API
    # Usage: verify-container-error POD CONTAINER REGEXP [TIMEOUT]
    #
    # Verify that creating CONTAINER of POD failed with an error matching
    # REGEXP. Wait TIMEOUT seconds, 5 by default, for the container to enter
    # the CreateContainerError state, then require REGEXP to match its state.
    #
    # Create the pod with wait="" to keep the framework from waiting for a
    # pod which is never going to become ready:
    #     wait="" CONTCOUNT=1 create guaranteed
    #     verify-container-error pod0 pod0c0 "invalid IRQ affinity"
    local pod=$1 ctr=$2 regexp=$3 timeout=${4:-5}

    wait-container-waiting-reason "$pod" "$ctr" CreateContainerError "$timeout"
    container-state "$pod" "$ctr"
    grep -q "$regexp" <<< "$COMMAND_OUTPUT" ||
        error "expected an error matching \"$regexp\" from creating container ${ctr:-*} of pod $pod"
}

wait-pod-gone() { # script API
    # Usage: wait-pod-gone POD [TIMEOUT]
    #
    # Wait until POD no longer exists, TIMEOUT seconds at most, 30 by default.
    # Fail the test if the pod is still there after that.
    local pod=$1 timeout=${2:-30}
    vm-run-until --timeout "$timeout" "! kubectl get pod $pod -o name 2>/dev/null | grep -q ." || {
        command-error "pod $pod did not disappear within ${timeout}s"
    }
}

###
### Launching the plugin
###

relaunch-policy() { # script API
    # Usage: relaunch-policy POLICY [CONFIG]
    #
    # Terminate the plugin if one is running, then launch POLICY with the
    # configuration helm override values in CONFIG. Without CONFIG, launch it
    # with the configuration helm-launch uses by default.
    #
    # Honour the same environment variables as helm-launch.
    #
    # Example:
    #     relaunch-policy balloons "$TEST_DIR/balloons-reserved.cfg"
    local policy=$1 config=$2

    helm-terminate
    if [ -n "$config" ]; then
        helm_config="$config" helm-launch "$policy"
    else
        helm-launch "$policy"
    fi
}

expect-launch-failure() { # script API
    # Usage: expect-launch-failure POLICY [TIMEOUT]
    #
    # Expect launching POLICY to fail within TIMEOUT, 5s by default. Fail the
    # test if the launch succeeds instead.
    #
    # Read the configuration from the helm_config variable, just like
    # helm-launch does. Use this to test that the plugin refuses an invalid
    # configuration:
    #     helm_config=$(instantiate broken-config.yaml) expect-launch-failure balloons
    local policy=$1 timeout=${2:-5s}

    # helm-launch is run in a subshell, because on some failures it calls
    # error, which would otherwise fail the test we expect to fail.
    if ( expect_error=1 launch_timeout=$timeout helm-launch "$policy" ); then
        error "launching $policy succeeded, but was expected to fail"
    fi
    echo "launching $policy failed as expected"
}

###
### Metrics and instrumentation
###

# The instrumentation HTTP server of the plugin, and its metrics endpoint.
# The plugin exports these only if its configuration opens the server.
instrumentation_url="http://localhost:8891"
verify_metrics_url="$instrumentation_url/metrics"

verify-metrics-has-line() { # script API
    # Usage: verify-metrics-has-line REGEXP [TIMEOUT]
    #
    # Wait until the metrics of the plugin have a line matching REGEXP, an
    # extended regular expression. Fail the test on timeout, 10s by default.
    local expected_line="$1"
    vm-run-until --timeout "${2:-10}" "echo 'waiting for metrics line: $expected_line' >&2; curl --silent --noproxy localhost $verify_metrics_url | grep -E '$expected_line'" || {
        command-error "expected line '$1' missing from the output"
    }
}

verify-metrics-has-no-line() { # script API
    # Usage: verify-metrics-has-no-line REGEXP [TIMEOUT]
    #
    # Wait until the metrics of the plugin have no line matching REGEXP. Fail
    # the test on timeout, 10s by default.
    local unexpected_line="$1"
    vm-run-until --timeout "${2:-10}" "echo 'checking absence of metrics line: $unexpected_line' >&2; ! curl --silent --noproxy localhost $verify_metrics_url | grep -Eq '$unexpected_line'" || {
        command-error "unexpected line '$1' found from the output"
    }
}

###
### Node resource topology
###

# The kubectl command which addresses the node resource topology of the node.
export nrt_kubectl_get="kubectl get noderesourcetopologies.topology.node.k8s.io \$(hostname)"

nrt-query() { # script API
    # Usage: nrt-query JQ-EXPRESSION
    #
    # Print the result of evaluating JQ-EXPRESSION on the node resource
    # topology of the node, and store it in COMMAND_OUTPUT.
    #
    # JQ-EXPRESSION must not contain single quotes.
    vm-command "$nrt_kubectl_get -o json | jq -r '$1'"
}

nrt-verify-zone-attribute() { # script API
    # Usage: nrt-verify-zone-attribute ZONE ATTRIBUTE REGEXP
    #
    # Fail the test unless the value of ATTRIBUTE of topology zone ZONE
    # matches REGEXP.
    local zone_name=$1
    local attribute_name=$2
    local expected_value_re=$3
    echo ""
    echo "### Verifying topology zone $zone_name attribute $attribute_name value matches $expected_value_re"
    nrt-query ".zones[] | select (.name == \"$zone_name\").attributes[] | select(.name == \"$attribute_name\").value"
    [[ "$COMMAND_OUTPUT" =~ $expected_value_re ]] ||
        command-error "expected zone $zone_name attribute $attribute_name value $expected_value_re, got: $COMMAND_OUTPUT"
}

nrt-verify-zone-resource() { # script API
    # Usage: nrt-verify-zone-resource ZONE RESOURCE FIELD VALUE
    #
    # Fail the test unless FIELD of RESOURCE of topology zone ZONE equals VALUE.
    local zone_name=$1
    local resource_name=$2
    local resource_field=$3
    local expected_value=$4
    echo ""
    echo "### Verifying topology zone $zone_name resource $resource_name field $resource_field equals $expected_value"
    nrt-query ".zones[] | select (.name == \"$zone_name\").resources[] | select(.name == \"$resource_name\").$resource_field"
    [[ "$COMMAND_OUTPUT" == "$expected_value" ]] ||
        command-error "expected zone $zone_name resource $resource_name.$resource_field $expected_value, got: $COMMAND_OUTPUT"
}

###
### Node state
###

clear-isolcpus() { # script API
    # Usage: clear-isolcpus
    #
    # Remove isolcpus from the kernel command line of the node, rebooting the
    # node if it is there. Do nothing if it is not.
    #
    # A test which isolates CPUs and does not get to restore the command line,
    # because it failed, leaves the CPUs isolated. That changes CPU pinning for
    # every test which runs after it on the same VM. Call this in a test which
    # needs to be sure that no CPUs are isolated.
    vm-command "grep isolcpus /proc/cmdline" || return 0

    vm-set-kernel-cmdline-reboot ""
    vm-command "grep isolcpus /proc/cmdline" &&
        error "failed to remove isolcpus from the kernel command line"

    echo "isolcpus removed from the kernel command line"
    return 0
}

disable-numa() { # script API
    # Usage: disable-numa [POLICY]
    #
    # Boot the node with a kernel which has NUMA support disabled, and make
    # sure that the container runtime and POLICY, $POLICY by default, are
    # running afterwards. Do nothing if NUMA is already disabled.
    vm-command '[ -d /sys/devices/system/node ]' || return 0

    vm-kernel-pkgs-install
    vm-post-reboot-runtime-check "${1:-$POLICY}"

    vm-command '[ -d /sys/devices/system/node ]' &&
        error "failed to disable NUMA in the kernel"
    return 0
}

enable-numa() { # script API
    # Usage: enable-numa [POLICY]
    #
    # Boot the node back with a kernel which has NUMA support, and make sure
    # that the container runtime and POLICY, $POLICY by default, are running
    # afterwards.
    vm-kernel-pkgs-uninstall
    vm-post-reboot-runtime-check "${1:-$POLICY}"
}

###
### CPU lists
###

expand-cpulist() { # script API
    # Usage: expand-cpulist CPULIST
    #
    # Print the CPUs in CPULIST as a sorted list of space separated CPU ids:
    #     expand-cpulist "0-2,5"   prints "0 1 2 5"
    #
    # A list which is already in the expanded form is printed as it is, so
    # this is safe to use for normalizing a list of unknown form.
    local cpus="$1"

    if [ "${cpus//-/}" == "$cpus" ] && [ "${cpus//,/}" == "$cpus" ]; then
        echo $cpus
        return 0
    fi

    python3 -c '
import sys
r = set()
for part in sys.argv[1].split(","):
    if not part:
        continue
    if "-" in part:
        a, b = part.split("-")
        r.update(range(int(a), int(b) + 1))
    else:
        r.add(int(part))
print(" ".join(str(x) for x in sorted(r)))
' "$cpus"
}

cpulist-difference() { # script API
    # Usage: cpulist-difference CPULIST1 CPULIST2
    #
    # Print the CPUs which are in CPULIST1 but not in CPULIST2, as a sorted
    # list of space separated CPU ids:
    #     cpulist-difference "1 2 3 4" "2 3"   prints "1 4"
    #
    # Both lists can be given in either the compact or the expanded form.
    local ids1="$1" ids2="$2"

    ids1=$(expand-cpulist "$ids1")
    ids2=$(expand-cpulist "$ids2")

    python3 -c 'import sys
allc = set(int(x) for x in sys.argv[1].split())
iso = set(int(x) for x in sys.argv[2].split())
print(" ".join(str(x) for x in sorted(allc - iso)))
' "$ids1" "$ids2"
}

container-cpus() { # script API
    # Usage: container-cpus POD CONTAINER
    #
    # Print the cpuset CONTAINER of POD is currently allowed to run on, as
    # read from inside the container. The list is in the compact form, for
    # instance "4-5,12".
    #
    # This is the live cpuset. Use allowed-cpu-ids instead to read the cpuset
    # from the latest snapshot which "report allowed" took.
    local pod=$1 ctr=$2 status

    status=$(vm-command-q \
        "kubectl exec $pod -c $ctr -- grep Cpus_allowed_list /proc/1/status") ||
        error "failed to read the cpuset of container $ctr of pod $pod"

    tr -d '\t ' <<< "$status" | cut -d ':' -f 2
}

allowed-cpu-ids() { # script API
    # Usage: allowed-cpu-ids CONTAINER
    #
    # Print the CPU ids CONTAINER is allowed to run on, as a sorted list of
    # space separated ids, for instance "4 5 12".
    #
    # This reads the latest snapshot which "report allowed" took, so it needs
    # a preceding report or verify. Use container-cpus instead to read the
    # live cpuset from inside the container.
    pyexec "print(' '.join(str(i) for i in sorted(cpu_ids(cpus['$1']))))"
}

###
### Scheduling
###

# Scheduling policies, as reported in /proc/PID/sched.
SCHED_OTHER=0
SCHED_FIFO=1
SCHED_RR=2
SCHED_BATCH=3
SCHED_ISO=4
SCHED_IDLE=5
SCHED_DEADLINE=6

verify-sched() { # script API
    # Usage: expected_policy=POLICY expected_prio=PRIO verify-sched CONTAINER
    #
    # Verify the scheduling policy and priority of the process of CONTAINER.
    # POLICY is one of the SCHED_* constants above. Fail the test unless both
    # expected values are given, so that a typo in a variable name cannot turn
    # the verification into a no-op.
    local podXcY=$1

    vm-command "cat /proc/\$(pgrep -f 'echo $podXcY')/sched | grep -E '^((policy)|(prio))'" ||
        command-error "cannot get /proc/PID/sched for $podXcY"

    if [ "$expected_policy" != "" ]; then
        echo "verify scheduling policy of $podXcY is $expected_policy"
        grep -q -E "policy .* $expected_policy" <<< $COMMAND_OUTPUT ||
            error "expected policy $expected_policy not found"
    else
        error "missing verify-sched expected_policy for $podXcY"
    fi

    if [ "$expected_prio" != "" ]; then
        echo "verify scheduling priority of $podXcY is $expected_prio"
        grep -q -E "prio .* $expected_prio" <<< $COMMAND_OUTPUT ||
            error "expected priority $expected_prio not found"
    else
        error "missing verify-sched expected_prio for $podXcY"
    fi
}

###
### Interrupts
###

resolve-irq() { # script API
    # Usage: resolve-irq IRQ
    #
    # Print the number of interrupt IRQ, which is either an interrupt number
    # or an extended regular expression matching a line in /proc/interrupts,
    # for instance ".*ttyS0.*". Fail the test if there is no such interrupt.
    local irq_or_pattern="$1" irq="" interrupts

    interrupts=$(vm-command-q "cat /proc/interrupts" | tr -s ' \t' ' ')

    # Try an interrupt number first, then a pattern.
    irq=$(grep "^ *$irq_or_pattern:" <<< "$interrupts" | cut -d ':' -f1 | tr -d ' ' | head -n 1)
    if [ -z "$irq" ]; then
        irq=$(grep -E "$irq_or_pattern" <<< "$interrupts" | cut -d ':' -f1 | tr -d ' ' | head -n 1)
    fi
    # Reject a match which is not an interrupt number. Without this, a pattern
    # which happens to match the header line, or one of the non-numbered
    # counters at the end of the file, would resolve to garbage.
    if ! [[ "$irq" =~ ^[0-9]+$ ]]; then
        error "no interrupt matching \"$irq_or_pattern\" found in /proc/interrupts"
    fi
    if [ "$irq" != "$irq_or_pattern" ]; then
        echo "interrupt \"$irq_or_pattern\" resolved to irq $irq" >&2
    fi
    echo "$irq"
}

irq-cpu-ids() { # script API
    # Usage: irq-cpu-ids IRQ
    #
    # Print the CPU ids in the affinity of interrupt IRQ, as a sorted list of
    # space separated ids. IRQ is resolved with resolve-irq.
    local irq
    irq=$(resolve-irq "$1") || return 1
    expand-cpulist "$(vm-command-q "cat /proc/irq/$irq/smp_affinity_list" | tr -d '[:space:]')"
}

verify-irq-cpus() { # script API
    # Usage: verify-irq-cpus IRQ EXPECTED [TIMEOUT]
    #
    # Wait until the affinity of interrupt IRQ becomes EXPECTED, TIMEOUT
    # seconds at most, 20 by default. Fail the test otherwise.
    #
    # Both the expected and the observed CPU list are normalized, so EXPECTED
    # can be given in either the compact or the expanded form.
    local irq expected got tmo=${3:-20}

    irq=$(resolve-irq "$1")
    expected=$(expand-cpulist "$2")

    retry-until --timeout "$tmo" 'got=$(irq-cpu-ids "$irq"); [ "$got" == "$expected" ]' || {
        error "irq $irq affinity: expected CPUs '$expected', got '$got'"
    }
    echo "irq $irq affinity is '$got' as expected"
}

###
### Extended resources of the node
###

get-node-resource() { # script API
    # Usage: get-node-resource [--allocatable] NAME
    #
    # Print the capacity, or with --allocatable the allocatable amount, of
    # extended resource NAME on the test node, and store it in COMMAND_OUTPUT.
    # Print "missing" if the node does not have the resource at all.
    local field=capacity
    while [ "${1#--}" != "$1" ]; do
        case "$1" in
            --capacity)    field=capacity; shift;;
            --allocatable) field=allocatable; shift;;
            *)             error "get-node-resource: unknown option \"$1\"";;
        esac
    done
    vm-command "kubectl get nodes -o json | jq -r '.items[] | (.status.$field[\"$1\"] // \"missing\")'"
}

wait-node-resource() { # script API
    # Usage: wait-node-resource [--allocatable] [--timeout SECS] [--interval SECS] NAME VALUE [MESSAGE]
    #
    # Wait until extended resource NAME on the test node equals VALUE, which
    # can also be the string "missing". Give up after SECS seconds, 30 by
    # default, checking every SECS seconds, 2 by default. Fail the test with
    # MESSAGE on timeout.
    local fieldopt="" tmo=30 ival=2
    while [ "${1#--}" != "$1" ]; do
        case "$1" in
            --timeout)  tmo="$2"; shift 2;;
            --interval) ival="$2"; shift 2;;
            *)          fieldopt="$1"; shift;;
        esac
    done
    local name=$1 value=$2 msg=${3:-"unexpected amount of $1 on the node"}
    retry-until --timeout "$tmo" --interval "$ival" \
        'get-node-resource $fieldopt "$name" && [ "$COMMAND_OUTPUT" == "$value" ]' || {
        command-error "$msg (expected '$value', got '$COMMAND_OUTPUT')"
    }
}

###
### Reading the log of the plugin
###

plugin-daemonset() { # script API
    # Usage: plugin-daemonset [PLUGIN]
    #
    # Print the name of the DaemonSet of PLUGIN, $POLICY by default.
    #
    # This mirrors the daemonset_name defaults of helm-launch.
    local plugin=${1:-$POLICY}
    case "$plugin" in
        *topology*aware*) echo nri-resource-policy-topology-aware;;
        *balloons*)       echo nri-resource-policy-balloons;;
        *memory-policy*)  echo nri-memory-policy;;
        *memtierd*)       echo nri-memtierd;;
        *)                error "plugin-daemonset: unknown plugin \"$plugin\"";;
    esac
}

plugin-log() { # script API
    # Usage: plugin-log [--plugin PLUGIN] [--tail LINES] [--ignore-case] [PATTERN]
    #
    # Print the log of the DaemonSet of PLUGIN, $POLICY by default, and store
    # it in COMMAND_OUTPUT. If PATTERN is given, print only the lines matching
    # it as an extended regular expression, case-insensitively if
    # --ignore-case is given. If LINES is given, print only the last LINES of
    # the matching lines.
    #
    # Return non-zero if PATTERN did not match anything, so that the caller
    # can report a missing log line:
    #     plugin-log 'associated cpus .* to CLOS 0' || command-error "no CLOS 0"
    #
    # Retry while the log of the plugin is not available. That happens for
    # instance right after the plugin has restarted.

    # What kubectl says while the log of a container is not readable yet.
    local unavailable="unable to retrieve container logs for"
    local plugin=$POLICY lines="" pattern="" grepopts="-E" cmd
    while [ "${1#--}" != "$1" ]; do
        case "$1" in
            --plugin)      plugin="$2"; shift 2;;
            --tail)        lines="$2"; shift 2;;
            --ignore-case) grepopts="$grepopts -i"; shift;;
            --)            shift; break;;
            *)             error "plugin-log: unknown option \"$1\"";;
        esac
    done
    pattern="$1"

    cmd="kubectl -n kube-system logs ds/$(plugin-daemonset "$plugin") 2>&1"
    if [ -n "$pattern" ]; then
        # Keep the transient availability error visible to the retry below.
        # Filtering it out would leave nothing for the retry to notice, and it
        # would give up after a single attempt.
        cmd="$cmd | grep $grepopts -e '$pattern' -e '$unavailable'"
    fi
    if [ -n "$lines" ]; then
        cmd="$cmd | tail -n $lines"
    fi

    retry-until --timeout 15 --interval 3 \
        'vm-command "$cmd"; ! grep -q "$unavailable" <<< "$COMMAND_OUTPUT"' || :

    # The log never became readable. Say so rather than reporting the status of
    # matching the pattern: with the pattern above that status is the status of
    # matching the availability error itself, that is, success.
    if grep -q "$unavailable" <<< "$COMMAND_OUTPUT"; then
        return 1
    fi

    # Report whether PATTERN matched, not whether the log became available.
    return "$COMMAND_STATUS"
}

plugin-log-tail() { # script API
    # Usage: plugin-log-tail [LINES]
    #
    # Print the last LINES lines of the plugin log which match the extended
    # regular expression in $plugin_log_filter, all lines if the variable is
    # unset. LINES defaults to $plugin_log_tail_lines, or 500.
    #
    # This is what the log assertions below look at. Set plugin_log_filter in
    # a test to restrict them to the log of the subsystem under test:
    #     plugin_log_filter='pct(:| mock:)'
    plugin-log --tail "${1:-${plugin_log_tail_lines:-500}}" "${plugin_log_filter:-}" || :
}

assert-log-contains() { # script API
    # Usage: assert-log-contains REGEXP [MESSAGE]
    #
    # Fail the test unless REGEXP, an extended regular expression, matches the
    # plugin log. See plugin-log-tail for which part of the log is inspected.
    local pattern=$1 msg=${2:-"expected log line missing"}
    plugin-log-tail
    grep -E -q "$pattern" <<< "$COMMAND_OUTPUT" || command-error "$msg (pattern: $pattern)"
}

assert-log-not-contains() { # script API
    # Usage: assert-log-not-contains REGEXP [MESSAGE]
    #
    # Fail the test if REGEXP matches the plugin log.
    local pattern=$1 msg=${2:-"unexpected log line"}
    plugin-log-tail
    if grep -E -q "$pattern" <<< "$COMMAND_OUTPUT"; then
        command-error "$msg (unexpected pattern: $pattern)"
    fi
}

wait-assert-log-contains() { # script API
    # Usage: wait-assert-log-contains REGEXP [MESSAGE] [TIMEOUT]
    #
    # Wait until REGEXP matches the plugin log, TIMEOUT seconds at most, 5 by
    # default. Fail the test on timeout, reporting the log which was inspected.
    local pattern=$1 msg=${2:-"expected log line missing"} tmo=${3:-5}
    retry-until --timeout "$tmo" \
        'plugin-log-tail; grep -E -q "$pattern" <<< "$COMMAND_OUTPUT"' ||
        assert-log-contains "$pattern" "$msg"
}

wait-assert-log-grew() { # script API
    # Usage: wait-assert-log-grew REGEXP COUNT [MESSAGE] [TIMEOUT]
    #
    # Wait until the plugin log has more than COUNT lines matching REGEXP,
    # TIMEOUT seconds at most, 5 by default. Fail the test on timeout.
    #
    # Use this instead of wait-assert-log-contains when REGEXP already matched
    # something in an earlier phase of the test, and the point is that a new
    # line shows up.
    local pattern=$1 count=$2 msg=${3:-"expected new log lines"} tmo=${4:-5}
    retry-until --timeout "$tmo" \
        'plugin-log-tail; [ "$(grep -c -E "$pattern" <<< "$COMMAND_OUTPUT")" -gt "$count" ]' ||
        command-error "$msg (pattern: $pattern, expected more than $count lines)"
}

assert-cpu-clos() { # script API
    # Usage: assert-cpu-clos CPUS CLOS [MESSAGE] [TIMEOUT]
    #
    # Wait until the plugin has associated CPUS to CLOS, for instance
    # "CLOS 0". CPUS is an extended regular expression matching a CPU list, so
    # ".*" matches any CPUs.
    local cpus=$1 clos=$2 msg=${3:-"CPUs '$1' not associated to $2"} tmo=${4:-5}
    wait-assert-log-contains "associated cpus $cpus to $clos" "$msg" "$tmo"
}

assert-cpu-freq() { # script API
    # Usage: assert-cpu-freq CPULIST CLASS [MESSAGE] [TIMEOUT]
    #
    # Wait until the plugin has enforced the CPU frequencies of CPU class
    # CLASS on every CPU in CPULIST.
    local cpus=$1 class=$2 msg=$3 tmo=${4:-5} cpu
    for cpu in $(expand-cpulist "$cpus"); do
        wait-assert-log-contains "enforcing cpu frequency from class .$class@.* on cpu $cpu\$" \
            "${msg:-CPU frequency class $class not enforced on cpu $cpu}" "$tmo"
    done
}

###
### Cleaning up
###

delete-pods() { # script API
    # Usage: delete-pods [-n NAMESPACE] {--all | POD...}
    #
    # Delete pods immediately, ignoring pods which do not exist. Delete the
    # pods in NAMESPACE, or in the default namespace if -n is not given.
    #
    # Never fails, so this is safe to call both before and after a test.
    local ns=""
    if [ "$1" == "-n" ]; then
        ns="-n $2"
        shift 2
    fi
    if [ $# -eq 0 ]; then
        error "delete-pods: expected --all or a list of pods"
    fi
    vm-command "kubectl delete pods $ns $* --now --ignore-not-found=true" || :
}

create-namespaces() { # script API
    # Usage: create-namespaces NAMESPACE...
    #
    # Create namespaces unless they already exist. Never fails.
    local ns
    for ns in "$@"; do
        vm-command "kubectl get namespace $ns > /dev/null 2>&1 || kubectl create namespace $ns" || :
    done
}

delete-namespaces() { # script API
    # Usage: delete-namespaces NAMESPACE...
    #
    # Delete all pods in the namespaces, then the namespaces themselves.
    # Namespaces which do not exist are ignored. Never fails.
    local ns
    for ns in "$@"; do
        vm-command "kubectl delete pods -n $ns --all --now --ignore-not-found=true
                    kubectl delete namespace $ns --now --ignore-not-found=true" || :
    done
}

remove-policy-cache() { # script API
    # Usage: remove-policy-cache
    #
    # Remove the cache file of the resource policy from the node. Never fails.
    #
    # Use this to prevent the cache of a previously running policy from
    # affecting the policy which the test launches.
    vm-command "rm -f /var/lib/nri-resource-policy/cache" || :
}

kill-test-processes() { # script API
    # Usage: kill-test-processes
    #
    # Kill leftover processes of test containers in the VM. Never fails.
    #
    # The patterns match the commands which the pod templates in files/ run.
    # They use a bracket expression so that they do not match the command
    # line of the shell which runs pkill.
    vm-command 'pkill -9 -f "sleep[ ]inf"
                pkill -9 -f "echo[ ]pod"' || :
}
