The goal is to add basic DRA support to the balloons policy. This task
includes but is not limited to following steps.

1. Design new option(s) to the balloons policy allowing a user
   (typically Kubernetes cluster admin) to configure selected balloon
   type(s) so that resources in every balloon instance of the type are
   published as a DRA ResourceSlice from the node. It may be useful if
   the configuration allows specifying the name (or a template for
   generating a name from balloon instance) for the DRA device that
   should be published. Keep the configuration option design simple
   and aligned with existing options.

2. Design how Kubernetes native resources (especially CPU) should be
   handled in co-operation by DRA, kubelet and the balloons
   policy. Assume most recent Kubernetes version v1.37 with any
   necessary feature gates added. It is important to ensure that
   traditional container resource requests (asking only native CPUs)
   can co-exist with containers that directly request DRA resources
   published from balloons configured with new configuration
   options. Study different options. If it simplifies the design
   considerably to limit "all DRA containers in DRA balloons, and no
   other containers", then we should accept this limitation. It may
   even turn out to be the only option.

3. Use the Opus model with careful thinking to implement DRA claim
   allocation and container resource allocation in the balloons
   policy. If a container requests DRA resources published by the
   balloons policy, then those resources specify the balloon type of
   the container. If there are any contradictions: pod annotation
   tries to force different balloon type for the container, or the
   container requests DRA resources of more than one balloon type or
   instance, creating the container must fail with appropriate error
   message. Create only minimal unit tests.

4. Use the Opus model with careful thinking to implement e2e tests for
   publishing balloons (or balloon instances) as DRA resources,
   creating containers that request these DRA resources, and mixing
   them with traditional containers that run in old-style non-DRA
   balloons.

Workflow:
- Write plan snapshots, findings and other notes under files in
  .plan/5jW-fable-dra-balloons/. These files should be detailed
  enough that AI design can be continued based on them if this
  session happens nto fail at any point. Use simple, clear but precise
  language.

References for design:
- For DRA design principles, read latest Kubernetes DRA documentation.
  For details, study Kubernetes source code in
  /home/akervine/github.com/kubernetes/kubernetes. You can switch
  branches in this working directory to find exact state of a file in
  v1.37 or v1.38-alpha(s) or the master branch.
- Read the most recent state of nri-plugins DRA support in pkg/resmgr
  implemented by latest commits by Ed Bartosh.
- Read rudimentary (place-holder-like) DRA support in
  cmd/plugins/template policy and its e2e test in test/e2e/policies.test-suite/template.

References for implementation and testing:
- Opus should read all skills under .github/skills/ for coding style,
  building images and running e2e tests.
