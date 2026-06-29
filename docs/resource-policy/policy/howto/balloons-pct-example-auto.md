# Balloons + Priority Core Turbo (managed) example

This example demonstrates how to let the balloons policy **own** the
Intel Speed Select Technology - Core Power (SST-CP) and Speed
Select Technology - Turbo Frequency (SST-TF) configuration on a
node, so that some containers run on **High Priority (HP)** cores
that reach maximum turbo frequency while others run on **Low
Priority (LP)** cores that are capped at base. This is the
"managed" PCT mode: the operator configures cpuClasses with
`pctPriority: high` and `pctPriority: low`, and the balloons
plugin programs the corresponding SST-CP CLOSes, enables SST-TF,
and associates container CPUs to the right CLOS at admission time.

A companion document,
[balloons-pct-example-manual.md](balloons-pct-example-manual.md),
walks through the same demo with the "assoc-only" PCT mode in
which the operator owns the SST configuration and balloons only
associates CPUs. The two documents share build steps and pod
YAMLs; the differences are concentrated in the BalloonsPolicy
(step 4) and the inspection step (step 6).

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
the same node, in balloons that pin them to SST-CP CLOSes
programmed by the balloons policy itself. The HP balloons spread
across separate SST power domains (punits), so each gets its own
SST-TF turbo budget. Each pod prints, once per `sysbench cpu`
iteration, the CPUs it is pinned to, the sysbench thread count,
sysbench events/s and the average `Bzy_MHz` (APERF/MPERF-derived
effective frequency, sampled by `turbostat` from inside the pod)
across the pinned CPUs:

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

- A server with Intel(R) Xeon(R) 6 CPUs that support SST-PP, SST-CP
  and SST-TF. This example was written against a dual-socket Xeon
  6776P.
- SST features enabled at the platform level (SST-PP profile
  selected so that SST-TF is available; on most platforms this is
  the default). The balloons policy will turn SST-CP and SST-TF
  on at runtime, but it does not select an SST-PP profile.
- A Linux kernel with the `isst_if_*` (or `isst_tpmi_*`) modules
  loaded. Modern distro kernels include them.
- The `msr` kernel module loaded on the node so the in-pod
  `turbostat` can read APERF/MPERF (`sudo modprobe msr`; see
  step 2).

Kubernetes:

- A working cluster. All commands target a single node; on a
  multi-node cluster, schedule the demo pods on the PCT-capable node
  (e.g. with `nodeSelector` or by tainting other nodes).
- Container runtime: containerd 1.7+ or CRI-O 1.26+ with NRI
  enabled (the default in current versions).
- The balloons policy installed with PCT enabled (see step 4).

Optional tools used in this example:

