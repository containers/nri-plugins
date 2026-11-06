# Quick start: Priority Core Turbo (PCT) in Kubernetes with Balloons

Balloons policy is an NRI plugin to container runtimes, containerd and
CRI-O. The policy creates balloons that associate sets of containers
with sets of CPUs. Containers belonging to a balloon are allowed to
run on the CPUs belonging to the same balloon and not on CPUs
belonging to other balloons. Balloons policy allows different CPU
tunings on different balloons. This guide shows how to run
high-priority containers with maximum turbo frequencies in Kubernetes
by scheduling them on nodes with free PCT capacity, and by tuning
their CPUs.

This short guide shows the minimum required to:

1. install the balloons NRI policy in a Kubernetes cluster,
2. configure it so that nodes with Intel Priority Core Turbo (PCT)
   hardware publish a `cpuclass.balloons.nri.io/hp-pct` extended
   resource the scheduler can bin-pack on, and
3. run two Burstable pods on the same node -- one that asks for HP
   cores and runs at the platform's top turbo frequency, and one
   that does not and runs at the base frequency -- and observe the
   performance difference with one `kubectl exec` per pod.

This guide uses balloons plugin's **managed** PCT mode, that is, the
plugin owns the SST-CP/SST-TF configuration, overriding whatever
existing configuration from BIOS settings and/or the
intel-speed-select tool. Balloons supports also **assoc-only mode**
that uses pre-defined and only associates balloons' CPUs to existing
CLOSes.

This document does not cover manual configuration of underlying SST
technology, step-by-step validation of real CPU frequencies,
CPUs-to-containers mapping, benchmarking. These are covered in longer
balloons PCT examples written separately for
[managed-mode](balloons-pct-example-auto.md) and
[assoc-only-mode](balloons-pct-example-manual.md).


## Prerequisites

- A Kubernetes cluster (1.27 or newer) with NRI enabled in every
  node's container runtime (containerd >= 1.7 or CRI-O >= 1.26).
- At least one node that supports Intel SST-CP and SST-TF, e.g.
  Intel Xeon 6700P/6900P. Nodes without PCT-capable hardware
  will simply not publish the `cpuclass.balloons.nri.io/hp-pct`
  extended resource, so HP pods naturally land on nodes that do.
- `kubectl` configured to talk to the cluster.

## 1. Install balloons with PCT enabled

```bash
helm install nri-resource-policy-balloons nri-plugins/nri-resource-policy-balloons --namespace kube-system --set allowPCT=true
```

`--set allowPCT=true` gives the plugin pod the `privileged: true`
security context and `/dev` mount it needs to talk to
`/dev/isst_interface`.

Wait for the daemonset to be Ready (one Pod per node):

```bash
kubectl -n kube-system rollout status ds/nri-resource-policy-balloons
```

## 2. Apply the minimal PCT policy

The policy below defines two cpuClasses and four balloonTypes.

cpuClasses:

- `default` -- the implicit fallback class. It carries
  `pctPriority: low`, which makes it the **LP class**: idle
  CPUs and every balloon whose `cpuClass` is unset (i.e. uses
  `default`) run on cores capped at base frequency. Defining
  an LP class is required: balloons routes idle CPUs to the LP
  CLOS so they do not inflate the active-HP-core count on each
  PCT power domain (punit).
- `hp-pct` -- `pctPriority: high`. Containers in this class
  run on cores programmed for top turbo frequency. The
  `publishExtendedResource: true` flag is what makes the
  scheduler see the per-node HP capacity.

balloonTypes (this part mirrors a typical Kubernetes
CPU-manager-style split, with one extra balloon for HP pods):

- `reserved` -- the implicit kube-system balloon that runs on
  `reservedResources.cpu`.
- `hp-bln` -- picked by the
  `balloon.balloons.resource-policy.nri.io: hp-bln` pod
  annotation. Uses the `hp-pct` cpuClass, so its containers
  get top-turbo HP cores. `preferNewBalloons: true` puts each
  HP pod in a fresh balloon on a separate PCT power domain,
  and `maxCPUs: 8` keeps the balloon within one bucket-0 HP
  budget per punit on Xeon 6.
