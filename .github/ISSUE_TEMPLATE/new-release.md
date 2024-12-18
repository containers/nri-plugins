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
      # You can do this with the in-repo artifact verification script:
      ./scripts/release/check-artifacts.sh --images $VERSION
      ```
  - [ ] Finalize the new *draft* release created by CI
    - [ ] Check that all artefacts (Helm charts) were uploaded
    - [ ] Write the change log to the release.
    - [ ] Get the change log OK'd by other maintainers.
  - [ ] Publish the draft as a release.
  - [ ] Verify that the Helm repo was updated
    - ```
      # You can do this with the in-repo artifact verification script:
      ./scripts/release/check-artifacts.sh --charts $VERSION
      # Or to do a final verification of both images and charts:
      ./scripts/release/check-artifacts.sh $VERSION
      ```
  - [ ] Add a link to the tagged release in this issue.
  - [ ] Generate the operator bundle by running make bundle within `deployment/operator` directory and submit the generated content to the [community-operators](https://github.com/k8s-operatorhub/community-operators).
- [ ] Create and push unannotated development tag `X.Y.0-devel` for next release cycle.
- [ ] Close this issue.
