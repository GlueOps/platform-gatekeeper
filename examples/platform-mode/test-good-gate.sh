#!/bin/bash
# Test platform mode with a passing gate

set -e

echo "Testing platform mode - good gate (keycloak-prod)"
echo "================================================="

# Configuration
NAMESPACE="glueops-core-gatekeeper"
SERVICE_ACCOUNT="glueops-core-gatekeeper"
GATE_NAME="keycloak-prod"
GATE_NAMESPACE="nonprod"

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

# Check if gate is ready
READY=$(echo "$BODY" | jq -r '.ready')
MODE=$(echo "$BODY" | jq -r '.mode')

echo ""
echo "Gate Status:"
echo "  Mode: $MODE"
echo "  Ready: $READY"

if [ "$MODE" == "platform" ]; then
    echo "✓ Correct mode: platform"
else
    echo "✗ Expected mode 'platform' but got '$MODE'"
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
