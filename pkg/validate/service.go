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
	"k8s.io/client-go/tools/clientcmd"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

const (
	smokeNamespace   = "nodevault-smoke"
	smokeTimeout     = 5 * time.Minute
	smokeJobTTL      = int32(120)
	smokeJobDeadline = int64(300)
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
	return &Service{kube: kube}, nil
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

// waitForJob watches job until Succeeded or Failed (or context done).
func (s *Service) waitForJob(ctx context.Context, jobName string) *nfv1.SmokeRunResult {
	timeoutCtx, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()

	watcher, err := s.kube.BatchV1().Jobs(smokeNamespace).Watch(timeoutCtx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + jobName,
	})
	if err != nil {
		return &nfv1.SmokeRunResult{Success: false, ErrorMessage: "watch smoke job: " + err.Error()}
	}
	defer watcher.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return &nfv1.SmokeRunResult{Success: false, ErrorMessage: "smoke run timed out"}
		case ev, ok := <-watcher.ResultChan():
			if !ok {
				return &nfv1.SmokeRunResult{Success: false, ErrorMessage: "watch channel closed"}
			}
			if ev.Type == watch.Deleted {
				return &nfv1.SmokeRunResult{Success: false, ErrorMessage: "smoke job deleted unexpectedly"}
			}
			j, ok := ev.Object.(*batchv1.Job)
			if !ok {
				continue
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
		}
	}
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
