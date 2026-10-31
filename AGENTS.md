# AGENTS.md

Guidance for AI coding assistants working in this repository.

## What this is

**GlueOps Core Gatekeeper** — a small Kubernetes controller that evaluates `Gate`
custom resources (`platform.glueops.dev/v1alpha2`) and publishes dependency
readiness to `Gate.status`. Argo CD reads that status as resource health, so a sync
wave can hold a rollout until the dependencies are up.

It is a **controller, not a service**. There is no request API: nothing calls
gatekeeper to ask a question. It lists Gates on a timer, evaluates each check, and
writes a `Ready` condition plus a per-check breakdown. The only HTTP endpoints are
`/healthz` (a plain process check for the kubelet) and `/metrics` (one gauge).

Gatekeeper reads dependencies with **its own** ServiceAccount. Callers have no
credentials and need no RBAC.

## Repo map

| Path | What |
| --- | --- |
| `main.go` | all application code — reconcile loop, evaluation, readiness predicates, status writing |
| `main_test.go` | unit tests for the predicates plus reconcile-level tests on fake clients |
| `crd.yaml` | the `Gate` CRD, including the CEL rule enforcing exactly one check type |
| `rbac.yaml` | ServiceAccount, ClusterRole, binding — **the ceiling on what any Gate can read** |
| `deploy.yaml` | Deployment and Service |
| `prometheusrule.yaml` | the single staleness alert (optional; needs the Prometheus Operator) |
| `examples/wave-gating/` | the worked example a consumer copies |
| `examples/argocd-health-check.yaml` | the Lua health check for `argocd-cm` — **required**, see Decisions |

## Build and test

Everything runs in Docker; don't install Go on the host.

```bash
docker run --rm -v "$PWD":/w -w /w golang:1.26-alpine sh -c 'go build ./... && go vet ./... && go test ./...'

# -race needs cgo, so use the Debian image for that
docker run --rm -v "$PWD":/w -w /w golang:1.26 go test -race ./...
```

End-to-end against a throwaway cluster:

```bash
k3d cluster create gatekeeper-test
docker build -t platform-gatekeeper:dev . && k3d image import platform-gatekeeper:dev -c gatekeeper-test
kubectl apply -f crd.yaml -f rbac.yaml -f deploy.yaml
```

## How evaluation is structured

