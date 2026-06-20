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

	// ObservedProfileDigest is the OCI digest of the rank-1 (most recent)
	// toolprofile referrer in ObservedProfileReferrers. Empty until a
	// successful ToolCheckRecord triggers pkg/oras.PushToolProfileReferrer.
	ObservedProfileDigest string `json:"observed_profile_digest,omitempty"`

	// ObservedProfileReferrers is the append-only record of every toolprofile
	// referrer pushed for this entry. NodeVault re-ranks this slice on every
	// push to compute GC_CANDIDATE marking locally — it never mutates,
	// re-pushes, or deletes the underlying OCI referrer manifests. Physical
	// deletion is delegated to Harbor retention/GC policy, operators, or an
	// external cleanup runner. See docs/OBSERVED_PROFILE_SPEC.md §5.2.
	ObservedProfileReferrers []ToolProfileReferrer `json:"observed_profile_referrers,omitempty"`

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

// ResolvedToolSpec is NodeVault's resolved view of a ToolSpecRequest sent by an external authoring tool.
// Primary key: ToolSpecDigest.
type ResolvedToolSpec struct {
	// ToolSpecDigest is the content digest of the resolved spec. Primary key.
	ToolSpecDigest string `json:"tool_spec_digest"`

	ToolName string `json:"tool_name,omitempty"`
	Version  string `json:"version,omitempty"`

	// RawSpec preserves the original NodeKit-submitted spec in a serializable form
	// (e.g. the JSON payload) so it can be replayed or audited later.
	RawSpec string `json:"raw_spec,omitempty"`

	RecipeInputsDigest string `json:"recipe_inputs_digest,omitempty"`
	BuildPlanDigest    string `json:"build_plan_digest,omitempty"`
	BuilderIdentity    string `json:"builder_identity,omitempty"`
	BaseImageRef       string `json:"base_image_ref,omitempty"`
	BaseImageDigest    string `json:"base_image_digest,omitempty"`

	ResolvedAt time.Time `json:"resolved_at"`
}

// BuildExecution records the runtime properties that affected one image build.
// Pointer fields preserve backward compatibility with records written before
// execution provenance was captured.
type BuildExecution struct {
	Mode          string `json:"mode,omitempty"`
	HostUsers     *bool  `json:"host_users,omitempty"`
	StorageDriver string `json:"storage_driver,omitempty"`
	Isolation     string `json:"isolation,omitempty"`
}

