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
  - [ ] Update deployment files
    - [ ] run `scripts/hack/release-helper.sh`
    - [ ] commit changes, submit as a PR to GitHub
    - [ ] wait for the PR to be merged
  - [ ] Run `git tag -a -m "NRI plugins $VERSION" $VERSION`.
- Publishing
  - [ ] Push the tag with `git push $VERSION`.
  - [ ] Check that new container images are published for the tag.
    - ```
      for i in nri-resource-policy-topology-aware nri-resource-policy-balloons nri-resource-policy-template nri-config-manager nri-memory-qos nri-memtierd; do \
          skopeo inspect --format "$i: {{.Digest}}" docker://ghcr.io/containers/nri-plugins/$i:$VERSION;
      done
      ```
  - [ ] Finalize the new *draft* release created by CI
    - [ ] Check that all artefacts (Helm charts) were uploaded
    - [ ] Write the change log to the release.
    - [ ] Get the change log OK'd by other maintainers.
  - [ ] Publish the draft as a release.
  - [ ] Verify that the Helm repo was updated
  - [ ] Add a link to the tagged release in this issue.
- [ ] Close this issue.
