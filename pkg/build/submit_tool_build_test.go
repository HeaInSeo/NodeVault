package build

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/buildstate"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

func newSubmitTestService(t *testing.T) *Service {
	t.Helper()
	idx, err := index.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("index.NewAt: %v", err)
	}
	state, err := buildstate.Open(filepath.Join(t.TempDir(), "build-state.db"))
	if err != nil {
		t.Fatalf("buildstate.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Fatalf("buildstate.Close: %v", err)
		}
	})
	if err := idx.AppendResolvedToolSpec(index.ResolvedToolSpec{
		ToolSpecDigest: "spec-123",
		ToolName:       "bwa-mem2",
		Version:        "2.2.1",
		ResolvedAt:     time.Unix(100, 0).UTC(),
	}); err != nil {
		t.Fatalf("AppendResolvedToolSpec: %v", err)
	}
	return &Service{indexStore: idx, buildState: state}
}

func TestSubmitToolBuild_CreatesRequestedBuildState(t *testing.T) {
	svc := newSubmitTestService(t)

	resp, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-123",
		ToolSpecDigest: "spec-123",
	})
	if err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}
	if resp.GetBuildId() != "build-123" || resp.GetToolSpecDigest() != "spec-123" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.GetStatus() != string(buildstate.StatusRequested) {
		t.Fatalf("status got %q want Requested", resp.GetStatus())
	}

	got, err := svc.buildState.Get("build-123")
	if err != nil {
		t.Fatalf("buildState.Get: %v", err)
	}
	if got.Status != buildstate.StatusRequested {
		t.Fatalf("stored status got %q want Requested", got.Status)
	}
}

func TestSubmitToolBuild_UnknownToolSpec_NotFound(t *testing.T) {
	svc := newSubmitTestService(t)

	_, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-unknown",
		ToolSpecDigest: "missing",
	})
	if err == nil {
		t.Fatal("expected error for unknown tool spec")
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("status got %v want NotFound", status.Code(err))
	}
}

func TestWatchToolBuild_SendsCurrentState(t *testing.T) {
	svc := newSubmitTestService(t)
	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-watch",
		ToolSpecDigest: "spec-123",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}

	stream := newFakeStream()
	if err := svc.WatchToolBuild(&nfv1.WatchToolBuildRequest{BuildId: "build-watch"}, stream); err != nil {
		t.Fatalf("WatchToolBuild: %v", err)
	}
	if len(stream.events) != 1 {
		t.Fatalf("events got %d want 1", len(stream.events))
	}
	if stream.events[0].GetBuildId() != "build-watch" || stream.events[0].GetStatus() != string(buildstate.StatusRequested) {
		t.Fatalf("unexpected event: %+v", stream.events[0])
	}
}

func TestCancelToolBuild_MarksInterrupted(t *testing.T) {
	svc := newSubmitTestService(t)
	if _, err := svc.SubmitToolBuild(context.Background(), &nfv1.SubmitToolBuildRequest{
		RequestId:      "build-cancel",
		ToolSpecDigest: "spec-123",
	}); err != nil {
		t.Fatalf("SubmitToolBuild: %v", err)
	}

	resp, err := svc.CancelToolBuild(context.Background(), &nfv1.CancelToolBuildRequest{
		BuildId: "build-cancel",
		Reason:  "user requested",
	})
	if err != nil {
		t.Fatalf("CancelToolBuild: %v", err)
	}
	if resp.GetStatus() != string(buildstate.StatusInterrupted) {
		t.Fatalf("status got %q want Interrupted", resp.GetStatus())
	}
	got, err := svc.buildState.Get("build-cancel")
	if err != nil {
		t.Fatalf("buildState.Get: %v", err)
	}
	if got.Status != buildstate.StatusInterrupted {
		t.Fatalf("stored status got %q want Interrupted", got.Status)
	}
}
