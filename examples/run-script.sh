#!/usr/bin/env bash
#
# Example: acquire a sentry, send a script to it, complete the lease.
#
# Usage:
#   ./examples/run-script.sh python 'print("hello from sandbox")'
#
# Prerequisites:
#   - gvisord running with at least one "python" workload
#   - curl and jq installed

set -euo pipefail

WORKLOAD="${1:?usage: run-script.sh <workload> <script>}"
SCRIPT="${2:?usage: run-script.sh <workload> <script>}"
SOCKET="${GVISORD_SOCKET:-/run/gvisord/gvisord.sock}"

# 1. Acquire a sentry
RESULT=$(gvisord execute "$WORKLOAD")
LEASE_ID=$(echo "$RESULT" | jq -r '.lease_id')
SENTRY_IP=$(echo "$RESULT" | jq -r '.ip // empty')
SENTRY_ID=$(echo "$RESULT" | jq -r '.sentry_id')

echo "Acquired sentry=$SENTRY_ID lease=$LEASE_ID ip=$SENTRY_IP"

# 2. Send script to harness (if IP is available via CNI)
if [ -n "$SENTRY_IP" ]; then
  RESPONSE=$(curl -s -X POST "http://${SENTRY_IP}:8080/run" \
    -H "Content-Type: application/json" \
    -d "{\"script\": $(echo "$SCRIPT" | jq -Rs .), \"interpreter\": \"python3\"}")
  echo "$RESPONSE" | jq .
fi

# 3. Complete the lease
gvisord complete "$LEASE_ID"
echo "Lease completed."
