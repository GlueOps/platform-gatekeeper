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
`/healthz` (a plain process check for the kubelet) and `/metrics` (three gauges).

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
| `servicemonitor.yaml` | scrape config, shipped so the alerts' `job` label is a contract |
| `prometheusrule.yaml` | the staleness alerts (optional; needs the Prometheus Operator) |
| `prometheusrule_test.yaml` | promtool unit tests for those alerts — see the header for how to run them |
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
kubectl set image deployment/glueops-core-gatekeeper gatekeeper=platform-gatekeeper:dev -n glueops-core-gatekeeper
```

That gives you a working controller but **not** working gating: Argo CD also needs
the Lua health check and `--sync-timeout` (Decisions 4 and 5), without which a Gate
silently gates nothing. See README's Install section.

Note `kubectl apply -f crd.yaml` fails on a cluster that already has `v1alpha1`
installed — see README's Upgrading section.

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
4. Add the kind to `rbac.yaml`, the table in `README.md`, and
   `examples/gates/all-check-types-gate.yaml` (which `examples/README.md` advertises
   as showing every type).
5. Table-test the predicate in `main_test.go`, **and add a row to
   `TestEveryCheckTypeDispatches`** covering each option name. A predicate test
   alone is not enough: the option names and GVRs in `evalOne` are string literals
   hand-matched against `crd.yaml`, and a typo there fails silently — the key lookup
   misses, `numOr`/`boolOr` return the default, and the predicate test still passes
   because it takes the value as an argument.

Only report `terminal` if the dependency's own controller has declared it stopped
trying — see Decision 6.

## Rules

- **Never return resource contents.** Status carries readiness booleans and
  human-safe messages only. A Gate is readable by anyone who can read its namespace,
  and the Argo CD health check republishes its condition message into the UI, where
  visibility is governed by AppProject RBAC — usually a much wider audience.
  Concretely: echo only strings the resource's *own controller* draws from a bounded
  vocabulary — a condition `reason`, an enum like Argo CD's `health.status`. Never
  echo a condition `message`: Flux's wraps a Helm error, which wraps whatever
  rejected the release, and admission webhooks routinely quote the offending
  object's field values. Use the `withReason` helper.
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
- **Keep `Server` immutable after `main()`** apart from the three published metrics
  (`lastSweep`, `gatesUnknown`, `sweepDuration`), which are atomics written by the
  sweep and read by `/metrics`. Per-sweep state goes in locals, not fields.
- **The metric names are a contract with `prometheusrule.yaml`.** Renaming either
  side silently disables the alerts; `TestHandleMetricsExposesTheAlertedSeries`
  guards it.
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
   cluster. The mitigation is alerting instead (`prometheusrule.yaml`), on sweep
   freshness and on Gates stuck unevaluated.
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
6. **Terminal means the upstream controller has declared it stopped trying** — not
   "this looks unhealthy right now". Only a Job's `Failed=True` and a Flux
   HelmRelease's `Stalled=True` qualify; they surface as `reason: Terminal` → Argo CD
   `Degraded` → prompt sync failure instead of waiting out `--sync-timeout`. A
   declarative `terminalWhen` field was rejected: it pushes per-technology trivia into
   tenant YAML and can't express the Job case at all.
   **Argo CD health values are deliberately never terminal.** Health is a rollup over
   an Application's resources: `Degraded` and `Progressing` both occur mid-rollout,
   and `Missing` is the normal state of an Application whose resources aren't live
   yet — which is exactly what a Gate guarding a not-yet-bootstrapped platform stack
   should wait for. An earlier version treated `Degraded` and `Missing` as terminal
   and inverted `examples/gates/observability-stack-gate.yaml` on a cold bootstrap:
   the Gate failed the rollout it was supposed to hold.
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
11. **A Gate must express steady state, not an event.** It is a managed resource in
    wave `-1` of *every* sync, re-evaluated each time — not a one-shot precondition.
    So the gated condition has to stay true for the life of the Application. Gating
    on a migration `jobComplete` is the trap: it works until `ttlSecondsAfterFinished`
    or a `HookSucceeded` hook removes the Job, after which every later sync of that
    Application hangs at wave `-1` with auto-sync skipped, including the fix-forward
    commit. `jobComplete` is still shipped, with the caveat documented at its check
    type and in `examples/wave-gating/gate.yaml`.
12. **Readiness counts backends, not addresses.** `serviceReadyEndpoints` takes the
    maximum across address families rather than summing EndpointSlices: a dual-stack
    Service has one slice per family describing the *same* pods, and
    `Endpoints[].Addresses` holds alternate addresses for a *single* endpoint. Summing
    either would satisfy `minReadyAddresses` with fewer real backends than asked for.
13. **Accepted: no per-Gate attribution in logs.** Removing per-caller
    SubjectAccessReview means the API server audit log attributes every dependency
    read to gatekeeper's ServiceAccount. An earlier version logged one line per Gate
    per sweep to compensate, but it only logged check *ids* — which are static in
    `spec.checks` and already published to `status.results` — at ~2,900 lines per
    Gate per day. Logging is now transition-only. Per-Gate attribution comes from
    the Gate's own spec and `status.results`; if a time-ordered record is ever
    needed, that's a reason to add Events rather than to restore the log line.
14. **Rejected: configurable Gate GVR.** `GATEKEEPER_GATE_{GROUP,VERSION,RESOURCE}`
    were removed. A different group or plural means a different CRD, hence a different
    `crd.yaml` and ClusterRole, so the knob served a scenario that can't occur.

## Ownership

Maintained by the GlueOps core platform team. Changes here affect whether production
rollouts are correctly gated — review with multi-tenancy and failure modes in mind.

## Argo CD interactions worth knowing

Verified against upstream, and easy to get wrong:

- **The Gate is kept out of the Application health rollup** by
  `argocd.argoproj.io/ignore-healthcheck: "true"` in the shipped example. Argo CD's
  app controller honours that annotation; the sync engine reads the Gate's health
  directly and ignores it, so wave gating and terminal fail-fast still work. Without
  the annotation, a Gate left blocked or terminal after a rollout keeps the whole
  Application `Degraded` while its workloads are fine.
- **`resource.exclusions` matching `gates` silently un-gates everything.** An
  excluded Gate is stripped from the target objects, so it is never applied, never
  produces a sync task, and the next wave runs immediately. The only signal is an
  `ExcludedResourceWarning` condition.
- **Wave gating is edge-triggered.** Once a wave is observed healthy Argo CD advances
  and does not recheck, so a single Unknown→True→Unknown flap releases the next wave
  for that sync.
- **A Lua error yields health `Unknown`**, which the sync loop ignores — the wave
  hangs with the Application showing Unknown rather than Degraded, and nothing
  alerts. Test `examples/argocd-health-check.yaml` against a real Gate object before
  shipping a change to it.
