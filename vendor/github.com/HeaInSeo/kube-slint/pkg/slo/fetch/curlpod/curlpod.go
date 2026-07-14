package curlpod

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/HeaInSeo/kube-slint/pkg/slo/evidence"
)

// CurlPod 는 외부 어댑터 없이 curl 파드 수명 주기를 캡슐화함.
type CurlPod struct {
	Client             *Client
	Namespace          string
	MetricsServiceName string
	ServiceAccountName string
	Token              string

	Image            string
	ServiceURLFormat string
}

// Run 은 curl 파드 수명 주기를 실행하고 로그를 반환함.
func (c *CurlPod) Run(ctx context.Context, waitTimeout time.Duration, logsTimeout time.Duration) (string, error) {
	client := c.Client
	if client == nil {
		client = New(nil, nil)
	}
	if c.Image != "" {
		client.Image = c.Image
	}
	if c.ServiceURLFormat != "" {
		client.ServiceURLFormat = c.ServiceURLFormat
	}

	podName, err := client.RunOnce(ctx, c.Namespace, c.Token, c.MetricsServiceName, c.ServiceAccountName)
	if err != nil {
		return "", err
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, waitTimeout)
	defer waitCancel()
	waitErr := client.WaitDone(waitCtx, c.Namespace, podName, 2*time.Second)
	if waitErr != nil && !errors.Is(waitErr, ErrPodFailed) {
		// A non-phase error (context deadline, kubectl call itself failed) —
		// the pod never reached a terminal phase, so there's nothing useful
		// to fetch from its logs.
		c.cleanupWithLog(ctx, client, podName)
		return "", waitErr
	}

	// Either the pod Succeeded, or it reached phase Failed — in both cases
	// its logs (the curl output, captured via --fail-with-body even on a
	// non-2xx response) are worth fetching: on success they're the
	// measurement itself, on failure they're the diagnostic the caller
	// needs to understand why (e.g. an RBAC 403 body).
	logCtx, logCancel := context.WithTimeout(ctx, logsTimeout)
	defer logCancel()
	out, logErr := client.Logs(logCtx, c.Namespace, podName)
	c.cleanupWithLog(ctx, client, podName)

	if waitErr != nil {
		if logErr == nil && strings.TrimSpace(out) != "" {
			// Redact before embedding: this is the raw metrics endpoint
			// response body (whatever caused the non-2xx that failed the
			// pod), which could echo back sensitive request data. Same
			// redaction contract applied to every other error/log surface
			// in this codebase (see pkg/kubeutil/runner.go).
			return "", fmt.Errorf("%w (pod output: %s)", waitErr, evidence.RedactString(strings.TrimSpace(out)))
		}
		return "", waitErr
	}
	return out, logErr
}

// cleanupWithLog deletes the pod and logs a warning if deletion fails.
// Cleanup failure is non-fatal but must be visible for manual remediation.
func (c *CurlPod) cleanupWithLog(ctx context.Context, client *Client, podName string) {
	if err := client.DeletePodNoWait(ctx, c.Namespace, podName); err != nil {
		client.Logger.Logf(
			"kube-slint [curlpod]: cleanup warning — failed to delete pod %s/%s: %v"+
				" (pod may require manual cleanup; selector: %s)",
			c.Namespace, podName, err, client.LabelSelector,
		)
	}
}
