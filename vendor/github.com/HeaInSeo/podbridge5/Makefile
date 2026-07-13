GO ?= go
GO_REQUIRED_VERSION ?= 1.25.6

PODBRIDGE5_VM_NAME ?= podbridge5-dev
PODBRIDGE5_VM_CPUS ?= 2
PODBRIDGE5_VM_MEMORY ?= 4G
PODBRIDGE5_VM_DISK ?= 20G
PODBRIDGE5_VM_REPO ?= /home/ubuntu/work/src/github.com/HeaInSeo/podbridge5
PODBRIDGE5_LOCAL_REPO ?= $(CURDIR)

REMOTE_HOST ?= 100.123.80.48
REMOTE_USER ?= seoy
REMOTE_PORT ?= 22
REMOTE_PASS ?=

ARTIFACTS_DIR ?= $(CURDIR)/artifacts
VM_TEST_RUNTIME_LOG ?= $(ARTIFACTS_DIR)/vm-test-runtime.log
VM_TEST_RUNTIME_INTEGRATION_LOG ?= $(ARTIFACTS_DIR)/vm-test-runtime-integration.log

COVERAGE_UNIT_OUT ?= $(ARTIFACTS_DIR)/coverage-unit.out
COVERAGE_RUNTIME_OUT ?= $(ARTIFACTS_DIR)/coverage-runtime.out
COVERAGE_RUNTIME_INTEGRATION_OUT ?= $(ARTIFACTS_DIR)/coverage-runtime-integration.out
COVERAGE_MERGED_OUT ?= $(ARTIFACTS_DIR)/coverage-merged.out

REMOTE_VM_RUN = \
	REMOTE_HOST='$(REMOTE_HOST)' REMOTE_USER='$(REMOTE_USER)' REMOTE_PORT='$(REMOTE_PORT)' REMOTE_PASS='$(REMOTE_PASS)' \
	PODBRIDGE5_VM_NAME='$(PODBRIDGE5_VM_NAME)' PODBRIDGE5_VM_CPUS='$(PODBRIDGE5_VM_CPUS)' PODBRIDGE5_VM_MEMORY='$(PODBRIDGE5_VM_MEMORY)' PODBRIDGE5_VM_DISK='$(PODBRIDGE5_VM_DISK)' \
	PODBRIDGE5_VM_REPO='$(PODBRIDGE5_VM_REPO)' PODBRIDGE5_LOCAL_REPO='$(PODBRIDGE5_LOCAL_REPO)' PODBRIDGE5_GO_VERSION='$(GO_REQUIRED_VERSION)' \
	cd '$(CURDIR)/hack/remotevm' && $(GO) run .

.PHONY: test test-unit test-runtime test-runtime-integration check-runtime-build go-version-check \
	runtime-env-check runtime-host-check runtime-integration-host-check check-remote-auth \
	vm-create-runtime vm-prepare-runtime vm-sync-runtime vm-run-runtime \
	vm-run-runtime-integration vm-delete-runtime vm-test-runtime \
	vm-test-runtime-integration coverage-merge lint lint-fix

TEST_TAGS_BASE ?= exclude_graphdriver_btrfs containers_image_openpgp exclude_graphdriver_devicemapper
TEST_TAGS_RUNTIME ?= $(TEST_TAGS_BASE) runtime
TEST_TAGS_RUNTIME_INTEGRATION ?= $(TEST_TAGS_BASE) runtime integration

lint: go-version-check
	golangci-lint run --build-tags "$(TEST_TAGS_BASE)"

lint-fix: go-version-check
	golangci-lint run --build-tags "$(TEST_TAGS_BASE)" --fix

# Legacy alias kept for compatibility.
test: test-unit

test-unit: go-version-check
	@mkdir -p '$(ARTIFACTS_DIR)'
	$(GO) test -v -race -cover -coverprofile='$(COVERAGE_UNIT_OUT)' -tags "$(TEST_TAGS_BASE)" ./...

