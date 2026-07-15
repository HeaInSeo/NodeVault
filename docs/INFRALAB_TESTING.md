# NodeVault — infra-lab 통합 테스트 가이드

갱신: 2026-07-15

이 문서가 설명하는 K8s 배포(`deploy/03-nodevault.yaml`, 이 문서)가 **상업적/이식
가능한 기준 배포 경로**다. seoy 호스트에 직접 systemd로 띄우는
`scripts/deploy-seoy.sh` 경로는 별개의 dev 전용 편의 도구이며, 이 문서가
설명하는 경로와 별도로 관리된다 — 둘을 혼동하지 말 것.

## 목적

infra-lab의 실제 VM Kubernetes 클러스터에서 다음 경로를 검증한다.

```text
NodeKit/test client
  → port-forward
  → NodeVault Service
  → NodeVault Pod
  → podbridge5 wrapper
  → Buildah Go API
  → Harbor push
```

이미지 빌드를 위해 별도 Kubernetes builder Job이나 worker Pod를 만들지 않는다.
L3/L4 검증 Job은 `nodevault-smoke` namespace에서 별도로 실행될 수 있다.

## 사전 조건

- infra-lab 클러스터 실행
- Harbor 실행
- Harbor CA 배포 완료 (`~/.config/infra-lab/install-guide.md`의 Harbor 설치 절차)
- NodeVault controlplane 이미지 push
- User Namespace(`hostUsers:false`) 지원 노드
- 다음 Secret 생성

```bash
kubectl apply -f deploy/00-namespaces.yaml

kubectl create secret docker-registry nodevault-registry-auth \
  --docker-server=harbor.lab.local \
  --docker-username=<username> \
  --docker-password=<password> \
  -n nodevault-system

# in-Pod Buildah가 Harbor의 자체서명 CA를 신뢰하도록 마운트하는 Secret.
# 인증서(공개키)일 뿐 비밀정보가 아니다 — Harbor를 재설치하면 CA도 새로
# 생성되므로 이 Secret도 다시 만들어야 한다.
kubectl create secret generic nodevault-harbor-ca \
  --from-file=ca.crt="$HOME/.config/infra-lab/certs/harbor-ca.crt" \
  -n nodevault-system
```

## 실행

```bash
cd /opt/go/src/github.com/HeaInSeo/NodeVault

make vendor
make push-image
make deploy-infralab
make test-integration-infralab
```

`INFRALAB_KUBECONFIG`는 `../infra-lab/state/*/kubeconfig`를 자동 탐색한다
(Makefile). infra-lab 환경 이름(예: `seoy-libvirt-cilium`)은 환경이 재생성될
때마다 바뀔 수 있으므로 특정 이름을 이 문서나 Makefile에 고정하지 않는다 —
여러 환경이 동시에 존재하면 `INFRALAB_KUBECONFIG=../infra-lab/state/<env-name>/kubeconfig`를
명시적으로 지정할 것. 예전에는 `../infra-lab/kubeconfig` 단일 파일이었으나
현재 infra-lab은 이 경로를 쓰지 않는다.

원격 host에서 `localhost:50051`이 이미 사용 중이면 다른 local port를 지정한다.

```bash
make test-integration-infralab INTEGRATION_GRPC_PORT=50052
```

`make deploy-infralab`은 다음을 적용한다.

| 파일 | 내용 |
|---|---|
| `deploy/00-namespaces.yaml` | `nodevault-system`, `nodevault-smoke` |
| `deploy/02-rbac.yaml` | L3/L4 ValidateService 전용 권한 |
| `deploy/03-nodevault.yaml` | `hostUsers:false`, `in-pod-buildah` NodeVault Deployment |

`make test-integration-infralab`은 Service를 `localhost:50051`로 임시
port-forward하고 `pkg/build` integration test를 실행한다. 로컬 NodeVault 바이너리는
실행하지 않는다.

## 수동 gRPC 연결 확인 (grpcurl)

**공개 gRPC 엔드포인트는 없다** — `nodevault-controlplane` Service는 ClusterIP라
`kubectl port-forward` 없이는 클러스터 밖에서 절대 닿지 않는다. seoy 호스트의
Tailscale IP(`100.123.80.48:50051`)에 직접 연결되는 것은 이 K8s 배포가 아니라
`scripts/deploy-seoy.sh`가 별도로 띄우는 seoy 전용 dev systemd 서비스다 — 완전히
다른 배포이며 혼동하지 말 것 (위 경고 참조).

