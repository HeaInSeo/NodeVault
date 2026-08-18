package validate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// setVar temporarily overrides a package-level timing knob for a test and
// returns a restore func. Tests use tiny values so timing-dependent paths run
// fast; assertions remain driven by fake-client behavior (call counts), not
// wall-clock sleeps.
func setVar[T any](p *T, v T) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

// ─── parseJobManifest ─────────────────────────────────────────────────────────

func TestParseJobManifest_ValidYAML(t *testing.T) {
	yaml := `
apiVersion: batch/v1
kind: Job
metadata:
  name: smoke-test
  namespace: nodevault-smoke
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
	if job.Namespace != "nodevault-smoke" {
		t.Errorf("Namespace: got %q want %q", job.Namespace, "nodevault-smoke")
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

// ─── waitForJob watch reconnect (Bug 1) ───────────────────────────────────────

// TestWaitForJob_ReconnectsAfterWatchChannelCloses simulates a watch channel
// that closes mid-wait for a reason unrelated to context cancellation (e.g. an
// API server restart or the watch's normal relist/timeout expiry). It asserts
// that waitForJob re-establishes a new watch instead of immediately reporting a
// false-negative failure, and that it succeeds once the second watch delivers
// the Job's actual completion event.
func TestWaitForJob_ReconnectsAfterWatchChannelCloses(t *testing.T) {
	defer setVar(&watchReconnectBackoff, time.Millisecond)()

	// Seed a still-running Job so the relist performed on each (re)connect finds
	// it non-terminal and proceeds to watch, rather than short-circuiting.
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "smoke-recon", Namespace: smokeNamespace}}
	kube := fake.NewSimpleClientset(running)
	svc := &Service{kube: kube}

	var watchCalls int
	kube.PrependWatchReactor("jobs", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		watchCalls++
		if watchCalls == 1 {
			// First watch: simulate a dropped/expired connection by handing back
			// an already-closed watcher, so ResultChan() reports ok=false
			// immediately with no terminal event ever observed.
			w := watch.NewFake()
			w.Stop()
			return true, w, nil
		}
		// Second (reconnected) watch: deliver the real completion event.
		w := watch.NewFake()
		go func() {
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "smoke-recon", Namespace: smokeNamespace},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
					},
				},
			}
			w.Modify(job)
		}()
		return true, w, nil
	})

	result := svc.waitForJob(context.Background(), "smoke-recon")

	if !result.Success {
		t.Fatalf("expected success after watch reconnect, got: %+v", result)
	}
	if watchCalls < 2 {
		t.Fatalf("expected waitForJob to re-establish the watch at least once, got %d Watch() calls", watchCalls)
	}
}

// TestWaitForJob_ReconnectFailureRespectsDeadline confirms that when watch
// re-establishment itself keeps failing (e.g. the API server is genuinely
// unreachable), waitForJob does not retry forever — it gives up once the
// overall smoke-run deadline passes, and the error clearly distinguishes a
// reconnect failure from a known Job outcome.
func TestWaitForJob_ReconnectFailureRespectsDeadline(t *testing.T) {
	kube := fake.NewSimpleClientset()
	svc := &Service{kube: kube}

	// The relist performed on each (re)connect also fails when the API server is
	// genuinely unreachable, so make GET fail too — otherwise the fake tracker
	// would report the Job absent and short-circuit before the watch is tried.
	kube.PrependReactor("get", "jobs", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	kube.PrependWatchReactor("jobs", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, errors.New("connection refused")
	})

	// waitForJob internally bounds itself to smokeTimeout (5m) via ctx, but we
	// don't want this test to take 5 minutes. Pass an already-short-lived
	// parent context so the overall deadline is reached quickly.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	result := svc.waitForJob(ctx, "smoke-unreachable")
	elapsed := time.Since(start)

	if result.Success {
		t.Fatal("expected failure when watch reconnect keeps failing")
	}
	if !strings.Contains(result.ErrorMessage, "reconnect") {
		t.Errorf("ErrorMessage should distinguish reconnect failure from a Job outcome, got: %q", result.ErrorMessage)
	}
	if elapsed > 5*time.Second {
		t.Errorf("waitForJob should give up around the deadline, took %s", elapsed)
	}
}

// ─── reachability check (Bug 2) ───────────────────────────────────────────────

// TestNewServiceFromClient_UnreachableCluster confirms that a well-formed but
// unreachable client (ServerVersion probe fails) causes construction to fail,
// distinct from today's success-with-a-broken-client behavior.
func TestNewServiceFromClient_UnreachableCluster(t *testing.T) {
	kube := fake.NewSimpleClientset()
	fakeDiscovery, ok := kube.Discovery().(*discoveryfake.FakeDiscovery)
	if !ok {
		t.Fatalf("expected *discoveryfake.FakeDiscovery, got %T", kube.Discovery())
	}
	fakeDiscovery.PrependReactor("get", "version", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp: i/o timeout")
	})

	svc, err := newServiceFromClient(kube)
	if err == nil {
		t.Fatal("expected an error when the cluster is unreachable")
	}
	if svc != nil {
		t.Error("expected a nil Service when the reachability probe fails")
	}
}

// TestNewServiceFromClient_Reachable confirms the happy path still succeeds.
func TestNewServiceFromClient_Reachable(t *testing.T) {
	kube := fake.NewSimpleClientset()

	svc, err := newServiceFromClient(kube)
	if err != nil {
		t.Fatalf("newServiceFromClient: %v", err)
	}
	if svc == nil {
		t.Fatal("expected a non-nil Service")
	}
}

// ─── P1a: relist catches a terminal transition during a watch gap ─────────────

// TestWaitForJob_RelistCatchesTerminalDuringWatchGap reproduces the missed
// terminal transition: the first watch closes and the Job reaches Complete
// before the replacement watch is established, and the second watch emits NO
// event. Without a relist-on-reconnect, waitForJob never observes the completion
// and only gives up at the deadline. With the relist, the reconnect's GET
// observes Complete and returns success.
func TestWaitForJob_RelistCatchesTerminalDuringWatchGap(t *testing.T) {
	defer setVar(&watchReconnectBackoff, time.Millisecond)()

	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "smoke-gap", Namespace: smokeNamespace}}
	kube := fake.NewSimpleClientset(running)
	svc := &Service{kube: kube}
	gvr := batchv1.SchemeGroupVersion.WithResource("jobs")

	var watchCalls int
	kube.PrependWatchReactor("jobs", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		watchCalls++
		if watchCalls == 1 {
			// The Job reaches Complete during the gap, before the replacement
			// watch is established.
			complete := running.DeepCopy()
			complete.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}
			if err := kube.Tracker().Update(gvr, complete, smokeNamespace); err != nil {
				t.Errorf("tracker update: %v", err)
			}
			w := watch.NewFake()
			w.Stop()
			return true, w, nil
		}
		// Second watch: never delivers any event.
		return true, watch.NewFake(), nil
	})

	// Bounded so the pre-fix code (which never observes completion) fails fast
	// instead of waiting the full smoke timeout.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result := svc.waitForJob(ctx, "smoke-gap")
	if !result.Success {
		t.Fatalf("expected relist to observe Complete during the watch gap, got: %+v", result)
	}
}

// ─── P2b: timeout after a watch gap is an unknown outcome ──────────────────────

// TestWaitForJob_TimeoutAfterGapReportsUnknown reproduces the ambiguous-timeout
// case: a watch is interrupted, a later watch stays open with no events until
// the deadline, and the confirming GET at the deadline cannot reach the API
// server. Because the terminal event may have occurred during the gap, the
// outcome must be reported as UNKNOWN rather than a definite timeout.
func TestWaitForJob_TimeoutAfterGapReportsUnknown(t *testing.T) {
	defer setVar(&watchReconnectBackoff, time.Millisecond)()

	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "smoke-unknown", Namespace: smokeNamespace}}
	kube := fake.NewSimpleClientset(running)
	svc := &Service{kube: kube}

	var getCalls int
	kube.PrependReactor("get", "jobs", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		getCalls++
		if getCalls >= 3 {
			// The confirming GET at the deadline cannot reach the API server.
			return true, nil, errors.New("connection refused")
		}
		return false, nil, nil // earlier relists succeed (tracker: running)
	})
	var watchCalls int
	kube.PrependWatchReactor("jobs", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		watchCalls++
		if watchCalls == 1 {
			w := watch.NewFake()
			w.Stop() // interruption
			return true, w, nil
		}
		return true, watch.NewFake(), nil // stays open, no events, until deadline
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	result := svc.waitForJob(ctx, "smoke-unknown")
	if result.Success {
		t.Fatal("expected non-success")
	}
	if !strings.Contains(result.ErrorMessage, "unknown") {
		t.Fatalf("expected an UNKNOWN outcome after a watch gap, got: %q", result.ErrorMessage)
	}
}

// ─── P2c: back off after clean watch closures ─────────────────────────────────

// TestWaitForJob_BacksOffOnCleanWatchClosure reproduces the busy-spin: a watch
// that establishes then immediately EOFs on every attempt. Without a backoff on
// clean closure, waitForJob reconnects with no delay and hammers the API server
// thousands of times until the deadline. With the backoff, reconnects are
// bounded.
func TestWaitForJob_BacksOffOnCleanWatchClosure(t *testing.T) {
	defer setVar(&watchReconnectBackoff, 20*time.Millisecond)()

	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "smoke-spin", Namespace: smokeNamespace}}
	kube := fake.NewSimpleClientset(running)
	svc := &Service{kube: kube}

	var watchCalls int
	kube.PrependWatchReactor("jobs", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		watchCalls++
		w := watch.NewFake()
		w.Stop() // always immediately closed
		return true, w, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	result := svc.waitForJob(ctx, "smoke-spin")
	if result.Success {
		t.Fatal("expected non-success (job never completes)")
	}
	// ~200ms / 20ms ≈ 10 reconnects; without the backoff this spins into the
	// thousands.
	if watchCalls > 40 {
		t.Fatalf("expected the clean-closure backoff to bound reconnects, got %d watch calls", watchCalls)
	}
}
