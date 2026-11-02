# AGENTS.md

Guidance for AI coding assistants working in this repository.

## What this is

**GlueOps Core Gatekeeper** — a small Kubernetes controller that evaluates `Gate`
custom resources (`platform.glueops.dev/v1alpha1`) and publishes dependency
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
| `crds/gates.yaml` | the `Gate` CRD, including the CEL rule enforcing exactly one check type. In a directory so platform-crds can pin it as a kustomize source |
| `rbac.yaml` | ServiceAccount, ClusterRole, binding — **the ceiling on what any Gate can read** |
| `deploy.yaml` | Deployment and Service |
| `servicemonitor.yaml` | scrape config, shipped so the alerts' `job` label is a contract |
| `prometheusrule.yaml` | the staleness alerts (optional; needs the Prometheus Operator) |
| `prometheusrule_test.yaml` | promtool unit tests for those alerts — see the header for how to run them |
| `examples/wave-gating/` | the worked example a consumer copies |
| `examples/argocd-health-check.yaml` | the Lua health check for `argocd-cm` — **required**, see Decisions |
| `examples/argocd-health-check_test.lua` | tests for that script — run them before changing it, see the file header |

## Build and test

Everything runs in Docker; don't install Go on the host.

```bash
docker run --rm -v "$PWD":/w -w /w golang:1.26-alpine sh -c 'go build ./... && go vet ./... && go test ./...'

# -race needs cgo, so use the Debian image for that
docker run --rm -v "$PWD":/w -w /w golang:1.26 go test -race ./...
```

**`go test` is one of three suites.** The other two guard files that fail silently —
a broken Lua health check hangs waves showing `Unknown`, a broken alert expression
simply never fires. CI runs all three (`.github/workflows/ci.yaml`), but run the
relevant one locally too when you touch the file it covers. Both live inside YAML and
need extracting first; the exact commands are in each test's own header, kept there
rather than duplicated here:

| Touching | Run |
| --- | --- |
| `examples/argocd-health-check.yaml` | `examples/argocd-health-check_test.lua` — under 5.1 *and* 5.4 |
| `prometheusrule.yaml` | `prometheusrule_test.yaml` (promtool) |

End-to-end against a throwaway cluster:

```bash
k3d cluster create gatekeeper-test
docker build -t platform-gatekeeper:dev . && k3d image import platform-gatekeeper:dev -c gatekeeper-test
kubectl apply -f crds/gates.yaml -f rbac.yaml -f deploy.yaml
kubectl set image deployment/glueops-core-gatekeeper gatekeeper=platform-gatekeeper:dev -n glueops-core-gatekeeper
```

That gives you a working controller but **not** working gating: Argo CD also needs
the Lua health check (Decision 4), without which a Gate silently gates nothing. To
actually exercise a gated wave in that cluster:

```bash
kubectl create namespace argocd
# --server-side: the ApplicationSet CRD is too large for client-side apply
kubectl apply -n argocd --server-side --force-conflicts \
  -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
kubectl patch configmap argocd-cm -n argocd --type merge \
  --patch-file examples/argocd-health-check.yaml
kubectl -n argocd rollout status statefulset/argocd-application-controller
```

Then point an Application at a copy of `examples/wave-gating/` in a repo you control
(the shipped `repoURL` is a placeholder) with its dependency scaled to 0, and confirm
the sync holds at wave `-1`.

One thing makes a gating test silently measure nothing, and it cost real time here:
`argocd.argoproj.io/ignore-healthcheck` left on the Gate. And a cluster that once ran
the old `:latest` manifest keeps `imagePullPolicy: Always`, so patch it to
`IfNotPresent` before `kubectl set image` to an imported build.

The CRD schema changed incompatibly without a version bump, which `v1alpha1`
permits. The version never leaves `spec.versions`, so there is no
`status.storedVersions` migration, and already-stored Gates simply have the removed
fields pruned on their next write.

