#!/bin/bash
# Usage: deploy.sh [--image TAR] [--prepare-node] local|local-shared
#
# Deploy nri-dra-driver-cxl with the Helm chart of this repository on the
# single-node cluster of TARGET (see lib.sh), or reconfigure it:
#
#   local    the local memdev $CXL_SERIAL is sliced (DeviceClass cxl-local,
#            modes numa and dax), the default configuration
#   local-shared  the memdev is in sharedMemdevs: one devdax device of the
#            whole memdev that claims of cxl-local-shared map
#
# --image TAR       copy the image tarball TAR to TARGET and import it into
#                   containerd (namespace k8s.io) first
# --prepare-node    prepare the node for the driver:
#                   - install /etc/udev/rules.d/40-redhat.rules without its
#                     memory hotplug section if the system rule onlines
#                     hot-added memory (RHEL, CentOS Stream, Fedora): the
#                     rule overrides auto_online_blocks=offline and races
#                     the driver when it creates regions
#                   - offline and destroy a region that the BIOS or an admin
#                     created on the memdev: the driver manages only memdevs
#                     without foreign regions. A reboot (BIOS) or "cxl
#                     create-region" brings such a region back.
#
# Environment (and the variables of lib.sh):
#   CONTAINER_FENCING  defaults.container.fencing of the driver
#                      configuration, allowed-mems or none; unset: not in
#                      the configuration, the driver's default
#                      allowed-mems applies
#
# Switching between local and local-shared needs an idle device: delete the
# pods and claims of the device first.
set -o pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

RELEASE=${RELEASE:-cxl}
CHART_DIR="$REPO_DIR/deployment/helm/dra-driver-cxl"
IMAGE_NAME=${IMAGE_NAME:-docker.io/library/nri-dra-driver-cxl}
IMAGE_TAG=${IMAGE_TAG:-}
tar="" prepare="" mode=""

while [ $# -gt 0 ]; do
    case "$1" in
        --image) tar="$2"; shift ;;
        --prepare-node) prepare=1 ;;
        local|local-shared) mode="$1" ;;
        *) echo "usage: $0 [--image TAR] [--prepare-node] local|local-shared" >&2; exit 2 ;;
    esac
    shift
done
[ -n "$mode" ] || { echo "usage: $0 [--image TAR] [--prepare-node] local|local-shared" >&2; exit 2; }

set -e
if [ -n "$tar" ]; then
    # tag the image with its id too: a rebuild with the same version tag
    # gets a new tag, and helm rolls the DaemonSet
    read -r ref id < <(tar -xOf "$tar" manifest.json | python3 -c 'import json,sys; m=json.load(sys.stdin)[0]; print(m["RepoTags"][0], m["Config"].split("/")[-1][:12])')
    IMAGE_TAG="${ref##*:}-$id"
    log "importing $tar ($ref) on $TARGET as $IMAGE_NAME:$IMAGE_TAG"
    scp -q "$tar" "$TARGET:/tmp/nri-dra-driver-cxl-image.tar"
    t "ctr -n k8s.io images import /tmp/nri-dra-driver-cxl-image.tar >/dev/null && rm -f /tmp/nri-dra-driver-cxl-image.tar && ctr -n k8s.io images tag --force docker.io/library/$ref $IMAGE_NAME:$IMAGE_TAG >/dev/null"
