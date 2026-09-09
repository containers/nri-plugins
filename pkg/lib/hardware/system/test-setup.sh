#!/bin/bash

# Unpack the recorded sysfs trees the equivalence tests compare over. They are
# the trees pkg/sysfs, pkg/cpuallocator and the topology-aware policy already
# keep for their own tests; this only unpacks them somewhere shared, rather than
# adding another copy of the same machines to the repository.

set -e

cd "$(dirname "$0")"

REPO=../../../..
OUT=testdata

mkdir -p $OUT

# pkg/sysfs keeps two trees, one machine each, as .tar.xz holding <name>/sys.
for tree in sample1 sample2; do
    [ -d "$OUT/$tree" ] && continue
    tar -C $OUT -xJf $REPO/pkg/sysfs/test-data-$tree.tar.xz
done

# The other two archives are .tar.bz2 holding sysfs/<name>/sys/... for several
# machines each. Lift each machine up to $OUT/<name>, so that every tree looks
# the same to the tests.
unpack_bz2() {
    local archive="$1" tmp

    tmp=$(mktemp -d)
    # shellcheck disable=SC2064
    trap "rm -rf '$tmp'" RETURN

    tar -C "$tmp" -xjf "$archive"
    for dir in "$tmp"/sysfs/*/; do
        local name
        name=$(basename "$dir")
        [ -d "$OUT/$name" ] && continue
        mkdir -p "$OUT/$name"
        cp -a "$dir/sys" "$OUT/$name/sys"
    done
}

unpack_bz2 $REPO/pkg/cpuallocator/testdata/sysfs.tar.bz2
unpack_bz2 $REPO/cmd/plugins/topology-aware/policy/testdata/sysfs.tar.bz2
