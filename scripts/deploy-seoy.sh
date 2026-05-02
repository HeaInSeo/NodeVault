#!/usr/bin/env bash
# deploy-seoy.sh — NodeVault + NodePalette를 seoy 호스트에 배포한다.
#
# 사전 조건:
#   - seoy (100.123.80.48)에 SSH 키 접근 가능
#   - seoy에 nodevault 사용자 및 /opt/nodevault 디렉토리 존재
#     (없으면 스크립트 내 setup-host 단계가 생성)
#   - make build 또는 go build 로 bin/nodevault, bin/nodepalette 빌드 완료
#
# 사용법:
#   ./scripts/deploy-seoy.sh
#   ./scripts/deploy-seoy.sh --restart-only   # 바이너리 복사 없이 재시작만
#
set -euo pipefail

SEOY_HOST="${SEOY_HOST:-100.123.80.48}"
SEOY_USER="${SEOY_USER:-seoy}"
REMOTE_DIR="/opt/nodevault"
LOCAL_BIN="$(dirname "$0")/../bin"
LOCAL_DEPLOY="$(dirname "$0")/../deploy"
LOCAL_ASSETS="$(dirname "$0")/../assets"
KUBECONFIG_MODE="${KUBECONFIG_MODE:-remote}"
LOCAL_KUBECONFIG="${LOCAL_KUBECONFIG:-}"
REMOTE_KUBECONFIG_SOURCE="${REMOTE_KUBECONFIG_SOURCE:-/opt/go/src/github.com/HeaInSeo/infra-lab/kubeconfig}"

RESTART_ONLY=false
if [[ "${1:-}" == "--restart-only" ]]; then
  RESTART_ONLY=true
fi

SSH="ssh ${SEOY_USER}@${SEOY_HOST}"
SCP_BASE="rsync -av --progress"

echo "==> 대상: ${SEOY_USER}@${SEOY_HOST}:${REMOTE_DIR}"

# ── 1. 원격 디렉토리 구조 초기화 (최초 1회) ─────────────────────────────────
$SSH "bash -s" <<'REMOTE_SETUP'
set -euo pipefail
if ! id nodevault &>/dev/null; then
  sudo useradd -r -s /bin/false -d /opt/nodevault nodevault
fi
sudo mkdir -p /opt/nodevault/{bin,assets/index,assets/catalog,assets/datacatalog,assets/policy}
sudo chown -R nodevault:nodevault /opt/nodevault
if ! grep -q '^nodevault:' /etc/subuid 2>/dev/null; then
  sudo usermod --add-subuids 100000-165535 nodevault
fi
if ! grep -q '^nodevault:' /etc/subgid 2>/dev/null; then
  sudo usermod --add-subgids 100000-165535 nodevault
fi
REMOTE_SETUP

if [[ "$RESTART_ONLY" == "true" ]]; then
  echo "==> --restart-only: 바이너리 복사 생략"
else
  # ── 2. 바이너리 복사 (/tmp 경유 → sudo mv) ────────────────────────────────
  # /opt/nodevault/bin/ 은 nodevault 소유 → seoy 유저가 직접 쓸 수 없으므로
  # /tmp에 먼저 올린 뒤 sudo mv 로 이동한다.
  echo "==> 바이너리 복사..."
  $SCP_BASE "${LOCAL_BIN}/nodevault"   "${SEOY_USER}@${SEOY_HOST}:/tmp/nodevault"
  $SCP_BASE "${LOCAL_BIN}/nodepalette" "${SEOY_USER}@${SEOY_HOST}:/tmp/nodepalette"
  $SSH "sudo mv /tmp/nodevault   ${REMOTE_DIR}/bin/nodevault && \
        sudo mv /tmp/nodepalette ${REMOTE_DIR}/bin/nodepalette && \
        sudo chown nodevault:nodevault ${REMOTE_DIR}/bin/nodevault ${REMOTE_DIR}/bin/nodepalette && \
        sudo chmod +x ${REMOTE_DIR}/bin/nodevault ${REMOTE_DIR}/bin/nodepalette && \
        sudo restorecon -v ${REMOTE_DIR}/bin/nodevault ${REMOTE_DIR}/bin/nodepalette 2>/dev/null || true"

  # ── 3. policy wasm 복사 (있는 경우) ───────────────────────────────────────
  if [[ -f "${LOCAL_ASSETS}/policy/dockguard.wasm" ]]; then
    echo "==> policy wasm 복사..."
    $SCP_BASE "${LOCAL_ASSETS}/policy/dockguard.wasm" \
      "${SEOY_USER}@${SEOY_HOST}:/tmp/dockguard.wasm"
    $SSH "sudo mv /tmp/dockguard.wasm ${REMOTE_DIR}/assets/policy/dockguard.wasm && \
          sudo chown nodevault:nodevault ${REMOTE_DIR}/assets/policy/dockguard.wasm && \
          sudo restorecon -v ${REMOTE_DIR}/assets/policy/dockguard.wasm 2>/dev/null || true"
  else
    echo "==> assets/policy/dockguard.wasm 없음 — 건너뜀"
  fi
