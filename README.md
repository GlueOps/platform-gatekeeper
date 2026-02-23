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

## Argo CD usage (recommended)

The most common integration is an Argo CD PreSync hook Job that blocks until Gatekeeper returns 200.

Example: PreSync hook Job
```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: gate-wait
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

## RBAC / Authorization model

Gatekeeper uses delegated authorization:
- The caller’s ServiceAccount must have RBAC permissions to read the resources referenced in the Gate checks.
- Gatekeeper verifies permissions with SubjectAccessReview before reading.

Example: minimal namespace Role for a hook SA

In many cases, you can give the PreSync hook SA a minimal Role in its namespace:
```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gate-waiter-read
  namespace: nonprod
rules:
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

### Local development
Run locally against a cluster
update / create .env file
```text
GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES="glueops-core-"
GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES="glueops-core,nonprod"
KUBECONFIG=/home/vscode/.kube/config
```
```bash
go run .
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

### Troubleshooting
jq: parse error

You are likely receiving a plain-text error response. Run with -i to see the status code:
```bash
curl -i -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/check?gate=keycloak-prod&ns=nonprod"
```
#### 403 forbidden with SubjectAccessReview

The caller ServiceAccount does not have permission to read a resource referenced by a check.

Fix by granting minimal RBAC in the namespace for that SA (see RBAC example above).

#### 404 gate not found

The Gate name or namespace is wrong, or you are calling without &ns= in platform mode.

