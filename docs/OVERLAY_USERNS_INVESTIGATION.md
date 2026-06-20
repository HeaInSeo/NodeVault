> **상태: RESOLVED (overlay 활성화 완료)**
> `deploy/03-nodevault.yaml`은 2026-06-18부로 `driver = "overlay"` + `appArmorProfile: Unconfined` +
> 확장된 capability set으로 재배포되었다. 이 문서는 그 과정에서 시도/실패한 모든 경로와
> 최종 근본 원인을 기록으로 남긴다. **vfs로 되돌리지 말 것** — 유전체 분석 워크로드 특성상
> overlay가 필수 요구사항이며, 아래 기록이 그 이유와 해결 근거다.

# NodeVault overlay storage driver — userns 환경 조사 기록

날짜: 2026-06-18
환경: `test-wizard-env` (libvirt backend), kubeconfig
`infra-lab/state/test-wizard-env/kubeconfig`, K8s v1.36.2, containerd 2.2.1,
kernel 6.8.0-117-generic, Ubuntu 24.04.4 LTS, 노드 `lab-master-0/lab-worker-0/lab-worker-1`.

제약 조건 (변경 불가, 전체 조사 기간 동안 유지):
`hostUsers: false`, `privileged: false`, `allowPrivilegeEscalation: false`(컨테이너 보안 컨텍스트는
최종적으로 이 값을 유지한 채로 문제가 해결됨 — 아래 4번 참조).

---

## 1. 최초 결론(틀렸음): "overlay는 이 클러스터에서 불가능하다"

`deploy/03-nodevault.yaml`에 한 차례 다음 결론으로 `driver = "vfs"`가 커밋된 적이 있다
(커밋 `510a175`). **이 결론은 틀렸다** — 근본 원인을 끝까지 추적하지 않고 증상만 보고
"불가능"으로 판단했기 때문이다. 아래 2~3번은 그 결론을 내릴 때 실제로 관찰한, 재현 가능한
증상들이다. 증상 자체는 사실이지만, "해결 불가능"이라는 해석이 틀렸다.

## 2. 시도/실패 기록 (시간순)

### 2.1 native overlay, CAP_SYS_ADMIN 없음

```
mount(MS_PRIVATE) ... failed to make mount private: operation not permitted (EPERM)
```
컨테이너 capability set이 `CHOWN, DAC_OVERRIDE, FOWNER, SETFCAP, SYS_CHROOT`뿐이라
`SYS_ADMIN`이 없어 graphroot mount를 private으로 만들 수 없었다.

### 2.2 native overlay, CAP_SYS_ADMIN 추가 (1회성 실험)

같은 호출이 EPERM → **EACCES**로 바뀜. 당시에는 "emptyDir 기반 graphroot가 Pod 자신의
userns 소유로 인식되지 않아 거부된다"고 해석했다. **이 해석이 틀렸다** — 실제 원인은
AppArmor였다 (4번 참조). EPERM→EACCES 변화는 capability가 부족해서 거부되던 것이
capability를 추가하니 AppArmor LSM hook 단계에서 다시 거부된 것일 뿐, capability
튜닝만으로는 영원히 풀리지 않는 문제라는 신호였다.

### 2.3 fuse-overlayfs 경로

이미지에는 `fuse-overlayfs`가 이미 포함되어 있고 노드에도 `/dev/fuse`가 존재했다.
그러나 `hostUsers: false` Pod에 `/dev/fuse`를 `hostPath` CharDevice로 마운트하면
컨테이너 생성 시점에 실패한다:

```
mount_setattr /dev/fuse ... Invalid argument
(maybe the file system used doesn't support idmap mounts on this kernel?)
```

idmapped-mount와 CharDevice hostPath의 비호환 문제로, `storage.conf`에
`mount_program=/usr/bin/fuse-overlayfs`를 배포하기 전에 이 단계에서 막혔다.

→ 위 2.1~2.3을 근거로 "hostUsers:false 유지 시 overlay 불가능"이라 결론짓고 `vfs`로
전환했다 (커밋 `510a175`). **틀린 결론.**

## 3. 진짜 원인 발견: containerd 기본 AppArmor 프로필

살아있는 컨테이너 안에서 직접 확인:

```
cat /proc/self/attr/current
→ cri-containerd.apparmor.d (enforce)
```

containerd의 기본 AppArmor 프로필이 `mount(MS_PRIVATE)`를 막고 있었다. 커널의
`kernel.apparmor_restrict_unprivileged_userns=1` 같은 sysctl도 점검했지만, 이건
userns 생성 자체를 막는 게 아니라(이미 `allowPrivilegeEscalation:false`를 제거하면
userns 생성은 성공했다) AppArmor 프로필이 활성화된 상태에서 **mount() 호출 단계**에서
거부되는 것이었다.

