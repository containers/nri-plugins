#!/bin/bash

usage() {
    cat <<EOF
generate.sh - generate fuzz tests.

Configuring test generation with environment variables:
  TESTCOUNT=<NUM>       Number of generated test scripts than run in parallel.
  MEM=<NUM>             Memory [MB] available for test pods in the system.
  CPU=<NUM>             Non-reserved CPU [mCPU] available for test pods in the system.
  RESERVED_CPU=<NUM>    Reserved CPU [mCPU] available for test pods in the system.
  STEPS=<NUM>           Total number of test steps in all parallel tests.
  SEARCH_DEPTH=<NUM>    Test generator search depth for best paths.

EOF
    exit 0
}

if [ -n "$1" ]; then
    usage
fi

TESTCOUNT=${TESTCOUNT:-1}
MEM=${MEM:-7500}
# 950 mCPU taken by the control plane, split the remaining 15050 mCPU
# available for test pods to CPU and RESERVED_CPU pods.
CPU=${CPU:-14050}
RESERVED_CPU=${RESERVED_CPU:-1000}
STEPS=${STEPS:-100}
SEARCH_DEPTH=${SEARCH_DEPTH:-4}

mem_per_test=$(( MEM / TESTCOUNT ))
cpu_per_test=$(( CPU / TESTCOUNT ))
reserved_cpu_per_test=$(( RESERVED_CPU / TESTCOUNT ))
steps_per_test=$(( STEPS / TESTCOUNT ))

cd "$(dirname "$0")" || {
    echo "cannot cd to the directory of $0"
    exit 1
}


for testnum in $(seq 1 "$TESTCOUNT"); do
    testid=$(( testnum - 1))
    OUTFILE=generated${testid}.sh
    prefix_pods_with_testid="s/\([^a-z0-9]\)\(r\?\)\(gu\|bu\|be\)\([0-9]\)/\1t${testid}\2\3\4/g"
    echo "generating $OUTFILE..."
    go run ./generate.go \
       --mem $mem_per_test \
       --cpu $cpu_per_test \
       --reserved-cpu $reserved_cpu_per_test \
       --test-steps $steps_per_test \
       --random-seed $testid \
       --randomness 2 \
       --search-depth $SEARCH_DEPTH \
        | sed -e "$prefix_pods_with_testid" \
              > "$OUTFILE"
done
