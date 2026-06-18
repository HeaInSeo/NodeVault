.PHONY: fmt lint lint-fix lint-config golangci-lint kube-linter kube-lint test test-integration test-integration-infralab \
        deploy-infralab undeploy-infralab build push-image vendor \
        proto coverage vuln slint clean all deploy-seoy

LOCALBIN      ?= $(CURDIR)/bin
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.11.3
KUBE_LINTER   ?= $(LOCALBIN)/kube-linter
KUBE_LINTER_VERSION ?= v0.8.3
KUBE_LINTER_SHA256_LINUX_AMD64 ?= 618d299a3e2839c8ca9d86fce0db617be0fba41f0fecbbbfb7fbf1c04299fae1
KUBE_LINTER_SHA256_LINUX_ARM64 ?= 9c39d35252e0dcafb16b26197b9e93ba578e44eb402c3c6660fc94e08f94094f
PROTOC        ?= protoc
PROTO_OUT     ?= ./gen/go
PROTO_SRC     ?= ./protos

# ── 컨테이너 빌드 관련 태그 ───────────────────────────────────────────────────
# btrfs-progs-devel, gpgme-devel C 헤더 없이도 빌드 가능하도록
# containers/storage, containers/image의 선택적 드라이버를 제외한다.
BUILDTAGS ?= exclude_graphdriver_btrfs containers_image_openpgp exclude_graphdriver_devicemapper

# ── infra-lab / Harbor 설정 ──────────────────────────────────────────
INFRALAB_KUBECONFIG ?= $(shell realpath ../infra-lab/kubeconfig 2>/dev/null || echo "")
INFRALAB_REGISTRY   ?= harbor.10.113.24.96.nip.io
IMAGE                ?= $(INFRALAB_REGISTRY)/nodevault/controlplane:latest

# ── 포맷 ──────────────────────────────────────────────────────────────────────
fmt:
	go fmt ./...

# ── 린트 ──────────────────────────────────────────────────────────────────────
golangci-lint:
	@mkdir -p "$(LOCALBIN)"
	@test -x "$(GOLANGCI_LINT)" || bash -c '\
		set -euo pipefail; \
		curl -fsSL "https://api.github.com/repos/golangci/golangci-lint/releases/tags/$(GOLANGCI_LINT_VERSION)" >/dev/null; \
		OS="$$(uname | tr A-Z a-z)"; \
		ARCH="$$(uname -m)"; \
		case "$$ARCH" in x86_64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) echo "unsupported arch: $$ARCH"; exit 1 ;; esac; \
		VER="$(GOLANGCI_LINT_VERSION)"; \
		VER="$${VER#v}"; \
		FILE="golangci-lint-$$VER-$$OS-$$ARCH.tar.gz"; \
		URL="https://github.com/golangci/golangci-lint/releases/download/$(GOLANGCI_LINT_VERSION)/$$FILE"; \
		SUM_URL="https://github.com/golangci/golangci-lint/releases/download/$(GOLANGCI_LINT_VERSION)/golangci-lint-$$VER-checksums.txt"; \
		TMP="$$(mktemp -d)"; \
		curl -fsSL "$$URL" -o "$$TMP/lint.tgz"; \
		curl -fsSL "$$SUM_URL" -o "$$TMP/checksums.txt"; \
		EXPECTED="$$(awk -v f="$$FILE" "\$$2==f{print \$$1}" "$$TMP/checksums.txt")"; \
		if [ -z "$$EXPECTED" ]; then echo "checksum not found for $$FILE"; exit 1; fi; \
		if command -v sha256sum >/dev/null 2>&1; then \
			ACTUAL="$$(sha256sum "$$TMP/lint.tgz" | awk "{print \$$1}")"; \
		elif command -v shasum >/dev/null 2>&1; then \
			ACTUAL="$$(shasum -a 256 "$$TMP/lint.tgz" | awk "{print \$$1}")"; \
		else \
			echo "no sha256 tool found (sha256sum/shasum)"; exit 1; \
		fi; \
		if [ "$$EXPECTED" != "$$ACTUAL" ]; then echo "checksum mismatch for $$FILE"; exit 1; fi; \
		tar -xzf "$$TMP/lint.tgz" -C "$$TMP"; \
		cp "$$TMP/golangci-lint-$$VER-$$OS-$$ARCH/golangci-lint" "$(GOLANGCI_LINT)"; \
		chmod +x "$(GOLANGCI_LINT)"; \
		rm -rf "$$TMP"'

