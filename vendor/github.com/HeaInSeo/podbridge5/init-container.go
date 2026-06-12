package podbridge5

import (
	"context"
	"fmt"
	"strings"
)

// TODO 일단 간단하게 overlayfs 가 잘 생성이 되고 잘 전파가 되는지만 먼저 확인하고, 다시 구현해야 함.
// TODO Pod 생성한 다음에, 제일 처음에는 init container 를 실행해서 overlayfs 를 생성하는 작업을 해야 한다.

// CreateInitContainer sets up an overlay mount in the pod's mount namespace.
// It launches a privileged container that executes the mount command directly.
func CreateInitContainer(ctx context.Context, podID, lowerdir, upperdir, workdir, target string) (string, error) {
	opts, err := overlayMountOptions(lowerdir, upperdir, workdir)
	if err != nil {
		return "", err
	}
	if validateErr := validateOverlayPathArg("target", target); validateErr != nil {
		return "", validateErr
	}

	spec, err := NewSpec(
		WithPod(podID),
		WithName("init-container"),
		WithImageName("docker.io/library/alpine:latest"), // Use a lightweight image for the init container
		WithSysAdmin(),
		WithUnconfinedSeccomp(),
		WithCommand([]string{
			"mount",
			"-t", "overlay",
			"overlay",
			"-o", opts,
			target,
		}),
	)
	if err != nil {
		return "", fmt.Errorf("failed to build mount-init spec: %w", err)
	}
	return StartContainer(ctx, spec)
}

func overlayMountOptions(lowerdir, upperdir, workdir string) (string, error) {
	for label, value := range map[string]string{
		"lowerdir": lowerdir,
		"upperdir": upperdir,
		"workdir":  workdir,
	} {
		if err := validateOverlayPathArg(label, value); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerdir, upperdir, workdir), nil
}

func validateOverlayPathArg(name, value string) error {
	if strings.ContainsAny(value, ",\n\r\x00") {
		return fmt.Errorf("%s contains unsupported characters", name)
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	return nil
}
