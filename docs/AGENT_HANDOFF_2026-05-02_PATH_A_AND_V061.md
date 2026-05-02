# Agent Handoff — Path A and v0.6.1 Baseline

목적: 여러 에이전트가 동시에 NodeVault / NodeKit / infra-lab 작업을 진행할 때,
이번 세션에서 어떤 문서를 근거로 판단했는지와 현재 운영/개발 상태를 빠르게 공유한다.

작성 시점: 2026-05-02

---

## 1. 이번 세션의 핵심 결론

- 현재 NodeVault 운영 모델은 **seoy 호스트 바이너리 실행**이다.
- 현재 `deploy/03-nodevault.yaml`, `deploy/04-grpcroute.yaml`은 **미사용**이며 미래 in-cluster 전환용이다.
- `Path A`는 GRPCRoute 적용이 아니라:
  - `nodevault` 서비스 계정 `subuid/subgid` 보정
  - `/opt/nodevault/kubeconfig` 배포
  - seoy에서 `make test-integration-infralab`가 가능한 상태 확보
- `NodeForge`는 seoy 호스트 서비스 및 cluster namespace 기준으로 제거했고,
  현재 `nodevault.service` / `nodepalette.service`가 활성 상태다.

---

## 2. 참조한 문서

### 운영 모델 / Path A 관련

- `NodeVault/CLAUDE.md`
  - §5 kubeconfig / K8s API access
  - §12 D-01 / D-02 known deployment issues
- `NodeVault/docs/INFRALAB_TESTING.md`
  - host-binary + remote cluster 모델
  - `make deploy-infralab`
  - `make test-integration-infralab`
- `NodeVault/docs/DEPLOY_IN_CLUSTER.md`
  - `deploy/03-nodevault.yaml` / `deploy/04-grpcroute.yaml`이 현재 미사용임을 확인
- `NodeVault/docs/PLATFORM_MAP.md`
  - NodeVault gRPC가 `100.123.80.48:50051` direct host access 기준임을 확인

### 업그레이드 설계 v0.6.1 관련

- `batch-integration/docs/master-plan/NodeKit/NodeVault_Reproducible_Tool_Authoring_업그레이드_설계_v0.6.1.md`
  - additive field:
    - `authoringHash`
    - `validationHash`
    - `observedProfileDigest`
    - `securityScanDigest`
  - new referrer axis:
    - `toolspec` 유지
    - `toolprofile` 추가
    - `security` referrer 추가
  - verification split:
    - L1 NodeKit
    - L2 build
    - L3 dry-run
    - L4 smoke run
    - L5-a Validator/Profiler
    - L5-b Security Scan

### 현재 상태 비교용 문서 / 코드

- `NodeVault/protos/nodevault/v1/nodevault.proto`
- `NodeVault/pkg/build/service.go`
- `NodeVault/pkg/oras/referrer.go`
- `NodeVault/pkg/catalog/data.go`
- `NodeVault/docs/NODEVAULT_TRANSITION_PLAN.md`
- `NodeKit/CLAUDE.md`
- `NodeKit/src/Grpc/DataRegisterRequestFactory.cs`
- `NodeKit/UI/MainWindow.axaml.cs`

---

## 3. 현재 구현 상태 요약

### 이미 구현된 축

- NodeKit L1 authoring + DockGuard WASM validation
- NodeVault L2 build + L3 dry-run + L4 smoke run
- NodeVault index dual-axis model
  - `lifecycle_phase`
  - `integrity_health`
- `toolspec` OCI referrer push
- Catalog REST read path
- Data read path
  - NodeVault `DataRegistryService` 존재
  - NodeKit `AdminDataList` 조회 존재

### 아직 미완료인 v0.6.1 핵심 축

- proto / storage additive field
  - `authoringHash`
  - `validationHash`
  - `observedProfileDigest`
  - `securityScanDigest`
- `toolprofile` referrer
- `security` referrer
- L5-a Validator / Profiler
- L5-b Security Scan
- NodeKit data registration UI / gRPC send path

---

## 4. 이번 세션에서 적용한 변경

### 운영 자동화

- `scripts/deploy-seoy.sh`
  - `nodevault` 사용자 `subuid/subgid` 자동 보정 추가
  - kubeconfig 표준 정책 추가
    - 기본: `KUBECONFIG_MODE=remote`
    - 명시적 local 주입: `KUBECONFIG_MODE=local LOCAL_KUBECONFIG=/path/...`
    - 생략: `KUBECONFIG_MODE=skip`
  - multi-agent 환경에서는 로컬 기본 kubeconfig 자동 주입을 금지

### 문서 보정

- `CLAUDE.md`
  - D-01 / D-02를 “추가 예정”이 아닌 현재 스크립트 동작 기준으로 갱신
- `docs/NODEVAULT_TRANSITION_PLAN.md`
  - TODO-12 data path 상태를 현재 코드 기준으로 보정

---

## 5. seoy 운영 상태

2026-05-02 기준 확인된 상태:

- `nodevault.service`: active
- `nodepalette.service`: active
- NodeForge host service: 제거
- NodeForge cluster namespaces / RBAC: 제거
- NodeVault cluster namespaces / RBAC: 적용
  - `nodevault-system`
  - `nodevault-builds`
  - `nodevault-smoke`

참고:
- 현 시점에서는 `GRPCRoute`가 없다.
- 이는 정상이다. 현재 host-binary 모델에서는 direct host access가 정식 경로다.

---

## 6. 다음 에이전트가 바로 이어서 할 일

우선순위 1:
- `make deploy-infralab`
- `make test-integration-infralab`
- 결과를 이 문서 또는 실행 로그 문서에 기록

우선순위 2:
- `nodevault.proto`에 additive field 초안 추가
- `toolprofile` / `security` referrer용 저장 경로 설계 시작

우선순위 3:
- NodeKit data registration UI 연결 여부 결정
- `DataRegisterRequestFactory`를 실제 gRPC send path에 연결

---

## 7. 주의사항

- 현재 운영 모델에서 `deploy/04-grpcroute.yaml`을 apply하지 않는다.
- 현재 운영 모델에서 NodeKit은 `100.123.80.48:50051`로 직접 접속한다.
- 문서 중 일부 오래된 계획 문서는 실제 코드보다 뒤처져 있을 수 있으므로,
  proto / service / build path는 반드시 코드로 재확인한다.