- `intel-speed-select` on the node, **only for inspection**
  (step 6). In managed mode the balloons policy programs SST-CP
  and SST-TF for you; you do not need to invoke
  `intel-speed-select` to *configure* anything. Most Linux
  distributions package it as part of `linux-tools` or
  `intel-speed-select`; otherwise build it from the Linux source
  tree under `tools/power/x86/intel-speed-select/` (see the
  upstream [documentation](https://docs.kernel.org/admin-guide/pm/intel-speed-select.html)).
- `turbostat`. The benchmark image already includes it (from the
  Debian `linux-cpupower` package) and the demo pods use it to
  report `Bzy_MHz` from inside the container. You only need
  `turbostat` on the *node* if you want to cross-check the demo
  numbers from outside the pod.
- `crictl` and `ctr` (containerd) or `podman` (CRI-O) on the node
  for loading the benchmark image without a registry.

> **No manual SST step.** Unlike the
> [assoc-only example](balloons-pct-example-manual.md), there is
> no `intel-speed-select turbo-freq enable -a` step here.
> Programming SST-CP CLOS bounds, enabling SST-CP in ordered
> priority mode, and enabling SST-TF on every package are all
> done by the balloons policy when it processes the cpuClasses in
> step 4. Pre-configuring SST in BIOS or via `intel-speed-select`
> is still compatible; the balloons policy resets and reprograms
> SST when it enters managed mode.

## 2. Build the benchmark image

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
the host `/dev` mounted. The pod yaml in step 5 does that. Make
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
# with THREADS env (used by the A/B pod in step 7).
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

## 3. Make the image available to the kubelet (no registry)

If you built the image on the same machine as the kubelet, import it
directly into the container runtime's image store. Pick the
subsection that matches your runtime.

### 3.1. containerd

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

### 3.2. CRI-O

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

## 4. Install balloons with PCT enabled

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
defines three cpuClasses. Two of them (`hp-pct`, `lp-pct`) use
`pctPriority` -- this is what selects **managed** mode for the
PCT allocator:

- `hp-pct` requests `pctPriority: high`. balloons assigns it to
  CLOS 0, programs that CLOS with min frequency `base` and max
  frequency `turbo` (which resolves to the hardware maximum turbo
  frequency on this SKU, 4600 MHz on Xeon 6776P), and enables
  SST-TF on every package so the bucket-0 turbo budget becomes
  available.
- `lp-pct` requests `pctPriority: low`. balloons assigns it to
  CLOS 3 and programs that CLOS with min frequency `min` and max
  frequency `base`, so LP cores are capped at base while idle LP
  cores still drop to Pmin (freeing turbo budget for HP cores).
- `default` has no PCT fields. It is the implicit fallback for
  idle CPUs and balloons that do not specify their `cpuClass`.
  In managed mode, when an LP class is defined, balloons routes
  these CPUs to the LP CLOS automatically (logged as `pct:
  fallback CLOS for non-PCT CPUs set to N (LP)`). This is
  essential: leaving idle CPUs on the HP CLOS would inflate the
  SST-TF active-HP-core count per punit and prevent bucket-0
  turbo selection on punits that also host an LP balloon.

`pctMinFreq` / `pctMaxFreq` accept the same symbolic names as
`minFreq` / `maxFreq` (`min`, `base`, `turbo`) and also explicit
values like `3.2GHz`. In managed mode, `turbo` resolves directly
to the hardware turbo maximum (not subject to `turboPriority`
arbitration).

The HP cpuClass additionally disables the deep C-states `C6` and
`C6P`. The HP cores in this demo are continuously busy with
`sysbench`, so C-state entry would normally not happen anyway;
the setting is included because removing C-state wake-up latency
is the typical reason latency-sensitive workloads ask for priority
cores. List the C-state names available on the node with
`grep . /sys/devices/system/cpu/cpu0/cpuidle/state*/name`. **Do
not** disable C-states on the `default` / `lp-pct` classes: idle
CPUs in deep C-states do not count toward the package's active-
core count and therefore free turbo budget for the HP cores.

The HP balloon type uses `preferNewBalloons: true` and
`maxCPUs: 8` (the SST-TF bucket-0 HP-core limit per punit on
Xeon 6776P), so each HP pod lands in its own balloon and the
balloons spread across separate punits. `minCPUs` is left unset
so the balloon size equals what the pod requests; with no
`hideHyperthreads` the container sees exactly the logical CPUs
the balloon allocated.

`agent.nodeResourceTopology: true` and `showContainersInNrt: true`
make the plugin publish per-balloon and per-container CPU sets in
the cluster's `NodeResourceTopology` (NRT) CRs. The verification
queries in step 6 read those CRs to confirm exactly which CPUs
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
cat > balloons-pct-managed.yaml <<EOF
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
    cpuClass: hp-pct
    maxCPUs: 8
    preferNewBalloons: true
    preferSpreadingPods: false
  - name: lp-bln
    cpuClass: lp-pct
    preferSpreadingPods: false

  cpuClasses:
  - name: default
    # no PCT fields => idle/default CPUs follow the fallback CLOS
  - name: hp-pct
    pctPriority: high
    pctMinFreq: base
    pctMaxFreq: turbo
    disabledCstates: [C6, C6P]
  - name: lp-pct
    pctPriority: low
    pctMinFreq: min
    pctMaxFreq: base

  log:
    debug:
    - policy
    - cpu
EOF

kubectl apply -f balloons-pct-managed.yaml
```

Confirm that balloons picked up the configuration and entered
managed mode -- you should see `mode=managed`, `programmed CLOS N`
lines for every CLOS used by a PCT cpuClass, `PrepareManagedMode
done` (which resets SST-CP, enables SST-TF and sets ordered
priority), and `EnableCP done`:

```bash
kubectl -n kube-system logs ds/nri-resource-policy-balloons \
    | grep -E 'pct(:| mock:)' | tail -n 20
```

Expected:

```text
pct: SST discovered: pkg=0 punit=0 level=1 cpus=<...> ...
pct: mode=managed, 2 PCT cpuClass(es), 4 punit(s) across 2 package(s)
pct: programmed CLOS 0 min=2300000 max=4600000 kHz
pct: programmed CLOS 3 min=800000 max=2300000 kHz
pct: cpuClass "hp-pct" classified HP (managed: pctPriority=high, CLOS 0)
pct: cpuClass "lp-pct" classified LP (managed: pctPriority=low, CLOS 3)
pct: fallback CLOS for non-PCT CPUs set to 3 (LP)
```

## 5. Deploy the HP and LP pods

Four HP pods and one LP pod. Each HP pod requests 2 CPUs; with
`preferNewBalloons: true` and `maxCPUs: 8` on `hp-bln`, each pod
gets its own balloon, and PCT placement spreads the balloons
across separate punits (one per HP pod, up to four on a
dual-socket Xeon 6776P). Because `hideHyperthreads` is not set,
the container sees exactly the requested logical CPUs and the
reporter starts that many sysbench threads.

The pods are `privileged: true` and mount the host `/dev` because
`turbostat` inside the container reads `/dev/cpu/*/msr` to compute
`Bzy_MHz` (see step 2).

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

## 6. Inspect what balloons configured

This is the section that differs most from the assoc-only example:
in managed mode the *plugin* drives all SST state, so the
inspection focuses on verifying that the on-host SST configuration
matches what the plugin logged.

### 6.1. From the plugin log

```bash
kubectl -n kube-system logs ds/nri-resource-policy-balloons \
    | grep -E 'pct(:| mock:)|associated cpus .* to CLOS' \
    | tail -n 40
```

The interesting lines are:

- `pct: mode=managed, 2 PCT cpuClass(es), 4 punit(s) across 2 package(s)`
- `pct: programmed CLOS 0 min=<base kHz> max=<turbo kHz> kHz`
- `pct: programmed CLOS 3 min=<min kHz> max=<base kHz> kHz`
- `pct: cpuClass "hp-pct" classified HP (managed: pctPriority=high, CLOS 0)`
- `pct: cpuClass "lp-pct" classified LP (managed: pctPriority=low, CLOS 3)`
- one `associated cpus <...> to CLOS 0` per HP pod admitted
- one `associated cpus <...> to CLOS 3` per LP pod admitted

### 6.2. From the node with `intel-speed-select`

These commands read SST state directly from the hardware. They
must agree with what the plugin logged. None of them write
anything.

```bash
# node:

# SST-PP profile (must report a level that has TF supported / enabled).
sudo intel-speed-select perf-profile info 2>&1 \
    | grep -E 'current|speed-select-turbo-freq|speed-select-core-power'

# SST-CP state per package (must report enable-status: enabled
# and priority-type: 1 (ordered) -- this is what
# PrepareManagedMode + EnableCP set).
sudo intel-speed-select core-power info 2>&1 \
    | grep -E 'package-|powerdomain-|enable-status|priority-type'

# SST-CP CLOS bounds. CLOS 0 should show max-frequency matching
# the "programmed CLOS 0 max=<...> kHz" line in the plugin log;
# CLOS 3 the corresponding LP cap.
sudo intel-speed-select core-power get-config -c 0 2>&1 \
    | grep -E 'powerdomain-|clos-min|clos-max'
sudo intel-speed-select core-power get-config -c 3 2>&1 \
    | grep -E 'powerdomain-|clos-min|clos-max'

# SST-TF enable state on every punit (plugin called TFEnable in
# PrepareManagedMode).
sudo intel-speed-select perf-profile info 2>&1 \
    | grep -E 'package-|powerdomain-|speed-select-turbo-freq:'
```

### 6.3. Per-CPU associations

```bash
# node:

# Build the list of pinned CPUs from the pods. (Bash expansion
# below assumes a single-container pod; adjust if you changed the
# layout.)
HP_CPUS=$(for p in pct-hp-1 pct-hp-2 pct-hp-3 pct-hp-4; do
    kubectl logs $p 2>/dev/null | awk -F'cpus=| ' '/starting/ {print $4}'
done | paste -sd,)
LP_CPUS=$(kubectl logs pct-lp 2>/dev/null \
    | awk -F'cpus=| ' '/starting/ {print $4}')
echo "HP_CPUS=$HP_CPUS"
echo "LP_CPUS=$LP_CPUS"

# Expected: clos:0 for every CPU in HP_CPUS, clos:3 for every CPU
# in LP_CPUS.
sudo intel-speed-select -c "$HP_CPUS" core-power get-assoc 2>&1 | grep -E 'cpu-|clos:'
sudo intel-speed-select -c "$LP_CPUS" core-power get-assoc 2>&1 | grep -E 'cpu-|clos:'
```

### 6.4. Verify punit spread

The four HP balloons should each land on a different punit. The
mapping from CPU to punit is visible in `sst info`:

```bash
# node:
sudo ./sst info | awk '/SST-PP/,/SST-BF/' | grep -E '^\s+[0-9]'
# Sample on Xeon 6776P:
#   0    0      0-31,128-159
#   0    1      32-63,160-191
#   1    0      64-95,192-223
#   1    1      96-127,224-255
```

The pinned CPUs of `pct-hp-1` .. `pct-hp-4` should each fall in a
different `(pkg, punit)` row.

### 6.5. Verify container-to-balloon-to-CPU mapping via NRT

The `agent.nodeResourceTopology: true` and `showContainersInNrt:
true` settings in step 4 make the plugin publish per-balloon and
per-container CPU sets in the
`noderesourcetopologies.topology.node.k8s.io` CR for the node.
Print every balloon (zone type `balloon`) and every container
(zone type `allocation for container`) assigned to it:

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

Expected:

- Four `hp-bln[0]`..`hp-bln[3]` zones, each with a 2-CPU set on a
  distinct punit, and the matching `pct-hp-N/bench` container
  pinned to that same set.
- One `lp-bln[0]` zone with the 8-CPU set, and `pct-lp/bench`
  pinned to the same set.
- A `reserved[0]` zone covering the currently-used subset of the
  reserved pool (the SMT pair of physical CPU 0 -- `0,128` -- is the
  typical outcome on this layout; balloons compacts the reserved
  balloon to what its containers actually need).
- An empty `default[0]` zone may also appear; it is the unused
  default balloon and can be ignored.

The CPU sets here must match the `cpus=` value printed by the
benchmark inside each pod (step 7), the `clos:0` / `clos:3`
reported by `core-power get-assoc` (step 6.3), and the punit
mapping (step 6.4).

## 7. Observe performance

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
[hp-1] cpus=32,160 threads=2 events_per_sec=4155.16 mhz_avg=4600
[hp-2] cpus=64,192 threads=2 events_per_sec=4153.55 mhz_avg=4600
[hp-3] cpus=100,228 threads=2 events_per_sec=4152.00 mhz_avg=4600
[hp-4] cpus=10,138 threads=2 events_per_sec=4155.50 mhz_avg=4600
[lp]   cpus=65-68,193-196 threads=8 events_per_sec=8296.69 mhz_avg=2138
```

Per-thread throughput on this run:

| Tag    | threads | mhz_avg | events_per_sec | events_per_sec per thread |
|--------|---------|---------|----------------|---------------------------|
| hp-1   | 2       | 4600    | 4155.16        | 2077.58                  |
| hp-2   | 2       | 4600    | 4153.55        | 2076.78                  |
| hp-3   | 2       | 4600    | 4152.00        | 2076.00                  |
| hp-4   | 2       | 4600    | 4155.50        | 2077.75                  |
| lp     | 8       | 2138    | 8296.69        | 1037.09                  |

Optionally cross-check the same numbers from outside the pod with
`turbostat` on the node:

```bash
# node:
sudo turbostat --show CPU,Bzy_MHz --quiet -c <cpu-list> -i 2 -n 2
```

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
[hp-on-lp] cpus=65,193 threads=2 events_per_sec=2075.61 mhz_avg=2300
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
| `hp-1`      | 2       | 4600          | 4155.16        | 2077.58                  |
| `hp-2`      | 2       | 4600          | 4153.55        | 2076.78                  |
| `hp-3`      | 2       | 4600          | 4152.00        | 2076.00                  |
| `hp-4`      | 2       | 4600          | 4155.50        | 2077.75                  |
| `lp`        | 8       | 2138          | 8296.69        | 1037.09                  |
| `hp-on-lp`  | 2       | 2300          | 2075.61        | 1037.81                  |

## 9. Cleanup

### 9.1. Kubernetes side

```bash
kubectl delete -f pod-hp-1.yaml -f pod-hp-2.yaml -f pod-hp-3.yaml \
    -f pod-hp-4.yaml -f pod-lp.yaml -f pod-hp-on-lp.yaml --ignore-not-found
kubectl delete -f balloons-pct-managed.yaml --ignore-not-found
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
helm uninstall nri-resource-policy-balloons -n kube-system
```

### 9.2. Restore SST defaults on the node

In managed mode the plugin's `Shutdown()` will, on a graceful
exit, run `CPReset -> TFDisable -> CPDisable` per package and
return the platform to its initial SST state. In practice
`helm uninstall` may not give the daemonset enough termination
grace for that hook to complete, so always verify and, if SST
is still enabled, run the teardown explicitly:

```bash
# node:
sudo intel-speed-select core-power info 2>&1 \
    | grep -E 'enable-status' | sort -u
sudo intel-speed-select perf-profile info 2>&1 \
    | grep -E 'speed-select-turbo-freq:' | sort -u

# If any value above is "enabled", reset:
sudo intel-speed-select turbo-freq disable -a
sudo intel-speed-select core-power disable

# Re-verify (both expected to be disabled):
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

The managed-mode plugin does not write `scaling_min_freq` /
`scaling_max_freq`, but earlier workloads or kernel modules might
have. Reset them to the hardware limits as a precaution:

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
rm -f balloons-pct-managed.yaml \
      pod-hp-1.yaml pod-hp-2.yaml pod-hp-3.yaml pod-hp-4.yaml \
      pod-lp.yaml pod-hp-on-lp.yaml
# Optional:
rm -rf pct-reporter
# Optional, on the node, free disk used by the demo image:
# sudo crictl rmi localhost/pct-reporter:demo
```

## 10. Optional: help the scheduler avoid HP over-subscription (experimental)

The default Kubernetes scheduler is unaware of how many CPUs on
a node can become HP cores. Two HP pods can land on the same
node even when a second node would have given them HP capacity,
and HP pods can pile up beyond the platform's HP budget.

The balloons policy ships an experimental opt-in that publishes
a per-cpuClass extended resource on the local Node so the
default scheduler can bin-pack on it. Set
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

For managed-mode HP classes, the per-punit cap used in the
capacity formula is the *guaranteed top-turbo HP CPU count*
(the smallest non-zero SST-TF bucket `HighPriorityCoreCount`,
or the SST-BF `HighPriorityCPUs` count when TF is
unsupported). This is the number of HP CPUs per punit that
can simultaneously sustain the highest turbo frequency this
platform exposes -- not the larger `MaxHpCpus` the allocator
uses internally. On a Xeon 6 with four 8-core SST-TF buckets
per punit and four active punits, that is 4 x 8 = 32 HP CPUs
of guaranteed top-turbo headroom, which is what the scheduler
should bin-pack on.

Add the flag to the policy:

```yaml
  cpuClasses:
  - name: hp-pct
    pctPriority: high
    pctMinFreq: base
    pctMaxFreq: turbo
    disabledCstates: [C6, C6P]
    publishExtendedResource: true   # experimental
  - name: lp-pct
    pctPriority: low
    pctMinFreq: min
    pctMaxFreq: base
    publishExtendedResource: true   # experimental
```

...and to every HP/LP pod, alongside the existing `cpu` request:

```yaml
    resources:
      requests:
        cpu: "2"
        memory: "128Mi"
        cpuclass.balloons.nri.io/hp-pct: "2"
      limits:
        cpu: "2"
        memory: "128Mi"
        cpuclass.balloons.nri.io/hp-pct: "2"
```

Verify on the node after applying:

```bash
kubectl get node -o jsonpath='{.items[0].status.capacity}' \
    | jq 'with_entries(select(.key | startswith("cpuclass")))'
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
- Plugin log shows `pct: failed to prepare managed mode` or
  `pct: failed to configure CLOS N`: another agent on the node may
  already hold SST exclusively, or a previous balloons instance
  exited without releasing it. Try
  `sudo intel-speed-select core-power disable` on the node, then
  restart the plugin (`kubectl -n kube-system delete pod -l
  app.kubernetes.io/name=nri-resource-policy-balloons`).
- Validation error `cpuClass "X": pctPriority and sstClosID are
  mutually exclusive`: only one of the two PCT fields may be set
  on a cpuClass. For managed mode use `pctPriority` and leave
  `sstClosID` unset.
- Validation error `at most one managed PCT cpuClass with
  pctPriority=high allowed`: in managed mode balloons programs
  exactly one HP and one LP CLOS, so at most one cpuClass with
  `pctPriority: high` and one with `pctPriority: low` may be
  defined.
- Validation error `pct: cannot mix managed (pctPriority) and
  assoc-only (sstClosID) modes`: the configuration mixes the two
  modes. Pick one and apply it to every PCT cpuClass.
- Pods stuck in `ErrImagePull` with image
  `localhost/pct-reporter:demo`: the image was not imported into
  the kubelet's container runtime store, or the kubelet has
  garbage-collected it. Repeat step 3, then `kubectl delete pod
  ...` to retry.
- Pod log shows `turbostat: no /dev/cpu/0/msr` or `mhz_avg=?`: the
  `msr` kernel module is not loaded on the node. Run `sudo modprobe
  msr` on the node and recreate the pod. If the pod is not
  privileged or `/dev` is not mounted, fix the pod yaml (step 5).
- HP CPUs do not reach Pmax under load: confirm `turbo-freq info`
  reports `enable-status: enabled` on every punit and that the HP
  balloons each landed on a different punit (step 6.4). Two HP
  balloons on the same punit share the bucket-0 turbo budget and
  may run below Pmax.
- All four HP balloons end up on the same punit: confirm
  `preferNewBalloons: true` on `hp-bln` and that the plugin
  build includes PCT-aware balloon placement. The plugin log
  prints the punit each new balloon is assigned to.
- `mhz_avg` for HP equals the standard turbo (not bucket-0 turbo):
  SST-TF did not enable. Check the plugin log for
  `PrepareManagedMode done` and `TFEnable`/`TFDisable` errors, and
  the host SST-PP profile (`intel-speed-select perf-profile info`)
  to confirm the selected SST-PP level lists SST-TF as supported.
