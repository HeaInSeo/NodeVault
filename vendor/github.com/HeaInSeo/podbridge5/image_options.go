package podbridge5

import (
	"fmt"
	"io"
	"strings"

	"go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/docker/reference"
	imageTypes "go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

const (
	DefaultUserNamespaceStorageRoot = "/storage"
	DefaultUserNamespaceRunRoot     = DefaultUserNamespaceStorageRoot + "/run/containers/storage"
	DefaultUserNamespaceGraphRoot   = DefaultUserNamespaceStorageRoot + "/.local/share/containers/storage"
	DefaultUserNamespaceRuntime     = "crun"
	DefaultUserNamespaceHome        = "/tmp/buildhome"

	ContainersUserNamespaceConfiguredEnv = "_CONTAINERS_USERNS_CONFIGURED"
	ContainersRootlessUIDEnv             = "_CONTAINERS_ROOTLESS_UID"
	ContainersRootlessGIDEnv             = "_CONTAINERS_ROOTLESS_GID"
	BuildahIsolationEnv                  = "BUILDAH_ISOLATION"
	BuildahRuntimeEnv                    = "BUILDAH_RUNTIME"
	HomeEnv                              = "HOME"
	XDGConfigHomeEnv                     = "XDG_CONFIG_HOME"
	XDGDataHomeEnv                       = "XDG_DATA_HOME"
)

type StoreOption func(*storage.StoreOptions) error

type ImageBuildOption func(*define.BuildOptions) error

type UserNamespaceBuildConfig struct {
	OutputRef        string
	ContextDirectory string
	Runtime          string
	CacheRef         string
}

func DefaultImageBuildOptions(outputRef string) define.BuildOptions {
	return define.BuildOptions{
		ContextDirectory: ".",
		PullPolicy:       define.PullIfMissing,
		Isolation:        define.IsolationOCI,
		SystemContext:    &imageTypes.SystemContext{},
		Output:           outputRef,
		OutputFormat:     buildah.Dockerv2ImageManifest,
	}
}

func DefaultUserNamespaceStoreOptions() storage.StoreOptions {
	return storage.StoreOptions{
		RunRoot:         DefaultUserNamespaceRunRoot,
		GraphRoot:       DefaultUserNamespaceGraphRoot,
		GraphDriverName: "overlay",
		PullOptions: map[string]string{
			"enable_partial_images": "true",
		},
	}
}

func WithStoreRoots(runRoot, graphRoot string) StoreOption {
	return func(opts *storage.StoreOptions) error {
		if strings.TrimSpace(runRoot) != "" {
			opts.RunRoot = runRoot
		}
		if strings.TrimSpace(graphRoot) != "" {
			opts.GraphRoot = graphRoot
		}
		return nil
	}
}

func WithStoreDriver(driver string) StoreOption {
	return func(opts *storage.StoreOptions) error {
		if strings.TrimSpace(driver) != "" {
			opts.GraphDriverName = driver
		}
		return nil
	}
}

func WithFuseOverlayfsMountProgram(path string) StoreOption {
	return func(opts *storage.StoreOptions) error {
		mountProgram := strings.TrimSpace(path)
		if mountProgram == "" {
			mountProgram = "/usr/bin/fuse-overlayfs"
		}
		option := "overlay.mount_program=" + mountProgram
		if !utilsContainsString(opts.GraphDriverOptions, option) {
			opts.GraphDriverOptions = append(opts.GraphDriverOptions, option)
		}
		return nil
	}
}

func WithPartialImagePulls(enabled bool) StoreOption {
	return func(opts *storage.StoreOptions) error {
		if opts.PullOptions == nil {
			opts.PullOptions = map[string]string{}
		}
		if enabled {
			opts.PullOptions["enable_partial_images"] = "true"
		} else {
			delete(opts.PullOptions, "enable_partial_images")
		}
		return nil
	}
}

func WithImageBuildContextDirectory(contextDirectory string) ImageBuildOption {
	return func(opts *define.BuildOptions) error {
		if strings.TrimSpace(contextDirectory) != "" {
			opts.ContextDirectory = contextDirectory
		}
		return nil
	}
}

func WithImageBuildRuntime(runtime string) ImageBuildOption {
	return func(opts *define.BuildOptions) error {
		if strings.TrimSpace(runtime) != "" {
			opts.Runtime = runtime
		}
		return nil
	}
}

func WithImageBuildCache(cacheRef string) ImageBuildOption {
	return func(opts *define.BuildOptions) error {
		trimmed := strings.TrimSpace(cacheRef)
		if trimmed == "" {
			return nil
		}
		named, err := reference.ParseNormalizedNamed(trimmed)
		if err != nil {
			return fmt.Errorf("parse cache ref %q: %w", cacheRef, err)
		}
		opts.CacheFrom = append(opts.CacheFrom, named)
		opts.CacheTo = append(opts.CacheTo, named)
		opts.Layers = true
		return nil
	}
}

func NewImageBuildOptions(outputRef string, opts ...ImageBuildOption) (define.BuildOptions, error) {
	buildOpts := DefaultImageBuildOptions(outputRef)
	for _, applyOpt := range opts {
		if err := applyOpt(&buildOpts); err != nil {
			return define.BuildOptions{}, err
		}
	}
	return buildOpts, nil
}

func UserNamespaceImageBuildOptions(config UserNamespaceBuildConfig) (define.BuildOptions, error) {
	buildOpts, err := NewImageBuildOptions(
		config.OutputRef,
		WithImageBuildContextDirectory(config.ContextDirectory),
		WithImageBuildRuntime(firstNonEmpty(config.Runtime, DefaultUserNamespaceRuntime)),
		WithImageBuildCache(config.CacheRef),
	)
	if err != nil {
		return define.BuildOptions{}, err
	}
	buildOpts.Isolation = define.IsolationChroot
	buildOpts.Layers = true
	return buildOpts, nil
}

func DefaultUserNamespaceBuildEnvironment() map[string]string {
	return map[string]string{
		ContainersUserNamespaceConfiguredEnv: "done",
		ContainersRootlessUIDEnv:             "1000",
		ContainersRootlessGIDEnv:             "1000",
		BuildahIsolationEnv:                  "chroot",
		BuildahRuntimeEnv:                    DefaultUserNamespaceRuntime,
		HomeEnv:                              DefaultUserNamespaceHome,
		XDGConfigHomeEnv:                     DefaultUserNamespaceHome + "/.config",
		XDGDataHomeEnv:                       DefaultUserNamespaceHome + "/.local/share",
	}
}

func DefaultUserNamespaceBuildCapabilities() []string {
	return []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETFCAP", "SYS_CHROOT"}
}

func NormalizePushDestination(destination string) (string, error) {
	trimmed := strings.TrimSpace(destination)
	if trimmed == "" {
		return "", fmt.Errorf("destination must not be empty")
	}
	if strings.Contains(trimmed, "://") {
		return trimmed, nil
	}
	return "docker://" + trimmed, nil
}

func newBuilderOptions(baseImage string, caps []string) (*buildah.BuilderOptions, error) {
	if strings.TrimSpace(baseImage) == "" {
		return nil, fmt.Errorf("from image cannot be empty")
	}

	return &buildah.BuilderOptions{
		FromImage:        baseImage,
		Isolation:        define.IsolationOCI,
		CommonBuildOpts:  &buildah.CommonBuildOptions{},
		SystemContext:    &imageTypes.SystemContext{},
		ConfigureNetwork: buildah.NetworkDefault,
		Format:           buildah.Dockerv2ImageManifest,
		Capabilities:     append([]string(nil), caps...),
	}, nil
}

func newDefaultAddAndCopyOptions(hasher io.Writer) buildah.AddAndCopyOptions {
	return newDefaultAddAndCopyOptionsWithDryRun(hasher, DefaultAddAndCopyDryRun)
}

func newDefaultAddAndCopyOptionsWithDryRun(hasher io.Writer, dryRun bool) buildah.AddAndCopyOptions {
	return NewAddAndCopyOptions(
		WithChmod("0o755"),
		WithChown("0:0"),
		WithHasher(hasher),
		WithContextDir("."),
		WithDryRun(dryRun),
	)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func utilsContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
