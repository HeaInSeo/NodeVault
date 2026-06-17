package build

import (
	"context"
	"errors"
	"fmt"

	"github.com/HeaInSeo/podbridge5"
	"github.com/containers/storage"
)

// Builder builds and pushes a container image from Dockerfile content.
// outputRef is the full destination reference, e.g. "harbor.example.com/myimage:latest".
// Build builds the image, pushes it to the registry, and returns the remote digest
// as reported by the registry after push. nanVersion is the version of the nan
// (node-artifact-runtime) binary injected into the image, or "" if the backend
// does not inject nan (e.g. podbridge5Builder, disabledBuilder).
type Builder interface {
	Build(ctx context.Context, dockerfileContent, outputRef string) (imageID, digest, nanVersion string, err error)
	Close() error
}

// ErrBuildBackendDisabled is returned by disabledBuilder when a build is attempted.
var ErrBuildBackendDisabled = errors.New("build backend disabled by configuration")

// disabledBuilder is a no-op Builder that always returns ErrBuildBackendDisabled.
// Used when NODEVAULT_BUILD_BACKEND=disabled to allow the gRPC server to start
// without initializing podbridge5 / containers-storage.
type disabledBuilder struct{}

func (disabledBuilder) Build(_ context.Context, _, _ string) (imageID, digest, nanVersion string, err error) {
	return "", "", "", ErrBuildBackendDisabled
}

func (disabledBuilder) Close() error { return nil }

// podbridge5Builder is NodeVault's in-process image builder.
// podbridge5 wraps the Buildah Go API; it is not a separate service or Pod.
type podbridge5Builder struct {
	store storage.Store
}

// newPodbridge5Builder creates the in-Pod Builder backed by podbridge5/Buildah.
func newPodbridge5Builder() (Builder, error) {
	store, err := podbridge5.NewStore()
	if err != nil {
		return nil, fmt.Errorf("podbridge5 NewStore: %w", err)
	}
	return &podbridge5Builder{store: store}, nil
}

// Build does not inject nan yet. nanVersion remains empty until the Tool Image
// generation path explicitly includes nan as part of the generated build context.
func (b *podbridge5Builder) Build(
	ctx context.Context, dockerfileContent, outputRef string,
) (imageID, remoteDigest, nanVersion string, err error) {
	imageID, remoteDigest, err = podbridge5.BuildAndPushDockerfileContent(ctx, b.store, dockerfileContent, outputRef)
	if err != nil {
		return "", "", "", fmt.Errorf("build and push image: %w", err)
	}
	return imageID, remoteDigest, "", nil
}

func (b *podbridge5Builder) Close() error {
	_, err := b.store.Shutdown(false)
	return err
}
