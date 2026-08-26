cleanup() {
    delete-pods --all
    helm-terminate
}

cleanup
helm_config=${TEST_DIR}/topology-aware-nrt.cfg helm-launch topology-aware

# Print full NRT yaml for debugging
nrt-dump

# Verify selected zone attributes
nrt-verify-zone-attribute "socket #0" "memory set" '^0,2,4$'
nrt-verify-zone-attribute "socket #0" "shared cpuset" '^0-2$'
nrt-verify-zone-attribute "socket #0" "reserved cpuset" '^3$'

nrt-verify-zone-attribute "socket #1" "memory set" '^1,3,5$'
nrt-verify-zone-attribute "socket #1" "shared cpuset" '^4-7$'

nrt-verify-zone-attribute "root" "memory set" '^0-5$'
nrt-verify-zone-attribute "root" "shared cpuset" '^0-2,4-7$'
nrt-verify-zone-attribute "root" "reserved cpuset" '^3$'
