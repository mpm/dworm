#!/bin/bash
# E2E test runner for dworm

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/skip.sh
source "$SCRIPT_DIR/lib/skip.sh"

# Check Docker availability
if ! docker info &>/dev/null; then
    log_skip "Docker not available, skipping E2E tests"
    exit 0
fi

# Check dworm binary exists
if [[ ! -x "$DWORM" ]]; then
    log_error "dworm binary not found at $DWORM. Run 'make build' first"
    exit 1
fi

# Track results
PASSED=0
FAILED=0
SKIPPED=0

echo "=================================="
echo "  dworm E2E Test Suite"
echo "=================================="
echo "Project root: $PROJECT_ROOT"
echo "dworm binary: $DWORM"
echo ""

# Run each test
run_test "test-port-forward.sh"
run_test "test-env-vars.sh"
run_test "test-ssh-agent.sh"
run_test "test-gpg-agent.sh"
run_test "test-git-creds.sh"
run_test "test-rebuild.sh"
run_test "test-remove.sh"

# Summary
echo ""
echo "=================================="
echo "  E2E Test Summary"
echo "=================================="
echo "Passed:  $PASSED"
echo "Failed:  $FAILED"
echo "Skipped: $SKIPPED"
echo ""

if [[ $FAILED -gt 0 ]]; then
    exit 1
fi
exit 0
