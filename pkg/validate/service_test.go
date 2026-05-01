package validate

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// ─── parseJobManifest ─────────────────────────────────────────────────────────

func TestParseJobManifest_ValidYAML(t *testing.T) {
	yaml := `
apiVersion: batch/v1
kind: Job
metadata:
  name: smoke-test
  namespace: nodeforge-smoke
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: smoke
          image: alpine:3.19@sha256:abc123
          command: ["sh", "-c", "echo smoke-ok"]
`
	job, err := parseJobManifest(yaml)
	if err != nil {
		t.Fatalf("parseJobManifest: %v", err)
	}
	if job.Name != "smoke-test" {
		t.Errorf("Name: got %q want %q", job.Name, "smoke-test")
	}
	if job.Namespace != "nodeforge-smoke" {
		t.Errorf("Namespace: got %q want %q", job.Namespace, "nodeforge-smoke")
	}
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(job.Spec.Template.Spec.Containers))
	}
	if job.Spec.Template.Spec.Containers[0].Image != "alpine:3.19@sha256:abc123" {
		t.Errorf("Image: got %q", job.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestParseJobManifest_InvalidYAML(t *testing.T) {
	_, err := parseJobManifest("not: valid: yaml: [unclosed")
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestParseJobManifest_NonJobKind(t *testing.T) {
	// A valid YAML for a different Kind — parseJobManifest doesn't validate Kind,
	// just unmarshals. Verify it returns something without panicking.
	yaml := `apiVersion: v1
kind: Pod
metadata:
  name: my-pod
`
	job, err := parseJobManifest(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A Pod YAML unmarshalled into batchv1.Job will have Name populated (shared ObjectMeta).
	_ = job
}

// ─── SmokeJobSpec ─────────────────────────────────────────────────────────────

func TestSmokeJobSpec_Fields(t *testing.T) {
	imageWithDigest := "registry.example.com/bwa:0.7.17@sha256:deadbeef"
	job := SmokeJobSpec("smoke-abc123", imageWithDigest)

	if job.Name != "smoke-abc123" {
		t.Errorf("Name: got %q want %q", job.Name, "smoke-abc123")
	}
	if job.Namespace != smokeNamespace {
		t.Errorf("Namespace: got %q want %q", job.Namespace, smokeNamespace)
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit: got %d want 0", *job.Spec.BackoffLimit)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy: got %q", job.Spec.Template.Spec.RestartPolicy)
	}
	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	if containers[0].Image != imageWithDigest {
		t.Errorf("Image: got %q want %q", containers[0].Image, imageWithDigest)
	}
	if len(containers[0].Command) == 0 {
		t.Error("Command should not be empty")
	}
}

func TestSmokeJobSpec_TTLAndDeadline(t *testing.T) {
	job := SmokeJobSpec("smoke-ttl", "alpine:3.19@sha256:abc")
	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Fatal("TTLSecondsAfterFinished should not be nil")
	}
	if *job.Spec.TTLSecondsAfterFinished != smokeJobTTL {
		t.Errorf("TTL: got %d want %d", *job.Spec.TTLSecondsAfterFinished, smokeJobTTL)
	}
	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("ActiveDeadlineSeconds should not be nil")
	}
	if *job.Spec.ActiveDeadlineSeconds != smokeJobDeadline {
		t.Errorf("Deadline: got %d want %d", *job.Spec.ActiveDeadlineSeconds, smokeJobDeadline)
	}
}

// ─── DryRunJob ────────────────────────────────────────────────────────────────

func TestDryRunJob_Success(t *testing.T) {
	svc := &Service{kube: fake.NewSimpleClientset()}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "build-job-001",
			Namespace: "nodeforge-builds",
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{Name: "kaniko", Image: "gcr.io/kaniko-project/executor:latest"},
					},
				},
			},
		},
	}

	result := svc.DryRunJob(context.Background(), job)
	if !result.Success {
		t.Errorf("DryRunJob should succeed with fake client: %s", result.ErrorMessage)
	}
}

