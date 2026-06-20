package podbridge5

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/HeaInSeo/utils"
	"github.com/opencontainers/go-digest"
	"go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/common/pkg/config"
	imageTypes "go.podman.io/image/v5/types"
	"go.podman.io/podman/v6/pkg/bindings/images"
	"go.podman.io/podman/v6/pkg/domain/entities/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/unshare"
)

var (
	digester = digest.Canonical.Digester()

	defaultRunOptions = buildah.RunOptions{
		User:      "root",
		Isolation: define.IsolationOCI,
		Runtime:   "runc",
	}

	// 사용하지 않음 주석처리함. 삭제하지 않음.
	/*	Verbose = true
		Debug   = true*/
)

// ------------------------------------------------------
// Functional Options for buildah.BuilderOptions
// ------------------------------------------------------

type BuilderOption func(*buildah.BuilderOptions) error

// WithArg sets an argument for the build. 함수 수정: 에러 발생 시 이를 반환
func WithArg(key, value string) BuilderOption {
	return func(opts *buildah.BuilderOptions) error {
		if opts.Args == nil {
			opts.Args = make(map[string]string)
		}
		if _, ok := opts.Args[key]; !ok {
			opts.Args[key] = value
		}
		return nil
	}
}

// WithFromImage sets the base image for the build. 함수 수정: 에러 발생 시 이를 반환
func WithFromImage(image string) BuilderOption {
	return func(opts *buildah.BuilderOptions) error {
		if utils.IsEmptyString(image) {
			return fmt.Errorf("from image cannot be empty")
		}
		opts.FromImage = image
		return nil
	}
}

// WithIsolation sets the isolation mode for the builder options. 함수 수정: 에러 발생 시 이를 반환
func WithIsolation(isolation define.Isolation) BuilderOption {
	return func(opts *buildah.BuilderOptions) error {
		opts.Isolation = isolation
		return nil
	}
}

// WithCommonBuildOptions sets the common build options such as CPU and memory limits. 함수 수정: 에러 발생 시 이를 반환
// TODO 확인하자.
func WithCommonBuildOptions(options *buildah.CommonBuildOptions) BuilderOption {
	return func(opts *buildah.BuilderOptions) error {
		if options != nil {
			opts.CommonBuildOpts = options
		} else {
			opts.CommonBuildOpts = &buildah.CommonBuildOptions{}
		}
		return nil
	}
}

// WithSystemContext sets the system context for the builder options. 함수 수정: 에러 발생 시 이를 반환
// TODO 확인하자.
func WithSystemContext(sysCtx *imageTypes.SystemContext) BuilderOption {
	return func(opts *buildah.BuilderOptions) error {
		if sysCtx != nil {
			opts.SystemContext = sysCtx
		} else {
			opts.SystemContext = &imageTypes.SystemContext{}
		}
		return nil
	}
}

// WithNetworkConfiguration sets the network configuration policy for the builder options. 함수 수정: 에러 발생 시 이를 반환
func WithNetworkConfiguration(policy define.NetworkConfigurationPolicy) BuilderOption {
	return func(opts *buildah.BuilderOptions) error {
		opts.ConfigureNetwork = policy
		return nil
	}
}

// WithFormat sets the format for the container image to be committed. 함수 수정: 에러 발생 시 이를 반환
func WithFormat(format string) BuilderOption {
	return func(opts *buildah.BuilderOptions) error {
		opts.Format = format
		return nil
	}
}

// WithCapabilities sets capabilities needed for running as root in a container. 함수 수정: 에러 발생 시 이를 반환
func WithCapabilities() BuilderOption {
	return func(opts *buildah.BuilderOptions) error {
		caps, err := capabilities()
		if err != nil {
			return fmt.Errorf("failed to get capabilities: %w", err)
		}
		opts.Capabilities = caps
		return nil
	}
}

// capabilities returns the default capabilities for root.
func capabilities() ([]string, error) {
	conf, err := config.Default()
	if err != nil {
		return nil, fmt.Errorf("failed to get default config: %w", err)
	}
	capabilitiesForRoot, err := conf.Capabilities("root", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get capabilities for root: %w", err)
	}

	return capabilitiesForRoot, nil
}

// ------------------------------------------------------
// Core Builder/Store Functions
// ------------------------------------------------------

