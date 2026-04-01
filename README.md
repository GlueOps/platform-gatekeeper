# GlueOps Core Gatekeeper
<img width="960" height="1088" alt="Gemini_Generated_Image_4z69mb4z69mb4z69" src="https://github.com/user-attachments/assets/2685397d-4055-4678-9601-908b4c827cf4" />

GlueOps Core Gatekeeper is a small Go HTTP service that acts as a **deployment dependency gate** for Kubernetes.

It lets applications define their own dependency checks (via a `Gate` custom resource), and provides a simple API that returns:

- **200 OK** when all checks pass (dependencies are ready)
- **409 Conflict** when any check is not ready yet (safe to retry)
- **4xx** for invalid specs or authorization issues

This is especially useful when you need ordering logic across a mix of:
- Argo CD-managed apps (sync waves / hooks)
- Helm-managed components (hooks, Jobs, controllers)
- “Out-of-band” resources not represented as Argo Applications

A common pattern is to use an **Argo CD PreSync hook Job** to block deployment of an app until the Gatekeeper reports that prerequisites (DB, migrations, external dependencies) are ready.

---

## Key features

- **Per-app dependency definition**: each app owns a `Gate` object in its namespace.
- **Safe-by-default multi-tenancy**:
  - Requests authenticate via Kubernetes **ServiceAccount tokens**.
  - Access is authorized using Kubernetes **SubjectAccessReview** (delegated authorization).
- **Namespace policy modes** controlled by a namespace label:
  - `customer` (default): gate lookup and checks are namespace-local
  - `platform`: optionally allows cross-namespace checks and/or gate lookups (restricted by allowlists)
- **Simple HTTP API** designed for hook-based retries.

---

## How it works

1. A caller (usually an Argo CD hook Job) calls Gatekeeper with its ServiceAccount token:
   - `GET /check?gate=<gate-name>`
2. Gatekeeper performs a `TokenReview` to authenticate the token.
3. Gatekeeper determines the caller namespace from the ServiceAccount identity.
4. Gatekeeper loads the `Gate` CR and evaluates each check.
5. Each check is authorized with a `SubjectAccessReview` for the caller.
6. Gatekeeper responds with JSON and appropriate HTTP status.

---

## Installation

### 1) Install the CRD

Apply the `Gate` CRD:

```bash
kubectl apply -f crd.yaml
```

### 2) Create Gatekeeper RBAC

Apply Gatekeeper RBAC (TokenReview + SubjectAccessReview + read-only allowlisted resources + Gate status patch):

```bash
kubectl apply -f rbac.yml
```

### 3) Deploy the Gatekeeper service

Your deployment should run with the Gatekeeper ServiceAccount and expose port 8080.
(Provide your own deployment YAML, Helm chart, or Kustomize overlay.)


## Configuration

Gatekeeper supports configuration via environment variables.

### Gate CRD location
| Env var                    | Default                        | Description               |
| -------------------------- | ------------------------------ | ------------------------- |
| `GATEKEEPER_GATE_GROUP`    | `platform.glueops.dev` | API group of the Gate CRD |
| `GATEKEEPER_GATE_VERSION`  | `v1alpha1`                     | API version               |
| `GATEKEEPER_GATE_RESOURCE` | `gates`                        | plural resource name      |


### Platform mode can be restricted to specific namespaces.

| Env var                                          | Default                | Description                                                                           |
| ------------------------------------------------ | ---------------------- | ------------------------------------------------------------------------------------- |
| `GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES`         | `glueops-core,nonprod` | CSV list of namespaces allowed for platform cross-namespace access                    |
| `GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES` | `glueops-core-`        | CSV list of namespace prefixes allowed (e.g. `glueops-core-` allows `glueops-core-*`) |


### HTTP

| Env var | Default | Description      |
| ------- | ------- | ---------------- |
| `PORT`  | `8080`  | HTTP listen port |

## Namespace policy modes

Gatekeeper chooses the mode based on a label on the caller’s namespace:

Label key:
```yaml
gatekeeper.platform.glueops.dev/mode: customer|platform
```

### customer mode (default)

- Gate lookup must be in the caller namespace.
- Checks must be in the same namespace as the Gate.

This is intended for customer/self-service namespaces.

### platform mode

- Cross-namespace Gate lookup is allowed via ?ns=... only if the namespace is allowed by:
  - GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES OR
  - GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES
- Cross-namespace checks are allowed using the same allow rules.

This is intended for platform-controlled automation and core namespaces.

## The Gate Custom Resource

A Gate defines a list of checks. Each check must set exactly one check type.

Example:
```yaml
apiVersion: platform.glueops.dev/v1alpha1
kind: Gate
metadata:
  name: keycloak-prod
  namespace: nonprod
spec:
  strict: true
  checks:
    - id: postgres
      deploymentAvailable:
        name: keycloak-pg-database-prod
        minAvailableReplicas: 1
```

