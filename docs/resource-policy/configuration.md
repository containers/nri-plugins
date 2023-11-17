# Dynamic Configuration

NRI Resource Policy plugin can be configured dynamically using plugin-specific
custom resources.

The plugin daemon monitors two custom resources the node, a primary node-specific
one and a secondary group-specific or default one, depending on whether the node
belongs to a configuration group. The node-specific custom resource always takes
precedence over the others.

The names of these custom resources are

1. `node.$NODE_NAME`: primary, node-specific configuration
2. `group.$GROUP_NAME`: secondary group-specific node configuration
3. `default`: secondary: secondary default node configuration

You can assign a node to a configuration group by setting the
`group.config.nri` label on the node to the name of the configuration
group. You can remove a node from its group by deleting the node group
label.

There are [sample configuration](tree:/sample-configs/) custom resources that
contain contains a node-specific, a group-specific, and a default configuration.
See [any available policy-specific documentation](policy/index.md)
for more information on the policy configurations.
