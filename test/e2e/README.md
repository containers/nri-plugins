# NRI-resource-manager E2E tests

## Prerequisites
Before running E2E tests ensure that you have all the required components locally available as described below.

1. Build NRI-RM static binary. You need to be at the root of the NRI-resource-manager directory.

    ```shell
    $ make build
    ```

1. Build the container image. You need to be at the root of the NRI-resource-manager directory.

    ```shell
    $ make images
    ```

1. Build containerd binaries that include NRI support (tag version [`containerd 1.7.0-beta.1`](https://github.com/containerd/containerd/releases/tag/v1.7.0-beta.1) or +)

    ```shell
    $ git clone https://github.com/containerd/containerd.git
    $ cd containerd
    $ make build
    ```

1. Install Vagrant required plugins

    ```shell
    $ vagrant plugin install dotenv
    $ vagrant plugin install vagrant-proxyconf
    $ vagrant plugin install vagrant-qemu
    ```
