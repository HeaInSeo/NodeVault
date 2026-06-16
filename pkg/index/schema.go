package index

import "time"

// ArtifactKind identifies the type of platform artifact.
type ArtifactKind string

const (
	KindTool ArtifactKind = "tool"
	KindData ArtifactKind = "data" // reserved for P3 data artifact axis
)

// LifecyclePhase is the operator-intent axis of an artifact.
// Changed only by explicit NodeVault operations — never by the reconcile loop.
type LifecyclePhase string

const (
	PhasePending   LifecyclePhase = "Pending"
	PhaseActive    LifecyclePhase = "Active"
	PhaseRetracted LifecyclePhase = "Retracted"
	PhaseDeleted   LifecyclePhase = "Deleted"
)

// IntegrityHealth is the observation axis derived from reconciling Harbor state.
// Changed only by the reconcile loop — never by lifecycle operations.
type IntegrityHealth string

const (
	HealthHealthy     IntegrityHealth = "Healthy"
	HealthPartial     IntegrityHealth = "Partial"     // image OK, spec referrer missing
	HealthMissing     IntegrityHealth = "Missing"     // both image and spec missing
	HealthUnreachable IntegrityHealth = "Unreachable" // transient access failure
	HealthOrphaned    IntegrityHealth = "Orphaned"    // spec referrer present, image missing
)

// Entry is a single record in the NodeVault artifact index.
// It carries two independent state axes — they must never be merged.
//
// Catalog visibility rule: lifecycle_phase == Active only.
// integrity_health has NO effect on Catalog visibility; it is for monitoring/alerting.
type Entry struct {
	// CasHash is the SHA256 of the tool spec JSON (content without cas_hash field).
	// Primary key. Used by pipelines for immutable pinning.
	CasHash string `json:"cas_hash"`

	// ArtifactKind distinguishes tool from data artifacts.
	ArtifactKind ArtifactKind `json:"artifact_kind"`

	// StableRef is the human-readable identifier used for UI search and Catalog listing.
	// Format: "{tool_name}@{version}". Multiple casHashes may share the same StableRef.
	// stableRef:casHash cardinality is 1:N.
	StableRef string `json:"stable_ref"`

	// ToolName and Version are the constituent parts of StableRef.
	ToolName string `json:"tool_name,omitempty"`
	Version  string `json:"version,omitempty"`

	// ImageRef is the full image reference as pushed to Harbor.
	// Format: "{registry_host}/{project}/{repo}:{tag}"
	// Example: "harbor.10.113.24.96.nip.io/library/bwa:latest"
	// Used by the reconcile loop to check Harbor existence without reconstructing the reference.
	ImageRef string `json:"image_ref,omitempty"`

	// ImageDigest is the OCI digest of the built tool image in Harbor.
	ImageDigest string `json:"image_digest,omitempty"`

	// SpecReferrerDigest is the OCI digest of the attached spec referrer artifact.
	// Empty until pkg/oras pushes the referrer (TODO-07).
	SpecReferrerDigest string `json:"spec_referrer_digest,omitempty"`

	// ── State axis 1: operator intent ────────────────────────────────────────
	// Changed only by NodeVault explicit operations (Register, Retract, Delete).
	LifecyclePhase LifecyclePhase `json:"lifecycle_phase"`

	// ── State axis 2: Harbor observation ─────────────────────────────────────
	// Changed only by the reconcile loop. Never by lifecycle operations.
	IntegrityHealth IntegrityHealth `json:"integrity_health"`

	// ── Timestamps ───────────────────────────────────────────────────────────
	RegisteredAt       time.Time `json:"registered_at"`
	LifecycleUpdatedAt time.Time `json:"lifecycle_updated_at"`
	HealthCheckedAt    time.Time `json:"health_checked_at"`
}

// ResolvedToolSpec is NodeVault's resolved view of a ToolSpecRequest sent by NodeKit.
// Primary key: ToolSpecDigest.
type ResolvedToolSpec struct {
	// ToolSpecDigest is the content digest of the resolved spec. Primary key.
	ToolSpecDigest string `json:"tool_spec_digest"`

	ToolName string `json:"tool_name,omitempty"`
	Version  string `json:"version,omitempty"`

	// RawSpec preserves the original NodeKit-submitted spec in a serializable form
	// (e.g. the JSON payload) so it can be replayed or audited later.
	RawSpec string `json:"raw_spec,omitempty"`

	ResolvedAt time.Time `json:"resolved_at"`
}

// ToolBuildRecord captures the outcome of a single build execution.
// Primary key: BuildID. Foreign key: ToolSpecDigest references ResolvedToolSpec.
type ToolBuildRecord struct {
	// BuildID identifies one build execution. Primary key.
	BuildID string `json:"build_id"`

	// ToolSpecDigest is the FK into ResolvedToolSpec.
	ToolSpecDigest string `json:"tool_spec_digest,omitempty"`

	ImageDigest string `json:"image_digest,omitempty"`

	// NanVersion is the nan (node-artifact-runtime) layer version injected into the
	// build. Populated by the nan injection step (Sprint 1, separate work item).
	NanVersion string `json:"nan_version,omitempty"`

	// Backend identifies the build executor, e.g. "k8s-job".
	Backend string `json:"backend,omitempty"`

	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Success     bool      `json:"success"`

	// FailureReason is set when Success is false.
	FailureReason string `json:"failure_reason,omitempty"`
}

// ToolImageRecord captures metadata of a successfully built and pushed image.
// Primary key: ImageDigest. Foreign keys: ToolSpecDigest, BuildID.
type ToolImageRecord struct {
	// ImageDigest is the OCI digest of the pushed image. Primary key.
	ImageDigest string `json:"image_digest"`

	ImageRef string `json:"image_ref,omitempty"`

	// ToolSpecDigest is the FK into ResolvedToolSpec.
	ToolSpecDigest string `json:"tool_spec_digest,omitempty"`

	// BuildID is the FK into ToolBuildRecord.
	BuildID string `json:"build_id,omitempty"`

	PushedAt time.Time `json:"pushed_at"`

	// Platform is the target platform, e.g. "linux/amd64".
	Platform string `json:"platform,omitempty"`
}

// indexFile is the on-disk representation of the index.
type indexFile struct {
	SchemaVersion int     `json:"schema_version"`
	Entries       []Entry `json:"entries"`

	// ResolvedToolSpecs, ToolBuildRecords, and ToolImageRecords are additive sections
	// introduced in schema version 2. Older files omit these fields; load() treats
	// missing fields as empty slices (zero value of a nil slice), so no migration is needed.
	ResolvedToolSpecs []ResolvedToolSpec `json:"resolved_tool_specs,omitempty"`
	ToolBuildRecords  []ToolBuildRecord  `json:"tool_build_records,omitempty"`
	ToolImageRecords  []ToolImageRecord  `json:"tool_image_records,omitempty"`
}
