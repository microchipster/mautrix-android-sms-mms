// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Unknown-sender policy values for anti_spam.unknown_sender_policy.
const (
	// AntiSpamPolicyBridge is the least destructive default: the message is
	// bridged, the sender gets the "unknown sender" label and the management
	// room gets a notice.
	AntiSpamPolicyBridge = "bridge"
	// AntiSpamPolicyLabel bridges and labels the sender, without a notice.
	AntiSpamPolicyLabel = "label"
	// AntiSpamPolicyHold drops the message before it reaches Matrix and posts
	// a management-room notice. The sender is still labeled.
	AntiSpamPolicyHold = "hold"
)

// Pattern action values for anti_spam.patterns[].action.
const (
	AntiSpamActionFlag  = "flag"
	AntiSpamActionBlock = "block"
)

const (
	defaultAntiSpamFirstContactLimit  = 5
	defaultAntiSpamFirstContactWindow = 10 * time.Minute
	defaultAntiSpamThreshold          = 10
	defaultAntiSpamURLWeight          = 4
	defaultAntiSpamDuplicateCount     = 3
	defaultAntiSpamDuplicateWindow    = time.Hour
)

// defaultAntiSpamAllowLabels are the ghost labels that mark a sender as
// already vetted. Labels are managed with the `label`/`labels` commands.
var defaultAntiSpamAllowLabels = []string{"family", "service", "allow", "allowed"}

// defaultAntiSpamPatterns is the built-in keyword/heuristic scoring set. All
// default actions are "flag" (nothing is dropped without explicit opt-in);
// users can override the list or add their own patterns.
var defaultAntiSpamPatterns = []AntiSpamPatternConfig{
	{
		Name:   "urgency",
		Regex:  `(?i)\b(urgent|act now|immediately|final notice|last warning|expires (?:today|soon))\b`,
		Weight: 4,
		Action: AntiSpamActionFlag,
	},
	{
		Name:   "payment",
		Regex:  `(?i)\b(wire transfer|gift ?cards?|prepaid card|money order|western union|paypal|venmo|cash ?app|zelle)\b`,
		Weight: 6,
		Action: AntiSpamActionFlag,
	},
	{
		Name:   "crypto",
		Regex:  `(?i)\b(bitcoin|ethereum|crypto|usdt|wallet (?:address|seed))\b`,
		Weight: 6,
		Action: AntiSpamActionFlag,
	},
	{
		Name:   "prize",
		Regex:  `(?i)\b(you(?:'ve| have) won|congratulations|claim (?:your )?(?:prize|reward)|selected (?:as a )?winner)\b`,
		Weight: 5,
		Action: AntiSpamActionFlag,
	},
	{
		Name:   "credential",
		Regex:  `(?i)\b(verify your account|confirm your (?:account|identity)|account (?:is )?suspended|unusual activity|update your (?:password|payment))\b`,
		Weight: 6,
		Action: AntiSpamActionFlag,
	},
}

// defaultAntiSpamSuspiciousTLDs is the lexical TLD list from public abuse
// reports. It is intentionally conservative; users can clear or extend it.
var defaultAntiSpamSuspiciousTLDs = []string{
	"zip", "mov", "top", "xyz", "click", "link", "country", "gq", "tk", "ml",
	"cf", "ga", "cam", "rest", "surf", "monster",
}

// defaultAntiSpamShorteners is the built-in URL shortener host list.
var defaultAntiSpamShorteners = []string{
	"bit.ly", "t.co", "tinyurl.com", "goo.gl", "is.gd", "buff.ly", "ow.ly",
	"rebrand.ly", "cutt.ly", "shorturl.at", "rb.gy", "t.ly", "tiny.cc",
}

// AntiSpamPatternConfig is one keyword/regex scoring rule. Patterns are
// matched with Go's RE2 engine, like history_ttl.custom_patterns.
type AntiSpamPatternConfig struct {
	// Name identifies the pattern in decision reasons and `spam recent`.
	Name string `yaml:"name"`
	// Regex is the RE2 expression matched against the message body.
	Regex string `yaml:"regex"`
	// Weight is added to the score for every message the pattern matches.
	Weight int `yaml:"weight,omitempty"`
	// Action is "flag" (default) or "block". A block action drops the
	// message regardless of the score.
	Action string `yaml:"action,omitempty"`
}

// AntiSpamFirstContactConfig limits how many messages a previously unseen
// sender may bridge before they are flagged.
type AntiSpamFirstContactConfig struct {
	// Limit is the number of messages per window before flagging.
	Limit int `yaml:"limit,omitempty"`
	// Window is the sliding window the limit applies to.
	Window TTLDuration `yaml:"window,omitempty"`
}

