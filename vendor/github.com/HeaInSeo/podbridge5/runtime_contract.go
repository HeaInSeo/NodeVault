package podbridge5

import (
	"errors"
	"strings"

	"go.podman.io/image/v5/manifest"
)

const (
	ContainerAppDir             = "/app"
	ContainerScriptsDir         = "/app/scripts"
	ContainerExecutorPath       = "/app/executor.sh"
	ContainerInstallPath        = "/app/install.sh"
	ContainerHealthcheckPath    = "/app/healthcheck.sh"
	ContainerUserScriptPath     = "/app/scripts/user_script.sh"
	ContainerResultLogPath      = "/app/result.log"
	ContainerExitCodeLogPath    = "/app/exit_code.log"
	ContainerDefaultWorkDir     = "/app"
	ContainerDefaultHealthcheck = "CMD-SHELL /app/healthcheck.sh"
)

func DefaultExecutorCommand() []string {
	return []string{"/bin/sh", "-c", ContainerExecutorPath}
}

func DefaultHealthcheckCommand() string {
	return ContainerDefaultHealthcheck
}

func ParseHealthcheckConfig(inCmd, interval string, retries uint, timeout, startPeriod string) (*manifest.Schema2HealthConfig, error) {
	cmdArr := strings.Fields(strings.TrimSpace(inCmd))
	if len(cmdArr) < 2 || cmdArr[0] != "CMD-SHELL" {
		return nil, errors.New("invalid command format: must start with CMD-SHELL")
	}

	hc := manifest.Schema2HealthConfig{Test: cmdArr}

	intervalDuration, err := parseHealthcheckInterval(interval)
	if err != nil {
		return nil, err
	}
	hc.Interval = intervalDuration

	if retries < 1 {
		return nil, errors.New("healthcheck-retries must be greater than 0")
	}
	hc.Retries = int(retries)

	timeoutDuration, err := parseHealthcheckTimeout(timeout)
	if err != nil {
		return nil, err
	}
	hc.Timeout = timeoutDuration

	startPeriodDuration, err := parseHealthcheckStartPeriod(startPeriod)
	if err != nil {
		return nil, err
	}
	hc.StartPeriod = startPeriodDuration

	return &hc, nil
}
