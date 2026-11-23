# The admission policies of the chart: the driver's service account may
# change only what belongs to its own node
#
# The ClusterRole of the driver lets it create and delete pods and
# ResourceClaims in every namespace (the companions of leases, test12), and
# change ResourceSlices. Two ValidatingAdmissionPolicies of the chart
# (deployment/helm/dra-driver-cxl/templates) narrow that down for the user
# system:serviceaccount:kube-system:nri-dra-driver-cxl, whose node the API
# server knows from the token of its pod (the user extra
# authentication.kubernetes.io/node-name, feature
# ServiceAccountTokenPodNodeInfo):
# - resourceslices-policy-nri-dra-driver-cxl: only the ResourceSlices of
#   its node.
# - lease-companions-policy-nri-dra-driver-cxl: creates and deletes only
#   pods and claims named cxl-lease-<lease> with the labels of a lease
#   companion (cxl.nri.io/lease=<lease>, cxl.nri.io/requester-uid,
#   cxl.nri.io/node) whose node label is its node. The claims request only
#   the classes cxl-local and cxl-pool and their -shared and -persistent
#   variants, without adminAccess. The pods are pinned to its node by a
#   required node affinity and use only the claim of their name. They run
#   the companion image of the chart without a command, volumes, host
#   namespaces, host ports, privileged containers, added capabilities or a
#   system priority class.
# The test installs the chart with leases.namespaceLabels. Then a third
# policy, lease-namespaces-policy-nri-dra-driver-cxl, binds every user: only
# namespaces with those labels may have claims of the class cxl-lease.
#
# The test talks to the API server as the driver of VM2 would: kubectl of
# the cluster's admin impersonates the service account with the node extra
# (--as, --as-user-extra), so the API server authorizes the requests with
# the ClusterRole of the driver and admits them with the policies. Creates
# are server-side dry runs: admission runs as for a real create, and
# nothing is left if a policy admits what it should refuse.
#
# What you will see: the three policies Deny-bound and free of type-check
# warnings; a companion pod, a companion claim and a ResourceSlice of VM2
# admitted. Refused with the message of its policy: one request for each
# rule, for instance a pod pinned to VM1, a claim of the class cxl-dram,
# the deletion of a pod of VM1, a ResourceSlice of all nodes, and a
# template of the class cxl-lease in a namespace without the labels. Then
# the real thing: a pool lease of a running container (test12) whose
# companion pod and claim the driver creates and, at the release, deletes
# under the policy. While they run, the driver of VM2 may delete them (dry
# runs). A pod that names the CDI device of the lease claim in a
# cdi.k8s.io annotation does not get the device. Last, an unowned companion
# claim (a lease name, no lease) that the driver deletes at its next
# reconcile (at most 60 s).
#
# Everything happens on VM2, the VM of the test (n4-cxl-shared-2), the
# control plane of the cluster of VM2, VM1 and VM0. Run test00-up of
# n4-cxl-shared-1, -0 and -2 first: they create the VMs and make them one
# cluster. The DRA driver comes from its image, build it first:
#   make PLUGINS=nri-dra-driver-cxl SKIP_LICENSES=1 OTHER_IMAGE_TARGETS= BINARIES= images

if [[ "$distro" != *"fedora"* ]]; then
    echo "Test verdict: SKIP (this test runs only on fedora)"
    exit 0
fi

echo "### preconditions: one cluster, VM2 control plane, VM1 and VM0 workers (test00-up)"
pool-cluster-require

VM1="$POOL_VM1_DIR"    # a worker of the cluster
VM1_NAME="$POOL_VM1_NAME"
VM2="$OUTPUT_DIR"      # the VM of this test, the control plane
VM2_NAME="$POOL_VM2_NAME"
POOLED0_SERIAL=0xc1ee0001   # 0xc1ee....: unshared pool devices
DRIVER_USER=system:serviceaccount:kube-system:nri-dra-driver-cxl
NODE_EXTRA=authentication.kubernetes.io/node-name
POLICY=lease-companions-policy-nri-dra-driver-cxl
SLICE_POLICY=resourceslices-policy-nri-dra-driver-cxl
NS_POLICY=lease-namespaces-policy-nri-dra-driver-cxl
NS_LABEL=cxl-leases         # leases.namespaceLabels: {cxl-leases: allowed}
FAKE_LEASE=0badc0de         # the lease of the companion-shaped objects
FAKE_COMPANION=cxl-lease-$FAKE_LEASE
ORPHAN=$FAKE_COMPANION      # a companion claim without a lease

