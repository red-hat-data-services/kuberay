#!/bin/bash

set -o allexport
# shellcheck disable=SC1091
source .env-odh
set +o allexport

# Create results directory if it doesn't exist
mkdir -p results

# Initialize variables
PARSED_TEST_TAGS=""
EXTRA_ARGS=()

# Function to show usage
show_usage() {
    echo "Usage: $0 [OPTIONS]"
    echo "Options:"
    echo "  -testTier=VALUE    Specify test tier (only Tier1 is supported)"
    echo "  -h, -help          Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0 -testTier=Tier1"
    exit 0
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        -testTier=*)
            PARSED_TEST_TAGS="${1#*=}"
            shift
            ;;
        -h|-help|--help)
            show_usage
            ;;
        *)
            # Collect unknown arguments to pass to gotestsum
            EXTRA_ARGS+=("$1")
            shift
            ;;
    esac
done

# Set TEST_TAGS only from command line arguments
TEST_TAGS="$PARSED_TEST_TAGS"

# If no TEST_TAGS provided via command line, show usage and exit
if [[ -z "$TEST_TAGS" ]]; then
    echo "Info: No test tier specified."
    echo ""
    show_usage
fi

TEST_RUN_REGEX=""

# Handle different combinations of test tags
if [[ "$TEST_TAGS" == *"Tier1"* ]]; then
    echo "Running Tier1 kuberay e2e tests"
    TEST_RUN_REGEX="^(TestRayJobWithClusterSelector|TestRayJob|TestRayJobSuspend|TestRayJobLightWeightMode)$"
elif [[ "$TEST_TAGS" == *"Sanity"* ]]; then
    echo "Warning: 'Sanity' tier is no longer supported. Only 'Tier1' is supported."
    echo ""
    show_usage
else
    echo "Info: Invalid test tier '$TEST_TAGS'. Only 'Tier1' is supported."
    echo ""
    show_usage
fi

# Run tests with junit XML output
gotestsum --format standard-verbose --junitfile results/xunit_report.xml --junitfile-testsuite-name short --junitfile-testcase-classname relative -- -timeout 30m -run "$TEST_RUN_REGEX" ./test/e2e -p 1 -parallel 1 "${EXTRA_ARGS[@]}"