// NewBuilder creates a new Builder with the specified options, 함수 수정: 각 옵션을 적용할 때 에러를 확인
func NewBuilder(ctx context.Context, store storage.Store, opts ...BuilderOption) (*buildah.Builder, error) {
	builderOpts := &buildah.BuilderOptions{}
	for _, applyOpt := range opts {
		if err := applyOpt(builderOpts); err != nil {
			return nil, err
		}
	}
	builder, err := buildah.NewBuilder(ctx, store, *builderOpts)
	if err != nil {
		return nil, fmt.Errorf("buildah.NewBuilder: %w", err)
	}
	return builder, nil
}

// NewStore creates and initializes a new storage.Store object
func NewStore() (storage.Store, error) {
	// Get default store options
	buildStoreOptions, err := storage.DefaultStoreOptions()
	if err != nil {
		Log.Errorf("failed to get default store options: %v", err)
		return nil, fmt.Errorf("storage.DefaultStoreOptions: %w", err)
	}
	// Check if running in rootless mode and using overlay driver
	if unshare.IsRootless() && buildStoreOptions.GraphDriverName == "overlay" {
		option := "overlay.mount_program=/usr/bin/fuse-overlayfs"
		// Add the overlay mount program option if it is not already present
		if !utils.Contains(buildStoreOptions.GraphDriverOptions, option) {
			buildStoreOptions.GraphDriverOptions = append(buildStoreOptions.GraphDriverOptions, option)
		}
	}
	// Get the storage store
	buildStore, err := storage.GetStore(buildStoreOptions)
	if err != nil {
		Log.Errorf("failed to get store: %v", err)
		return nil, fmt.Errorf("storage.GetStore: %w", err)
	}
	return buildStore, nil
}

func NewStoreWithOptions(opts ...StoreOption) (storage.Store, error) {
	buildStoreOptions, err := storage.DefaultStoreOptions()
	if err != nil {
		Log.Errorf("failed to get default store options: %v", err)
		return nil, fmt.Errorf("storage.DefaultStoreOptions: %w", err)
	}
	for _, applyOpt := range opts {
		if applyErr := applyOpt(&buildStoreOptions); applyErr != nil {
			return nil, applyErr
		}
	}
	buildStore, err := storage.GetStore(buildStoreOptions)
	if err != nil {
		Log.Errorf("failed to get store: %v", err)
		return nil, fmt.Errorf("storage.GetStore: %w", err)
	}
	return buildStore, nil
}

func NewUserNamespaceStore(opts ...StoreOption) (storage.Store, error) {
	buildStoreOptions := DefaultUserNamespaceStoreOptions()
	for _, applyOpt := range opts {
		if err := applyOpt(&buildStoreOptions); err != nil {
			return nil, err
		}
	}
	buildStore, err := storage.GetStore(buildStoreOptions)
	if err != nil {
		Log.Errorf("failed to get user namespace store: %v", err)
		return nil, fmt.Errorf("storage.GetStore: %w", err)
	}
	return buildStore, nil
}

// shutdown force 를 true 로 잡아주면 다른 컨테이너에게도 영향을 줄 수 있음.
// 기본적으로 false 를 유지하도록 하고, 모든 컨테이너가 종료되어 다른 레이어를 사용하지 않는다면 true 로 해줄 수 있음.
func shutdown(store storage.Store, force bool) error {
	if store == nil {
		return fmt.Errorf("storage.Store is nil")
	}
	_, err := store.Shutdown(force)
	if err != nil {
		return fmt.Errorf("failed to shutdown store: %w", err)
	}
	return nil
}

// ------------------------------------------------------
// Functional Options for buildah.AddAndCopyOptions
// ------------------------------------------------------

// WithChmod sets the Chmod option for AddAndCopyOptions.
func WithChmod(chmod string) func(*buildah.AddAndCopyOptions) {
	return func(opts *buildah.AddAndCopyOptions) {
		opts.Chmod = chmod
	}
}

// WithChown sets the Chown option for AddAndCopyOptions.
func WithChown(chown string) func(*buildah.AddAndCopyOptions) {
	return func(opts *buildah.AddAndCopyOptions) {
		opts.Chown = chown
	}
}

