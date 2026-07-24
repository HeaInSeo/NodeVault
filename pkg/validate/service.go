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

	// watchReconnectBackoff bounds how quickly waitForJob retries establishing
	// a new watch after a failed reconnect attempt (e.g. API server briefly
	// unreachable), so repeated failures don't hammer the API server in a
	// tight loop.
	watchReconnectBackoff = 2 * time.Second

	// reachabilityTimeout bounds the startup probe used to confirm the
	// configured cluster is actually reachable, not just that the kubeconfig
	// parsed successfully.
	reachabilityTimeout = 10 * time.Second
)

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
	return newServiceFromClient(kube)
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
	return newServiceFromClient(kube)
}

// newServiceFromClient wraps kube in a Service, first confirming the cluster is
// actually reachable. A well-formed kubeconfig that points at an unreachable
// cluster (network partition, stale cert, wrong context) would otherwise only
// surface later, per-RPC, as an ErrorMessage string — this makes that case
// fail loud at startup, the same way a missing/malformed kubeconfig already does.
func newServiceFromClient(kube kubernetes.Interface) (*Service, error) {
	if err := checkReachable(kube); err != nil {
		return nil, fmt.Errorf("cluster unreachable: %w", err)
	}
	return &Service{kube: kube}, nil
}

// checkReachable performs a lightweight, timeout-bounded probe against the API
// server (ServerVersion) to verify it is actually reachable, not just that the
// kubeconfig parsed into a valid rest.Config.
func checkReachable(kube kubernetes.Interface) error {
	ctx, cancel := context.WithTimeout(context.Background(), reachabilityTimeout)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := kube.Discovery().ServerVersion()
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("server version: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timed out after %s waiting for API server", reachabilityTimeout)
	}
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
	for {
		result, watchErr := s.watchJobOnce(timeoutCtx, jobName)
		if result != nil {
			return result
		}
		if watchErr == nil {
			// Channel closed cleanly with no terminal event observed (relist/
			// timeout expiry, or a dropped connection) — reconnect immediately.
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

// watchJobOnce establishes a single watch session on jobName and consumes
// events from it until either a terminal outcome is reached (Job
// succeeded/failed/deleted, or the overall context is done) or the watch
// channel closes. It returns:
//   - (result, nil) when a terminal outcome was reached — the caller should
//     return result directly.
//   - (nil, nil) when the channel closed without a terminal event — the caller
//     should reconnect.
//   - (nil, err) when the watch could not even be established — the caller
//     should retry after a backoff, bounded by the overall deadline.
func (s *Service) watchJobOnce(ctx context.Context, jobName string) (*nfv1.SmokeRunResult, error) {
	watcher, err := s.kube.BatchV1().Jobs(smokeNamespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + jobName,
	})
	if err != nil {
		return nil, fmt.Errorf("watch smoke job: %w", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return &nfv1.SmokeRunResult{Success: false, ErrorMessage: "smoke run timed out"}, nil
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
