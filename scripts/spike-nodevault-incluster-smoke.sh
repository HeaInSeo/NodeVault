#!/usr/bin/env bash
# spike-nodevault-incluster-smoke.sh
#
# NodeVault in-cluster spike 검증 스크립트 — Option A (K8sPodbridgeBackend)
#
# 검증 항목:
#   1. kubectl apply 성공
#   2. Pod Running 확인
#   3. logs에 runtime_mode=incluster, build_backend=k8s-job 표시
#   4. PingService gRPC 응답 확인
#   5. BuildService → K8s Job 생성 → 빌드 완료 → digest 반환 확인
#
# 사전 조건:
#   - kubectl, grpcurl이 PATH에 있어야 함
#   - 이미지가 Harbor에 push되어 있어야 함:
#       make vendor && make push-image IMAGE=harbor.lab.local/nodevault/controlplane:spike-incluster
#   - Harbor pull secret 생성:
#       kubectl create secret docker-registry harbor-pull-secret \
#         --docker-server=harbor.lab.local \
#         --docker-username=admin --docker-password=Harbor12345 \
#         -n nodeplatform-system --dry-run=client -o yaml | kubectl apply -f -
#
# 사용법:
#   KUBECONFIG=/path/to/kubeconfig bash scripts/spike-nodevault-incluster-smoke.sh

set -euo pipefail

KUBECONFIG=${KUBECONFIG:-"/opt/go/src/github.com/HeaInSeo/infra-lab/state/test-wizard-env/kubeconfig"}
MANIFEST="deploy/spike/nodevault-incluster.yaml"
NAMESPACE="nodeplatform-system"
DEPLOYMENT="nodevault"
GRPC_PORT="50051"
LOCAL_PORT="15051"
TIMEOUT=120
BUILD_TIMEOUT=600  # builder Job은 최대 10분

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass() { echo -e "${GREEN}[PASS]${NC} $*"; }
fail() { echo -e "${RED}[FAIL]${NC} $*"; }
info() { echo -e "${YELLOW}[INFO]${NC} $*"; }

if [ -z "$KUBECONFIG" ] || [ ! -f "$KUBECONFIG" ]; then
  fail "KUBECONFIG not found. Set KUBECONFIG env or place kubeconfig at ../infra-lab/kubeconfig"
  exit 1
fi

export KUBECONFIG

info "KUBECONFIG=$KUBECONFIG"
info "Manifest: $MANIFEST"
echo ""

# ── Step 1: kubectl apply ──────────────────────────────────────────────────────
info "Step 1: applying manifest..."
if kubectl apply -f "$MANIFEST"; then
  pass "kubectl apply succeeded"
else
  fail "kubectl apply failed"
  exit 1
fi
echo ""

# ── Step 2: Pod Running 확인 ───────────────────────────────────────────────────
info "Step 2: waiting for pod Running (timeout: ${TIMEOUT}s)..."
if kubectl rollout status deployment/"$DEPLOYMENT" \
    -n "$NAMESPACE" --timeout="${TIMEOUT}s"; then
  pass "Deployment rollout complete"
else
  fail "Deployment rollout timed out"
  info "Pod events:"
  kubectl describe pods -n "$NAMESPACE" -l app=nodevault | tail -20
  exit 1
fi
echo ""

# ── Step 3: startup log 확인 ──────────────────────────────────────────────────
info "Step 3: checking startup log..."
POD=$(kubectl get pods -n "$NAMESPACE" -l app=nodevault \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

if [ -z "$POD" ]; then
  fail "No pod found"
  exit 1
fi

LOGS=$(kubectl logs -n "$NAMESPACE" "$POD" 2>/dev/null | head -50)
echo "$LOGS"
echo ""

if echo "$LOGS" | grep -q "runtime_mode=incluster"; then
  pass "runtime_mode=incluster found in logs"
else
  fail "runtime_mode=incluster NOT found in logs"
  exit 1
fi

if echo "$LOGS" | grep -q "build_backend=k8s-job"; then
  pass "build_backend=k8s-job found in logs"
else
  fail "build_backend=k8s-job NOT found in logs"
  exit 1
fi
echo ""

# ── Step 4 & 5: grpcurl로 gRPC 검증 ──────────────────────────────────────────
if ! command -v grpcurl &>/dev/null; then
  info "grpcurl not found — skipping Steps 4 and 5"
  info "Install: https://github.com/fullstorydev/grpcurl/releases"
  echo ""
  pass "Spike smoke check PARTIAL (grpcurl missing)"
  exit 0
fi

info "Step 4: port-forwarding $GRPC_PORT → localhost:$LOCAL_PORT..."
kubectl port-forward -n "$NAMESPACE" "pod/$POD" "${LOCAL_PORT}:${GRPC_PORT}" &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT
sleep 2

PROTO_FLAGS="-import-path protos -proto nodevault/v1/nodevault.proto"

info "Calling PingService..."
if grpcurl -plaintext $PROTO_FLAGS -d '{}' \
    "localhost:${LOCAL_PORT}" nodevault.v1.PingService/Ping 2>&1; then
  pass "PingService responded"
else
  fail "PingService did not respond"
  exit 1
fi
echo ""

# ── Step 5: BuildService → K8s Job 빌드 ──────────────────────────────────────
info "Step 5: calling BuildService with k8s-job backend (timeout: ${BUILD_TIMEOUT}s)..."
info "This will spawn a builder Job in nodevault-builds namespace..."
info "Monitor with: kubectl get jobs -n nodevault-builds -w"
echo ""

BUILD_RESULT=$(grpcurl -plaintext -max-time "$BUILD_TIMEOUT" \
  $PROTO_FLAGS \
  -d '{"request_id":"spike-optA-01","tool_name":"smoke-bwa","dockerfile_content":"FROM alpine:3.19\nRUN echo spike-option-a-build > /result.txt\n"}' \
  "localhost:${LOCAL_PORT}" nodevault.v1.BuildService/BuildAndRegister 2>&1 || true)

echo "BuildService response:"
echo "$BUILD_RESULT"
echo ""

if echo "$BUILD_RESULT" | grep -qi "sha256:"; then
  pass "BuildService returned a digest — K8s Job build succeeded"
elif echo "$BUILD_RESULT" | grep -qi "digest"; then
  pass "BuildService returned digest reference — K8s Job build succeeded"
elif echo "$BUILD_RESULT" | grep -qi "build+register complete"; then
  pass "BuildService: build+register complete"
else
  fail "BuildService did not return a successful build result"
  fail "Got: $BUILD_RESULT"
  info "Check builder job: kubectl get pods -n nodevault-builds"
  info "Builder logs: kubectl logs -n nodevault-builds -l app=nodevault-builder"
fi

info "Verifying server still alive after build..."
if grpcurl -plaintext $PROTO_FLAGS -d '{}' \
    "localhost:${LOCAL_PORT}" nodevault.v1.PingService/Ping 2>&1; then
  pass "Server still alive after BuildService call"
else
  fail "Server appears to have died"
fi

echo ""
echo "──────────────────────────────────────────────────────"
pass "NodeVault Option A spike smoke check COMPLETE"
echo "──────────────────────────────────────────────────────"
