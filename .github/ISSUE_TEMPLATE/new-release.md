---
name: New release
about: Propose a new major release
title: Release v0.0.0
labels: ''
assignees: ''

---

## Release Process

<!--
If making adjustments to the checklist please also file a PR against this issue
template (.github/ISSUE_TEMPLATE/new-release.md) to incorporate the changes for
future releases.
-->

- [ ] Create and push new release branch
- Local release preparations (on the release branch)
  - [ ] Run e2e tests
  - [ ] Sync/tidy up dependencies.
    - [ ] Run `go mod tidy`.
    - [ ] Run `git commit -m 'go.mod,go.sum: update dependencies.' go.{mod,sum}`, if necessary.
  - [ ] Run `git tag -a -m "NRI plugins $VERSION" $VERSION`.
- Publishing
  - [ ] Push the tag with `git push $VERSION`.
  - [ ] Check that new container images are published for the tag.
    - ```
      for i in nri-resource-policy-topology-aware nri-resource-policy-balloons nri-resource-policy-template nri-config-manager nri-memory-qos nri-memtierd nri-sgx-epc; do
          skopeo inspect --format "$i: {{.Digest}}" docker://ghcr.io/containers/nri-plugins/$i:$VERSION
      done
      # Notes:
      # You can also do an image artifact verification with this repo script:
      ./scripts/release/check-artifacts.sh --images $VERSION
      ```
  - [ ] Finalize the new *draft* release created by CI
    - [ ] Check that all artefacts (Helm charts) were uploaded
    - [ ] Write the change log to the release.
    - [ ] Get the change log OK'd by other maintainers.
  - [ ] Publish the draft as a release.
  - [ ] Verify that the Helm repo was updated
    - ```
      rm -f helm-index
      wget https://containers.github.io/nri-plugins/index.yaml -Ohelm-index
      for i in nri-resource-policy-topology-aware nri-resource-policy-balloons nri-resource-policy-template nri-memory-qos nri-memtierd nri-sgx-epc; do
          if [ $(cat helm-index | yq ".entries.$i[] | select(.version == \"$VERSION\") | length > 0") != "true" ]; then
              echo "FAILED: Helm chart $i:$VERSION NOT FOUND"
          else
              echo "OK: $i"
          fi
      done
      rm helm-index
      # Notes:
      # You can also do an image+helm chart artifact verification with this repo script:
      ./scripts/release/check-artifacts.sh $VERSION
      ```
  - [ ] Add a link to the tagged release in this issue.
  - [ ] Generate the operator bundle by running make bundle within `deployment/operator` directory and submit the generated content to the [community-operators](https://github.com/k8s-operatorhub/community-operators).
- [ ] Create and push unannotated development tag `X.Y.0-devel` for next release cycle.
- [ ] Close this issue.
