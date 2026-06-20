package podbridge5

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
	"go.podman.io/image/v5/transports/alltransports"
	imageTypes "go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

type imageBuildRuntime interface {
	BuildDockerfiles(ctx context.Context, store storage.Store, options define.BuildOptions, dockerfilePath string) (string, string, error)
	PushImage(ctx context.Context, store storage.Store, imageRef, normalizedDestination string, sysCtx *imageTypes.SystemContext) (string, error)
}

type realImageBuildRuntime struct{}

type userNamespaceStoreFactory func(UserNamespaceBuildConfig) (storage.Store, error)

type storeShutdownFunc func(storage.Store, bool) error

func (realImageBuildRuntime) BuildDockerfiles(ctx context.Context, store storage.Store, options define.BuildOptions, dockerfilePath string) (buildID, buildRef string, buildErr error) {
	id, ref, err := imagebuildah.BuildDockerfiles(ctx, store, options, dockerfilePath)
	if err != nil {
		return "", "", fmt.Errorf("imagebuildah.BuildDockerfiles: %w", err)
	}
	if ref == nil {
		return id, "", nil
	}
	return id, ref.Digest().String(), nil
}

func (realImageBuildRuntime) PushImage(ctx context.Context, store storage.Store, imageRef, normalizedDestination string, sysCtx *imageTypes.SystemContext) (string, error) {
	destRef, err := alltransports.ParseImageName(normalizedDestination)
	if err != nil {
		return "", fmt.Errorf("parse destination %q: %w", normalizedDestination, err)
	}
	if sysCtx == nil {
		sysCtx = &imageTypes.SystemContext{}
	}

	_, manifestDigest, err := buildah.Push(ctx, imageRef, destRef, buildah.PushOptions{
		Store:         store,
		SystemContext: sysCtx,
	})
	if err != nil {
		return "", fmt.Errorf("buildah.Push: %w", err)
	}
	return manifestDigest.String(), nil
}

func writeDockerfileTempFile(dockerfileContent string) (filePath string, cleanupFn func(), buildErr error) {
	tmpFile, err := os.CreateTemp("", "nodeforge-dockerfile-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp Dockerfile: %w", err)
	}
	cleanup := func() {
		_ = os.Remove(tmpFile.Name())
	}
	if _, err := tmpFile.WriteString(dockerfileContent); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return "", nil, fmt.Errorf("failed to write Dockerfile content: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to close temp Dockerfile: %w", err)
	}
	return tmpFile.Name(), cleanup, nil
}

func buildDockerfileContentWithRuntime(ctx context.Context, runtime imageBuildRuntime, store storage.Store, dockerfileContent, outputRef string) (imageID, digestStr string, err error) {
	buildOpts := DefaultImageBuildOptions(outputRef)
	return buildDockerfileContentWithOptionsRuntime(ctx, runtime, store, dockerfileContent, buildOpts)
}

func buildDockerfileContentWithOptionsRuntime(ctx context.Context, runtime imageBuildRuntime, store storage.Store, dockerfileContent string, buildOpts define.BuildOptions) (imageID, digestStr string, err error) {
	if ctx == nil {
		return "", "", fmt.Errorf("ctx must not be nil")
	}
	if strings.TrimSpace(dockerfileContent) == "" {
		return "", "", fmt.Errorf("dockerfileContent must not be empty")
	}

	dockerfilePath, cleanup, err := writeDockerfileTempFile(dockerfileContent)
	if err != nil {
		return "", "", err
	}
	defer cleanup()

	id, digestStr, err := runtime.BuildDockerfiles(ctx, store, buildOpts, dockerfilePath)
	if err != nil {
		return "", "", fmt.Errorf("imagebuildah.BuildDockerfiles: %w", annotateBuildahExecutionError(err))
	}
	return id, digestStr, nil
}

func annotateBuildahExecutionError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unshare") || strings.Contains(msg, "clone_newuser"):
		return fmt.Errorf("%w; hint: Buildah failed while creating a user namespace. In Kubernetes hostUsers:false pods, prefer explicit OCI isolation when nested user namespaces are unnecessary; otherwise verify AppArmor allows unprivileged user namespaces and the pod has the required capabilities", err)
	case strings.Contains(msg, "setgroups"):
		return fmt.Errorf("%w; hint: Buildah failed while configuring supplementary groups. Check user namespace gid mappings, /proc/self/setgroups policy, and whether the pod runtime blocks nested user namespace setup", err)
	case strings.Contains(msg, "setcap") || strings.Contains(msg, "set file capabilities"):
		return fmt.Errorf("%w; hint: Buildah failed while setting file capabilities. Verify CAP_SETFCAP is present, the filesystem supports file capabilities, and the active security profile is not denying setcap", err)
	default:
		return err
	}
}

func pushImageWithRuntime(ctx context.Context, runtime imageBuildRuntime, store storage.Store, imageRef, destination string) (string, error) {
	return pushImageWithSystemContextRuntime(ctx, runtime, store, imageRef, destination, nil)
}

func pushImageWithSystemContextRuntime(ctx context.Context, runtime imageBuildRuntime, store storage.Store, imageRef, destination string, sysCtx *imageTypes.SystemContext) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("ctx must not be nil")
	}
	if strings.TrimSpace(imageRef) == "" {
		return "", fmt.Errorf("imageRef must not be empty")
	}
	if strings.TrimSpace(destination) == "" {
		return "", fmt.Errorf("destination must not be empty")
	}

	normalizedDestination, err := NormalizePushDestination(destination)
	if err != nil {
		return "", err
	}

	digestStr, err := runtime.PushImage(ctx, store, imageRef, normalizedDestination, sysCtx)
	if err != nil {
		return "", fmt.Errorf("push image %q to %q: %w", imageRef, destination, err)
	}
	return digestStr, nil
}

func buildAndPushDockerfileContentWithRuntime(ctx context.Context, runtime imageBuildRuntime, store storage.Store, dockerfileContent, outputRef string) (imageID, digestStr string, err error) {
	imageID, _, err = buildDockerfileContentWithRuntime(ctx, runtime, store, dockerfileContent, outputRef)
	if err != nil {
		return "", "", err
	}

	digestStr, err = pushImageWithRuntime(ctx, runtime, store, outputRef, outputRef)
	if err != nil {
		return "", "", err
	}
	return imageID, digestStr, nil
}

func buildAndPushUserNamespaceWithRuntime(ctx context.Context, runtime imageBuildRuntime, storeFactory userNamespaceStoreFactory, shutdownFn storeShutdownFunc, config UserNamespaceBuildConfig, dockerfileContent string) (imageID, digestStr string, err error) {
	if strings.TrimSpace(config.OutputRef) == "" {
		return "", "", fmt.Errorf("output ref must not be empty")
	}

	store, err := storeFactory(config)
	if err != nil {
		return "", "", err
	}
	defer func() {
		shutdownErr := shutdownFn(store, false)
		if err == nil && shutdownErr != nil {
			err = shutdownErr
		}
	}()

	buildOpts, err := UserNamespaceImageBuildOptions(config)
	if err != nil {
		return "", "", err
	}
	imageID, _, err = buildDockerfileContentWithOptionsRuntime(ctx, runtime, store, dockerfileContent, buildOpts)
	if err != nil {
		return "", "", err
	}

	digestStr, err = pushImageWithSystemContextRuntime(ctx, runtime, store, config.OutputRef, config.OutputRef, UserNamespaceSystemContext(config))
	if err != nil {
		return "", "", err
	}
	return imageID, digestStr, nil
}