// DefaultAntiSpamFirstContactConfig returns the first-contact limiter defaults.
func DefaultAntiSpamFirstContactConfig() AntiSpamFirstContactConfig {
	return AntiSpamFirstContactConfig{
		Limit:  defaultAntiSpamFirstContactLimit,
		Window: TTLDuration(defaultAntiSpamFirstContactWindow),
	}
}

// UnmarshalYAML applies per-field defaults so a partial mapping never
// disables the limiter.
func (c *AntiSpamFirstContactConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Limit  *int        `yaml:"limit"`
		Window TTLDuration `yaml:"window"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	cfg := DefaultAntiSpamFirstContactConfig()
	if raw.Limit != nil {
		cfg.Limit = *raw.Limit
	}
	if raw.Window > 0 {
		cfg.Window = raw.Window
	}
	*c = cfg
	return nil
}

// AntiSpamURLConfig configures the offline lexical URL analysis.
type AntiSpamURLConfig struct {
	// SuspiciousTLDs is matched against the final label of a URL host.
	SuspiciousTLDs []string `yaml:"suspicious_tlds,omitempty"`
	// Shorteners is matched against the full URL host (and its subdomains).
	Shorteners []string `yaml:"shorteners,omitempty"`
	// Weight is added to the score per distinct signal in a message.
	Weight int `yaml:"weight,omitempty"`
}

// DefaultAntiSpamURLConfig returns the URL analysis defaults.
func DefaultAntiSpamURLConfig() AntiSpamURLConfig {
	return AntiSpamURLConfig{
		SuspiciousTLDs: append([]string(nil), defaultAntiSpamSuspiciousTLDs...),
		Shorteners:     append([]string(nil), defaultAntiSpamShorteners...),
		Weight:         defaultAntiSpamURLWeight,
	}
}

// UnmarshalYAML applies per-field defaults for a partial mapping.
func (c *AntiSpamURLConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		SuspiciousTLDs *[]string `yaml:"suspicious_tlds"`
		Shorteners     *[]string `yaml:"shorteners"`
		Weight         *int      `yaml:"weight"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	cfg := DefaultAntiSpamURLConfig()
	if raw.SuspiciousTLDs != nil {
		cfg.SuspiciousTLDs = *raw.SuspiciousTLDs
	}
	if raw.Shorteners != nil {
		cfg.Shorteners = *raw.Shorteners
	}
	if raw.Weight != nil {
		cfg.Weight = *raw.Weight
	}
	*c = cfg
	return nil
}

// AntiSpamDuplicateConfig flags the same body hash when it arrives from
// several distinct senders inside a window.
type AntiSpamDuplicateConfig struct {
	// Window is the sliding window the count applies to.
	Window TTLDuration `yaml:"window,omitempty"`
	// Count is the number of distinct senders required to flag.
	Count int `yaml:"count,omitempty"`
}

// DefaultAntiSpamDuplicateConfig returns the duplicate detector defaults.
func DefaultAntiSpamDuplicateConfig() AntiSpamDuplicateConfig {
	return AntiSpamDuplicateConfig{
		Window: TTLDuration(defaultAntiSpamDuplicateWindow),
		Count:  defaultAntiSpamDuplicateCount,
	}
}

// UnmarshalYAML applies per-field defaults for a partial mapping.
func (c *AntiSpamDuplicateConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Window TTLDuration `yaml:"window"`
		Count  *int        `yaml:"count"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	cfg := DefaultAntiSpamDuplicateConfig()
	if raw.Window > 0 {
		cfg.Window = raw.Window
	}
	if raw.Count != nil {
		cfg.Count = *raw.Count
	}
	*c = cfg
	return nil
}

// AntiSpamNoticesConfig gates management-room notices.
type AntiSpamNoticesConfig struct {
	// Management posts a management-room notice for flagged/held/blocked
	// messages (and, with the default "bridge" policy, for new senders).
	Management bool `yaml:"management"`
}

// DefaultAntiSpamNoticesConfig returns the notice defaults.
func DefaultAntiSpamNoticesConfig() AntiSpamNoticesConfig {
	return AntiSpamNoticesConfig{Management: true}
}

// UnmarshalYAML applies the default when the mapping omits management.
func (c *AntiSpamNoticesConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Management *bool `yaml:"management"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	cfg := DefaultAntiSpamNoticesConfig()
	if raw.Management != nil {
		cfg.Management = *raw.Management
	}
	*c = cfg
	return nil
}

