# ADR: NodeVault In-Cluster Spike

날짜: 2026-06-12  
브랜치: `spike/nodevault-incluster-controlplane`  
상태: **Spike 진행 중**

---

## 1. 배경

현재 NodeVault는 seoy 호스트 바이너리로 실행된다.

```
[seoy 호스트]
  bin/nodevault
    ├── gRPC :50051  (BuildService, PolicyService, ValidateService, ToolRegistryService, PingService)
    ├── HTTP :8082   (Harbor webhook)
    └── podbridge5 (buildah) in-process 호출  ← L2 이미지 빌드
```

이 구조가 선택된 이유: **podbridge5(buildah) rootless 제약**.  
K8s Pod 안에서 overlay 파일시스템 마운트가 불가능하기 때문에, L2 이미지 빌드는 반드시 호스트에서 실행해야 한다.

그러나 NodeVault의 다른 기능(Ping, Policy, Catalog, ValidateService, reconcile)은 L2 빌드와 독립적이며, 이론상 K8s 내부에서 실행할 수 있다.

---

## 2. 이번 Spike의 목표

**NodeVault Core가 K8s 내부 Pod로 기동 가능한지 빠르게 검증한다.**

전체 기능 이전이 아니다. 확인 항목:

- `NODEVAULT_RUNTIME_MODE=incluster` 환경 변수로 in-cluster K8s API 접근이 되는가?
- `NODEVAULT_BUILD_BACKEND=disabled`로 BuildService를 비활성화한 상태에서 gRPC 서버가 정상 기동되는가?
- PingService, PolicyService, Catalog read path가 Pod 안에서 동작하는가?
- BuildRequest가 들어왔을 때 서버가 죽지 않고 명확한 에러를 반환하는가?

---

## 3. 구현 내용

### 3.1 추가된 환경 변수

| 환경 변수 | 값 | 기본값 | 의미 |
|-----------|-----|--------|------|
| `NODEVAULT_RUNTIME_MODE` | `host` \| `incluster` | `host` | K8s 클라이언트 초기화 방식 |
| `NODEVAULT_BUILD_BACKEND` | `local-podbridge` \| `disabled` | `local-podbridge` | 이미지 빌드 백엔드 |

기존 배포(`NODEVAULT_RUNTIME_MODE` 미설정)는 아무런 변경 없이 동작한다.

### 3.2 변경된 파일

| 파일 | 변경 내용 |
|------|-----------|
| `pkg/build/builder.go` | `disabledBuilder` 추가 (즉시 `ErrBuildBackendDisabled` 반환) |
| `pkg/build/service.go` | `NewDisabledService()` 팩토리 추가 |
| `pkg/validate/service.go` | `NewInClusterService()` 추가 (`rest.InClusterConfig()` 사용) |
| `cmd/controlplane/main.go` | `runtimeConfig` 추가, 조건부 podbridge5 reexec, 기동 로그 |
| `deploy/spike/nodevault-incluster.yaml` | K8s Deployment + Service + RBAC + ConfigMap |
| `scripts/spike-nodevault-incluster-smoke.sh` | 기동 확인 스크립트 |
| `pkg/build/service_test.go` | disabled backend 단위 테스트 4개 추가 |

### 3.3 기동 로그 예시 (spike 모드)

```
time=... level=INFO msg="NodeVault starting"
  runtime_mode=incluster
  build_backend=disabled
  catalog_path=/data/catalog
  index_path=/data/index
  grpc_listen_address=:50051
  kube_config_mode=incluster_serviceaccount
```

---

## 4. 비목표 (이번 브랜치 범위 밖)

- Shipwright 설치 또는 연동
- podbridge5를 K8s Job으로 실행
- buildah rootless 문제 해결
- NodeSentinel 연동 완성
- NodePalette 독립 repo 분리
- systemd 배포 제거

---

## 5. 알려진 제약

**기존 host 모드 제약은 변경 없음.**  
`NODEVAULT_BUILD_BACKEND=local-podbridge` (기본) 상태에서 K8s Pod 안에서 실행하면 `podbridge5.NewStore()`가 실패한다 — overlay mount 불가. 이것은 expected failure이며 이번 spike 범위가 아니다.

---

## 6. 다음 단계 후보

이번 spike 결과에 따라 다음 중 하나를 선택한다.

### 옵션 A: K8sPodbridgeBackend

podbridge5 빌드를 K8s Job으로 실행한다.

```
[NodeVault Pod]
  BuildService.BuildAndRegister()
    → K8s Job (builder image: quay.io/buildah/stable)
      → podbridge5 in-job 실행
      → Harbor push
      → digest 반환
```

장점: NodeVault Core가 완전히 K8s 내부로 이동 가능  
단점: builder Job에 privileged 또는 fuse-overlayfs 설정 필요, 복잡도 증가

### 옵션 B: ShipwrightBackend

Shipwright Build/BuildRun CRD를 사용한다.

```
[NodeVault Pod]
  BuildService.BuildAndRegister()
    → Shipwright BuildRun 생성
    → Shipwright가 Tekton/buildah Step 실행
    → Harbor push
    → digest 반환
```

장점: 표준화된 K8s-native 빌드 프레임워크  
단점: Shipwright 설치 필요, 학습 비용

### 옵션 C: LocalPodbridgeBackend 유지 (host fallback)

NodeVault Core는 K8s 내부로 이동하되, L2 빌드 전담 사이드카 프로세스를 별도로 유지한다.

```
[seoy 호스트]
  build-agent (별도 프로세스, podbridge5 실행)
    ↑ gRPC/Unix socket
[K8s]
  NodeVault Core Pod
    BuildService → build-agent 호출 → image 빌드 + Harbor push
```

장점: 즉시 적용 가능, buildah rootless 제약 해소 불필요  
단점: 여전히 seoy 호스트 의존성 유지

---

## 7. 판단 기준

이번 spike가 성공하면 (Pod Running + Ping 응답 + disabled error 확인):
- 옵션 A 또는 B 검토를 위한 별도 spike 브랜치 생성
- 옵션 C는 단기 운영 편의로 고려

이번 spike가 실패하면 (Pod 기동 불가 등):
- 실패 원인을 이 문서에 기록
- 원인에 따라 대응 방향 결정
