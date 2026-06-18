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
	PushImage(ctx context.Context, store storage.Store, imageRef, normalizedDestination string) (string, error)
}

type realImageBuildRuntime struct{}

func (realImageBuildRuntime) BuildDockerfiles(ctx context.Context, store storage.Store, options define.BuildOptions, dockerfilePath string) (buildID, buildRef string, buildErr error) {
	id, ref, err := imagebuildah.BuildDockerfiles(ctx, store, options, dockerfilePath)
	if err != nil {
		return "", "", err
	}
	if ref == nil {
		return id, "", nil
	}
	return id, ref.Digest().String(), nil
}

func (realImageBuildRuntime) PushImage(ctx context.Context, store storage.Store, imageRef, normalizedDestination string) (string, error) {
	destRef, err := alltransports.ParseImageName(normalizedDestination)
	if err != nil {
		return "", fmt.Errorf("parse destination %q: %w", normalizedDestination, err)
	}

	_, manifestDigest, err := buildah.Push(ctx, imageRef, destRef, buildah.PushOptions{
		Store: store,
		SystemContext: &imageTypes.SystemContext{
			DockerInsecureSkipTLSVerify: imageTypes.OptionalBoolFalse,
		},
	})
	if err != nil {
		return "", err
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
		return "", "", fmt.Errorf("imagebuildah.BuildDockerfiles: %w", err)
	}
	return id, digestStr, nil
}

func pushImageWithRuntime(ctx context.Context, runtime imageBuildRuntime, store storage.Store, imageRef, destination string) (string, error) {
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

	digestStr, err := runtime.PushImage(ctx, store, imageRef, normalizedDestination)
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
