# pod-deletion-cost-controller

A Kubernetes controller that prevents HPA scale-down from terminating pods that are actively processing work.

It does this by continuously annotating pods with [`controller.kubernetes.io/pod-deletion-cost`](https://kubernetes.io/docs/concepts/workloads/controllers/replicaset/#pod-deletion-cost) based on their real-time CPU usage. The ReplicaSet controller uses this annotation when choosing which pod to remove during scale-down: **lower cost = preferred for deletion**. Busy pods get a high cost so they are spared; idle pods get a low cost so they are picked first.

## Problem

If you run a workload that processes files of varying size — from small text documents to multi-hour video jobs — HPA scale-down will kill pods seemingly at random. A pod burning 2 CPU cores transcoding a video can be terminated just as easily as a pod sitting idle waiting for its next task. The annotation-based approach solves this without any changes to the workload code.

## How it works

```text
every syncInterval (default 60s):
  for each configured target (namespace + labelSelector):
    for each matching pod in Running phase:
      get CPU from metrics-server
      if CPU > busyCPUThreshold  → annotate with busyCost   (high, protected)
      if CPU ≤ busyCPUThreshold  → annotate with idleCost   (low, preferred for deletion)
      if metrics not available   → annotate with noMetricsCost (high, pod is starting up)
```

Pods in non-Running phases (Pending, Succeeded, Failed) and pods already being terminated are left untouched.

## Prerequisites

- Kubernetes ≥ 1.22 (pod-deletion-cost annotation support)
- [metrics-server](https://github.com/kubernetes-sigs/metrics-server) installed and working (`kubectl top pods` returns data)
- Helm ≥ 3.x (for the recommended installation method)

## Installation

### Helm (recommended)

Install from the OCI registry (no `helm repo add` needed):

```bash
helm install pdcc oci://ghcr.io/zepellin/charts/pod-deletion-cost-controller \
  --version 0.3.2 \
  --namespace pdcc-system \
  --create-namespace \
  --set config.targets[0].namespace=default \
  --set 'config.targets[0].labelSelector=app=video-processor' \
  --set config.busyCPUThreshold=200m
```

To deploy with a custom values file instead (recommended for multiple targets):

```bash
helm install pdcc oci://ghcr.io/zepellin/charts/pod-deletion-cost-controller \
  --version 0.3.2 \
  --namespace pdcc-system \
  --create-namespace \
  --values my-values.yaml
```

To upgrade an existing release:

```bash
helm upgrade pdcc oci://ghcr.io/zepellin/charts/pod-deletion-cost-controller \
  --version 0.3.2 \
  --namespace pdcc-system \
  --values my-values.yaml
```

To uninstall:

```bash
helm uninstall pdcc --namespace pdcc-system
```

### Verify the installation

```bash
# Controller pod should be Running
kubectl get pods -n pdcc-system

# After one sync interval, check that target pods have the annotation
kubectl get pods -n <your-namespace> \
  -l <your-label-selector> \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,COST:.metadata.annotations.controller\.kubernetes\.io/pod-deletion-cost'
```

## Configuration

All configuration lives in a single YAML file, rendered from Helm values and mounted as a ConfigMap.

### Full reference

```yaml
# How often to reconcile all target pods.
# Accepts any Go duration string: "15s", "1m", "2m30s".
syncInterval: "60s"

# CPU threshold above which a pod is considered "busy" and protected from deletion.
# Uses Kubernetes quantity notation: "100m" = 100 millicores, "0.5" = 500 millicores.
# Tune this above your workload's idle baseline (including sidecar CPU noise).
busyCPUThreshold: "500m"

# pod-deletion-cost assigned to busy pods (CPU > threshold).
# Higher value = less likely to be chosen for deletion.
# Range: any int32. Default: 10000.
busyCost: 10000

# pod-deletion-cost assigned to idle pods (CPU ≤ threshold).
# Lower value = preferred for deletion during scale-down.
# Range: any int32. Default: 0. Use a negative value (e.g. -10000) for stronger preference.
idleCost: 0

# pod-deletion-cost assigned when metrics are not yet available.
# This happens when a pod has just started and the metrics-server hasn't scraped it yet
# (typically within the first 15–30 seconds of pod life).
# Setting this equal to busyCost protects starting pods from being preempted.
noMetricsCost: 10000

# One or more workloads to manage. Each entry selects pods by namespace and label selector.
targets:
  - namespace: "default"
    labelSelector: "app=video-processor"
    # containers: which container names to include when summing CPU.
    # Leave empty to sum ALL containers (including native sidecar containers).
    # List specific names to exclude sidecar CPU from the busy calculation.
    containers: []
    # strategy: costing algorithm to use. Default: "threshold".
    # "threshold"           — assigns busyCost when above threshold, idleCost otherwise.
    # "escalating"          — cost increments by escalatingStep each busy sync cycle up to
    #                         escalatingMax, then resets to idleCost when idle.
    # "escalating-weighted" — same, but the increment is scaled by how much CPU the pod
    #                         is using, so heavier pods escalate faster.
    strategy: threshold

  - namespace: "processing"
    labelSelector: "app=file-processor,tier=worker"
    containers:
      - main
    strategy: escalating
    # escalatingStep: cost added per busy sync cycle (default: busyCost / 10).
    escalatingStep: 1000
    # escalatingMax: cost ceiling for the escalating strategies (default: 1000000).
    escalatingMax: 1000000

  - namespace: "rendering"
    labelSelector: "app=renderer"
    strategy: escalating-weighted
    # Under escalating-weighted, escalatingStep is the cost added per cycle for a pod
    # running at escalatingCPUReference; heavier pods gain proportionally more.
    escalatingStep: 1000
    # escalatingCPUReference: usage that earns a full step (default: "1000m", one core).
    escalatingCPUReference: "1000m"
    escalatingMax: 1000000
```

### Helm values

The controller is configured entirely through `config.*` in `values.yaml`. All other values control the Kubernetes resources themselves.

| Value | Default | Description |
| --- | --- | --- |
| `config.syncInterval` | `"60s"` | How often to reconcile all target pods |
| `config.busyCPUThreshold` | `"500m"` | CPU above this = busy (Kubernetes quantity) |
| `config.busyCost` | `10000` | Annotation value for busy pods |
| `config.idleCost` | `0` | Annotation value for idle pods |
| `config.noMetricsCost` | `10000` | Annotation value when metrics are unavailable |
| `config.targets` | `[]` | List of target workloads (see above) |
| `config.targets[*].strategy` | `"threshold"` | Costing algorithm: `threshold`, `escalating` or `escalating-weighted` (see [Strategies](#strategies)) |
| `config.targets[*].escalatingStep` | `busyCost / 10` | Cost increment per busy sync cycle (escalating strategies only) |
| `config.targets[*].escalatingMax` | `1000000` | Cost ceiling for the escalating strategies |
| `config.targets[*].escalatingCPUReference` | `"1000m"` | CPU usage that earns a full `escalatingStep` (`escalating-weighted` only) |
| `replicaCount` | `1` | Number of replicas; leader election is auto-enabled when > 1 |
| `leaderElection.enabled` | `false` | Force-enable leader election even with `replicaCount: 1` |
| `rbac.create` | `true` | Create the Role/ClusterRole and bindings (see [RBAC](#rbac)) |
| `rbac.scope` | `cluster` | `cluster` for one ClusterRole, `namespaced` for a Role per target namespace |
| `podDisruptionBudget.enabled` | `false` | Create a PodDisruptionBudget |
| `podDisruptionBudget.minAvailable` | `1` | Min pods available during disruption (integer or `"50%"`) |
| `podDisruptionBudget.maxUnavailable` | _(unset)_ | Max pods unavailable during disruption; takes precedence over `minAvailable` when set |
| `image.repository` | `ghcr.io/zepellin/pod-deletion-cost-controller` | Container image repository |
| `image.tag` | Chart `appVersion` | Image tag override |
| `logLevel` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor` (see [Observability](#observability)) |
| `serviceMonitor.namespace` | _(release namespace)_ | Namespace to create the `ServiceMonitor` in |
| `serviceMonitor.interval` | `30s` | Scrape interval |
| `serviceMonitor.scrapeTimeout` | `10s` | Scrape timeout |
| `serviceMonitor.additionalLabels` | `{}` | Extra labels on the `ServiceMonitor` |
| `resources.requests.cpu` | `50m` | Controller CPU request |
| `resources.requests.memory` | `64Mi` | Controller memory request |
| `resources.limits.memory` | `128Mi` | Controller memory limit |
| `serviceAccount.create` | `true` | Create a ServiceAccount for the controller |
| `serviceAccount.name` | _(generated)_ | ServiceAccount name; defaults to the fullname template |
| `serviceAccount.annotations` | `{}` | Annotations on the ServiceAccount (e.g. for IRDA/Workload Identity) |
| `podSecurityContext` | `runAsNonRoot`, uid/gid `65532` | Pod-level security context |
| `securityContext` | no privilege escalation, read-only rootfs, all capabilities dropped | Container-level security context |
| `podAnnotations` | `{}` | Extra annotations on the controller pod |
| `nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` / `{}` | Standard scheduling controls |
| `imagePullSecrets` | `[]` | Pull secrets for a private registry |
| `nameOverride` / `fullnameOverride` | `""` | Override the generated resource names |

### Tuning the CPU threshold

The `busyCPUThreshold` is the most important setting. Set it:

- **High enough** to be above the idle noise floor of your pods (a sleeping pod may burn 5–20m CPU just from the runtime, sidecars, etc.).
- **Low enough** to catch a pod that has just started receiving work before it ramps up to full utilisation.

A good starting point is to run `kubectl top pods` on your idle workload for a few minutes, take the maximum reading, and set the threshold to 2–3× that value.

### Strategies

Each target can use one of three costing algorithms, configured per target via the `strategy` field.

**`threshold`** (default) — simple on/off assignment:

- CPU > `busyCPUThreshold` → cost set to `busyCost`
- CPU ≤ `busyCPUThreshold` → cost set to `idleCost`

This is suitable for most workloads where a pod is clearly busy or idle.

**`escalating`** — cost grows the longer a pod stays busy:

- Each sync cycle where CPU > `busyCPUThreshold`, the cost is incremented by `escalatingStep` (default: `busyCost / 10`), up to `escalatingMax` (default: `1000000`).
- When the pod goes idle, cost resets to `idleCost`.

This gives long-running jobs stronger protection over time, so a pod that has been busy for ten sync cycles is far less likely to be evicted than one that just started processing.

**`escalating-weighted`** — cost grows with how _hard_ the pod is working, not just how long:

- Each sync cycle where CPU > `busyCPUThreshold`, the cost is incremented by `escalatingStep × (CPU ÷ escalatingCPUReference)`, up to `escalatingMax`.
- `escalatingCPUReference` (default: `1000m`, one core) is the usage that earns exactly one full step, so `escalatingStep` reads as "cost per core-cycle". With the default reference, a pod at `1000m` gains ten times as much per cycle as a pod at `100m`.
- A busy pod always gains at least `1`, so a pod just above the threshold still escalates rather than sitting at zero.
- When the pod goes idle, cost resets to `idleCost`, exactly as with `escalating`.

Use this when the _amount of work already done_ is what makes a restart expensive: a pod chewing through a full core has more in flight to lose than one idling just above the busy threshold.

```yaml
config:
  targets:
    - namespace: default
      labelSelector: "app=video-processor"
      strategy: threshold          # default, no extra fields needed

    - namespace: processing
      labelSelector: "app=file-processor,tier=worker"
      strategy: escalating
      escalatingStep: 1000        # cost += 1000 each busy sync cycle
      escalatingMax: 1000000      # ceiling

    - namespace: rendering
      labelSelector: "app=renderer"
      strategy: escalating-weighted
      escalatingStep: 1000            # cost added per cycle at the reference usage
      escalatingCPUReference: "1000m" # a pod at 1 core gains 1000/cycle, at 200m gains 200
      escalatingMax: 1000000
```

Worked example with the settings above — two pods busy for the same three cycles:

| Cycle | `renderer-a` @ `2000m` | `renderer-b` @ `250m` |
| --- | --- | --- |
| 1 | 2000 | 250 |
| 2 | 4000 | 500 |
| 3 | 6000 | 750 |

If the cluster autoscaler needs to remove one, `renderer-b` is the cheaper choice at every point.

### Sidecar containers

Pods running service meshes (Istio/Envoy), logging agents, or native Kubernetes sidecars (init containers with `restartPolicy: Always`, k8s ≥ 1.29) will have their sidecar CPU included in the total when `containers: []`.

If sidecar CPU is significant enough to push the sum above `busyCPUThreshold` even when the main container is idle, use the `containers` list to restrict which containers are counted:

```yaml
targets:
  - namespace: default
    labelSelector: "app=video-processor"
    containers:
      - main         # only count the main application container
```

## Usage examples

### Single namespace, single workload

```yaml
config:
  busyCPUThreshold: "200m"
  targets:
    - namespace: default
      labelSelector: "app=video-processor"
      containers: []
```

### Multiple namespaces

```yaml
config:
  syncInterval: "20s"
  busyCPUThreshold: "150m"
  targets:
    - namespace: team-a
      labelSelector: "app=encoder"
      containers:
        - encoder
    - namespace: team-b
      labelSelector: "app=transcoder"
      containers:
        - transcoder
```

### More aggressive idle preference

Setting `idleCost` to a large negative value pushes idle pods strongly to the front of the deletion queue, useful if you have many replicas and want fast scale-down of idle ones:

```yaml
config:
  busyCost: 10000
  idleCost: -10000
```

### Debug logging

To see per-pod decisions on every sync:

```bash
helm upgrade pdcc ./helm/pod-deletion-cost-controller \
  --namespace pdcc-system \
  --set logLevel=debug
```

Log output is structured JSON (one line per event):

```json
{"time":"...","level":"DEBUG","msg":"cost decided","pod":"worker-abc","namespace":"default","cpu":"450m","cost":10000}
{"time":"...","level":"DEBUG","msg":"cost decided","pod":"worker-xyz","namespace":"default","cpu":"8m","cost":0}
{"time":"...","level":"DEBUG","msg":"metrics not yet available","pod":"worker-new","namespace":"default"}
{"time":"...","level":"INFO","msg":"annotation updated","pod":"worker-xyz","namespace":"default","from":"10000","to":"0"}
```

## Observability

The controller serves health and Prometheus endpoints on `--health-addr` (`:8080` by default, exposed as the `metrics` port on the chart's headless Service).

| Endpoint | Purpose |
| --- | --- |
| `/healthz` | Liveness. Fails once the **leader** has gone without completing a sync cycle for `max(3 × syncInterval, 2m)`, so a wedged controller is restarted. Standby replicas always pass — they are not syncing by design. |
| `/readyz` | Readiness. Succeeds whenever the process is serving, including on standby replicas, so their metrics keep being scraped. |
| `/metrics` | Prometheus exposition, including standard Go runtime and process collectors. |

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `pdcc_sync_cycles_total` | counter | `result` (`success`, `partial_error`) | Sync cycles completed |
| `pdcc_sync_duration_seconds` | histogram | — | Duration of a full sync across all targets |
| `pdcc_pods_managed` | gauge | `namespace`, `cost_class` (`idle`, `busy`, `no_metrics`) | Running pods under management, rebuilt each cycle |
| `pdcc_annotation_patches_total` | counter | `namespace`, `result` (`updated`, `gone`, `error`) | Annotation patches attempted; `gone` means the pod was deleted mid-cycle, which is routine |
| `pdcc_metrics_unavailable_total` | counter | `namespace`, `reason` (`not_found`, `error`) | Pods whose CPU metrics could not be read |
| `pdcc_leader` | gauge | — | `1` if this replica is running the sync loop, `0` if standing by |
| `pdcc_last_sync_timestamp_seconds` | gauge | — | Unix timestamp of the last completed sync cycle |

To scrape with the Prometheus Operator, enable the bundled `ServiceMonitor`:

```yaml
serviceMonitor:
  enabled: true
  # namespace: monitoring    # defaults to the release namespace
  # interval: 30s
  # scrapeTimeout: 10s
  # additionalLabels: {}     # e.g. labels your Prometheus selects on
```

Useful starting alerts: `pdcc_sync_cycles_total{result="partial_error"}` increasing, or `time() - pdcc_last_sync_timestamp_seconds` exceeding a few sync intervals while `pdcc_leader == 1`.

## RBAC

The controller needs exactly these permissions on target pods, and no write access to any other resource type:

```yaml
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch", "patch"]
  - apiGroups: ["metrics.k8s.io"]
    resources: ["pods"]
    verbs: ["get", "list"]
```

`rbac.scope` controls how widely they are granted:

| Scope | What is created | When to use it |
| --- | --- | --- |
| `cluster` (default) | One `ClusterRole` + `ClusterRoleBinding` covering every namespace | Targets span many namespaces, or you add targets in namespaces that don't exist yet |
| `namespaced` | A `Role` + `RoleBinding` in each namespace named by `config.targets` | Least privilege — the controller can only touch pods in namespaces you named |

```yaml
rbac:
  scope: namespaced
config:
  targets:
    - namespace: team-a
      labelSelector: "app=encoder"
    - namespace: team-b
      labelSelector: "app=transcoder"
```

Two constraints apply to `namespaced` scope: every target namespace must **already exist** when the chart is installed, and adding a target later requires a `helm upgrade` so its namespace gets a `Role` — otherwise the controller logs `list pods: ... is forbidden` for that target on every cycle and leaves those pods unannotated. Rendering fails fast if `config.targets` is empty, since the controller would be granted access to nothing.

Set `rbac.create: false` to manage all of the above yourself. Leader election additionally needs `get`/`create`/`update` on `coordination.k8s.io/leases` in the release namespace; the chart creates that `Role` whenever leader election is active.

## Building from source

```bash
git clone https://github.com/zepellin/pod-deletion-cost-controller
cd pod-deletion-cost-controller

# Build the binary
make build               # produces bin/controller

# Run all tests (downloads envtest binaries on first run)
make test

# Unit tests only (no cluster binaries required)
make test-unit

# Build the container image
docker build -t pod-deletion-cost-controller:dev .
```

**Requirements:** Go 1.26, Docker (for image build), GNU Make.

## High availability

Leader election is **automatically enabled when `replicaCount` is greater than 1**. Only the elected leader runs the sync loop; the others stand by and take over if the leader crashes or is evicted. No extra values need to be set — just increase the replica count:

```bash
helm install pdcc oci://ghcr.io/zepellin/charts/pod-deletion-cost-controller \
  --version 0.3.2 \
  --namespace pdcc-system \
  --create-namespace \
  --set replicaCount=2 \
  --set config.targets[0].namespace=default \
  --set 'config.targets[0].labelSelector=app=video-processor'
```

The current leader is visible in the `Lease` object created in the controller namespace:

```bash
kubectl get lease pod-deletion-cost-controller -n pdcc-system -o yaml
```

The Helm chart automatically creates a namespace-scoped `Role` and `RoleBinding` for `coordination.k8s.io/leases` whenever leader election is active. To force-enable leader election with a single replica (for testing), set `leaderElection.enabled: true`.

### Pod disruption budget

For HA deployments, consider adding a PDB so that voluntary disruptions (node drains) cannot take down all replicas simultaneously:

```yaml
replicaCount: 2
podDisruptionBudget:
  enabled: true
  minAvailable: 1   # or use maxUnavailable: 1, or a percentage like "50%"
```

## Limitations

- **CPU is the only signal.** The controller has no way to know that a pod is "busy" by other means (open file handles, queue depth, network I/O). If your workload's CPU profile during processing is similar to idle, the threshold approach will not work reliably — consider exposing a custom metric instead.
- **metrics-server required.** If your cluster uses the Prometheus Adapter to serve `metrics.k8s.io`, verify that `kubectl top pods` works in your target namespaces. If the metrics API is unavailable, the controller logs a warning and protects all pods (sets `noMetricsCost`) until metrics recover.
- **Escalating counters are in-memory.** They are held only by the running leader, so a controller restart or a leader failover resets every pod's escalation to a single increment on the next cycle. This applies to both `escalating` and `escalating-weighted`. Pods keep their last annotation value until then, so protection is never lost — only the accumulated head start.
- **Annotation is not removed when a pod exits scope.** If you remove a pod's labels or change the `labelSelector`, the pod keeps its last annotation value. This is harmless — the ReplicaSet will simply not include it in deletion-cost ordering once it's no longer part of the managed set.
- **Polling, not watching.** Each cycle lists target pods rather than maintaining a watch, so a pod that becomes busy is only noticed at the next tick — worst case one full `syncInterval` late. Listings are served from the API server's watch cache rather than etcd, which keeps the cost low, but a very short `syncInterval` across very large namespaces will still generate meaningful API traffic.

## License

Apache-2.0 — see [LICENSE](LICENSE).