- `guaranteed` -- picked by Kubernetes pod QoS class
  `Guaranteed`. Containers get exclusive CPUs in the same
  spirit as Kubernetes' built-in CPU manager.
- `burstable` -- picked by Kubernetes pod QoS class
  `Burstable`. Containers share CPUs with other burstables.
  `shareIdleCPUsInSame: package` lets a burstable container
  burst onto every otherwise-idle CPU in the same CPU package,
  giving the largest pool of burst CPUs that still preserves
  data locality (good balance between memory latency and
  bandwidth).

```bash
cat > balloons-pct.yaml <<EOF
apiVersion: config.nri/v1alpha1
kind: BalloonsPolicy
metadata:
  name: default
  namespace: kube-system
spec:
  pinCPU: true
  pinMemory: false
  allocatorTopologyBalancing: true
  balloonTypes:
  - name: reserved
  - name: hp-bln
    cpuClass: hp-pct
    maxCPUs: 8
    preferNewBalloons: true
  - name: guaranteed
    matchExpressions:
    - key: qosclass
      operator: Equals
      values: ["Guaranteed"]
    preferNewBalloons: true
  - name: burstable
    matchExpressions:
    - key: qosclass
      operator: In
      values: ["Burstable", "BestEffort"]
    shareIdleCPUsInSame: package
    minBalloons: 2
  cpuClasses:
  - name: default
    pctPriority: low
    pctMinFreq: min
    pctMaxFreq: base
  - name: hp-pct
    pctPriority: high
    pctMinFreq: base
    pctMaxFreq: turbo
    publishExtendedResource: true
EOF
kubectl apply -f balloons-pct.yaml
```

Verify that the cluster now advertises HP-core capacity per
node. PCT-capable nodes show a non-zero number; other nodes
omit the resource entirely.

```bash
kubectl get nodes -o json | jq -r '
  .items[] | [.metadata.name,
    (.status.capacity["cpuclass.balloons.nri.io/hp-pct"] // "-")]
  | @tsv'
```

Example output (one PCT-capable node, two regular nodes):

```text
node-pct-1   32
node-2       -
node-3       -
```

The HP capacity is the platform's *guaranteed top-turbo HP CPU
count* -- the number of CPUs across the node that can
simultaneously run at the highest turbo bucket. On a dual-socket
Xeon 6776P with eight HP cores per punit and four active punits
that is 32.

## 3. Deploy one HP pod and one normal pod

Both pods use the `nginx:stable` Docker Official image (which
ships with `openssl`) and stay idle waiting for `kubectl exec`.

- `hp-app` -- **Guaranteed** (CPU request equals limit, memory
  request equals limit). Requests
  `cpuclass.balloons.nri.io/hp-pct: "2"`. That extended-resource
  request forces the scheduler to place the pod on a node that
  has at least two free HP CPUs, and the
  `balloon.balloons.resource-policy.nri.io: hp-bln` annotation
  routes it into the HP balloon.
- `plain-app` -- **Guaranteed** (same shape, no extended
  resource, no annotation). It lands in the `guaranteed`
  balloon type by Kubernetes QoS class. It runs on LP cores.

Both pods request the same number of CPUs (`2`), so the
performance comparison reflects only the HP vs. LP cpuClass
difference, not the CPU count.

> **Bookkeeping rule.** When using
> `publishExtendedResource`, the number of HP CPUs requested
> via `cpuclass.balloons.nri.io/hp-pct` **must equal the pod's
> `cpu` request**. Each HP CPU consumed by the container has
> to be counted once in the scheduler's extended-resource
> bookkeeping and once in normal CPU bookkeeping; mismatched
> counts let the scheduler oversubscribe HP CPUs on the node
> or, conversely, leave them stranded.

