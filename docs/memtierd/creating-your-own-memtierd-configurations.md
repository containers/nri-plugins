# Creating your own configurations for Memtierd

When deploying pods that you want to be tracked by Memtierd, you need to specify a configuration that is used to tell Memtierd how to track your pod. This can be done by adding your own configurations to the cmd/memtierd/templates/ directory before building the image for your Memtierd NRI plugin.

## How to create a configuration

Many sample configurations are specified [here](https://github.com/intel/memtierd/tree/main/sample-configs) with comments on what each parameter does.

- Take a configuration from the sample configs, add your own parameters based on your workloads needs and then add that configuration to the templates/ directory.
- When specifying your deployments add the <b>"class.memtierd.nri"</b> annotation to point to the configuration file you want to use. See the templates/example-deployment.yaml for example.

## Allowing the use of new configurations

Just adding the configuration to the templates is not enough. You will also need to modify the StartContainer() function in cmd/memtierd/main.go to make the plugin look for the correct template you just added. There is an example in the code on how to do this easily.

After these steps just make a new Memtierd NRI plugin image and you can use your newly created configuration with your workloads.
