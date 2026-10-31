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
    - id: cache
      serviceReadyEndpoints:
        name: redis
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

### The rule that decides what belongs in a Gate

**A Gate is re-evaluated on every sync, so the condition must stay true for the life
of the Application — not merely become true once.** It is a managed resource in wave
`-1` of every sync, not a one-shot precondition.

Gate on steady state ("the database is available", "the dependency is healthy"), not
on events. The classic mistake is gating on a migration Job: it works until
something removes the Job — `ttlSecondsAfterFinished`, a `HookSucceeded` hook
policy, Argo CD pruning, or someone deleting it to re-run the migration — and from
then on **every** sync of that Application hangs at wave `-1`.

### What this costs: readiness can be stale

Gatekeeper publishes readiness rather than answering requests on demand, so a
gatekeeper that is down or wedged leaves Gates showing their last known state.
Neither Argo CD's health rollup nor `kubectl wait` can tell how old that state is,
so a rollout gated on an already-`Ready` Gate can proceed on stale information.

This is a deliberate trade. The dependencies normally gated here are self-healing —
an app that starts before its database crashloops and then recovers — and it is how
Argo CD, Flux and cert-manager all already behave. `prometheusrule.yaml` ships the
alerts that cover it.

**Where it would genuinely hurt is a gate protecting something irreversible**, such
as a migration that an app must not start before. If you need that, say so — a
synchronous check that fails closed is a small addition, but it isn't in the box
today.

---

## Install

### 1. CRD and RBAC

```bash
kubectl apply -f crd.yaml
kubectl apply -f rbac.yaml
```

> **Upgrading from `v1alpha1`?** `kubectl apply -f crd.yaml` will be **rejected**,
> because `v1alpha1` remains in the live CRD's `status.storedVersions`. See
> [Upgrading](#upgrading-from-v1alpha1) below.

`rbac.yaml`'s ClusterRole is **the ceiling on what any Gate can observe**. A kind
granted there becomes readable by every Gate (in its own namespace, or an allowlisted
namespace in platform mode). Add kinds deliberately, and never grant `secrets` or
`configmaps`.

### 2. The controller

```bash
kubectl apply -f deploy.yaml
```

Set the image to a released tag pinned by digest and review the allowlist env vars
first.

### 3. Teach Argo CD to read a Gate — required

```bash
kubectl patch configmap argocd-cm -n argocd --type merge \
  --patch-file examples/argocd-health-check.yaml
```

**Use `kubectl patch`, not `kubectl apply`.** Argo CD installs with client-side
apply, so applying a one-key `argocd-cm` manifest would three-way-merge and delete
every other key in it — after which Argo CD selfHeals the ConfigMap back, silently
removing this health check and un-gating every Gate in the cluster. The file is
shipped as a patch fragment so that mistake isn't possible. If `argocd-cm` is
managed by Helm or GitOps, add the key there instead (`configs.cm` in the argo-cd
chart).

**Without this step, Gates do nothing.** Argo CD has no built-in health check for a
`Gate`, and a resource with no health check is *skipped* in the Application health
rollup — the wave completes immediately and the next wave proceeds ungated.

**Also check `gates` isn't matched by `resource.exclusions`.** An excluded Gate is
stripped from the target objects before the sync, so it is never applied, never
produces a sync task, and wave 0 runs immediately — the same silent un-gating as a
missing health check. The only signal is an `ExcludedResourceWarning` condition on
the Application.

### 4. Set an Argo CD sync timeout — required

Set `--sync-timeout` (or `ARGOCD_APPLICATION_CONTROLLER_SYNC_TIMEOUT`) on the
application controller. It defaults to *never*, so a gate that legitimately never
becomes ready leaves the sync operation Running indefinitely — and a Running
operation blocks every later auto-sync of that Application, **including the commit
that would fix the gate**. Recovery then needs a manual
`argocd app terminate-op <app>`.

This is a controller-wide setting, so pick a value above the longest legitimate gate
wait in the cluster.

### 5. Monitoring — optional but strongly recommended

```bash
kubectl apply -f servicemonitor.yaml   # needs the Prometheus Operator
kubectl apply -f prometheusrule.yaml
```

Two labels decide whether this works at all, and getting either wrong fails
silently:

- The alerts key on `job="glueops-core-gatekeeper"`. The shipped ServiceMonitor
  produces exactly that. If you scrape some other way, fix the label in
  `prometheusrule.yaml` — otherwise `absent()` is true forever on a healthy cluster,
  the alert fires immediately and gets silenced.
- `metadata.labels` on both files must satisfy your Prometheus's `ruleSelector` /
  `serviceMonitorSelector`. kube-prometheus-stack defaults both to
  `{release: <releaseName>}`, so without that label they are accepted by the
  apiserver, appear in `kubectl get`, and are never loaded.

Verify rather than assume:

```bash
# in Prometheus
up{job="glueops-core-gatekeeper"}
prometheus_rule_group_last_evaluation_timestamp_seconds{rule_group=~".*gatekeeper.*"}
```

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
  {"id":"cache","ready":false,"reason":"DependenciesNotReady","message":"ready addresses 0 < 1"}
]
```

`REASON` separates the cases that need different responses:

| Reason | Status | Meaning |
| --- | --- | --- |
| `Ready` | True | every check passed |
| `DependenciesNotReady` | False | still waiting; expected to resolve on its own |
| `Terminal` | False | the upstream controller has given up — a failed Job, a stalled HelmRelease. It will not recover without a change, so stop waiting |
| `PolicyViolation` | False | the Gate references a namespace it isn't allowed to read |
| `ReadForbidden` | False | gatekeeper's ClusterRole is missing a kind; the message names what to grant |
| `InvalidSpec` | False | the Gate itself is malformed |
| `EvaluationFailed` | Unknown | gatekeeper couldn't evaluate a check, so it makes no claim either way |

`Terminal` surfaces in Argo CD as `Degraded`, which fails the sync operation promptly
instead of waiting out the timeout. Everything else that isn't ready reads as
`Progressing`.

One consequence worth knowing: because Argo CD folds resource health into the
Application's on a worst-wins basis, a Gate left blocked or terminal *after* a
rollout keeps the Application `Degraded` even when every workload is healthy — so
health notifications and `argocd app wait` fire on the dependency rather than on
your app, and later syncs fail at wave `-1`, including an unrelated image bump.
Annotating the Gate `argocd.argoproj.io/ignore-healthcheck: "true"` removes it from
that rollup while leaving wave gating and terminal fail-fast fully intact, because
the sync engine reads the Gate's health directly rather than through the rollup.
The shipped example sets it.

Note that `LAST-TRANSITION` is the last time the condition *changed*, not the last
time it was evaluated — so it cannot tell you whether gatekeeper is still running.
Use the sweep log or the metrics for that.

---

## The Gate resource

Each check sets **exactly one** check type. The CRD enforces that, which also
catches a misspelled type name — the API server prunes unknown fields, so without
the check a typo would leave a Gate that looks right in Git and verifies nothing.
(A typo *inside* a check block, like `minAvailableReplcias`, is pruned and silently
defaulted instead; only a client using strict field validation catches that, which
`kubectl` does by default.)

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

> **Think twice before gating on this.** A Gate is re-evaluated on every sync, so
> once the Job is garbage-collected (`ttlSecondsAfterFinished`, a `HookSucceeded`
> hook, a prune) this check reports not-found forever and blocks every later sync.
> See [the rule above](#the-rule-that-decides-what-belongs-in-a-gate).

### `serviceReadyEndpoints`

Service has at least N ready backends, counted from its EndpointSlices.

```yaml
- id: redis
  serviceReadyEndpoints:
    name: redis
    minReadyAddresses: 1   # default 1