**But a manifest still carrying `spec.strict` is REJECTED, not pruned** — verified:
`kubectl apply` gives `strict decoding error: unknown field "spec.strict"` and
server-side apply gives `.spec.strict: field not declared in schema`. Only
`--validate=false` prunes silently. Since the CRD bundle lands before Argo CD, the
ordering is a requirement, not a nicety: **tenant Git must drop `spec.strict` before
the new CRD ships**, or every Application carrying one fails to sync with no rollback
path but fix-forward.

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

1. Add the schema block to `crds/gates.yaml` **and** the key to the CEL rule's list.
2. Add the key to `countCheckTypes()` in `main.go`.
3. Add a branch to `evalOne()` and a pure predicate function.
4. Add the kind to `rbac.yaml`, the table in `README.md`, and
   `examples/gates/all-check-types-gate.yaml` (which `examples/README.md` advertises
   as showing every type).
5. Table-test the predicate in `main_test.go`, **and add a row to
   `TestEveryCheckTypeDispatches`** covering each option name. A predicate test
   alone is not enough: the option names and GVRs in `evalOne` are string literals
   hand-matched against `crds/gates.yaml`, and a typo there fails silently — the key lookup
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
- **Gates are evaluated serially and the client is capped at 50 QPS / 100 burst.**
  Measured on k3d: 206 two-check Gates sweep in ~6.4s steady-state, about 64 reads/s,
  so past roughly 250 two-check Gates the rate limiter starts pacing sweeps and
  staleness grows linearly with the fleet. Around 470 maximum-size Gates a sweep
  would exceed the alert's 300s threshold. Raise the threshold, or parallelise
  evaluation, before that point.
- **`fluxHelmReleaseReady` pins `helm.toolkit.fluxcd.io/v2`**, served by Flux 2.3 and
  later. On an older Flux (v2beta1/v2beta2) the read 404s and the Gate reports "not
  found" forever rather than saying the version is wrong. Not handled because this
  platform does not run Flux; if that changes, probe the older versions.

## Decisions

Settled choices, with their accepted edge cases. Don't re-raise these without new
information.

1. **Published status, not a request API.** The HTTP `/check` and `/explain`
   endpoints, TokenReview authentication and per-caller SubjectAccessReview were all
   removed in this change. The driver was consumer ergonomics: the old design required
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
5. **`--sync-timeout` is optional; `argocd app terminate-op` is the accepted
   recovery.** It defaults to never, so a gate that never goes ready leaves the
   operation `Running`, which blocks later auto-syncs of that Application including
   the fix. Setting it turns that hang into a visible `Failed` instead. The platform
   team runs without it deliberately — they have used `terminate-op` for this before
   and are comfortable with the manual step — so don't re-raise it as a prerequisite.
   It is also controller-wide with no per-Application override, so a value below the
   longest legitimate sync in the cluster breaks unrelated Applications.
   **What `terminate-op` does not cover:** an operation that has already *failed*,
   which is what the `Degraded` reasons in Decision 15 produce. That case needs
   the retry budget, a manual sync, or a new commit — see the auto-sync bullet under
   "Argo CD interactions worth knowing".
6. **Terminal means the upstream controller has declared it stopped trying** — not
   "this looks unhealthy right now". Only a Job's `Failed=True` and a Flux
   HelmRelease's `Stalled=True` qualify. A declarative `terminalWhen` field was
   rejected: it pushes per-technology trivia into tenant YAML and can't express the
   Job case at all. This definition scopes the per-check `terminal` flag only; which
   *reasons* read `Degraded` is Decision 15.
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
10. **This model assumes tenants cannot set namespace labels.** The
    `customer`/`platform` mode label is the policy boundary, so a tenant who can set
    it promotes themselves to platform mode. That means `patch` AND `update` on an
    existing namespace, and `create` of a new one -- an RBAC review looking only for
    `patch` misses two of the three.
    **Argo CD can set it on a tenant's behalf** — an Application with
    `CreateNamespace=true` and `managedNamespaceMetadata.labels` — but only in a
    project that permits the cluster-scoped `Namespace` kind. Measured on v3.5.6: a
    project without it refuses the sync, for a new namespace and an existing one
    alike. So tenant AppProjects must keep `Namespace` out of their cluster-resource
    whitelist (the GlueOps tenant template blacklists every cluster-scoped kind) and
    must never list a platform-mode namespace as a destination. Requiring
    platform-mode Gates to live in an allowlisted namespace would not help: tenant
    environment namespaces are allowlisted targets.
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
    `crds/gates.yaml` and ClusterRole, so the knob served a scenario that can't occur.