fi
if [ -z "$IMAGE_TAG" ]; then
    # the image of the running driver
    img=$(t "kubectl get ds -n kube-system nri-dra-driver-cxl -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null" || true)
    if [ -n "$img" ]; then
        IMAGE_NAME=${img%:*}
        IMAGE_TAG=${img##*:}
    fi
    [ -n "$IMAGE_TAG" ] || { echo "the driver is not deployed on $TARGET, use --image TAR" >&2; exit 1; }
fi

S=$(serial)
log "memdev $S, memory block size $(block_bytes) bytes, auto_online_blocks $(t cat /sys/devices/system/memory/auto_online_blocks)"
if [ "$(t cat /sys/devices/system/memory/auto_online_blocks)" != offline ]; then
    log "setting auto_online_blocks to offline"
    t "echo offline > /sys/devices/system/memory/auto_online_blocks"
fi

udev_onlines_memory() { # does a udev rule online hot-added memory on TARGET
    t 'for f in /usr/lib/udev/rules.d/*.rules /etc/udev/rules.d/*.rules; do [ -e "/etc/udev/rules.d/${f##*/}" ] && [ "${f%/*}" = /usr/lib/udev/rules.d ] && continue; grep -l "SUBSYSTEM!=\"memory\"" "$f" 2>/dev/null | xargs -r grep -l "ATTR{state}=\"\$env{.state}\"" ; done | grep .'
}
if rules=$(udev_onlines_memory); then
    if [ -z "$prepare" ]; then
        echo "WARNING: udev rules that online hot-added memory on $TARGET: $rules (use --prepare-node)" >&2
    else
        for r in $rules; do
            log "installing /etc/udev/rules.d/${r##*/} without the memory hotplug section of $r"
            t "sed '/^# Memory hotadd request/,/^LABEL=\"memory_hotplug_end\"/d' $r > /etc/udev/rules.d/${r##*/} && udevadm control --reload"
        done
        if rules=$(udev_onlines_memory); then
            echo "udev rules still online hot-added memory on $TARGET: $rules" >&2
            exit 1
        fi
    fi
fi

if [ -n "$prepare" ] && ! t "kubectl get ds -n kube-system nri-dra-driver-cxl" >/dev/null 2>&1; then
    regions=$(t "cxl list -R 2>/dev/null | python3 -c 'import json,sys; print(\" \".join(r[\"region\"] for r in json.load(sys.stdin)))'" || true)
    for r in $regions; do
        log "offlining and destroying foreign $r (memory of its dax devices goes offline)"
        t "for d in \$(daxctl list -r ${r#region} 2>/dev/null | python3 -c 'import json,sys; print(\" \".join(d[\"chardev\"] for d in json.load(sys.stdin) if d.get(\"mode\") == \"system-ram\"))'); do daxctl offline-memory \$d; done; cxl destroy-region -f $r"
    done
fi

log "copying the chart to $TARGET"
t "rm -rf /root/dra-driver-cxl-chart"
scp -q -r "$CHART_DIR" "$TARGET:/root/dra-driver-cxl-chart"

values="image:
  name: $IMAGE_NAME
  tag: $IMAGE_TAG
  pullPolicy: Never
logLevel: 4"
config=""
if [ "$mode" == local-shared ]; then
    config+="
  sharedMemdevs: [\"$S\"]"
fi
if [ -n "$CONTAINER_FENCING" ]; then
    config+="
  defaults: {container: {fencing: $CONTAINER_FENCING}}"
fi
if [ -n "$config" ]; then
    values+="
config:$config"
fi
echo "$values" | t "cat > /root/dra-driver-cxl-values.yaml"
log "helm values:"
echo "$values" | sed 's/^/    /'

log "installing/upgrading release $RELEASE ($mode)"
t "helm upgrade --install -n kube-system $RELEASE /root/dra-driver-cxl-chart -f /root/dra-driver-cxl-values.yaml --wait --timeout 180s" | grep -E "STATUS|REVISION" || true
t "kubectl rollout status -n kube-system ds/nri-dra-driver-cxl --timeout=180s"

want=False
[ "$mode" == local-shared ] && want=True
published() {
    local d
    d=$(slice_json) && [ "$(jget "$d" 'j["attributes"]["shared"]["bool"]')" == "$want" ]
}
wait_for 120 "$(device_name) published with shared=$want" published ||
    { echo "the driver does not publish $(device_name) as $mode" >&2; driver_log | tail -n 30; exit 1; }
log "published device:"
slice_json | python3 -m json.tool | sed 's/^/    /'
