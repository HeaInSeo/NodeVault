# ── Stage 1: Go 빌드 ──────────────────────────────────────────────────────────
# podbridge5가 사용하는 컨테이너 빌드 라이브러리는 CGO를 사용하므로
# CGO_ENABLED=1 + 관련 C 헤더가 필요하다.
#
# 컨테이너 빌드 전에 반드시 `make vendor` (= go mod vendor) 를 실행해야 한다.
# vendor/ 디렉토리가 없으면 -mod=vendor 빌드가 실패한다.
FROM quay.io/buildah/stable:latest AS builder

ENV GO_VERSION=1.26.6
# gpgme-devel/libassuan-devel: go.podman.io/image/v5's default (cgo-based)
# signature mechanism needs these headers now that containers_image_openpgp
# is no longer set below (#29) — without the tag it would otherwise fall
# through to the pure-Go x/crypto/openpgp implementation NodeVault doesn't
# need. Matches the "Ensure CGo system dependencies" step in ci.yml.
RUN dnf install -y gcc gpgme-devel libassuan-devel && \
    curl -fsSL https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz \
    | tar -C /usr/local -xz

ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOPATH=/opt/go
ENV CGO_ENABLED=1

WORKDIR /src

# go.mod 먼저 복사 (vendor와 함께 캐시 활용)
COPY go.mod ./
# vendor/ 가 있어야 -mod=vendor 빌드 가능 (`make vendor` 선행 필요)
COPY vendor/ ./vendor/
COPY . .

RUN go build \
    -mod=vendor \
    -tags "exclude_graphdriver_btrfs exclude_graphdriver_devicemapper" \
    -ldflags="-s -w" \
    -o /bin/nodevault \
    ./cmd/controlplane/...

# ── Stage 2: 런타임 이미지 ────────────────────────────────────────────────────
# 런타임 이미지 자체가 NodeVault의 in-Pod Buildah 실행 환경이다.
FROM quay.io/buildah/stable:latest

COPY --from=builder /bin/nodevault /usr/local/bin/nodevault

EXPOSE 50051

ENTRYPOINT ["/usr/local/bin/nodevault"]
