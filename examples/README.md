# Examples Directory

This directory contains example Gate resources and test scripts for the GlueOps Core Gatekeeper.

## Directory Structure

```
examples/
├── gates/                              # Example Gate resources
│   ├── passing-gate.yaml               # Simple gate: deploymentAvailable (exists)
│   ├── failing-gate.yaml               # Simple gate: deploymentAvailable (doesn't exist)
│   ├── migration-gate.yaml             # statefulSetReady + jobComplete
│   ├── all-check-types-gate.yaml       # All 6 check types in one Gate
│   └── observability-stack-gate.yaml   # Cross-namespace argoApplicationHealthy (platform mode)
├── platform-mode/                      # Platform mode test scripts
│   ├── test-good-gate.sh              # Test with passing gate
│   ├── test-bad-gate.sh               # Test with failing gate
│   └── test-cross-namespace.sh        # Test cross-namespace access (allowed)
├── customer-mode/                      # Customer mode test scripts
│   ├── test-good-gate.sh             # Test with passing gate
│   ├── test-bad-gate.sh              # Test with failing gate
│   └── test-cross-namespace.sh       # Test cross-namespace access (denied)
└── run-all-tests.sh                   # Master test runner
```

## Example Gates

### Simple dependency: `passing-gate.yaml`

Uses `deploymentAvailable` to check a single Deployment exists and has replicas. This is the simplest Gate pattern.

```yaml
apiVersion: platform.glueops.dev/v1alpha1
kind: Gate
metadata:
  name: keycloak-prod
  namespace: nonprod
spec:
  checks:
    - id: postgres
      deploymentAvailable:
        name: keycloak-pg-database-prod
        minAvailableReplicas: 1
```

### Expected failure: `failing-gate.yaml`

Same pattern but references a Deployment that doesn't exist. Used to verify Gatekeeper correctly returns 409/not ready.

### Migration gate: `migration-gate.yaml`

Combines `statefulSetReady` and `jobComplete` — wait for the database to be ready AND a migration Job to complete before allowing the app to deploy.

```yaml
checks:
  - id: database
    statefulSetReady:
      name: postgres
      minReadyReplicas: 1
      requireUpdatedRevision: true
  - id: migration
    jobComplete:
      name: my-app-migrate
```

### All check types: `all-check-types-gate.yaml`

Demonstrates all 6 check types in a single Gate:

| Check | Type | What it verifies |
| --- | --- | --- |
| `database` | `statefulSetReady` | StatefulSet has ready replicas and current revision |
| `migration` | `jobComplete` | Job completed successfully |
| `cache` | `serviceReadyEndpoints` | Service has ready endpoint addresses |
| `api` | `deploymentAvailable` | Deployment is Available with min replicas |
| `workers` | `podLabelReady` | Pods matching a label selector are Ready |
| `monitoring` | `argoApplicationHealthy` | Argo CD Application is Healthy and Synced |

### Cross-namespace observability stack: `observability-stack-gate.yaml`

**Requires platform mode.** Uses `argoApplicationHealthy` with `namespace` overrides to check Argo CD Applications across multiple platform namespaces. This is the primary pattern for gating platform-level deployments (e.g., block Grafana until Prometheus, Thanos, Tempo, and Loki are all healthy).

```yaml
checks:
  - id: prometheus
    argoApplicationHealthy:
      name: prometheus
    namespace: glueops-core-kube-prometheus-stack
  - id: thanos
    argoApplicationHealthy:
      name: thanos
    namespace: glueops-core-thanos
```

Cross-namespace checks require:
- The caller's namespace has the label `gatekeeper.platform.glueops.dev/mode: platform`
- Target namespaces are in `GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES` or match `GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES`

## RBAC for callers

