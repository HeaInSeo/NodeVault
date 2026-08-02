# NodeVault Kubernetes 배포 가이드

버전: 3.1  
갱신: 2026-07-15

이 문서가 설명하는 K8s 배포가 **상업적/이식 가능한 기준 배포 경로**다.
`scripts/deploy-seoy.sh`(seoy 호스트에 systemd 서비스로 직접 띄우는 방식)는
이 경로와 무관한 seoy 랩 전용 dev 편의 도구이며, 컨테이너 이미지 버전관리·
롤백·이중화가 없다 — 상업적 배포 기준으로 참조하지 말 것.

## 배포 모델

NodeVault는 Kubernetes 데이터플레인 애플리케이션으로 장기 실행 Pod에 배포한다.

```text
NodeVault Pod
  └── NodeVault process
      └── podbridge5 wrapper
          └── Buildah Go API
              ├── containers/storage
              ├── image build
              └── Harbor push
```

NodeVault는 이미지 빌드를 위해 Kubernetes Job이나 별도 worker Pod를 생성하지 않는다.
`NODEVAULT_BUILD_BACKEND=k8s-job` 경로는 제거되었다.

L3/L4 검증은 아직 `ValidateService`가 별도 Kubernetes Job으로 실행하므로 해당 권한은
`deploy/02-rbac.yaml`에 제한적으로 남아 있다. 이것은 이미지 빌드 경로와 별개다.

## 보안 기준

대상 클러스터에서 User Namespace 실행을 검증했으므로 기본 Deployment는 다음을 사용한다.

```yaml
hostUsers: false
securityContext:
  runAsUser: 0
  seccompProfile:
    type: RuntimeDefault
```

컨테이너 root는 host의 비특권 UID에 매핑된다. 컨테이너는 privileged 권한을 사용하지 않으며,
overlay storage driver와 podbridge5 chroot-isolation 빌드 경로에 필요한 capability set을
명시적으로 추가한다. 자세한 검증 기록은 `docs/OVERLAY_USERNS_INVESTIGATION.md`를 참조한다.

## 사전 조건

- NodeVault 이미지를 `harbor.lab.local/nodevault/controlplane:latest`에 push
- infra-lab Harbor CA 배포 완료 (`~/.config/infra-lab/install-guide.md`의 Harbor 설치 절차)
- image pull, Buildah push, ORAS referrer push에 공통으로 사용하는 `nodevault-registry-auth` 생성 (auth.json 형식, ORAS도 이 파일을 파싱)
- in-Pod Buildah가 Harbor CA를 신뢰하도록 `nodevault-harbor-ca` 생성

```bash
kubectl apply -f deploy/00-namespaces.yaml

kubectl create secret docker-registry nodevault-registry-auth \
  --docker-server=harbor.lab.local \
  --docker-username=<username> \
  --docker-password=<password> \
  -n nodevault-system

kubectl create secret generic nodevault-harbor-ca \
  --from-file=ca.crt="$HOME/.config/infra-lab/certs/harbor-ca.crt" \
  -n nodevault-system
```

## 배포

```bash
make vendor
make push-image
make deploy-infralab
```

`make deploy-infralab`은 다음을 적용한다.

```text
deploy/00-namespaces.yaml
  - nodevault-system
  - nodevault-smoke

deploy/02-rbac.yaml
  - ValidateService 전용 ServiceAccount/RBAC

deploy/03-nodevault.yaml
  - hostUsers:false NodeVault Deployment
  - in-pod-buildah backend
  - containers/storage.conf
  - graphroot/runroot/data volumes
```

## 저장소 구성

NodeVault의 현재 배포 storage driver는 `overlay`다. `vfs`와 `fuse-overlayfs`는
이 환경에서 실패 경로로 확인되었으므로, 명시적 재검증 없이 fallback으로 전환하지 않는다.
현재 Deployment는 `local-path` 기본 StorageClass에 RWO PVC를 요청한다. NodeVault는
단일 replica여야 하며, replica 확장은 shared build-state/scheduling 설계가 정해진 뒤에만
진행한다.

단일 Pod이 RWO PVC 4개를 점유하므로 Deployment는 `strategy: Recreate`를 사용한다.
기본 RollingUpdate는 새 Pod을 먼저 띄우려다 옛 Pod이 물고 있는 볼륨을 attach하지 못해
교착하기 때문이다. Recreate는 옛 Pod을 먼저 내린 뒤 새 Pod을 띄우므로, 이미지 태그를
바꿔 rollout을 거는 동안 **NodeVault 서비스가 잠시 중단(다운타임)된다.** 무중단 롤아웃은
RWM 스토리지와 shared build-state 설계가 선행되어야 하며 현재 범위가 아니다.

```text
/var/lib/nodevault/containers  nodevault-build-graphroot PVC (20Gi)
/run/nodevault/containers      nodevault-build-runroot PVC (5Gi)
/data                           nodevault-data PVC (5Gi, catalog/index/SQLite WAL)
```

PVC 크기는 infra-lab 기준 초기값이다. cache 계층과 capacity admission은 Phase 4에서
별도로 추가한다.

## 확인

```bash
kubectl -n nodevault-system get pod
kubectl -n nodevault-system logs deployment/nodevault-controlplane
```

기동 로그에는 다음 값이 보여야 한다.

```text
runtime_mode=incluster
build_backend=in-pod-buildah
```

다음 리소스가 생성되면 안 된다.

```text
nodevault-builds namespace
builder Job
Buildah worker Pod
```

L3/L4 검증 요청이 발생한 경우 `nodevault-smoke`의 검증 Job은 생성될 수 있다.