```bash
export KUBECONFIG=/opt/go/src/github.com/HeaInSeo/infra-lab/state/<env-name>/kubeconfig
kubectl -n nodevault-system port-forward service/nodevault-controlplane 50052:50051 &

grpcurl -plaintext -import-path protos -proto nodevault/v1/nodevault.proto \
  127.0.0.1:50052 nodevault.v1.PingService/Ping

# ResolveToolSpec → SubmitToolBuild → WatchToolBuild 전체 경로 확인 예시는
# 2026-07-15 smoke test 기록 참조 — raw_spec에 image_uri(＠sha256 pinned)와
# dockerfile_content를 함께 넣어야 한다(ResolveToolSpec은 raw_spec의
# base_image/base_image_uri/image_uri 키에서만 base image digest를 읽는다).
```

## 확인 항목

```bash
kubectl -n nodevault-system get deployment,pod,service
kubectl -n nodevault-system logs deployment/nodevault-controlplane
```

기동 로그:

```text
runtime_mode=incluster
build_backend=in-pod-buildah
```

빌드 중 확인:

```bash
kubectl get jobs,pods -A
```

다음은 나타나지 않아야 한다.

```text
nodevault-builds namespace
nvbuild-* Job
nodevault-builder Pod
```

L3/L4가 실행된 경우 `nodevault-smoke`의 검증 Job은 정상이다.

## 현재 제한

1. 현재 전환 패치는 storage driver로 `overlay`를 사용한다. `vfs`와 `fuse-overlayfs`는
   이 환경에서 실패 경로로 확인되었으므로, 명시적 재검증 없이 fallback으로 전환하지 않는다.
   graphroot/runroot와 `/data`는 PVC로 전환되어 있다(`deploy/03-nodevault.yaml`). 2026-06-23
   seoy에서 Pod 재시작 전후 graphroot의 `overlay-layers/layers.json`과 레이어 디렉터리가
   byte-identical하게 유지됨을 직접 확인했고, 재시작 직후 Pod에 대해
   `TestBuildAndRegister_SimpleDockerfile`이 정상적으로 빌드+push+L3+L4+register까지
   완료됨을 확인했다(issue [#7](https://github.com/HeaInSeo/NodeVault/issues/7)).
2. legacy `BuildRequest`/`BuildAndRegister`는 L2→L3→L4 결합 흐름을 유지한다. 신규
   `SubmitToolBuild`는 resolved `raw_spec`의 build 요청을 L2 background build로 실행한다.
   `ResolveToolSpec`/`SubmitToolBuild`/`WatchToolBuild`/`CancelToolBuild` 전 경로를
   2026-07-15 seoy 클러스터에서 실 gRPC 호출로 검증 완료 — `ResolveToolSpec` →
   `SubmitToolBuild` → `WatchToolBuild`가 terminal event(`image_digest`,
   `spec_referrer_digest`, `integrity_health=Healthy` 포함)까지 정상 수신되고
   `ListTools`로 index record도 확인됨. `CancelToolBuild`는 이미 terminal인 빌드에
   대해 `FailedPrecondition`을 정확히 반환함을 확인(라우팅 검증) — 진행 중인 빌드를
   실제로 중도 취소하는 시나리오는 아직 별도 검증 필요.
3. podbridge5 in-Pod 경로는 nan 자동 주입을 아직 수행하지 않는다.
4. registry 인증은 Buildah와 ORAS 양쪽 모두 동일한 `auth.json`(`REGISTRY_AUTH_FILE`,
   기본 `/run/containers/0/auth.json`)을 파싱한다 — `pkg/registryconfig`(Sprint 5,
   2026-07-12)로 통합됨. 예전에는 ORAS용 별도 username/password secret이 있었으나
   issue #12(2026-07-02)로 제거됐다.
5. `TestBuildCancel_CleansUpSubprocess`는 NodeVault 패키지 경계 내(simulated subprocess) 추가
   완료(issue [#7](https://github.com/HeaInSeo/NodeVault/issues/7)). 실제 podbridge5/Buildah
   subprocess의 cancel-시 kill/wait 동작은 podbridge5 저장소 책임 범위.
