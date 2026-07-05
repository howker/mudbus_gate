package quality

// Package quality provides device-agnostic quality tagging for point
// readings, per FINAL_TRD.md section 5.7 and profile.schema.json's
// quality_rule.quality enum. This file holds the stable public surface
// (Tag, StatusValues) - matching evaluation logic lives in rules.go.
//
// Note on scope (ADR): CONTRACTS.md does not define a quality contract
// (IMPLEMENTATION_BACKLOG.md''s reference to "CONTRACTS section 5.3" for
// this module is stale - that section is actually about composite codec
// rules). This package''s shape follows LLD.md''s description (Tag,
// Evaluate(reading, deviceStatus, rules) Tag) adapted to the concrete
// types that exist in this codebase today: there is no internal/poller
// or pollcore.Reading yet (internal/device is the current vertical-slice
// stand-in for both), so Evaluate takes the point name/instance directly
// rather than a not-yet-existing Reading type.

// Tag is a quality tag, restricted to the vocabulary in FINAL_TRD.md
// section 5.7 / profile.schema.json quality_rule.quality enum.
type Tag string

const (
    Valid        Tag = "VALID"
    Verified     Tag = "VERIFIED"
    Estimated    Tag = "ESTIMATED"
    Interpolated Tag = "INTERPOLATED"
    Stale        Tag = "STALE"
    Invalid      Tag = "INVALID"
    NoData       Tag = "NO_DATA"
)

// StatusValues holds decoded status/bitfield source values, keyed by
// source point name and then by instance ("" for device-wide sources
// without a parametric instance). A source point that itself has a
// profile-level instance (e.g. a per-pipe alarm word) is expected to
// have one entry per instance key here; a device-wide source has a
// single "" entry.
type StatusValues map[string]map[string]int64