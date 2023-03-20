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
nri_resource_policy_cfg=$(instantiate nri-resource-policy.cfg)

terminate nri-resource-policy
launch nri-resource-policy
terminate nri-resource-policy

# Turn cache into a symlink.
symlink_cache

# Try to re-launch nri-resource-policy, check whether and how it fails.
(
  trap 'restore_cache' 0
  if (wait_t=5 ds_wait_t=none launch nri-resource-policy); then
      exit 1
  else
      if ! vm-command "kubectl -n kube-system logs daemonset.apps/nri-resource-policy | \
          grep -q 'exists, but is a symbolic link'"; then
          exit 2
      else
          exit 0
      fi
  fi
)
status=$?

terminate nri-resource-policy

# Check and report test status.
case "$status" in
    1) error "ERROR: nri-resource-policy expected to reject symlinked cache, but it did not.";;
    2) error "ERROR: nri-resource-policy failed to start, but looks like for the wrong reason...";;
    0) echo "OK: nri-resource-policy properly rejected symlinked cache"; return 0;;
    *) error "ERROR: test failed with unexpected status.";;
esac