```bash
cat > pods.yaml <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: hp-app
  annotations:
    balloon.balloons.resource-policy.nri.io: hp-bln
spec:
  containers:
  - name: app
    image: docker.io/library/nginx:stable
    command: ["sleep", "3600"]
    resources:
      requests:
        cpu: "2"
        memory: "64Mi"
        cpuclass.balloons.nri.io/hp-pct: "2"
      limits:
        cpu: "2"
        memory: "64Mi"
        cpuclass.balloons.nri.io/hp-pct: "2"
---
apiVersion: v1
kind: Pod
metadata:
  name: plain-app
spec:
  containers:
  - name: app
    image: docker.io/library/nginx:stable
    command: ["sleep", "3600"]
    resources:
      requests:
        cpu: "2"
        memory: "64Mi"
      limits:
        cpu: "2"
        memory: "64Mi"
EOF
kubectl apply -f pods.yaml
kubectl wait --for=condition=Ready --timeout=60s pod/hp-app pod/plain-app
```

If `hp-app` stays `Pending` with
`FailedScheduling ... Insufficient cpuclass.balloons.nri.io/hp-pct`,
either no node in the cluster has HP CPUs available (see step
2's capacity listing) or every HP CPU is currently taken by
other HP pods. Drop the request to fit, or wait.

To confirm which balloon each pod landed in, ask the plugin:

```bash
kubectl -n kube-system logs ds/nri-resource-policy-balloons \
    | grep -E 'assigning container default/(hp-app|plain-app)' | tail -2
```

Example:

```text
assigning container default/hp-app/app    to balloon hp-bln[0]{cpus:"32,160", mems:"1"}
assigning container default/plain-app/app to balloon guaranteed[0]{cpus:"64,192", mems:"2"}
```

`hp-app` is in the HP balloon; `plain-app` is in a fresh
`guaranteed` balloon with its own exclusive CPUs. Both pods
hold the same number of CPUs, so the throughput difference
in the next step reflects only the HP vs. LP frequency.

## 4. Observe the performance difference

`nginx:stable` ships with `openssl`, which has a built-in
self-benchmark (`openssl speed`) that measures cipher
throughput on the local CPU. Pure CPU work, no I/O, no
warm-up needed. Run it in both pods:

```bash
kubectl exec hp-app    -- openssl speed -seconds 5 -evp aes-128-cbc 2>&1 | tail -2
kubectl exec plain-app -- openssl speed -seconds 5 -evp aes-128-cbc 2>&1 | tail -2
```

Example output (Xeon 6776P, 4.6 GHz HP vs. 2.3 GHz LP):

```text
# hp-app
type             16 bytes     64 bytes    256 bytes   1024 bytes   8192 bytes  16384 bytes
AES-128-CBC    1788838.33k  2156328.92k  2205063.68k  2217644.85k  2221249.33k  2221617.97k

# plain-app
type             16 bytes     64 bytes    256 bytes   1024 bytes   8192 bytes  16384 bytes
AES-128-CBC     893471.74k   1075806.28k 1100445.44k  1106751.28k  1108534.89k  1108836.35k
```

The HP pod processes AES-128-CBC at roughly 2x the throughput
of the plain pod (`2221617.97k` vs. `1108836.35k` bytes/s on
the 16 KB block size). The ratio mirrors the HP/LP frequency
ratio of the node and is reproducible from one invocation to
the next.

## 5. Clean up

```bash
kubectl delete -f pods.yaml
kubectl delete -f balloons-pct.yaml
helm -n kube-system uninstall nri-resource-policy-balloons
rm -f balloons-pct.yaml pods.yaml
```

## What next

- For a deeper, fully verified walk-through of managed mode --
  including `intel-speed-select` inspection, `NodeResourceTopology`
  verification, and per-pod `sysbench`/`turbostat` reporting --
  see [balloons-pct-example-auto.md](balloons-pct-example-auto.md).
- For the assoc-only PCT mode, where the operator owns the
  SST-CP/SST-TF configuration and balloons only associates CPUs
  to operator-programmed CLOSes, see
  [balloons-pct-example-manual.md](balloons-pct-example-manual.md).
- For the full balloons policy reference (other cpuClass
  fields, C-state control, cpufreq, scheduling-class
  integration), see [balloons documentation](../balloons.md).
