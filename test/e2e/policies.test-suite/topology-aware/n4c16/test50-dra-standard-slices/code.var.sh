# This test compares the topology-aware DRA devices with the reference driver
# devices. The reference driver publishes the standard CPU device attributes.
#
# The reference driver runs from an image of its staging registry. Its chart
# comes from its git repository, at the commit of the image. The test fails
# when the "latest" image lags the main branch of the repository by more than
# std_max_lag_days.
#
# Our devices are saved first. The test then runs the reference driver twice.
# The first run publishes a device per NUMA node. The second run publishes a
# device per socket. For each reference device, we must publish a device with
# the same name and level. Our device must have every attribute of the reference
# device, the same capacity values and the same allowMultipleAllocations. The
# request policies of the capacities are not compared. Our extra attributes are
# fine.
#
# The test does not compare machine devices. The two drivers pick the claim
# CPUs in different places. The reference driver groups by machine only with
# its external allocator. Its machine device then lists the IDs of all its
# CPUs. The Kubernetes scheduler picks the CPUs of each claim from them. Our
# policy picks the claim CPUs itself. Our machine device does not list CPU IDs.
#
# Variables:
# - std_repo: the git repository of the reference driver
# - std_image: the image repository of the reference driver. Its registry
#   must list the tag digests to resolve "latest". Artifact Registry does.
# - std_tag: the image tag to run, "latest" by default
# - std_max_lag_days: how many days "latest" may lag main, 14 by default
# - std_allowed: the attributes of the reference devices which our devices
#   may lack, the legacy NUMA attributes by default