admin-kubectl() {
    # Usage: admin-kubectl KUBECTL-ARGS...
    #
    # Run "kubectl KUBECTL-ARGS" in namespace $DRA_NS in VM2 as the admin
    # of the cluster. With "-f -" in KUBECTL-ARGS the YAML comes from
    # stdin. Return the exit status of kubectl. COMMAND_OUTPUT has the
    # output, COMMAND_STATUS the exit status.
    local yaml
    if [[ " $* " == *" -f - "* ]]; then
        yaml=$(cat)
        vmdir-command "$VM2" "kubectl -n $DRA_NS $* <<'DRIVER_KUBECTL_EOF'
$yaml
DRIVER_KUBECTL_EOF"
    else
        vmdir-command "$VM2" "kubectl -n $DRA_NS $*"
    fi
}

driver-kubectl() {
    # Usage: driver-kubectl NODE KUBECTL-ARGS...
    #
    # Run admin-kubectl KUBECTL-ARGS as the service account of the driver
    # on node NODE ("" for a user without a node).
    local node="$1" as
    shift
    as=(--as="$DRIVER_USER")
    [ -z "$node" ] || as+=(--as-user-extra="$NODE_EXTRA=$node")
    admin-kubectl "${as[@]}" "$@"
}

admitted() {
    # Usage: admitted WHAT
    #
    # Fail the test unless the last driver-kubectl succeeded.
    [ "$COMMAND_STATUS" == 0 ] ||
        error "$1: refused, expected the driver of $VM2_NAME to be allowed: $COMMAND_OUTPUT"
    echo "admitted: $1"
}

policy-denied() {
    # Usage: policy-denied WHAT MESSAGE [POLICY]
    #
    # Fail the test unless the last driver-kubectl failed, denied by POLICY
    # (default $POLICY) with MESSAGE.
    local policy="${3:-$POLICY}"
    [ "$COMMAND_STATUS" != 0 ] ||
        error "$1: admitted, expected $policy to refuse it"
    grep -qF "ValidatingAdmissionPolicy '$policy' with binding '$policy' denied request: $2" <<< "$COMMAND_OUTPUT" ||
        error "$1: not denied by $policy with \"$2\""
    echo "denied by $policy: $1"
}

companion-denied() {
    # Usage: companion-denied WHAT MESSAGE SPEC-LINES [CONTAINER-LINES [IMAGE]]
    #
    # Fail the test unless $POLICY refuses with MESSAGE that the driver of
    # VM2 creates its companion pod with SPEC-LINES, CONTAINER-LINES and
    # IMAGE (see companion-pod-yaml).
    driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(companion-pod-yaml "$VM2_NAME" "$VM2_NAME" "$3" "${4:-}" "${5:-}")"
    policy-denied "$1" "$2"
}

companion-pod-yaml() {
    # Usage: companion-pod-yaml NODE-LABEL AFFINITY-NODE [SPEC-LINES [CONTAINER-LINES [IMAGE]]]
    #
    # Print pod $FAKE_COMPANION in the shape of a companion pod: the labels
    # of lease $FAKE_LEASE with node label NODE-LABEL, a required node
    # affinity to AFFINITY-NODE (none if AFFINITY-NODE is ""), no service
    # account token, the claim $FAKE_COMPANION, one container of IMAGE
    # (default $DRA_COMPANION_IMAGE). CONTAINER-LINES are added to the container
    # (indented by 4), SPEC-LINES to the spec (indented by 2). SPEC-LINES
    # with resourceClaims replace the claim.
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $FAKE_COMPANION
  labels:
    $DRA_LEASE_LABEL: $FAKE_LEASE
    $DRA_REQUESTER_LABEL: 00000000-0000-0000-0000-00000badc0de
    $DRA_NODE_LABEL: $1
spec:
EOF
    [ -z "$2" ] || cat <<EOF
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchFields:
          - {key: metadata.name, operator: In, values: [$2]}
EOF
    [[ "${3:-}" == *resourceClaims:* ]] ||
        echo "  resourceClaims: [{name: lease, resourceClaimName: $FAKE_COMPANION}]"
    cat <<EOF
  automountServiceAccountToken: false
  enableServiceLinks: false
  containers:
  - name: pause
    image: ${5:-$DRA_COMPANION_IMAGE}
EOF
    [ -z "${4:-}" ] || echo "$4"
    [ -z "${3:-}" ] || echo "$3"
}