# Concatenates the unit profile with whichever runtime/integration profiles
# vm-run-runtime / vm-run-runtime-integration fetched from the VM into
# artifacts/, then reports the combined statement coverage. go tool cover
# sums per-block counts across repeated "file:line.col,line.col numStmts"
# entries, so naive concatenation under one mode header is a valid merge as
# long as each tagged test suite covers mostly disjoint code paths (true
# here: unit tests exercise the *WithRuntime logic via fakes, runtime tests
# exercise the real podman/buildah adapters that unit tests can't reach).
coverage-merge:
	@mkdir -p '$(ARTIFACTS_DIR)'
	@profiles=""; \
	for f in '$(COVERAGE_UNIT_OUT)' '$(COVERAGE_RUNTIME_OUT)' '$(COVERAGE_RUNTIME_INTEGRATION_OUT)'; do \
		if [ -f "$$f" ]; then profiles="$$profiles $$f"; fi; \
	done; \
	if [ -z "$$profiles" ]; then \
		echo "[coverage-merge] no coverage profiles found in $(ARTIFACTS_DIR)" >&2; \
		echo "[coverage-merge] run 'make test-unit' and/or 'make vm-run-runtime' first" >&2; \
		exit 1; \
	fi; \
	echo "[coverage-merge] merging:$$profiles"; \
	echo "mode: atomic" > '$(COVERAGE_MERGED_OUT)'; \
	for f in $$profiles; do tail -n +2 "$$f" >> '$(COVERAGE_MERGED_OUT)'; done; \
	$(GO) tool cover -func='$(COVERAGE_MERGED_OUT)' | tail -1

# Compiles every runtime/integration-tagged test file without needing a live
# Podman socket or remote VM. Catches build breaks (e.g. references to
# functions removed from production code) that go-version-check/test-unit
# can't see because they never compile those files.
check-runtime-build: go-version-check
	@echo "[check-runtime-build] tags: $(TEST_TAGS_RUNTIME_INTEGRATION)"
	$(GO) vet -tags "$(TEST_TAGS_RUNTIME_INTEGRATION)" ./...

test-runtime: go-version-check runtime-env-check runtime-host-check
	@echo "[test-runtime] tags: $(TEST_TAGS_RUNTIME)"
	@echo "[test-runtime] running runtime-tagged tests on the current host"
	$(GO) test -v -tags "$(TEST_TAGS_RUNTIME)" ./...

# Runtime-sensitive integration tests.
#
# Plain `unshare -r` only maps the calling uid to 0 inside a fresh user
# namespace - it doesn't grant the full subuid/subgid range, so extracting a
# layer that chowns into a wider GID (e.g. /etc/shadow at gid 42) fails with
# "potentially insufficient UIDs or GIDs available in user namespace".
# `podman unshare` builds the namespace via newuidmap/newgidmap using the
# host's configured subuid/subgid ranges (see /etc/subuid, /etc/subgid) and
# also exports _CONTAINERS_USERNS_CONFIGURED/_CONTAINERS_ROOTLESS_UID/
# _CONTAINERS_ROOTLESS_GID itself, so go.podman.io/storage correctly resolves
# rootless ($HOME-based) storage paths inside the namespace.
test-runtime-integration: go-version-check runtime-env-check runtime-host-check runtime-integration-host-check
	@echo "[test-runtime-integration] tags: $(TEST_TAGS_RUNTIME_INTEGRATION)"
	@echo "[test-runtime-integration] running integration tests with podman unshare"
	@podman unshare $(GO) test -v -tags "$(TEST_TAGS_RUNTIME_INTEGRATION)" ./...

