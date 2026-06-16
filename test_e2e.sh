#!/bin/bash
# WLM end-to-end test: two cgroups competing for CPU
# interactive (4 CPUs) vs batch (8 CPUs), both start at weight=100
# WLM should raise interactive's weight under pressure
set -e
cd "$(dirname "$0")"

echo "=== WLM End-to-End Test ==="

CGROOT=/sys/fs/cgroup
CURUSER=$(whoami)

# Clean prior runs
sudo rmdir $CGROOT/wlm-demo/interactive $CGROOT/wlm-demo/batch $CGROOT/wlm-demo 2>/dev/null || true

# Step 1: Delegate cpu at root
echo "+cpu" | sudo tee $CGROOT/cgroup.subtree_control > /dev/null 2>&1

# Step 2: Create parent and delegate cpu to children
sudo mkdir -p $CGROOT/wlm-demo
echo "+cpu" | sudo tee $CGROOT/wlm-demo/cgroup.subtree_control > /dev/null 2>&1

# Step 3: Create leaf cgroups (now cpu.weight will be available)
for cg in wlm-demo/interactive wlm-demo/batch; do
    sudo mkdir -p "$CGROOT/$cg"
    echo 100 | sudo tee "$CGROOT/$cg/cpu.weight" > /dev/null
    # Let current user write to these cgroup files (for wlmd)
    sudo chown -R $CURUSER "$CGROOT/$cg"
done

echo "[setup] interactive cpu.weight=$(cat $CGROOT/wlm-demo/interactive/cpu.weight)"
echo "[setup] batch cpu.weight=$(cat $CGROOT/wlm-demo/batch/cpu.weight)"

# Spawn CPU burners (add to cgroups with sudo)
echo "[spawn] starting 4 CPUs in interactive, 8 in batch..."
for i in $(seq 1 4); do
    stress-ng --cpu 1 --timeout 40s > /dev/null 2>&1 &
    echo $! | sudo tee $CGROOT/wlm-demo/interactive/cgroup.procs > /dev/null 2>&1
done
for i in $(seq 1 8); do
    stress-ng --cpu 1 --timeout 40s > /dev/null 2>&1 &
    echo $! | sudo tee $CGROOT/wlm-demo/batch/cgroup.procs > /dev/null 2>&1
done

# Build
echo "[build] compiling..."
go build -o wlmd ./cmd/wlmd/

# Run wlmd for 30s
echo "[wlmd] running for 30s..."
timeout 30s ./wlmd -policy policy.yaml -interval 10s 2>&1 &
WLM_PID=$!
sleep 32
kill $WLM_PID 2>/dev/null || true

# Results
echo ""
echo "=== Final State ==="
echo "interactive: $(cat $CGROOT/wlm-demo/interactive/cpu.weight 2>/dev/null || echo 'N/A')"
echo "batch:       $(cat $CGROOT/wlm-demo/batch/cpu.weight 2>/dev/null || echo 'N/A')"

# Cleanup
killall stress-ng 2>/dev/null || true
sleep 1
for cg in wlm-demo/interactive wlm-demo/batch wlm-demo; do
    sudo rmdir $CGROOT/$cg 2>/dev/null || true
done
echo "[done]"