- `reconcileOnce` lists Gates and calls `reconcileGate` per Gate (bounded by a
  per-Gate timeout, with `recover()` so one bad Gate can't take down the sweep).
- `evaluateGate` walks `spec.checks` and returns `[]EvalResult`. It enforces the
  namespace policy before any read.
- `evalOne` dispatches on check type and classifies read failures:
  `IsForbidden` → `ReadForbidden`, `IsNotFound` → not-ready, anything else →
  `EvaluationFailed`.
- `summarize` is the **only** place the condition's status, reason and message are
  derived, applying a fixed reason precedence. Add new reasons there, not inline.
- Readiness predicates (`deploymentReady`, `jobComplete`, …) are pure functions over
  an API object. Keep them that way — they're the best-tested part of the codebase.

### Adding a check type

1. Add the schema block to `crd.yaml` **and** the key to the CEL rule's list.
2. Add the key to `countCheckTypes()` in `main.go`.
3. Add a branch to `evalOne()` and a pure predicate function.
4. Add the kind to `rbac.yaml`, and to the table in `README.md`.
5. Table-test the predicate in `main_test.go`.

If the dependency can enter a state it will never leave without human action, the
predicate must report `terminal` — see Decisions.

## Rules

- **Never return resource contents.** Status carries readiness booleans and
  human-safe messages only. Don't echo arbitrary upstream fields into a Gate's
  status; a Gate is readable by anyone who can read its namespace.
- **Never grant `secrets` or `configmaps`** in `rbac.yaml`. That ClusterRole is the
  only thing bounding what a Gate can observe.
- **Enforce the namespace policy before reading.** `customer` mode is
  namespace-local; `platform` mode is allowlist-bounded. Both allowlists default to
  empty and must stay that way.
- **A read failure is not a "not ready" answer.** Transient errors must produce
  `Ready=Unknown`, never `False`, or one API blip flips a healthy Gate and interrupts
  a wave that was about to proceed.
- **Preserve `lastTransitionTime` unless the status actually changes.** It means "when
  this last changed", and restamping it every sweep turns reconciliation into
  continuous writes that wake every Argo CD watcher.
- **Keep `Server` immutable after `main()`.** The reconcile loop owns all state; the
  only shared field is `lastSweep` (an atomic). Don't add mutable fields — put
  per-sweep state in locals.
- Use client-go; never shell out.

## Decisions

Settled choices, with their accepted edge cases. Don't re-raise these without new
information.

1. **Published status, not a request API.** The HTTP `/check` and `/explain`
   endpoints, TokenReview authentication and per-caller SubjectAccessReview were all
   removed in `v1alpha2`. The driver was consumer ergonomics: the old design required
   a PreSync hook Job plus a ServiceAccount, Role and RoleBinding per app, each
   carrying a copy of a shell retry loop.
2. **Accepted: readiness can be stale.** A gatekeeper that is down leaves Gates at
   their last value, and neither Argo CD nor `kubectl wait` can tell how old that is,
   so a rollout gated on an already-`Ready` Gate may proceed unverified. There is no
   Kubernetes primitive that fixes this — no per-object TTL, no admission on read
   paths. A heartbeat Lease plus a watchdog that marks Gates stale was designed and
   **rejected**: it bought about a minute of detection over ordinary Deployment
   alerting while becoming the only component able to block every rollout in the
   cluster. The mitigation is one alert (`prometheusrule.yaml`).
   **The open edge case:** a gate protecting something irreversible, such as a
   migration an app must not start before, does not self-heal. Nothing in the tooling
   prevents writing one. If that comes up, add a synchronous check that fails closed
   rather than reintroducing a watchdog.
3. **Accepted: no cross-Application gating.** Sync waves only order resources within
   one Application. A PreSync Job running `kubectl wait` covers it, but only if the
   Gate already exists, so it isn't shipped as a supported pattern.
4. **The Argo CD Lua health check is required, not optional.** Argo CD skips
   resources with no health check in its health rollup, so without
   `examples/argocd-health-check.yaml` a Gate gates nothing. Its `observedGeneration`
   guard is also required: without it an edited Gate is judged against the condition
   left from its previous spec and gets bypassed on the very sync that tightened it.
5. **`--sync-timeout` must be set cluster-wide.** It defaults to never, and a Running
   sync operation blocks later auto-syncs of that Application including the fix. This
   is also what turns a hung wave into a visible failure.
6. **Terminal is distinct from not-ready, and derived in Go.** A failed Job, a
   Degraded Application and a stalled HelmRelease will not recover on their own, so
   they surface as `reason: Terminal` → Argo CD `Degraded` → prompt sync failure. A
   declarative `terminalWhen` field was rejected: it pushes per-technology trivia into
   tenant YAML and can't express the Job case at all.
7. **`spec.strict` was removed.** It claimed non-strict Gates could pass with skipped
   checks, which the code never did. Every check is always evaluated and reported, and
   a check that cannot be evaluated always blocks the Gate.
8. **No generic "check any resource" type.** Each check type is explicit. A generic
   `condition` check was rejected: it needs a GVR allowlist, cluster-scope rejection
   and message redaction to be safe, which is a poor trade for the two technologies
   actually in use. Add a concrete check type instead — it's ~25 lines.
9. **Rejected: per-check status-write diffing.** At this scale always patching is
   ~0.3 writes/s and Argo CD already suppresses redundant requeues, while diffing
   introduced a silent fail-stuck bug. Inherit `lastTransitionTime`; write everything
   else unconditionally.
10. **This model assumes tenants cannot `patch` namespaces.** The `customer`/`platform`
    mode label is the policy boundary, so a tenant able to relabel their own namespace
    could promote themselves to platform mode.

## Ownership

Maintained by the GlueOps core platform team. Changes here affect whether production
rollouts are correctly gated — review with multi-tenancy and failure modes in mind.
