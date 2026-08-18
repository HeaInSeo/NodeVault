// Package validate implements L3 (kind dry-run) and L4 (smoke run) validation.
package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

const (
	smokeNamespace   = "nodevault-smoke"
	smokeTimeout     = 5 * time.Minute
	smokeJobTTL      = int32(120)
	smokeJobDeadline = int64(300)

	// reachabilityTimeout bounds a single startup reachability probe against the
	// API server. It is applied as the REST client timeout of the dedicated
	// probe client (see newReachabilityProbe) so the /version request is
	// actually canceled at the transport if the server never responds.
	reachabilityTimeout = 10 * time.Second
)

// watchReconnectBackoff bounds how quickly waitForJob retries after a watch
// reconnect — whether the previous attempt failed to establish or closed
// cleanly — so repeated churn doesn't hammer the API server in a tight loop.
// It is a var (not a const) so tests can shrink it for deterministic, fast runs.
var watchReconnectBackoff = 2 * time.Second

// Service implements ValidateServiceServer.
type Service struct {
	nfv1.UnimplementedValidateServiceServer
	kube kubernetes.Interface
}

// NewService creates a ValidateService using local kubeconfig.
func NewService() (*Service, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	restCfg, err := cfg.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}
	probe, err := newReachabilityProbe(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s probe client: %w", err)
	}
	return newServiceFromClientWithProbe(kube, probe)
}

// NewInClusterService creates a ValidateService using the in-cluster ServiceAccount
// token automatically mounted by Kubernetes at /var/run/secrets/kubernetes.io/serviceaccount.
// Used when NODEVAULT_RUNTIME_MODE=incluster.
func NewInClusterService() (*Service, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}
	probe, err := newReachabilityProbe(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s probe client: %w", err)
	}
	return newServiceFromClientWithProbe(kube, probe)
}

// newReachabilityProbe builds a client dedicated to the startup reachability
// probe whose REST timeout is bounded by reachabilityTimeout, so the /version
// request is actually canceled at the transport if the API server never
// responds — instead of leaking a goroutine and socket for the process
// lifetime. It is kept separate from the long-lived watch client, whose Watch
// requests must not carry a short client-level timeout.
func newReachabilityProbe(restCfg *rest.Config) (kubernetes.Interface, error) {
	probeCfg := rest.CopyConfig(restCfg)
	probeCfg.Timeout = reachabilityTimeout
	return kubernetes.NewForConfig(probeCfg)
}

// newServiceFromClient wraps kube in a Service, using kube itself as the
// reachability probe. Tests construct services this way with a fake client.
func newServiceFromClient(kube kubernetes.Interface) (*Service, error) {
	return newServiceFromClientWithProbe(kube, kube)
}

// newServiceFromClientWithProbe wraps kube in a Service, first confirming the
// cluster is actually reachable via a single probe. A well-formed kubeconfig
// that points at an unreachable cluster (network partition, stale cert, wrong
// context) would otherwise only surface later, per-RPC, as an ErrorMessage
// string; this fails loud at startup, the same way a missing/malformed
// kubeconfig already does.
//
// NOTE: on failure the caller (cmd/controlplane) currently logs and skips
// registering ValidateService while the process stays healthy, so a transient
// startup outage leaves the service silently unregistered until a manual
// restart. Recovering from that (background retry / readiness / restart) is a
// registration+readiness contract decision tracked in
// DESIGN-NV-VALIDATE-REGISTRATION-READINESS-20260818, out of this package's scope.
func newServiceFromClientWithProbe(kube, probe kubernetes.Interface) (*Service, error) {
	if err := checkReachable(probe); err != nil {
		return nil, fmt.Errorf("cluster unreachable: %w", err)
	}
	return &Service{kube: kube}, nil
}

// checkReachable performs a single lightweight probe against the API server
// (ServerVersion) to verify it is actually reachable, not just that the
// kubeconfig parsed into a valid rest.Config. The request is bounded by the
// probe client's REST timeout (see newReachabilityProbe), so it cannot hang or
// leak.
func checkReachable(probe kubernetes.Interface) error {
	if _, err := probe.Discovery().ServerVersion(); err != nil {
		return fmt.Errorf("server version: %w", err)
	}
	return nil
}

