#!/bin/bash
# E2E smoke test for wlmd token mode.
# Requires write access to /var/run/wlm (sudo or root).
set -e

echo "=== Token WLM E2E Test ==="

go build ./cmd/wlmd/

# Start wlmd in token mode
sudo ./wlmd --mode token --policy policy_token.yaml --interval 2s &
PID=$!
sleep 4

if sudo test -f /var/run/wlm/token_state.json; then
    echo "✓ Shared state written"
    sudo cat /var/run/wlm/token_state.json
else
    echo "✗ Shared state missing"
    sudo kill $PID 2>/dev/null
    exit 1
fi

sudo kill $PID 2>/dev/null
echo "=== PASS ==="