// AntiSpamConfig configures the local-only anti-spam layer. The whole feature
// is disabled unless `enabled: true`; see docs/anti-spam.md.
type AntiSpamConfig struct {
	// Enabled turns the inbound anti-spam gate on. Default false.
	Enabled bool `yaml:"enabled,omitempty"`
	// DryRun logs and announces decisions without applying labels or dropping
	// messages. Default false.
	DryRun bool `yaml:"dry_run,omitempty"`
	// UnknownSenderPolicy is "bridge" (default), "label" or "hold".
	UnknownSenderPolicy string `yaml:"unknown_sender_policy,omitempty"`
	// AllowLabels are the ghost labels that mark a sender as vetted, in
	// addition to senders that already have a bridged DM/portal.
	AllowLabels []string `yaml:"allow_labels,omitempty"`
	// Threshold is the score at which a message is flagged. Scores below the
	// threshold still bridge. 0 disables score-based flagging.
	Threshold int `yaml:"threshold,omitempty"`
	// Patterns is the keyword/regex scoring list.
	Patterns []AntiSpamPatternConfig `yaml:"patterns,omitempty"`
	// FirstContact limits first-contact bursts from unknown senders.
	FirstContact AntiSpamFirstContactConfig `yaml:"first_contact,omitempty"`
	// URLs configures the offline lexical URL analysis.
	URLs AntiSpamURLConfig `yaml:"urls,omitempty"`
	// Duplicates configures cross-sender duplicate detection.
	Duplicates AntiSpamDuplicateConfig `yaml:"duplicates,omitempty"`
	// Notices gates management-room notices.
	Notices AntiSpamNoticesConfig `yaml:"notices,omitempty"`
}

// DefaultAntiSpamConfig returns the anti-spam defaults: disabled, but with
// ready-to-use patterns, URL lists and thresholds for when it is enabled.
func DefaultAntiSpamConfig() AntiSpamConfig {
	return AntiSpamConfig{
		UnknownSenderPolicy: AntiSpamPolicyBridge,
		AllowLabels:         append([]string(nil), defaultAntiSpamAllowLabels...),
		Threshold:           defaultAntiSpamThreshold,
		Patterns:            defaultAntiSpamPatternsCopy(),
		FirstContact:        DefaultAntiSpamFirstContactConfig(),
		URLs:                DefaultAntiSpamURLConfig(),
		Duplicates:          DefaultAntiSpamDuplicateConfig(),
		Notices:             DefaultAntiSpamNoticesConfig(),
	}
}

// defaultAntiSpamPatternsCopy returns a deep copy of the built-in patterns so
// callers can mutate their config without touching the shared defaults.
func defaultAntiSpamPatternsCopy() []AntiSpamPatternConfig {
	out := make([]AntiSpamPatternConfig, len(defaultAntiSpamPatterns))
	copy(out, defaultAntiSpamPatterns)
	return out
}

