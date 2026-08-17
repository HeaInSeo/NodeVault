// Package catalog — casHash content-identity preimage rules.
//
// Constitution §1.2 (2026-08-17 clarification) requires that the casHash of a
// registered artifact identify its *content* only. Registration-time bookkeeping
// timestamps and the dual-axis lifecycle/integrity *state* (§1.4) must not enter
// the hash preimage: they change over an artifact's life without changing its
// content, so including them made casHash non-deterministic and let the same
// content produce different hashes at different times or in different states.
//
// This file is the single place that lists the excluded fields. SaveWithCasHash
// (tool and data) clears them before the content marshal and restores them
// afterward, so the stored file still carries the full record while the hash is
// computed over content alone. Adding a new registration-time / state field
// means adding it here (and to the determinism tests in cas_preimage_test.go),
// so the exclusion set can never silently drift out of sync with the record.
package catalog

import nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"

// casExcludedToolFields zeroes the fields excluded from a tool's casHash
// preimage and returns a closure that restores their original values. The four
// excluded fields (constitution §1.2 / §1.4):
//
//   - RegisteredAt                 registration-time timestamp
//   - Validation.LastValidatedAt   validation-time timestamp
//   - LifecyclePhase               lifecycle state axis
//   - IntegrityHealth              integrity state axis
func casExcludedToolFields(t *nfv1.RegisteredToolDefinition) func() {
	registeredAt := t.RegisteredAt
	lifecyclePhase := t.LifecyclePhase
	integrityHealth := t.IntegrityHealth
	t.RegisteredAt = 0
	t.LifecyclePhase = ""
	t.IntegrityHealth = ""

	// Validation may be nil at build time (validation not yet observed, §1.10);
	// only its timestamp is excluded. The pass/fail Phase, when observed by the
	// validation path, is genuine content and stays in the preimage.
	var restoreValidation func()
	if t.Validation != nil {
		lastValidatedAt := t.Validation.LastValidatedAt
		t.Validation.LastValidatedAt = 0
		restoreValidation = func() { t.Validation.LastValidatedAt = lastValidatedAt }
	}

	return func() {
		t.RegisteredAt = registeredAt
		t.LifecyclePhase = lifecyclePhase
		t.IntegrityHealth = integrityHealth
		if restoreValidation != nil {
			restoreValidation()
		}
	}
}

// casExcludedDataFields zeroes the fields excluded from a reference-data
// artifact's casHash preimage and returns a restore closure. Reference data has
// no Validation submessage, so three fields are excluded:
//
//   - RegisteredAt       registration-time timestamp
//   - LifecyclePhase     lifecycle state axis
//   - IntegrityHealth    integrity state axis
func casExcludedDataFields(d *nfv1.RegisteredDataDefinition) func() {
	registeredAt := d.RegisteredAt
	lifecyclePhase := d.LifecyclePhase
	integrityHealth := d.IntegrityHealth
	d.RegisteredAt = 0
	d.LifecyclePhase = ""
	d.IntegrityHealth = ""
	return func() {
		d.RegisteredAt = registeredAt
		d.LifecyclePhase = lifecyclePhase
		d.IntegrityHealth = integrityHealth
	}
}