### Supported check types

`deploymentAvailable`

Checks a Deployment is Available and has at least N available replicas.
```yaml
- id: api
  deploymentAvailable:
    name: my-api
    minAvailableReplicas: 2
```

`statefulSetReady`

Checks a StatefulSet has at least N ready replicas (and optionally is fully updated).
```yaml
- id: postgres
  statefulSetReady:
    name: postgres
    minReadyReplicas: 1
    requireUpdatedRevision: true
```

`jobComplete`

Checks a Job has completed successfully.
```yaml
- id: migrate
  jobComplete:
    name: my-app-migrate
```

`serviceReadyEndpoints`
Checks a Service has at least N ready endpoint addresses (via EndpointSlices).
```yaml
- id: redis
  serviceReadyEndpoints:
    name: redis
    minReadyAddresses: 1
```

`podLabelReady`
Checks at least N Pods matching a label selector are Ready.
```yaml
- id: workers
  podLabelReady:
    selector: "app=my-worker"
    minReadyPods: 2
```

> Note: If you allow very broad selectors, this may list many pods. Prefer selectors that are specific to the app.

`argoApplicationHealthy`

Checks an Argo CD Application is Healthy and/or Synced.
```yaml
- id: my-app
  argoApplicationHealthy:
    name: my-app
    requireSynced: true
    requireHealthy: true
```

## HTTP API
### GET /healthz

Simple health endpoint.
```bash
curl -i http://gatekeeper:8080/healthz
```

### GET /check?gate=<name>

Evaluates the Gate in the caller namespace (customer mode), or by default in the caller namespace (platform mode too).
- 200 OK → all checks passed
- 409 Conflict → at least one check is blocking (safe to retry)

Example:
```bash
TOKEN="$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)"
curl -sS -H "Authorization: Bearer $TOKEN" \
  "http://gatekeeper:8080/check?gate=keycloak-prod"
```

### GET /check?gate=<name>&ns=<namespace> (platform mode)

In platform mode only, allows evaluating a Gate in another namespace if allowed by allowlists.
```bash
TOKEN="$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)"
curl -sS -H "Authorization: Bearer $TOKEN" \
  "http://gatekeeper:8080/check?gate=keycloak-prod&ns=nonprod"
```

### GET /explain?gate=<name>[&ns=<namespace>]

Same evaluation logic as /check, but always returns JSON with 200 OK (useful for debugging).
```
curl -sS -H "Authorization: Bearer $TOKEN" \
  "http://gatekeeper:8080/explain?gate=keycloak-prod&ns=nonprod" | jq .
```

### JSON response format
```json
{
  "gate": "keycloak-prod",
  "namespace": "nonprod",
  "ready": false,
  "mode": "platform",
  "results": [
    {
      "id": "postgres",
      "ready": false,
      "message": "availableReplicas 0 < 1"
    }
  ]
}
```

## Usage patterns

### Pattern 1: Simple database dependency (customer mode)

Block an application deployment until its database is available.

**Gate:**
```yaml
apiVersion: platform.glueops.dev/v1alpha1
kind: Gate
metadata:
  name: keycloak-prod
  namespace: nonprod
spec:
  strict: true
  checks:
    - id: postgres
      deploymentAvailable:
        name: keycloak-pg-database-prod
        minAvailableReplicas: 1
```

**PreSync hook Job (minimal):**
```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: gate-wait
  namespace: nonprod
  annotations:
    argocd.argoproj.io/hook: PreSync
    argocd.argoproj.io/hook-delete-policy: HookSucceeded
spec:
  backoffLimit: 0
  template:
    spec:
      serviceAccountName: gate-waiter
      restartPolicy: Never
      containers:
        - name: wait
          image: curlimages/curl:8.5.0
          command: ["/bin/sh", "-c"]
          args:
            - |
              set -e
              TOKEN="$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)"
              until curl -sf -H "Authorization: Bearer ${TOKEN}" \
                "http://glueops-core-gatekeeper.glueops-core-gatekeeper.svc.cluster.local:8080/check?gate=keycloak-prod"; do
                echo "dependencies not ready yet"; sleep 5
              done
              echo "dependencies ready"
```

This pattern makes Argo CD wait until prerequisites are ready before continuing the sync.

### Pattern 2: Migration gate (customer mode)

Block an application until both the database is ready and a migration Job has completed.

**Gate:**
```yaml
apiVersion: platform.glueops.dev/v1alpha1
kind: Gate
metadata:
  name: my-app-ready
  namespace: nonprod
spec:
  strict: true
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

### Pattern 3: Service mesh readiness (customer mode)

Block until backing services have ready endpoints.

**Gate:**
```yaml
apiVersion: platform.glueops.dev/v1alpha1
kind: Gate
metadata:
  name: api-dependencies
  namespace: nonprod
