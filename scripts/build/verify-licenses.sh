#!/bin/sh

SUMMARY="licenses-summary.txt"
ALLOWED=" \
    Apache-2.0 \
    BSD-2-Clause \
    BSD-3-Clause \
    ISC \
    MIT \
"
VERBOSE=""

while [ "$#" -gt 0 ]; do
    case "$1" in
        -v|--verbose)
            VERBOSE=1
            shift
            ;;
        -*)
            echo "Unknown option: $1"
            exit 1
            ;;
        *)
            break
            ;;
    esac
done

LICENSE_PATH="${1:-./build/licenses}"

fail() {
    echo "FAIL: $@"
}

pass() {
    echo "PASS: $@"
}

verbose() {
    if [ -z "$VERBOSE" ]; then
        return 0
    fi
    $@
}

for summary in $(find $LICENSE_PATH -name $SUMMARY); do
    component="${summary%/*}"
    component="${component##*/}"
    cat $summary | (
        unexpected=""
        while read lic; do
        ok=""
        for exp in $ALLOWED; do
            if [ "$lic" = "$exp" ]; then
                ok=1
                break
            fi
        done
        if [ -z "$ok" ]; then
            unexpected="$unexpected${unexpected:+ }$lic"
            verbose fail "$component: unexpected license $lic"
        else
            verbose pass "$component: allowed license $lic"
        fi
    done
    if [ -n "$unexpected" ]; then
        fail "$component: unexpected license(s) $unexpected"
        exit 1
    else
        pass "$component: no unexpected licenses"
    fi)

    if [ $? != 0 ]; then
        FAIL=1
    fi
done

if [ -n "$FAIL" ]; then
    exit 1
fi

exit 0
