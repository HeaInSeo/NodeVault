# NodeVault

관리자 UI(NodeKit)에서 `BuildRequest`를 받아 tool 이미지를 빌드·검증·등록하는 제어 플레인 서버.
gRPC 서버 + Catalog REST API를 단일 바이너리(`bin/nodevault`)로 제공한다.

→ 전체 플랫폼 구성 및 end-to-end 흐름: [docs/PLATFORM_MAP.md](docs/PLATFORM_MAP.md)
→ TrueNAS/iLO/NFS 운영 메모: [docs/TRUENAS_NFS_RUNBOOK.md](docs/TRUENAS_NFS_RUNBOOK.md)

---

## 전체 구조

```
NodeKit (C# 어드민 UI)
    │  BuildRequest (gRPC)
    ▼
NodeVault (이 프로젝트 — Go gRPC + REST 서버)
    │
    ├── L2: 이미지 빌드 백엔드 (NODEVAULT_BUILD_BACKEND 선택)
    │       local-podbridge  → podbridge5 in-process (seoy 호스트)
    │       k8s-job          → K8s Job (buildah, privileged, incluster)
    │       disabled         → 빌드 없이 gRPC 서버만 기동
    ├── L3: K8s Job dry-run (스키마 검증)
    ├── L4: K8s smoke run (컨테이너 실행 검증)
    ├── 등록: CAS 저장 + pkg/index append (lifecycle_phase=Active)
    ├── Catalog REST: pkg/catalogrest (lifecycle_phase=Active만 노출)
    └── /debug/vars: expvar 메트릭 (NODEVAULT_METRICS_ADDR, 기본 :9090)
    │
    ▼
Harbor (harbor.10.113.24.96.nip.io)
    └── library/<tool>:latest + digest
```

---

## gRPC 서비스 목록

| 서비스 | 패키지 | 설명 |
|--------|--------|------|
| `PingService` | `pkg/ping` | 연결 확인 |
| `PolicyService` | `pkg/policy` | DockGuard `.wasm` 번들 제공 (NodeKit L1 정책 평가) |
| `BuildService` | `pkg/build` | L2→L3→L4→등록 전체 파이프라인 + BuildEvent 스트림 |
| `ValidateService` | `pkg/validate` | L3 dry-run / L4 smoke run (BuildService 내부 호출) |

프로토 정의: [`protos/nodevault/v1`](protos/nodevault/v1/)

---

## Catalog REST API

`pkg/catalogrest`가 동일 바이너리 안에서 HTTP REST를 제공한다.

| 엔드포인트 | 설명 |
|------------|------|
| `GET /api/v1/tools` | `lifecycle_phase=Active` tool 목록 |
| `GET /api/v1/data` | `lifecycle_phase=Active` data 목록 |
| `GET /api/v1/tools/{casHash}` | casHash 기준 단건 조회 |

---

## 메트릭 (`/debug/vars`)

NodeVault는 `expvar` 기반 메트릭을 `NODEVAULT_METRICS_ADDR`(기본 `:9090`)에서 제공한다.

```bash
curl http://localhost:9090/debug/vars
```

| 카운터 | 설명 |
|--------|------|
| `nodevault_reconcile_fast_total` | FastRun 실행 횟수 (integrity 존재 확인 루프) |
| `nodevault_reconcile_slow_total` | SlowRun 실행 횟수 (pull 도달 가능성 확인 루프) |
| `nodevault_reconcile_error_total` | 리콘사일 루프 오류 횟수 |
| `nodevault_build_success_total` | L2 이미지 빌드 성공 횟수 |
| `nodevault_build_failure_total` | L2 이미지 빌드 실패 횟수 |

헬스체크: `GET /healthz` → `200 ok`

---

## 빠른 시작

### 사전 조건