spec:
  strict: true
  checks:
    - id: redis
      serviceReadyEndpoints:
        name: redis
        minReadyAddresses: 1
    - id: workers
      podLabelReady:
        selector: "app=my-worker,tier=backend"
        minReadyPods: 2
```

> Note: Prefer specific label selectors for `podLabelReady`. Broad selectors may list many pods and add API server load.

### Pattern 4: Cross-namespace observability stack (platform mode)

Block Grafana deployment until the full observability stack (Prometheus, Thanos, Tempo, Loki) is ready across multiple platform namespaces. This is the primary use case for platform mode with cross-namespace checks.

**Gate** (in the gatekeeper’s platform namespace):
```yaml
apiVersion: platform.glueops.dev/v1alpha1
kind: Gate
metadata:
  name: observability-stack-complete
  namespace: glueops-core-gatekeeper
spec:
  strict: true
  checks:
    - id: prometheus
      argoApplicationHealthy:
        name: prometheus
      namespace: glueops-core-kube-prometheus-stack
    - id: thanos
      argoApplicationHealthy:
        name: thanos
      namespace: glueops-core-thanos
    - id: tempo
      argoApplicationHealthy:
        name: tempo
      namespace: glueops-core-tempo
    - id: loki
      argoApplicationHealthy:
        name: loki
      namespace: glueops-core-loki
```

**PreSync hook Job** (with progress reporting):
```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: wait-for-all-datasources
  namespace: glueops-core-kube-prometheus-stack
  annotations:
    argocd.argoproj.io/hook: PreSync
    argocd.argoproj.io/hook-delete-policy: BeforeHookCreation
spec:
  backoffLimit: 30
  template:
    spec:
      serviceAccountName: grafana-gate-waiter
      restartPolicy: OnFailure
      containers:
      - name: wait
        image: alpine:3.21
        command: ["/bin/sh", "-c"]
        args:
          - |
            set -e
            apk add --no-cache curl jq >/dev/null 2>&1
            TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
            GATEKEEPER_URL="http://gatekeeper.glueops-core-gatekeeper.svc.cluster.local:8080"
            GATE_NAME="observability-stack-complete"
            GATE_NS="glueops-core-gatekeeper"
            MAX_ATTEMPTS=60
            ATTEMPT=0

            while [ $ATTEMPT -lt $MAX_ATTEMPTS ]; do
              ATTEMPT=$((ATTEMPT + 1))
              HTTP_CODE=$(curl -s -o /tmp/response.json -w "%{http_code}" \
                -H "Authorization: Bearer $TOKEN" \
                "$GATEKEEPER_URL/check?gate=$GATE_NAME&ns=$GATE_NS")

              if [ "$HTTP_CODE" = "200" ]; then
                echo "All datasources ready, proceeding with Grafana deployment"
                exit 0
              elif [ "$HTTP_CODE" = "409" ]; then
                READY=$(jq ‘[.results[] | select(.ready == true)] | length’ /tmp/response.json)
                TOTAL=$(jq ‘.results | length’ /tmp/response.json)
                echo "[$ATTEMPT/$MAX_ATTEMPTS] $READY/$TOTAL checks ready, waiting 15s..."
                sleep 15
              else
                echo "[$ATTEMPT/$MAX_ATTEMPTS] HTTP $HTTP_CODE, retrying in 20s..."
                sleep 20
              fi
            done

            echo "TIMEOUT: observability stack not ready"
            exit 1
```

Platform mode requires:
- The caller namespace has the label `gatekeeper.platform.glueops.dev/mode: platform`
- Target namespaces are in `GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES` or match `GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES`

### Pattern 5: Argo CD application dependency (customer mode)

Block deployment until an Argo CD Application is both healthy and synced.

**Gate:**
```yaml
apiVersion: platform.glueops.dev/v1alpha1
kind: Gate
metadata:
  name: app-dependencies
  namespace: nonprod
spec:
  strict: true
  checks:
    - id: auth-service
      argoApplicationHealthy:
        name: auth-service
        requireSynced: true
        requireHealthy: true
    - id: config-db
      deploymentAvailable:
        name: config-db
        minAvailableReplicas: 1
```

`argoApplicationHealthy` checks the Argo CD Application CR’s `status.health.status` (must be `"Healthy"`) and `status.sync.status` (must be `"Synced"`). Both flags default to `true` and can be independently disabled if you only care about one dimension.

### Pattern 6: Mixed check types

A single Gate can combine any check types. Each check must set exactly one type.

**Gate:**
```yaml
apiVersion: platform.glueops.dev/v1alpha1
kind: Gate
metadata:
  name: full-stack-ready
  namespace: nonprod
