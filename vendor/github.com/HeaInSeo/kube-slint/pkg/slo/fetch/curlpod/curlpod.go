package curlpod

import (
	"context"
	"time"
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
	if err := client.WaitDone(waitCtx, c.Namespace, podName, 2*time.Second); err != nil {
		c.cleanupWithLog(ctx, client, podName)
		return "", err
	}

	logCtx, logCancel := context.WithTimeout(ctx, logsTimeout)
	defer logCancel()
	out, err := client.Logs(logCtx, c.Namespace, podName)
	c.cleanupWithLog(ctx, client, podName)
	return out, err
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
