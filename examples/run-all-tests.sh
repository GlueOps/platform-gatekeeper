#!/bin/bash
# Master test runner for all gatekeeper examples

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "========================================"
echo "GlueOps Core Gatekeeper - Test Runner"
echo "========================================"
echo ""

# Check if gatekeeper is running
echo "Checking if gatekeeper is running..."
if ! curl -sf http://localhost:8080/healthz > /dev/null 2>&1; then
    echo "ERROR: Gatekeeper is not running on localhost:8080"
    echo "Please start gatekeeper first:"
    echo "  export GATEKEEPER_PLATFORM_ALLOWED_NAMESPACE_PREFIXES=\"glueops-core-\""
    echo "  export GATEKEEPER_PLATFORM_ALLOWED_NAMESPACES=\"glueops-core,nonprod\""
    echo "  export KUBECONFIG=/path/to/.kube/config"
    echo "  go run ."
    exit 1
fi
echo "✓ Gatekeeper is running"
echo ""

# Function to run a test
run_test() {
    local test_name=$1
    local test_script=$2
    
    echo "----------------------------------------"
    echo "Running: $test_name"
    echo "----------------------------------------"
    
    if "$test_script"; then
        echo "✓ PASS: $test_name"
    else
        echo "✗ FAIL: $test_name"
        return 1
    fi
    echo ""
}

# Track test results
failed_tests=0
total_tests=0

echo "========================================"
echo "PLATFORM MODE TESTS"
echo "========================================"
echo ""

# Platform mode tests
total_tests=$((total_tests + 1))
run_test "Platform Mode - Good Gate" "$SCRIPT_DIR/platform-mode/test-good-gate.sh" || failed_tests=$((failed_tests + 1))

total_tests=$((total_tests + 1))
run_test "Platform Mode - Bad Gate" "$SCRIPT_DIR/platform-mode/test-bad-gate.sh" || failed_tests=$((failed_tests + 1))

total_tests=$((total_tests + 1))
run_test "Platform Mode - Cross-Namespace" "$SCRIPT_DIR/platform-mode/test-cross-namespace.sh" || failed_tests=$((failed_tests + 1))

echo "========================================"
echo "CUSTOMER MODE TESTS"
echo "========================================"
echo ""

# Customer mode tests
total_tests=$((total_tests + 1))
run_test "Customer Mode - Good Gate" "$SCRIPT_DIR/customer-mode/test-good-gate.sh" || failed_tests=$((failed_tests + 1))

total_tests=$((total_tests + 1))
run_test "Customer Mode - Bad Gate" "$SCRIPT_DIR/customer-mode/test-bad-gate.sh" || failed_tests=$((failed_tests + 1))

total_tests=$((total_tests + 1))
run_test "Customer Mode - Cross-Namespace" "$SCRIPT_DIR/customer-mode/test-cross-namespace.sh" || failed_tests=$((failed_tests + 1))

echo "========================================"
echo "TEST SUMMARY"
echo "========================================"
echo "Total tests: $total_tests"
echo "Passed: $((total_tests - failed_tests))"
echo "Failed: $failed_tests"
echo ""

if [ $failed_tests -eq 0 ]; then
    echo "✓ All tests passed!"
    exit 0
else
    echo "✗ Some tests failed"
    exit 1
fi
