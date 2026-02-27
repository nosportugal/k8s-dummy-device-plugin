#!/usr/bin/env bash
#
# End-to-end test for k8s-dummy-device-plugin.
# Spins up a kind cluster, deploys the plugin, and verifies device allocation.
#
# Usage:
#   ./test/e2e.sh
#
# Requirements: docker, kind, kubectl
#
set -euo pipefail

CLUSTER_NAME="dummy-device-plugin-e2e"
IMAGE_NAME="k8s-dummy-device-plugin:e2e-test"
NAMESPACE="kube-system"
TEST_NAMESPACE="default"
FAILURES=0

# ── Cleanup ─────────────────────────────────────────────────────────
cleanup() {
    echo ""
    echo "==> Cleaning up kind cluster '$CLUSTER_NAME'..."
    kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true
}
trap cleanup EXIT

# ── Pre-flight checks ──────────────────────────────────────────────
for cmd in docker kind kubectl; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "ERROR: '$cmd' is required but not found in PATH."
        exit 1
    fi
done

# ── Create cluster ─────────────────────────────────────────────────
echo "==> Creating kind cluster '$CLUSTER_NAME'..."
kind create cluster --name "$CLUSTER_NAME" --wait 120s

# ── Build and load image ───────────────────────────────────────────
echo "==> Building Docker image '$IMAGE_NAME'..."
docker build -t "$IMAGE_NAME" .

echo "==> Loading image into kind..."
kind load docker-image "$IMAGE_NAME" --name "$CLUSTER_NAME"

# ── Deploy device plugin ───────────────────────────────────────────
echo "==> Deploying device plugin..."
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: dummy-device-plugin-config
  namespace: kube-system
data:
  dummyResources.json: |
    [
      {"resourceName": "dummy/gpu", "count": 4},
      {"resourceName": "dummy/nic", "count": 2}
    ]
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: dummy-device-plugin
  namespace: kube-system
spec:
  selector:
    matchLabels:
      app: dummy-device-plugin
  template:
    metadata:
      labels:
        app: dummy-device-plugin
    spec:
      tolerations:
      - key: CriticalAddonsOnly
        operator: Exists
      - key: node-role.kubernetes.io/control-plane
        operator: Exists
        effect: NoSchedule
      hostNetwork: true
      containers:
      - name: dummy-device-plugin
        image: k8s-dummy-device-plugin:e2e-test
        imagePullPolicy: Never
        volumeMounts:
        - name: device-plugin
          mountPath: /var/lib/kubelet/device-plugins
        - name: config
          mountPath: /dummyResources.json
          subPath: dummyResources.json
      volumes:
      - name: device-plugin
        hostPath:
          path: /var/lib/kubelet/device-plugins
      - name: config
        configMap:
          name: dummy-device-plugin-config
          items:
          - key: dummyResources.json
            path: dummyResources.json
EOF

# ── Wait for DaemonSet rollout ─────────────────────────────────────
echo "==> Waiting for DaemonSet rollout..."
kubectl -n "$NAMESPACE" rollout status daemonset/dummy-device-plugin --timeout=120s

# ── Wait for resources to appear on the node ───────────────────────
echo "==> Waiting for resources to be advertised to the node..."
GPU_CAPACITY=""
NIC_CAPACITY=""
for i in $(seq 1 30); do
    GPU_CAPACITY=$(kubectl get nodes -o jsonpath='{.items[0].status.capacity.dummy/gpu}' 2>/dev/null || echo "")
    NIC_CAPACITY=$(kubectl get nodes -o jsonpath='{.items[0].status.capacity.dummy/nic}' 2>/dev/null || echo "")
    if [ "$GPU_CAPACITY" = "4" ] && [ "$NIC_CAPACITY" = "2" ]; then
        echo "    Resources available: dummy/gpu=$GPU_CAPACITY, dummy/nic=$NIC_CAPACITY"
        break
    fi
    echo "    Attempt $i/30: dummy/gpu=$GPU_CAPACITY, dummy/nic=$NIC_CAPACITY"
    sleep 5
done

if [ "$GPU_CAPACITY" != "4" ] || [ "$NIC_CAPACITY" != "2" ]; then
    echo "FAIL: Resources not available on node after waiting."
    echo "      dummy/gpu=$GPU_CAPACITY (expected 4)"
    echo "      dummy/nic=$NIC_CAPACITY (expected 2)"
    echo ""
    echo "==> DaemonSet description:"
    kubectl -n "$NAMESPACE" describe daemonset dummy-device-plugin
    echo ""
    echo "==> Plugin logs:"
    kubectl -n "$NAMESPACE" logs -l app=dummy-device-plugin --tail=50
    exit 1
fi

# ── Create test pod ────────────────────────────────────────────────
echo "==> Creating test pod..."
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: test-dummy-device
  namespace: default
spec:
  containers:
  - name: test
    image: busybox
    command: ["sleep", "3600"]
    resources:
      limits:
        dummy/gpu: 2
        dummy/nic: 1
EOF

echo "==> Waiting for test pod to be ready..."
kubectl wait --for=condition=Ready pod/test-dummy-device -n "$TEST_NAMESPACE" --timeout=120s

# ── Verify environment variables ───────────────────────────────────
echo "==> Verifying allocated device environment variables..."

GPU_ENV=$(kubectl exec -n "$TEST_NAMESPACE" test-dummy-device -- printenv DUMMY_DEVICES_DUMMY_GPU 2>/dev/null || echo "")
NIC_ENV=$(kubectl exec -n "$TEST_NAMESPACE" test-dummy-device -- printenv DUMMY_DEVICES_DUMMY_NIC 2>/dev/null || echo "")

echo "    DUMMY_DEVICES_DUMMY_GPU=$GPU_ENV"
echo "    DUMMY_DEVICES_DUMMY_NIC=$NIC_ENV"

# Validate GPU env: should contain exactly 2 comma-separated device IDs.
if [ -z "$GPU_ENV" ]; then
    echo "FAIL: DUMMY_DEVICES_DUMMY_GPU is not set."
    FAILURES=$((FAILURES + 1))
else
    GPU_COUNT=$(echo "$GPU_ENV" | tr ',' '\n' | wc -l | tr -d ' ')
    if [ "$GPU_COUNT" -ne 2 ]; then
        echo "FAIL: Expected 2 GPU devices, got $GPU_COUNT ($GPU_ENV)."
        FAILURES=$((FAILURES + 1))
    else
        echo "PASS: GPU devices allocated correctly."
    fi
fi

# Validate NIC env: should contain exactly 1 device ID.
if [ -z "$NIC_ENV" ]; then
    echo "FAIL: DUMMY_DEVICES_DUMMY_NIC is not set."
    FAILURES=$((FAILURES + 1))
else
    NIC_COUNT=$(echo "$NIC_ENV" | tr ',' '\n' | wc -l | tr -d ' ')
    if [ "$NIC_COUNT" -ne 1 ]; then
        echo "FAIL: Expected 1 NIC device, got $NIC_COUNT ($NIC_ENV)."
        FAILURES=$((FAILURES + 1))
    else
        echo "PASS: NIC devices allocated correctly."
    fi
fi

# ── Results ────────────────────────────────────────────────────────
echo ""
if [ "$FAILURES" -gt 0 ]; then
    echo "==> $FAILURES test(s) FAILED."
    echo ""
    echo "==> Debug — plugin logs:"
    kubectl -n "$NAMESPACE" logs -l app=dummy-device-plugin --tail=50
    exit 1
fi

echo "==> All tests PASSED!"
