package build

// k8sjob_builder.go — run Buildah as a K8s user-namespace Job.
//
// NodeVault Pod spawns a Buildah K8s Job (quay.io/buildah/stable), waits for
// completion, reads the pushed digest from pod logs, and returns it to the caller.
//
// Current lab constraints:
//   - --tls-verify=false (Harbor self-signed cert)
//   - Harbor creds in env vars (not mounted from a properly managed secret)
//   - vfs storage driver by default; overlay is still blocked by mount propagation
//     in the current K8s user namespace lab.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
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
	defaultBuilderImage = "quay.io/buildah/stable:latest"
	buildJobTimeout     = 2 * time.Hour
	buildStorageDriver  = "vfs"

	// defaultNanBinaryPath is where the nan (node-artifact-runtime) static binary
	// is expected to live inside the NodeVault container image, unless overridden.
	defaultNanBinaryPath = "/opt/nodevault/bin/nan"

	// nanImagePath is the path nan is injected to inside every built tool image.
	nanImagePath = "/usr/local/bin/nan"
)

var buildJobTTL = int32(300)
var buildJobBackoff = int32(0)
var buildJobDeadline = int64(buildJobTimeout.Seconds())
var boolFalse = false
var int64Zero = int64(0)

// K8sJobBuilder implements Builder by submitting a K8s Job that runs buildah.
type K8sJobBuilder struct {
	kube         kubernetes.Interface
	builderImage string
	harborUser   string
	harborPass   string

	// nanBinaryPath is the path to the nan static binary on the host/NodeVault
	// container image, read into the build ConfigMap and copied into every
	// built tool image at nanImagePath. Configurable via NODEVAULT_NAN_BINARY.
	nanBinaryPath string

	// nanVersion identifies the injected nan binary, recorded on ToolBuildRecord.
	// Configurable via NODEVAULT_NAN_VERSION; resolved once at construction time.
	nanVersion string
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

	nanBinaryPath := os.Getenv("NODEVAULT_NAN_BINARY")
	if nanBinaryPath == "" {
		nanBinaryPath = defaultNanBinaryPath
	}

	return &K8sJobBuilder{
		kube:          kube,
		builderImage:  img,
		harborUser:    os.Getenv("HARBOR_USER"),
		harborPass:    os.Getenv("HARBOR_PASS"),
		nanBinaryPath: nanBinaryPath,
		nanVersion:    resolveNanVersion(nanBinaryPath),
	}, nil
}

// resolveNanVersion determines the version of the nan binary that will be injected
// into built tool images. It first tries `<nanBinaryPath> version` (nan supports a
// "version" subcommand printing e.g. "v0.1.5"); if the binary is missing or the
// command fails (common in dev/test environments without nan installed), it falls
// back to the NODEVAULT_NAN_VERSION env var, then to "" (best-effort, non-fatal —
// ToolBuildRecord.NanVersion is simply left blank).
func resolveNanVersion(nanBinaryPath string) string {
	if out, err := exec.Command(nanBinaryPath, "version").Output(); err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			return v
		}
	}
	return os.Getenv("NODEVAULT_NAN_VERSION")
}

