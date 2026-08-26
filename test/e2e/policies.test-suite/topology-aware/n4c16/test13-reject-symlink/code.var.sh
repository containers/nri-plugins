# This test verifies that nri-resource-policy properly refuses to start if a cache
# file exists but it is a symbolic link.

cache="/var/lib/nri-resource-policy/cache"

symlink_cache() {
    vm-command "mv $cache $cache.real && ln -s $cache.real $cache"
}

restore_cache() {
    if vm-command-q "[ -L $cache ]"; then
        vm-command "rm -f $cache && mv $cache.real $cache"
    fi
}

# Make sure we have a cache.

helm-terminate
helm_config=$(instantiate helm-config.yaml)
helm-launch topology-aware
helm-terminate topology-aware

# Turn cache into a symlink. Restore it whatever happens, otherwise the
# symlink is left behind for the tests which run after this one.
trap restore_cache EXIT
symlink_cache

# Try to re-launch nri-resource-policy, check whether and how it fails.
expect-launch-failure topology-aware

vm-command "kubectl -n kube-system logs ds/nri-resource-policy-topology-aware"
vm-command "kubectl -n kube-system logs ds/nri-resource-policy-topology-aware | \
    grep -q 'exists, but is a symbolic link'" ||
    error "nri-resource-policy failed to start, but looks like for the wrong reason..."

restore_cache
trap - EXIT

helm-terminate

echo "OK: nri-resource-policy properly rejected symlinked cache"
