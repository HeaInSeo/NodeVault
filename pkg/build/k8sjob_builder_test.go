package build

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newTestK8sJobBuilder() *K8sJobBuilder {
	return &K8sJobBuilder{
		kube:         fake.NewSimpleClientset(),
		builderImage: "quay.io/buildah/stable:v1.37.1",
		harborUser:   "testuser",
		harborPass:   "testpass",
	}
}

// ─── buildJob spec ────────────────────────────────────────────────────────────

func TestBuildJob_Namespace(t *testing.T) {
	b := newTestK8sJobBuilder()
	job := b.buildJob("test-job", "test-cm", "harbor.example.com/library/tool:latest")
	if job.Namespace != buildsNamespace {
		t.Errorf("namespace: got %q, want %q", job.Namespace, buildsNamespace)
	}
}

func TestBuildJob_Name(t *testing.T) {
	b := newTestK8sJobBuilder()
	job := b.buildJob("my-job", "cm", "dest")
	if job.Name != "my-job" {
		t.Errorf("name: got %q, want my-job", job.Name)
	}
}

func TestBuildJob_ContainerImage(t *testing.T) {
	b := newTestK8sJobBuilder()
	job := b.buildJob("j", "cm", "dest")
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != b.builderImage {
		t.Errorf("image: got %q, want %q", c.Image, b.builderImage)
	}
}

func TestBuildJob_Privileged(t *testing.T) {
	b := newTestK8sJobBuilder()
	job := b.buildJob("j", "cm", "dest")
	c := job.Spec.Template.Spec.Containers[0]
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Error("container must be privileged (buildah requirement)")
	}
}

func TestBuildJob_RestartPolicy(t *testing.T) {
	b := newTestK8sJobBuilder()
	job := b.buildJob("j", "cm", "dest")
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy: got %v, want Never", job.Spec.Template.Spec.RestartPolicy)
	}
}

func TestBuildJob_EnvVars(t *testing.T) {
	b := newTestK8sJobBuilder()
	dest := "harbor.example.com/library/mytool:latest"
	job := b.buildJob("j", "cm", dest)
	c := job.Spec.Template.Spec.Containers[0]

	envMap := make(map[string]string, len(c.Env))
	for _, e := range c.Env {
		envMap[e.Name] = e.Value
	}

	tests := []struct{ key, want string }{
		{"DESTINATION", dest},
		{"HARBOR_USER", "testuser"},
		{"HARBOR_PASS", "testpass"},
	}
	for _, tc := range tests {
		if envMap[tc.key] != tc.want {
			t.Errorf("env %s: got %q, want %q", tc.key, envMap[tc.key], tc.want)
		}
	}
}

func TestBuildJob_Volumes(t *testing.T) {
	b := newTestK8sJobBuilder()
	job := b.buildJob("j", "my-cm", "dest")

	volumes := make(map[string]corev1.VolumeSource)
	for _, v := range job.Spec.Template.Spec.Volumes {
		volumes[v.Name] = v.VolumeSource
	}
	if _, ok := volumes["workspace"]; !ok {
		t.Error("missing workspace volume")
	}
	if _, ok := volumes["container-storage"]; !ok {
		t.Error("missing container-storage volume")
	}
	if ws := volumes["workspace"]; ws.ConfigMap == nil || ws.ConfigMap.Name != "my-cm" {
		t.Errorf("workspace volume: want ConfigMap=my-cm, got %+v", ws)
	}
}

func TestBuildJob_BuildScript(t *testing.T) {
	b := newTestK8sJobBuilder()
	job := b.buildJob("j", "cm", "dest")
	script := job.Spec.Template.Spec.Containers[0].Args[0]

	required := []string{
		"set -e",
		"buildah bud",
		"buildah push",
		"BUILD_DIGEST=",
		"--digestfile",
	}
	for _, s := range required {
		if !strings.Contains(script, s) {
			t.Errorf("build script missing %q\nscript:\n%s", s, script)
		}
	}
}