// Build submits a K8s Job that builds and pushes the image, then returns the digest.
// The image's final stage always has the nan (node-artifact-runtime) binary copied
// to nanImagePath, regardless of the caller-supplied Dockerfile content.
func (b *K8sJobBuilder) Build(
	ctx context.Context, dockerfileContent, outputRef string,
) (imageID, digest, nanVersion string, err error) {
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())[:8]
	jobName := "nvbuild-" + suffix
	cmName := "nvbuild-df-" + suffix

	if ensureErr := b.ensureNamespace(ctx); ensureErr != nil {
		return "", "", "", fmt.Errorf("k8s-job builder: ensure namespace: %w", ensureErr)
	}

	nanBytes, nanErr := os.ReadFile(b.nanBinaryPath)
	if nanErr != nil {
		return "", "", "", fmt.Errorf("k8s-job builder: read nan binary %q: %w", b.nanBinaryPath, nanErr)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: buildsNamespace,
			Labels:    map[string]string{"app": "nodevault-builder"},
		},
		Data:       map[string]string{"Dockerfile": injectNanCopyStep(dockerfileContent)},
		BinaryData: map[string][]byte{"nan": nanBytes},
	}
	if _, cmErr := b.kube.CoreV1().ConfigMaps(buildsNamespace).Create(ctx, cm, metav1.CreateOptions{}); cmErr != nil {
		return "", "", "", fmt.Errorf("k8s-job builder: create dockerfile configmap: %w", cmErr)
	}
	defer func() {
		bg := context.Background()
		_ = b.kube.CoreV1().ConfigMaps(buildsNamespace).Delete(bg, cmName, metav1.DeleteOptions{})
	}()

	job := b.buildJob(jobName, cmName, outputRef)
	if _, jobErr := b.kube.BatchV1().Jobs(buildsNamespace).Create(ctx, job, metav1.CreateOptions{}); jobErr != nil {
		return "", "", "", fmt.Errorf("k8s-job builder: create build job: %w", jobErr)
	}
	defer func() {
		bg := context.Background()
		pp := metav1.DeletePropagationBackground
		_ = b.kube.BatchV1().Jobs(buildsNamespace).Delete(bg, jobName, metav1.DeleteOptions{PropagationPolicy: &pp})
	}()

	slog.Info("k8s build job created", "job", jobName, "destination", outputRef, "nan_version", b.nanVersion)

	digest, err = b.waitAndCollectDigest(ctx, jobName)
	if err != nil {
		return "", "", "", fmt.Errorf("k8s-job builder: %w", err)
	}
	slog.Info("k8s build job complete", "job", jobName, "digest", digest)
	return jobName, digest, b.nanVersion, nil
}

// injectNanCopyStep appends a COPY+RUN step to the final build stage of dockerfileContent
// so the nan binary (mounted into the build context at /workspace/nan via ConfigMap
// BinaryData) ends up at nanImagePath in every built tool image. The step is inserted
// right after the last "FROM" line, so it always lands in the final stage of a
// multi-stage build rather than an intermediate builder stage.
func injectNanCopyStep(dockerfileContent string) string {
	lines := strings.Split(dockerfileContent, "\n")
	lastFrom := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "FROM ") || strings.TrimSpace(line) == "FROM" {
			lastFrom = i
		}
	}
	nanStep := []string{
		"COPY nan " + nanImagePath,
		"RUN chmod +x " + nanImagePath,
	}
	if lastFrom == -1 {
		// No FROM found (malformed Dockerfile) — fall back to prepending; buildah
		// will surface the real error to the caller via the build job failure.
		return strings.Join(append(nanStep, lines...), "\n")
	}
	out := make([]string, 0, len(lines)+len(nanStep))
	out = append(out, lines[:lastFrom+1]...)
	out = append(out, nanStep...)
	out = append(out, lines[lastFrom+1:]...)
	return strings.Join(out, "\n")
}

