package curlpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/HeaInSeo/kube-slint/pkg/kubeutil"
	"github.com/HeaInSeo/kube-slint/pkg/slo"
)

// ErrPodFailed indicates the curl pod reached phase "Failed" — the scrape
// itself did not complete successfully (e.g. curl's --fail-with-body exited
// non-zero on a non-2xx response, or the container was OOM-killed), as
// opposed to a kubectl/context error. Callers must treat this as a scrape
// failure, not attempt to parse the pod's logs as if they were a
// successful measurement — "Succeeded" and "Failed" used to be
// indistinguishable to WaitDone's caller, which let a failed scrape's raw
// (often non-Prometheus) log output flow into the metrics parser and
// silently report as an empty-but-successful sample.
var ErrPodFailed = errors.New("curl pod reached phase Failed")

// PodLabelSelector 는 curl 파드에 사용되는 레이블임.
const PodLabelSelector = "app=curl-metrics"

// Client 는 curl-metrics 파드를 실행하고 로그를 가져옴.
// 테스트 지향적임 (kubectl + curlimages/curl 사용).
type Client struct {
	Logger slo.Logger
	Runner kubeutil.CmdRunner

	// 옵션 조정 가능 항목
	Image            string
	LabelSelector    string
	PodNamePrefix    string
	ServiceURLFormat string // e.g. "https://%s.%s.svc:8443/metrics"

	// TLSInsecureSkipVerify: if true, adds -k to curl.
	//
	// Deprecated: use DangerouslySkipTLSVerify instead — this field is kept
	// for backward compatibility and still takes effect (the two are OR'd),
	// but new callers should prefer the visibly-named dangerous option.
	TLSInsecureSkipVerify bool

	// DangerouslySkipTLSVerify disables TLS certificate verification for the
	// metrics scrape (adds -k to curl). Off by default; only enable this for
	// a narrow, known-insecure dev/test target.
	DangerouslySkipTLSVerify bool

	// DangerouslyAllowExternalMetricsURL disables the default-deny check that
	// ServiceURLFormat must resolve to a cluster-local Service address
	// ("<service>.<namespace>.svc[.cluster.local]"). Off by default: enabling
	// it allows the Authorization bearer token to be sent to whatever host
	// ServiceURLFormat happens to resolve to, including an external one.
	DangerouslyAllowExternalMetricsURL bool

	// DangerouslyAllowKubeSystemNamespace disables the default rejection of
	// cluster-critical target namespaces (kube-system, kube-public,
	// kube-node-lease). Off by default.
	DangerouslyAllowKubeSystemNamespace bool
}

// New 는 안전한 기본값으로 클라이언트를 생성함.
// logger는 nil일 수 있음.
func New(logger slo.Logger, r kubeutil.CmdRunner) *Client {
	if r == nil {
		r = kubeutil.DefaultRunner{}
	}
	return &Client{
		Logger: slo.NewLogger(logger),
		Runner: r,
		// Version tag, not a digest — see docs/DECISIONS.md D-019.
		Image:            "curlimages/curl:8.11.0",
		LabelSelector:    PodLabelSelector,
		PodNamePrefix:    "curl-metrics",
		ServiceURLFormat: "https://%s.%s.svc:8443/metrics",
		// TLSInsecureSkipVerify defaults to false: skipping TLS verification
		// is a security boundary bypass and must be an explicit opt-in
		// (DangerouslySkipTLSVerify), not a silent default.
		TLSInsecureSkipVerify: false,
	}
}