go-version-check:
	@command -v $(GO) >/dev/null 2>&1 || { echo "missing: $(GO)" >&2; exit 1; }
	@actual="$$( $(GO) env GOVERSION 2>/dev/null || true )"; \
	if [ -z "$$actual" ]; then \
		actual="$$( $(GO) version | awk '{print $$3}' )"; \
	fi; \
	case "$$actual" in \
		go$(GO_REQUIRED_VERSION)) \
			echo "[go-version-check] using $$actual"; \
			;; \
		*) \
			echo "[go-version-check] required go$(GO_REQUIRED_VERSION), got $${actual:-<unknown>}" >&2; \
			echo "[go-version-check] podbridge5 tracks the same Go 1.25.x baseline as sibling projects" >&2; \
			exit 1; \
			;; \
	esac

runtime-env-check:
	@command -v buildah >/dev/null 2>&1 || { echo "missing: buildah" >&2; exit 1; }
	@command -v fuse-overlayfs >/dev/null 2>&1 || { echo "missing: fuse-overlayfs" >&2; exit 1; }
	@command -v pkg-config >/dev/null 2>&1 || { echo "missing: pkg-config" >&2; exit 1; }
	@pkg-config --exists gpgme || { echo "missing pkg-config entry: gpgme" >&2; exit 1; }
	@test -f /usr/include/btrfs/version.h || { echo "missing header: /usr/include/btrfs/version.h" >&2; exit 1; }
	@echo "runtime environment looks ready"

runtime-host-check:
	@set -e; \
	uid=$$(id -u); \
	runtime_dir="$${XDG_RUNTIME_DIR:-/run/user/$$uid}"; \
	container_host="$${CONTAINER_HOST:-}"; \
	user_socket="$$runtime_dir/podman/podman.sock"; \
	system_socket="/run/podman/podman.sock"; \
	echo "[runtime-host-check] uid=$$uid"; \
	echo "[runtime-host-check] XDG_RUNTIME_DIR=$$runtime_dir"; \
	echo "[runtime-host-check] CONTAINER_HOST=$${container_host:-<unset>}"; \
	if [ -n "$$container_host" ]; then \
		case "$$container_host" in \
			unix://*) socket_path="$${container_host#unix://}" ;; \
			unix:*) socket_path="$${container_host#unix:}" ;; \
			*) echo "[runtime-host-check] CONTAINER_HOST is set; podbridge5 will use it as-is"; exit 0 ;; \
		esac; \
		if [ -S "$$socket_path" ]; then \
			echo "[runtime-host-check] found CONTAINER_HOST socket: $$socket_path"; \
			exit 0; \
		fi; \
		echo "[runtime-host-check] missing CONTAINER_HOST socket: $$socket_path" >&2; \
		exit 1; \
	fi; \
	if [ -S "$$user_socket" ]; then \
		echo "[runtime-host-check] found user podman socket: $$user_socket"; \
	elif [ -S "$$system_socket" ]; then \
		echo "[runtime-host-check] found system podman socket: $$system_socket"; \
	else \
		echo "[runtime-host-check] missing podman socket" >&2; \
		echo "[runtime-host-check] expected one of:" >&2; \
		echo "  - $$user_socket" >&2; \
		echo "  - $$system_socket" >&2; \
		echo "[runtime-host-check] podbridge5 resolves runtime in this order: CONTAINER_HOST -> XDG_RUNTIME_DIR -> default podman socket" >&2; \
		echo "[runtime-host-check] if VM validation is unavailable, keep using make test-unit until the host runtime is ready" >&2; \
		exit 1; \
	fi

runtime-integration-host-check:
	@command -v podman >/dev/null 2>&1 || { echo "missing: podman" >&2; exit 1; }
	@echo "[runtime-integration-host-check] podman unshare is available"