lint: golangci-lint
	$(GOLANGCI_LINT) run --config=.golangci.yml --build-tags "$(BUILDTAGS)" ./...

lint-fix: golangci-lint
	$(GOLANGCI_LINT) run --config=.golangci.yml --build-tags "$(BUILDTAGS)" --fix ./...

lint-config: golangci-lint
	$(GOLANGCI_LINT) config verify --config=.golangci.yml

# ── kube-linter (K8s 매니페스트 가드레일) ──────────────────────────────────────
# stackrox/kube-linter는 checksums.txt를 게시하지 않으므로, golangci-lint와
# 동일한 검증 방식을 적용하기 위해 고정 버전의 SHA256을 직접 핀한다.
kube-linter:
	@mkdir -p "$(LOCALBIN)"
	@test -x "$(KUBE_LINTER)" || bash -c '\
		set -euo pipefail; \
		ARCH="$$(uname -m)"; \
		case "$$ARCH" in \
			x86_64) SUFFIX=""; EXPECTED="$(KUBE_LINTER_SHA256_LINUX_AMD64)" ;; \
			aarch64|arm64) SUFFIX="_arm64"; EXPECTED="$(KUBE_LINTER_SHA256_LINUX_ARM64)" ;; \
			*) echo "unsupported arch: $$ARCH"; exit 1 ;; \
		esac; \
		URL="https://github.com/stackrox/kube-linter/releases/download/$(KUBE_LINTER_VERSION)/kube-linter-linux$$SUFFIX"; \
		curl -fsSL "$$URL" -o "$(KUBE_LINTER)"; \
		ACTUAL="$$(sha256sum "$(KUBE_LINTER)" | awk "{print \$$1}")"; \
		if [ "$$EXPECTED" != "$$ACTUAL" ]; then echo "checksum mismatch for kube-linter"; rm -f "$(KUBE_LINTER)"; exit 1; fi; \
		chmod +x "$(KUBE_LINTER)"'

kube-lint: kube-linter
	$(KUBE_LINTER) lint deploy/ --config .kube-linter.yaml

# ── 단위 테스트 ───────────────────────────────────────────────────────────────
test:
	go test -tags "$(BUILDTAGS)" -v -race -cover ./...

# ── 통합 테스트 (infra-lab VM 클러스터) ───────────────────────────────
# 사전 조건:
#   1. infra-lab 클러스터와 Harbor가 실행 중
#   2. NodeVault image push 완료
#   3. make deploy-infralab 완료
#
# The test connects to the deployed NodeVault Pod through a temporary
# port-forward, so the in-pod-buildah path is exercised end to end.
test-integration-infralab:
	@if [ -z "$(INFRALAB_KUBECONFIG)" ]; then \
	    echo "ERROR: infra-lab/kubeconfig not found. 클러스터를 먼저 실행하세요." >&2; exit 1; \
	fi
	@echo "==> Cluster: $$(KUBECONFIG=$(INFRALAB_KUBECONFIG) kubectl get nodes --no-headers 2>&1 | awk '{print $$1, $$2}' | tr '\n' '  ')"
	@echo "==> NodeVault service port-forward 시작..."
	@KUBECONFIG=$(INFRALAB_KUBECONFIG) kubectl -n nodevault-system \
	    port-forward service/nodevault-controlplane 50051:50051 >/tmp/nodevault-port-forward.log 2>&1 & \
	PF_PID=$$!; \
	trap 'kill $$PF_PID 2>/dev/null || true' EXIT INT TERM; \
	sleep 3; \
	if ! kill -0 $$PF_PID 2>/dev/null; then \
	    cat /tmp/nodevault-port-forward.log >&2; \
	    exit 1; \
	fi; \
	echo "==> in-pod-buildah 통합 테스트 실행 (port-forward pid=$$PF_PID)..."; \
	KUBECONFIG=$(INFRALAB_KUBECONFIG) \
	    go test -v -tags "integration $(BUILDTAGS)" ./pkg/build/... -timeout 12m

