# This test uses the simplest template policy to verify functionality of the e2e test
# framework itself.

cleanup-test --helm --cache
helm_config=$(DRA_ENABLED='' instantiate helm-config.yaml) \
    helm-launch template

# Verify that the framework can validate placement of pods with non-standard names.
NAME=non-standard-pod-name create besteffort
verify \
    'len(cpus["non-standard-pod-namec0"]) != 0'

cleanup-test --helm --cache