// DryRunJob submits a Job with server-side dry-run. Called internally by BuildService.
func (s *Service) DryRunJob(ctx context.Context, job *batchv1.Job) *nfv1.DryRunResult {
	j := job.DeepCopy()
	j.Name = "dry-" + job.Name

	_, err := s.kube.BatchV1().Jobs(job.Namespace).Create(ctx, j, metav1.CreateOptions{
		DryRun: []string{metav1.DryRunAll},
	})
	if err != nil {
		return &nfv1.DryRunResult{Success: false, ErrorMessage: err.Error()}
	}
	slog.Info("L3 dry-run passed", "job", j.Name)
	return &nfv1.DryRunResult{Success: true}
}

// SmokeRunJob creates a real K8s Job, waits for completion, and collects logs.
// Called internally by BuildService after L3 passes.
func (s *Service) SmokeRunJob(ctx context.Context, job *batchv1.Job) *nfv1.SmokeRunResult {
	if err := s.ensureNamespace(ctx, smokeNamespace); err != nil {
		return &nfv1.SmokeRunResult{Success: false, ErrorMessage: err.Error()}
	}

	j := job.DeepCopy()
	j.Namespace = smokeNamespace

	created, err := s.kube.BatchV1().Jobs(smokeNamespace).Create(ctx, j, metav1.CreateOptions{})
	if err != nil {
		return &nfv1.SmokeRunResult{Success: false, ErrorMessage: "create smoke job: " + err.Error()}
	}
	slog.Info("smoke job created", "job", created.Name)

	defer func() {
		bg := context.Background()
		pp := metav1.DeletePropagationForeground
		_ = s.kube.BatchV1().Jobs(smokeNamespace).Delete(bg, created.Name, metav1.DeleteOptions{
			PropagationPolicy: &pp,
		})
	}()

	result := s.waitForJob(ctx, created.Name)
	if result.Success {
		result.LogOutput = s.collectLogs(ctx, created.Name)
	}
	return result
}

// DryRun implements ValidateServiceServer for external callers (YAML manifest).
func (s *Service) DryRun(ctx context.Context, req *nfv1.DryRunRequest) (*nfv1.DryRunResult, error) {
	job, err := parseJobManifest(req.ManifestYaml)
	if err != nil {
		return &nfv1.DryRunResult{Success: false, ErrorMessage: "parse manifest: " + err.Error()}, nil
	}
	return s.DryRunJob(ctx, job), nil
}

// SmokeRun implements ValidateServiceServer for external callers (YAML manifest).
func (s *Service) SmokeRun(ctx context.Context, req *nfv1.SmokeRunRequest) (*nfv1.SmokeRunResult, error) {
	job, err := parseJobManifest(req.ManifestYaml)
	if err != nil {
		return &nfv1.SmokeRunResult{Success: false, ErrorMessage: "parse manifest: " + err.Error()}, nil
	}
	return s.SmokeRunJob(ctx, job), nil
}

// ensureNamespace creates ns if it does not exist.
func (s *Service) ensureNamespace(ctx context.Context, ns string) error {
	_, err := s.kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	obj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	_, err = s.kube.CoreV1().Namespaces().Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create namespace %s: %w", ns, err)
	}
	return nil
}

