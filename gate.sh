#!/usr/bin/env bash
set -euo pipefail

NS="${GATE_NAMESPACE:-glueops-core-gatekeeper}"
SA="${GATE_SERVICE_ACCOUNT:-glueops-core-gatekeeper}"
GATE="${GATE_NAME:-keycloak-prod}"
TARGET_NS="${GATE_TARGET_NS:-nonprod}"
GATEKEEPER_URL="${GATEKEEPER_URL:-http://localhost:8080}"

TOKEN=$(kubectl -n "$NS" create token "$SA" --duration=10m) || {
  echo "ERROR: failed to create token for $SA in namespace $NS" >&2
  exit 1
}

curl -sS -H "Authorization: Bearer $TOKEN" \
  "${GATEKEEPER_URL}/check?gate=${GATE}&ns=${TARGET_NS}" | jq .
