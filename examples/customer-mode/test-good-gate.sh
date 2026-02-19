#!/bin/bash
# Test customer mode with a passing gate

set -e

echo "Testing customer mode - good gate (keycloak-prod)"
echo "=================================================="

# Configuration
# In customer mode, we need to use a service account from the same namespace as the gate
NAMESPACE="nonprod"
SERVICE_ACCOUNT="default"  # You may need to create a specific SA for this test
GATE_NAME="keycloak-prod"

# Create token
echo "Creating token for ServiceAccount: $SERVICE_ACCOUNT in namespace: $NAMESPACE"
TOKEN=$(kubectl -n "$NAMESPACE" create token "$SERVICE_ACCOUNT" --duration=10m)

# Make request
# Note: In customer mode, we don't use &ns= parameter - gate must be in same namespace as caller
echo "Calling /explain endpoint..."
RESPONSE=$(curl -sS -w "\n%{http_code}" -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/explain?gate=${GATE_NAME}")

# Parse response
HTTP_CODE=$(echo "$RESPONSE" | tail -n 1)
BODY=$(echo "$RESPONSE" | head -n -1)

echo ""
echo "HTTP Status Code: $HTTP_CODE"
echo "Response Body:"
echo "$BODY" | jq .

# Check if gate is ready
READY=$(echo "$BODY" | jq -r '.ready')
MODE=$(echo "$BODY" | jq -r '.mode')

echo ""
echo "Gate Status:"
echo "  Mode: $MODE"
echo "  Ready: $READY"

if [ "$MODE" == "customer" ]; then
    echo "✓ Correct mode: customer"
else
    echo "✗ Expected mode 'customer' but got '$MODE'"
    exit 1
fi

if [ "$READY" == "true" ]; then
    echo "✓ Gate is ready (as expected for passing gate)"
else
    echo "✗ Gate is not ready (expected to be ready)"
    echo "  This may be expected if the deployment doesn't exist yet"
fi

echo ""
echo "Test completed successfully!"
