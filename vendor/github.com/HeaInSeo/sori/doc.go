// Package sori provides OCI-based packaging, push/fetch, and metadata helpers
// for reference datasets.
//
// Preferred entrypoint:
//  1. Load configuration with LoadConfig
//  2. Build a Client with Config.NewClient or NewClient
//  3. Package with Client.PackageVolumeWithOptions
//  4. Push with Client.PushPackagedVolumeWithOptions
//  5. Build generic metadata with BuildArtifactMetadata
//
// The Client path plus generic packaging, push, fetch, metadata, and core
// error/option surfaces form the preferred core path and the current stable
// core candidate.
//
// NodeVault-oriented data spec, registration, and referrer helpers remain in
// this package for compatibility, but they are still experimental and are not
// yet part of the frozen core contract.
//
// This package is intentionally in a soft-boundary stage: core and
// higher-level experimental helpers share the same package path today, but not
// the same stability level. Some experimental areas may be reevaluated before
// any future stable promotion.
package sori