// waitForJob watches job until Succeeded or Failed (or context done). If the
// watch channel closes for a reason other than the overall deadline expiring —
// e.g. an API server restart, a network blip, or the watch's normal
// relist/timeout expiry — this does not immediately report failure. Instead it
// re-establishes a new watch (re-list + re-watch, the standard client-go
// reconnect pattern) and keeps waiting, bounded by the same overall smoke-run
// deadline. Only if re-establishing the watch keeps failing until that
// deadline is reached do we give up, and the resulting error makes clear the
// failure is about watch connectivity, not a known Job outcome.
func (s *Service) waitForJob(ctx context.Context, jobName string) *nfv1.SmokeRunResult {
	timeoutCtx, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()

	var lastReconnectErr error
	interrupted := false
	for {
		result, watchErr := s.watchJobOnce(timeoutCtx, jobName, interrupted)
		if result != nil {
			return result
		}
		// Reaching here means the watch either closed cleanly without a terminal
		// event or failed to establish: observation is now interrupted, so a
		// subsequent deadline is no longer proof the Job actually timed out.
		interrupted = true
		if watchErr == nil {
			// Channel closed cleanly (relist/timeout expiry or a dropped
			// connection). Back off before reconnecting so a watch that
			// immediately EOFs on every call doesn't spin against the API
			// server; the relist at the top of the next watchJobOnce still
			// catches a terminal state promptly.
			select {
			case <-timeoutCtx.Done():
				return s.deadlineResult(jobName, interrupted)
			case <-time.After(watchReconnectBackoff):
			}
			continue
		}
		lastReconnectErr = watchErr
		select {
		case <-timeoutCtx.Done():
			return &nfv1.SmokeRunResult{
				Success: false,
				ErrorMessage: fmt.Sprintf(
					"smoke run watch reconnect failed repeatedly (job outcome unknown): %v",
					lastReconnectErr,
				),
			}
		case <-time.After(watchReconnectBackoff):
		}
	}
}

// watchJobOnce relists the Job, then establishes a single watch session on it
// and consumes events until either a terminal outcome is reached (Job
// succeeded/failed/deleted, or the overall context is done) or the watch channel
// closes.
//
// It relists (GET) first: if the first watch closed and the Job reached a
// terminal state during the gap before this watch is established, a fresh Watch
// carrying no resourceVersion could miss that transition entirely. So this GETs
// the current Job and, if it is already terminal (or gone), returns that outcome
// immediately; otherwise it starts the Watch from the observed resourceVersion
// so no event is missed across the gap.
//
// interrupted reports whether observation was already interrupted at least once
// before this attempt; it only affects how a deadline hit is reported (see
// deadlineResult). It returns:
//   - (result, nil) when a terminal (or deadline) outcome was reached — the
//     caller should return result directly.
//   - (nil, nil) when the channel closed without a terminal event — the caller
//     should reconnect.
//   - (nil, err) when the Job could not be relisted or the watch could not be
//     established — the caller should retry after a backoff, bounded by the
//     overall deadline.
func (s *Service) watchJobOnce(ctx context.Context, jobName string, interrupted bool) (*nfv1.SmokeRunResult, error) {
	job, err := s.kube.BatchV1().Jobs(smokeNamespace).Get(ctx, jobName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &nfv1.SmokeRunResult{Success: false, ErrorMessage: "smoke job deleted unexpectedly"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("relist smoke job: %w", err)
	}
	if result := jobResultFromConditions(jobName, job); result != nil {
		return result, nil
	}

	watcher, err := s.kube.BatchV1().Jobs(smokeNamespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + jobName,
		ResourceVersion: job.ResourceVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("watch smoke job: %w", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return s.deadlineResult(jobName, interrupted), nil
		case ev, ok := <-watcher.ResultChan():
			if !ok {
				return nil, nil
			}
			if result := handleJobEvent(jobName, ev); result != nil {
				return result, nil
			}
		}
	}
}

// deadlineResult decides what to report when the overall smoke-run deadline is
// reached. A definite "timed out" is only justified when the final state is
// confirmed: a fresh GET shows the Job still non-terminal, or observation was
// never interrupted (an uninterrupted watch saw everything up to the deadline).
// If observation was interrupted and the final state cannot be confirmed, the
// terminal event may have occurred during a watch gap, so the outcome is
// reported as unknown rather than a definite timeout.
func (s *Service) deadlineResult(jobName string, interrupted bool) *nfv1.SmokeRunResult {
	// The caller's context is already done; use a fresh bounded context for the
	// confirming GET.
	ctx, cancel := context.WithTimeout(context.Background(), reachabilityTimeout)
	defer cancel()

	job, err := s.kube.BatchV1().Jobs(smokeNamespace).Get(ctx, jobName, metav1.GetOptions{})
	switch {
	case err == nil:
		if result := jobResultFromConditions(jobName, job); result != nil {
			return result
		}
		// Confirmed still non-terminal at the deadline: a genuine timeout.
		return &nfv1.SmokeRunResult{Success: false, ErrorMessage: "smoke run timed out"}
	case apierrors.IsNotFound(err):
		return &nfv1.SmokeRunResult{Success: false, ErrorMessage: "smoke job deleted unexpectedly"}
	default:
		if interrupted {
			return &nfv1.SmokeRunResult{
				Success: false,
				ErrorMessage: "smoke run outcome unknown: observation was interrupted and the " +
					"final Job state could not be confirmed before the deadline",
			}
		}
		// Uninterrupted watch observed continuously up to the deadline and never
		// saw a terminal event: a genuine timeout even though this confirming
		// GET failed.
		return &nfv1.SmokeRunResult{Success: false, ErrorMessage: "smoke run timed out"}
	}
}

// handleJobEvent inspects a single watch event and returns a terminal
// SmokeRunResult if the event indicates the Job finished (or was deleted), or
// nil if waiting should continue.
func handleJobEvent(jobName string, ev watch.Event) *nfv1.SmokeRunResult {
	if ev.Type == watch.Deleted {
		return &nfv1.SmokeRunResult{Success: false, ErrorMessage: "smoke job deleted unexpectedly"}
	}
	j, ok := ev.Object.(*batchv1.Job)
	if !ok {
		return nil
	}
	return jobResultFromConditions(jobName, j)
}

// jobResultFromConditions returns a terminal SmokeRunResult if j's conditions
// show it completed or failed, or nil if it is still non-terminal.
func jobResultFromConditions(jobName string, j *batchv1.Job) *nfv1.SmokeRunResult {
	for _, cond := range j.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			slog.Warn("smoke run failed", "job", jobName, "msg", cond.Message)
			return &nfv1.SmokeRunResult{Success: false, ErrorMessage: cond.Message}
		}
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			slog.Info("smoke run succeeded", "job", jobName)
			return &nfv1.SmokeRunResult{Success: true, ExitCode: 0}
		}
	}
	return nil
}

