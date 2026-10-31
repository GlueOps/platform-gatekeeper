# Examples

## Start here: `wave-gating/`

A complete, copyable setup — an Argo CD `Application` containing a `Gate` in
sync-wave `-1` and a `Deployment` in the default wave. Argo CD applies the Gate,
waits for it to report `Healthy`, and only then rolls out the workload.

```
wave-gating/
├── application.yaml   # the Application holding both resources
├── gate.yaml          # the Gate, annotated sync-wave: "-1"
└── deployment.yaml    # the workload, no annotation (default wave 0)
```

Three things decide whether this works:

1. **`argocd-health-check.yaml` must be installed** (see below). Without it Argo CD
   has no health check for a `Gate`, treats it as having no health at all, and
   proceeds immediately — the Gate exists but gates nothing.
2. **The Gate goes in wave `-1`, not `0`.** The default wave *is* `0`, so a Gate at
   `0` shares a wave with every un-annotated resource and orders nothing.
3. **The Gate must be in the same Application** as the workloads it gates. Sync
   waves only order resources within a single sync.

## Platform setup: `argocd-health-check.yaml`

The Lua health check that teaches Argo CD how to read a Gate. Merge it into the
existing `argocd-cm` (don't replace the ConfigMap — it holds other settings).
Applied once per Argo CD instance.

Your platform also needs `--sync-timeout` (or
`ARGOCD_APPLICATION_CONTROLLER_SYNC_TIMEOUT`) set on the application controller. It
defaults to *never*, so a gate that legitimately never becomes ready would leave
the sync Running indefinitely — and a Running operation blocks all later auto-syncs
of that Application, including the commit that would fix the gate.

## Check type reference: `gates/`

- `all-check-types-gate.yaml` — every supported check type in one Gate.
- `observability-stack-gate.yaml` — cross-namespace checks, which require the
  Gate's namespace to be in `platform` mode and the targets to be on gatekeeper's
  allowlist.

## Trying it without Argo CD

The Gate's status is what everything reads, so you can exercise gatekeeper with
nothing but `kubectl`:

```bash
kubectl apply -f examples/wave-gating/gate.yaml

# Gatekeeper reconciles every 30s by default.
kubectl get gates -n nonprod
# NAME                  READY   REASON                   LAST-TRANSITION
# my-app-dependencies   False   DependenciesNotReady     10s

# Which check is blocking, and why:
kubectl get gate my-app-dependencies -n nonprod -o jsonpath='{.status.results}' | jq .

# Block until it passes, the same way a cross-Application hook would:
kubectl wait --for=condition=Ready --timeout=5m gate/my-app-dependencies -n nonprod
```

`REASON` distinguishes the cases that need different responses:

| Reason | Meaning |
| --- | --- |
| `Ready` | every check passed |
| `DependenciesNotReady` | still waiting; this should resolve on its own |
| `Terminal` | a failed Job, Degraded Application or stalled HelmRelease — it will not recover without a change, so stop waiting |
| `PolicyViolation` | the Gate references a namespace it isn't allowed to read |
| `ReadForbidden` | gatekeeper's own ClusterRole is missing a kind; the message names it |
| `InvalidSpec` | the Gate itself is malformed |
| `EvaluationFailed` | gatekeeper couldn't evaluate a check, so it makes no claim either way |