The caller SA only needs RBAC for the check types used in its Gate. See the [RBAC reference table in the README](../README.md#full-rbac-reference-by-check-type) for the exact API groups, resources, and verbs per check type.

Example: minimal Role for a gate that only uses `deploymentAvailable`:
```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gate-waiter
  namespace: nonprod
rules:
  - apiGroups: ["platform.glueops.dev"]
    resources: ["gates"]
    verbs: ["get"]
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get"]
```

## Prerequisites

Before running the test scripts, ensure you have:

1. **Gatekeeper running locally**:
   ```bash
   go run .
   ```
   With `.env` configured (see `.env.example`).

2. **Required namespaces**:
   ```bash
   # Platform mode namespace
   kubectl create namespace glueops-core-gatekeeper
   kubectl label namespace glueops-core-gatekeeper gatekeeper.platform.glueops.dev/mode=platform

   # Customer mode namespace (no label needed, defaults to customer)
   kubectl create namespace nonprod

   # Cross-namespace target
   kubectl create namespace glueops-core-cert-manager
   ```

3. **CRDs and RBAC installed**:
   ```bash
   kubectl apply -f crd.yaml
   kubectl apply -f rbac.yaml
   ```

4. **Example deployment for passing gate**:
   ```bash
   kubectl create deployment keycloak-pg-database-prod --image=nginx -n nonprod
   ```

5. **Required tools**: `curl`, `jq`, `kubectl`

## Running Tests

### All tests at once

```bash
./examples/run-all-tests.sh
```

### Individual tests

```bash
# Platform mode
./examples/platform-mode/test-good-gate.sh
./examples/platform-mode/test-bad-gate.sh
./examples/platform-mode/test-cross-namespace.sh

# Customer mode
./examples/customer-mode/test-good-gate.sh
./examples/customer-mode/test-bad-gate.sh
./examples/customer-mode/test-cross-namespace.sh
```

## Platform Mode Tests

Platform mode allows cross-namespace gate lookups and dependency checks within allowed namespaces.

- **test-good-gate.sh**: Evaluates `keycloak-prod` gate in `nonprod` from a platform SA. Expects mode=platform, ready=true.
- **test-bad-gate.sh**: Evaluates `some-name-prod` gate (non-existent deployment). Expects mode=platform, ready=false.
- **test-cross-namespace.sh**: Creates a temporary gate with a cross-namespace `deploymentAvailable` check. Expects cross-namespace access to be allowed. Cleans up after.

## Customer Mode Tests

Customer mode restricts gate lookups and checks to the caller's namespace.

- **test-good-gate.sh**: Evaluates `keycloak-prod` gate from a customer SA in `nonprod`. Expects mode=customer, ready=true.
- **test-bad-gate.sh**: Evaluates `some-name-prod` gate. Expects mode=customer, ready=false.
- **test-cross-namespace.sh**: Creates a temporary gate with a cross-namespace check. Expects the check to be **denied** with a policy violation message. Cleans up after.

## Troubleshooting

### Token creation fails
Make sure the service accounts exist:
```bash
kubectl get sa glueops-core-gatekeeper -n glueops-core-gatekeeper
kubectl get sa default -n nonprod
```

### Gate not found
Apply the gate resources:
```bash
kubectl apply -f examples/gates/passing-gate.yaml
kubectl apply -f examples/gates/failing-gate.yaml
```

### Forbidden errors
The caller SA needs RBAC to read the resources referenced in Gate checks. See RBAC section above.

### Namespace mode not detected
Verify namespace labels:
```bash
kubectl get namespace glueops-core-gatekeeper --show-labels
kubectl get namespace nonprod --show-labels
```

## Notes

- All test scripts use the `/explain` endpoint which always returns HTTP 200 with JSON, making it easier to parse results.
- The `/check` endpoint returns HTTP 409 when checks are not ready — use this in real PreSync hook Jobs.
- Cross-namespace test scripts create and clean up temporary gates automatically.
- The example gates are for documentation — adapt names, namespaces, and check types to your environment.
