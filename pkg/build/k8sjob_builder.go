package build

// k8sjob_builder.go — Option A spike: run podbridge5/buildah as a K8s Job.
//
// NodeVault Pod spawns a privileged K8s Job (quay.io/buildah/stable), waits for
// completion, reads the pushed digest from pod logs, and returns it to the caller.
//
// Spike constraints (must NOT be carried forward to production):
//   - --tls-verify=false (Harbor self-signed cert)
//   - privileged: true  (required for overlay/fuse-overlayfs inside K8s)
//   - Harbor creds in env vars (not mounted from a properly managed secret)

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	buildsNamespace     = "nodevault-builds"
	defaultBuilderImage = "quay.io/buildah/stable:v1.37.1"
	buildJobTimeout     = 15 * time.Minute
)

var buildJobTTL = int32(300)
var buildJobBackoff = int32(0)
var buildJobDeadline = int64(buildJobTimeout.Seconds())
var privileged = true

// K8sJobBuilder implements Builder by submitting a K8s Job that runs buildah.
type K8sJobBuilder struct {
	kube         kubernetes.Interface
	builderImage string
	harborUser   string
	harborPass   string
}

// newK8sJobBuilder creates a K8sJobBuilder using incluster SA token or local kubeconfig.
func newK8sJobBuilder(runtimeMode string) (*K8sJobBuilder, error) {
	var restCfg *rest.Config
	var err error

	if runtimeMode == "incluster" {
		restCfg, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("k8s-job builder: in-cluster config: %w", err)
		}
	} else {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
		restCfg, err = cfg.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("k8s-job builder: kubeconfig: %w", err)
		}
	}

	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s-job builder: k8s client: %w", err)
	}

	img := os.Getenv("NODEVAULT_BUILDER_IMAGE")
	if img == "" {
		img = defaultBuilderImage
	}

	return &K8sJobBuilder{
		kube:         kube,
		builderImage: img,
		harborUser:   os.Getenv("HARBOR_USER"),
		harborPass:   os.Getenv("HARBOR_PASS"),
	}, nil
}

// Build submits a K8s Job that builds and pushes the image, then returns the digest.
func (b *K8sJobBuilder) Build(
	ctx context.Context, dockerfileContent, outputRef string,
) (imageID, digest string, err error) {
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())[:8]
	jobName := "nvbuild-" + suffix
	cmName := "nvbuild-df-" + suffix

	if ensureErr := b.ensureNamespace(ctx); ensureErr != nil {
		return "", "", fmt.Errorf("k8s-job builder: ensure namespace: %w", ensureErr)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: buildsNamespace,
			Labels:    map[string]string{"app": "nodevault-builder"},
		},
		Data: map[string]string{"Dockerfile": dockerfileContent},
	}
	if _, cmErr := b.kube.CoreV1().ConfigMaps(buildsNamespace).Create(ctx, cm, metav1.CreateOptions{}); cmErr != nil {
		return "", "", fmt.Errorf("k8s-job builder: create dockerfile configmap: %w", cmErr)
	}
	defer func() {
		bg := context.Background()
		_ = b.kube.CoreV1().ConfigMaps(buildsNamespace).Delete(bg, cmName, metav1.DeleteOptions{})
	}()

	job := b.buildJob(jobName, cmName, outputRef)
	if _, jobErr := b.kube.BatchV1().Jobs(buildsNamespace).Create(ctx, job, metav1.CreateOptions{}); jobErr != nil {
		return "", "", fmt.Errorf("k8s-job builder: create build job: %w", jobErr)
	}
	defer func() {
		bg := context.Background()
		pp := metav1.DeletePropagationBackground
		_ = b.kube.BatchV1().Jobs(buildsNamespace).Delete(bg, jobName, metav1.DeleteOptions{PropagationPolicy: &pp})
	}()

	slog.Info("k8s build job created", "job", jobName, "destination", outputRef)

	digest, err = b.waitAndCollectDigest(ctx, jobName)
	if err != nil {
		return "", "", fmt.Errorf("k8s-job builder: %w", err)
	}
	slog.Info("k8s build job complete", "job", jobName, "digest", digest)
	return jobName, digest, nil
}