15. **Four reasons read `Degraded`, the rest `Progressing`.** The health check's
    `stuck` table holds `Terminal`, `PolicyViolation`, `ReadForbidden` and
    `InvalidSpec` — every one a verdict that the Gate needs an edit to its spec, to
    the namespace's mode label, or to gatekeeper's ClusterRole. `EvaluationFailed`
    (a read failed, so no claim is made about the dependency) and
    `DependenciesNotReady` (the normal waiting state) must stay `Progressing`;
    `TestHealthCheckScriptMatchesTheReasonContract` enforces both halves.
    **What this costs, measured against Argo CD v3.5.6 — and it is not free.**
    `Degraded` fails the sync operation, and a *failed* operation blocks auto-sync of
    that revision (`Skipping auto-sync: failed previous sync attempt to [rev] and will
    not retry`). Recovery comes from the automated operation's own retry budget — 5
    retries, ~5½ min — and **not** from `selfHeal`: in `autoSync` the
    `!lastAttemptedPhase.Successful()` check returns before the selfHeal branch. Two
    Applications identical but for `selfHeal` produced byte-identical timelines; with
    the fix applied inside the window one recovered in ~35s *with `selfHeal: false`*,
    and with the fix applied after it neither recovered in 7+ minutes.
    So a `Degraded` Gate gives you ~5½ minutes of self-recovery and then needs a
    manual sync or a new commit. Three of the four reasons are fixed cluster-side with
    no commit — `ReadForbidden` (grant a kind), `PolicyViolation` (relabel the
    namespace or change the allowlist) and `Terminal` (delete the failed Job) — so
    remediation usually lands *after* the budget and does not release it by itself.

    The `Progressing` path has no such limit: a Running operation keeps re-checking
    the wave, so with `--sync-timeout` at its default (Decision 5) a fix at any point
    lets the sync proceed. `Progressing` is therefore strictly more recoverable, and
    this decision trades that away for the only signal there is — a stuck
    `Progressing` gate looks like an ordinary slow deploy forever and nothing alerts,
    while `Degraded` surfaces on the Application and fires notifications. Sound
    specifically because these four reasons never clear on their own, so the
    unbounded grace window buys them nothing.
    (Three earlier versions of this note were wrong: that both paths reach "the same
    blocked state"; that the window was 30 minutes; and that `selfHeal: true` was what
    provided recovery. The last was measured, with the two arms fixed at different
    times, which made them incomparable.)
    `InvalidSpec` is near-unreachable in practice (`required` + `minLength: 1` on
    every check field, `required: ["checks"]`, `items: type: object`, and the CEL
    rule block every path that emits it); it is in the table for uniformity, so the
    rule reads as one sentence rather than a list of exceptions.
16. **Accepted: gatekeeper's ClusterRole is every Gate author's read permission.**
    This is what removing per-caller SubjectAccessReview costs, beyond Decision 13's
    audit attribution:
    - Anyone who can create or update a Gate learns, for every kind in `rbac.yaml`
      that the Gate can reach, whether a named object exists, its replica, pod and
      endpoint counts, whether pods match an arbitrary selector (so label values can
      be probed), Job and Flux condition reasons, and Argo CD health and sync — all
      in `status.results`, readable by anyone who can read the Gate, and in the
      Ready message the Argo CD UI shows to anyone the AppProject lets see the app.
    - The cross-namespace limit is one layer, the mode label plus the allowlist
      (Decision 10), where SAR used to be a second.
    - Widening `rbac.yaml` grants every Gate author at once. Review it as a
      tenant-facing privilege change, not internal plumbing.
    - `create gates` is effectively read access to those kinds, so never aggregate
      `gates` into the `edit` or `admin` roles.
    **Deferred:** a `ValidatingAdmissionPolicy` calling `authorizer` to check the
    Gate author's own RBAC at write time. Under GitOps the requester is Argo CD's
    controller, not the tenant, unless AppProjects use `destinationServiceAccounts`,
    which these don't.