| 도구 | 용도 |
|------|------|
| Go 1.22+ | 빌드 |
| CGO 빌드 의존성 | `pkg/build` (podbridge5): gpgme, btrfs-progs-devel 등 |
| kubectl | L3/L4 K8s 연동 |

### 빌드 및 실행

```bash
# 바이너리 빌드 (CGO 의존성 필요)
make build

# seoy 배포 (SSH + rsync)
make deploy-seoy

# 직접 실행 (disabled 모드 — 빌드 없이 gRPC 서버만 기동)
NODEVAULT_BUILD_BACKEND=disabled ./bin/nodevault
```

### 환경 변수

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `NODEVAULT_ADDR` | `:50051` | gRPC 서버 바인딩 주소 |
| `NODEVAULT_WEBHOOK_ADDR` | `:8082` | Harbor webhook 수신 주소 |
| `NODEVAULT_METRICS_ADDR` | `:9090` | expvar 메트릭 HTTP 서버 주소 |
| `NODEVAULT_BUILD_BACKEND` | `local-podbridge` | 빌드 백엔드: `local-podbridge` / `k8s-job` / `disabled` |
| `NODEVAULT_RUNTIME_MODE` | `host` | 실행 모드: `host` / `incluster` |
| `NODEVAULT_FAST_RECONCILE` | `5m` | FastRun 주기 (integrity 존재 확인) |
| `NODEVAULT_SLOW_RECONCILE` | `30m` | SlowRun 주기 (pull 도달 가능성) |
| `NODEVAULT_REGISTRY_ADDR` | `harbor.10.113.24.96.nip.io` | 이미지 push 대상 Harbor 주소 |
| `NODEVAULT_ORAS_INSECURE_TLS` | `false` | ORAS referrer push TLS 검증 비활성화 |
| `HARBOR_USER` / `HARBOR_PASS` | — | Harbor 인증 정보 |
| `CATALOG_DIR` | `assets/catalog` | tool CAS 파일 저장 디렉토리 |
| `DATA_CATALOG_DIR` | `assets/data-catalog` | data CAS 파일 저장 디렉토리 |
| `INDEX_DIR` | `assets/index` | vault-index.json 저장 디렉토리 |
| `KUBECONFIG` | `~/.kube/config` | K8s 클러스터 인증 |

---

## 테스트

```bash
# 유닛 테스트 (race detector + coverage)
make test

# 커버리지 리포트
make coverage

# kube-slint gate (churn 억제 + 회귀 검증)
# 사전 조건: make build
make slint

# 취약점 스캔
make vuln
```

### CI (GitHub Actions)

4+1 job 구성 (`[self-hosted, linux, podbridge5]`):

| Job | 내용 |
|-----|------|
| `lint` | golangci-lint (zero-warning) |
| `build` | go build + go vet |
| `test-unit` | -race -cover, coverage artifact 업로드 |
| `vuln-scan` | govulncheck (continue-on-error) |
| `slint-gate` | kube-slint churn/regression gate (25s window) |

### kube-slint gate

