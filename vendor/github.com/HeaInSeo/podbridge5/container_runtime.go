package podbridge5

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/HeaInSeo/utils"
	"go.podman.io/podman/v6/libpod/define"
	"go.podman.io/podman/v6/pkg/bindings/containers"
	"go.podman.io/podman/v6/pkg/bindings/images"
	entitiesTypes "go.podman.io/podman/v6/pkg/domain/entities/types"
	"go.podman.io/podman/v6/pkg/specgen"
)

type containerRuntime interface {
	ContainerExists(ctx context.Context, name string) (bool, error)
	EnsureImage(ctx context.Context, imageRef string) error
	CreateContainer(ctx context.Context, spec *specgen.SpecGenerator) (*entitiesTypes.ContainerCreateResponse, error)
	StartContainer(ctx context.Context, containerID string) error
	RemoveContainer(ctx context.Context, containerID string) error
	InspectContainer(ctx context.Context, containerID string) (*define.InspectContainerData, error)
}

var ErrContainerAlreadyExists = errors.New("container already exists")

type podmanContainerRuntime struct{}

func (podmanContainerRuntime) ContainerExists(ctx context.Context, name string) (bool, error) {
	exists, err := containers.Exists(ctx, name, &containers.ExistsOptions{External: utils.PFalse})
	if err != nil {
		return false, fmt.Errorf("failed to check if container exists: %w", err)
	}
	return exists, nil
}

func (podmanContainerRuntime) EnsureImage(ctx context.Context, imageRef string) error {
	exists, err := images.Exists(ctx, imageRef, nil)
	if err != nil {
		return fmt.Errorf("failed to check if image exists: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := images.Pull(ctx, imageRef, &images.PullOptions{}); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}
	return nil
}

func (podmanContainerRuntime) CreateContainer(ctx context.Context, spec *specgen.SpecGenerator) (*entitiesTypes.ContainerCreateResponse, error) {
	createResponse, err := containers.CreateWithSpec(ctx, spec, &containers.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}
	return &createResponse, nil
}

func (podmanContainerRuntime) StartContainer(ctx context.Context, containerID string) error {
	if err := containers.Start(ctx, containerID, &containers.StartOptions{}); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	return nil
}

func (podmanContainerRuntime) RemoveContainer(ctx context.Context, containerID string) error {
	if _, err := containers.Remove(ctx, containerID, &containers.RemoveOptions{Force: utils.PTrue}); err != nil {
		return fmt.Errorf("remove container %q: %w", containerID, err)
	}
	return nil
}

func (podmanContainerRuntime) InspectContainer(ctx context.Context, containerID string) (*define.InspectContainerData, error) {
	data, err := containers.Inspect(ctx, containerID, &containers.InspectOptions{Size: utils.PFalse})
	if err != nil {
		return nil, fmt.Errorf("inspect container %q: %w", containerID, err)
	}
	return data, nil
}

func startContainerWithRuntime(ctx context.Context, runtime containerRuntime, spec *specgen.SpecGenerator) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("context is nil")
	}
	if spec == nil {
		return "", fmt.Errorf("spec is nil")
	}

	ccr, err := createContainerWithRuntime(ctx, runtime, spec)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	if err := runtime.StartContainer(ctx, ccr.ID); err != nil {
		cleanupCtx := context.Background()
		if removeErr := runtime.RemoveContainer(cleanupCtx, ccr.ID); removeErr != nil {
			return "", errors.Join(fmt.Errorf("start container: %w", err), removeErr)
		}
		return "", fmt.Errorf("start container: %w", err)
	}

	return ccr.ID, nil
}

// createContainerWithRuntime implements the create-if-absent contract: it
// returns ErrContainerAlreadyExists rather than reusing an existing
// container with the same name.
func createContainerWithRuntime(ctx context.Context, runtime containerRuntime, conSpec *specgen.SpecGenerator) (*CreateContainerResult, error) {
	if err := conSpec.Validate(); err != nil {
		Log.Errorf("validation failed: %v", err)
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	if utils.IsEmptyString(conSpec.Name) || utils.IsEmptyString(conSpec.Image) {
		Log.Error("Container's name or image's name is not set")
		return nil, fmt.Errorf("container name or image's name is not set")
	}

	containerExists, err := runtime.ContainerExists(ctx, conSpec.Name)
	if err != nil {
		Log.Errorf("Failed to check if container exists: %v", err)
		return nil, fmt.Errorf("check container exists: %w", err)
	}
	if containerExists {
		info, inspectErr := runtime.InspectContainer(ctx, conSpec.Name)
		if inspectErr != nil {
			return nil, fmt.Errorf("inspect container: %w", inspectErr)
		}
		return nil, fmt.Errorf("%w: name=%s existing_id=%s existing_image=%s expected_image=%s", ErrContainerAlreadyExists, conSpec.Name, info.ID, info.Image, conSpec.Image)
	}

	err = runtime.EnsureImage(ctx, conSpec.Image)
	if err != nil {
		Log.Errorf("Failed to ensure image: %v", err)
		return nil, fmt.Errorf("ensure image %q: %w", conSpec.Image, err)
	}

	Log.Infof("Creating %s container using %s image...", conSpec.Name, conSpec.Image)
	createResponse, err := runtime.CreateContainer(ctx, conSpec)
	if err != nil {
		Log.Errorf("Failed to create container: %v", err)
		return nil, fmt.Errorf("create container: %w", err)
	}

	return &CreateContainerResult{
		Name:     conSpec.Name,
		ID:       createResponse.ID,
		Warnings: createResponse.Warnings,
		Status:   Created,
	}, nil
}

func inspectContainerWithRuntime(ctx context.Context, runtime containerRuntime, containerID string) (*define.InspectContainerData, error) {
	data, err := runtime.InspectContainer(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("inspect container %q: %w", containerID, err)
	}
	return data, nil
}

func containerStatusFromInspectState(state *define.InspectContainerState) ContainerStatus {
	if state == nil {
		return Created
	}

	switch {
	case state.Running:
		return Running
	case state.Paused:
		return Paused
	case state.Dead:
		return Dead
	case strings.EqualFold(state.Status, "created") || strings.EqualFold(state.Status, "configured"):
		return Created
	case state.ExitCode >= 0:
		if state.ExitCode == 0 {
			return Exited
		}
		return ExitedErr
	default:
		return Created
	}
}