// WithPreserveOwnership sets the PreserveOwnership option for AddAndCopyOptions.
func WithPreserveOwnership(preserve bool) func(*buildah.AddAndCopyOptions) {
	return func(opts *buildah.AddAndCopyOptions) {
		opts.PreserveOwnership = preserve
	}
}

// WithHasher sets the Hasher option for AddAndCopyOptions.
func WithHasher(hasher io.Writer) func(*buildah.AddAndCopyOptions) {
	return func(opts *buildah.AddAndCopyOptions) {
		opts.Hasher = hasher
	}
}

// WithExcludes sets the Excludes option for AddAndCopyOptions.
func WithExcludes(excludes []string) func(*buildah.AddAndCopyOptions) {
	return func(opts *buildah.AddAndCopyOptions) {
		opts.Excludes = excludes
	}
}

// WithIgnoreFile sets the IgnoreFile option for AddAndCopyOptions.
func WithIgnoreFile(ignoreFile string) func(*buildah.AddAndCopyOptions) {
	return func(opts *buildah.AddAndCopyOptions) {
		opts.IgnoreFile = ignoreFile
	}
}

// WithContextDir sets the ContextDir option for AddAndCopyOptions.
func WithContextDir(contextDir string) func(*buildah.AddAndCopyOptions) {
	return func(opts *buildah.AddAndCopyOptions) {
		opts.ContextDir = contextDir
	}
}

// WithIDMappingOptions sets the IDMappingOptions option for AddAndCopyOptions.
func WithIDMappingOptions(idMappingOptions *define.IDMappingOptions) func(*buildah.AddAndCopyOptions) {
	return func(opts *buildah.AddAndCopyOptions) {
		opts.IDMappingOptions = idMappingOptions
	}
}

// WithDryRun sets the DryRun option for AddAndCopyOptions.
func WithDryRun(dryRun bool) func(*buildah.AddAndCopyOptions) {
	return func(opts *buildah.AddAndCopyOptions) {
		opts.DryRun = dryRun
	}
}

// WithStripSetuidBit sets the StripSetuidBit option for AddAndCopyOptions.
func WithStripSetuidBit(strip bool) func(*buildah.AddAndCopyOptions) {
	return func(opts *buildah.AddAndCopyOptions) {
		opts.StripSetuidBit = strip
	}
}

// WithStripSetgidBit sets the StripSetgidBit option for AddAndCopyOptions.
func WithStripSetgidBit(strip bool) func(*buildah.AddAndCopyOptions) {
	return func(opts *buildah.AddAndCopyOptions) {
		opts.StripSetgidBit = strip
	}
}

// WithStripStickyBit sets the StripStickyBit option for AddAndCopyOptions.
func WithStripStickyBit(strip bool) func(*buildah.AddAndCopyOptions) {
	return func(opts *buildah.AddAndCopyOptions) {
		opts.StripStickyBit = strip
	}
}

// NewAddAndCopyOptions creates a new AddAndCopyOptions with the specified options applied.
func NewAddAndCopyOptions(opts ...func(*buildah.AddAndCopyOptions)) buildah.AddAndCopyOptions {
	options := &buildah.AddAndCopyOptions{}
	for _, applyOpt := range opts {
		applyOpt(options)
	}
	return *options
}

// ------------------------------------------------------
// Image Build Helper Functions
// ------------------------------------------------------
// TODO dockerfilePath 관점에서 ContextDirectory 생각해봐야 한다. 실제 caleb 적용시 수정될 수 있음. (중요)
// buildImageFromDockerfile builds an image from the provided Dockerfile
func buildImageFromDockerfile(ctx context.Context, dockerfilePath string) (string, error) {
	// Define build options
	options := types.BuildOptions{
		BuildOptions: define.BuildOptions{
			ContextDirectory: ".",
			PullPolicy:       define.PullIfMissing,
			Isolation:        define.IsolationOCI,
			SystemContext:    &imageTypes.SystemContext{},
		},
		ContainerFiles: []string{dockerfilePath},
	}
	// Build the Dockerfile
	r, err := images.Build(ctx, options.ContainerFiles, options)
	if err != nil {
		return "", fmt.Errorf("images.Build: %w", err)
	}

	return r.ID, nil
}