// ─── parseDigestFromLogs ──────────────────────────────────────────────────────

func TestParseDigestFromLogs(t *testing.T) {
	tests := []struct {
		name    string
		logs    string
		want    string
		wantErr bool
	}{
		{
			name: "found at end",
			logs: "some build output\nBUILD_DIGEST=sha256:deadbeef\n",
			want: "sha256:deadbeef",
		},
		{
			name: "found with surrounding whitespace",
			logs: "  BUILD_DIGEST=sha256:abc123  ",
			want: "sha256:abc123",
		},
		{
			name:    "not found",
			logs:    "build output without marker",
			wantErr: true,
		},
		{
			name:    "wrong prefix — no sha256",
			logs:    "BUILD_DIGEST=md5:abc",
			wantErr: true,
		},
		{
			name:    "empty logs",
			logs:    "",
			wantErr: true,
		},
		{
			name: "multiple lines, digest last",
			logs: "step1\nstep2\nBUILD_DIGEST=sha256:cafebabe\ndone",
			want: "sha256:cafebabe",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDigestFromLogs("test-pod", []byte(tc.logs))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error: got %v, wantErr=%v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("digest: got %q, want %q", got, tc.want)
			}
		})
	}
}

// ─── ensureNamespace ──────────────────────────────────────────────────────────

func TestEnsureNamespace_AlreadyExists(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: buildsNamespace}}
	b := &K8sJobBuilder{kube: fake.NewSimpleClientset(ns)}
	if err := b.ensureNamespace(context.Background()); err != nil {
		t.Fatalf("unexpected error when namespace exists: %v", err)
	}
}

func TestEnsureNamespace_NotExists(t *testing.T) {
	client := fake.NewSimpleClientset()
	b := &K8sJobBuilder{kube: client}
	if err := b.ensureNamespace(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := client.CoreV1().Namespaces().Get(
		context.Background(), buildsNamespace, metav1.GetOptions{},
	); err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
}

func TestEnsureNamespace_Idempotent(t *testing.T) {
	b := &K8sJobBuilder{kube: fake.NewSimpleClientset()}
	if err := b.ensureNamespace(context.Background()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := b.ensureNamespace(context.Background()); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}
}

// ─── waitAndCollectDigest ─────────────────────────────────────────────────────

func TestWaitAndCollectDigest_JobFailed(t *testing.T) {
	client := fake.NewSimpleClientset()
	fw := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(fw, nil))

	b := &K8sJobBuilder{kube: client}
	go func() {
		time.Sleep(10 * time.Millisecond)
		fw.Modify(&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: buildsNamespace},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{
						Type:    batchv1.JobFailed,
						Status:  corev1.ConditionTrue,
						Message: "pod failed",
					},
				},
			},
		})
	}()

	_, err := b.waitAndCollectDigest(context.Background(), "test-job")
	if err == nil {
		t.Fatal("expected error for failed job, got nil")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("error should mention 'failed': %v", err)
	}
}

func TestWaitAndCollectDigest_WatchChannelClosed(t *testing.T) {
	client := fake.NewSimpleClientset()
	fw := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(fw, nil))

	b := &K8sJobBuilder{kube: client}
	go func() {
		time.Sleep(10 * time.Millisecond)
		fw.Stop()
	}()

	_, err := b.waitAndCollectDigest(context.Background(), "test-job")
	if err == nil {
		t.Fatal("expected error when watch channel closed, got nil")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error should mention 'closed': %v", err)
	}
}

func TestWaitAndCollectDigest_ContextCancelled(t *testing.T) {
	client := fake.NewSimpleClientset()
	fw := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(fw, nil))
	defer fw.Stop()

	b := &K8sJobBuilder{kube: client}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := b.waitAndCollectDigest(ctx, "test-job")
	if err == nil {
		t.Fatal("expected error on context timeout, got nil")
	}
}
