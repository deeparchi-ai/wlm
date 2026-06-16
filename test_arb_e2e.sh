#!/bin/bash
# WLM arbitration E2E test: three classes with different importance
# critical(imp=1, goal=response_time<500ms), normal(imp=3), batch(imp=5)
# Under CPU pressure, critical should take from batch, then normal.
set -e
cd "$(dirname "$0")"

echo "=== WLM Arbitration Test ==="
CGROOT=/sys/fs/cgroup
CURUSER=$(whoami)

# Clean — kill any processes in these cgroups first
for cg in wlm-demo; do
    if [ -d "$CGROOT/$cg" ]; then
        # Kill all processes in the tree
        for leaf in $CGROOT/$cg/*/cgroup.procs $CGROOT/$cg/cgroup.procs; do
            [ -f "$leaf" ] && sudo bash -c "kill -9 \$(cat $leaf)" 2>/dev/null || true
        done
        sleep 0.5
        # Remove leaf cgroups first
        for leaf in $CGROOT/$cg/*/; do sudo rmdir "$leaf" 2>/dev/null || true; done
        sudo rmdir $CGROOT/$cg 2>/dev/null || true
    fi
done

# Setup cgroup hierarchy with delegation
echo "+cpu" | sudo tee $CGROOT/cgroup.subtree_control > /dev/null 2>&1
sudo mkdir -p $CGROOT/wlm-demo
echo "+cpu" | sudo tee $CGROOT/wlm-demo/cgroup.subtree_control > /dev/null 2>&1

for cg in wlm-demo/critical wlm-demo/normal wlm-demo/batch; do
    sudo mkdir -p "$CGROOT/$cg"
    echo 100 | sudo tee "$CGROOT/$cg/cpu.weight" > /dev/null
    sudo chown -R $CURUSER "$CGROOT/$cg"
done

echo "[setup] all classes at weight=100"

# Spawn CPU burners — equal load in each class (4 CPUs each)
echo "[spawn] 4 CPUs in each class..."
for cls in critical normal batch; do
    for i in $(seq 1 4); do
        stress-ng --cpu 1 --timeout 40s > /dev/null 2>&1 &
        echo $! | sudo tee $CGROOT/wlm-demo/$cls/cgroup.procs > /dev/null 2>&1
    done
done

# Build
echo "[build] compiling..."
go build -o wlmd ./cmd/wlmd/

# Run
echo "[wlmd] running for 30s with arbitration policy..."
timeout 30s ./wlmd -policy policy_arb.yaml -interval 10s 2>&1 &
WLM_PID=$!
sleep 32
kill $WLM_PID 2>/dev/null || true

# Results
echo ""
echo "=== Final Weights ==="
for cls in critical normal batch; do
    w=$(cat $CGROOT/wlm-demo/$cls/cpu.weight 2>/dev/null || echo "N/A")
    echo "  $cls (imp=$(
        case $cls in critical) echo 1;; normal) echo 3;; batch) echo 5;; esac
    )): cpu.weight=$w"
done

# Cleanup
killall stress-ng 2>/dev/null || true
sleep 1
for cg in wlm-demo/critical wlm-demo/normal wlm-demo/batch wlm-demo; do
    sudo rmdir $CGROOT/$cg 2>/dev/null || true
done
echo "[done]"