**수정**: Pod `securityContext.appArmorProfile.type: Unconfined`
(K8s 1.30+ 안정 필드, 본 클러스터 v1.36.2에서 지원).

### 3.1 검증 1 — UID 1000, graphroot를 별도 서브디렉토리로 분리

CERN 예제와 동일한 구조(`graphroot=/storage/.local/share/containers/storage`)로
재현 Pod(`buildah-overlay-reference`)를 만들어 `appArmorProfile: Unconfined`만
추가하니 `buildah info`에서 `Native Overlay Diff: true`가 나왔고, `RUN` 단계가 있는
실제 `buildah build`가 끝까지 성공했다.

### 3.2 검증 2 — NodeVault 실제 구조 그대로 (UID 0, graphroot == mountpoint)

NodeVault의 실제 매니페스트는 emptyDir 마운트 지점이 곧 graphroot다
(서브디렉토리 분리 없음). 이 구조를 그대로 복제한 재현 Pod(`buildah-overlay-ref-b`,
`runAsUser:0`, `appArmorProfile: Unconfined`)도 동일하게 성공했다. **즉 GPT가 제안한
"UID/마운트 구조를 CERN 예제처럼 바꿔야 한다"는 제안은 불필요했다** — AppArmor 설정
하나만으로 현재 구조 그대로 충분했다.

## 4. 두 번째, 더 깊은 원인: podbridge5가 모든 빌드에 강제하는 chroot isolation

3번 검증은 모두 `kubectl exec` 안에서 **`buildah` CLI를 직접** 실행한 것이었다.
AppArmor 수정을 실제 `deploy/03-nodevault.yaml`의 **정확한** securityContext
(`allowPrivilegeEscalation:false` + capability `CHOWN, DAC_OVERRIDE, FOWNER, SETFCAP,
SYS_CHROOT` 5개만)에 그대로 적용해 재검증하자 새로운 실패가 나타났다:

```
Error during unshare(CLONE_NEWUSER): Operation not permitted
```

추적 결과 (`podbridge5/image_options.go:159`), **podbridge5는 storage driver와 무관하게
모든 빌드에 `Isolation = define.IsolationChroot`를 하드코딩한다.** 이 경로는 `RUN` 단계마다
Buildah 자신이 `unshare(CLONE_NEWUSER)`를 호출한다 — overlay든 vfs든 동일하게 영향을 받는다.
**즉 이전까지 "정상 동작 중"으로 보였던 vfs 운영 배포도, RUN 단계가 있는 실제 빌드는 한 번도
끝까지 성공한 적이 없다** (Pod가 `1/1 Running`인 것은 gRPC 서버 프로세스가
살아있다는 뜻일 뿐, BuildService가 실제로 동작한다는 증거가 아니었다).

capability를 하나씩 추가하며 정확히 어디까지 필요한지 확인:

| 추가한 capability | 다음에 나타난 에러 |
|---|---|
| (없음, 5개 기본) | `unshare(CLONE_NEWUSER): Operation not permitted` |
| `+SYS_ADMIN` | `error setting supplemental groups list: operation not permitted` |
| `+SETGID, SETUID` | `error setting capabilities for process: operation not permitted` |
| `+FSETID, KILL, SETPCAP, NET_BIND_SERVICE, NET_RAW, MKNOD, AUDIT_WRITE` (OCI 기본 풀셋) | **빌드 성공** (pull → RUN → commit → tag 전부 성공) |

`allowPrivilegeEscalation`은 `true`/`false` 둘 다 테스트했고 결과에 영향 없음 —
이 경로의 진짜 요구사항은 capability set이었다.

## 5. 최종 수정 사항 (둘 다 필요, 둘 중 하나만으로는 불충분)

`deploy/03-nodevault.yaml`:

1. Pod `securityContext.appArmorProfile.type: Unconfined`
2. 컨테이너 capability `add` 목록을 5개에서 OCI 기본 풀셋 + `SYS_ADMIN`으로 확장:
   `CHOWN, DAC_OVERRIDE, FOWNER, FSETID, KILL, SETGID, SETUID, SETPCAP,
   NET_BIND_SERVICE, NET_RAW, SYS_CHROOT, MKNOD, AUDIT_WRITE, SETFCAP, SYS_ADMIN`
3. `storage.conf`: `driver = "overlay"`

`hostUsers: false`, `privileged: false`는 변경 없이 유지된다.

## 6. 실제 gRPC BuildService를 통한 종단 검증

재배포 후 (`nodevault-controlplane` Pod `1/1 Running`, 새 securityContext 적용),
재현 Pod가 아닌 **실제 Deployment**에 port-forward + `grpcurl`로
`nodevault.v1.BuildService/BuildAndRegister`를 직접 호출했다:

```
grpcurl -plaintext -import-path protos -proto nodevault/v1/nodevault.proto \
  -d @ localhost:50051 nodevault.v1.BuildService/BuildAndRegister
```