spec:
  strict: true
  checks:
    - id: database
      statefulSetReady:
        name: postgres
        minReadyReplicas: 1
    - id: migration
      jobComplete:
        name: db-migrate
    - id: cache
      serviceReadyEndpoints:
        name: redis
        minReadyAddresses: 1
    - id: api
      deploymentAvailable:
        name: api-server
        minAvailableReplicas: 2
    - id: workers
      podLabelReady:
        selector: "app=worker"
        minReadyPods: 3
    - id: monitoring
      argoApplicationHealthy:
        name: monitoring-stack
```

### strict vs non-strict mode

When `spec.strict` is `true` (the default), any check failure immediately fails the entire Gate. When `false`, invalid or policy-violating checks are skipped, and the Gate can still pass if all remaining checks pass. Use `strict: false` only when some checks are optional or expected to be temporarily unavailable.

## RBAC / Authorization model

Gatekeeper uses delegated authorization:
- The caller’s ServiceAccount must have RBAC permissions to read the resources referenced in the Gate checks.
- Gatekeeper verifies permissions with SubjectAccessReview before reading.
- Only `get` is needed for named resources (Deployments, StatefulSets, Jobs, Services, Argo Applications). Only `list` is needed for collection queries (Pods, EndpointSlices).
- Gatekeeper itself does **not** need `list` or `watch` — it fetches each resource individually by name from the Gate spec.

### Caller SA RBAC: only include what your Gate checks need

Grant only the RBAC rules that match the check types used in your Gate. For example, if your Gate only uses `deploymentAvailable` and `argoApplicationHealthy`, you only need:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: grafana-gate-waiter
  namespace: nonprod
rules:
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get"]
  - apiGroups: ["argoproj.io"]
    resources: ["applications"]
    verbs: ["get"]
  - apiGroups: ["platform.glueops.dev"]
    resources: ["gates"]
    verbs: ["get"]
```

### Full RBAC reference by check type

| Check type | API group | Resource | Verb |
| --- | --- | --- | --- |
| `deploymentAvailable` | `apps` | `deployments` | `get` |
| `statefulSetReady` | `apps` | `statefulsets` | `get` |
| `jobComplete` | `batch` | `jobs` | `get` |
| `serviceReadyEndpoints` | (core) | `services` | `get` |
| `serviceReadyEndpoints` | `discovery.k8s.io` | `endpointslices` | `list` |
| `podLabelReady` | (core) | `pods` | `list` |
| `argoApplicationHealthy` | `argoproj.io` | `applications` | `get` |
| (all Gates) | `platform.glueops.dev` | `gates` | `get` |

### Example: full Role covering all check types

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gate-waiter-read
  namespace: nonprod
rules:
  - apiGroups: ["platform.glueops.dev"]
    resources: ["gates"]
    verbs: ["get"]
  - apiGroups: ["apps"]
    resources: ["deployments","statefulsets"]
    verbs: ["get"]
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["services"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["list"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["list"]
  - apiGroups: ["argoproj.io"]
    resources: ["applications"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: gate-waiter-read
  namespace: nonprod
subjects:
  - kind: ServiceAccount
    name: gate-waiter
    namespace: nonprod
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: gate-waiter-read
```

## Local development

Run locally against a cluster. Update / create `.env` file (see `.env.example`):
```text
GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES="glueops-core-"
GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES="glueops-core,nonprod"
KUBECONFIG=~/.kube/config
```
```bash
go run .
```

### Run tests

```bash
go test ./...
```

### Test with a ServiceAccount token

For example, to test platform mode from a platform namespace:
```bash
NS=glueops-core-gatekeeper
SA=glueops-core-gatekeeper
TOKEN=$(kubectl -n "$NS" create token "$SA" --duration=10m)

curl -sS -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/check?gate=keycloak-prod&ns=nonprod" | jq .
```

If you get a 403:
- verify the caller SA has RBAC to read the referenced resources (or intentionally doesn’t)
- verify the target namespace is allowed by GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES/..._PREFIXES

## Troubleshooting

### jq: parse error

You are likely receiving a plain-text error response. Run with -i to see the status code:
```bash
curl -i -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/check?gate=keycloak-prod&ns=nonprod"
```

### 403 forbidden with SubjectAccessReview

The caller ServiceAccount does not have permission to read a resource referenced by a check.

Fix by granting minimal RBAC in the namespace for that SA (see RBAC reference table above). Only grant the verbs/resources needed by the check types in your Gate.

### 404 gate not found

The Gate name or namespace is wrong, or you are calling without &ns= in platform mode.

### 409 conflict (not ready)

This is normal — it means at least one check is not ready yet. Use the `/explain` endpoint to see which checks are blocking:
```bash
curl -sS -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/explain?gate=my-gate&ns=nonprod" | jq .
```