std_repo=${std_repo:-https://github.com/kubernetes-sigs/dra-driver-cpu}
std_image=${std_image:-us-central1-docker.pkg.dev/k8s-staging-images/dra-driver-cpu/dra-driver-cpu}
std_tag=${std_tag:-latest}
std_max_lag_days=${std_max_lag_days:-14}
std_allowed=${std_allowed-dra.net/numaNode dra.cpu/numaNodeID}

# The host directory of the tag list, the clone of the reference driver and
# the saved devices.
std_dir=$TEST_OUTPUT_DIR/reference
# The chart of the reference driver in its repository.
std_chart=deployment/helm/dra-driver-cpu
# Main builds have tags like v<build date>-<sha7>. A build at a git tag uses
# that git tag.
std_dated='^v[0-9]{8}-[0-9a-f]{7}$'
# The driver name and the helm release of the reference driver.
std_driver=dra.cpu
std_release=test-reference
# The reference driver chart directory on the VM.
std_vm_chart=test-reference-chart
# The CPU which both drivers reserve. It gives socket 1 one CPU less than
# socket 0.
reserved=15

std-pods() {
    # Usage: std-pods
    #
    # Print the pods of both drivers. Fail if the pods cannot be listed. Does
    # not log the command. This makes it safe to call in a retry loop.
    vm-command-q "kubectl get pods -A -o name \
                      -l app.kubernetes.io/name=$(plugin-daemonset) &&
                  kubectl get pods -n kube-system -o name \
                      -l app.kubernetes.io/instance=$std_release"
}

cleanup() {
    cleanup-test --helm --cache --claims
    vm-command "helm uninstall -n kube-system $std_release --ignore-not-found --wait --timeout 60s" ||
        error "failed to uninstall the reference driver"
    vm-command "rm -rf $std_vm_chart" ||
        error "failed to remove $std_vm_chart"
    # Both drivers are NRI plugins. The next one starts only after both have
    # stopped.
    retry-until --timeout 60 --message "both drivers to stop" \
        'pods=$(std-pods) && [ -z "$pods" ]' ||
        error "the drivers did not stop: $(std-pods)"
    # The kubelet may keep the slices of stopped drivers for a while. A driver
    # check could then pass on stale devices.
    vm-command "kubectl delete resourceslices --field-selector spec.driver=$POLICY.nri.io &&
                kubectl delete resourceslices --field-selector spec.driver=$std_driver" ||
        error "failed to delete stale slices"
}

std-list-tags() {
    # Usage: std-list-tags
    #
    # Save the tag list of $std_image to $std_dir/tags.json, and print the tags.
    host-command "curl -sSf https://${std_image%%/*}/v2/${std_image#*/}/tags/list \
                      -o \"$std_dir/tags.json\"" ||
        error "failed to list the tags of $std_image"
    echo "tags of $std_image: $(jq -r '.tags | join(" ")' "$std_dir/tags.json")"
}

std-resolve-tag() {
    # Usage: std-resolve-tag
    #
    # Set std_run_tag to the tag to run, and std_commit to the git revision of
    # its build. Map "latest" to another tag with the same digest. Prefer a main
    # build tag to a git tag.
    local digest
    std_run_tag=$std_tag
    if [ "$std_tag" == latest ]; then
        jq -e 'has("manifest")' "$std_dir/tags.json" > /dev/null ||
            error "the registry of $std_image lists no tag digests: set std_tag"
        digest=$(jq -r '.manifest | to_entries[] |
                        select(.value.tag | index("latest")) | .key' "$std_dir/tags.json")
        std_run_tag=$(jq -r --arg d "$digest" --arg re "$std_dated" \
                         '.manifest[$d].tag - ["latest"] | sort_by(test($re) | not) |
                          .[0] // empty' "$std_dir/tags.json")
        [ -n "$std_run_tag" ] ||
            error "no tag besides latest has digest $digest"
    fi
    jq -e --arg t "$std_run_tag" '.tags | index($t)' "$std_dir/tags.json" > /dev/null ||
        error "$std_image has no tag $std_run_tag"
    if [[ "$std_run_tag" =~ $std_dated ]]; then
        std_commit=${std_run_tag##*-}
    else
        std_commit=$std_run_tag
    fi
}

std-clone() {
    # Usage: std-clone
    #
    # Clone $std_repo without trees to $std_dir/repo, and check out its chart
    # at $std_commit. Set std_commit to the full commit hash.
    local repo=$std_dir/repo
    host-command "git clone -q --filter=tree:0 --no-checkout $std_repo \"$repo\"" ||
        error "failed to clone $std_repo"
    std_commit=$(git -C "$repo" rev-parse -q --verify "$std_commit^{commit}")
    [ -n "$std_commit" ] ||
        error "$std_repo has no commit for tag $std_run_tag"
    host-command "git -C \"$repo\" sparse-checkout set $std_chart &&
                  git -C \"$repo\" checkout -q --detach $std_commit" ||
        error "failed to check out $std_chart at $std_commit"
    [ -f "$repo/$std_chart/Chart.yaml" ] ||
        error "$std_repo has no chart $std_chart at $std_commit"
    echo "reference driver: tag $std_run_tag, commit $std_commit"
}

std-check-lag() {
    # Usage: std-check-lag
    #
    # Print how many days $std_commit lags main of $std_repo. Fail the test if
    # the lag is more than $std_max_lag_days. Warn if it is more than 0.
    #
    # The lag is the time since the oldest merge to main which $std_commit
    # lacks.
    local oldest when lag days
    oldest=$(git -C "$std_dir/repo" log --first-parent --reverse --format='%ct %h %s' \
                 "$std_commit..origin/main") ||
        error "failed to list the merges to main after $std_commit"
    oldest=$(head -n 1 <<< "$oldest")
    if [ -z "$oldest" ]; then
        echo "reference driver: lag 0 days, $std_run_tag is the head of main"
        return 0
    fi
    when=${oldest%% *}
    lag=$(( $(date +%s) - when ))
    days=$(awk -v s="$lag" 'BEGIN { printf "%.1f", s / 86400 }')
    oldest="${oldest#* } of $(date -u -d "@$when" +%F)"
    if [ "$lag" -gt $((std_max_lag_days * 86400)) ]; then
        error "$std_run_tag lags main by $days days, more than $std_max_lag_days: it lacks $oldest"
    fi
    echo "WARNING: $std_run_tag lags main by $days days: it lacks $oldest"
}

std-run() {
    # Usage: std-run LEVEL
    #
    # Run the reference driver grouped by LEVEL, and wait until it publishes
    # only devices of LEVEL. Install the driver if it does not run yet.

    # The test creates no claims. It needs no DeviceClass. Older charts lack
    # the deviceClass option.
    local devclass=""
    grep -q '^deviceClass:' "$std_dir/repo/$std_chart/values.yaml" &&
        devclass="--set deviceClass.create=false"
    vm-command "helm upgrade --install -n kube-system $std_release ./$std_vm_chart $devclass \
                    --wait --timeout 180s \
                    --set image.repository=$std_image \
                    --set image.tag=$std_run_tag \
                    --set image.pullPolicy=Always \
                    --set args.exposePCIeRoots=$std_pcie \
                    --set-string args.reservedCPUs=$reserved \
                    --set args.groupBy=$1" ||
        error "failed to run the reference driver grouped by $1"
    # The names of the devices tell their level: cpudevnuma000 or
    # cpudevsocket000. The old devices stay until the new driver publishes.
    local prefix=cpudev${1%node}
    retry-until --timeout 60 --message "the reference driver to publish devices of level $1" \
        '[ "$(dra-devices $std_driver | jq --arg p $prefix \
                  "length > 0 and all(.[]; .name | startswith(\$p))")" == true ]' ||
        error "the reference driver published no devices of level $1 only"
}

std-diff() {
    # Usage: std-diff LEVEL
    #
    # Print the differences of the saved reference devices of LEVEL from our
    # devices, one per line.
    jq -r -n --arg level "$1" --arg allowed "$std_allowed" \
       --slurpfile ours "$std_dir/ours.json" --slurpfile std "$std_dir/std-$1.json" '
        ($ours[0] | map(select(.attributes["level"].string == $level)) |
            INDEX(.name)) as $ours |
        ($allowed | split(" ")) as $allowed |
        $std[0][] | . as $ref | $ours[.name] as $our |
        if $our == null then
            "\(.name): no device of ours of level \($level)"
        else
            (.attributes | to_entries[] |
                select((.key | IN($allowed[])) and $our.attributes[.key] == null | not) |
                select($our.attributes[.key] != .value) |
                "\($ref.name): attribute \(.key) is \($our.attributes[.key] | tojson), " +
                "reference \(.value | tojson)"),
            ((.capacity // {}) + ($our.capacity // {}) | keys[] |
                select($our.capacity[.].value != $ref.capacity[.].value) |
                "\($ref.name): capacity \(.) is \($our.capacity[.].value | tojson), " +
                "reference \($ref.capacity[.].value | tojson)"),
            (select($our.allowMultipleAllocations != .allowMultipleAllocations) |
                "\(.name): allowMultipleAllocations is \($our.allowMultipleAllocations), " +
                "reference \(.allowMultipleAllocations)")
        end'
}

std-print() {
    # Usage: std-print LEVEL
    #
    # Print each saved reference device of LEVEL after our device of the same
    # name and level, one device per line.
    jq -r -n --arg level "$1" \
       --slurpfile ours "$std_dir/ours.json" --slurpfile std "$std_dir/std-$1.json" '
        def line: "\(.name): " +
            ([.attributes | to_entries[] | "\(.key)=\(.value[] | tojson)"] +
             [.capacity | to_entries[] | "capacity \(.key)=\(.value.value)"] +
             ["allowMultipleAllocations=\(.allowMultipleAllocations)"] | join(" "));
        ($ours[0] | map(select(.attributes["level"].string == $level)) |
            INDEX(.name)) as $ours |
        $std[0][] |
        (if $ours[.name] then $ours[.name] | line else "\(.name): no device of ours" end),
        line'
}

# Another $std_driver driver would publish devices next to the reference
# driver. The test leaves it and its devices alone. A leftover reference
# release would hide such a driver. Without the release, any $std_driver
# slice belongs to another driver. The cleanup deletes the same slices.
# Helm 4 has no -a. The status flags list the releases in any status, with
# Helm 3 and 4.
std_releases=$(vm-command-q "helm list -n kube-system -q --filter '^$std_release\$' \
                                 --deployed --failed --pending --superseded \
                                 --uninstalled --uninstalling") ||
    error "failed to list the helm releases in kube-system"
[ -z "$std_releases" ] ||
    error "$std_release is installed in kube-system: uninstall it first"
other_slices=$(vm-command-q "kubectl get resourceslices -o name \
                                 --field-selector spec.driver=$std_driver") ||
    error "failed to list the $std_driver slices"
[ -z "$other_slices" ] ||
    error "another $std_driver driver has slices: $other_slices"

cleanup
trap cleanup EXIT
# host-command would parse the path again. Any $(...) in it would run.
{ rm -rf "$std_dir" && mkdir -p "$std_dir"; } ||
    error "failed to create $std_dir"

helm_config=$(RESERVED_CPU="cpuset:$reserved" DRA_ENABLED=true instantiate helm-config.yaml) \
    helm-launch topology-aware

wait-dra-devices

k8s-feature-enabled DRAConsumableCapacity ||
    { cleanup; trap - EXIT; skip-test "DRAConsumableCapacity is not enabled"; }

std-list-tags
std-resolve-tag
std-clone
if [ "$std_tag" == latest ]; then
    std-check-lag
else
    echo "reference driver: lag not checked, tag $std_tag given by hand"
fi

# Save our devices before any claim exists. Claims lower their capacities.
dra-devices > "$std_dir/ours.json"
vm-command "kubectl get resourceslices -o yaml"

# Both drivers are NRI plugins. Ours stops before the reference driver runs.
cleanup
host-command "$SCP -r \"$std_dir/repo/$std_chart\" $VM_HOSTNAME:$std_vm_chart" ||
    error "failed to copy $std_chart to the VM"

# The reference driver lists the PCIe roots only if the API server accepts
# list attributes.
std_pcie=false
k8s-feature-enabled DRAListTypeAttributes && std_pcie=true

differences=0
for level in numanode socket; do
    std-run $level
    dra-devices $std_driver > "$std_dir/std-$level.json"
    vm-command "kubectl get resourceslices -o yaml"
    echo "devices of level $level, ours and then the reference:"
    std-print $level
    found=$(std-diff $level) ||
        error "failed to compare the devices of level $level"
    if [ -n "$found" ]; then
        echo "differences of level $level:"
        echo "$found"
        differences=$((differences + $(wc -l <<< "$found")))
    fi
done

[ "$differences" == 0 ] ||
    error "$differences differences of the reference devices from ours"
echo "OK: our devices match the devices of tag $std_run_tag of commit $std_commit by NUMA node and by socket"