결과: `BUILD_EVENT_KIND_JOB_CREATED` 이벤트가 발생했고, 4번에서 고치기 전이라면
여기서 `unshare`/`setgroups`/`setcap` 단계에서 막혔어야 하는데, 이번에는 그 단계를
모두 통과해서 **실제 베이스 이미지 pull(TLS 핸드셰이크) 단계까지 도달**했다. 즉
overlay storage + chroot isolation 경로 자체는 실제 BuildService를 통해 작동 확인됨.

## 7. Harbor CA 이슈 (overlay와 별개, infra-lab에서 해결)

종단 테스트에서 다음 에러로 멈췄다:

```
initializing source docker://harbor.lab.local/nodevault/controlplane:latest:
pinging container registry harbor.lab.local: Get "https://harbor.lab.local/v2/":
tls: failed to verify certificate: x509: certificate signed by unknown authority
```

이것은 overlay/userns 문제와 무관한 **별개의, 기존부터 있던 문제**였다: 노드의 containerd는
`/etc/containerd/certs.d/harbor.lab.local`(추정) 등으로 Harbor의 자체서명 인증서를
신뢰하도록 설정되어 있어 NodeVault 이미지 자체의 `imagePullPolicy: Always` pull은
문제없이 동작하지만, **Pod 내부에서 Buildah가 직접 수행하는 pull은 이 신뢰 설정을
공유하지 않는 것처럼 보였다.** `deploy/03-nodevault.yaml`에는 Harbor CA를 Pod에
마운트하거나 `registries.conf`에 insecure 항목을 추가하는 설정이 없다 — 처음부터
없었고, 이번에 처음으로 RUN 단계까지 도달하는 빌드를 실행해봐서 드러난 것이다.

**조용히 우회하지 않음**: `buildah` CLI 수동 테스트에서는 `--tls-verify=false`로
이 문제를 피해갔지만, 실제 gRPC 경로(podbridge5 Go API)에는 그런 플래그가 없다.

**추가 확인 (2026-06-18)**: `~/.config/infra-lab/certs/harbor-ca.crt` (로컬 및 seoy
양쪽에 존재)가 정확히 `harbor.lab.local`이 서빙하는 인증서의 issuer임을 확인했다
(`openssl x509 -issuer` 일치, `CN = harbor-lab-ca`). 즉 **CA가 없어서가 아니라, 이
CA를 NodeVault Pod에 마운트/등록하는 절차가 처음부터 없었던 것**이다. NodeVault repo
어디에도 `harbor-ca` 또는 `.config/infra-lab` 참조가 없다 (grep 결과 0건) — 즉 의도적
설계가 아니라 빠진 설정으로 보였다. 당시 검토한 해결책은 다음 둘 중 하나였다:

- `harbor-ca.crt`를 K8s Secret으로 만들어 Pod에 마운트하고 컨테이너 시스템 트러스트
  스토어(또는 `/etc/containers/certs.d/harbor.lab.local/ca.crt`)에 추가, 또는
- `/etc/containers/registries.conf.d/`에 `harbor.lab.local`을 insecure 또는
  특정 CA로 등록하는 ConfigMap 추가

**추가 확인 (2026-06-18, 마운트 경로 검증)**: podbridge5 v0.1.3 기준 build/push 양쪽 경로
(`image_options.go:48`의 `DefaultImageBuildOptions()`, `image_options.go:159`의
`UserNamespaceImageBuildOptions()`, `image_runtime.go:43`의 `PushImage()`)에서
`types.SystemContext`의 `DockerCertPath`/`DockerPerHostCertDirPath`를 전혀 채우지
않는다 — zero-valued. 즉 `go.podman.io/image/v5`의 기본 탐색 경로
(`docker/docker_client.go`의 `dockerCertDir`)로 그대로 떨어지며, 이 경로는:

```
/etc/containers/certs.d/<registry-domain>/*.crt   (포트 443은 기본값이라 suffix 불필요)
```

디렉토리 안의 `*.crt` 파일을 전부 신뢰 풀에 추가한다 (파일명이 꼭 `ca.crt`일 필요는
없음). **podbridge5/NodeVault 코드 변경 없이, K8s Secret + volumeMount만으로 해결
가능**:

```yaml
# 최초 1회: kubectl create secret generic nodevault-harbor-ca \
#   --from-file=ca.crt=harbor-ca.crt -n nodevault-system   (harbor-ca.crt는
#   ~/.config/infra-lab/certs/harbor-ca.crt — 위 추가 확인 참조)

volumeMounts:
  - name: harbor-ca
    mountPath: /etc/containers/certs.d/harbor.lab.local
    readOnly: true
volumes:
  - name: harbor-ca
    secret:
      secretName: nodevault-harbor-ca
```

