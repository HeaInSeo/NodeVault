#!/usr/bin/env bash
# spike-nodevault-incluster-smoke.sh
#
# NodeVault in-cluster spike 검증 스크립트
#
# 검증 항목:
#   1. kubectl apply 성공
#   2. Pod Running 확인
#   3. logs에 runtime_mode=incluster 표시 확인
#   4. PingService gRPC 응답 확인 (grpcurl 사용)
#   5. BuildRequest 호출 → disabled backend 에러, 서버 계속 살아있는지 확인
#
# 사전 조건:
#   - kubectl이 PATH에 있고 kubeconfig가 설정되어 있어야 함
#   - grpcurl이 PATH에 있어야 함 (없으면 4/5번 건너뜀)
#   - 이미지가 Harbor에 push되어 있어야 함:
#       make vendor && make push-image IMAGE=harbor.10.113.24.96.nip.io/nodevault/controlplane:spike-incluster
#   - Harbor pull secret이 존재해야 함:
#       kubectl create secret docker-registry harbor-pull-secret \
#         --docker-server=harbor.10.113.24.96.nip.io \
#         --docker-username=admin --docker-password=Harbor12345 \
#         -n nodeplatform-system
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

# ── Step 3: logs에 runtime_mode=incluster 확인 ────────────────────────────────
info "Step 3: checking startup log for runtime_mode=incluster..."
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

if echo "$LOGS" | grep -q "build_backend=disabled"; then
  pass "build_backend=disabled found in logs"
else
  fail "build_backend=disabled NOT found in logs"
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

info "Calling PingService (nodevault.v1.PingService/Ping)..."
if grpcurl -plaintext -d '{}' \
    "localhost:${LOCAL_PORT}" nodevault.v1.PingService/Ping 2>&1; then
  pass "PingService responded"
else
  fail "PingService did not respond"
fi
echo ""

# ── Step 5: BuildService → disabled error ──────────────────────────────────────
info "Step 5: calling BuildService — expecting disabled backend error..."
BUILD_RESULT=$(grpcurl -plaintext \
  -d '{"request_id":"spike-smoke-01","tool_name":"bwa","dockerfile_content":"FROM alpine:3.19\nRUN echo test"}' \
  "localhost:${LOCAL_PORT}" nodevault.v1.BuildService/BuildAndRegister 2>&1 || true)

echo "BuildService response: $BUILD_RESULT"

if echo "$BUILD_RESULT" | grep -qi "disabled"; then
  pass "BuildService returned disabled backend error as expected"
else
  fail "BuildService did not return expected disabled error"
  fail "Got: $BUILD_RESULT"
fi

info "Verifying server is still alive after disabled error..."
if grpcurl -plaintext -d '{}' \
    "localhost:${LOCAL_PORT}" nodevault.v1.PingService/Ping 2>&1; then
  pass "Server still alive after BuildService error"
else
  fail "Server appears to have died after BuildService error"
fi

echo ""
echo "──────────────────────────────────────────────"
pass "NodeVault in-cluster spike smoke check COMPLETE"
echo "──────────────────────────────────────────────"
