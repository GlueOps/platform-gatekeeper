# GlueOps Core Gatekeeper
<img width="960" height="1088" alt="Gatekeeper" src="gatekeeper.png" />

Gatekeeper is a small Kubernetes controller that answers one question per app:
**are this app's dependencies ready?**

You declare the dependencies in a `Gate` custom resource. Gatekeeper evaluates them
on a timer and publishes the answer to `Gate.status`. Argo CD reads that status as
resource health, so an ordinary sync wave holds your rollout until the dependencies
are actually up:

```yaml
# gate.yaml — applied in wave -1
apiVersion: platform.glueops.dev/v1alpha2
kind: Gate
metadata:
  name: my-app-dependencies
  annotations:
    argocd.argoproj.io/sync-wave: "-1"
spec:
  checks:
    - id: database
      deploymentAvailable:
        name: postgres
    - id: migration
      jobComplete:
        name: my-app-migrate
```

Your workloads stay in the default wave and need no annotation. There is no hook
Job, no polling script, and no per-app RBAC.

This is useful when ordering has to span things sync waves can't see on their own —
Helm-managed components, Flux HelmReleases, other Argo CD Applications, and
dependencies that aren't represented as Kubernetes workloads at all.

---

## How it works

1. Gatekeeper lists every `Gate` in the cluster every 30 seconds.
2. For each check it reads the referenced resource and decides whether it's ready.
3. It writes a `Ready` condition plus a per-check breakdown to `Gate.status`.
4. Argo CD maps that condition to resource health via a Lua script, and a sync wave
   does the ordering.

Gatekeeper reads dependencies with its **own** ServiceAccount. What a Gate may
observe is bounded by two things: the kinds granted in gatekeeper's ClusterRole, and
the namespace rules below. Callers need no credentials and no RBAC of their own.

### What this costs: readiness can be stale

Gatekeeper publishes readiness rather than answering requests on demand, so a
gatekeeper that is down or wedged leaves Gates showing their last known state.
Neither Argo CD's health rollup nor `kubectl wait` can tell how old that state is,
so a rollout gated on an already-`Ready` Gate can proceed on stale information.

This is a deliberate trade. The dependencies normally gated here are self-healing —
an app that starts before its database crashloops and then recovers — and it is how
Argo CD, Flux and cert-manager all already behave. `prometheusrule.yaml` ships the
single alert that covers it.

**Where it would genuinely hurt is a gate protecting something irreversible**, such
as a migration that an app must not start before. If you add a gate like that, say
so — a synchronous check that fails closed is a small addition, but it isn't in the
box today.

---

## Install

### 1. CRD and RBAC

```bash
kubectl apply -f crd.yaml
kubectl apply -f rbac.yaml
```

`rbac.yaml`'s ClusterRole is **the ceiling on what any Gate can observe**. A kind
granted there becomes readable by every Gate (in its own namespace, or an allowlisted
namespace in platform mode). Add kinds deliberately, and never grant `secrets` or
`configmaps`.

### 2. The controller

```bash
kubectl apply -f deploy.yaml
```

