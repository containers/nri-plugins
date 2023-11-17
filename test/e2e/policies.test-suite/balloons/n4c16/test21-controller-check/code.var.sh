# Test that the nri-resource-policy controllers are called properly

helm-terminate
helm_config=$TEST_DIR/balloons-config.cfg helm-launch balloons

# Check that the test controller starts and gets called in proper places
vm-run-until --timeout 5 \
    "curl --silent --noproxy localhost http://localhost:8891/e2e-test-controller-state | \
         jq '.Log.ControllerEvent[]' 2>&1 | grep -q Start" || \
    error "Controller not started properly."

# Create a pod with two containers, make sure we get controller events
CPUREQ="" CPULIM="" MEMREQ="" MEMLIM="" CONTCOUNT=2 create balloons-busybox

# For pod creation we should see PreCreate and PostStart events
vm-command-q "curl --silent --noproxy localhost http://localhost:8891/e2e-test-controller-state" | jq '.Log.PreCreate[]' 2>&1 | tr -d '"' | awk -v RS="" '/pod0c0/&&/pod0c1/{r=1; exit} END{exit !r}' || error "PreCreate event not proper"
vm-command-q "curl --silent --noproxy localhost http://localhost:8891/e2e-test-controller-state" | jq '.Log.PostStart[]' 2>&1 | tr -d '"' | awk -v RS="" '/pod0c0/&&/pod0c1/{r=1; exit} END{exit !r}' || error "PostStart event not proper"

# Then delete the pod, we should see PostStop event
vm-command "kubectl delete pods pod0 --now"

vm-command-q "curl --silent --noproxy localhost http://localhost:8891/e2e-test-controller-state" | jq '.Log.PostStop[]' 2>&1 | tr -d '"' | awk -v RS="" '/pod0c0/&&/pod0c1/{r=1; exit} END{exit !r}' || error "PostStop event not proper"

helm-terminate