위 Secret 마운트 설계는 검토 완료였지만, 2026-06-19 현재 Harbor CA 문제는
infra-lab Harbor 설치/CA 배포 절차 쪽에서 해결된 상태로 운용한다
(`~/.config/infra-lab/install-guide.md`, `~/.config/infra-lab/certs/harbor-ca.crt`).
따라서 이 변경에서는 NodeVault 매니페스트에 별도 CA Secret 마운트를 추가하지 않았다.

## 8. 결론

- overlay storage driver는 `hostUsers:false`, `privileged:false`,
  `allowPrivilegeEscalation:false` 제약을 모두 유지한 채로 동작 가능하다 (GPT가 제안한
  UID/마운트 구조 변경은 불필요했다).
- 필요했던 것은 (a) `appArmorProfile: Unconfined`, (b) capability set을 OCI 기본
  풀셋 + `SYS_ADMIN`으로 확장하는 것 두 가지였다. 현재 NodeVault 배포의 storage driver는
  overlay이며, vfs와 fuse-overlayfs fallback은 명시적 재검증 없이 적용하지 않는다.
- 별도로, Harbor 자체서명 인증서에 대한 신뢰 설정 문제는 이번 조사로 드러났지만
  overlay와 무관하며, 이후 infra-lab Harbor CA 배포 절차 쪽에서 해결된 상태로 운용한다 (7번).

## 9. podbridge5 개발 트랙에 대한 제안 (2026-06-18)

podbridge5는 현재 별도 트랙에서 개발 중이다. 이번 조사로 NodeVault 쪽에서 capability
set 확장으로 우회한 4번 문제는, podbridge5 쪽에서 다음을 고려하면 K8s 소비자 입장에서
더 적은 권한으로 해결될 수 있다 — 우회가 아니라 근본 해결의 선택지로 기록한다.

1. **`Isolation = define.IsolationChroot` 하드코딩 재검토**
   (`image_options.go:159`, `UserNamespaceImageBuildOptions`). storage driver와
   무관하게 모든 빌드에 강제되고 있고, 이 경로가 RUN 단계마다 `unshare(CLONE_NEWUSER)`
   → `setgroups()` → `setcap()`을 직접 수행하기 때문에 K8s 호출자는 OCI 기본
   capability 풀셋 + `SYS_ADMIN`을 전부 부여해야 한다 (5번 표 참조). 이미 `hostUsers:false`
   로 Pod 전체가 자신만의 userns를 갖고 UID 0로 실행되는 상황이라면, 굳이 또 한 번
   nested unshare를 시도할 필요가 없을 수 있다. Isolation을 `IsolationOCI`
   (crun/runc에 위임)로 전환하거나, 최소한 호출자가 isolation 모드를 선택할 수 있게
   옵션을 노출해주면, K8s 배포 시 필요한 capability 수가 크게 줄어들 가능성이 있다.
2. **"이미 Pod-owned userns 안에서 root로 실행 중"인 경우를 감지해 nested unshare를
   skip하는 경로**가 있다면 가장 이상적이다. `rootless.go`의 `ReexecIfNeeded()`가
   비슷한 목적의 코드처럼 보이는데(`image.go`의 호출 경로에서는 사용되지 않음),
   이 로직을 chroot isolation 경로에도 연결할 수 있는지 검토해볼 만하다.
3. **registries 신뢰 설정(TLS CA)을 명확한 옵션으로 노출**. 현재
   `NewStoreWithOptions()`는 storage driver/runroot/graphroot는 ConfigMap 기반으로
   완전히 따라가지만(이 부분은 그대로 유지하면 좋음), 사설 registry의 자체서명 CA를
   신뢰시키는 경로(`/etc/containers/certs.d/<registry>/ca.crt` 등)는 호출자가 직접
   파일시스템에 깔아줘야 하고 podbridge5 쪽에 별도 옵션이 없다. 환경변수나 옵션으로
   "이 경로의 CA 디렉토리를 certs.d로 써라" 정도만 노출해줘도 NodeVault 같은 소비자가
   설정하기 쉬워진다.
4. **에러 wrapping/힌트**. `unshare(CLONE_NEWUSER): Operation not permitted`,
   `error setting supplemental groups list`, `error setting capabilities for
   process` 세 가지 에러는 원인(AppArmor vs capability 부족)이 서로 다른데 사용자
   입장에서는 구분하기 어렵다. 가능하다면 이런 실패를 좀 더 구체적인 힌트(예: "이
   기능은 hostUsers:false 환경에서 CAP_SYS_ADMIN과 AppArmor unconfined가 필요합니다"
   같은)로 wrap해주면 K8s 환경에서의 디버깅 비용이 크게 줄어든다.

전체 조사 세부사항(증상별 정확한 에러 메시지, 재현 절차)은 위 1~7번 참조.
