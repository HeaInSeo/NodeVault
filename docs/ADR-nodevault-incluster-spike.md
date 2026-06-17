> **상태: SUPERSEDED / REJECTED**  
> 이 문서의 `k8s-job` builder 방식은 2026-06-17 폐기되었다. 현재 결정은 NodeVault Pod 내부에서 podbridge5/Buildah Go API를 호출하는 `in-pod-buildah` 단일 경로다. 이 문서는 실험 기록으로만 보존한다.

# ADR: NodeVault In-Cluster Spike

날짜: 2026-06-12  
브랜치: `spike/nodevault-incluster-controlplane`  
상태: **Option A (K8sPodbridgeBackend) 구현 중**

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
| `NODEVAULT_BUILD_BACKEND` | `local-podbridge` \| `disabled` \| `k8s-job` | `local-podbridge` | 이미지 빌드 백엔드 |
| `NODEVAULT_BUILDER_IMAGE` | e.g. `quay.io/buildah/stable:v1.37.1` | 위 기본값 | k8s-job 백엔드의 builder 컨테이너 이미지 |
| `HARBOR_USER` | e.g. `admin` | — | k8s-job builder Job에 전달할 Harbor 사용자명 |
| `HARBOR_PASS` | e.g. `<harbor-password>` | — | k8s-job builder Job에 전달할 Harbor 비밀번호 |

기존 배포(`NODEVAULT_RUNTIME_MODE` 미설정)는 아무런 변경 없이 동작한다.

### 3.2 변경된 파일

| 파일 | 변경 내용 |
|------|-----------|
| `pkg/build/builder.go` | `disabledBuilder` 추가 (즉시 `ErrBuildBackendDisabled` 반환) |
| `pkg/build/k8sjob_builder.go` | `K8sJobBuilder` 추가 — Option A 구현 (K8s Job + buildah) |
| `pkg/build/service.go` | `NewDisabledService()`, `NewK8sJobService()` 팩토리 추가 |
| `pkg/validate/service.go` | `NewInClusterService()` 추가 (`rest.InClusterConfig()` 사용) |
| `cmd/controlplane/main.go` | `runtimeConfig` 추가, `k8s-job` 백엔드 케이스, reexec 가드 |
| `deploy/spike/nodevault-incluster.yaml` | Phase 2로 업데이트: k8s-job 백엔드, Harbor 자격증명 Secret, `nodevault-builds` 네임스페이스, ConfigMap RBAC 추가 |
| `scripts/spike-nodevault-incluster-smoke.sh` | Step 5: 실제 빌드 Job 검증 추가 |
| `pkg/build/service_test.go` | disabled backend 단위 테스트 4개 추가 |

### 3.3 기동 로그 예시 (Option A k8s-job 모드)

```
time=... level=INFO msg="NodeVault starting"
  runtime_mode=incluster
  build_backend=k8s-job
  catalog_path=/data/catalog
  index_path=/data/index
  grpc_listen_address=:50051
  kube_config_mode=incluster_serviceaccount
time=... level=INFO msg="BuildService registered with k8s-job backend (Option A spike)"
```

### 3.4 Option A K8sJobBuilder 동작 흐름

```
BuildAndRegister(req)
  → K8sJobBuilder.Build(ctx, dockerfileContent, outputRef)
      1. ConfigMap 생성 (nvbuild-df-{suffix}): Dockerfile 내용 저장
      2. Job 생성 (nvbuild-{suffix}): quay.io/buildah/stable:v1.37.1, privileged=true
         - /workspace/ 에 ConfigMap 마운트 (Dockerfile)
         - /var/lib/containers 에 emptyDir 마운트 (container storage)
         - 환경변수: DESTINATION, HARBOR_USER, HARBOR_PASS
         - 빌드 스크립트:
             buildah bud --tls-verify=false -t $DESTINATION /workspace/
             buildah push --tls-verify=false --creds ... --digestfile=/tmp/digest.txt ...
             printf 'BUILD_DIGEST=%s\n' "$(cat /tmp/digest.txt)"
      3. Job 완료 Watch (최대 15분)
      4. Pod 로그에서 BUILD_DIGEST=sha256:... 파싱
      5. ConfigMap + Job cleanup (defer)
  → L3 dry-run → L4 smoke run → 등록
```

**제약 (spike only, 운영 금지):**
- `privileged: true` — fuse-overlayfs 사용을 위한 K8s 특권 컨테이너
- `--tls-verify=false` — Harbor 자체서명 TLS
- Harbor 자격증명 env var 직접 전달 (운영은 Vault/ESO 사용)

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
