# Examples Directory

This directory contains example Gate resources and test scripts for the GlueOps Core Gatekeeper.

## Directory Structure

```
examples/
├── gates/                      # Example Gate resources
│   ├── passing-gate.yaml       # Gate that checks existing deployment
│   └── failing-gate.yaml       # Gate that checks non-existent deployment
├── platform-mode/              # Platform mode test scripts
│   ├── test-good-gate.sh       # Test with passing gate
│   ├── test-bad-gate.sh        # Test with failing gate
│   └── test-cross-namespace.sh # Test cross-namespace access (allowed)
├── customer-mode/              # Customer mode test scripts
│   ├── test-good-gate.sh       # Test with passing gate
│   ├── test-bad-gate.sh        # Test with failing gate
│   └── test-cross-namespace.sh # Test cross-namespace access (denied)
└── run-all-tests.sh            # Master test runner
```

## Prerequisites

Before running the test scripts, ensure you have:

1. **Gatekeeper running locally**:
   ```bash
   export GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES="glueops-core-"
   export GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES="glueops-core,nonprod"
   export KUBECONFIG=/path/to/.kube/config
   go run .
   ```

2. **Required namespaces**:
   - `glueops-core-gatekeeper` (with platform mode label)
   - `nonprod` (without platform mode label, defaults to customer mode)
   - `glueops-core-cert-manager` (for cross-namespace tests)

   Create namespaces with labels:
   ```bash
   # Platform mode namespace
   kubectl create namespace glueops-core-gatekeeper
   kubectl label namespace glueops-core-gatekeeper gatekeeper.platform.onglueopshosted.com/mode=platform

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
   kubectl scale deployment keycloak-pg-database-prod --replicas=1 -n nonprod
   ```

5. **Example deployment for cross-namespace tests** (optional):
   ```bash
   kubectl create deployment cert-manager --image=nginx -n glueops-core-cert-manager
   kubectl scale deployment cert-manager --replicas=1 -n glueops-core-cert-manager
   ```

## Gate Resources

### Passing Gate (`gates/passing-gate.yaml`)

This gate checks for a deployment that should exist in your test environment:

```yaml
apiVersion: platform.onglueopshosted.com/v1alpha1
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

Apply it:
```bash
kubectl apply -f examples/gates/passing-gate.yaml
```

### Failing Gate (`gates/failing-gate.yaml`)

This gate checks for a deployment that doesn't exist:

```yaml
apiVersion: platform.onglueopshosted.com/v1alpha1
kind: Gate
metadata:
  name: some-name-prod
  namespace: nonprod
spec:
  checks:
    - id: postgres
      deploymentAvailable:
        name: some-name
        minAvailableReplicas: 1
```

Apply it:
```bash
kubectl apply -f examples/gates/failing-gate.yaml
```

## Platform Mode Tests

Platform mode allows cross-namespace gate lookups and dependency checks within allowed namespaces.

### Test Good Gate

Tests a passing gate in platform mode:

```bash
./examples/platform-mode/test-good-gate.sh
```

Expected outcome: 
- Mode: `platform`
- Ready: `true` (if deployment exists)
- HTTP 200

### Test Bad Gate

Tests a failing gate in platform mode:

```bash
./examples/platform-mode/test-bad-gate.sh
```

Expected outcome:
- Mode: `platform`
- Ready: `false`
- HTTP 200 (using /explain endpoint)

### Test Cross-Namespace Access

Tests that platform mode allows cross-namespace checks:

```bash
./examples/platform-mode/test-cross-namespace.sh
```

Expected outcome:
- Creates temporary gate with cross-namespace check
- Mode: `platform`
- Cross-namespace access is **allowed**
- HTTP 200
- Cleans up test gate

## Customer Mode Tests

Customer mode restricts gate lookups and checks to the same namespace.

### Test Good Gate

Tests a passing gate in customer mode:

```bash
./examples/customer-mode/test-good-gate.sh
```

Expected outcome:
- Mode: `customer`
- Ready: `true` (if deployment exists)
- HTTP 200

### Test Bad Gate

Tests a failing gate in customer mode:

```bash
./examples/customer-mode/test-bad-gate.sh
```

Expected outcome:
- Mode: `customer`
- Ready: `false`
- HTTP 200 (using /explain endpoint)

### Test Cross-Namespace Access

Tests that customer mode denies cross-namespace checks:

```bash
./examples/customer-mode/test-cross-namespace.sh
```

Expected outcome:
- Creates temporary gate with cross-namespace check
- Mode: `customer`
- Cross-namespace access is **denied**
- Gate fails with policy violation message
- Cleans up test gate

## Running All Tests

### Using the Master Test Runner (Recommended)

Run all tests with a single command:

```bash
./examples/run-all-tests.sh
```

This script will:
- Check if gatekeeper is running
- Execute all platform mode tests
- Execute all customer mode tests
- Provide a summary of passed/failed tests

### Manual Execution

You can also run tests individually or in groups:

```bash
# Run all platform mode tests
for script in examples/platform-mode/*.sh; do
    echo "Running $script..."
    "$script"
    echo ""
done

# Run all customer mode tests
for script in examples/customer-mode/*.sh; do
    echo "Running $script..."
    "$script"
    echo ""
done
```

## Troubleshooting

### "Service account not found" or token creation fails

Make sure the service accounts exist:
```bash
kubectl get sa glueops-core-gatekeeper -n glueops-core-gatekeeper
kubectl get sa default -n nonprod
```

If they don't exist, create them:
```bash
kubectl create sa glueops-core-gatekeeper -n glueops-core-gatekeeper
```

### "Gate not found"

Apply the gate resources:
```bash
kubectl apply -f examples/gates/passing-gate.yaml
kubectl apply -f examples/gates/failing-gate.yaml
```

### "Forbidden" errors

Ensure RBAC is configured correctly. The service accounts need appropriate permissions:
```bash
kubectl apply -f rbac.yaml
```

For customer mode tests, you may need to grant the `default` service account in the `nonprod` namespace permissions to read deployments and gates:

```bash
kubectl create rolebinding nonprod-gate-reader \
  --clusterrole=view \
  --serviceaccount=nonprod:default \
  -n nonprod
```

### Namespace mode not detected correctly

Verify namespace labels:
```bash
kubectl get namespace glueops-core-gatekeeper -o jsonpath='{.metadata.labels}'
kubectl get namespace nonprod -o jsonpath='{.metadata.labels}'
```

The `glueops-core-gatekeeper` namespace should have:
```
gatekeeper.platform.onglueopshosted.com/mode: platform
```

The `nonprod` namespace should NOT have this label (defaults to customer mode).

## Notes

- All test scripts use the `/explain` endpoint which always returns HTTP 200 with JSON, making it easier to parse results.
- The `/check` endpoint returns HTTP 409 when checks are not ready.
- Platform mode tests use cross-namespace gate lookups via the `?ns=` parameter.
- Customer mode tests do NOT use the `?ns=` parameter since it's not allowed.
- Cross-namespace test scripts create and clean up temporary gates automatically.