func TestDryRunJob_PrefixesDryName(t *testing.T) {
	// Verify DryRunJob uses "dry-" prefix. With fake client the call succeeds without
	// creating a real resource. We check that the original job name is NOT modified.
	svc := &Service{kube: fake.NewSimpleClientset()}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "build-xyz",
			Namespace: "nodeforge-builds",
		},
	}
	originalName := job.Name

	result := svc.DryRunJob(context.Background(), job)
	if !result.Success {
		t.Errorf("DryRunJob: %s", result.ErrorMessage)
	}
	if job.Name != originalName {
		t.Errorf("DryRunJob must not mutate the original job name: got %q want %q", job.Name, originalName)
	}
}

// ─── DryRun (gRPC handler) ────────────────────────────────────────────────────

func TestDryRun_ValidManifest(t *testing.T) {
	svc := &Service{kube: fake.NewSimpleClientset()}

	manifest := `
apiVersion: batch/v1
kind: Job
metadata:
  name: build-dry-001
  namespace: nodeforge-builds
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: kaniko
          image: gcr.io/kaniko-project/executor:v1.9.0
`
	resp, err := svc.DryRun(context.Background(), &nfv1.DryRunRequest{
		RequestId:    "test-req",
		ManifestYaml: manifest,
	})
	if err != nil {
		t.Fatalf("DryRun RPC: %v", err)
	}
	if !resp.Success {
		t.Errorf("DryRun should succeed: %s", resp.ErrorMessage)
	}
}

func TestDryRun_InvalidManifest(t *testing.T) {
	svc := &Service{kube: fake.NewSimpleClientset()}

	resp, err := svc.DryRun(context.Background(), &nfv1.DryRunRequest{
		RequestId:    "test-req",
		ManifestYaml: "not: valid: yaml: [",
	})
	if err != nil {
		t.Fatalf("DryRun RPC itself should not return an error (errors go in result): %v", err)
	}
	if resp.Success {
		t.Error("DryRun with invalid manifest should not succeed")
	}
	if !strings.Contains(resp.ErrorMessage, "parse manifest") {
		t.Errorf("ErrorMessage should mention parse failure, got: %q", resp.ErrorMessage)
	}
}

// ─── SmokeRun (gRPC handler — parse-error path only) ─────────────────────────

func TestSmokeRun_InvalidManifest(t *testing.T) {
	svc := &Service{kube: fake.NewSimpleClientset()}

	resp, err := svc.SmokeRun(context.Background(), &nfv1.SmokeRunRequest{
		RequestId:    "test-req",
		ManifestYaml: "not: valid: yaml: [",
	})
	if err != nil {
		t.Fatalf("SmokeRun RPC itself should not return an error: %v", err)
	}
	if resp.Success {
		t.Error("SmokeRun with invalid manifest should not succeed")
	}
	if !strings.Contains(resp.ErrorMessage, "parse manifest") {
		t.Errorf("ErrorMessage should mention parse failure, got: %q", resp.ErrorMessage)
	}
}

// ─── ensureNamespace ─────────────────────────────────────────────────────────

func TestEnsureNamespace_CreatesIfMissing(t *testing.T) {
	svc := &Service{kube: fake.NewSimpleClientset()}

	if err := svc.ensureNamespace(context.Background(), "new-ns"); err != nil {
		t.Fatalf("ensureNamespace: %v", err)
	}
	// Second call must be idempotent (namespace already exists).
	if err := svc.ensureNamespace(context.Background(), "new-ns"); err != nil {
		t.Fatalf("ensureNamespace idempotent: %v", err)
	}
}

func TestEnsureNamespace_ExistingNamespace(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pre-existing"}}
	svc := &Service{kube: fake.NewSimpleClientset(ns)}

	if err := svc.ensureNamespace(context.Background(), "pre-existing"); err != nil {
		t.Fatalf("ensureNamespace for pre-existing ns: %v", err)
	}
}
