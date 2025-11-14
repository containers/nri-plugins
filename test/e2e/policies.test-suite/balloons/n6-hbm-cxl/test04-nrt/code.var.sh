cleanup() {
    vm-command "kubectl delete pods --all --now"
    helm-terminate
}

cleanup
helm_config=${TEST_DIR}/balloons-nrt.cfg helm-launch balloons

# Print full NRT yaml for debugging
vm-command "$nrt_kubectl_get -o yaml"

# Verify zones when fullsocket balloons do not include containers.
nrt-verify-zone-attribute "fullsocket[0]" "cpuset" "[4567]"
nrt-verify-zone-attribute "fullsocket[0]" "shared cpuset" "5-7|4,6-7|4-5,7|4-6"
nrt-verify-zone-attribute "fullsocket[0]" "excess cpus" "1"
nrt-verify-zone-attribute "fullsocket[1]" "cpuset" "0|1"
nrt-verify-zone-attribute "fullsocket[1]" "shared cpuset" "1-2|0,2"
nrt-verify-zone-attribute "fullsocket[1]" "excess cpus" "1"
nrt-verify-zone-attribute "reserved[0]" "cpuset" "3"
nrt-verify-zone-attribute "reserved[0]" "shared cpuset" "^\$"

nrt-verify-zone-resource "reserved[0]" "cpu" "capacity" "8"
nrt-verify-zone-resource "reserved[0]" "cpu" "allocatable" "6"
nrt-verify-zone-resource "fullsocket[0]" "cpu" "allocatable" "4"
nrt-verify-zone-resource "fullsocket[0]" "cpu" "available" "4"
nrt-verify-zone-resource "fullsocket[1]" "cpu" "allocatable" "4"
nrt-verify-zone-resource "fullsocket[1]" "cpu" "available" "4"

# Create burstable containers without CPU limits
CPUREQ="750m" MEMREQ="100M" CPULIM="" MEMLIM="500M"
POD_ANNOTATION='cpu.preserve.resource-policy.nri.io/container.pod0c1: "true"
    memory.preserve.resource-policy.nri.io/container.pod0c2: "true"'
CONTCOUNT=3 create balloons-busybox

# Print full NRT yaml for debugging
vm-command "$nrt_kubectl_get -o yaml"

# Verify selected zone attributes
nrt-verify-zone-resource "default/pod0/pod0c0" "cpu" "capacity" "4" # balloon's + shared CPUs
nrt-verify-zone-resource "default/pod0/pod0c0" "cpu" "allocatable" "750m" # requested CPUs
nrt-verify-zone-resource "default/pod0/pod0c0" "cpu" "available" "0" # nothing available on the subzone
nrt-verify-zone-attribute "default/pod0/pod0c0" "cpuset" "4-7"
nrt-verify-zone-attribute "default/pod0/pod0c0" "memory set" "1"

nrt-verify-zone-resource "default/pod0/pod0c1" "cpu" "capacity" "" # preserve => container should not exist, not assigned into any balloon

nrt-verify-zone-resource "default/pod0/pod0c2" "cpu" "capacity" "4" # expect same balloon as pod0c0
nrt-verify-zone-resource "default/pod0/pod0c2" "cpu" "allocatable" "750m"
nrt-verify-zone-attribute "default/pod0/pod0c2" "memory set" "^\$" # preserve memory

# Create burstable containers with CPU limits
CPUREQ="200m" MEMREQ="100M" CPULIM="1500m" MEMLIM="500M"
POD_ANNOTATION=''
CONTCOUNT=2 create balloons-busybox

# Print full NRT yaml for debugging
vm-command "$nrt_kubectl_get -o yaml"

nrt-verify-zone-resource "default/pod1/pod1c0" "cpu" "capacity" "1500m" # limit < allowed cpus
nrt-verify-zone-attribute "default/pod1/pod1c0" "cpuset" "0-2" # expected fullsocket[1]
