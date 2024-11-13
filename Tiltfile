# -*- mode: Python -*-

# Extensions 
load('ext://helm_resource', 'helm_resource', 'helm_repo')

# Specifies that Tilt is allowed to run against the specified k8s context name. 
allow_k8s_contexts('kubernetes-admin@kubernetes')
# Anonymous, ephemeral image registry.
default_registry('ttl.sh')

# Run a defined set of services via config.set_enabled_resources.
config.define_string_list("to-run", args=True)
cfg = config.parse()
groups = {
    'balloons': ['binary-build-logs', 'controller-logs'],
    'topology-aware': ['binary-build-logs', 'controller-logs'],
}

resources = []
for resource_name in cfg.get('to-run', []):
    if resource_name in groups:
        resources += groups[resource_name]
config.set_enabled_resources(resources)

# Fail if the policy name is not provided.
if len(resources) == 0:
    fail("🚨 ERROR: No policy passed! Please run: tilt up <POLICY_NAME>")

POLICY = resource_name
IMAGE_BASE = "ttl.sh/ghcr.io/containers/nri-plugins/nri-resource-policy-"
IMAGE = IMAGE_BASE + POLICY
COMPILE_CMD = ('make BINARIES= OTHER_IMAGE_TARGETS= PLUGINS=nri-resource-policy-' + POLICY + ' build-plugins')
DEPS = ['./pkg', './cmd', './deployment']

###################################
# The main tasks are executed here.
###################################

# Builds a binary.
local_resource(
    'binary-build-logs',
    COMPILE_CMD,
    deps=DEPS
)

# Builds a container image.
docker_build(
    IMAGE,
    '.',
    dockerfile='./cmd/plugins/' + POLICY + '/Dockerfile'
)

# Deploy the policy Helm chart.
helm_resource(
    'controller-logs',
    './deployment/helm/' + POLICY,
    namespace='kube-system',
    deps=DEPS,
    image_deps=[IMAGE],
    image_keys=[('image.registry', 'image.repository', 'image.tag')],
    flags=['--set=config.instrumentation.prometheusExport=true','--set=ports[0].name=metrics','--set=ports[0].container=8891','--set=image.name=' + IMAGE],
    resource_deps=['binary-build-logs']
)
