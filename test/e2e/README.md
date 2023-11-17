# NRI-resource-policy E2E tests

## Prerequisites
Before running E2E tests ensure that you have all the required components locally available as described below.

0. Install dependencies:
   - `vagrant`
   - `qemu-system-x86`

1. Build NRI resource policy static binaries. You need to be at the root of the NRI-resource-policy directory.

    ```shell
    make build
    ```

2. Build the container image. You need to be at the root of the NRI-resource-policy directory.

    ```shell
    make images
    ```

3. Build containerd binaries that include NRI support (minimum tag version [`containerd 1.7.0-beta.1`](https://github.com/containerd/containerd/releases/tag/v1.7.0-beta.1) or +)

    ```shell
    git clone https://github.com/containerd/containerd.git
    cd containerd
    make
    ```

4. Then run the tests.

    ```shell
    cd test/e2e
    ./run_tests.sh policies.test-suite
    ```

    The default test output directory name is generated from the used topology
    and runtime name and the directory is created under test/e2e.
    The test output directory can be given as a 2nd paramter to the script.

    ```shell
    ./run_tests.sh policies.test-suite ~/output-directory
    ```

    Note that Vagrant VM is stored into the output directory. If you want to
    remove the output directory, then please remove the Vagrant VM first like
    this:

    ```shell
    cd ~/output-directory && vagrant destroy -f
    ```

    The e2e test runs can be can be configured by setting various options as
    environment variables when starting the run_tests.sh script. These environent
    variables are passed to test VM so that it can establish connection to
    net to download packages etc.

    ```
    HTTP_PROXY
    HTTPS_PROXY
    NO_PROXY
    http_proxy
    https_proxy
    no_proxy
    dns_nameserver
    dns_search_domain
    k8scri
    ```

    For example:

    ```shell
    https_proxy=http://proxy.example.com dns_nameserver=8.8.8.8 dns_search_domain=example.com ./run_tests.sh policies.test-suite
    ```

5. You can login to the e2e test VM:

    ```shell
    cd ~/output-directory
    make ssh
    ```

6. While the e2e tests are running, you can monitor the status of the tests:

    ```shell
    cd ~/output-directory
    <nri-plugins-root-directory>/test/e2e/report-test-status.sh

    policies.test-suite balloons test01-basic-placement : PASS
    policies.test-suite balloons test02-prometheus-metrics : PASS
    policies.test-suite balloons test03-reserved : PASS
    policies.test-suite balloons test05-namespace : PASS
    policies.test-suite balloons test06-update-config : PASS
    policies.test-suite balloons test07-maxballoons : PASS
    policies.test-suite balloons test08-numa : PASS
    policies.test-suite balloons test09-isolated : PASS
    policies.test-suite balloons test10-health-checking : PASS
    ```