labelled-claim-yaml() {
    # Usage: labelled-claim-yaml NAME [NODE-LABEL [LEASE [REQUEST-LINES]]]
    #
    # Print ResourceClaim NAME with the labels of lease LEASE (default
    # $FAKE_LEASE) and node label NODE-LABEL, without labels if NODE-LABEL
    # is "" or not given. REQUEST-LINES are its device requests (indented
    # by 4). Without them its spec is empty. No pod uses it. The driver of
    # the node deletes it as unowned only when NAME is cxl-lease-LEASE.
    cat <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: $1
EOF
    [ -z "${2:-}" ] || cat <<EOF
  labels:
    $DRA_LEASE_LABEL: ${3:-$FAKE_LEASE}
    $DRA_REQUESTER_LABEL: 00000000-0000-0000-0000-00000badc0de
    $DRA_NODE_LABEL: $2
EOF
    if [ -z "${4:-}" ]; then
        echo "spec: {}"
    else
        printf 'spec:\n  devices:\n    requests:\n%s\n' "$4"
    fi
}

slice-yaml() {
    # Usage: slice-yaml NODE-LINE
    #
    # Print ResourceSlice test22-slice of driver $DRA_LOCAL_DRIVER without
    # devices. NODE-LINE (indented by 2) says which nodes it is for.
    cat <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
metadata:
  name: test22-slice
spec:
  driver: $DRA_LOCAL_DRIVER
  pool: {name: test22, generation: 1, resourceSliceCount: 1}
$1
  devices: []
EOF
}

requester-yaml() {
    # Usage: requester-yaml
    #
    # Pod "requester" on VM2 with a lease claim from a template: its
    # container main may lease memory (test12).
    dra-lease-template-yaml cxl-lease
    cat <<EOF
---
apiVersion: v1
kind: Pod
metadata:
  name: requester
spec:
  nodeSelector:
    kubernetes.io/hostname: $VM2_NAME
  terminationGracePeriodSeconds: 2
  restartPolicy: Never
  resourceClaims:
  - name: cxl
    resourceClaimTemplateName: cxl-lease
  containers:
  - name: main
    image: $DRA_TOOLS_IMAGE
    imagePullPolicy: IfNotPresent
    command: ["sh", "-c", "trap 'exit 0' TERM; sleep infinity & wait"]
    resources:
      claims:
      - name: cxl
EOF
}

echo "### preconditions: patched qemu, hdm_for_passthrough=on, kernel with CONFIG_FS_DAX"
pool-vm-require "$VM2" "$(basename "$TOPOLOGY_DIR")"

echo "### preconditions: no CXL memory devices in VM2 and VM1"
pool-vm-tools-install "$VM2"
pool-vm-reset "$VM2"
pool-vm-reset "$VM1"

echo "### preconditions: fake-cxl-pool-server on the host, one unshared device"
pool-server-start "
  - name: pooled0
    size: 512Mi
    shared: false
    serial: $POOLED0_SERIAL"
pool-client-install "$VM2"

echo "### preconditions: the pool controller and the driver run in the cluster"
# The chart has the policy of lease namespaces only with namespaceLabels.
values="$TEST_OUTPUT_DIR/test22-helm-values.yaml"
{ cat "$DRA_HELM_VALUES"; printf 'leases:\n  namespaceLabels:\n    %s: allowed\n' "$NS_LABEL"; } > "$values"
DRA_HELM_VALUES="$values"
dra-build
dra-install
dra-pull-images "$VM2" "$DRA_TOOLS_IMAGE" "$DRA_COMPANION_IMAGE"

