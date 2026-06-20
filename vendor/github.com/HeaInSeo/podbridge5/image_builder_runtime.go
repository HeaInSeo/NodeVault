package podbridge5

import (
	"context"
	"fmt"

	"go.podman.io/buildah"
	"go.podman.io/storage"
)

type imageBuilder interface {
	Run(command []string, options buildah.RunOptions) error
	Add(dest string, extract bool, options buildah.AddAndCopyOptions, src ...string) error
}

type imageBuilderFactoryRuntime interface {
	Capabilities() ([]string, error)
	NewBuilder(ctx context.Context, store storage.Store, options buildah.BuilderOptions) (*buildah.Builder, error)
}

type realImageBuilderFactoryRuntime struct{}

func (realImageBuilderFactoryRuntime) Capabilities() ([]string, error) {
	return capabilities()
}

func (realImageBuilderFactoryRuntime) NewBuilder(ctx context.Context, store storage.Store, options buildah.BuilderOptions) (*buildah.Builder, error) {
	builder, err := buildah.NewBuilder(ctx, store, options)
	if err != nil {
		return nil, fmt.Errorf("buildah.NewBuilder: %w", err)
	}
	return builder, nil
}

func newBuilderWithRuntime(ctx context.Context, runtime imageBuilderFactoryRuntime, store storage.Store, idName string) (*buildah.Builder, error) {
	caps, err := runtime.Capabilities()
	if err != nil {
		return nil, fmt.Errorf("failed to get capabilities: %w", err)
	}

	builderOpts, err := newBuilderOptions(idName, caps)
	if err != nil {
		return nil, err
	}

	builder, err := runtime.NewBuilder(ctx, store, *builderOpts)
	if err != nil {
		return nil, fmt.Errorf("new builder: %w", err)
	}
	return builder, nil
}

func createDirectoriesWithRuntime(builder imageBuilder, dirs []string) error {
	for _, dir := range dirs {
		if err := builder.Run([]string{"mkdir", "-p", dir}, defaultRunOptions); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}
	return nil
}

func setFilePermissionsWithRuntime(builder imageBuilder, files []string) error {
	chmodArgs := append([]string{"chmod", "755"}, files...)
	if err := builder.Run(chmodArgs, defaultRunOptions); err != nil {
		return fmt.Errorf("failed to set file permissions: %w", err)
	}
	return nil
}

func installDependenciesWithRuntime(builder imageBuilder) error {
	if err := builder.Run([]string{ContainerInstallPath}, defaultRunOptions); err != nil {
		return fmt.Errorf("failed to run install.sh: %w", err)
	}
	return nil
}

func copyScriptsWithRuntime(builder imageBuilder, options buildah.AddAndCopyOptions, scripts map[string][]string) error {
	for dest, srcList := range scripts {
		for _, src := range srcList {
			if err := builder.Add(dest, false, options, src); err != nil {
				return fmt.Errorf("failed to copy script %s to %s: %w", src, dest, err)
			}
		}
	}
	return nil
}
