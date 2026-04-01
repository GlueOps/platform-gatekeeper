# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

GlueOps Core Gatekeeper — a Go HTTP service that acts as a deployment dependency gate for Kubernetes. It evaluates `Gate` custom resources (CRD: `platform.glueops.dev/v1alpha1`) and returns readiness status. Primary consumers are Argo CD PreSync hook Jobs that poll until dependencies are ready.

## Build and Run

```bash
# Run locally (requires kubeconfig or in-cluster config)
go run .

# Build binary
go build -trimpath -ldflags="-s -w" -o gatekeeper .

# Build container
docker build -t gatekeeper .
```

Configuration is via `.env` file (loaded by godotenv) or environment variables. See `.env.example`.

## Architecture

**Single-file Go service** (`main.go`) with tests in `main_test.go`. All logic is in one file:

- **HTTP layer**: Three endpoints (`/check`, `/explain`, `/healthz`) on a standard `net/http` mux
- **Authentication**: Kubernetes `TokenReview` — only ServiceAccount tokens are accepted (username must be `system:serviceaccount:<ns>:<name>`)
- **Authorization**: Kubernetes `SubjectAccessReview` (SAR) before every resource read — the caller SA must have RBAC to read the resources referenced in Gate checks
- **Gate evaluation**: Loads a `Gate` CR via dynamic client, iterates `spec.checks`, evaluates each against the Kubernetes API
- **Namespace policy**: Determined by label `gatekeeper.platform.glueops.dev/mode` on the caller's namespace:
  - `customer` (default): namespace-local only, no cross-namespace
  - `platform`: cross-namespace allowed if target is in allowlist/prefix list
- **Status update**: Best-effort patch to `Gate.status` subresource after each evaluation

**Supported check types** (each check must set exactly one):

| Check type | What it checks | K8s verb needed |
| --- | --- | --- |
| `deploymentAvailable` | Deployment Available condition + min replicas | `get` on `apps/deployments` |
| `statefulSetReady` | StatefulSet ready replicas + optional revision | `get` on `apps/statefulsets` |
| `jobComplete` | Job Complete/Failed condition | `get` on `batch/jobs` |
| `serviceReadyEndpoints` | Endpoint addresses via EndpointSlices | `get` services, `list` endpointslices |
| `podLabelReady` | Pod count by label selector | `list` on pods |
| `argoApplicationHealthy` | Argo CD Application health + sync status | `get` on `argoproj.io/applications` |

Gatekeeper fetches each resource individually by name — no `list` or `watch` needed except for pods and endpointslices which are collection queries. Caller SAs only need RBAC for the check types used in their Gate.

## Key Files

- `main.go` — all application code
- `main_test.go` — unit tests (run with `go test ./...`)
- `crd.yaml` — Gate CRD definition
- `rbac.yaml` — ClusterRole/ServiceAccount for the gatekeeper itself
- `gate.sh` — shell script for manual gate checking
- `examples/` — test scripts for customer-mode and platform-mode validation

## Security Constraints (Do Not Regress)

- Always authenticate via TokenReview; always authorize via SAR before reading any resource
- No cross-namespace access in customer mode, even if requested via query params
- Never return resource contents — only readiness booleans and human-safe messages
- No arbitrary GVR access — check types are explicitly allowlisted in `countCheckTypes()`
- Use client-go only — no shelling out
- RBAC should be minimal — only `get` for named resources, `list` for collection queries. Never grant `watch`, `list`, or broader verbs than needed.

## Adding a New Check Type

1. Add schema to `crd.yaml` under `spec.checks.items.properties`
2. Add the key to `countCheckTypes()` in `main.go`
3. Implement evaluation in `evalOne()` with SAR authorization before any resource read
4. Add unit tests in `main_test.go`
5. Update RBAC reference table in README.md and check type table in this file