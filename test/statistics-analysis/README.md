# Statistics analysis tool

This file contains instructions on using the tool.

## How to use

0. In order to save container runtime logs, add yourself to systemd-journal
   group and make sure you are in that group when running the script (use "id" command)

1. Install [helm](https://helm.sh/) for using Prometheus chart.
   * In Fedora: "dnf install helm"

2. Install Python plotting libraries.
```console
pip3 install -r requirements.txt
```

### Running the scripts together

3. Run the script, for example:

```console
template=~/nri-plugins/deployment/helm/template topology_aware=~/nri-plugins/deployment/helm/topology-aware topology_aware_overrides="--set image.name=localhost/nri-resource-policy-topology-aware --set image.tag=v0.4.0" balloons=~/nri-plugins/deployment/helm/balloons balloons_overrides="--set image.name=localhost/nri-resource-policy-balloons --set image.tag=v0.4.0"./scripts/run-tests.sh
```

4. Generate graphs with `plot-graphs.py`. If you use labels `baseline`, `template`, `topology-aware`, and `balloons` you can use the `post-run.sh` script.

5. Remove all files from the output directory to not have overlapping labels (filenames).

### Running the scripts individually

3. Configure cluster to desired state.

4. Run the `pre-run.sh` script. This deploys Jaeger and Prometheus. Example:

```console
./scripts/pre-run.sh
```

```console
usage: ./scripts/pre-run.sh -p <use prometheus: "true" or "false">
```

5. Wait for the Jaeger and Prometheus pods to be ready.

6. Run the test with `run-test.sh`. Example:

```console
./scripts/run-test.sh -n 10 -i 9 -l baseline
```

```console
usage: ./scripts/run-test.sh
    -n <number of stress-ng containers in increment>
    -i <increments>
    -l <filename label>
    -s <time to sleep waiting to query results>
```

7. To remove installed resources, run `destroy-deployment.sh`.

8. Repeat steps 1-5 for each desired setup and **label each setup with different labels that are not substrings of each other**.

9. Generate graphs with `plot-graphs.py`. If you use labels `baseline`, `template`, `topology-aware`, and `balloons` you can use the `post-run.sh` script.

10. Remove all files from the output directory to not have overlapping labels (filenames).

## How to setup tracing

# In containerd

* https://github.com/containerd/containerd/blob/main/docs/tracing.md
   * enable tracing in the containerd config
```toml
...
[plugins."io.containerd.internal.v1.tracing"]
   sampling_ratio = 1.0
...
[plugins."io.containerd.tracing.processor.v1.otlp"]
   endpoint = "http://127.0.0.1:30318"
...
```
* Use port 30318 instead of 4318 when configuring the container runtime for otlp with http
* Use port 30317 instead of 4317 when configuring the container runtime for otlp with grpc

# In crio

```console
mkdir -p /etc/crio/crio.conf.d
cat > /etc/crio/crio.conf.d/10-enable-tracing.conf <<EOF
[crio.tracing]
enable_tracing = true
tracing_endpoint = "127.0.0.1:30317"
tracing_sampling_rate_per_million = 1000000
EOF
systemctl restart crio
```

See also [`crio tracing`](https://github.com/cri-o/cri-o/blob/main/docs/crio.conf.5.md#criotracing-table) for more details.