check-remote-auth:
	@if [ -n "$(REMOTE_PASS)" ]; then \
		echo "[check-remote-auth] using REMOTE_PASS"; \
	elif [ -n "$$SSH_AUTH_SOCK" ]; then \
		echo "[check-remote-auth] using SSH agent"; \
	elif [ -f "$$HOME/.ssh/id_ed25519" ] || [ -f "$$HOME/.ssh/id_rsa" ]; then \
		echo "[check-remote-auth] using default SSH key file"; \
	else \
		echo "set REMOTE_PASS or configure SSH key auth for remote VM automation" >&2; \
		exit 1; \
	fi

vm-create-runtime: check-remote-auth
	@$(REMOTE_VM_RUN) create

vm-prepare-runtime: check-remote-auth
	@$(REMOTE_VM_RUN) prepare

vm-sync-runtime: check-remote-auth
	@$(REMOTE_VM_RUN) sync

vm-run-runtime: check-remote-auth
	@$(REMOTE_VM_RUN) run

vm-run-runtime-integration: check-remote-auth
	@$(REMOTE_VM_RUN) run-integration

vm-delete-runtime: check-remote-auth
	@$(REMOTE_VM_RUN) delete

vm-test-runtime:
	@mkdir -p '$(ARTIFACTS_DIR)'
	@set -euo pipefail; \
	log_file='$(VM_TEST_RUNTIME_LOG)'; \
	cleanup() { \
		$(MAKE) --no-print-directory vm-delete-runtime \
			REMOTE_HOST='$(REMOTE_HOST)' REMOTE_USER='$(REMOTE_USER)' REMOTE_PORT='$(REMOTE_PORT)' REMOTE_PASS='$(REMOTE_PASS)' \
			PODBRIDGE5_VM_NAME='$(PODBRIDGE5_VM_NAME)' >/dev/null; \
	}; \
	trap cleanup EXIT INT TERM; \
	{ \
		echo "[vm-test-runtime] log file: $$log_file"; \
		echo "[vm-test-runtime] local repo: $(PODBRIDGE5_LOCAL_REPO)"; \
		echo "[vm-test-runtime] required Go version: $(GO_REQUIRED_VERSION)"; \
		$(MAKE) --no-print-directory vm-create-runtime \
			REMOTE_HOST='$(REMOTE_HOST)' REMOTE_USER='$(REMOTE_USER)' REMOTE_PORT='$(REMOTE_PORT)' REMOTE_PASS='$(REMOTE_PASS)' \
			PODBRIDGE5_VM_NAME='$(PODBRIDGE5_VM_NAME)' PODBRIDGE5_VM_CPUS='$(PODBRIDGE5_VM_CPUS)' PODBRIDGE5_VM_MEMORY='$(PODBRIDGE5_VM_MEMORY)' PODBRIDGE5_VM_DISK='$(PODBRIDGE5_VM_DISK)'; \
		$(MAKE) --no-print-directory vm-prepare-runtime \
			REMOTE_HOST='$(REMOTE_HOST)' REMOTE_USER='$(REMOTE_USER)' REMOTE_PORT='$(REMOTE_PORT)' REMOTE_PASS='$(REMOTE_PASS)' \
			PODBRIDGE5_VM_NAME='$(PODBRIDGE5_VM_NAME)' PODBRIDGE5_VM_REPO='$(PODBRIDGE5_VM_REPO)'; \
		$(MAKE) --no-print-directory vm-sync-runtime \
			REMOTE_HOST='$(REMOTE_HOST)' REMOTE_USER='$(REMOTE_USER)' REMOTE_PORT='$(REMOTE_PORT)' REMOTE_PASS='$(REMOTE_PASS)' \
			PODBRIDGE5_VM_NAME='$(PODBRIDGE5_VM_NAME)' PODBRIDGE5_VM_REPO='$(PODBRIDGE5_VM_REPO)' PODBRIDGE5_LOCAL_REPO='$(PODBRIDGE5_LOCAL_REPO)'; \
		$(MAKE) --no-print-directory vm-run-runtime \
			REMOTE_HOST='$(REMOTE_HOST)' REMOTE_USER='$(REMOTE_USER)' REMOTE_PORT='$(REMOTE_PORT)' REMOTE_PASS='$(REMOTE_PASS)' \
			PODBRIDGE5_VM_NAME='$(PODBRIDGE5_VM_NAME)' PODBRIDGE5_VM_REPO='$(PODBRIDGE5_VM_REPO)'; \
	} 2>&1 | tee "$$log_file"; \
	status=$${PIPESTATUS[0]}; \
	exit $$status