echo "### A: the policies of the chart"
for p in "$SLICE_POLICY" "$POLICY" "$NS_POLICY"; do
    # The API server type-checks a policy against the kinds it matches and
    # reports the expressions that do not fit in its status.
    dra-wait "$VM2" "policy-$p" "validatingadmissionpolicy $p" \
        "j.get('status', {}).get('observedGeneration') == j['metadata']['generation'] and not j['status'].get('typeChecking', {}).get('expressionWarnings')" 30
    # The policy of lease namespaces binds every user.
    conditions="['request.userInfo.username == \"$DRIVER_USER\"']"
    [ "$p" != "$NS_POLICY" ] || conditions="[]"
    dra-assert "$COMMAND_OUTPUT" "j['spec']['failurePolicy'] == 'Fail' and [c['expression'] for c in j['spec'].get('matchConditions', [])] == $conditions"
    dra-json "$VM2" validatingadmissionpolicybinding "$p" ||
        command-error "no ValidatingAdmissionPolicyBinding $p"
    dra-assert "$COMMAND_OUTPUT" "j['spec']['policyName'] == '$p' and j['spec']['validationActions'] == ['Deny']"
done
dra-json "$VM2" validatingadmissionpolicy "$POLICY"
dra-assert "$COMMAND_OUTPUT" "sorted((r['apiGroups'], r['resources'], sorted(r['operations'])) for r in j['spec']['matchConstraints']['resourceRules']) == [([''], ['pods'], ['CREATE', 'DELETE']), (['resource.k8s.io'], ['resourceclaims'], ['CREATE', 'DELETE'])]"
# An unowned companion: a companion claim of VM2 without a lease. The driver of VM2
# deletes it at its next reconcile, checked in E.
dra-apply "$VM2" <<< "$(labelled-claim-yaml "$ORPHAN" "$VM2_NAME")"

