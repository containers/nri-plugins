# Node Agent

CRI Resource Manager can be configured dynamically using the CRI Resource
Manager Node Agent and Kubernetes\* ConfigMaps. The agent is built in the
NRI resource plugin.

The agent monitors two ConfigMaps for the node, a primary node-specific one
and a secondary group-specific or default one, depending on whether the node
belongs to a configuration group. The node-specific ConfigMap always takes
precedence over the others.

The names of these ConfigMaps are

1. `cri-resmgr-config.node.$NODE_NAME`: primary, node-specific configuration
2. `cri-resmgr-config.group.$GROUP_NAME`: secondary group-specific node
    configuration
3. `cri-resmgr-config.default`: secondary: secondary default node
    configuration

You can assign a node to a configuration group by setting the
`cri-resource-manager.intel.com/group` label on the node to the name of
the configuration group. You can remove a node from its group by deleting
the node group label.

There is a
[sample ConfigMap spec](/sample-configs/nri-resmgr-configmap.example.yaml)
that contains a node-specific, a group-specific, and a default ConfigMap
example. See [any available policy-specific documentation](policy/index.rst)
for more information on the policy configurations.
