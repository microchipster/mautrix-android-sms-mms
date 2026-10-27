// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// TTLDuration is a YAML-friendly duration string: "12h", "30d", "365d", or
// "0"/empty for "no retention". Go's time.Duration cannot represent days and
// YAML cannot parse "30d" into an int64, so the config uses this type and the
// existing parseLookbackDuration helper.
type TTLDuration time.Duration

// Duration converts the configured value into a time.Duration.
func (d TTLDuration) Duration() time.Duration {
	return time.Duration(d)
}

// UnmarshalYAML parses a scalar duration such as "30d" or "12h".
func (d *TTLDuration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("history_ttl durations must be strings like \"30d\" or \"12h\"")
	}
	parsed, err := parseTTLDuration(node.Value)
	if err != nil {
		return err
	}
	*d = TTLDuration(parsed)
	return nil
}

// MarshalYAML keeps the config round-trippable when mxmain re-writes the file.
func (d TTLDuration) MarshalYAML() (any, error) {
	if d <= 0 {
		return nil, nil
	}
	return formatTTLDuration(time.Duration(d)), nil
}

// formatTTLDuration renders a duration in the same shape the config accepts
// (whole days as "30d", otherwise the usual Go duration string).
func formatTTLDuration(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	return d.String()
}

// parseTTLDuration parses a configured retention duration. Empty values, "0",
// "off", "never" and "none" all mean "no retention".
func parseTTLDuration(value string) (time.Duration, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "0", "off", "none", "never":
		return 0, nil
	}
	parsed, err := parseLookbackDuration(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid retention duration %q (use e.g. 12h, 30d, 365d, or 0 for none)", value)
	}
	return parsed, nil
}

// HistoryTTLConfig configures the Matrix-history retention cleanup. See
// docs/history-ttl.md for the full schema.
type HistoryTTLConfig struct {
	// Enabled turns the periodic cleanup on.
	Enabled bool `yaml:"enabled,omitempty"`
	// Global is the default retention period. Empty/0 means "keep forever".
	Global TTLDuration `yaml:"global,omitempty"`
	// PerRoom overrides the retention per Matrix room ID.
	PerRoom map[string]TTLDuration `yaml:"per_room,omitempty"`
	// PerContact overrides the retention per canonical phone number.
	// A value of 0 keeps that contact's history forever, even when a room or
	// global retention period is configured.
	PerContact map[string]TTLDuration `yaml:"per_contact,omitempty"`
	// Patterns selects named content patterns that are redacted regardless of
	// the retention period (e.g. one-time codes).
	Patterns []string `yaml:"patterns,omitempty"`
	// CustomPatterns extends the preset list with name -> regular expression.
	CustomPatterns map[string]string `yaml:"custom_patterns,omitempty"`
	// DeleteMedia includes messages carrying MMS attachments in the cleanup.
	DeleteMedia bool `yaml:"delete_media"`
	// DryRun only reports what the cleanup would do.
	DryRun bool `yaml:"dry_run,omitempty"`
	// Interval is how often the cleanup loop runs.
	Interval TTLDuration `yaml:"interval,omitempty"`
	// ScanWindow bounds the recent-message scan used for pattern matching.
	ScanWindow TTLDuration `yaml:"scan_window,omitempty"`
	// ScanLimit caps how many messages are inspected per portal and run.
	ScanLimit int `yaml:"scan_limit,omitempty"`
}

// DefaultHistoryTTLConfig returns the history retention defaults: disabled,
// media included, cleanup every 6h over a 30d pattern scan window.
func DefaultHistoryTTLConfig() HistoryTTLConfig {
	return HistoryTTLConfig{
		DeleteMedia: true,
		Interval:    TTLDuration(defaultHistoryTTLInterval),
		ScanWindow:  TTLDuration(defaultHistoryTTLScanWindow),
		ScanLimit:   defaultHistoryTTLScanLimit,
	}
}

// UnmarshalYAML applies per-field defaults so a partial `history_ttl` mapping
// never disables media deletion by accident, then validates the pattern
// configuration so typos fail at startup instead of silently doing nothing.
func (c *HistoryTTLConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Enabled        *bool                  `yaml:"enabled"`
		Global         TTLDuration            `yaml:"global"`
		PerRoom        map[string]TTLDuration `yaml:"per_room"`
		PerContact     map[string]TTLDuration `yaml:"per_contact"`
		Patterns       []string               `yaml:"patterns"`
		CustomPatterns map[string]string      `yaml:"custom_patterns"`
		DeleteMedia    *bool                  `yaml:"delete_media"`
		DryRun         *bool                  `yaml:"dry_run"`
		Interval       TTLDuration            `yaml:"interval"`
		ScanWindow     TTLDuration            `yaml:"scan_window"`
		ScanLimit      *int                   `yaml:"scan_limit"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	cfg := DefaultHistoryTTLConfig()
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	cfg.Global = raw.Global
	cfg.PerRoom = normalizePerRoomTTLs(raw.PerRoom)
	cfg.PerContact = normalizePerContactTTLs(raw.PerContact)
	cfg.Patterns = normalizeTTLPatternNames(raw.Patterns)
	cfg.CustomPatterns = raw.CustomPatterns
	if raw.DeleteMedia != nil {
		cfg.DeleteMedia = *raw.DeleteMedia
	}
	if raw.DryRun != nil {
		cfg.DryRun = *raw.DryRun
	}
	if raw.Interval > 0 {
		cfg.Interval = raw.Interval
	}
	if raw.ScanWindow > 0 {
		cfg.ScanWindow = raw.ScanWindow
	}
	if raw.ScanLimit != nil {
		if *raw.ScanLimit < 0 {
			return fmt.Errorf("history_ttl.scan_limit cannot be negative")
		}
		if *raw.ScanLimit > 0 {
			cfg.ScanLimit = *raw.ScanLimit
		}
	}
	if err := validateTTLPatterns(cfg.Patterns, cfg.CustomPatterns); err != nil {
		return err
	}
	*c = cfg
	return nil
}

// normalizePerRoomTTLs trims room IDs (exact Matrix room IDs) and drops blanks.
func normalizePerRoomTTLs(roomTTLs map[string]TTLDuration) map[string]TTLDuration {
	if len(roomTTLs) == 0 {
		return nil
	}
	out := make(map[string]TTLDuration, len(roomTTLs))
	for roomID, ttl := range roomTTLs {
		roomID = strings.TrimSpace(roomID)
		if roomID == "" {
			continue
		}
		out[roomID] = ttl
	}
	return out
}

// normalizePerContactTTLs canonicalizes phone numbers so the lookup at cleanup
// time matches portal contact addresses.
func normalizePerContactTTLs(contactTTLs map[string]TTLDuration) map[string]TTLDuration {
	if len(contactTTLs) == 0 {
		return nil
	}
	out := make(map[string]TTLDuration, len(contactTTLs))
	for contact, ttl := range contactTTLs {
		canonical := CanonicalAddress(contact)
		if canonical == "" {
			continue
		}
		out[canonical] = ttl
	}
	return out
}

func normalizeTTLPatternNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}