// ToolBuildRecord captures the outcome of a single build execution.
// Primary key: BuildID. Foreign key: ToolSpecDigest references ResolvedToolSpec.
type ToolBuildRecord struct {
	// BuildID identifies one build execution. Primary key.
	BuildID string `json:"build_id"`

	// ToolSpecDigest is the FK into ResolvedToolSpec.
	ToolSpecDigest string `json:"tool_spec_digest,omitempty"`

	ImageDigest string `json:"image_digest,omitempty"`

	// Backend identifies the build executor, e.g. "in-pod-buildah".
	Backend   string          `json:"backend,omitempty"`
	Execution *BuildExecution `json:"execution,omitempty"`

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

// ObservedIoProfile records the actual I/O file observations from a functional validation run.
type ObservedIoProfile struct {
	Inputs  []PortObservation `json:"inputs,omitempty"`
	Outputs []PortObservation `json:"outputs,omitempty"`
}

// PortObservation is a single port's observed file state.
type PortObservation struct {
	Port      string `json:"port"`
	FileCount int    `json:"file_count"`
	NonEmpty  bool   `json:"non_empty"`
}

// ObservedResourceProfile records resource usage observed during a validation run.
type ObservedResourceProfile struct {
	PeakCPUMillicores int64 `json:"peak_cpu_millicores,omitempty"`
	PeakMemoryMiB     int64 `json:"peak_memory_mib,omitempty"`
	DurationSeconds   int64 `json:"duration_seconds,omitempty"`
	DiskReadMiB       int64 `json:"disk_read_mib,omitempty"`
	DiskWriteMiB      int64 `json:"disk_write_mib,omitempty"`
	Timeout           bool  `json:"timeout,omitempty"`
	TimeoutSeconds    int64 `json:"timeout_seconds,omitempty"`
}

// ContractCheck records whether the tool met its declared I/O contract.
type ContractCheck struct {
	AllOutputsPresent bool   `json:"all_outputs_present"`
	Result            string `json:"result"` // "pass" | "fail" | "unknown"
}

// ReferrerLifecycleStatus is NodeVault's local GC marking state for one
// toolprofile referrer. It never reflects an action taken against the OCI
// registry — NodeVault does not mutate, re-push, or delete referrer
// manifests. See docs/OBSERVED_PROFILE_SPEC.md §5.2.
type ReferrerLifecycleStatus string

const (
	// ReferrerActive is one of the latest DefaultToolProfileReferrerRetain
	// referrers for its subject digest.
	ReferrerActive ReferrerLifecycleStatus = "ACTIVE"
	// ReferrerGCCandidate is older than the retained set. Physical deletion
	// is delegated to Harbor retention/GC policy, operators, or an external
	// cleanup runner — NodeVault only marks it here.
	ReferrerGCCandidate ReferrerLifecycleStatus = "GC_CANDIDATE"
	// ReferrerMissingInRegistry is reserved for a future reconcile pass that
	// detects a tracked referrer no longer present in the registry. No
	// current code path sets this value.
	ReferrerMissingInRegistry ReferrerLifecycleStatus = "MISSING_IN_REGISTRY"
)

// DefaultToolProfileReferrerRetain is the number of most-recent toolprofile
// referrers kept ACTIVE per subject digest; older ones are marked
// GC_CANDIDATE. See docs/OBSERVED_PROFILE_SPEC.md §5.2.
const DefaultToolProfileReferrerRetain = 3

// ToolProfileReferrer is one pushed toolprofile OCI referrer, tracked
// index-locally so NodeVault can compute "latest N" deterministically without
// depending on the registry Referrers() listing order.
type ToolProfileReferrer struct {
	Digest           string `json:"digest"`
	ValidationRunID  string `json:"validation_run_id,omitempty"`
	ValidationStatus string `json:"validation_status,omitempty"`

	// ValidatedAt is the validation-completion timestamp (the source
	// ToolCheckRecord's CheckedAt) — the primary recency ordering key.
	ValidatedAt time.Time `json:"validated_at"`
	// ObservedAt is when NodeVault recorded the push; a tie-breaker when
	// ValidatedAt is equal or zero.
	ObservedAt time.Time `json:"observed_at"`

	// Rank is the 1-based recency position after ordering (1 = most recent).
	Rank int `json:"rank"`
	// LifecycleStatus is recomputed on every push for the full set.
	LifecycleStatus ReferrerLifecycleStatus `json:"lifecycle_status"`
	// GCReason explains why LifecycleStatus is GC_CANDIDATE.
	GCReason string `json:"gc_reason,omitempty"`
	// MarkedAt is when LifecycleStatus first became GC_CANDIDATE.
	MarkedAt time.Time `json:"marked_at,omitempty"`
}

// ToolCheckRecord is the result of a NodeSentinel L5-a functional validation run.
// Primary key: CheckID. Foreign keys: ToolSpecDigest, ImageDigest.
type ToolCheckRecord struct {
	CheckID string `json:"check_id"`

	// ToolSpecDigest references ResolvedToolSpec.
	ToolSpecDigest string `json:"tool_spec_digest,omitempty"`
	ImageDigest    string `json:"image_digest"`
	Platform       string `json:"platform,omitempty"`
	ToolName       string `json:"tool_name,omitempty"`
	Version        string `json:"version,omitempty"`

	// ValidationStatus: "succeeded" | "infra_failed" | "app_failed"
	ValidationStatus string `json:"validation_status"`

	// ValidationHash is the content-deterministic fingerprint of a successful run.
	// Empty when ValidationStatus != "succeeded".
	ValidationHash string `json:"validation_hash,omitempty"`

	Command  string `json:"command,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`

	ObservedIoProfile       *ObservedIoProfile       `json:"observed_io_profile,omitempty"`
	ObservedResourceProfile *ObservedResourceProfile `json:"observed_resource_profile,omitempty"`
	ContractCheck           *ContractCheck           `json:"contract_check,omitempty"`

	FailureReason string    `json:"failure_reason,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
}

// ToolScanRecord is the result of a NodeSentinel L5-b trivy security scan.
// Primary key: ScanID. Foreign key: ImageDigest.
type ToolScanRecord struct {
	ScanID      string `json:"scan_id"`
	ImageDigest string `json:"image_digest"`
	ToolName    string `json:"tool_name,omitempty"`
	Platform    string `json:"platform,omitempty"`

	Scanner        string `json:"scanner,omitempty"`         // "trivy"
	ScannerVersion string `json:"scanner_version,omitempty"` // "0.50.0"
	// DbDigest identifies the vulnerability database snapshot used by the scanner.
	DbDigest string `json:"db_digest,omitempty"`
	Source   string `json:"source,omitempty"` // "trivy-operator" | "not-available"

	CriticalCount int `json:"critical_count"`
	HighCount     int `json:"high_count"`
	MediumCount   int `json:"medium_count"`
	LowCount      int `json:"low_count"`

	// PolicyMode: "record_only" | "gate_critical" | "gate_high"
	PolicyMode string `json:"policy_mode"`
	// PolicyResult: "pass" | "warning" | "blocked"
	PolicyResult string `json:"policy_result"`

	ScannedAt time.Time `json:"scanned_at"`
}

// PromotionStatus reflects a CertifiedToolImageRecord's lifecycle.
type PromotionStatus string

const (
	PromotionActive     PromotionStatus = "active"
	PromotionSuperseded PromotionStatus = "superseded"
	PromotionRetracted  PromotionStatus = "retracted"
)

// CertifiedToolImageRecord is the NodeVault decision record after reviewing
// a ToolCheckRecord + ToolScanRecord. It is the source of truth for whether a
// given tool spec and platform is safe to expose via NodePalette.
// Primary key: ToolSpecDigest + Platform. Records without both fields retain
// ImageDigest as their backward-compatible lookup key.
type CertifiedToolImageRecord struct {
	ImageDigest    string `json:"image_digest"`
	ToolSpecDigest string `json:"tool_spec_digest,omitempty"`
	Platform       string `json:"platform,omitempty"`
	ToolName       string `json:"tool_name,omitempty"`
	Version        string `json:"version,omitempty"`
	CasHash        string `json:"cas_hash,omitempty"`

	PromotionStatus PromotionStatus `json:"promotion_status"`
	CertifiedAt     time.Time       `json:"certified_at"`

	// Reference IDs to the Records that drove this decision.
	CheckID string `json:"check_id,omitempty"`
	ScanID  string `json:"scan_id,omitempty"`
}

// ToolFunctionCatalogEntry is the stable catalog projection of a certified tool,
// ready for NodePalette to serve to pipeline builders.
// Primary key: CasHash.
type ToolFunctionCatalogEntry struct {
	CasHash     string `json:"cas_hash"`
	ToolName    string `json:"tool_name,omitempty"`
	Version     string `json:"version,omitempty"`
	StableRef   string `json:"stable_ref,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
	ImageRef    string `json:"image_ref,omitempty"`

	DisplayLabel       string   `json:"display_label,omitempty"`
	DisplayDescription string   `json:"display_description,omitempty"`
	DisplayCategory    string   `json:"display_category,omitempty"`
	DisplayTags        []string `json:"display_tags,omitempty"`

	PromotionStatus PromotionStatus `json:"promotion_status"`
	CertifiedAt     time.Time       `json:"certified_at"`

	// ValidationHash from the ToolCheckRecord that drove this entry.
	ValidationHash string `json:"validation_hash,omitempty"`
}

// indexFile is the on-disk representation of the index.
// schemaVersion 3 adds ToolCheckRecords, ToolScanRecords,
// CertifiedToolImageRecords, and ToolFunctionCatalogEntries.
type indexFile struct {
	SchemaVersion int     `json:"schema_version"`
	Entries       []Entry `json:"entries"`

	// schema_version >= 2: typed build records
	ResolvedToolSpecs []ResolvedToolSpec `json:"resolved_tool_specs,omitempty"`
	ToolBuildRecords  []ToolBuildRecord  `json:"tool_build_records,omitempty"`
	ToolImageRecords  []ToolImageRecord  `json:"tool_image_records,omitempty"`

	// schema_version >= 3: validation records + certification
	ToolCheckRecords           []ToolCheckRecord          `json:"tool_check_records,omitempty"`
	ToolScanRecords            []ToolScanRecord           `json:"tool_scan_records,omitempty"`
	CertifiedToolImageRecords  []CertifiedToolImageRecord `json:"certified_tool_image_records,omitempty"`
	ToolFunctionCatalogEntries []ToolFunctionCatalogEntry `json:"tool_function_catalog_entries,omitempty"`
}