// RunOnce 는 /metrics를 스크랩하는 수명이 짧은 curl 파드를 생성함.
// 생성된 파드 이름을 반환함.
// 기다리지 않으므로 WaitDone을 호출한 다음 Logs를 호출해야 함.
func (c *Client) RunOnce(ctx context.Context, ns, token, metricsSvcName, serviceAccountName string) (string, error) {
	c.Logger = slo.NewLogger(c.Logger)
	if c.Runner == nil {
		c.Runner = kubeutil.DefaultRunner{}
	}
	// Compatibility: Token remains in the public call signature, but the pod
	// reads its own mounted ServiceAccount token so secrets do not enter
	// kubectl args, PodSpec command strings, or command logs.
	_ = token

	// Validate the target namespace and metrics URL BEFORE creating any pod
	// (best-effort cleanup included) — a rejected config must never reach
	// kubectl.
	if isDangerousNamespace(ns) && !c.DangerouslyAllowKubeSystemNamespace {
		return "", fmt.Errorf(
			"namespace %q is a cluster-critical namespace and is rejected by default; "+
				"set DangerouslyAllowKubeSystemNamespace to override", ns)
	}
	metricsURL, err := ValidateMetricsURL(c.ServiceURLFormat, metricsSvcName, ns, c.DangerouslyAllowExternalMetricsURL)
	if err != nil {
		return "", err
	}
	if !isValidDNSLabel(serviceAccountName) {
		return "", fmt.Errorf("invalid ServiceAccount name %q: must be a valid DNS label", serviceAccountName)
	}

	// 이전 curl-metrics 파드 최선(best-effort) 정리
	_ = c.CleanupByLabel(ctx, ns)

	podName := fmt.Sprintf("%s-%d", c.PodNamePrefix, time.Now().UnixNano())

	insecureFlag := ""
	if c.TLSInsecureSkipVerify || c.DangerouslySkipTLSVerify {
		insecureFlag = "-k"
	}

	curlCmd := fmt.Sprintf(`set -euo pipefail;
TOKEN="$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)";
curl %s -sS --fail-with-body -H "Authorization: Bearer ${TOKEN}" "%s";`, insecureFlag, metricsURL)

	// Build pod labels from LabelSelector so that CleanupByLabel and
	// runCleanupActions (which use the same selector) can find these pods.
	// Always include app=curl-metrics for base compatibility.
	podLabels := parseSelectorToLabels(c.LabelSelector)
	if _, ok := podLabels["app"]; !ok {
		podLabels["app"] = "curl-metrics"
	}

	overridesJSON, err := json.Marshal(podOverride{
		APIVersion: "v1",
		Kind:       "Pod",
		Metadata: podOverrideMetadata{
			Name:      podName,
			Namespace: ns,
			Labels:    podLabels,
		},
		Spec: podOverrideSpec{
			ServiceAccountName:           serviceAccountName,
			AutomountServiceAccountToken: true,
			RestartPolicy:                "Never",
			Containers: []podOverrideContainer{{
				Name:    "curl",
				Image:   c.Image,
				Command: []string{"/bin/sh", "-c", curlCmd},
				SecurityContext: podOverrideSecurityContext{
					AllowPrivilegeEscalation: false,
					Capabilities:             podOverrideCapabilities{Drop: []string{"ALL"}},
					RunAsNonRoot:             true,
					RunAsUser:                1000,
					SeccompProfile:           podOverrideSeccompProfile{Type: "RuntimeDefault"},
				},
			}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshal pod overrides: %w", err)
	}

	cmd := exec.Command(
		"kubectl", "run", podName,
		"--restart=Never",
		"--namespace", ns,
		"--image", c.Image,
		"--labels", c.LabelSelector,
		"--overrides", string(overridesJSON),
	)

	_, err = c.Runner.Run(ctx, c.Logger, cmd)
	return podName, err
}

// podOverride and its nested types mirror just the fields RunOnce needs from
// a Kubernetes Pod spec. Marshaled via encoding/json rather than built with
// fmt.Sprintf string interpolation, so every field (including
// caller-supplied ones like ServiceAccountName and Image) is properly JSON-
// escaped — no field can smuggle extra keys (e.g. hostNetwork, hostPath
// volumes) into the --overrides payload the way raw string splicing could.
type podOverride struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   podOverrideMetadata `json:"metadata"`
	Spec       podOverrideSpec     `json:"spec"`
}

type podOverrideMetadata struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
}

type podOverrideSpec struct {
	ServiceAccountName           string                 `json:"serviceAccountName"`
	AutomountServiceAccountToken bool                   `json:"automountServiceAccountToken"`
	RestartPolicy                string                 `json:"restartPolicy"`
	Containers                   []podOverrideContainer `json:"containers"`
}

type podOverrideContainer struct {
	Name            string                     `json:"name"`
	Image           string                     `json:"image"`
	Command         []string                   `json:"command"`
	SecurityContext podOverrideSecurityContext `json:"securityContext"`
}

type podOverrideSecurityContext struct {
	AllowPrivilegeEscalation bool                      `json:"allowPrivilegeEscalation"`
	Capabilities             podOverrideCapabilities   `json:"capabilities"`
	RunAsNonRoot             bool                      `json:"runAsNonRoot"`
	RunAsUser                int64                     `json:"runAsUser"`
	SeccompProfile           podOverrideSeccompProfile `json:"seccompProfile"`
}

type podOverrideCapabilities struct {
	Drop []string `json:"drop"`
}

type podOverrideSeccompProfile struct {
	Type string `json:"type"`
}

// WaitDone 은 curl 파드가 종료 단계(Succeeded/Failed)에 도달할 때까지 기다림.
// Returns ErrPodFailed (wrapped with the pod's identity) if the pod reaches
// phase "Failed" — this is a terminal state, not a transient kubectl error,
// so the caller must not proceed to treat the pod's logs as a successful
// measurement.
func (c *Client) WaitDone(ctx context.Context, ns, podName string, poll time.Duration) error {
	c.Logger = slo.NewLogger(c.Logger)
	if c.Runner == nil {
		c.Runner = kubeutil.DefaultRunner{}
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	// 즉시 첫 번째 확인
	if done, err := c.checkTerminalPhase(ctx, ns, podName); err != nil {
		return err
	} else if done {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			done, err := c.checkTerminalPhase(ctx, ns, podName)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// checkTerminalPhase reports whether the pod has reached a terminal phase.
// done=true, err=nil means phase=Succeeded. done=true, err=ErrPodFailed
// means phase=Failed — still "terminal" in the sense that polling should
// stop, but the caller must treat it as a failure, not success.
func (c *Client) checkTerminalPhase(ctx context.Context, ns, podName string) (done bool, err error) {
	phase, err := c.podPhase(ctx, ns, podName)
	if err != nil {
		return false, err
	}
	switch phase {
	case "Succeeded":
		return true, nil
	case "Failed":
		return true, fmt.Errorf("%w: %s/%s", ErrPodFailed, ns, podName)
	default:
		return false, nil
	}
}

// Logs 는 주어진 파드의 kubectl 로그를 반환함.
func (c *Client) Logs(ctx context.Context, ns, podName string) (string, error) {
	c.Logger = slo.NewLogger(c.Logger)
	if c.Runner == nil {
		c.Runner = kubeutil.DefaultRunner{}
	}

	cmd := exec.Command("kubectl", "logs", podName, "-n", ns)
	return c.Runner.Run(ctx, c.Logger, cmd)
}

// DeletePodNoWait 는 기다리지 않고 최선의 노력(best-effort)으로 파드를 삭제함.
func (c *Client) DeletePodNoWait(ctx context.Context, ns, podName string) error {
	c.Logger = slo.NewLogger(c.Logger)
	if c.Runner == nil {
		c.Runner = kubeutil.DefaultRunner{}
	}

	cmd := exec.Command(
		"kubectl", "delete", "pod", podName,
		"-n", ns,
		"--ignore-not-found=true",
		"--wait=false",
	)
	_, err := c.Runner.Run(ctx, c.Logger, cmd)
	return err
}

// CleanupByLabel 은 레이블 셀렉터로 모든 curl-metrics 파드를 삭제함 (최선의 노력, 기다리지 않음).
func (c *Client) CleanupByLabel(ctx context.Context, ns string) error {
	c.Logger = slo.NewLogger(c.Logger)
	if c.Runner == nil {
		c.Runner = kubeutil.DefaultRunner{}
	}

	cmd := exec.Command(
		"kubectl", "delete", "pods",
		"-n", ns,
		"-l", c.LabelSelector,
		"--ignore-not-found=true",
		"--wait=false",
	)
	_, err := c.Runner.Run(ctx, c.Logger, cmd)
	// 최선의 노력(best-effort)이므로 에러를 하드 실패로 간주하지 않고 호출부에서 무시해도 됨.
	return err
}

// parseSelectorToLabels converts a simple equality label selector string into a
// map. Only key=value pairs are supported; set-based selectors are ignored.
// Example: "app.kubernetes.io/managed-by=kube-slint,slint-run-id=abc" →
//
//	{"app.kubernetes.io/managed-by": "kube-slint", "slint-run-id": "abc"}
func parseSelectorToLabels(selector string) map[string]string {
	m := map[string]string{}
	for _, pair := range strings.Split(selector, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		idx := strings.IndexByte(pair, '=')
		if idx < 0 {
			continue
		}
		m[strings.TrimSpace(pair[:idx])] = strings.TrimSpace(pair[idx+1:])
	}
	return m
}

// podPhase returns the pod's current .status.phase (e.g. "Running",
// "Succeeded", "Failed").
func (c *Client) podPhase(ctx context.Context, ns, podName string) (string, error) {
	cmd := exec.Command(
		"kubectl", "get", "pod", podName,
		"-n", ns,
		"-o", "jsonpath={.status.phase}",
	)
	out, err := c.Runner.Run(ctx, c.Logger, cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
