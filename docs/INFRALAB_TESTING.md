# NodeVault — infra-lab 통합 테스트 가이드

갱신: 2026-06-17

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
   `SubmitToolBuild`는 resolved `raw_spec`의 build 요청을 L2 background build로 실행하며,
   Watch/Cancel 경로의 실제 클러스터 검증은 아직 남아 있다.
3. podbridge5 in-Pod 경로는 nan 자동 주입을 아직 수행하지 않는다.
4. registry 인증은 Buildah용 docker auth secret과 ORAS용 username/password secret이 분리되어 있다.
5. `TestBuildCancel_CleansUpSubprocess`는 NodeVault 패키지 경계 내(simulated subprocess) 추가
   완료(issue [#7](https://github.com/HeaInSeo/NodeVault/issues/7)). 실제 podbridge5/Buildah
   subprocess의 cancel-시 kill/wait 동작은 podbridge5 저장소 책임 범위.
