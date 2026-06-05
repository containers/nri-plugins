# Balloons + Priority Core Turbo (assoc-only) example

This example demonstrates how to use the balloons policy to associate
container CPUs to *pre-configured* Intel Speed Select Technology -
Core Power (SST-CP) classes of service (CLOSes), so that some
containers run on **High Priority (HP)** cores that reach maximum
turbo frequency while others run on **Low Priority (LP)** cores that
are capped at base. This is the "assoc-only" PCT mode: the operator
(or BIOS) owns the SST-CP configuration; balloons only associates
container CPUs to the chosen CLOSes and does not reconfigure SST-CP.

For background on the feature, see the
[Intel(R) Xeon(R) 6 with Priority Core Turbo Technical
Brief](https://www.intel.com/content/www/us/en/products/docs/processors/xeon/6-priority-core-turbo-brief.html),
the [PCT section of the balloons policy
documentation](../balloons.md#priority-core-turbo-pct), and the
[Intel Speed Select kernel
documentation](https://docs.kernel.org/admin-guide/pm/intel-speed-select.html).

The full session below is meant to be copy-pasted into a bash prompt
on a workstation that has `kubectl` configured to talk to a single
target node. Commands that must run *on the node itself* are marked
with `# node:`.

## What you will see

Four HP pods and one LP pod running the same benchmark image, on
the same node, in balloons that pin them to different SST-CP
CLOSes. The HP balloons spread across separate SST power domains
(punits), so each gets its own SST-TF turbo budget. Each pod
prints, once per `sysbench cpu` iteration, the CPUs it is pinned
to, the sysbench thread count, sysbench events/s and the average
`Bzy_MHz` (APERF/MPERF-derived effective frequency, sampled by
`turbostat` from inside the pod) across the pinned CPUs:

```text
[hp-1] cpus=<...> threads=<N> events_per_sec=<...> mhz_avg=<HP MHz>
[hp-2] cpus=<...> threads=<N> events_per_sec=<...> mhz_avg=<HP MHz>
[hp-3] cpus=<...> threads=<N> events_per_sec=<...> mhz_avg=<HP MHz>
[hp-4] cpus=<...> threads=<N> events_per_sec=<...> mhz_avg=<HP MHz>
[lp]   cpus=<...> threads=<N> events_per_sec=<...> mhz_avg=<LP MHz>
```

With PCT in effect, `mhz_avg` and per-thread `events_per_sec` are
visibly higher in the HP pods than in the LP pod.

## 1. Prerequisites

Hardware and platform:

- A server with Intel(R) Xeon(R) 6 CPUs that support SST-PP and SST-CP.
  This example was written against a dual-socket Xeon 6776P.
- SST-PP and SST-CP enabled on the platform (see step 2).
- A Linux kernel with the `isst_if_*` (or `isst_tpmi_*`) modules
  loaded. Modern distro kernels include them.

Kubernetes:

- A working cluster. All commands target a single node; on a
  multi-node cluster, schedule the demo pods on the PCT-capable node
  (e.g. with `nodeSelector` or by tainting other nodes).
- Container runtime: containerd 1.7+ or CRI-O 1.26+ with NRI
  enabled (the default in current versions).
- The balloons policy installed with PCT enabled (see step 5).

Optional tools used in this example:

- `intel-speed-select`. Most Linux distributions package it as part
  of `linux-tools` or `intel-speed-select`; otherwise build it from
  the Linux source tree under
  `tools/power/x86/intel-speed-select/` (see the upstream
  [documentation](https://docs.kernel.org/admin-guide/pm/intel-speed-select.html)).
  Used only to configure and inspect SST-CP. Configuration via BIOS
  is an alternative (see step 2.1).
- `turbostat`. The benchmark image already includes it (from the
  Debian `linux-cpupower` package) and the demo pods use it to
  report `Bzy_MHz` from inside the container. You only need
  `turbostat` on the *node* if you want to cross-check the demo
  numbers from outside the pod; in that case install it from your
  distro's kernel-tools package.
- `crictl` and `ctr` (containerd) or `podman` (CRI-O) on the node
  for loading the benchmark image without a registry.

## 2. Prepare the node

### 2.1. Clear stale `cpufreq` caps

The Linux `cpufreq` subsystem caps each CPU at the lower of its
per-CPU `scaling_max_freq` and the SST-CP CLOS max. If a previous
workload (e.g. another resource policy, an earlier balloons run
with a `maxFreq:` cpuClass, or a manual `cpupower frequency-set`)
left `scaling_max_freq` below the hardware maximum on some CPUs,
those CPUs will stay capped even after they are associated to
CLOS 0. Reset every CPU's per-CPU `scaling_min_freq` and
`scaling_max_freq` to the hardware limits before starting the
demo:

```bash
# node:
for f in /sys/devices/system/cpu/cpu*/cpufreq/scaling_max_freq; do
    base=${f%scaling_max_freq}cpuinfo_max_freq
    sudo tee "$f" < "$base" > /dev/null
done
for f in /sys/devices/system/cpu/cpu*/cpufreq/scaling_min_freq; do
    base=${f%scaling_min_freq}cpuinfo_min_freq
    sudo tee "$f" < "$base" > /dev/null
done

# Verify (should print exactly two lines: the hardware min and max
# in kHz, e.g. "800000" and "4600000" on Xeon 6776P).
for i in $(seq 0 $(($(nproc) - 1))); do
    cat /sys/devices/system/cpu/cpu$i/cpufreq/scaling_max_freq \
        /sys/devices/system/cpu/cpu$i/cpufreq/scaling_min_freq
done | sort -u
```

The cpuClasses below (step 5) deliberately leave `minFreq` /
`maxFreq` unset, so balloons will not write to these files; once
reset they stay at the hardware limits and the SST-CP CLOS bounds
become the effective frequency caps. This is also what the Linux
SST documentation recommends: *"Once associated, avoid changing
Linux cpufreq subsystem scaling frequency limits."*

### 2.2. Enable SST-TF and SST-CP

In assoc-only mode the balloons policy does **not** enable SST
features or program CLOS frequency bounds. Those must be in place
*before* deploying pods. With SST-TF enabled in ordered priority
mode the CLOS frequency bounds come from the SST-TF buckets
themselves (CLOS 0 = the bucket-0 HP turbo limit; CLOS 3 = the LP
clip frequency), so no manual `core-power config` is needed.

`intel-speed-select turbo-freq enable --auto` enables, on every
punit that contains at least one of the CPUs passed via `-c`:

- SST-TF (so HP cores can exceed the standard turbo-ratio bucket
  limit),
- SST-CP with `priority-type:ordered`,
- the initial CPU-to-CLOS association (the passed CPUs -> CLOS 0,
  every other CPU on the punit -> CLOS 3).

The balloons policy overwrites the CPU-to-CLOS associations at pod
admission time, but it does **not** enable SST-TF or SST-CP for
you, so the initial designation must cover every punit you plan to
run HP pods on. Pick one CPU per punit on the node:

```bash
# node:
# Discover punits and one representative CPU each. The "sst" tool
# (https://github.com/intel/intel-speed-select) prints this cleanly;
# you can also read it from goresctrl debug or from sysfs.
sudo ./sst info | awk '/SST-PP/,/SST-BF/' | grep -E '^\s+[0-9]'
# Output on a dual-socket Xeon 6776P:
#   0    0      0-31,128-159
#   0    1      32-63,160-191
#   1    0      64-95,192-223
#   1    1      96-127,224-255

# Pick one CPU from each of the four punits, then:
export TF_INIT_CPUS=2,34,66,98
sudo intel-speed-select -c $TF_INIT_CPUS turbo-freq enable -a
```

### 2.3. Configure SST-TF from BIOS (alternative)

Many OEM BIOSes for Intel Xeon 6 expose SST-PP profile selection
and SST-TF enablement directly in Setup. If your platform supports
it, do the equivalent of the `turbo-freq enable -a` command from
BIOS and skip the `intel-speed-select` step. Consult your server
vendor's BIOS guide for the exact menu paths.

### 2.4. Verify

```bash
# node:
# SST-TF status on every punit (should print "enabled" for every
# punit you intend to host HP pods on).
sudo intel-speed-select perf-profile info 2>&1 \
    | grep -E 'package-|powerdomain-|speed-select-turbo-freq:'

# Per-CPU CLOS association (initial; balloons will overwrite later).
sudo intel-speed-select -c 0,2,34,66,98 core-power get-assoc 2>&1 \
    | grep -E 'cpu-|clos:'
```

`get-assoc` should show `clos:0` for the CPUs in `$TF_INIT_CPUS`
and `clos:3` for every other CPU (including CPU 0, even though
its punit received an HP designation, because CPU 0 itself was
not in `$TF_INIT_CPUS`).

## 3. Build the benchmark image

The benchmark image runs `sysbench cpu` in a loop and prints one
status line per iteration. The effective frequency is measured with
`turbostat --cpu` over the same time window as the `sysbench` run,
restricted to the CPUs the container is pinned to.

`turbostat` is used instead of `scaling_cur_freq` /
`/proc/cpuinfo`'s `cpu MHz` because the latter reflect what the OS
*requests* from the firmware; on HWP/`intel_pstate` kernels they
can lag or under-report when the firmware boosts autonomously.
`Bzy_MHz` is derived from the `APERF`/`MPERF` MSRs over the
sampling window and is the actual *busy* frequency the cores ran
at.

Reading those MSRs requires access to `/dev/cpu/*/msr` and
`CAP_SYS_RAWIO`. In a standard Kubernetes cluster the simplest way
to get both is to run the benchmark pod as `privileged: true` with
the host `/dev` mounted. The pod yaml in step 6 does that. Make
sure the `msr` kernel module is loaded on the node:

```bash
# node:
sudo modprobe msr
ls /dev/cpu/0/msr   # must exist
```

Create the build context:

```bash
mkdir -p pct-reporter && cd pct-reporter

cat > reporter.sh <<'EOF'
#!/bin/bash
# Continuously run sysbench cpu and report, per iteration:
#   label, cpus the container is pinned to (from /proc/self/status,
#   which is correct even when running as privileged), thread count,
#   sysbench events/s, and the average Bzy_MHz across the pinned
#   CPUs as measured by turbostat over the same interval.
set -u
LABEL="${LABEL:-reporter}"
INTERVAL="${INTERVAL:-5}"

CPUS_LIST="$(awk '/Cpus_allowed_list/ {print $2}' /proc/self/status)"

expand_count() {
    local list="$1" n=0 part lo hi
    IFS="," read -ra parts <<< "$list"
    for part in "${parts[@]}"; do
        if [[ "$part" == *-* ]]; then
            lo="${part%-*}"; hi="${part#*-}"
            n=$(( n + hi - lo + 1 ))
        else
            n=$(( n + 1 ))
        fi
    done
    echo "$n"
}
# Default: one sysbench thread per pinned logical CPU. Override
# with THREADS env (used by the A/B pod in step 8).
NTHREADS="${THREADS:-$(expand_count "$CPUS_LIST")}"

echo "[$LABEL] starting: cpus=$CPUS_LIST threads=$NTHREADS interval=${INTERVAL}s"

while true; do
    TS_OUT="$(mktemp)"
    turbostat --quiet --cpu "$CPUS_LIST" --show CPU,Bzy_MHz \
              --num_iterations 1 --interval "$INTERVAL" \
              > "$TS_OUT" 2>/dev/null &
    TS_PID=$!

    SB_OUT="$(sysbench cpu --threads="$NTHREADS" --time="$INTERVAL" \
              run 2>/dev/null)"
    wait "$TS_PID"

    EVPS="$(echo "$SB_OUT" | awk -F: '/events per second/ {gsub(/ /,"",$2); print $2}')"
    # Average Bzy_MHz across the requested CPUs. Skip header and
    # turbostat's "-" all-CPUs summary row.
    MHZ_AVG="$(awk 'NR>1 && $1 ~ /^[0-9]+$/ {s+=$2; n++} END {if (n) printf "%.0f", s/n}' "$TS_OUT")"
    rm -f "$TS_OUT"

    printf '[%s] cpus=%s threads=%d events_per_sec=%s mhz_avg=%s\n' \
        "$LABEL" "$CPUS_LIST" "$NTHREADS" "${EVPS:-?}" "${MHZ_AVG:-?}"
done
EOF
chmod +x reporter.sh

cat > Dockerfile <<'EOF'
FROM debian:stable-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        sysbench linux-cpupower util-linux ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY reporter.sh /usr/local/bin/reporter.sh
ENTRYPOINT ["/usr/local/bin/reporter.sh"]
EOF
```

`linux-cpupower` ships `/usr/sbin/turbostat`. `util-linux` provides
`taskset` and the rest of the standard userspace.

Build the image. Use whichever tool is available on your build host.
With docker, prefix with `sudo` if your user is not in the `docker`
group:

```bash
# With docker:
docker build -t localhost/pct-reporter:demo .

# Or with podman:
podman build -t localhost/pct-reporter:demo .
```

If the build host is behind an HTTP proxy, pass it through:

```bash
docker build \
    --build-arg http_proxy=$http_proxy \
    --build-arg https_proxy=$https_proxy \
    -t localhost/pct-reporter:demo .
```

## 4. Make the image available to the kubelet (no registry)

If you built the image on the same machine as the kubelet, import it
directly into the container runtime's image store. Pick the
subsection that matches your runtime.

### 4.1. containerd

```bash
# On the build host:
docker save localhost/pct-reporter:demo -o /tmp/pct-reporter.tar
# (or: podman save -o /tmp/pct-reporter.tar localhost/pct-reporter:demo)

# node:
sudo ctr -n k8s.io images import /tmp/pct-reporter.tar
sudo crictl images | grep pct-reporter
```

The `-n k8s.io` namespace is the one kubelet uses; without it the
image will not be visible to Kubernetes.

### 4.2. CRI-O

```bash
# On the build host:
docker save localhost/pct-reporter:demo -o /tmp/pct-reporter.tar
# (or: podman save -o /tmp/pct-reporter.tar localhost/pct-reporter:demo)

# node:
sudo podman --root /var/lib/containers/storage load -i /tmp/pct-reporter.tar
sudo crictl images | grep pct-reporter
```

`--root /var/lib/containers/storage` makes `podman` load the image
into the same storage CRI-O reads from. If you built the image
directly on the node with `sudo podman build`, this step is not
needed.

The demo pods set `imagePullPolicy: IfNotPresent` and use the image
reference `localhost/pct-reporter:demo`, so the kubelet will not
attempt to pull from a registry. Note that the kubelet garbage-
collects unused local images: re-import the image if pod creation
later fails with `ErrImagePull`.

## 5. Install / reconfigure the balloons policy with PCT enabled

```bash
helm install nri-resource-policy-balloons nri-plugins/nri-resource-policy-balloons --namespace kube-system --set allowPCT=true
```

`--set allowPCT=true` makes the plugin pod privileged and mounts the
host `/dev` at `/host/dev`. Enable it only on nodes where PCT
cpuClasses are used.

Verify the plugin pod has the privileged settings the chart's
`allowPCT=true` flag enables:

```bash
kubectl -n kube-system get pod \
    -l app.kubernetes.io/name=nri-resource-policy-balloons \
    -o jsonpath='{.items[0].spec.containers[0].securityContext}{"\n"}'
# Expect: {"privileged":true}

kubectl -n kube-system get pod \
    -l app.kubernetes.io/name=nri-resource-policy-balloons \
    -o jsonpath='{.items[0].spec.containers[0].volumeMounts[?(@.name=="hostdev")]}{"\n"}'
# Expect a mount of /host/dev.
```

Now apply the policy configuration. The `BalloonsPolicy` below
defines three cpuClasses with only `sstClosID` set (no
`pctPriority`, no frequency overrides), which selects assoc-only
mode for PCT and lets the SST-CP CLOS bounds -- set by the
`intel-speed-select turbo-freq enable -a` recipe in step 2 -- define
the actual frequency caps. Following the Linux SST guidance, the
cpuClasses do not touch `minFreq` / `maxFreq` at all.

The CLOS layout matches what `turbo-freq enable -a` programs in
ordered priority mode:

- CLOS 0 -- HP -- bucket-0 turbo (Pmax),
- CLOS 3 -- LP -- LP clip (= base on this platform),
- the class named `default` is the implicit fallback for idle CPUs
  and balloons that do not specify their `cpuClass`. It is mapped
  to the LP CLOS so idle CPUs do not consume HP turbo budget.

The HP cpuClass additionally disables the deep C-states `C6` and
`C6P`. The HP cores in this demo are continuously busy with
`sysbench`, so C-state entry would normally not happen anyway;
the setting is included because removing C-state wake-up latency
is the typical reason latency-sensitive workloads ask for priority
cores. List the C-state names available on the node with
`grep . /sys/devices/system/cpu/cpu0/cpuidle/state*/name`. **Do
not** disable C-states on the default / LP classes: idle CPUs in
deep C-states do not count toward the package's active-core count
and therefore free turbo budget for the HP cores.

The HP balloon type uses `preferNewBalloons: true` and
`maxCPUs: 8` (the SST-TF bucket-0 HP-core limit per punit), so
each HP pod lands in its own balloon and the balloons spread
across separate punits. `minCPUs` is intentionally left unset so
the balloon size equals what the pod requests; with no
`hideHyperthreads` the container sees exactly the logical CPUs the
balloon allocated.

`agent.nodeResourceTopology: true` and `showContainersInNrt: true`
make the plugin publish per-balloon and per-container CPU sets in
the cluster's `NodeResourceTopology` (NRT) CRs. The verification
queries in step 7 read those CRs to confirm exactly which CPUs
each pod's container ended up pinned to. The NRT CRD must exist
in the cluster (`kubectl get crd
noderesourcetopologies.topology.node.k8s.io`).

`availableResources` is intentionally left unset: balloons manages
all CPUs of the node, as in the normal mode of operation. The
`reservedResources` covers physical CPU 0 (`0` and its SMT sibling
`128`) and physical CPU 1 (`1` and its SMT sibling `129`); adjust
the sibling numbers if your topology differs (`lscpu -e` shows
them).

```bash
cat > balloons-pct-assoconly.yaml <<EOF
apiVersion: config.nri/v1alpha1
kind: BalloonsPolicy
metadata:
  name: default
  namespace: kube-system
spec:
  agent:
    nodeResourceTopology: true
  reservedResources:
    cpu: cpuset:0,1,128,129
  pinCPU: true
  showContainersInNrt: true

  balloonTypes:
  - name: reserved
    # uses the implicit "default" cpuClass below
  - name: hp-bln
    cpuClass: hp-clos0
    maxCPUs: 8
    preferNewBalloons: true
    preferSpreadingPods: false
  - name: lp-bln
    cpuClass: lp-clos3
    preferSpreadingPods: false

  cpuClasses:
  - name: default
    sstClosID: 3
  - name: hp-clos0
    sstClosID: 0
    disabledCstates: [C6, C6P]
  - name: lp-clos3
    sstClosID: 3

  log:
    debug:
    - policy
    - cpu
EOF

kubectl apply -f balloons-pct-assoconly.yaml
```

Confirm that balloons picked up the configuration and entered
assoc-only mode -- you should see `mode=assoc-only` and (after pods
are deployed) `associated cpus ... to CLOS N` lines, but **no**
`PrepareManagedMode done` nor `EnableCP done`:

```bash
kubectl -n kube-system logs ds/nri-resource-policy-balloons \
    | grep -E 'pct(:| mock:)' | tail -n 20
```

Expected:

```text
pct: SST discovered: pkg=0 punit=0 level=1 cpus=<...> ...
pct: mode=assoc-only, 3 PCT cpuClass(es), 4 punit(s) across 2 package(s)
pct: assoc-only: CLOS 0 programmed min=0 max=<ceiling> kHz
pct: assoc-only: CLOS 3 programmed min=0 max=<ceiling> kHz
pct: cpuClass "hp-clos0" classified HP (assoc-only: CLOS 0 ...)
```

The `assoc-only: CLOS N programmed` lines record permissive
(min=0, max=hardware ceiling) bounds that the plugin writes when
entering assoc-only mode; they leave the SST-CP CLOS bounds that
`turbo-freq enable -a` programmed in step 2 unchanged in practice,
because the effective frequency is the minimum of the per-CLOS
cap and the SST-TF bucket-0 limit. The plugin only classifies one
cpuClass per priority bucket on the same CLOS, so when both
`default` and `lp-clos3` use CLOS 3 only one of them is reported in
the classification log.

If any punit you intend to host HP pods on shows up with an
`assoc-only: SST-TF disabled on pkg=N punit=M` warning, repeat
step 2 with a CPU from that punit included in `$TF_INIT_CPUS`.

## 6. Deploy the HP and LP pods

Four HP pods and one LP pod. Each HP pod requests 2 CPUs; with
`preferNewBalloons: true` and `maxCPUs: 8` on `hp-bln`, each pod
gets its own balloon, and PCT placement spreads the balloons
across separate punits (one per HP pod, up to four on a
dual-socket Xeon 6776P). Because `hideHyperthreads` is not set,
the container sees exactly the requested logical CPUs and the
reporter starts that many sysbench threads.

The pods are `privileged: true` and mount the host `/dev` because
`turbostat` inside the container reads `/dev/cpu/*/msr` to compute
`Bzy_MHz` (see step 3).

```bash
for i in 1 2 3 4; do
cat > pod-hp-$i.yaml <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: pct-hp-$i
  annotations:
    balloon.balloons.resource-policy.nri.io: hp-bln
spec:
  restartPolicy: Never
  containers:
  - name: bench
    image: localhost/pct-reporter:demo
    imagePullPolicy: IfNotPresent
    env:
    - name: LABEL
      value: "hp-$i"
    - name: INTERVAL
      value: "5"
    securityContext:
      privileged: true
    volumeMounts:
    - name: hostdev
      mountPath: /dev
    resources:
      requests: { cpu: "2", memory: "128Mi" }
      limits:   { cpu: "2", memory: "128Mi" }
  volumes:
  - name: hostdev
    hostPath: { path: /dev, type: Directory }
EOF
done

cat > pod-lp.yaml <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: pct-lp
  annotations:
    balloon.balloons.resource-policy.nri.io: lp-bln
spec:
  restartPolicy: Never
  containers:
  - name: bench
    image: localhost/pct-reporter:demo
    imagePullPolicy: IfNotPresent
    env:
    - name: LABEL
      value: "lp"
    - name: INTERVAL
      value: "5"
    securityContext:
      privileged: true
    volumeMounts:
    - name: hostdev
      mountPath: /dev
    resources:
      requests: { cpu: "8", memory: "128Mi" }
      limits:   { cpu: "8", memory: "128Mi" }
  volumes:
  - name: hostdev
    hostPath: { path: /dev, type: Directory }
EOF

kubectl apply -f pod-hp-1.yaml -f pod-hp-2.yaml -f pod-hp-3.yaml -f pod-hp-4.yaml -f pod-lp.yaml
kubectl wait --for=condition=Ready --timeout=60s \
    pod/pct-hp-1 pod/pct-hp-2 pod/pct-hp-3 pod/pct-hp-4 pod/pct-lp
```

## 7. Observe

Tail every pod's log:

```bash
for p in pct-hp-1 pct-hp-2 pct-hp-3 pct-hp-4 pct-lp; do
    kubectl logs -f --prefix=true --max-log-requests=5 $p &
done
wait
```

Sample shape on a dual-socket Intel(R) Xeon(R) 6776P (replace
`<...>` with your own measurements):

```text
[hp-1] cpus=32,160 threads=2 events_per_sec=4154.72 mhz_avg=4600
[hp-2] cpus=100,228 threads=2 events_per_sec=4152.06 mhz_avg=4600
[hp-3] cpus=10,138 threads=2 events_per_sec=4154.54 mhz_avg=4600
[hp-4] cpus=64,192 threads=2 events_per_sec=4151.79 mhz_avg=4600
[lp]   cpus=65-68,193-196 threads=8 events_per_sec=8295.63 mhz_avg=2300
```

Per-thread throughput on this run:

| Tag    | threads | mhz_avg | events_per_sec | events_per_sec per thread |
|--------|---------|---------|----------------|---------------------------|
| hp-1   | 2       | 4600    | 4154.72        | 2077.36                  |
| hp-2   | 2       | 4600    | 4152.06        | 2076.03                  |
| hp-3   | 2       | 4600    | 4154.54        | 2077.27                  |
| hp-4   | 2       | 4600    | 4151.79        | 2075.89                  |
| lp     | 8       | 2300    | 8295.63        | 1036.95                  |

Verify that the four HP balloons landed on four distinct punits.
With the policy's `cpu` debug log enabled, balloons logs the
(pkg, punit) of each balloon at admission time. You can also map
the `cpus` line of each HP pod back to a punit through the
`sst info` output from step 2 -- each HP pod's CPUs should fall
into a different punit row.

Optionally cross-check the same numbers from outside the pod with
`turbostat` on the node:

```bash
# node:
# Replace the CPU list with the union of cpus= reported by the
# five pods.
sudo turbostat --show CPU,Bzy_MHz --quiet -c <cpu-list> -i 2 -n 2
```

The pod-reported `mhz_avg` and the node-side `Bzy_MHz` come from
the same source (APERF/MPERF), so they should agree to within a
few MHz.

Verify the CLOS association of the pinned CPUs:

```bash
# node:
sudo intel-speed-select -c <cpu-list> core-power get-assoc 2>&1 \
    | grep -E 'cpu-|clos:'
```

Expected: `clos:0` for every CPU in any HP pod, `clos:3` for every
CPU in the LP pod.

Confirm the policy decision from its log:

```bash
kubectl -n kube-system logs ds/nri-resource-policy-balloons \
    | grep -E 'assigning container|associated cpus .* to CLOS'
```

### 7.1. Verify container-to-balloon-to-CPU mapping via NRT

The `agent.nodeResourceTopology: true` and `showContainersInNrt:
true` settings in step 5 make the plugin publish per-balloon and
per-container CPU sets in the
`noderesourcetopologies.topology.node.k8s.io` CR for the node.
Print every balloon (zone type `balloon`) with its CPU set, and
every container assigned to it (zone type `allocation for
container`):

```bash
kubectl get noderesourcetopologies.topology.node.k8s.io -o json | jq -r '
  ["NODE","BALLOON","CPUSET"],
  (
    .items.[] as $node
    | $node.zones[]
    | select(.type == "balloon")
    | [
        $node.metadata.name,
        .name,
        (.attributes[] | select(.name=="cpuset") | .value)
      ]
  ) | @tsv'

kubectl get noderesourcetopologies.topology.node.k8s.io -o json | jq -r '
  ["NODE","BALLOON","CONTAINER","CPUS"],
  (
    .items.[] as $node
    | $node.zones[]
    | select(.type == "allocation for container")
    | [
        $node.metadata.name,
        .parent,
        .name,
        (.attributes[] | select(.name=="cpuset") | .value)
      ]
  ) | @tsv'
```

Expected (one row per balloon and one row per pod's container):

- One `hp-bln[0]`..`hp-bln[3]` zone, each with a 2-CPU set on a
  distinct punit, and the corresponding `pct-hp-N/bench` container
  pinned to that exact set.
- One `lp-bln[0]` zone with the 8-CPU set, and `pct-lp/bench`
  pinned to the same set.
- A `reserved[0]` zone covering the currently-used subset of the
  reserved pool (the SMT pair of physical CPU 0 -- `0,128` -- is the
  typical outcome on this layout; balloons compacts the reserved
  balloon to what its containers actually need).
- An empty `default[0]` zone may also appear; it is the unused
  default balloon and can be ignored.

The CPU sets here must match the `cpus=` value printed by the
benchmark inside each pod (step 7) and the `clos:N` reported by
`core-power get-assoc` for those same CPUs.

## 8. A/B comparison

Run the same 2-thread workload on the LP CLOS instead of an HP
CLOS. The pod below pins to the LP balloon (CLOS 3, base
frequency cap) and uses `THREADS=2` to keep the sysbench workload
identical to a single `pct-hp-*`:

```bash
kubectl delete pod pct-lp --now    # free LP-balloon CPUs

cat > pod-hp-on-lp.yaml <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: pct-hp-on-lp
  annotations:
    balloon.balloons.resource-policy.nri.io: lp-bln
spec:
  restartPolicy: Never
  containers:
  - name: bench
    image: localhost/pct-reporter:demo
    imagePullPolicy: IfNotPresent
    env:
    - name: LABEL
      value: "hp-on-lp"
    - name: THREADS
      value: "2"
    - name: INTERVAL
      value: "5"
    securityContext:
      privileged: true
    volumeMounts:
    - name: hostdev
      mountPath: /dev
    resources:
      requests: { cpu: "2", memory: "128Mi" }
      limits:   { cpu: "2", memory: "128Mi" }
  volumes:
  - name: hostdev
    hostPath: { path: /dev, type: Directory }
EOF

kubectl apply -f pod-hp-on-lp.yaml
kubectl logs -f pct-hp-on-lp
```

Sample shape (replace with your own measurements):

```text
[hp-on-lp] cpus=65,193 threads=2 events_per_sec=2074.27 mhz_avg=2300
```

Per-thread `events_per_sec` should drop from the HP value to
roughly LP base / HP turbo x HP value -- the same ratio as the
per-CPU frequency ratio reported by `mhz_avg`. This is the
headline number aligned with the PCT brief: priority cores let
the same code finish more work per unit time because they run at
a higher frequency.

Record your own numbers:

| Tag         | threads | mhz_avg (MHz) | events_per_sec | events_per_sec per thread |
|-------------|---------|---------------|----------------|---------------------------|
| `hp-1`      | 2       | 4600          | 4154.72        | 2077.36                  |
| `hp-2`      | 2       | 4600          | 4152.06        | 2076.03                  |
| `hp-3`      | 2       | 4600          | 4154.54        | 2077.27                  |
| `hp-4`      | 2       | 4600          | 4151.79        | 2075.89                  |
| `lp`        | 8       | 2300          | 8295.63        | 1036.95                  |
| `hp-on-lp`  | 2       | 2300          | 2074.27        | 1037.13                  |

## 9. Cleanup

Reset the cluster, then the host, back to a defined initial state.

### 9.1. Kubernetes side

```bash
kubectl delete -f pod-hp-1.yaml -f pod-hp-2.yaml -f pod-hp-3.yaml \
    -f pod-hp-4.yaml -f pod-lp.yaml -f pod-hp-on-lp.yaml --ignore-not-found
kubectl delete -f balloons-pct-assoconly.yaml --ignore-not-found
```

Deleting the `BalloonsPolicy` CR is the policy's defined "reset"
trigger: the plugin reacts to losing its effective configuration
by removing every `cpuclass.balloons.nri.io/*` extended resource
it had published. Verify before uninstalling the chart:

```bash
kubectl get node -o jsonpath='{.items[0].status.capacity}' \
    | jq 'with_entries(select(.key | startswith("cpuclass.balloons.nri.io/")))'
# Expect: {}
```

Then uninstall the chart:

```bash
helm uninstall balloons -n kube-system
```

Uninstalling the chart triggers the plugin's graceful shutdown,
but in assoc-only mode the plugin does **not** touch SST-CP /
SST-TF state on the host (that is the whole point of assoc-only
mode), so SST stays in whatever state step 2 left it. The next
two sub-steps complete the reset.

### 9.2. Restore SST defaults on the node

```bash
# node:
sudo intel-speed-select turbo-freq disable -a
sudo intel-speed-select core-power disable

# Verify:
sudo intel-speed-select core-power info 2>&1 \
    | grep -E 'enable-status' | sort -u
# Expect (both lines):
#   clos-enable-status:disabled
#   enable-status:disabled

sudo intel-speed-select perf-profile info 2>&1 \
    | grep -E 'speed-select-turbo-freq:' | sort -u
# Expect: speed-select-turbo-freq:disabled
```

### 9.3. Restore `cpufreq` defaults on the node

```bash
# node:
for f in /sys/devices/system/cpu/cpu*/cpufreq/scaling_max_freq; do
    base=${f%scaling_max_freq}cpuinfo_max_freq
    sudo tee "$f" < "$base" > /dev/null
done
for f in /sys/devices/system/cpu/cpu*/cpufreq/scaling_min_freq; do
    base=${f%scaling_min_freq}cpuinfo_min_freq
    sudo tee "$f" < "$base" > /dev/null
done

# Verify (should print exactly the hardware min and the hardware
# max in kHz):
for i in $(seq 0 $(($(nproc) - 1))); do
    cat /sys/devices/system/cpu/cpu$i/cpufreq/scaling_max_freq \
        /sys/devices/system/cpu/cpu$i/cpufreq/scaling_min_freq
done | sort -u
```

### 9.4. Remove leftover files

```bash
rm -f balloons-pct-assoconly.yaml \
      pod-hp-1.yaml pod-hp-2.yaml pod-hp-3.yaml pod-hp-4.yaml \
      pod-lp.yaml pod-hp-on-lp.yaml
# Optional:
rm -rf pct-reporter
# Optional, on the node, free disk used by the demo image:
# sudo crictl rmi localhost/pct-reporter:demo
```

## 10. Optional: help the scheduler avoid HP over-subscription (experimental)

By default the Kubernetes scheduler is unaware of how many CPUs
on a node can become HP cores: it sees the BalloonsPolicy
neither as a CRD it understands nor as a resource it can
bin-pack on. Two HP pods can therefore land on the same node
even if a second node would have given them HP capacity, and
HP pods can pile up beyond the platform's actual HP budget.

The balloons policy ships an experimental opt-in that publishes
a per-cpuClass extended resource on the local Node so that the
default scheduler can do that bin-packing for you. Set
`publishExtendedResource: true` on every PCT-enabled cpuClass
(i.e. classes that carry `sstClosID` or `pctPriority`) and the
agent advertises:

```text
status.capacity:
  cpuclass.balloons.nri.io/<class-name>: <free logical CPUs>
```

The capacity reflects "CPUs eligible for this class that are
not currently held by balloons of other classes", and is
re-published on every container create/update/release, so
cross-class consumption (e.g. an LP balloon eating CPUs that
would otherwise have been available for HP) is reflected
immediately.

For HP classes, the per-punit cap used in the capacity
formula is the *guaranteed top-turbo HP CPU count* (the
smallest non-zero SST-TF bucket `HighPriorityCoreCount`, or
the SST-BF `HighPriorityCPUs` count when TF is unsupported)
-- not the larger `MaxHpCpus`. That is the number of HP CPUs
per punit that can simultaneously sustain the highest turbo
frequency this platform exposes, which is the right figure
for the scheduler to bin-pack on. In assoc-only mode a punit
contributes to HP capacity only when SST-TF is currently
enabled on it (the operator's responsibility -- typically via
`intel-speed-select ... turbo-freq enable -a`); a punit where
SST-TF is disabled cannot exceed the standard turbo-ratio
bucket frequency and contributes `0`, so the scheduler will
not bin-pack HP pods onto nodes that cannot deliver top
turbo. Same-class consumption inside HP is intentionally not
subtracted (an admitted HP pod does not shrink the published
HP capacity); only cross-class consumption is. LP capacity
equals `|Allowed \ held|`.

Add the flag to the policy:

```yaml
  cpuClasses:
  - name: hp-clos0
    sstClosID: 0
    disabledCstates: [C6, C6P]
    publishExtendedResource: true   # experimental
  - name: lp-clos3
    sstClosID: 3
    publishExtendedResource: true   # experimental
```

...and to every HP/LP pod, alongside the existing `cpu`
request:

```yaml
    resources:
      requests:
        cpu: "2"
        memory: "128Mi"
        cpuclass.balloons.nri.io/hp-clos0: "2"
      limits:
        cpu: "2"
        memory: "128Mi"
        cpuclass.balloons.nri.io/hp-clos0: "2"
```

Verify on the node after applying:

```bash
kubectl get node -o jsonpath='{.items[0].status.capacity}' \
    | jq 'with_entries(select(.key | startswith("cpuclass")))'
# Expect (HP capacity = sum_punit GuaranteedHpCpus over
# SST-TF-enabled punits; LP capacity = |Allowed \ held|):
# {
#   "cpuclass.balloons.nri.io/hp-clos0": "<HP capacity>",
#   "cpuclass.balloons.nri.io/lp-clos3": "<free CPUs>"
# }
```

A pod whose request exceeds the published capacity gets
`FailedScheduling: Insufficient cpuclass.balloons.nri.io/<name>`
and stays `Pending` until another pod releases the resource.

This is an experimental flag: the resource name, semantics
(capacity vs. allocatable, conservative-on-grow), and update
cadence may change before becoming stable.

## 11. Troubleshooting

- Plugin pod log shows `Speed Select Technology (SST) support not
  detected`: the pod cannot access `/dev/isst_interface`. Re-install
  the chart with `--set allowPCT=true`. Verify with `kubectl -n
  kube-system get pod -l app.kubernetes.io/name=nri-resource-policy-balloons
  -o jsonpath='{.items[0].spec.containers[0].securityContext}'`
  that it shows `privileged:true`.
- Plugin log shows `pct: assoc-only: SST-TF disabled on pkg=N
  punit=M`: that punit has SST-TF off, so HP cores on it cannot
  exceed the standard turbo-ratio bucket frequency even when
  associated to CLOS 0. Add a CPU from that punit to
  `$TF_INIT_CPUS` in step 2 and rerun `intel-speed-select -c
  $TF_INIT_CPUS turbo-freq enable -a`.
- `intel-speed-select --info` reports SST-TF as *not supported*: the
  `isst_if_*` or `isst_tpmi_*` kernel modules may be missing; load
  them or use a more recent distro kernel. On some platforms SST
  features must be enabled in BIOS first.
- Pods stuck in `ErrImagePull` with image
  `localhost/pct-reporter:demo`: the image was not imported into
  the kubelet's container runtime store, or the kubelet has
  garbage-collected it. Repeat step 4, then `kubectl delete pod
  ...` to retry.
- Pod log shows `turbostat: no /dev/cpu/0/msr` or `mhz_avg=?`: the
  `msr` kernel module is not loaded on the node. Run `sudo modprobe
  msr` on the node and recreate the pod. If the pod is not
  privileged or `/dev` is not mounted, fix the pod yaml (step 6).
- HP CPUs do not reach Pmax under load: another HP pod on the same
  punit may be consuming the bucket-0 turbo budget. Verify the
  per-punit HP CPU count stays within the SST-TF bucket-0 limit
  (8 on this platform; check with `sst info`) and that each HP
  balloon really ended up on a different punit (see step 7).
  Cross-check with `turbostat --show CPU,Bzy_MHz`.
- All four HP balloons end up on the same punit: confirm
  `preferNewBalloons: true` on `hp-bln` and that the plugin
  build includes PCT-aware balloon placement. The plugin log
  prints the punit each new balloon is assigned to.
- Validation error `pctPriority and sstClosID are mutually
  exclusive`: only one of the two may be set on a cpuClass. For
  assoc-only mode use `sstClosID` and leave `pctPriority` unset.