Edit the image and the allowlist env vars first. Optionally also apply
`prometheusrule.yaml` (needs the Prometheus Operator and a scrape config pointing at
the Service's `/metrics`).

### 3. Teach Argo CD to read a Gate — required

```bash
# Merge into the existing argocd-cm; do not replace it.
kubectl apply -f examples/argocd-health-check.yaml
```

**Without this, Gates do nothing.** Argo CD has no built-in health check for a
`Gate`, and a resource with no health check is *skipped* in the Application health
rollup — the wave completes immediately and the next wave proceeds ungated.

### 4. Set an Argo CD sync timeout — required

Set `--sync-timeout` (or `ARGOCD_APPLICATION_CONTROLLER_SYNC_TIMEOUT`) on the
application controller. It defaults to *never*, so a gate that legitimately never
becomes ready leaves the sync operation Running indefinitely — and a Running
operation blocks every later auto-sync of that Application, **including the commit
that would fix the gate**. Recovery then needs a manual
`argocd app terminate-op <app>`.

Note this is a controller-wide setting, so pick a value above the longest legitimate
gate wait in the cluster.

---

## Using it

Start from [`examples/wave-gating/`](examples/wave-gating) — an Application with a
Gate in wave `-1` and a Deployment in the default wave.

Two rules that are easy to get wrong:

- **Wave `-1`, not `0`.** The default wave *is* `0`, so a Gate at `0` shares a wave
  with un-annotated resources and orders nothing.
- **Same Application.** Sync waves only order resources within one sync, so a Gate
  cannot gate workloads in a different Argo CD Application. For that case, a PreSync
  Job running `kubectl wait --for=condition=Ready gate/x` works, but only if the
  Gate already exists — PreSync runs before manifests are applied, so a Gate shipped
  alongside the workloads won't exist yet and the wait will fail immediately.

### Reading status

```bash
$ kubectl get gates -A
NAMESPACE   NAME                  READY   REASON                 LAST-TRANSITION
nonprod     my-app-dependencies   False   DependenciesNotReady   40s

$ kubectl get gate my-app-dependencies -n nonprod -o jsonpath='{.status.results}' | jq .
[
  {"id":"database","ready":true,"reason":"Ready","message":"deployment Available=True"},
  {"id":"migration","ready":false,"reason":"DependenciesNotReady","message":"job not complete yet"}
]
```

`REASON` separates the cases that need different responses:

| Reason | Status | Meaning |
| --- | --- | --- |
| `Ready` | True | every check passed |
| `DependenciesNotReady` | False | still waiting; expected to resolve on its own |
| `Terminal` | False | a failed Job, Degraded Application or stalled HelmRelease — it will not recover without a change, so stop waiting |
| `PolicyViolation` | False | the Gate references a namespace it isn't allowed to read |
| `ReadForbidden` | False | gatekeeper's ClusterRole is missing a kind; the message names what to grant |
| `InvalidSpec` | False | the Gate itself is malformed |
| `EvaluationFailed` | Unknown | gatekeeper couldn't evaluate a check, so it makes no claim either way |

`Terminal` surfaces in Argo CD as `Degraded`, which fails the sync operation promptly
instead of waiting out the timeout. Everything else that isn't ready reads as
`Progressing`.

---

## The Gate resource

Each check sets **exactly one** check type. The CRD enforces that, which is also what
catches a misspelled type name — the API server prunes unknown fields, so without the
check a typo would leave a Gate that looks right in Git and verifies nothing.

A check reads from the Gate's own namespace unless it sets `namespace:`, which
requires platform mode (below).

### `deploymentAvailable`

Deployment is `Available` with at least N available replicas.

```yaml
- id: api
  deploymentAvailable:
    name: my-api
    minAvailableReplicas: 2   # default 1
```

### `statefulSetReady`

StatefulSet has at least N ready replicas, optionally fully rolled out.

```yaml
- id: postgres
  statefulSetReady:
    name: postgres
    minReadyReplicas: 1            # default 1
    requireUpdatedRevision: true   # default true
```

### `jobComplete`

Job completed successfully. A `Failed` Job is reported as `Terminal`.

```yaml
- id: migrate
  jobComplete:
    name: my-app-migrate
```

### `serviceReadyEndpoints`

Service has at least N ready endpoint addresses, counted across its EndpointSlices.

```yaml
- id: redis
  serviceReadyEndpoints:
    name: redis
    minReadyAddresses: 1   # default 1
```

### `podLabelReady`

At least N pods matching a label selector are `Ready`.

```yaml
- id: workers
  podLabelReady:
    selector: "app=my-worker,tier=backend"
    minReadyPods: 2   # default 1
```

> Prefer a specific selector — a broad one lists many pods on every sweep.

### `argoApplicationHealthy`

Argo CD Application is Healthy and/or Synced. `Degraded` and `Missing` are reported as
`Terminal`.

```yaml
- id: monitoring
  argoApplicationHealthy:
    name: monitoring-stack
    requireSynced: true    # default true
    requireHealthy: true   # default true
```

### `fluxHelmReleaseReady`

Flux `helm.toolkit.fluxcd.io/v2` HelmRelease is Ready, on the current generation, and
not Stalled. A stalled release is reported as `Terminal`.

```yaml
- id: cert-manager
  namespace: glueops-core-flux
  fluxHelmReleaseReady:
    name: cert-manager
    requireCurrentGeneration: true   # default true
```

Prefer this over pointing `deploymentAvailable` at the workload a HelmRelease manages.
A Deployment check reads whatever is in the cluster right now, so during a failed
upgrade it reports ready against the *previous* revision's pods while the release
behind them is broken.

Flux HelmReleases usually live in the namespace Flux reconciles from rather than
alongside the workload, hence the `namespace` override — which needs platform mode.

---

## Namespace modes

A Gate's namespace decides whether its checks may look outside that namespace. The
mode comes from a label on the namespace, which only the platform team can set:

```yaml
gatekeeper.platform.glueops.dev/mode: customer|platform
```

**`customer` (the default, including unlabelled namespaces)** — every check reads
from the Gate's own namespace. A `namespace:` override is refused with
`PolicyViolation`.

**`platform`** — checks may target another namespace, but only if it matches
`GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES` or one of
`GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES`. Anything else is refused.

Both lists are **empty by default**, so a deployment that omits them gets no
cross-namespace access rather than a silently working allowlist.

Note that the allowlist is global: any platform-mode namespace can reach every
allowlisted namespace. "Who can create Gates in a platform-mode namespace" is
therefore a privileged grant.

---

## Configuration

| Env var | Default | Description |
| --- | --- | --- |
| `GATEKEEPER_RECONCILE_INTERVAL` | `30s` | How often to re-evaluate every Gate. `0` disables the loop. |
| `GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES` | *(empty)* | CSV of namespaces platform-mode Gates may read from |
| `GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES` | *(empty)* | CSV of namespace prefixes, e.g. `glueops-core-` |
| `GATEKEEPER_GATE_GROUP` | `platform.glueops.dev` | API group of the Gate CRD |
| `GATEKEEPER_GATE_VERSION` | `v1alpha2` | API version |
| `GATEKEEPER_GATE_RESOURCE` | `gates` | plural resource name |
| `PORT` | `8080` | HTTP port for `/healthz` and `/metrics` |

### Endpoints

- `GET /healthz` — plain process check, for the kubelet probe. It deliberately does
  **not** report sweep health: tying liveness to sweep success would turn an apiserver
  outage into CrashLoopBackOff, and a restarting pod reconciles less than a wedged one.
- `GET /metrics` — exports `gatekeeper_last_successful_sweep_timestamp_seconds`.

---

## Troubleshooting

**A Gate never leaves `DependenciesNotReady`.** Read `status.results` — it names the
check and why. Each message describes the actual resource state
(`availableReplicas 0 < 1`).

**A Gate says `ReadForbidden`.** Gatekeeper's ClusterRole is missing a kind the Gate
references. The message names it, e.g. `grant batch/jobs in its ClusterRole`.

**A Gate says `PolicyViolation`.** The check targets another namespace from a
`customer`-mode namespace, or from a platform-mode namespace to a target that isn't
allowlisted. The message names the target.

**A Gate says `EvaluationFailed`.** Gatekeeper couldn't complete a read — usually a
transient API error. The condition is `Unknown` rather than `False` on purpose: it is
not a claim that the dependency is unready.

**A sync hangs and the Gate looks healthy.** Check that the Lua health check from step
3 is actually installed; without it Argo CD ignores the Gate entirely. A *syntax
error* in that script yields health `Unknown`, which the sync loop silently ignores
while the wave hangs — so test changes against a real Gate first.

**A sync is stuck Running and won't pick up new commits.** That's the
`--sync-timeout` default from step 4. Release it with
`argocd app terminate-op <app>`.

**`GatekeeperNotReconciling` is firing.** Gate readiness is stale, so gated rollouts
may proceed without their dependencies being verified. Check the gatekeeper
Deployment, then confirm Gates are updating with `kubectl get gates -A`.

---

## Local development

```bash
# Runs against your current kubeconfig; see .env.example
go run .

# Tests
go test ./...
```

Build:

```bash
go build -trimpath -ldflags="-s -w" -o gatekeeper .
docker build -t gatekeeper .
```
