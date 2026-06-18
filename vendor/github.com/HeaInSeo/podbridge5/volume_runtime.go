package podbridge5

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.podman.io/podman/v6/pkg/bindings/containers"
	"go.podman.io/podman/v6/pkg/bindings/images"
	"go.podman.io/podman/v6/pkg/specgen"
)

const (
	volumeTransferImage             = "docker.io/library/alpine:latest"
	volumeWriterContainerNamePrefix = "temp-folder-writer"
	volumeReaderContainerNamePrefix = "temp-data-reader"
)

type volumeContainerRuntime interface {
	EnsureImage(ctx context.Context, imageRef string) error
	CreateContainer(ctx context.Context, spec *specgen.SpecGenerator) (string, error)
	StartContainer(ctx context.Context, containerID string) error
	StopContainer(ctx context.Context, containerID string) error
	RemoveContainer(ctx context.Context, containerID string) error
}

type podmanVolumeContainerRuntime struct{}

func (podmanVolumeContainerRuntime) EnsureImage(ctx context.Context, imageRef string) error {
	exists, err := images.Exists(ctx, imageRef, nil)
	if err != nil {
		return fmt.Errorf("image exists check: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := images.Pull(ctx, imageRef, &images.PullOptions{}); err != nil {
		return fmt.Errorf("image pull: %w", err)
	}
	return nil
}

func (podmanVolumeContainerRuntime) CreateContainer(ctx context.Context, spec *specgen.SpecGenerator) (string, error) {
	resp, err := containers.CreateWithSpec(ctx, spec, nil)
	if err != nil {
		return "", fmt.Errorf("container create: %w", err)
	}
	return resp.ID, nil
}

func (podmanVolumeContainerRuntime) StartContainer(ctx context.Context, containerID string) error {
	if err := containers.Start(ctx, containerID, nil); err != nil {
		return fmt.Errorf("container start: %w", err)
	}
	return nil
}

func (podmanVolumeContainerRuntime) StopContainer(ctx context.Context, containerID string) error {
	if err := containers.Stop(ctx, containerID, nil); err != nil {
		return fmt.Errorf("container stop: %w", err)
	}
	return nil
}

func (podmanVolumeContainerRuntime) RemoveContainer(ctx context.Context, containerID string) error {
	if _, err := containers.Remove(ctx, containerID, nil); err != nil {
		return fmt.Errorf("container remove: %w", err)
	}
	return nil
}

func newVolumeWriterSpec(volumeName, mountPath string) (*specgen.SpecGenerator, error) {
	return NewSpec(
		WithImageName(volumeTransferImage),
		WithName(uniqueTempContainerName(volumeWriterContainerNamePrefix)),
		WithEnv("MOUNT", mountPath),
		WithCommand([]string{
			"sh", "-c",
			"mkdir -p \"$MOUNT\"; exec tail -f /dev/null",
		}),
		WithNamedVolume(volumeName, mountPath, ""),
	)
}

func newVolumeReaderSpec(volumeName, mountPath string) (*specgen.SpecGenerator, error) {
	return NewSpec(
		WithImageName(volumeTransferImage),
		WithName(uniqueTempContainerName(volumeReaderContainerNamePrefix)),
		WithCommand([]string{"sh", "-c", "mkdir -p /data && sleep infinity"}),
		WithNamedVolume(volumeName, mountPath, ""),
	)
}

func startVolumeContainer(ctx context.Context, runtime volumeContainerRuntime, spec *specgen.SpecGenerator) (containerID string, cleanupFn func(), startErr error) {
	if err := runtime.EnsureImage(ctx, spec.Image); err != nil {
		return "", nil, fmt.Errorf("ensure image %q: %w", spec.Image, err)
	}

	cID, err := runtime.CreateContainer(ctx, spec)
	if err != nil {
		return "", nil, fmt.Errorf("create container: %w", err)
	}

	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if stopErr := runtime.StopContainer(cleanupCtx, cID); stopErr != nil {
			Log.Warnf("stop container %s: %v", cID, stopErr)
		}
		if rmErr := runtime.RemoveContainer(cleanupCtx, cID); rmErr != nil {
			Log.Warnf("remove container %s: %v", cID, rmErr)
		}
	}

	if err := runtime.StartContainer(ctx, cID); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("start container %q: %w", cID, err)
	}

	return cID, cleanup, nil
}

func uniqueTempContainerName(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, uuid.NewString())
}
