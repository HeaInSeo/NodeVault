# NodeVault — infra-lab 클러스터 통합 테스트 가이드

## 배경 및 목적

NodeVault의 통합 테스트는 실제 Kubernetes 클러스터에서 실행됩니다.
기존에는 kind(Kubernetes in Docker)만을 공식 환경으로 지정했으나,
다음과 같은 이유로 [infra-lab](https://github.com/HeaInSeo/infra-lab) VM 클러스터를 추가 테스트 환경으로 채택합니다.

| 항목 | kind | infra-lab |
|------|------|-----------|
| 설치 위치 | 단일 호스트 프로세스 | Ubuntu 24.04 VM 3대 (1 master + 2 worker) |
| 네트워크 격리 | 호스트 네트워크 공유 | 독립 VM 네트워크 (backend 별 상이) |
| 노드 수 | 1 | 3 |
| 컨테이너 런타임 | containerd | containerd |
| 레지스트리 접근 | ClusterIP `10.96.0.1:5000` | Harbor (`harbor.10.113.24.96.nip.io`) |
| 멀티노드 스케줄링 | 불가 | 가능 |
| VM backend | — | `multipass` 또는 `libvirt` |

---

## 클러스터 정보

infra-lab은 OpenTofu + kubeadm 기반으로 두 가지 VM backend를 지원합니다.

| 항목 | 값 |
|------|----|
| 프로젝트 경로 | `/opt/go/src/github.com/HeaInSeo/infra-lab` |
| Kubeconfig | `infra-lab/kubeconfig` |
| 베이스라인 토폴로지 | 1 control-plane + 2 workers |
| K8s 버전 | kubeadm 기본 (Ubuntu 24.04 게스트) |
| Pod CIDR | `10.244.0.0/16` (Flannel 기본) |
| Service CIDR | `10.96.0.0/12` |
| 내부 레지스트리 | Harbor — `harbor.10.113.24.96.nip.io` |
| Backend 선택 | `BACKEND=multipass` (기본) 또는 `BACKEND=libvirt` |

VM IP 대역은 backend에 따라 달라지므로 (`multipass`는 multipass 네트워크, `libvirt`는 default 네트워크 DHCP) 실제 주소는 클러스터 기동 후 `kubectl get nodes -o wide`로 확인합니다.

---

## 아키텍처: 로컬 바이너리 + 원격 클러스터

NodeVault는 **클러스터 내부에 배포되지 않고, 호스트에서 바이너리로 실행**됩니다.
kubeconfig를 통해 원격 클러스터 K8s API에 접근합니다.

```
[호스트]                         [infra-lab VM 클러스터]
  NodeVault binary  ──gRPC─→  (localhost:50051 — 클라이언트가 접속)
  (bin/nodevault)
       │
       ├── K8s API 접근 ──────→  control-plane:6443
       │   (KUBECONFIG 사용)
       │
       └── 레지스트리 접근 ────→  harbor.10.113.24.96.nip.io
                                  │
                                  └── podbridge5(buildah)가 이미지 push
```

이 방식의 장점:
- NodeVault 배포 이미지 없이 바이너리만으로 테스트 가능
- 클러스터 재시작 없이 바이너리 교체 가능
- 기존 통합 테스트(`localhost:50051`)를 수정 없이 재사용

---

## 사전 조건

```bash
# 1. infra-lab 클러스터 기동 (최초 1회 또는 down 상태일 때)
cd /opt/go/src/github.com/HeaInSeo/infra-lab
./scripts/k8s-tool.sh up                  # multipass backend (기본)
# 또는
BACKEND=libvirt ./scripts/k8s-tool.sh up  # libvirt backend

# 2. 클러스터 상태 확인
KUBECONFIG=/opt/go/src/github.com/HeaInSeo/infra-lab/kubeconfig \
  kubectl get nodes

# 예상 출력 (예시):
# NAME           STATUS   ROLES           AGE
# lab-master-0   Ready    control-plane   ...
# lab-worker-0   Ready    <none>          ...
# lab-worker-1   Ready    <none>          ...

# 3. Harbor 가용성 확인 (suspend 상태인 경우 재개 필요)
curl -sk https://harbor.10.113.24.96.nip.io/api/v2.0/health | python3 -m json.tool
```

원격 호스트(예: seoy)에서 클러스터를 운영하는 경우 `HOST_PROFILE=hosts/remote-lab.env ./scripts/k8s-tool.sh up`을 사용합니다. 자세한 host/backend/transport 분리 모델은 `infra-lab/docs/ARCHITECTURE.ko.md` 참조.

---

## 실행 방법

```bash
cd /opt/go/src/github.com/HeaInSeo/NodeVault

# 1. K8s 지원 리소스 배포 (최초 1회 또는 클러스터 재기동 후)
make deploy-infralab

# 2. 통합 테스트 실행
make test-integration-infralab

# 또는 한 번에
make deploy-infralab test-integration-infralab

# 3. 정리 (선택적)
make undeploy-infralab
```

`deploy-infralab`이 적용하는 리소스:

| 파일 | 내용 |
|------|------|
| `deploy/00-namespaces.yaml` | `nodevault-system`, `nodevault-builds`, `nodevault-smoke` |
| `deploy/02-rbac.yaml` | ServiceAccount + ClusterRole + ClusterRoleBinding (L3/L4 Job 제출 권한) |

`deploy/03-nodevault.yaml` (Deployment) 와 `deploy/04-grpcroute.yaml` (Cilium GRPCRoute)은 미래 in-cluster 전환을 위한 선행 정의로, 현재는 미적용입니다.

---

## 환경 변수

`test-integration-infralab` Makefile 타겟이 자동으로 설정하는 환경 변수:

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `KUBECONFIG` | `../infra-lab/kubeconfig` | 클러스터 인증 |
| `NODEVAULT_REGISTRY_ADDR` | `harbor.10.113.24.96.nip.io` | podbridge5 push 대상 레지스트리 |

Override 방법:
```bash
INFRALAB_REGISTRY=harbor.example.com:5000 make test-integration-infralab
```

---

## 레지스트리 (Harbor) 메모

- Harbor는 infra-lab 측에서 운영합니다 (`infra-lab/k8s/harbor/01-httproute.yaml` 참조).
- 호스트 측 podbridge5(buildah) 인증/TLS 설정은 podbridge5 vendored 구성과 호스트 `/etc/containers/registries.conf`에 의존합니다.
- Harbor가 suspend된 상태라면 infra-lab의 `scripts/host/harbor-resume.sh` (또는 호스트 운영 절차)로 재개합니다.

---

## 알려진 제약 사항 및 리스크

| ID | 내용 | 영향 |
|----|------|------|
| R-IL-01 | infra-lab kubeconfig는 `up` 시점에 생성/갱신됨 | 클러스터 재기동 시 기존 kubeconfig 무효 — `up` 재실행 필요 |
| R-IL-02 | NodeVault가 호스트 바이너리로 실행 — gRPC 서버 충돌 방지 필요 | 이전 프로세스가 남아있으면 50051 포트 충돌 |
| R-IL-03 | libvirt backend는 `default` 네트워크 DHCP 사용 — IP 비고정 | guest interface 이름/IP를 코드에서 가정하지 말 것 |
| R-IL-04 | podbridge5 첫 실행 시 베이스 이미지 pull 시간 | 클러스터 노드와 무관 (호스트에서 buildah가 in-process 실행) |
| R-IL-05 | in-cluster ServiceAccount 인증 미구현 | 현재 kubeconfig 직접 사용. 프로덕션 in-cluster 전환 시 추가 필요 |

---

## 기존 kind 통합 테스트와의 차이

| 항목 | kind (`test-integration`) | infra-lab (`test-integration-infralab`) |
|------|---------------------------|-----------------------------------------|
| 레지스트리 주소 | `10.96.0.1:5000` (ClusterIP) | `harbor.10.113.24.96.nip.io` (Harbor) |
| NodeVault 실행 위치 | 별도 프로세스 또는 클러스터 내 | 호스트 바이너리 |
| 멀티노드 검증 | 불가 | 가능 |
| 네트워크 현실성 | 낮음 (loopback) | 높음 (실제 VM 네트워크) |
| VM backend | — | multipass / libvirt |