echo "### B: what the driver of VM2 may create"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(companion-pod-yaml "$VM2_NAME" "$VM2_NAME")"
admitted "a companion pod of $VM2_NAME"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(labelled-claim-yaml cxl-lease-0badcafe "$VM2_NAME" 0badcafe "    - name: mem
      firstAvailable:
      - {name: local, deviceClassName: cxl-local}
      - {name: pool, deviceClassName: cxl-pool}")"
admitted "a companion claim of $VM2_NAME"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(slice-yaml "  nodeName: $VM2_NAME")"
admitted "a ResourceSlice of $VM2_NAME"

echo "### C: what the policies refuse"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(companion-pod-yaml "$VM2_NAME" "$VM2_NAME" | grep -vF -e "$DRA_LEASE_LABEL:" -e "$DRA_REQUESTER_LABEL:")"
policy-denied "a pod without the labels of a lease" \
    "this user may create only pods with the labels $DRA_LEASE_LABEL, $DRA_REQUESTER_LABEL and $DRA_NODE_LABEL of a lease companion"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(labelled-claim-yaml unlabelled-object)"
policy-denied "an unlabelled claim" \
    "this user may create only resourceclaims with the labels $DRA_LEASE_LABEL, $DRA_REQUESTER_LABEL and $DRA_NODE_LABEL of a lease companion"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(companion-pod-yaml "$VM1_NAME" "$VM1_NAME")"
policy-denied "a companion pod of $VM1_NAME" \
    "this user running on node '$VM2_NAME' may not create pods of node '$VM1_NAME'"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(labelled-claim-yaml not-a-companion "$VM2_NAME")"
policy-denied "a claim not named after its lease" \
    "this user may create only resourceclaims named cxl-lease-<lease> with the label $DRA_LEASE_LABEL=<lease>"
pinned="this user running on node '$VM2_NAME' may create only pods whose required node affinity selects that node by metadata.name in every term"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(companion-pod-yaml "$VM2_NAME" "$VM1_NAME")"
policy-denied "a pod of $VM2_NAME pinned to $VM1_NAME" "$pinned"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(companion-pod-yaml "$VM2_NAME" "")"
policy-denied "a pod without node affinity" "$pinned"
companion-denied "a pod with nodeName $VM1_NAME" "$pinned" "  nodeName: $VM1_NAME"
companion-denied "a pod with a hostPath volume" "this user may not create pods with volumes" "  volumes:
  - {name: root, hostPath: {path: /}}" "    volumeMounts:
    - {name: root, mountPath: /host}"
for ns in hostNetwork hostPID hostIPC; do
    companion-denied "a pod with $ns" "this user may not create pods in the host network, PID or IPC namespace" "  $ns: true"
done
companion-denied "a pod with a privileged container" "this user may not create pods with privileged containers" \
    "" "    securityContext: {privileged: true}"
companion-denied "a pod with a privileged initContainer" "this user may not create pods with privileged containers" "  initContainers:
  - {name: init, image: $DRA_COMPANION_IMAGE, securityContext: {privileged: true}}"
image="this user may create only pods whose containers run the companion image $DRA_COMPANION_IMAGE"
companion-denied "a pod of another image" "$image" "" "" "$DRA_TOOLS_IMAGE"
companion-denied "a pod with an initContainer of another image" "$image" "  initContainers:
  - {name: init, image: $DRA_TOOLS_IMAGE}"
companion-denied "a pod with a claim from a template" "this user may create only pods whose ResourceClaims have the name of the pod" \
    "  resourceClaims: [{name: lease, resourceClaimTemplateName: cxl-lease}]"
companion-denied "a pod with a command" "this user may not create pods with a command or args" "" "    command: [sh]"
companion-denied "a pod with args" "this user may not create pods with a command or args" "" "    args: [sh]"
companion-denied "a pod with a host port" "this user may not create pods with host ports" \
    "" "    ports: [{containerPort: 8080, hostPort: 8080}]"
companion-denied "a pod that adds a capability" "this user may not create pods that add capabilities" \
    "" "    securityContext: {capabilities: {add: [SYS_ADMIN]}}"
companion-denied "a pod of priority class system-node-critical" \
    "this user may not create pods of the priority classes system-node-critical and system-cluster-critical" \
    "  priorityClassName: system-node-critical"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(labelled-claim-yaml cxl-lease-0badcafe "$VM2_NAME" 0badcafe "    - {name: mem, exactly: {deviceClassName: cxl-dram}}")"
policy-denied "a claim of class cxl-dram" \
    "this user may create only resourceclaims of the device classes cxl-local, cxl-local-shared, cxl-local-persistent, cxl-pool, cxl-pool-shared, cxl-pool-persistent"
# The API server refuses adminAccess in a namespace without this label
# before the policies run.
vmdir-command "$VM2" "kubectl label namespace $DRA_NS resource.kubernetes.io/admin-access=true" ||
    command-error "cannot label namespace $DRA_NS for adminAccess"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(labelled-claim-yaml cxl-lease-0badcafe "$VM2_NAME" 0badcafe "    - {name: mem, exactly: {deviceClassName: cxl-pool, adminAccess: true}}")"
policy-denied "a claim with adminAccess" "this user may not create resourceclaims with adminAccess"
vmdir-command "$VM2" "kubectl label namespace $DRA_NS resource.kubernetes.io/admin-access-" ||
    command-error "cannot remove label resource.kubernetes.io/admin-access of namespace $DRA_NS"
driver-kubectl "" create --dry-run=server -f - <<< "$(companion-pod-yaml "$VM2_NAME" "$VM2_NAME")"
policy-denied "a companion pod of a user without a node" \
    "no node association found for user, this user must run in a pod on a node and ServiceAccountTokenPodNodeInfo must be enabled"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(slice-yaml "  nodeName: $VM1_NAME")"
policy-denied "a ResourceSlice of $VM1_NAME" \
    "this user running on node '$VM2_NAME' may not modify resourceslices on node '$VM1_NAME'" "$SLICE_POLICY"
driver-kubectl "$VM2_NAME" create --dry-run=server -f - <<< "$(slice-yaml "  allNodes: true")"
policy-denied "a ResourceSlice of all nodes" \
    "this user running on node '$VM2_NAME' may not modify cluster resourceslices" "$SLICE_POLICY"
# The admin is not restricted. It creates objects that the driver of VM2
# may not delete: an unlabelled claim, a claim of VM2 not named after its
# lease, a claim of VM1 and a pod of VM1 that never runs. The driver of
# VM1 does not take them for its companions: they are not named after
# their lease.
dra-apply "$VM2" <<< "$(labelled-claim-yaml unlabelled-object)
---
$(labelled-claim-yaml not-a-companion "$VM2_NAME")
---
$(labelled-claim-yaml other-node-object "$VM1_NAME")
---
apiVersion: v1
kind: Pod
metadata:
  name: other-node-pod
  labels:
    $DRA_NODE_LABEL: $VM1_NAME
spec:
  nodeSelector:
    test22-no-such-node: none
  containers:
  - {name: pause, image: $DRA_COMPANION_IMAGE}"
driver-kubectl "$VM2_NAME" delete --dry-run=server resourceclaim unlabelled-object
policy-denied "the deletion of an unlabelled claim" \
    "this user running on node '$VM2_NAME' may not delete resourceclaims without the label $DRA_NODE_LABEL"
driver-kubectl "$VM2_NAME" delete --dry-run=server resourceclaim not-a-companion
policy-denied "the deletion of a claim not named after its lease" \
    "this user may delete only resourceclaims named cxl-lease-<lease> with the label $DRA_LEASE_LABEL=<lease>"
driver-kubectl "$VM2_NAME" delete --dry-run=server pod other-node-pod
policy-denied "the deletion of a pod of $VM1_NAME" \
    "this user running on node '$VM2_NAME' may not delete pods of node '$VM1_NAME'"
driver-kubectl "$VM2_NAME" delete resourceclaim other-node-object
policy-denied "the deletion of a claim of $VM1_NAME" \
    "this user running on node '$VM2_NAME' may not delete resourceclaims of node '$VM1_NAME'"
dra-json "$VM2" resourceclaim -n "$DRA_NS" other-node-object ||
    error "claim other-node-object is gone, the driver of $VM2_NAME deleted it"
dra-delete "$VM2" pod other-node-pod --grace-period=0
for claim in unlabelled-object not-a-companion other-node-object; do
    dra-delete "$VM2" resourceclaim "$claim"
done
# Namespace $DRA_NS has no label $NS_LABEL yet: nobody may create the
# template of the lease claim of the requester in D there.
admin-kubectl create --dry-run=server -f - <<< "$(dra-lease-template-yaml cxl-lease)"
policy-denied "a template of class cxl-lease in namespace $DRA_NS" \
    "namespace $DRA_NS may not have resourceclaimtemplates of the device class cxl-lease: only namespaces with the labels $NS_LABEL=allowed may" "$NS_POLICY"
vmdir-command "$VM2" "kubectl label namespace $DRA_NS $NS_LABEL=allowed" ||
    command-error "cannot label namespace $DRA_NS"

echo "### D: the companion of a real lease, created and deleted by the driver"
dra-apply "$VM2" <<< "$(requester-yaml)"
dra-pod-wait "$VM2" requester Running 60
dra-json "$VM2" pod -n "$DRA_NS" requester
REQUESTER_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" request --source pool --size 512Mi --wait 120s ||
    command-error "cxl-request request --source pool --size 512Mi failed"
json=$(dra-last-json "$COMMAND_OUTPUT") || error "cxl-request printed no JSON response"
dra-assert "$json" "j.get('state') == 'Ready' and j.get('source') == 'pool' and j.get('serial') == '$POOLED0_SERIAL'"
LEASE=$(dra-value "$json" "j['lease']")
COMPANION="cxl-lease-$LEASE"
# The pod as the API server admitted it, after the mutating admission
# (default service account, tolerations): what the policy checked.
dra-pod-wait "$VM2" "$COMPANION" Running 30
dra-json "$VM2" pod -n "$DRA_NS" "$COMPANION"
dra-companion-meta-check "$COMMAND_OUTPUT" "$LEASE" "$REQUESTER_UID" "$VM2_NAME" "$DRA_NS/requester/main"
dra-assert "$COMMAND_OUTPUT" "j['spec']['affinity']['nodeAffinity']['requiredDuringSchedulingIgnoredDuringExecution']['nodeSelectorTerms'] == [{'matchFields': [{'key': 'metadata.name', 'operator': 'In', 'values': ['$VM2_NAME']}]}]"
dra-assert "$COMMAND_OUTPUT" "not j['spec'].get('volumes') and not any(j['spec'].get(k) for k in ('hostNetwork', 'hostPID', 'hostIPC'))"
dra-assert "$COMMAND_OUTPUT" "[(c['image'], bool(c.get('securityContext', {}).get('privileged'))) for c in j['spec']['containers'] + j['spec'].get('initContainers', [])] == [('$DRA_COMPANION_IMAGE', False)]"
dra-json "$VM2" resourceclaim -n "$DRA_NS" "$COMPANION" ||
    command-error "no companion claim $COMPANION"
dra-companion-meta-check "$COMMAND_OUTPUT" "$LEASE" "$REQUESTER_UID" "$VM2_NAME" "$DRA_NS/requester/main"
# The driver deletes companions with foreground propagation.
for kind in resourceclaim pod; do
    driver-kubectl "$VM2_NAME" delete --dry-run=server --cascade=foreground "$kind" "$COMPANION"
    admitted "the deletion of the companion $kind $COMPANION"
done
# The CDI device of the lease claim: the lease socket. A pod that names
# it in a cdi.k8s.io annotation does not get it.
LEASE_CLAIM=$(dra-pod-claim "$VM2" requester) || error "pod requester has no claim"
dra-json "$VM2" resourceclaim -n "$DRA_NS" "$LEASE_CLAIM" ||
    command-error "no lease claim $LEASE_CLAIM"
LEASE_CLAIM_UID=$(dra-value "$COMMAND_OUTPUT" "j['metadata']['uid']")
dra-lease-cdi-assert "$VM2" "$LEASE_CLAIM_UID"
dra-apply "$VM2" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: cdi-annotated
  annotations:
    cdi.k8s.io/test22: $DRA_LOCAL_DRIVER/device=lease-$LEASE_CLAIM_UID
spec:
  nodeSelector:
    kubernetes.io/hostname: $VM2_NAME
  terminationGracePeriodSeconds: 2
  restartPolicy: Never
  containers:
  - name: main
    image: $DRA_TOOLS_IMAGE
    imagePullPolicy: IfNotPresent
    command: ["sh", "-c", "trap 'exit 0' TERM; sleep infinity & wait"]
EOF
dra-pod-wait "$VM2" cdi-annotated Running 60
# shellcheck disable=SC2016 # expanded by sh in the container
dra-exec "$VM2" cdi-annotated/main sh -c 'echo "${CXL_LEASE_SOCKET:-no socket}"; test -e /run/cxl || echo "no /run/cxl"' ||
    command-error "cannot look for the lease socket in pod cdi-annotated"
[ "$COMMAND_OUTPUT" == "no socket
no /run/cxl" ] || error "pod cdi-annotated got CDI device lease-$LEASE_CLAIM_UID from its annotation: $COMMAND_OUTPUT"
dra-delete-pod "$VM2" cdi-annotated
dra-exec "$VM2" requester/main "$DRA_REQUEST_CTR_BIN" release --lease "$LEASE" ||
    command-error "cxl-request release --lease $LEASE failed"
dra-assert "$(dra-last-json "$COMMAND_OUTPUT")" "j.get('lease') == '$LEASE' and j.get('state') == 'Released'"
dra-wait "$VM2" companion-pod-deleted "pods -n $DRA_NS -l $DRA_LEASE_LABEL=$LEASE" "items == []" 90
dra-wait "$VM2" companion-claim-deleted "resourceclaims -n $DRA_NS" "'$COMPANION' not in names()" 90
dra-leases "$VM2" requester/main
dra-assert "$DRA_LEASES" "j == []"

echo "### E: the unowned companion claim, deleted by the driver"
dra-wait "$VM2" orphan-deleted "resourceclaims -n $DRA_NS" "'$ORPHAN' not in names()" 90

echo "### cleanup"
dra-delete "$VM2" pod requester --grace-period=2 --wait=true --timeout=60s
dra-delete "$VM2" resourceclaimtemplate cxl-lease --wait=true --timeout=60s
vmdir-command "$VM2" "kubectl label namespace $DRA_NS $NS_LABEL-" ||
    command-error "cannot remove label $NS_LABEL of namespace $DRA_NS"
dra-assert-clean "$VM2" "$VM1"
dra-cleanup
pool-cleanup
trap - EXIT
pool-port-busy && error "port $POOL_PORT is still busy after stopping the server"
echo "the driver of $VM2_NAME created and deleted its companions under $POLICY"