fi

# ── 4. kubeconfig 배포 ─────────────────────────────────────────────────────────
case "${KUBECONFIG_MODE}" in
  remote)
    echo "==> kubeconfig 복사 (remote authoritative source)..."
    $SSH "test -f '${REMOTE_KUBECONFIG_SOURCE}' && \
          sudo cp '${REMOTE_KUBECONFIG_SOURCE}' '${REMOTE_DIR}/kubeconfig' && \
          sudo chown nodevault:nodevault '${REMOTE_DIR}/kubeconfig' && \
          sudo chmod 600 '${REMOTE_DIR}/kubeconfig' && \
          sudo restorecon -v '${REMOTE_DIR}/kubeconfig' 2>/dev/null || true"
    ;;
  local)
    if [[ -z "${LOCAL_KUBECONFIG}" || ! -f "${LOCAL_KUBECONFIG}" ]]; then
      echo "ERROR: KUBECONFIG_MODE=local 이면 LOCAL_KUBECONFIG=/path/to/kubeconfig 가 필요합니다." >&2
      exit 1
    fi
    echo "==> kubeconfig 복사 (local explicit source)..."
    $SCP_BASE "${LOCAL_KUBECONFIG}" "${SEOY_USER}@${SEOY_HOST}:/tmp/nodevault.kubeconfig"
    $SSH "sudo mv /tmp/nodevault.kubeconfig ${REMOTE_DIR}/kubeconfig && \
          sudo chown nodevault:nodevault ${REMOTE_DIR}/kubeconfig && \
          sudo chmod 600 ${REMOTE_DIR}/kubeconfig && \
          sudo restorecon -v ${REMOTE_DIR}/kubeconfig 2>/dev/null || true"
    ;;
  skip)
    echo "==> kubeconfig 배포 생략 (KUBECONFIG_MODE=skip)"
    ;;
  *)
    echo "ERROR: 알 수 없는 KUBECONFIG_MODE='${KUBECONFIG_MODE}'. expected: remote|local|skip" >&2
    exit 1
    ;;
esac

# ── 5. systemd 서비스 파일 배포 ───────────────────────────────────────────────
echo "==> systemd 서비스 파일 배포..."
$SCP_BASE "${LOCAL_DEPLOY}/nodevault.service"   "${SEOY_USER}@${SEOY_HOST}:/tmp/nodevault.service"
$SCP_BASE "${LOCAL_DEPLOY}/nodepalette.service" "${SEOY_USER}@${SEOY_HOST}:/tmp/nodepalette.service"
$SSH "sudo mv /tmp/nodevault.service /etc/systemd/system/nodevault.service && \
      sudo mv /tmp/nodepalette.service /etc/systemd/system/nodepalette.service && \
      sudo systemctl daemon-reload"

# ── 6. 서비스 활성화 및 재시작 ────────────────────────────────────────────────
echo "==> 서비스 재시작..."
$SSH "sudo systemctl enable nodevault nodepalette && \
      sudo systemctl restart nodevault nodepalette"

# ── 7. 기동 확인 (최대 30초 대기) ───────────────────────────────────────────
echo "==> 서비스 기동 대기 (최대 30초)..."
for i in $(seq 1 6); do
  sleep 5
  STATUS=$($SSH "sudo systemctl is-active nodevault 2>/dev/null || true")
  echo "    [${i}/6] nodevault: ${STATUS}"
  if [[ "$STATUS" == "active" ]]; then
    break
  fi
done

echo ""
echo "--- nodevault 상태 ---"
$SSH "sudo systemctl status nodevault --no-pager -l || true"
echo ""
echo "--- nodevault log (last 20 lines) ---"
$SSH "sudo journalctl -u nodevault -n 20 --no-pager || true"
echo ""
echo "--- nodepalette log (last 10 lines) ---"
$SSH "sudo journalctl -u nodepalette -n 10 --no-pager || true"

echo ""
echo "✅ 배포 완료"
echo "   NodeVault gRPC  : ${SEOY_HOST}:50051"
echo "   NodeVault webhook: ${SEOY_HOST}:8082"
echo "   NodePalette REST : ${SEOY_HOST}:8080"
echo ""
echo "   연결 확인:"
echo "     grpc: grpcurl -plaintext ${SEOY_HOST}:50051 nodevault.v1.PingService/Ping"
echo "     rest: curl http://${SEOY_HOST}:8080/v1/catalog/tools"
