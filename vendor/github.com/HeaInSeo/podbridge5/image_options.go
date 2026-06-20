package podbridge5

import (
	"fmt"
	"io"
	"os"
	"strconv"
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

type StorageMode string

const (
	StorageVFS           StorageMode = "vfs"
	StorageNativeOverlay StorageMode = "native-overlay"
	StorageFuseOverlay   StorageMode = "fuse-overlay"
)

type BuildIsolation string

const (
	BuildIsolationChroot      BuildIsolation = "chroot"
	BuildIsolationOCI         BuildIsolation = "oci"
	BuildIsolationOCIRootless BuildIsolation = "rootless"
)

type UserNamespaceBuildConfig struct {
	OutputRef                 string
	ContextDirectory          string
	Runtime                   string
	CacheRef                  string
	Isolation                 BuildIsolation
	StorageMode               StorageMode
	RunRoot                   string
	GraphRoot                 string
	FuseOverlayfsMountProgram string
	RegistryCertDirPath       string
	// InsecureSkipTLSVerify disables TLS verification when pushing/pulling
	// against the configured registry (e.g. a plain-HTTP in-cluster cache).
	// Defaults to false (verify TLS), matching prior behavior.
	InsecureSkipTLSVerify bool
}

type UserNamespaceExecutionProfile struct {
	ActualEUID          int
	ActualEGID          int
	UserNamespace       bool
	StorageMode         StorageMode
	StorageDriver       string
	GraphRoot           string
	RunRoot             string
	MountProgram        string
	Isolation           string
	Runtime             string
	BuildahVersion      string
	RegistryCertDirPath string
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

func UserNamespaceStoreOptions(config UserNamespaceBuildConfig) (storage.StoreOptions, error) {
	opts := DefaultUserNamespaceStoreOptions()
	if err := WithStoreRoots(config.RunRoot, config.GraphRoot)(&opts); err != nil {
		return storage.StoreOptions{}, err
	}

	switch config.StorageMode {
	case StorageVFS:
		opts.GraphDriverName = "vfs"
		opts.GraphDriverOptions = nil
	case StorageNativeOverlay:
		opts.GraphDriverName = "overlay"
		opts.GraphDriverOptions = nil
	case StorageFuseOverlay:
		opts.GraphDriverName = "overlay"
		opts.GraphDriverOptions = nil
		if err := WithFuseOverlayfsMountProgram(config.FuseOverlayfsMountProgram)(&opts); err != nil {
			return storage.StoreOptions{}, err
		}
	case "":
		return storage.StoreOptions{}, fmt.Errorf("storage mode must be explicit")
	default:
		return storage.StoreOptions{}, fmt.Errorf("unsupported storage mode %q", config.StorageMode)
	}

	return opts, nil
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
	isolation, err := resolveBuildIsolation(config.Isolation)
	if err != nil {
		return define.BuildOptions{}, err
	}
	buildOpts, err := NewImageBuildOptions(
		config.OutputRef,
		WithImageBuildContextDirectory(config.ContextDirectory),
		WithImageBuildRuntime(firstNonEmpty(config.Runtime, DefaultUserNamespaceRuntime)),
		WithImageBuildCache(config.CacheRef),
	)
	if err != nil {
		return define.BuildOptions{}, err
	}
	buildOpts.Isolation = isolation
	buildOpts.SystemContext = UserNamespaceSystemContext(config)
	buildOpts.Layers = true
	buildOpts.RemoveIntermediateCtrs = true
	return buildOpts, nil
}

func UserNamespaceSystemContext(config UserNamespaceBuildConfig) *imageTypes.SystemContext {
	sysCtx := &imageTypes.SystemContext{}
	if certDir := strings.TrimSpace(config.RegistryCertDirPath); certDir != "" {
		sysCtx.DockerPerHostCertDirPath = certDir
	}
	if config.InsecureSkipTLSVerify {
		sysCtx.DockerInsecureSkipTLSVerify = imageTypes.OptionalBoolTrue
	}
	return sysCtx
}

func resolveBuildIsolation(isolation BuildIsolation) (define.Isolation, error) {
	switch isolation {
	case "", BuildIsolationChroot:
		return define.IsolationChroot, nil
	case BuildIsolationOCI:
		return define.IsolationOCI, nil
	case BuildIsolationOCIRootless:
		return define.IsolationOCIRootless, nil
	default:
		return define.IsolationDefault, fmt.Errorf("unsupported build isolation %q", isolation)
	}
}

func DefaultUserNamespaceBuildEnvironment() map[string]string {
	env, err := UserNamespaceBuildEnvironment(UserNamespaceBuildConfig{Isolation: BuildIsolationChroot})
	if err != nil {
		return map[string]string{}
	}
	return env
}

func UserNamespaceBuildEnvironment(config UserNamespaceBuildConfig) (map[string]string, error) {
	isolation, err := resolveBuildIsolation(config.Isolation)
	if err != nil {
		return nil, err
	}
	euid := os.Geteuid()
	egid := os.Getegid()
	return map[string]string{
		ContainersUserNamespaceConfiguredEnv: "done",
		ContainersRootlessUIDEnv:             strconv.Itoa(euid),
		ContainersRootlessGIDEnv:             strconv.Itoa(egid),
		BuildahIsolationEnv:                  isolation.String(),
		BuildahRuntimeEnv:                    DefaultUserNamespaceRuntime,
		HomeEnv:                              DefaultUserNamespaceHome,
		XDGConfigHomeEnv:                     DefaultUserNamespaceHome + "/.config",
		XDGDataHomeEnv:                       DefaultUserNamespaceHome + "/.local/share",
	}, nil
}

func UserNamespaceBuildExecutionProfile(config UserNamespaceBuildConfig) (UserNamespaceExecutionProfile, error) {
	storeOpts, err := UserNamespaceStoreOptions(config)
	if err != nil {
		return UserNamespaceExecutionProfile{}, err
	}
	isolation, err := resolveBuildIsolation(config.Isolation)
	if err != nil {
		return UserNamespaceExecutionProfile{}, err
	}

	return UserNamespaceExecutionProfile{
		ActualEUID:          os.Geteuid(),
		ActualEGID:          os.Getegid(),
		UserNamespace:       true,
		StorageMode:         config.StorageMode,
		StorageDriver:       storeOpts.GraphDriverName,
		GraphRoot:           storeOpts.GraphRoot,
		RunRoot:             storeOpts.RunRoot,
		MountProgram:        overlayMountProgram(storeOpts.GraphDriverOptions),
		Isolation:           isolation.String(),
		Runtime:             firstNonEmpty(config.Runtime, DefaultUserNamespaceRuntime),
		BuildahVersion:      buildah.Version,
		RegistryCertDirPath: strings.TrimSpace(config.RegistryCertDirPath),
	}, nil
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
		WithChmod("755"),
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

func overlayMountProgram(options []string) string {
	const prefix = "overlay.mount_program="
	for _, option := range options {
		if strings.HasPrefix(option, prefix) {
			return strings.TrimPrefix(option, prefix)
		}
	}
	return ""
}