// buildJob constructs the K8s Job spec for the buildah build+push step.
func (b *K8sJobBuilder) buildJob(jobName, cmName, destination string) *batchv1.Job {
	// Shell script: build image, push to registry, print digest marker to stdout.
	buildScript := strings.Join([]string{
		"set -e",
		`buildah bud --tls-verify=false -t "$DESTINATION" /workspace/`,
		`buildah push --tls-verify=false --creds "$HARBOR_USER:$HARBOR_PASS" --digestfile=/tmp/digest.txt "$DESTINATION"`,
		`printf 'BUILD_DIGEST=%s\n' "$(cat /tmp/digest.txt)"`,
	}, "\n")

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: buildsNamespace,
			Labels:    map[string]string{"app": "nodevault-builder"},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &buildJobBackoff,
			TTLSecondsAfterFinished: &buildJobTTL,
			ActiveDeadlineSeconds:   &buildJobDeadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":      "nodevault-builder",
						"job-name": jobName,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "builder",
							Image:   b.builderImage,
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{buildScript},
							Env: []corev1.EnvVar{
								{Name: "DESTINATION", Value: destination},
								{Name: "HARBOR_USER", Value: b.harborUser},
								{Name: "HARBOR_PASS", Value: b.harborPass},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/workspace"},
								{Name: "container-storage", MountPath: "/var/lib/containers"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "workspace",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
								},
							},
						},
						{
							Name:         "container-storage",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
				},
			},
		},
	}
}

// waitAndCollectDigest watches the Job until Complete or Failed, then reads digest from pod logs.
func (b *K8sJobBuilder) waitAndCollectDigest(ctx context.Context, jobName string) (string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, buildJobTimeout)
	defer cancel()

	watcher, err := b.kube.BatchV1().Jobs(buildsNamespace).Watch(timeoutCtx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + jobName,
	})
	if err != nil {
		return "", fmt.Errorf("watch job %s: %w", jobName, err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return "", fmt.Errorf("build job %s timed out after %s", jobName, buildJobTimeout)
		case ev, ok := <-watcher.ResultChan():
			if !ok {
				return "", fmt.Errorf("build job %s watch channel closed unexpectedly", jobName)
			}
			if ev.Type == watch.Error {
				return "", fmt.Errorf("build job %s watch error event", jobName)
			}
			job, ok := ev.Object.(*batchv1.Job)
			if !ok {
				continue
			}
			for _, cond := range job.Status.Conditions {
				if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
					return "", fmt.Errorf("build job %s failed: %s", jobName, cond.Message)
				}
				if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
					return b.extractDigestFromLogs(ctx, jobName)
				}
			}
		}
	}
}

// extractDigestFromLogs reads the builder pod logs and parses the BUILD_DIGEST= marker.
func (b *K8sJobBuilder) extractDigestFromLogs(ctx context.Context, jobName string) (string, error) {
	time.Sleep(2 * time.Second) // brief wait for pod log flush after job completion

	pods, err := b.kube.CoreV1().Pods(buildsNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return "", fmt.Errorf("list pods for job %s: %w", jobName, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods found for job %s", jobName)
	}

	podName := pods.Items[0].Name
	rc, err := b.kube.CoreV1().Pods(buildsNamespace).GetLogs(podName, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("stream logs from pod %s: %w", podName, err)
	}
	defer func() { _ = rc.Close() }()

	logBytes, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("read pod logs: %w", err)
	}

	return parseDigestFromLogs(podName, logBytes)
}

// parseDigestFromLogs scans log output for a BUILD_DIGEST=sha256:... marker.
func parseDigestFromLogs(podName string, logs []byte) (string, error) {
	for _, line := range strings.Split(string(logs), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "BUILD_DIGEST=") {
			digest := strings.TrimPrefix(line, "BUILD_DIGEST=")
			if strings.HasPrefix(digest, "sha256:") {
				return digest, nil
			}
		}
	}
	return "", fmt.Errorf("BUILD_DIGEST=sha256:... not found in pod %s logs:\n%s", podName, string(logs))
}

// ensureNamespace creates the builds namespace if it does not exist.
func (b *K8sJobBuilder) ensureNamespace(ctx context.Context) error {
	if _, err := b.kube.CoreV1().Namespaces().Get(ctx, buildsNamespace, metav1.GetOptions{}); err == nil {
		return nil
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: buildsNamespace}}
	_, err := b.kube.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	return err
}

func (*K8sJobBuilder) Close() error { return nil }
