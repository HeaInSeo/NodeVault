// Package build manages builder orchestration via podbridge5.
// BuildService receives BuildRequests, calls podbridge5 to build and push images,
// streams events back to the caller, and acquires the pushed image digest.
// After L2 succeeds it drives L3 (dry-run) → L4 (smoke run) → tool registration.
package build

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"

	"github.com/HeaInSeo/NodeVault/pkg/catalog"
	"github.com/HeaInSeo/NodeVault/pkg/index"
	"github.com/HeaInSeo/NodeVault/pkg/oras"
	"github.com/HeaInSeo/NodeVault/pkg/validate"
)

// ReconcileTriggerer triggers a targeted integrity check for one artifact.
// Implemented by *reconcile.Reconciler in production; nil disables eager reconcile.
// Authority: only SetIntegrityHealth is called through this path (reconcile axis).
type ReconcileTriggerer interface {
	ReconcileOne(ctx context.Context, casHash string) error
}

const defaultRegistryAddr = "harbor.10.113.24.96.nip.io"

func registryAddr() string {
	if v := os.Getenv("NODEVAULT_REGISTRY_ADDR"); v != "" {
		return v
	}
	return defaultRegistryAddr
}

// Service implements BuildServiceServer.
type Service struct {
	nfv1.UnimplementedBuildServiceServer
	builder    Builder
	validator  *validate.Service
	registry   *catalog.ToolRegistryService
	indexStore *index.Store
	reconciler ReconcileTriggerer // nil = no eager reconcile
}

// NewService creates a BuildService backed by podbridge5.
// reconciler may be nil; when non-nil it is called after successful referrer push
// so integrity_health transitions to Healthy without waiting for the next reconcile tick.
func NewService(
	validator *validate.Service, registry *catalog.ToolRegistryService, store *index.Store, reconciler ReconcileTriggerer,
) (*Service, error) {
	builder, err := newPodbridge5Builder()
	if err != nil {
		return nil, fmt.Errorf("build service init: %w", err)
	}
	return &Service{
		builder: builder, validator: validator, registry: registry,
		indexStore: store, reconciler: reconciler,
	}, nil
}

// Close releases the underlying image build storage.
func (s *Service) Close() error {
	return s.builder.Close()
}

