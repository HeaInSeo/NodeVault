package podbridge5

import (
	"context"
	"fmt"

	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/specgen"
)

type ContainerStatus int

const (
	Created   ContainerStatus = iota //0
	Running                          // 1
	Exited                           // 2
	ExitedErr                        // 3
	Healthy                          // 4
	Unhealthy                        // 5
	Dead                             // 6
	Paused                           // 7
	UnKnown                          // 8
	None                             // 9
)

type ContainerOptions func(spec *specgen.SpecGenerator) error

// CreateContainerResult 컨테이너 생성 정보를 담는 구조체
type (
	CreateContainerResult struct {
		Name     string
		ID       string
		Warnings []string
		Status   ContainerStatus
	}
)

// NewSpec creates a new SpecGenerator.
func NewSpec(opts ...ContainerOptions) (*specgen.SpecGenerator, error) {
	spec := &specgen.SpecGenerator{}
	for _, opt := range opts {
		if err := opt(spec); err != nil {
			return nil, err
		}
	}
	return spec, nil
}

func WithImageName(imgName string) ContainerOptions {
	return func(spec *specgen.SpecGenerator) error {
		spec.Image = imgName
		return nil
	}
}

func WithName(name string) ContainerOptions {
	return func(spec *specgen.SpecGenerator) error {
		spec.Name = name
		return nil
	}
}

func WithTerminal(terminal bool) ContainerOptions {
	return func(spec *specgen.SpecGenerator) error {
		spec.Terminal = &terminal
		return nil
	}
}

// WithPod sets the pod ID for a container spec, allowing the container to join the given pod.
func WithPod(podID string) ContainerOptions {
	return func(spec *specgen.SpecGenerator) error {
		spec.Pod = podID
		return nil
	}
}

func WithSysAdmin() ContainerOptions {
	return func(spec *specgen.SpecGenerator) error {
		spec.CapAdd = append(spec.CapAdd, "SYS_ADMIN")
		return nil
	}
}

// WithUnconfinedSeccomp sets the container’s seccomp policy to “unconfined”,
// allowing syscalls like mount(2) that the default profile would block.
func WithUnconfinedSeccomp() ContainerOptions {
	return func(spec *specgen.SpecGenerator) error {
		spec.SeccompPolicy = "unconfined"
		return nil
	}
}

// WithEnv 단일 키/값 환경변수 추가
func WithEnv(key, value string) ContainerOptions {
	return func(spec *specgen.SpecGenerator) error {
		if spec.Env == nil {
			spec.Env = make(map[string]string)
		}
		spec.Env[key] = value
		return nil
	}
}

// WithEnvs 여러 개를 한 번에 추가하고 싶다면 (옵션)
func WithEnvs(envs map[string]string) ContainerOptions {
	return func(spec *specgen.SpecGenerator) error {
		if spec.Env == nil {
			spec.Env = make(map[string]string)
		}
		for k, v := range envs {
			spec.Env[k] = v
		}
		return nil
	}
}

func WithCommand(cmd []string) ContainerOptions {
	return func(spec *specgen.SpecGenerator) error {
		spec.Command = cmd
		return nil
	}
}

// WithHealthChecker healthcheck 설정에 문제가 발생하면 에러를 반환
func WithHealthChecker(inCmd, interval string, retries uint, timeout, startPeriod string) ContainerOptions {
	// 한 번만 파싱/검증
	hc, err := ParseHealthcheckConfig(inCmd, interval, retries, timeout, startPeriod)
	return func(spec *specgen.SpecGenerator) error {
		if err != nil {
			// 옵션 생성 시점에 실패 원인을 그대로 반환
			return fmt.Errorf("invalid healthcheck config: %w", err)
		}
		spec.HealthConfig = hc
		return nil
	}
}

// StartContainer 컨테이너를 만들고 시작함.
func StartContainer(ctx context.Context, spec *specgen.SpecGenerator) (string, error) {
	return startContainerWithRuntime(ctx, podmanContainerRuntime{}, spec)
}

// CreateContainer 컨테이너 생성
func CreateContainer(ctx context.Context, conSpec *specgen.SpecGenerator) (*CreateContainerResult, error) {
	return createContainerWithRuntime(ctx, podmanContainerRuntime{}, conSpec)
}

func InspectContainer(ctx context.Context, containerID string) (*define.InspectContainerData, error) {
	return inspectContainerWithRuntime(ctx, podmanContainerRuntime{}, containerID)
}

// HealthCheckContainer returns the container's Status string and an exitCode:
//   - exitCode == -1 : no health information available
//   - exitCode ==  0 : healthy or latest exitCode=0
//   - exitCode  > 0 : the latest nonzero exit code from health logs
func HealthCheckContainer(ctx context.Context, containerID string) (status string, exitCode int, err error) {
	// 1) Inspect
	data, err := InspectContainer(ctx, containerID)
	if err != nil {
		return "", -1, err
	}

	// 2) 상태
	if data.State.Status == "" {
		return "", -1, fmt.Errorf("container %q state status is empty", containerID)
	}
	status = data.State.Status

	// 3) 헬스 정보
	if data.State.Health == nil || len(data.State.Health.Log) == 0 {
		// 헬스체크가 설정되지 않았거나 로그가 없는 경우
		return status, -1, nil
	}

	latest := data.State.Health.Log[len(data.State.Health.Log)-1]
	if latest.ExitCode != 0 {
		return status, latest.ExitCode, nil
	}

	return status, 0, nil
}