```

Counts ready endpoints, taking the maximum across address families — a dual-stack
Service has an IPv4 and an IPv6 slice describing the *same* pods, so they are not
added together.

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

Argo CD Application is Healthy and/or Synced.

```yaml
- id: monitoring
  argoApplicationHealthy:
    name: monitoring-stack
    requireSynced: true    # default true
    requireHealthy: true   # default true
```

Nothing here is reported as `Terminal`. Argo CD health is a rollup over an
Application's resources rather than a statement that its controller stopped trying:
`Progressing` and `Degraded` both occur mid-rollout, and `Missing` is simply an
Application whose resources aren't live yet — which is exactly what a Gate guarding
a not-yet-bootstrapped stack should wait for.

### `fluxHelmReleaseReady`

Flux `helm.toolkit.fluxcd.io/v2` HelmRelease is Ready, on the current generation, and
not Stalled. A stalled release is reported as `Terminal`, because helm-controller has
explicitly stopped retrying until the spec changes.

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
| `GATEKEEPER_RECONCILE_INTERVAL` | `30s` | How often to re-evaluate every Gate. Minimum 1s; anything lower or unparseable falls back to the default. To stop reconciling, scale the Deployment to 0. |
| `GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES` | *(empty)* | CSV of namespaces platform-mode Gates may read from |
| `GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES` | *(empty)* | CSV of namespace prefixes, e.g. `glueops-core-` |
| `PORT` | `8080` | HTTP port for `/healthz` and `/metrics` |
| `GOMEMLIMIT` | *(unset)* | Standard Go setting; `deploy.yaml` sets it below the memory limit so growth costs GC rather than an OOMKill |

### Endpoints

- `GET /healthz` — plain process check, for the kubelet probe. It deliberately does
  **not** report sweep health: tying liveness to sweep success would turn an apiserver
  outage into CrashLoopBackOff, and a restarting pod reconciles less than a wedged one.
- `GET /metrics` — exports:
  - `gatekeeper_last_successful_sweep_timestamp_seconds` — last sweep that listed the
    Gates and wrote a status to every one of them.
  - `gatekeeper_gates_unevaluated` — Gates whose last evaluation reached no verdict.
  - `gatekeeper_sweep_duration_seconds` — wall time of the last sweep; distinguishes
    a slow sweep from a wedged one.

---

## Upgrading from `v1alpha1`

The CRD ships `v1alpha2` only, and `kubectl apply -f crd.yaml` is **rejected** on a
cluster that already has `v1alpha1`:

```
status.storedVersions[0]: Invalid value: "v1alpha1": missing from spec.versions
```

In GitOps this is a hard Argo CD sync failure, and the obvious workaround
(`kubectl delete crd`) cascade-deletes every Gate in the cluster.

`spec.strict` was also removed, so existing Gates must drop it.

**If you have no Gates worth keeping** (nothing in production), the short path is
fine — note it deletes all Gate objects:

```bash
kubectl get gates -A -o yaml > gates-backup.yaml   # somewhere durable and private;
                                                   # it contains tenant namespace names