// collectLogs retrieves logs from the first pod of the smoke job.
func (s *Service) collectLogs(ctx context.Context, jobName string) string {
	time.Sleep(1 * time.Second)
	pods, err := s.kube.CoreV1().Pods(smokeNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	podName := pods.Items[0].Name
	rc, err := s.kube.CoreV1().Pods(smokeNamespace).GetLogs(podName, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return ""
	}
	defer func() {
		_ = rc.Close()
	}()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if rerr == io.EOF || rerr != nil {
			break
		}
	}
	return sb.String()
}

// parseJobManifest converts a YAML/JSON manifest string to a batchv1.Job.
func parseJobManifest(manifest string) (*batchv1.Job, error) {
	jsonBytes, err := utilyaml.ToJSON([]byte(manifest))
	if err != nil {
		return nil, fmt.Errorf("yaml to json: %w", err)
	}
	var job batchv1.Job
	if err := json.Unmarshal(jsonBytes, &job); err != nil {
		return nil, fmt.Errorf("unmarshal job: %w", err)
	}
	return &job, nil
}

// SmokeJobSpec builds a minimal smoke-run Job for the given image.
// Called by BuildService to create the spec passed to DryRunJob/SmokeRunJob.
func SmokeJobSpec(jobName, imageWithDigest string) *batchv1.Job {
	ttl := smokeJobTTL
	deadline := smokeJobDeadline
	backoff := int32(0)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: smokeNamespace,
			Labels:    map[string]string{"app": "nodevault-smoke"},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "smoke",
							Image:   imageWithDigest,
							Command: []string{"sh", "-c", "echo smoke-ok"},
						},
					},
				},
			},
		},
	}
}
