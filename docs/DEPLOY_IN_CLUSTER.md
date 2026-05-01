# NodeVault 배포 가이드

버전: 2.0  
갱신: 2026-04-19

---

## 실행 환경

**NodeVault는 seoy 호스트 바이너리로 실행한다 (K8s Pod 아님).**

podbridge5(buildah)의 rootless 제약으로 인해 K8s Pod 안에서 overlay 파일시스템 마운트가
불가능하다. 이미지 빌드(L2)는 NodeVault 바이너리가 직접 in-process로 처리하며,
L3/L4 dry-run / smoke run만 kubeconfig를 통해 K8s Job으로 제출한다.

| 구분 | 실행 위치 |
|------|-----------|
| NodeVault 바이너리 (gRPC :50051, REST :8080) | seoy 호스트 직접 실행 |
| L2 이미지 빌드 (podbridge5 in-process) | seoy 호스트 (NodeVault 프로세스 내) |
| L3 dry-run / L4 smoke run | K8s Job (nodevault-smoke namespace) |
| Harbor | harbor.10.113.24.96.nip.io (Cilium LB VIP) |

---

## 사전 조건

- seoy 장비 (100.123.80.48) 접근 가능
- infra-lab 클러스터 실행 중 (multipass 또는 libvirt backend)
- Harbor 접근 가능: `http://harbor.10.113.24.96.nip.io`
- `infra-lab/kubeconfig` 존재

---

## 배포 절차

### 1. K8s 지원 리소스 배포 (최초 1회 또는 클러스터 재설치 후)

L3/L4 Job 실행에 필요한 네임스페이스와 RBAC만 배포한다.

```bash
make deploy-infralab
# 적용 대상:
#   deploy/00-namespaces.yaml — nodevault-system, nodevault-smoke 네임스페이스
#   deploy/02-rbac.yaml       — ServiceAccount + ClusterRole (Job 제출 권한)
```

### 2. NodeVault 바이너리 빌드

```bash
make vendor   # go mod vendor (podbridge5 등 의존성)
make build    # bin/nodevault 생성
```

### 3. NodeVault 실행 (seoy 호스트)

```bash
KUBECONFIG=/path/to/infra-lab/kubeconfig \
NODEVAULT_REGISTRY_ADDR=harbor.10.113.24.96.nip.io \
./bin/nodevault
```

gRPC는 `:50051`, NodePalette REST는 `:8080`에서 수신 대기한다.

### 4. Harbor 인증 설정

podbridge5가 Harbor에 push하려면 seoy 호스트에서 먼저 로그인해야 한다.

```bash
podman login harbor.10.113.24.96.nip.io
# Username: admin
# Password: <harbor-admin-password>
```

---

## K8s 리소스 파일 현황

| 파일 | 상태 | 설명 |
|------|------|------|
| `deploy/00-namespaces.yaml` | **사용 중** | nodevault-system, nodevault-smoke 네임스페이스 |
| `deploy/02-rbac.yaml` | **사용 중** | ServiceAccount + ClusterRole (L3/L4 Job 권한) |
| `deploy/03-nodevault.yaml` | 미사용 (미래용) | NodeVault K8s Deployment 템플릿 (in-cluster 전환 시 사용) |
| `deploy/04-grpcroute.yaml` | 미사용 (미래용) | Cilium GRPCRoute (in-cluster 전환 시 사용) |

> `03-nodevault.yaml`과 `04-grpcroute.yaml`은 현재 적용하지 않는다.
> NodeVault가 K8s Pod로 이동할 경우를 대비해 보존.

---

## NodeKit 연결 설정

NodeKit은 NodeVault gRPC에 seoy 호스트 IP로 직접 연결한다.

```
NodeVault gRPC addr: 100.123.80.48:50051
NodePalette REST:    http://100.123.80.48:8080
```

---

## seoy → Harbor 라우트

seoy 호스트에서 Harbor Cilium LB VIP에 접근하려면 라우트가 필요하다.

```bash
ip route add 10.113.24.96/32 via 10.113.24.254
```

multipassd 시작 시 자동 추가됨 (`discard-ns.conf` ExecStartPost).