[kube-slint](https://github.com/HeaInSeo/kube-slint) SLI 게이트는 `NODEVAULT_BUILD_BACKEND=disabled` + `NODEVAULT_FAST_RECONCILE=5s` 조건에서 25초 관찰 창을 측정한다.

**정책** (`.slint/policy.yaml`):

| SLI | 조건 | 목적 |
|-----|------|------|
| `reconcile_fast_delta` | `>= 1` | 리콘사일 루프 liveness |
| `reconcile_fast_delta` | `<= 15` | churn 억제 (25s에 최대 15회) |
| `reconcile_error_delta` | `== 0` | 회귀 없음 |
| `build_failure_delta` | `== 0` | 빌드 실패 없음 |

---

## 패키지 구조

```
cmd/controlplane/     — gRPC + webhook 서버 진입점 (main.go)
cmd/palette/          — NodePalette REST 서버 (별도 바이너리)
pkg/
  build/              — BuildService: L2 빌드 백엔드 + L3/L4 오케스트레이션
  catalog/            — RegisteredDefinition CAS 저장/조회
  catalogrest/        — Catalog REST API + Harbor webhook 수신
  index/              — vault-index.json: lifecycle_phase / integrity_health 이중 축
  metrics/            — expvar 카운터 + /debug/vars HTTP 서버
  oras/               — OCI spec referrer push (sori wrapping)
  ping/               — PingService: 헬스체크
  policy/             — PolicyService: DockGuard .wasm 번들 제공
  reconcile/          — Harbor 현실 대조 → integrity_health 갱신 (FastRun / SlowRun)
  registry/           — OCI 레지스트리 클라이언트
  validate/           — ValidateService: L3 dry-run / L4 smoke run
test/
  slint/              — kube-slint gate 테스트 (build tag: slint)
protos/nodevault/v1/  — gRPC proto 정의
deploy/               — K8s 매니페스트 (namespaces, RBAC, Deployment)
.slint/               — kube-slint policy.yaml (슬린트 게이트 임계값)
```

---

## artifact 상태 이중 축

`vault-index.json`은 두 축으로 artifact 상태를 분리한다.

| 축 | 값 | 변경 주체 | 의미 |
|----|-----|-----------|------|
| `lifecycle_phase` | Pending / Active / Retracted / Deleted | NodeVault 명시적 호출만 | 관리자의 승인 의도 |
| `integrity_health` | Healthy / Partial / Missing / Unreachable / Orphaned | reconcile loop만 | Harbor 현실 대조 결과 |

**Catalog 노출**: `lifecycle_phase = Active`인 것만. `integrity_health`는 무관.

---

## 오케스트레이션 흐름

`BuildService.BuildAndRegister` 스트리밍 RPC 실행 순서:

```
1. L2: 빌드 백엔드 (local-podbridge / k8s-job) → Harbor push → digest 확보
2. L3: K8s dry-run — smoke Job spec 스키마 검증
3. L4: K8s smoke run — 실제 Job 실행, 정상 종료 확인
4. 등록:
   ├── pkg/catalog: CAS JSON 저장
   ├── pkg/index: vault-index.json append (lifecycle_phase=Active)
   └── pkg/oras: OCI spec referrer push → integrity_health=Healthy
```

이벤트는 `BuildEvent` 스트림으로 NodeKit에 실시간 전달.

---

## DockGuard 정책

NodeKit L1 정책 평가에 사용되는 `.wasm` 번들은 [`DockGuard`](https://github.com/HeaInSeo/DockGuard) 레포에서 빌드.

| 패키지 | 규칙 | 설명 |
|--------|------|------|
| `dockerfile.multistage` | DFM001–DFM004 | 멀티스테이지 빌드 강제 |
| `dockerfile.security` | DSF001–DSF003 | root 실행 금지, 시크릿 노출 금지, ADD URL 금지 |
| `dockerfile.genomics` | DGF001–DGF002 | conda/pip 버전 고정 강제 |

---

## 관련 프로젝트

| 프로젝트 | 역할 |
|----------|------|
| [`NodeKit`](https://github.com/HeaInSeo/NodeKit) | C# 어드민 UI — ToolDefinition 편집, L1 검증, BuildRequest gRPC 전송 |
| [`DockGuard`](https://github.com/HeaInSeo/DockGuard) | OPA/Rego Dockerfile 정책 + .wasm 번들 빌드 |
| [`kube-slint`](https://github.com/HeaInSeo/kube-slint) | K8s SLI 게이트 (churn 억제, 회귀 검증) |
| [`bori`](https://github.com/HeaInSeo/bori) | K8s 오퍼레이터 — BoriDataPlane CRD로 NodeVault 배포 관리 (예정) |
| [`infra-lab`](https://github.com/HeaInSeo/infra-lab) | VM 기반 K8s 테스트 클러스터 (multipass / libvirt backend) |
