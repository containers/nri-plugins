# shellcheck shell=bash disable=SC2154 # nri_resource_policy_src is of the e2e framework
# The pool controllers of the demo run in the cluster: Deployments of the
# names of dra-install's units in namespace $DRA_CONTROLLER_NS, made from
# the manifest $DEMO_CONTROLLER_MANIFEST (its Deployment has the pool
# server URL $DEMO_CONTROLLER_MANIFEST_URL), with the image that
# demo-controller-image-build builds ($DEMO_CONTROLLER_IMAGE_TAR, "make
# image" of fake-cxl-pool, its content tag in $DEMO_CONTROLLER_IMAGE_REF)
# imported into every node as $DEMO_CONTROLLER_IMAGE. The pods of all of
# them have the label $DEMO_CONTROLLER_POD_LABEL. This file redefines the
# hooks of dra.source.sh for that: dra-controller-start,
# dra-controllers-status-cmd and dra-controllers-log-cmd.
DEMO_CONTROLLER_MANIFEST="$nri_resource_policy_src/scripts/testing/fake-cxl-pool/deploy/fake-cxl-pool-controller.yaml"
DEMO_CONTROLLER_MANIFEST_URL=http://192.168.76.2:9909
DEMO_CONTROLLER_IMAGE=localhost/fake-cxl-pool-controller:testing
DEMO_CONTROLLER_IMAGE_TAR="$nri_resource_policy_src/scripts/testing/fake-cxl-pool/bin/fake-cxl-pool-controller-image.tar"
DEMO_CONTROLLER_IMAGE_REF="$nri_resource_policy_src/scripts/testing/fake-cxl-pool/bin/fake-cxl-pool-controller-image.ref"
DEMO_CONTROLLER_POD_LABEL=app.kubernetes.io/name=fake-cxl-pool-controller
DEMO_CONTROLLER_IMAGES_PUT=""   # set once dra-controller-start has put the image on the nodes

demo-controller-image-build() {
    # Usage: demo-controller-image-build
    #
    # Build the image of the controller ($DEMO_CONTROLLER_IMAGE_TAR) on the
    # host, after dra-build.
    host-command "make -C \"$nri_resource_policy_src/scripts/testing/fake-cxl-pool\" image" ||
        command-error "cannot build the image of fake-cxl-pool-controller (docker)"
    [ -f "$DEMO_CONTROLLER_IMAGE_TAR" ] && [ -s "$DEMO_CONTROLLER_IMAGE_REF" ] ||
        error "demo-controller-image-build: make image did not save $DEMO_CONTROLLER_IMAGE_TAR and $DEMO_CONTROLLER_IMAGE_REF"
}

