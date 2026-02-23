# AGENTS.md

This repository contains **GlueOps Core Gatekeeper**, a small Go HTTP service that evaluates Kubernetes dependency “gates” defined by a `Gate` CRD. This file is intended to help AI coding assistants understand the system, constraints, and expected workflows.

---

## Project summary

Gatekeeper exposes a minimal HTTP API used primarily by deployment tooling (e.g. Argo CD hook Jobs) to **block or allow progression** of a rollout based on Kubernetes resource readiness.

Key properties:

- **AuthN**: Kubernetes `TokenReview` of a bearer token (typically a ServiceAccount token).
- **AuthZ**: Kubernetes `SubjectAccessReview` for each requested action (delegated authorization).
- **Policy modes**: Determined by a namespace label:
  - `gatekeeper.platform.glueops.dev/mode=customer|platform`
- **Gate source of truth**: A namespaced `Gate` custom resource (`platform.glueops.dev/v1alpha1`).
- **Checks**: Allowlisted resource checks (Deployment, StatefulSet, Job, Service endpoints, Pod selector, etc).
- **Expected client**: An Argo CD PreSync hook Job (or similar) that polls until 200 OK.

---

## Repository conventions

### Primary runtime
- **Language**: Go
- **Runtime**: Kubernetes (in-cluster), but should also run locally against a kubeconfig for development.

### API behavior conventions
- `/healthz` → 200 OK, empty body
- `/check` → returns:
  - **200** when gate passes
  - **409** when checks are not ready (retryable)
  - **4xx** on invalid spec / policy violation / auth errors
- `/explain` → returns 200 with JSON (even if not ready), useful for debugging

### JSON responses
- Always return `Content-Type: application/json` for successful JSON responses.
- Plain-text errors are acceptable for now, but prefer structured JSON error bodies in future changes.

---

## Kubernetes model

### Gate CRD
- Group/version: `platform.glueops.dev/v1alpha1`
- Kind: `Gate`
- Scope: Namespaced
- Gate status is updated using the CRD status subresource (`gates/status`).

**Important**: A check item must set **exactly one** check type. Assistants should enforce this in code.

### Namespace policy modes
Mode is determined from the **caller namespace** label:

```yaml
gatekeeper.platform.glueops.dev/mode: customer|platform
```

### Expected behavior:

- customer mode
  - Gate lookup must be within caller namespace
  - dependency checks must remain within the Gate’s namespace
- platform mode
  - optional cross-namespace Gate lookup via ?ns=<namespace>
  - optional cross-namespace dependency checks
  - cross-namespace is restricted by allowlists (see env vars below)

### Platform allowlists

Platform mode uses allow rules based on:
- exact namespace allowlist
- prefix allowlist (e.g. glueops-core- matches glueops-core-*)

### Environment variables:
- GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES (CSV)
- GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES (CSV)

## Security requirements (do not regress)

1. Never trust request parameters alone
  - Always authenticate via TokenReview.
  - Always authorize via SubjectAccessReview (SAR) for:
    - reading the Gate (in platform mode when ns differs)
    - each check’s resource access
2. No “kubectl exec” / no shelling out
  - Use client-go.
3. Allowlist check types
  - Do not add “generic resource query” endpoints that allow arbitrary GVR access.
4. No cross-namespace in customer mode
  - Even if requested by query params.
5. Avoid data leakage
  - Never return resource contents. Return only readiness booleans and human-safe messages.

## Development workflows
### Local run
- go run . uses:
  - in-cluster config if available
  - otherwise falls back to KUBECONFIG

### Testing tips
 - Use curl -i when debugging to see non-JSON error bodies and HTTP status.
 - To simulate a real client:
    - create a ServiceAccount token with kubectl create token
    - call /check?gate=<name>[&ns=<gate-ns>] with Authorization: Bearer <token>

## What AI assistants should do when modifying code
### Preferred changes
- Keep HTTP surface area minimal.
- Add new check types by:
  - updating CRD schema
  - implementing evaluation in Go
  - adding SAR checks and RBAC documentation
- Improve error reporting:
  - differentiate 400 (bad spec), 403 (forbidden), 409 (not ready), 404 (missing gate)

### Avoid
- Introducing broad RBAC requirements (“cluster-admin”-like)
- Removing SAR enforcement
- Adding endpoints that accept arbitrary resource kinds/namespaces from user input without strict policy & authorization

## Suggested future enhancements (safe direction)
- Structured JSON error responses
- Metrics endpoint (/metrics) with Prometheus counters
- Admission validation for Gate objects (reject unsafe specs at creation time)
- Implement argoApplicationHealthy check type (if required) using allowlisted access

## Contact / ownership
- Maintained by GlueOps core platform team.
- This service is security-sensitive: changes must be reviewed with multi-tenancy and RBAC implications in mind.