17. **Settled in review.**
    - **Rejected: duplicate check ids wedging status writes after the upgrade.**
      Measured: a Gate stored under the old CRD with two `id: db` checks kept being
      written (`0 failed`), because validation ratcheting accepts the duplicates on an
      object that already had them; a new Gate is rejected at create.
    - **Deferred: one Gate's persistently failing status write withholds `lastSweep`
      and `/readyz` cluster-wide.** With NotFound and Conflict skipped, the only
      trigger found is a deliberately oversized Gate near the object size limit.
      `maxLength` on `name` and `selector` would close it, at the cost of a CRD change.

## Ownership

Maintained by the GlueOps core platform team. Changes here affect whether production
rollouts are correctly gated — review with multi-tenancy and failure modes in mind.

## Argo CD interactions worth knowing

Verified against upstream, and easy to get wrong:

- **`argocd.argoproj.io/ignore-healthcheck` is deliberately NOT set** in the shipped
  example. It removes the Gate from the Application health rollup while leaving wave
  gating intact (the sync engine reads the Gate's health directly and ignores the
  annotation) — but measured against the same blocked Gate, the Application reads
  `Progressing` without it and `Healthy` with it. An Application reporting Healthy
  while its declared dependency is down is the worse default. It remains the opt-out
  for teams where a `Degraded` Gate left in place keeping the Application `Degraded`
  matters more — most often one reporting `ReadForbidden`, not a failed Job.
- **`selfHeal` is irrelevant to gate-failure recovery; the retry budget is what
  matters.** Measured: a *failed* sync operation blocks auto-sync of that revision
  (`Skipping auto-sync: failed previous sync attempt to [rev] and will not retry`),
  and `autoSync` returns on `!lastAttemptedPhase.Successful()` *before* the selfHeal
  branch. What recovers a gated sync is the automated operation's injected
  `Retry{Limit: 5}` with exponential backoff, ~5½ min: fix the dependency inside it
  and the sync succeeds ~35s later even with `selfHeal: false`; fix it after and
  neither setting recovers. The way out then is a manual sync (works at the same
  revision) or a new commit.
- **Mid-sync an Application already reads `Synced` and `Healthy`.** Measured: re-syncing
  an already-synced, hooked, multi-wave app, sync and health never left
  `Synced`/`Healthy` for the whole time `operationState.phase` was `Running` (a first
  sync does go `OutOfSync` first). So
  `argoApplicationHealthy` also requires no operation requested, running, or
  finished-but-unreconciled — the rule `argocd app wait` applies. Health and sync
  alone release a Gate before its dependency has finished syncing.
  A last sync that ended in any phase but `Succeeded` also blocks (dry runs aside),
  because a failed PostSync hook leaves the Application `Synced` and `Healthy`. It is
  not `Terminal`, but nothing re-syncs a `Synced` app on its own: recovery is a manual
  sync, or a commit that changes a non-hook resource.
- **A wedged Application can read `Healthy`.** Measured: after the dependency was
  fixed, the Application showed `health=Healthy` with `sync=OutOfSync` and the gated
  Deployment absent — the Gate was the only live resource, so the rollup followed it.
  Anything alerting on Application *health* rather than sync status sees nothing.
- **`argocd app terminate-op` only helps a *Running* operation.** It does nothing once
  the operation has already failed, which is the state a `Degraded` Gate produces.
  Recovery there is a new commit or a manual sync.
