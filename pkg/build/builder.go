package build

import (
	"context"
	"errors"
	"fmt"

	"github.com/HeaInSeo/podbridge5"
	"go.podman.io/storage"
)

// Builder builds and pushes a container image from Dockerfile content.
// outputRef is the full destination reference, e.g. "harbor.example.com/myimage:latest".
// Build builds the image, pushes it to the registry, and returns the remote digest
// as reported by the registry after push.
type Builder interface {
	Build(ctx context.Context, dockerfileContent, outputRef string) (imageID, digest string, err error)
	Close() error
}

// ErrBuildBackendDisabled is returned by disabledBuilder when a build is attempted.
var ErrBuildBackendDisabled = errors.New("build backend disabled by configuration")

// disabledBuilder is a no-op Builder that always returns ErrBuildBackendDisabled.
// Used when NODEVAULT_BUILD_BACKEND=disabled to allow the gRPC server to start
// without initializing podbridge5 / containers-storage.
type disabledBuilder struct{}

func (disabledBuilder) Build(_ context.Context, _, _ string) (imageID, digest string, err error) {
	return "", "", ErrBuildBackendDisabled
}

func (disabledBuilder) Close() error { return nil }

// podbridge5Builder is NodeVault's in-process image builder.
// podbridge5 wraps the Buildah Go API; it is not a separate service or Pod.
type podbridge5Builder struct {
	store storage.Store
}

// newPodbridge5Builder creates the in-Pod Builder backed by podbridge5/Buildah.
// Storage driver/runroot/graphroot stay sourced from the mounted
// /etc/containers/storage.conf (CONTAINERS_STORAGE_CONF, see
// deploy/03-nodevault.yaml's nodevault-containers-storage ConfigMap) via
// NewStoreWithOptions, so switching storage driver (e.g. vfs <-> overlay
// while userns mount-propagation support is being verified on the cluster)
// remains a ConfigMap/redeploy change, not a code change. The build options
// themselves use podbridge5's user-namespace defaults (chroot isolation,
// crun runtime, --layers) regardless of storage driver.
func newPodbridge5Builder() (Builder, error) {
	store, err := podbridge5.NewStoreWithOptions()
	if err != nil {
		return nil, fmt.Errorf("podbridge5 NewStoreWithOptions: %w", err)
	}
	return &podbridge5Builder{store: store}, nil
}

func (b *podbridge5Builder) Build(
	ctx context.Context, dockerfileContent, outputRef string,
) (imageID, remoteDigest string, err error) {
	// ContextDirectory must not be left at podbridge5's "." default, which
	// resolves to the container root ("/"). Buildah wraps the build context
	// in its own overlay and forces the "userxattr" mount option whenever
	// unshare.IsRootless() is true (vendor/go.podman.io/buildah/pkg/overlay).
	// Every hostUsers:false Pod reports rootless there regardless of granted
	// capabilities, and nesting a userxattr overlay on top of containerd's
	// nouserxattr root overlay fails with EINVAL. /tmp is a separate
	// emptyDir-backed filesystem (see deploy/03-nodevault.yaml), so no
	// nesting occurs.
	cfg := podbridge5.UserNamespaceBuildConfig{OutputRef: outputRef, ContextDirectory: "/tmp"}
	imageID, _, err = podbridge5.BuildDockerfileContentUserNamespace(ctx, b.store, dockerfileContent, cfg)
	if err != nil {
		return "", "", fmt.Errorf("build image: %w", err)
	}
	remoteDigest, err = podbridge5.PushImage(ctx, b.store, outputRef, outputRef)
	if err != nil {
		return "", "", fmt.Errorf("push image: %w", err)
	}
	return imageID, remoteDigest, nil
}

func (b *podbridge5Builder) Close() error {
	_, err := b.store.Shutdown(false)
	return err
}
