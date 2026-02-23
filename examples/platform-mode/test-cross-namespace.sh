#!/bin/bash
# Test platform mode with cross-namespace access (should pass)

set -e

echo "Testing platform mode - cross-namespace access (should pass)"
echo "============================================================="

# Configuration
NAMESPACE="glueops-core-gatekeeper"
SERVICE_ACCOUNT="glueops-core-gatekeeper"

# We'll create a gate that checks cert-manager deployment in glueops-core-cert-manager namespace
GATE_NAME="cross-ns-test"
GATE_NAMESPACE="nonprod"

# First, let's create a test gate that references cross-namespace deployment
echo "Creating temporary gate for cross-namespace test..."
cat <<EOF | kubectl apply -f -
apiVersion: platform.glueops.dev/v1alpha1
kind: Gate
metadata:
  name: ${GATE_NAME}
  namespace: ${GATE_NAMESPACE}
spec:
  checks:
    - id: cert-manager
      namespace: glueops-core-cert-manager
      deploymentAvailable:
        name: cert-manager
        minAvailableReplicas: 1
EOF

# Create token
echo "Creating token for ServiceAccount: $SERVICE_ACCOUNT in namespace: $NAMESPACE"
TOKEN=$(kubectl -n "$NAMESPACE" create token "$SERVICE_ACCOUNT" --duration=10m)

# Make request
echo "Calling /explain endpoint..."
RESPONSE=$(curl -sS -w "\n%{http_code}" -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/explain?gate=${GATE_NAME}&ns=${GATE_NAMESPACE}")

# Parse response
HTTP_CODE=$(echo "$RESPONSE" | tail -n 1)
BODY=$(echo "$RESPONSE" | head -n -1)

echo ""
echo "HTTP Status Code: $HTTP_CODE"
echo "Response Body:"
echo "$BODY" | jq .

# Check mode and that the request was accepted
MODE=$(echo "$BODY" | jq -r '.mode')

echo ""
echo "Gate Status:"
echo "  Mode: $MODE"

if [ "$MODE" == "platform" ]; then
    echo "✓ Correct mode: platform"
else
    echo "✗ Expected mode 'platform' but got '$MODE'"
    kubectl delete gate "$GATE_NAME" -n "$GATE_NAMESPACE" 2>/dev/null || true
    exit 1
fi

# In platform mode, cross-namespace access should be allowed
# Check that we got a valid response (not a 403 error)
if [ "$HTTP_CODE" == "200" ]; then
    echo "✓ Cross-namespace access allowed in platform mode"
else
    echo "✗ Expected HTTP 200 but got $HTTP_CODE"
    kubectl delete gate "$GATE_NAME" -n "$GATE_NAMESPACE" 2>/dev/null || true
    exit 1
fi

# Clean up
echo ""
echo "Cleaning up test gate..."
kubectl delete gate "$GATE_NAME" -n "$GATE_NAMESPACE" 2>/dev/null || true

echo ""
echo "Test completed successfully!"