- **PreSync hooks are never gated.** Measured: a PreSync Job was already `Running`
  while the Gate did not yet exist. Hooks run before the waves that contain the Gate,
  so a migration Job — the one workload Decision 2 names as the dangerous
  irreversible case — is completely ungated. Don't put anything in a PreSync hook
  that a Gate is supposed to protect.
- **A blocked Gate also blocks deletion — unless the Gate goes with it.** With
  `prune: true`, removing a gated resource from Git while the Gate is `False` pins the
  operation on the Gate's health and the prune never runs. Removing the Gate *in the
  same commit* works, and so does removing a Gate on its own: **a prune's wave barrier
  is "object gone", not "object healthy"**, so a Gate's own `False` never blocks its
  own deletion. Both measured; README has the procedure.
  **Why it works, which is not what "reverse prune order" suggests.** `getSyncTasks`
  in gitops-engine doesn't negate prune waves, it *reflects the sorted list of waves
  that currently contain prunes onto itself* (odd counts leave the middle fixed). So
  with prunes at `{-1, 0}` the two swap and the wave-`0` workload is deleted before
  the wave-`-1` Gate. Three consequences worth knowing:
  - Effective prune waves are always values already present in that set — prunes at
    `{-5, 0}` swap to `0` and `-5`, never `5`.
  - Those values share the numeric space with create/update tasks, so prunes and
    creates interleave and can tie. **Don't add resources in the same commit that
    removes a Gate** and assume they're ordered after it.
  - The overrides are recomputed every pass from whichever prunes are still
    outstanding, so they aren't stable. It is also why deleting a Gate alone works:
    as the only prune there is nothing to swap with, and it runs at its natural wave.
- **`resource.exclusions` matching `gates` silently un-gates everything.** An
  excluded Gate is stripped from the target objects, so it is never applied, never
  produces a sync task, and the next wave runs immediately. The only signal is an
  `ExcludedResourceWarning` condition.
- **Removing the health-check key from `argocd-cm` un-gates with *zero* signal.**
  Measured: the sync `Succeeded`, the workload was created, the dependency was absent,
  and the Gate's `health.status` was simply empty with the Application reading
  Healthy. This is the quietest way to break gating, and unlike `resource.exclusions`
  it leaves no condition behind.
- **Wave gating is edge-triggered.** Once a wave is observed healthy Argo CD advances
  and does not recheck, so a single Unknown→True→Unknown flap releases the next wave
  for that sync.
- **A Lua *syntax* error is loud, not silent.** Measured: the operation goes
  `phase=Error` with a `parse error` message, the workload is not created, and the
  health rollup stays poisoned until a hard refresh. That is better than the hazard
  originally assumed here — a runtime error on an unexpected object shape is the one
  that yields health `Unknown`, which the sync loop ignores while the wave hangs with
  nothing alerting. So `examples/argocd-health-check_test.lua` covers every branch of
  the script, the generation guard in both directions, and every reason in and out of
  the `stuck` table; run it before changing the script. (No counts here on purpose —
  "eleven branches" and then "sixteen cases" both rotted within a day, and nothing
  enforces a number in prose.) Mutation-checked: removing the generation guard makes the stale-`True`
  case return `Healthy`, which is the bypass it exists to prevent, and deleting the
  message-assembly line changes no status at all — which is why the cases carrying a
  condition assert the message too.
- **Argo CD's Lua is gopher-lua, a 5.1 dialect, sandboxed with `SkipOpenLibs`** and
  only package/base/table loaded. So the script may use nothing beyond `ipairs` and
  `..` — no `string` library — and no 5.2+ syntax. The suite compiles the script with
  `loadstring or load` so it runs under a 5.1 interpreter, which is what catches the
  syntax half of that; run it under `nickblah/lua:5.1-alpine` as well as 5.4. The
  harness itself is more permissive than Argo CD (it has the string metatable), so a
  case that indexes a non-table would pass locally and error in production.
