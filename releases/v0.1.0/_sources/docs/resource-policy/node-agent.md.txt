# Dynamic Configuration

NRI Resource Policy plugin can be configured dynamically using ConfigMaps.

The plugin daemon monitors two ConfigMaps for the node, a primary node-specific one
and a secondary group-specific or default one, depending on whether the node
belongs to a configuration group. The node-specific ConfigMap always takes
precedence over the others.

The names of these ConfigMaps are

1. `nri-resource-policy-config.node.$NODE_NAME`: primary, node-specific configuration
2. `nri-resource-policy-config.group.$GROUP_NAME`: secondary group-specific node
    configuration
3. `nri-resource-policy-config.default`: secondary: secondary default node
    configuration

You can assign a node to a configuration group by setting the
`resource-policy.nri.io/group` label on the node to the name of
the configuration group. You can remove a node from its group by deleting
the node group label.

There is a
[sample ConfigMap spec](/sample-configs/nri-resource-policy-configmap.example.yaml)
that contains a node-specific, a group-specific, and a default ConfigMap
example. See [any available policy-specific documentation](policy/index.rst)
for more information on the policy configurations.
