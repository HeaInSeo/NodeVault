package kubeutil

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/HeaInSeo/kube-slint/pkg/slo"
)

const (
	// PrometheusOperatorVersion is the version of the bundle to install.
	// PrometheusOperatorVersion은 설치할 번들의 버전입니다.
	PrometheusOperatorVersion = "v0.77.1"

	// Split to satisfy lll (max 120 chars) while keeping identical URL.
	// lll(최대 120자)을 만족하기 위해 분할했지만 URL은 동일합니다.
	prometheusOperatorURLTmpl = "https://github.com/prometheus-operator/" +
		"prometheus-operator/releases/download/%s/bundle.yaml"
)

// PrometheusOperatorURL returns the download URL for the bundle.
// PrometheusOperatorURL은 번들의 다운로드 URL을 반환합니다.
func PrometheusOperatorURL() string {
	return fmt.Sprintf(prometheusOperatorURLTmpl, PrometheusOperatorVersion)
}

// InstallPrometheusOperator installs Prometheus Operator bundle.
// - enabled=false이면 설치를 건너뛰고 nil 반환(테스트/운영에서 토글하기 쉬움).
// - logger may be nil (no-op).
// - r may be nil (uses DefaultRunner).
// InstallPrometheusOperator는 Prometheus Operator 번들을 설치합니다.
// - enabled=false이면 설치를 건너뛰고 nil 반환(테스트/운영에서 토글하기 쉬움).
// - logger는 nil일 수 있습니다 (no-op).
// - r은 nil일 수 있습니다 (DefaultRunner 사용).
func InstallPrometheusOperator(
	ctx context.Context,
	logger slo.Logger,
	r CmdRunner,
	enabled bool,
) error {
	logger = slo.NewLogger(logger)
	if r == nil {
		r = DefaultRunner{}
	}
	if !enabled {
		logger.Logf("prometheus-operator install skipped (disabled)")
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	url := PrometheusOperatorURL()
	logger.Logf(
		"installing prometheus-operator version=%s",
		PrometheusOperatorVersion,
	)

	cmd := exec.Command("kubectl", "apply", "-f", url) // apply is idempotent
	_, err := r.Run(ctx, logger, cmd)
	return err
}

// UninstallPrometheusOperator uninstalls Prometheus Operator bundle.
// - logger may be nil (no-op).
// - r may be nil (uses DefaultRunner).
// UninstallPrometheusOperator는 Prometheus Operator 번들을 제거합니다.
// - logger는 nil일 수 있습니다 (no-op).
// - r은 nil일 수 있습니다 (DefaultRunner 사용).
func UninstallPrometheusOperator(
	ctx context.Context,
	logger slo.Logger,
	r CmdRunner,
) error {
	logger = slo.NewLogger(logger)
	if r == nil {
		r = DefaultRunner{}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	url := PrometheusOperatorURL()
	logger.Logf(
		"uninstalling prometheus-operator version=%s",
		PrometheusOperatorVersion,
	)

	cmd := exec.Command(
		"kubectl", "delete", "-f", url,
		"--ignore-not-found=true",
	)
	_, err := r.Run(ctx, logger, cmd)
	return err
}

// IsPrometheusOperatorCRDsInstalled checks if Prometheus Operator CRDs exist.
// - logger may be nil (no-op).
// - r may be nil (uses DefaultRunner).
// IsPrometheusOperatorCRDsInstalled는 Prometheus Operator CRD가 존재하는지 확인합니다.
// - logger는 nil일 수 있습니다 (no-op).
// - r은 nil일 수 있습니다 (DefaultRunner 사용).
func IsPrometheusOperatorCRDsInstalled(
	ctx context.Context,
	logger slo.Logger,
	r CmdRunner,
) bool {
	logger = slo.NewLogger(logger)
	if r == nil {
		r = DefaultRunner{}
	}
	if err := ctx.Err(); err != nil {
		logger.Logf("IsPrometheusOperatorCRDsInstalled: ctx error: %v", err)
		return false
	}

	prometheusCRDs := []string{
		"prometheuses.monitoring.coreos.com",
		"prometheusrules.monitoring.coreos.com",
		"prometheusagents.monitoring.coreos.com",
	}

	cmd := exec.Command(
		"kubectl", "get", "crds",
		"-o", "custom-columns=NAME:.metadata.name",
	)
	out, err := r.Run(ctx, logger, cmd)
	if err != nil {
		return false
	}

	for _, line := range strings.Split(out, "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		for _, crd := range prometheusCRDs {
			if strings.Contains(s, crd) {
				return true
			}
		}
	}
	return false
}