# ── 클러스터 리소스 배포 ────────────────────────────────────────────────────
# NodeVault is a Kubernetes data-plane application. The Deployment executes
# image builds in the NodeVault Pod through podbridge5/Buildah; it does not
# create a builder Job.
deploy-infralab:
	@if [ -z "$(INFRALAB_KUBECONFIG)" ]; then \
	    echo "ERROR: infra-lab/kubeconfig not found." >&2; exit 1; \
	fi
	@echo "==> NodeVault namespaces + validation RBAC 적용..."
	KUBECONFIG=$(INFRALAB_KUBECONFIG) kubectl apply -f deploy/00-namespaces.yaml
	KUBECONFIG=$(INFRALAB_KUBECONFIG) kubectl apply -f deploy/02-rbac.yaml
	@for secret in nodevault-registry-auth nodevault-harbor-auth; do \
	    if ! KUBECONFIG=$(INFRALAB_KUBECONFIG) kubectl -n nodevault-system get secret $$secret >/dev/null 2>&1; then \
	        echo "ERROR: nodevault-system/$$secret is required before deploying NodeVault." >&2; \
	        exit 1; \
	    fi; \
	done
	@echo "==> NodeVault in-Pod Buildah Deployment 적용..."
	KUBECONFIG=$(INFRALAB_KUBECONFIG) kubectl apply -f deploy/03-nodevault.yaml
	KUBECONFIG=$(INFRALAB_KUBECONFIG) kubectl -n nodevault-system rollout status deployment/nodevault-controlplane --timeout=180s

# ── 클러스터 리소스 제거 ──────────────────────────────────────────────────────
undeploy-infralab:
	@if [ -z "$(INFRALAB_KUBECONFIG)" ]; then \
	    echo "ERROR: infra-lab/kubeconfig not found." >&2; exit 1; \
	fi
	KUBECONFIG=$(INFRALAB_KUBECONFIG) kubectl delete -f deploy/04-grpcroute.yaml --ignore-not-found=true
	KUBECONFIG=$(INFRALAB_KUBECONFIG) kubectl delete -f deploy/03-nodevault.yaml --ignore-not-found=true
	KUBECONFIG=$(INFRALAB_KUBECONFIG) kubectl delete -f deploy/02-rbac.yaml --ignore-not-found=true

# ── 로컬 바이너리 빌드 ────────────────────────────────────────────────────────
build:
	go build -tags "$(BUILDTAGS)" -o bin/nodevault ./cmd/controlplane/...
	go build -o bin/nodepalette ./cmd/palette/...

# ── vendor 생성 (컨테이너 이미지 빌드 전 필요) ────────────────────────────────
# go.mod의 replace directive(podbridge5)가 로컬 경로를 가리키므로
# vendor/ 에 복사해야 Dockerfile 내 빌드가 가능하다.
vendor:
	go mod vendor

# ── NodeVault 이미지 빌드 + Harbor push ───────────────────────────────────────
# 사전 조건:
#   podman login harbor.10.113.24.96.nip.io   (최초 1회)
#
# 실행:
#   make push-image
#   make push-image IMAGE=harbor.10.113.24.96.nip.io/nodevault/controlplane:v1.0.0
push-image: vendor
	podman build \
	    -t $(IMAGE) \
	    -f Dockerfile \
	    .
	podman push $(IMAGE)

# ── proto 생성 ────────────────────────────────────────────────────────────────
proto:
	@mkdir -p $(PROTO_OUT)
	$(PROTOC) --proto_path=$(PROTO_SRC) \
	  --go_out=$(PROTO_OUT) --go_opt=paths=source_relative \
	  --go-grpc_out=$(PROTO_OUT) --go-grpc_opt=paths=source_relative \
	  $(shell find $(PROTO_SRC) -name '*.proto')

# ── 커버리지 ──────────────────────────────────────────────────────────────────
coverage:
	go test -tags "$(BUILDTAGS)" -race -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# ── 취약점 스캔 ───────────────────────────────────────────────────────────────
vuln:
	go install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck -tags "$(BUILDTAGS)" ./...

# ── kube-slint gate (churn suppression + reconcile regression) ────────────────
# 사전 조건: make build (bin/nodevault 존재해야 함)
slint: build
	NODEVAULT_BIN=bin/nodevault \
	go test -tags "slint $(BUILDTAGS)" -timeout 120s -v -run TestNodeVaultSlintGate ./test/slint/...

# ── 정리 ──────────────────────────────────────────────────────────────────────
clean:
	rm -rf bin/ vendor/ coverage.out $(PROTO_OUT)

# ── seoy 호스트 배포 ──────────────────────────────────────────────────────────
# 바이너리를 빌드하고 seoy(100.123.80.48)에 배포한다.
# SEOY_USER 환경 변수로 SSH 사용자 지정 가능 (기본: heain)
deploy-seoy: build
	bash scripts/deploy-seoy.sh

# ── 전체 (포맷 → 테스트 → 빌드) ──────────────────────────────────────────────
all: fmt test build