// buildJob constructs the K8s Job spec for the buildah build+push step.
func (b *K8sJobBuilder) buildJob(jobName, cmName, destination string) *batchv1.Job {
	// Shell script: build image, push to registry, print digest marker to stdout.
	storageDriver := os.Getenv("NODEVAULT_BUILDER_STORAGE_DRIVER")
	if storageDriver == "" {
		storageDriver = buildStorageDriver
	}
	cacheRef := os.Getenv("NODEVAULT_BUILDER_CACHE_REF")
	cacheArgs := ""
	if cacheRef != "" {
		cacheArgs = ` --cache-from "$CACHE_REF" --cache-to "$CACHE_REF"`
	}

	buildScript := strings.Join([]string{
		"set -e",
		`mkdir -p "$HOME/.config" "$XDG_DATA_HOME/containers" /home/build/.config /home/build/.local/share/containers /tmp/run/containers/storage /storage/.local/share/containers/storage`,
		`chown -R 0:0 /home/build`,
		`cat > /tmp/storage.conf <<EOF`,
		`[storage]`,
		`driver = "` + storageDriver + `"`,
		`runroot = "/tmp/run/containers/storage"`,
		`graphroot = "/storage/.local/share/containers/storage"`,
		`rootless_storage_path = "/storage/.local/share/containers/storage"`,
		`[storage.options.pull_options]`,
		`enable_partial_images = "true"`,
		`EOF`,
		`CONTAINERS_STORAGE_CONF=/tmp/storage.conf buildah bud --tls-verify=false --isolation chroot --runtime crun --layers` + cacheArgs + ` -t "$DESTINATION" /workspace/`,
		`CONTAINERS_STORAGE_CONF=/tmp/storage.conf buildah push --tls-verify=false --creds "$HARBOR_USER:$HARBOR_PASS" --digestfile=/tmp/digest.txt "$DESTINATION"`,
		`printf 'BUILD_DIGEST=%s\n' "$(cat /tmp/digest.txt)"`,
	}, "\n")
	hostUsers := false

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
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &boolFalse,
					HostUsers:                    &hostUsers,
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{
						{
							Name:    "builder",
							Image:   b.builderImage,
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{buildScript},
							Env: appendUserNamespaceBuildEnvironment([]corev1.EnvVar{
								{Name: "DESTINATION", Value: destination},
								{Name: "HARBOR_USER", Value: b.harborUser},
								{Name: "HARBOR_PASS", Value: b.harborPass},
								{Name: "CACHE_REF", Value: cacheRef},
							}),
							SecurityContext: &corev1.SecurityContext{
								Privileged:               &boolFalse,
								AllowPrivilegeEscalation: &boolFalse,
								RunAsUser:                &int64Zero,
								RunAsGroup:               &int64Zero,
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
									Add:  userNamespaceBuildCapabilities(),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/workspace"},
								{Name: "container-storage", MountPath: "/storage"},
								{Name: "tmp", MountPath: "/tmp"},
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
						{
							Name:         "tmp",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
				},
			},
		},
	}
}

// userNamespaceBuildEnv returns the environment variables required for rootless
// buildah in user-namespace mode inside a K8s Job pod. Values are sourced from
// podbridge5 v0.1.2 DefaultUserNamespaceBuildEnvironment() — inlined here so
// NodeVault remains pinned to podbridge5 v0.1.1 which lacks these exports.
func appendUserNamespaceBuildEnvironment(env []corev1.EnvVar) []corev1.EnvVar {
	for _, kv := range [][2]string{
		{"_CONTAINERS_USERNS_CONFIGURED", "done"},
		{"_CONTAINERS_ROOTLESS_UID", "1000"},
		{"_CONTAINERS_ROOTLESS_GID", "1000"},
		{"BUILDAH_ISOLATION", "chroot"},
		{"BUILDAH_RUNTIME", "crun"},
		{"HOME", "/tmp/buildhome"},
		{"XDG_CONFIG_HOME", "/tmp/buildhome/.config"},
		{"XDG_DATA_HOME", "/tmp/buildhome/.local/share"},
	} {
		env = append(env, corev1.EnvVar{Name: kv[0], Value: kv[1]})
	}
	return env
}

// userNamespaceBuildCapabilities returns the Linux capabilities required for
// rootless buildah in user-namespace mode. Values sourced from podbridge5 v0.1.2
// DefaultUserNamespaceBuildCapabilities() — inlined to stay on v0.1.1.
func userNamespaceBuildCapabilities() []corev1.Capability {
	return []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETFCAP", "SYS_CHROOT"}
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
