# NodeVault Kubernetes 배포 가이드

버전: 3.0  
갱신: 2026-06-17

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
검증된 최소 capability(`CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `SETFCAP`, `SYS_CHROOT`)만 추가한다.

## 사전 조건

- NodeVault 이미지를 `harbor.lab.local/nodevault/controlplane:latest`에 push
- image pull과 Buildah push에 공통으로 사용하는 `nodevault-registry-auth` 생성
- ORAS referrer 호환용 username/password secret 생성

```bash
kubectl apply -f deploy/00-namespaces.yaml

kubectl create secret docker-registry nodevault-registry-auth \
  --docker-server=harbor.lab.local \
  --docker-username=<username> \
  --docker-password=<password> \
  -n nodevault-system

kubectl create secret generic nodevault-harbor-auth \
  --from-literal=username=<username> \
  --from-literal=password=<password> \
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

첫 전환 패치에서는 기존 lab 호환성을 위해 `vfs` driver와 `emptyDir`를 사용한다.

```text
/var/lib/nodevault/containers  Buildah graphroot
/run/nodevault/containers      Buildah runroot
/data                           catalog/index 임시 저장
```

이 구성은 실행 모델 검증용이다. 다음 단계에서 graphroot/cache와 durable state를 PVC 및
SQLite WAL로 전환해야 한다.

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
