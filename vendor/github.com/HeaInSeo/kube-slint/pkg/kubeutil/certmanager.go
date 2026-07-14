package kubeutil

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/HeaInSeo/kube-slint/pkg/slo"
)

const (
	certmanagerVersion = "v1.16.3"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"
)

func certmanagerURL() string {
	return fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
}

// InstallCertManager installs cert-manager and waits for webhook deployment to be Available.
// - logger may be nil (no-op).
// - r may be nil (uses DefaultRunner).
func InstallCertManager(ctx context.Context, logger slo.Logger, r CmdRunner) error {
	logger = slo.NewLogger(logger)
	if r == nil {
		r = DefaultRunner{}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	url := certmanagerURL()
	logger.Logf("installing cert-manager version=%s", certmanagerVersion)

	// Apply bundle
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := r.Run(ctx, logger, cmd); err != nil {
		return err
	}

	// Wait for webhook to be ready (can take time after reinstall).
	// Note: kubectl --timeout is independent of ctx; ctx still can cancel the process.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)
	_, err := r.Run(ctx, logger, cmd)
	return err
}

// UninstallCertManager uninstalls cert-manager bundle.
// - logger may be nil (no-op).
// - r may be nil (uses DefaultRunner).
func UninstallCertManager(ctx context.Context, logger slo.Logger, r CmdRunner) error {
	logger = slo.NewLogger(logger)
	if r == nil {
		r = DefaultRunner{}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	url := certmanagerURL()
	logger.Logf("uninstalling cert-manager version=%s", certmanagerVersion)

	cmd := exec.Command("kubectl", "delete", "-f", url)
	_, err := r.Run(ctx, logger, cmd)
	return err
}

// IsCertManagerCRDsInstalled checks if any cert-manager CRDs are installed.
// It returns true if at least one well-known CRD is found.
// - logger may be nil (no-op).
// - r may be nil (uses DefaultRunner).
func IsCertManagerCRDsInstalled(ctx context.Context, logger slo.Logger, r CmdRunner) bool {
	logger = slo.NewLogger(logger)
	if r == nil {
		r = DefaultRunner{}
	}
	if err := ctx.Err(); err != nil {
		logger.Logf("IsCertManagerCRDsInstalled: ctx error: %v", err)
		return false
	}

	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	cmd := exec.Command("kubectl", "get", "crds")
	output, err := r.Run(ctx, logger, cmd)
	if err != nil {
		// A failed `kubectl get crds` (no cluster access, permission denied,
		// kubectl missing) is not the same thing as "CRDs genuinely absent" —
		// log it so a caller deciding whether to InstallCertManager can tell
		// the difference instead of silently retrying an install that will
		// also fail for the same underlying reason.
		logger.Logf("IsCertManagerCRDsInstalled: kubectl get crds failed: %v", err)
		return false
	}

	lines := getNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range lines {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}
	return false
}

func getNonEmptyLines(output string) []string {
	lines := strings.Split(output, "\n")
	res := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		res = append(res, line)
	}
	return res
}
