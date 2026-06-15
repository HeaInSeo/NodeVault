package kubeutil

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/HeaInSeo/kube-slint/pkg/slo"
)

// WaitOptions controls polling behavior.
// WaitOptions는 폴링 동작을 제어합니다.
type WaitOptions struct {
	Timeout  time.Duration // 전체 타임아웃 (0 => 기본값)
	Interval time.Duration // 폴링 간격 (0 => 기본값)
}

// withDefaults applies safe defaults.
// withDefaults는 안전한 기본값을 적용합니다.
func (o WaitOptions) withDefaults() WaitOptions {
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	if o.Interval <= 0 {
		o.Interval = 5 * time.Second
	}
	return o
}

// WaitControllerManagerReady waits until controller-manager pod is Ready.
// Assumes label selector "control-plane=controller-manager" (kubebuilder default).
// WaitControllerManagerReady는 controller-manager 파드가 준비될 때까지 기다립니다.
// 레이블 셀렉터 "control-plane=controller-manager" (kubebuilder 기본값)를 가정합니다.
// Original:
// func WaitControllerManagerReady(ctx context.Context, logger slo.Logger, r CmdRunner,
//
//	ns string, opts WaitOptions) error {
func WaitControllerManagerReady(
	ctx context.Context,
	logger slo.Logger,
	r CmdRunner,
	ns string,
	opts WaitOptions,
) error {
	return WaitPodContainerReadyByLabel(
		ctx,
		logger,
		r,
		ns,
		"control-plane=controller-manager",
		0,
		0,
		opts,
	)
}

// WaitPodContainerReadyByLabel waits until the first matching pod's Nth container is ready.
// podIndex/containerIndex default to 0 in most kubebuilder setups.
// WaitPodContainerReadyByLabel은 일치하는 첫 번째 파드의 N번째 컨테이너가 준비될 때까지 기다립니다.
// podIndex/containerIndex는 대부분의 kubebuilder 설정에서 0을 기본값으로 사용합니다.
// Original:
// func WaitPodContainerReadyByLabel(ctx context.Context, logger slo.Logger, r CmdRunner,
//
//	ns string, labelSelector string, podIndex int, containerIndex int, opts WaitOptions) error {
func WaitPodContainerReadyByLabel(
	ctx context.Context,
	logger slo.Logger,
	r CmdRunner,
	ns string,
	labelSelector string,
	podIndex int,
	containerIndex int,
	opts WaitOptions,
) error {
	logger = slo.NewLogger(logger)
	if r == nil {
		r = DefaultRunner{}
	}
	opts = opts.withDefaults()

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	jsonpath := fmt.Sprintf(
		"{.items[%d].status.containerStatuses[%d].ready}",
		podIndex,
		containerIndex,
	)

	err := pollUntil(waitCtx, opts.Interval, func() (bool, error) {
		cmd := exec.Command(
			"kubectl", "get", "pods",
			"-n", ns,
			"-l", labelSelector,
			"-o", "jsonpath="+jsonpath,
		)
		out, err := r.Run(waitCtx, logger, cmd)
		if err != nil {
			logger.Logf("wait pod ready: not ready yet: %v", err)
			return false, err
		}
		return strings.TrimSpace(out) == "true", nil
	})
	if err != nil {
		return fmt.Errorf(
			"timeout waiting pod ready (ns=%s selector=%q): %w",
			ns, labelSelector, err,
		)
	}
	return nil
}

// WaitServiceHasEndpoints waits until the Endpoints object has at least one address.
// WaitServiceHasEndpoints는 Endpoints 객체에 적어도 하나의 주소가 있을 때까지 기다립니다.
// Original:
// func WaitServiceHasEndpoints(ctx context.Context, logger slo.Logger, r CmdRunner,
//
//	ns string, svc string, opts WaitOptions) error {
func WaitServiceHasEndpoints(
	ctx context.Context,
	logger slo.Logger,
	r CmdRunner,
	ns string,
	svc string,
	opts WaitOptions,
) error {
	logger = slo.NewLogger(logger)
	if r == nil {
		r = DefaultRunner{}
	}
	opts = opts.withDefaults()

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	err := pollUntil(waitCtx, opts.Interval, func() (bool, error) {
		cmd := exec.Command(
			"kubectl", "get", "endpoints", svc,
			"-n", ns,
			"-o", "jsonpath={.subsets[0].addresses[0].ip}",
		)
		out, err := r.Run(waitCtx, logger, cmd)
		if err != nil {
			logger.Logf("wait endpoints: not ready yet: %v", err)
			return false, err
		}
		return strings.TrimSpace(out) != "", nil
	})
	if err != nil {
		return fmt.Errorf(
			"timeout waiting endpoints (ns=%s svc=%s): %w",
			ns, svc, err,
		)
	}
	return nil
}