// BuildAndRegister implements BuildServiceServer.
// Full orchestration: L2 (image build+push) → L3 (dry-run) → L4 (smoke run) → registration.
//
//nolint:funlen // orchestration function — extracting sub-steps would obscure the L2→L3→L4 sequence.
func (s *Service) BuildAndRegister(req *nfv1.BuildRequest, stream grpc.ServerStreamingServer[nfv1.BuildEvent]) error {
	ctx := stream.Context()

	send := func(kind nfv1.BuildEventKind, msg string) error {
		return stream.Send(&nfv1.BuildEvent{
			Kind:      kind,
			Message:   msg,
			Timestamp: time.Now().UnixMilli(),
		})
	}

	destination := fmt.Sprintf("%s/library/%s:latest", registryAddr(), sanitizeName(req.ToolName))

	// ── L2: image build + push ───────────────────────────────────────────────────

	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_JOB_CREATED, "image build starting: "+destination)
	slog.Info("image build starting", "destination", destination)

	_, digest, err := s.builder.Build(ctx, req.DockerfileContent, destination)
	if err != nil {
		_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED, err.Error())
		return fmt.Errorf("image build: %w", err)
	}

	slog.Info("image build succeeded", "destination", destination, "digest", digest)
	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_PUSH_SUCCEEDED, "image pushed to "+destination)

	imageWithDigest := destination + "@" + digest
	_ = stream.Send(&nfv1.BuildEvent{
		Kind:      nfv1.BuildEventKind_BUILD_EVENT_KIND_DIGEST_ACQUIRED,
		Message:   imageWithDigest,
		Digest:    digest,
		Timestamp: time.Now().UnixMilli(),
	})

	// ── L3: dry-run ──────────────────────────────────────────────────────────────

	reqID := req.RequestId
	if len(reqID) > 8 {
		reqID = reqID[:8]
	}
	jobSuffix := sanitizeName(reqID)
	if jobSuffix == "" {
		jobSuffix = fmt.Sprintf("%04x", time.Now().UnixMilli()%0xFFFF)
	}
	smokeJob := validate.SmokeJobSpec("nfsmoke-"+jobSuffix, imageWithDigest)
	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "L3: submitting dry-run...")

	dryResult := s.validator.DryRunJob(ctx, smokeJob)
	if !dryResult.Success {
		_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED, "L3 dry-run failed: "+dryResult.ErrorMessage)
		return fmt.Errorf("L3 dry-run failed: %s", dryResult.ErrorMessage)
	}
	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "L3 dry-run passed")

	// ── L4: smoke run ────────────────────────────────────────────────────────────

	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "L4: starting smoke run...")

	smokeResult := s.validator.SmokeRunJob(ctx, smokeJob)
	if !smokeResult.Success {
		_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_FAILED, "L4 smoke run failed: "+smokeResult.ErrorMessage)
		return fmt.Errorf("L4 smoke run failed: %s", smokeResult.ErrorMessage)
	}
	if smokeResult.LogOutput != "" {
		_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "smoke log: "+strings.TrimSpace(smokeResult.LogOutput))
	}
	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "L4 smoke run passed")

	// ── 등록 ─────────────────────────────────────────────────────────────────────

	regResp, regErr := s.registry.RegisterTool(ctx, &nfv1.RegisterToolRequest{
		RequestId:        req.RequestId,
		ToolDefinitionId: req.ToolDefinitionId,
		ToolName:         req.ToolName,
		ImageUri:         destination,
		Digest:           digest,
		EnvironmentSpec:  req.EnvironmentSpec,
		Version:          req.Version,
		Inputs:           req.Inputs,
		Outputs:          req.Outputs,
		Display:          req.Display,
		Command:          req.Command,
	})
	if regErr != nil {
		_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "registration warning: "+regErr.Error())
	} else {
		_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "tool registered: cas="+regResp.CasHash)
	}

	// ── spec referrer push (TODO-07) ─────────────────────────────────────────────
	// Non-fatal: if push fails, integrity_health stays Partial and reconcile retries.
	// integrity_health is updated ONLY via ReconcileOne (reconcile axis — authority map).
	if regErr == nil && s.indexStore != nil {
		imageRepo := fmt.Sprintf("%s/library/%s", registryAddr(), sanitizeName(req.ToolName))
		referrerDigest, refErr := oras.PushToolSpecReferrer(ctx, imageRepo, digest, regResp.Tool)
		if refErr != nil {
			slog.Warn("spec referrer push failed (integrity_health=Partial)", "err", refErr)
			_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "spec referrer push failed: "+refErr.Error())
		} else {
			slog.Info("spec referrer attached", "referrer", referrerDigest)
			_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_LOG, "spec referrer attached: "+referrerDigest)
			if idxErr := s.indexStore.SetSpecReferrerDigest(regResp.CasHash, referrerDigest); idxErr != nil {
				slog.Warn("index spec referrer digest update failed", "err", idxErr)
			}
			// Delegate integrity_health update to reconciler (authority map: reconcile axis only).
			if s.reconciler != nil {
				if recErr := s.reconciler.ReconcileOne(ctx, regResp.CasHash); recErr != nil {
					slog.Warn("eager reconcile after referrer push failed", "err", recErr)
				}
			}
		}
	}

	_ = send(nfv1.BuildEventKind_BUILD_EVENT_KIND_SUCCEEDED,
		fmt.Sprintf("build+register complete: %s@%s", destination, digest))
	return nil
}

// sanitizeName makes a string safe for use as an image name component.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	result := strings.Trim(b.String(), "-")
	if len(result) > 50 {
		result = result[:50]
	}
	return result
}
