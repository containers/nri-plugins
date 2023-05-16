#!/bin/bash

# This is executed by the Github Workflow Actions and it will run e2e tests
# with suitable parameters.

if [ -z "$1" ]; then
    echo "Usage: $0 <test-directory-to-use>"
    exit 1
fi

if [ -z "$GITHUB_WORKSPACE" ]; then
    echo "This script can only work when run from Github Actions."
    exit 2
fi

cd $GITHUB_WORKSPACE/test/e2e

# Set any site specific environment variables to the env file so that they
# will be available to the run_tests.sh script. The self hosted runner
# script (runner.sh) will write this file with needed information.
if [ -f /mnt/env ]; then
    . /mnt/env
fi

# Make sure ts does not print error
export LC_ALL=C

echo "Test started" | ts '[%Y-%m-%d %H:%M:%S %z]'

# Run the actual tests inside another VM.
./run_tests.sh $1 "$GITHUB_WORKSPACE/e2e-test-results" | ts -s '(%H:%M:%.S)'
RET=$?

echo "Test stopped" | ts '[%Y-%m-%d %H:%M:%S %z]'

exit $RET