// UnmarshalYAML applies defaults field-by-field, validates the pattern list at
// config parse time (typos fail at startup instead of silently doing nothing)
// and normalizes the allow/URL lists.
func (c *AntiSpamConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Enabled             *bool                       `yaml:"enabled"`
		DryRun              *bool                       `yaml:"dry_run"`
		UnknownSenderPolicy *string                     `yaml:"unknown_sender_policy"`
		AllowLabels         *[]string                   `yaml:"allow_labels"`
		Threshold           *int                        `yaml:"threshold"`
		Patterns            *[]AntiSpamPatternConfig    `yaml:"patterns"`
		FirstContact        *AntiSpamFirstContactConfig `yaml:"first_contact"`
		URLs                *AntiSpamURLConfig          `yaml:"urls"`
		Duplicates          *AntiSpamDuplicateConfig    `yaml:"duplicates"`
		Notices             *AntiSpamNoticesConfig      `yaml:"notices"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	cfg := DefaultAntiSpamConfig()
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if raw.DryRun != nil {
		cfg.DryRun = *raw.DryRun
	}
	if raw.UnknownSenderPolicy != nil {
		cfg.UnknownSenderPolicy = strings.ToLower(strings.TrimSpace(*raw.UnknownSenderPolicy))
	}
	if raw.AllowLabels != nil {
		cfg.AllowLabels = normalizeSpamStrings(*raw.AllowLabels, false)
	}
	if raw.Threshold != nil {
		cfg.Threshold = *raw.Threshold
	}
	if raw.Patterns != nil {
		cfg.Patterns = normalizeSpamPatterns(*raw.Patterns)
	}
	if raw.FirstContact != nil {
		cfg.FirstContact = *raw.FirstContact
	}
	if raw.URLs != nil {
		cfg.URLs = *raw.URLs
	}
	if raw.Duplicates != nil {
		cfg.Duplicates = *raw.Duplicates
	}
	if raw.Notices != nil {
		cfg.Notices = *raw.Notices
	}
	cfg.URLs.SuspiciousTLDs = normalizeSpamStrings(cfg.URLs.SuspiciousTLDs, true)
	cfg.URLs.Shorteners = normalizeSpamStrings(cfg.URLs.Shorteners, true)
	if err := validateAntiSpamConfig(cfg); err != nil {
		return err
	}
	*c = cfg
	return nil
}

// normalizeSpamPatterns trims names/actions and defaults the action to flag.
func normalizeSpamPatterns(patterns []AntiSpamPatternConfig) []AntiSpamPatternConfig {
	if len(patterns) == 0 {
		return nil
	}
	out := make([]AntiSpamPatternConfig, 0, len(patterns))
	for _, pattern := range patterns {
		pattern.Name = strings.TrimSpace(pattern.Name)
		pattern.Regex = strings.TrimSpace(pattern.Regex)
		pattern.Action = strings.ToLower(strings.TrimSpace(pattern.Action))
		if pattern.Action == "" {
			pattern.Action = AntiSpamActionFlag
		}
		out = append(out, pattern)
	}
	return out
}

// normalizeSpamStrings trims entries, drops blanks and lowercases them when
// lowercase is set. dropLeadingDot also strips a leading "." from TLD entries.
func normalizeSpamStrings(values []string, lowercase bool) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if lowercase {
			value = strings.ToLower(value)
		}
		value = strings.TrimPrefix(value, ".")
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

// validateAntiSpamConfig rejects impossible configurations at parse time.
func validateAntiSpamConfig(cfg AntiSpamConfig) error {
	switch cfg.UnknownSenderPolicy {
	case AntiSpamPolicyBridge, AntiSpamPolicyLabel, AntiSpamPolicyHold:
	default:
		return fmt.Errorf("anti_spam.unknown_sender_policy must be one of bridge, label, hold (got %q)", cfg.UnknownSenderPolicy)
	}
	if cfg.Threshold < 0 {
		return fmt.Errorf("anti_spam.threshold cannot be negative")
	}
	if cfg.URLs.Weight < 0 {
		return fmt.Errorf("anti_spam.urls.weight cannot be negative")
	}
	seenNames := make(map[string]struct{}, len(cfg.Patterns))
	for index, pattern := range cfg.Patterns {
		if pattern.Name == "" {
			return fmt.Errorf("anti_spam.patterns[%d] has an empty name", index)
		}
		if _, ok := seenNames[strings.ToLower(pattern.Name)]; ok {
			return fmt.Errorf("anti_spam.patterns contains duplicate name %q", pattern.Name)
		}
		seenNames[strings.ToLower(pattern.Name)] = struct{}{}
		if pattern.Regex == "" {
			return fmt.Errorf("anti_spam.patterns[%q] has an empty regex", pattern.Name)
		}
		if _, err := regexp.Compile(pattern.Regex); err != nil {
			return fmt.Errorf("invalid anti_spam.patterns[%q] regex: %w", pattern.Name, err)
		}
		if pattern.Weight < 0 {
			return fmt.Errorf("anti_spam.patterns[%q].weight cannot be negative", pattern.Name)
		}
		switch pattern.Action {
		case AntiSpamActionFlag, AntiSpamActionBlock:
		default:
			return fmt.Errorf("anti_spam.patterns[%q].action must be flag or block (got %q)", pattern.Name, pattern.Action)
		}
	}
	if cfg.FirstContact.Limit < 0 {
		return fmt.Errorf("anti_spam.first_contact.limit cannot be negative")
	}
	if cfg.FirstContact.Limit > 0 && cfg.FirstContact.Window <= 0 {
		return fmt.Errorf("anti_spam.first_contact.window must be positive when a limit is set")
	}
	if cfg.Duplicates.Count < 0 {
		return fmt.Errorf("anti_spam.duplicates.count cannot be negative")
	}
	if cfg.Duplicates.Count > 0 && cfg.Duplicates.Window <= 0 {
		return fmt.Errorf("anti_spam.duplicates.window must be positive when a count is set")
	}
	return nil
}

// antiSpamConfig returns the configured anti-spam settings (never zero).
func (c *MyConnector) antiSpamConfig() AntiSpamConfig {
	if c == nil || c.config == nil {
		return DefaultAntiSpamConfig()
	}
	return c.config.AntiSpam
}
