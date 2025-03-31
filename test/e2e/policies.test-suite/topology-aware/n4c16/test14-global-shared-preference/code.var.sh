cleanup-test-pods() {
    # Make sure all the pods in default namespace are cleared so we get a fresh start
    vm-command "kubectl delete pods --all --now"
}

# restart with a global shared CPU allocation preference
PREFER_SHARED_CPUS=true helm_config=$(instantiate helm-config.yaml) helm-launch topology-aware

# verify that an unannotated guaranteed containers get shared CPUs
CONTCOUNT=1 CPU=1 create guaranteed
report allowed
verify `# pod0c0 has shared CPUs` \
       "len(cpus['pod0c0']) > 1"

CONTCOUNT=1 CPU=2 create guaranteed
report allowed
verify `# pod1c0 has shared CPUs` \
       "len(cpus['pod1c0']) > 1"

cleanup-test-pods

# verify that a container can be annotated to opt out from shared allocation
ANNOTATIONS=('prefer-shared-cpus.resource-policy.nri.io/pod: "false"')
CONTCOUNT=1 CPU=1 create guaranteed-annotated
report allowed
verify `# pod2c0 has a single exclusive CPU allocated` \
       "len(cpus['pod2c0']) == 1"

# verify that a container can be annotated to opt out from shared allocation
ANNOTATIONS=('prefer-shared-cpus.resource-policy.nri.io/pod: "false"')
CONTCOUNT=1 CPU=2 create guaranteed-annotated
report allowed
verify `# pod3c0 has a single exclusive CPU allocated` \
       "len(cpus['pod3c0']) == 2"

cleanup-test-pods

helm-terminate
