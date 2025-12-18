#!/bin/bash

TESTS_DIR="$1"
SKIP_LONG_TESTS="${skip_long_tests:-yes}"
RUN_SH="${0%/*}/run.sh"

DEFAULT_DISTRO=${DEFAULT_DISTRO:-"fedora/42"}

allow_var_override=""

k8scri=${k8scri:="containerd"}
efi=${efi:-}

proxy=${proxy:=$https_proxy}
proxy=${proxy:=$HTTPS_PROXY}
export proxy

usage() {
    echo "Usage: [skip_long_tests=no] run_tests.sh TESTS_DIR"
    echo "TESTS_DIR is expected to be structured as POLICY/TOPOLOGY/TEST with files:"
    echo "POLICY/nri-resource_policy.cfg: configuration of nri-resource_policy"
    echo "POLICY/TOPOLOGY/topology.var.json: contents of the topology variable for run.sh"
    echo "POLICY/TOPOLOGY/TEST/code.var.sh: contents of the code var (that is, test script)"
    echo "skip_long_tests=no enables long-running tests (including fuzzing and stress tests)."
}

error() {
    (echo ""; echo "error: $1" ) >&2
    exit 1
}

warning() {
    echo "WARNING: $1" >&2
}

export-var-files() {
    # export ENV_VAR from ENV_VAR.var.* file content
    local var_file_dir="$1"
    local var_filepath
    local var_file_name
    local var_name
    for var_filepath in "$var_file_dir"/*.var "$var_file_dir"/*.var.*; do
        if ! [ -f "$var_filepath" ] || [[ "$var_filepath" == *"~" ]] || [[ "$var_filepath" == *"#"* ]]; then
            continue
        fi
        var_file_name=$(basename "$var_filepath")
        var_name=${var_file_name%%.var*}
        if [ "$var_name" == "code" ] || [ "$var_name" == "py_consts" ]; then
            # append values in code variables
            echo "exporting $var_name - appending from $var_filepath"
            export "$var_name"="${!var_name}""
$(< "$var_filepath")"
        else
            # creating / replace other variables
            if [ -z "${!var_name}" ]; then
                echo "exporting $var_name - creating from $var_filepath"
                allow_var_override+=" $var_name "
            elif grep -q " $var_name " <<< "$allow_var_override"; then
                echo "exporting $var_name - overriding from $var_filepath"
            else
                echo "not overriding $var_name - variable is not created from *.var.* files"
                continue
            fi
            if [[ "$var_file_name" == *.var.in.* ]]; then
                export "$var_name"="$(eval "echo -e \"$(<"${var_filepath}")\"")"
            else
                export "$var_name"="$(< "$var_filepath")"
            fi
        fi
    done
}

export-vm-files() {
    # update and export vm_files associative array from directory content
    local vm_files_dir="$1"
    if [ ! -d "$vm_files_dir" ]; then
        return
    fi
    if [[ "$vm_files" == *"="* ]] ; then
        eval "declare -A vm_files_aa=${vm_files#*=}"
    else
        declare -A vm_files_aa
    fi
    prefix_len=${#vm_files_dir}
    shopt -s globstar
    for f in "$vm_files_dir"/**; do
        file_vm_name=${f:$prefix_len}
        if [ -z "$file_vm_name" ] || [ "$file_vm_name" == "/" ]; then
            continue
        elif [ -f "$f" ]; then
            if [ -n "${vm_files_aa[$file_vm_name]}" ]; then
                warning "vm file $file_vm_name: new file \"$f\" overrides \"${vm_files_aa[$file_vm_name]}\""
            fi
            vm_files_aa[$file_vm_name]="file:$(realpath "$f")"
        fi
    done
    # serialize from associative array
    local serialized_vm_files
    serialized_vm_files="$(declare -p vm_files_aa)"
    export vm_files="declare -A vm_files${serialized_vm_files#declare -A vm_files_aa}"
}

source-source-files() {
    # Test execution will source *.source.* files before it executes
    # the real test code. The files will be sourced starting from the
    # test suite (root) directory and ending up to the test directory,
    # which enables overriding inherited functions and variables.
    local src_file_dir="$1"
    local src_filepath
    for src_filepath in "$src_file_dir"/*.source "$src_file_dir"/*.source.*; do
        if ! [ -f "$src_filepath" ] || [[ "$src_filepath" == *"~" ]]; then
            continue
        fi
        echo "sourcing $src_filepath before running test code"
        source_libs="${source_libs}""
source \"$src_filepath\"
"
    done
}

export-and-source-dir() {
    local dir="$1"
    export-var-files "$dir"
    export-vm-files "$dir/vm-files"
    source-source-files "$dir"
}

case "$TESTS_DIR" in
    "")
        usage
        error "missing TESTS_DIR"
        ;;
    "help"|"--help"|"-h")
        usage
        exit 0
        ;;
esac

if ! [ -d "$TESTS_DIR" ]; then
    error "bad TESTS_DIR: \"$TESTS_DIR\""
fi

# Find TESTS_DIR root by looking for POLICY_DIR/*.cfg. If TESTS_DIR was not the
# root dir, then execute tests only under TESTS_DIR.
root_dir_glob="*.test-suite"
# shellcheck disable=SC2053
if [[ "$(basename "$TESTS_DIR")" == $root_dir_glob ]]; then
    TESTS_ROOT_DIR="$TESTS_DIR"
elif [[ "$(basename "$(realpath "$TESTS_DIR"/..)")" == $root_dir_glob ]]; then
    TESTS_ROOT_DIR=$(realpath "$TESTS_DIR/..")
    TESTS_POLICY_FILTER=$(basename "${TESTS_DIR}")
elif [[ "$(basename "$(realpath "$TESTS_DIR"/../..)")" == $root_dir_glob ]]; then
    TESTS_ROOT_DIR=$(realpath "$TESTS_DIR/../..")
    TESTS_POLICY_FILTER=$(basename "$(dirname "${TESTS_DIR}")")
    TESTS_TOPOLOGY_FILTER=$(basename "${TESTS_DIR}")
elif [[ "$(basename "$(realpath "$TESTS_DIR"/../../..)")" == $root_dir_glob ]]; then
    TESTS_ROOT_DIR=$(realpath "$TESTS_DIR/../../..")
    TESTS_POLICY_FILTER=$(basename "$(dirname "$(dirname "${TESTS_DIR}")")")
    TESTS_TOPOLOGY_FILTER=$(basename "$(dirname "${TESTS_DIR}")")
    TESTS_TEST_FILTER=$(basename "${TESTS_DIR}")
else
    error "TESTS_DIR=\"$TESTS_DIR\" is invalid tests/policy/topology/test dir: *.cfg not found"
fi

echo "Running tests matching:"
echo "    TESTS_ROOT_DIR=$TESTS_ROOT_DIR"
echo "    TESTS_POLICY_FILTER=$TESTS_POLICY_FILTER"
echo "    TESTS_TOPOLOGY_FILTER=$TESTS_TOPOLOGY_FILTER"
echo "    TESTS_TEST_FILTER=$TESTS_TEST_FILTER"
echo "    skip long tests: $SKIP_LONG_TESTS"

source "$TESTS_ROOT_DIR"/../lib/vm.bash

cleanup() {
    rm -rf "$summary_dir"
}
summary_dir=$(mktemp -d)
trap cleanup TERM EXIT QUIT

summary_file="$summary_dir/summary.txt"
echo -n "" > "$summary_file"

export-and-source-dir "$TESTS_ROOT_DIR"

TEST_SUITE_NAME="$(basename $TESTS_ROOT_DIR)"
TEST_PARAMS="$TESTS_ROOT_DIR/.run.sh-parameters"

for POLICY_DIR in "$TESTS_ROOT_DIR"/*; do
    if ! [ -d "$POLICY_DIR" ]; then
        continue
    fi
    if ! [[ "$(basename "$POLICY_DIR")" =~ .*"$TESTS_POLICY_FILTER".* ]]; then
        continue
    fi
    # Run exports in subshells so that variables exported for previous
    # tests do not affect any other tests.
    (
        for CFG_FILE in "$POLICY_DIR"/*.cfg; do
            if ! [ -f "$CFG_FILE" ]; then
                continue
            fi
            export nri_resource_policy_cfg=$CFG_FILE
        done
        export-and-source-dir "$POLICY_DIR"
        for TOPOLOGY_DIR in "$POLICY_DIR"/*; do
            if ! [ -d "$TOPOLOGY_DIR" ]; then
                continue
            fi
            if ! [[ "$(basename "$TOPOLOGY_DIR")" =~ .*"$TESTS_TOPOLOGY_FILTER".* ]]; then
                continue
            fi
            if [ "$(basename "$TOPOLOGY_DIR")" == "vm-files" ]; then
                continue
            fi
            (
                distro=${distro:=$DEFAULT_DISTRO}
                export distro

                vagrant_debug=${vagrant_debug:-}
                export vagrant_debug

		policy_name="$(basename $POLICY_DIR)"

		# Create name for the vm.
		export vm_name=$(vm-create-name "$k8scri" "$(basename "$TOPOLOGY_DIR")" ${distro})
                export-and-source-dir "$TOPOLOGY_DIR"

		# Create ansible inventory file from a template
		ESCAPED_VM=$(printf '%s\n' "$vm_name" | sed -e 's/[\/]/-/g')

		OUTPUT_DIR=$(realpath ${2:-"`pwd`/$ESCAPED_VM"})

                for TEST_DIR in "$TOPOLOGY_DIR"/test*; do
                    if ! [ -d "$TEST_DIR" ]; then
                        continue
                    fi
                    if ! [[ "$(basename "$TEST_DIR")" =~ .*"$TESTS_TEST_FILTER".* ]]; then
                        continue
                    fi
                    if [ "$(basename "$TEST_DIR")" == "vm-files" ]; then
                        continue
                    fi

                    if [ "$SKIP_LONG_TESTS" = "yes" ]; then
                        case $TEST_DIR in
                            *fuzz*)
                                echo "SKIP long test $TEST_DIR (skip_long_tests=$SKIP_LONG_TESTS)"
                                continue
                                ;;
                            *long*)
                                echo "SKIP long test $TEST_DIR (skip_long_tests=$SKIP_LONG_TESTS)"
                                continue
                                ;;
                            *stress*)
                                echo "SKIP long test $TEST_DIR (skip_long_tests=$SKIP_LONG_TESTS)"
                                continue
                                ;;
                        esac
                    fi

                    (
                        export-and-source-dir "$TEST_DIR"
                        export code="${source_libs}""
${code}"
			TEST_NAME=$(basename "$TEST_DIR")
                        export outdir="$OUTPUT_DIR"

			test_outdir="$outdir/$TEST_SUITE_NAME/$policy_name/$(basename "$TEST_DIR")"

                        mkdir -p "$test_outdir"
                        echo "Run $TEST_NAME"

			test_start_time=$(epochrealtime)

                        policy="$policy_name" test_outdir="$test_outdir" TEST_DIR=$TEST_DIR TOPOLOGY_DIR=$TOPOLOGY_DIR POLICY_DIR=$POLICY_DIR \
                            "$RUN_SH" test 2>&1 | tee "$test_outdir/run.sh.output"

			test_end_time=$(epochrealtime)
			test_time=$(echo "$test_end_time - $test_start_time" | bc)

			printf "\nTest duration: $test_time sec\n\n" >> "$test_outdir/run.sh.output"

                        test_name="$policy_name/$(basename "$TOPOLOGY_DIR")/$(basename "$TEST_DIR")"
                        if grep -q "Test verdict: PASS" "$test_outdir/run.sh.output"; then
                            echo "PASS $test_name" >> "$summary_file"
                        elif grep -q "Test verdict: FAIL" "$test_outdir/run.sh.output"; then
                            echo "FAIL $test_name" >> "$summary_file"
                        else
                            echo "ERROR $test_name" >> "$summary_file"
                        fi
                    )
                done
            )
        done
    )
done

echo ""
echo "Tests summary:"
cat "$summary_file"
if grep -q ERROR "$summary_file" || grep -q FAIL "$summary_file"; then
    exit 1
fi
