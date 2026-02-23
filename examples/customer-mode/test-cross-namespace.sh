#!/bin/bash
# Test customer mode with cross-namespace access (should fail)

set -e

echo "Testing customer mode - cross-namespace access (should fail)"
echo "=============================================================="

# Configuration
# In customer mode, we need to use a service account from the same namespace
NAMESPACE="nonprod"
SERVICE_ACCOUNT="default"  # You may need to create a specific SA for this test

# We'll create a gate that tries to check cert-manager deployment in another namespace
GATE_NAME="cross-ns-test-customer"

# First, let's create a test gate that references cross-namespace deployment
echo "Creating temporary gate for cross-namespace test..."
cat <<EOF | kubectl apply -f -
apiVersion: platform.glueops.dev/v1alpha1
kind: Gate
metadata:
  name: ${GATE_NAME}
  namespace: ${NAMESPACE}
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
# Note: In customer mode, we don't use &ns= parameter
echo "Calling /explain endpoint..."
RESPONSE=$(curl -sS -w "\n%{http_code}" -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/explain?gate=${GATE_NAME}")

# Parse response
HTTP_CODE=$(echo "$RESPONSE" | tail -n 1)
BODY=$(echo "$RESPONSE" | head -n -1)

echo ""
echo "HTTP Status Code: $HTTP_CODE"
echo "Response Body:"
echo "$BODY" | jq . 2>/dev/null || echo "$BODY"

# Check mode
MODE=$(echo "$BODY" | jq -r '.mode' 2>/dev/null || echo "")

echo ""
echo "Gate Status:"
echo "  Mode: $MODE"

if [ "$MODE" == "customer" ]; then
    echo "✓ Correct mode: customer"
else
    echo "✗ Expected mode 'customer' but got '$MODE'"
    kubectl delete gate "$GATE_NAME" -n "$NAMESPACE" 2>/dev/null || true
    exit 1
fi

# In customer mode, cross-namespace access should NOT be allowed
# Check if response indicates the policy violation
READY=$(echo "$BODY" | jq -r '.ready' 2>/dev/null || echo "false")
if [ "$READY" == "false" ]; then
    # Check if the error message indicates cross-namespace not allowed
    MESSAGE=$(echo "$BODY" | jq -r '.results[0].message' 2>/dev/null || echo "")
    if echo "$MESSAGE" | grep -q "cross-namespace"; then
        echo "✓ Cross-namespace access denied in customer mode (as expected)"
    else
        echo "✓ Gate failed (expected due to cross-namespace restriction)"
    fi
else
    echo "✗ Expected gate to fail due to cross-namespace restriction"
    kubectl delete gate "$GATE_NAME" -n "$NAMESPACE" 2>/dev/null || true
    exit 1
fi

# Clean up
echo ""
echo "Cleaning up test gate..."
kubectl delete gate "$GATE_NAME" -n "$NAMESPACE" 2>/dev/null || true

echo ""
echo "Test completed successfully!"