dra-image-tar-put() {
    # Usage: dra-image-tar-put VMDIR TAR TAG REF
    #
    # Import the image TAG of the docker save tarball TAR into the
    # containerd of the VM of VMDIR and tag it REF, unless REF is TAG there
    # already (the same digest), the way dra-image-put imports the driver.
    local vmdir="$1" tar="$2" tag="$3" ref="$4" label file
    label=$(vmdir-label "$vmdir")
    file=${tar##*/}
    [ "${k8scri:-containerd}" == containerd ] ||
        error "dra-image-tar-put: $tag is imported with ctr, $label runs ${k8scri}"
    if vmdir-command-q "$vmdir" "ctr -n k8s.io images ls | awk -v ref=$ref -v tag=$tag '\$1 == ref {r = \$3} \$1 == tag {t = \$3} END {exit !(r != \"\" && r == t)}'"; then
        echo "dra-image-tar-put $label: $ref is $tag already"
        return 0
    fi
    host-command "scp -F \"$vmdir/.ssh-config\" \"$tar\" node:\"$file\"" ||
        command-error "cannot copy $tar to $label"
    vmdir-command "$vmdir" "ctr -n k8s.io images import $file && ctr -n k8s.io images tag --force $tag $ref; rc=\$?; rm -f $file; exit \$rc" ||
        command-error "cannot import $file as $ref in $label"
}

dra-controller-deploy() {
    # Usage: dra-controller-deploy NAME URL
    #
    # Apply the manifest of the pool controller, $DEMO_CONTROLLER_MANIFEST,
    # in the cluster of the VM of the test with its Deployment named NAME
    # (and labeled app.kubernetes.io/instance=NAME) for the pool server at
    # URL, and wait until the Deployment is rolled out, 90 s at most. Fail
    # the test unless the Deployment runs with -server URL.
    local name="$1" url="$2" yaml
    # The Deployment of the manifest is named $DRA_CONTROLLER_UNIT and has
    # -server $DEMO_CONTROLLER_MANIFEST_URL; the other objects are shared
    # by all controllers.
    yaml=$(sed -e "/^kind: Deployment\$/,/^---\$/ s/^\(  name:\) $DRA_CONTROLLER_UNIT\$/\1 $name/" \
               -e "s/^\( *app\.kubernetes\.io\/instance:\) $DRA_CONTROLLER_UNIT\$/\1 $name/" \
               -e "s|^\( *- \)$DEMO_CONTROLLER_MANIFEST_URL\$|\1$url|" "$DEMO_CONTROLLER_MANIFEST") ||
        error "dra-controller-deploy: cannot read $DEMO_CONTROLLER_MANIFEST"
    vmdir-command "$OUTPUT_DIR" "kubectl apply -f - <<'DRA_CONTROLLER_EOF'
$yaml
DRA_CONTROLLER_EOF
kubectl rollout status -n $DRA_CONTROLLER_NS deployment/$name --timeout=90s && kubectl get deployment -n $DRA_CONTROLLER_NS $name -o jsonpath='{.spec.template.spec.containers[0].args}{\"\\n\"}'" ||
        command-error "the Deployment $name of fake-cxl-pool-controller does not roll out in namespace $DRA_CONTROLLER_NS"
    grep -qF "\"$url\"" <<< "$COMMAND_OUTPUT" ||
        error "dra-controller-deploy: the Deployment $name runs without -server $url: has $DEMO_CONTROLLER_MANIFEST the Deployment $DRA_CONTROLLER_UNIT with $DEMO_CONTROLLER_MANIFEST_URL?"
}

dra-controller-start() {
    # Usage: dra-controller-start NAME
    #
    # The hook of dra-install: deploy the pool controller NAME for its pool
    # server (DRA_CONTROLLER_URLS) as a Deployment (dra-controller-deploy),
    # after the first call has imported its image into every node of the
    # cluster (DRA_INSTALLED_VMS).
    local ref v
    if [ -z "$DEMO_CONTROLLER_IMAGES_PUT" ]; then
        ref=$(cat "$DEMO_CONTROLLER_IMAGE_REF" 2>/dev/null)
        [ -f "$DEMO_CONTROLLER_IMAGE_TAR" ] && [ -n "$ref" ] ||
            error "dra-controller-start: no image of fake-cxl-pool-controller ($DEMO_CONTROLLER_IMAGE_TAR), run demo-controller-image-build first"
        for v in "${DRA_INSTALLED_VMS[@]}"; do
            dra-image-tar-put "$v" "$DEMO_CONTROLLER_IMAGE_TAR" "$ref" "$DEMO_CONTROLLER_IMAGE"
        done
        DEMO_CONTROLLER_IMAGES_PUT=1
    fi
    dra-controller-deploy "$1" "${DRA_CONTROLLER_URLS[$1]}"
}

dra-controllers-status-cmd() {
    # Usage: dra-controllers-status-cmd
    #
    # The hook of dra-driver-wait: the pods of the controllers and their
    # last log lines.
    echo "kubectl get pods -n $DRA_CONTROLLER_NS -o wide; kubectl logs -n $DRA_CONTROLLER_NS -l $DEMO_CONTROLLER_POD_LABEL --tail=40"
}

dra-controllers-log-cmd() {
    # Usage: dra-controllers-log-cmd
    #
    # The hook of dra-trace-start: follow the log of every controller pod.
    echo "kubectl logs -n $DRA_CONTROLLER_NS -f --tail=0 --max-log-requests=20 -l $DEMO_CONTROLLER_POD_LABEL"
}
