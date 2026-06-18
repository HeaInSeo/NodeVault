package podbridge5

import (
	"context"
	"fmt"

	"go.podman.io/buildah"
	is "go.podman.io/image/v5/storage"
	imageTypes "go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

type configuredBuilder interface {
	imageBuilder
	SetWorkDir(string)
	SetCmd([]string)
}

func applyImageConfigToBuilder(builder configuredBuilder, config ImageConfig) error {
	if err := createDirectoriesWithRuntime(builder, config.Directories); err != nil {
		return fmt.Errorf("failed to create directories: %w", err)
	}
	if err := copyScriptsWithRuntime(builder, newAddAndCopyOptions(), config.ScriptMap); err != nil {
		return fmt.Errorf("failed to copy scripts: %w", err)
	}
	if err := setFilePermissionsWithRuntime(builder, config.PermissionFiles); err != nil {
		return fmt.Errorf("failed to set file permissions: %w", err)
	}
	if err := installDependenciesWithRuntime(builder); err != nil {
		return fmt.Errorf("failed to install dependency: %w", err)
	}
	builder.SetWorkDir(config.WorkDir)
	builder.SetCmd(config.CMD)
	return nil
}

func applyContainerConfigToBuilder(builder configuredBuilder, config ContainerConfig) error {
	if err := createDirectoriesWithRuntime(builder, config.Directories); err != nil {
		return fmt.Errorf("failed to create directories: %w", err)
	}
	if err := copyScriptsWithRuntime(builder, newAddAndCopyOptions(), config.ScriptMap); err != nil {
		return fmt.Errorf("failed to copy scripts: %w", err)
	}
	if err := setFilePermissionsWithRuntime(builder, config.PermissionFiles); err != nil {
		return fmt.Errorf("failed to set file permissions: %w", err)
	}
	if err := installDependenciesWithRuntime(builder); err != nil {
		return fmt.Errorf("failed to install dependencies: %w", err)
	}
	builder.SetWorkDir(config.WorkDir)
	builder.SetCmd(config.Cmd)
	return nil
}

func commitAndSaveBuilderImage(ctx context.Context, builder *buildah.Builder, imageName, imageSavePath string) (string, error) {
	imageRef, err := is.Transport.ParseReference(imageName)
	if err != nil {
		return "", fmt.Errorf("failed to parse image reference: %w", err)
	}

	imageID, _, _, err := builder.Commit(ctx, imageRef, buildah.CommitOptions{
		PreferredManifestType: buildah.Dockerv2ImageManifest,
		SystemContext:         &imageTypes.SystemContext{},
	})
	if err != nil {
		return "", fmt.Errorf("failed to commit image: %w", err)
	}

	if err := saveImage(ctx, imageSavePath, imageName, imageID, false); err != nil {
		return imageID, fmt.Errorf("failed to save image: %w", err)
	}
	return imageID, nil
}

func createConfiguredBuilder(ctx context.Context, store storage.Store, baseImage string, config ImageConfig) (*buildah.Builder, error) {
	builder, err := newBuilder(ctx, store, baseImage)
	if err != nil {
		return nil, fmt.Errorf("failed to create new builder: %w", err)
	}
	if err := applyImageConfigToBuilder(builder, config); err != nil {
		return builder, err
	}
	return builder, nil
}
