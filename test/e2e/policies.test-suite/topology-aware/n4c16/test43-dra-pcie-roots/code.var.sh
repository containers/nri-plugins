# This test verifies that DRA devices list the PCIe roots of their socket,
# or none if the cluster does not support list attributes.
#

cleanup() {
    helm-terminate || :
}

cleanup

# A dry run tells whether the API server accepts list attributes.
list_attributes_supported=true
if ! vm-command "kubectl create --dry-run=server -f - <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
metadata:
  name: list-attribute-probe
spec:
  driver: probe.nri.io
  nodeName: \$(hostname)
  pool:
    name: probe
    generation: 0
    resourceSliceCount: 1
  devices:
  - name: probe
    attributes:
      probe:
        strings: [a, b]
EOF"; then
    list_attributes_supported=false
fi

# The VM's roots report all CPUs, so every device lists all roots.
roots=$(vm-command-q "ls -d /sys/devices/pci[0-9a-f][0-9a-f][0-9a-f][0-9a-f]:[0-9a-f][0-9a-f] |
                      xargs -n1 basename | paste -sd, -")
[ -n "$roots" ] || error "the VM has no PCIe roots"
online=$(vm-command-q "cat /sys/devices/system/cpu/online")
for root in ${roots//,/ }; do
    affinity=$(vm-command-q "cat /sys/devices/$root/pci_bus/${root#pci}/cpulistaffinity")
    [ "$affinity" == "$online" ] ||
        error "PCIe root $root reports CPUs $affinity, not all CPUs $online: update the test"
done

helm_config=$(DRA_ENABLED=true instantiate helm-config.yaml) \
    helm-launch topology-aware

retry-until --timeout 30 --message "the plugin to publish its devices" \
    '[ -n "$(dra-devices | jq -r ".[0].name // empty")" ]' ||
    error "the plugin published no devices"

vm-command "kubectl get resourceslices -o yaml"

expected=$roots
if [ "$list_attributes_supported" == false ]; then
    expected=""
fi

devices=$(dra-devices)
names=$(jq -r '.[].name' <<< "$devices")
[ -n "$names" ] || error "the plugin published no devices"
for name in $names; do
    got=$(jq -r --arg name "$name" '.[] | select(.name == $name) |
              .attributes["resource.kubernetes.io/pcieRoot"].strings // [] |
              join(",")' <<< "$devices")
    [ "$got" == "$expected" ] ||
        error "device $name publishes PCIe roots \"$got\", expected \"$expected\""
done

cleanup

if [ "$list_attributes_supported" == true ]; then
    echo "OK: every DRA device lists the PCIe roots of its socket: $roots"
else
    echo "OK: without list attribute support the devices are published without PCIe roots"
fi