// BuildDockerfileContent builds an OCI image from a Dockerfile content string using the
// buildah library directly (no Podman socket required). This is the preferred entry point
// for NodeForge and other host-side admin tools running with elevated privileges.
//
// Parameters:
//   - ctx: context for the build (must not be nil)
//   - store: buildah storage store (use NewStore() to create one)
//   - dockerfileContent: full Dockerfile text as a string
//   - outputRef: destination image reference, e.g. "10.87.127.18:31500/myimage:latest"
//
// Returns:
//   - imageID: short content-addressable image ID
//   - digestStr: sha256 digest of the built image manifest (empty string if unavailable)
//   - err: any build error
func BuildDockerfileContent(ctx context.Context, store storage.Store, dockerfileContent, outputRef string) (imageID, digestStr string, err error) {
	if store == nil {
		return "", "", fmt.Errorf("store must not be nil")
	}
	return buildDockerfileContentWithRuntime(ctx, realImageBuildRuntime{}, store, dockerfileContent, outputRef)
}

func BuildDockerfileContentWithOptions(ctx context.Context, store storage.Store, dockerfileContent string, buildOpts define.BuildOptions) (imageID, digestStr string, err error) {
	if store == nil {
		return "", "", fmt.Errorf("store must not be nil")
	}
	return buildDockerfileContentWithOptionsRuntime(ctx, realImageBuildRuntime{}, store, dockerfileContent, buildOpts)
}

func BuildDockerfileContentUserNamespace(ctx context.Context, store storage.Store, dockerfileContent string, cfg UserNamespaceBuildConfig) (imageID, digestStr string, err error) {
	if store == nil {
		return "", "", fmt.Errorf("store must not be nil")
	}
	buildOpts, err := UserNamespaceImageBuildOptions(cfg)
	if err != nil {
		return "", "", err
	}
	return buildDockerfileContentWithOptionsRuntime(ctx, realImageBuildRuntime{}, store, dockerfileContent, buildOpts)
}

// PushImage pushes a locally built image from the provided storage store to the
// destination registry reference and returns the pushed manifest digest.
func PushImage(ctx context.Context, store storage.Store, imageRef, destination string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("store must not be nil")
	}
	return pushImageWithRuntime(ctx, realImageBuildRuntime{}, store, imageRef, destination)
}

// BuildAndPushDockerfileContent builds an image from Dockerfile content and
// pushes it to the destination registry reference.
func BuildAndPushDockerfileContent(ctx context.Context, store storage.Store, dockerfileContent, outputRef string) (imageID, digestStr string, err error) {
	if store == nil {
		return "", "", fmt.Errorf("store must not be nil")
	}
	return buildAndPushDockerfileContentWithRuntime(ctx, realImageBuildRuntime{}, store, dockerfileContent, outputRef)
}

// newBuilder creates a new builder using the NewBuilder function with default options.
// TODO 좀더 study 필요. 옵션들에 대해서.
func newBuilder(ctx context.Context, store storage.Store, idName string) (*buildah.Builder, error) {
	return newBuilderWithRuntime(ctx, realImageBuilderFactoryRuntime{}, store, idName)
}

// newAddAndCopyOptions creates default add and copy options.
func newAddAndCopyOptions() buildah.AddAndCopyOptions {
	return newDefaultAddAndCopyOptions(digester.Hash())
}

// ------------------------------------------------------
// Helper Functions for Image Building
// ------------------------------------------------------

// saveImage saves the built image to an archive file.
func saveImage(ctx context.Context, path, imageName, imageID string, compress bool) error {
	return saveImageWithRuntime(ctx, realImageExportRuntime{}, path, imageName, imageID, compress)
}

// internalizeImageName 은 입력 이미지 이름에서 태그 앞에 "-internal"을 삽입하여 내부 전용 이미지 이름을 생성
// 예: "docker.io/library/alpine:latest" -> "docker.io/library/alpine-internal:latest"
func internalizeImageName(imageName string) string {
	// 마지막 콜론의 인덱스를 찾습니다.
	colonIndex := strings.LastIndex(imageName, ":")
	if colonIndex == -1 {
		// 태그가 없는 경우, 그냥 "-internal"을 추가합니다.
		return imageName + "-internal"
	}

	// 콜론 앞까지의 이미지 이름과 태그를 분리
	baseName := imageName[:colonIndex]
	tag := imageName[colonIndex:] // 콜론 포함

	// 내부 전용 이미지 이름 생성
	return baseName + "-internal" + tag
}