vm-test-runtime-integration:
	@mkdir -p '$(ARTIFACTS_DIR)'
	@set -euo pipefail; \
	log_file='$(VM_TEST_RUNTIME_INTEGRATION_LOG)'; \
	cleanup() { \
		$(MAKE) --no-print-directory vm-delete-runtime \
			REMOTE_HOST='$(REMOTE_HOST)' REMOTE_USER='$(REMOTE_USER)' REMOTE_PORT='$(REMOTE_PORT)' REMOTE_PASS='$(REMOTE_PASS)' \
			PODBRIDGE5_VM_NAME='$(PODBRIDGE5_VM_NAME)' >/dev/null; \
	}; \
	trap cleanup EXIT INT TERM; \
	{ \
		echo "[vm-test-runtime-integration] log file: $$log_file"; \
		echo "[vm-test-runtime-integration] local repo: $(PODBRIDGE5_LOCAL_REPO)"; \
		echo "[vm-test-runtime-integration] required Go version: $(GO_REQUIRED_VERSION)"; \
		$(MAKE) --no-print-directory vm-create-runtime \
			REMOTE_HOST='$(REMOTE_HOST)' REMOTE_USER='$(REMOTE_USER)' REMOTE_PORT='$(REMOTE_PORT)' REMOTE_PASS='$(REMOTE_PASS)' \
			PODBRIDGE5_VM_NAME='$(PODBRIDGE5_VM_NAME)' PODBRIDGE5_VM_CPUS='$(PODBRIDGE5_VM_CPUS)' PODBRIDGE5_VM_MEMORY='$(PODBRIDGE5_VM_MEMORY)' PODBRIDGE5_VM_DISK='$(PODBRIDGE5_VM_DISK)'; \
		$(MAKE) --no-print-directory vm-prepare-runtime \
			REMOTE_HOST='$(REMOTE_HOST)' REMOTE_USER='$(REMOTE_USER)' REMOTE_PORT='$(REMOTE_PORT)' REMOTE_PASS='$(REMOTE_PASS)' \
			PODBRIDGE5_VM_NAME='$(PODBRIDGE5_VM_NAME)' PODBRIDGE5_VM_REPO='$(PODBRIDGE5_VM_REPO)'; \
		$(MAKE) --no-print-directory vm-sync-runtime \
			REMOTE_HOST='$(REMOTE_HOST)' REMOTE_USER='$(REMOTE_USER)' REMOTE_PORT='$(REMOTE_PORT)' REMOTE_PASS='$(REMOTE_PASS)' \
			PODBRIDGE5_VM_NAME='$(PODBRIDGE5_VM_NAME)' PODBRIDGE5_VM_REPO='$(PODBRIDGE5_VM_REPO)' PODBRIDGE5_LOCAL_REPO='$(PODBRIDGE5_LOCAL_REPO)'; \
		$(MAKE) --no-print-directory vm-run-runtime-integration \
			REMOTE_HOST='$(REMOTE_HOST)' REMOTE_USER='$(REMOTE_USER)' REMOTE_PORT='$(REMOTE_PORT)' REMOTE_PASS='$(REMOTE_PASS)' \
			PODBRIDGE5_VM_NAME='$(PODBRIDGE5_VM_NAME)' PODBRIDGE5_VM_REPO='$(PODBRIDGE5_VM_REPO)'; \
	} 2>&1 | tee "$$log_file"; \
	status=$${PIPESTATUS[0]}; \
	exit $$status