kubectl delete crd gates.platform.glueops.dev
kubectl wait --for=delete crd/gates.platform.glueops.dev --timeout=60s
kubectl apply -f crd.yaml
# re-apply Gates with spec.strict removed and a sync-wave annotation added
```

**Otherwise, migrate the stored version without deleting anything:**

1. Apply a CRD listing *both* versions with `v1alpha2` as the storage version. The
   repo doesn't ship a `v1alpha1` schema, but it doesn't need to be a real one — a
   stub is enough to satisfy the check:

   ```yaml
   - name: v1alpha1
     served: false
     storage: false
     schema:
       openAPIV3Schema:
         type: object
         x-kubernetes-preserve-unknown-fields: true
   ```

2. Rewrite every Gate so it re-persists at the new version:
   `kubectl get gates -A -o json | kubectl replace -f -`
3. Drop the old version from the CRD's `status.storedVersions`. **The
   `--subresource=status` flag is required** — without it the `status` key is
   silently discarded and step 4 fails with the same error:

   ```bash
   kubectl patch crd gates.platform.glueops.dev --subresource=status \
     --type merge -p '{"status":{"storedVersions":["v1alpha2"]}}'
   ```

4. Now apply the `v1alpha2`-only `crd.yaml`.

Either way, merge the tenant-side Git changes (dropping `spec.strict`, adding the
wave annotation) **before** the CRD swap, or pause auto-sync for the window —
otherwise Argo CD re-applies old-shape Gates that the new CRD rejects.

Expect a short window after the swap where Gates have no `status.conditions` until
the first sweep. The Lua check reports `Progressing` for that, so gated waves hold
rather than racing ahead — bounded by `--sync-timeout`.

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

**A sync hangs and the Gate looks healthy.** Check that the Lua health check from
step 3 is actually installed; without it Argo CD ignores the Gate entirely. A *syntax
error* in that script yields health `Unknown`, which the sync loop silently ignores
while the wave hangs — so test changes against a real Gate first.

**A sync is stuck Running and won't pick up new commits.** That's the
`--sync-timeout` default from step 4. Release it with
`argocd app terminate-op <app>`.

**Is gatekeeper actually running?** Not answerable from `kubectl get gates` —
`LAST-TRANSITION` is deliberately inherited, so a Gate that has been Ready for days
looks identical whether it was evaluated seconds or hours ago. Use:

```bash
kubectl logs -n glueops-core-gatekeeper deploy/glueops-core-gatekeeper --tail=20 | grep sweep:
```

**`GatekeeperNotReconciling` is firing.** Gate readiness is stale, so gated rollouts
may have proceeded without their dependencies being verified. Check the Deployment,
confirm sweeps are completing with the command above, and once recovered review which
gated Applications synced during the window.

**`GatekeeperGatesUnevaluated` is firing.** Some Gates are stuck at `Ready=Unknown`,
so gated rollouts are *waiting* rather than proceeding. Find them with
`kubectl get gates -A | grep Unknown` and read `.status.results`.

---

## Local development

Everything runs in Docker; no Go toolchain needed on the host.

```bash
# build, vet and test
docker run --rm -v "$PWD":/w -w /w golang:1.26-alpine \
  sh -c 'go build ./... && go vet ./... && go test ./...'

# -race needs cgo, so use the Debian image
docker run --rm -v "$PWD":/w -w /w golang:1.26 go test -race ./...

# container image
docker build -t gatekeeper .
```

To run it against a cluster, point `KUBECONFIG` at one and see `.env.example`.
End-to-end instructions, including a throwaway k3d cluster, are in
[AGENTS.md](AGENTS.md).
