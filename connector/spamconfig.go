// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"fmt"
	"regexp"
	"sort"
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

// Management-notice modes for anti_spam.notices.mode.
const (
	// AntiSpamNoticeImmediate posts one management-room notice per decision.
	AntiSpamNoticeImmediate = "immediate"
	// AntiSpamNoticeDigest batches decisions into one periodic summary.
	AntiSpamNoticeDigest = "digest"
	// AntiSpamNoticeOff disables management-room notices.
	AntiSpamNoticeOff = "off"
)

// Sender classes detected by the offline classifier. The class weights and
// hooks live in anti_spam.sender_classes.
const (
	// SpamSenderClassShortcode is a 3-6 digit short code.
	SpamSenderClassShortcode = "shortcode"
	// SpamSenderClassTollFree is a NANP toll-free number (+1 800/833/...).
	SpamSenderClassTollFree = "toll_free"
	// SpamSenderClassAlphanumeric is a text sender ID such as "BANK".
	SpamSenderClassAlphanumeric = "alphanumeric"
	// SpamSenderClassInternational is an E.164 number outside HomeCountry.
	SpamSenderClassInternational = "international"
	// SpamSenderClassHome is a number in the configured home country.
	SpamSenderClassHome = "home"
)

// Per-contact policy override values for `spam policy <number> <policy>`.
const (
	// SpamContactPolicyAllow skips the gate entirely for this sender.
	SpamContactPolicyAllow = "allow"
	// SpamContactPolicyBridge treats the sender as a known contact.
	SpamContactPolicyBridge = AntiSpamPolicyBridge
	// SpamContactPolicyLabel forces the unknown-sender "label" path.
	SpamContactPolicyLabel = AntiSpamPolicyLabel
	// SpamContactPolicyHold drops messages from the sender like
	// unknown_sender_policy: hold.
	SpamContactPolicyHold = AntiSpamPolicyHold
	// SpamContactPolicyBlock drops messages from the sender like a block
	// pattern.
	SpamContactPolicyBlock = "block"
	// SpamContactPolicyClear removes a stored override.
	SpamContactPolicyClear = "clear"
)

const (
	defaultAntiSpamFirstContactLimit  = 5
	defaultAntiSpamFirstContactWindow = 10 * time.Minute
	defaultAntiSpamThreshold          = 10
	defaultAntiSpamURLWeight          = 4
	defaultAntiSpamDuplicateCount     = 3
	defaultAntiSpamDuplicateWindow    = time.Hour
	defaultAntiSpamDigestInterval     = time.Hour
	// defaultAntiSpamHomeCountry is the E.164 calling code assumed when the
	// config does not set sender_classes.home_country.
	defaultAntiSpamHomeCountry = "1"
)

// Default sender-class weights. A class weight is added to the score for
// messages from a sender of that class that is not known or allowlisted.
const (
	defaultAntiSpamShortcodeWeight     = 4
	defaultAntiSpamTollFreeWeight      = 3
	defaultAntiSpamAlphanumericWeight  = 6
	defaultAntiSpamInternationalWeight = 3
)

// Default reputation settings (anti_spam.reputation).
const (
	defaultAntiSpamReputationWeight          = 2
	defaultAntiSpamReputationFlagWeight      = 1
	defaultAntiSpamReputationBlockWeight     = 2
	defaultAntiSpamReputationVolumeThreshold = 10
	defaultAntiSpamReputationMaxScore        = 20
)

// defaultAntiSpamFeedWeight is the score added per matching offline feed
// signal (anti_spam.feeds.weight).
const defaultAntiSpamFeedWeight = 6

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
	// Mode is "immediate" (default), "digest" or "off". Digest batches
	// decisions into one periodic summary; off disables notices entirely.
	Mode string `yaml:"mode,omitempty"`
	// DigestInterval is how often a digest is posted. Default 1h; only used
	// when Mode is "digest".
	DigestInterval TTLDuration `yaml:"digest_interval,omitempty"`
}

// DefaultAntiSpamNoticesConfig returns the notice defaults.
func DefaultAntiSpamNoticesConfig() AntiSpamNoticesConfig {
	return AntiSpamNoticesConfig{
		Mode:           AntiSpamNoticeImmediate,
		DigestInterval: TTLDuration(defaultAntiSpamDigestInterval),
	}
}

// NoticeMode returns the effective notice mode, defaulting to immediate.
func (c AntiSpamNoticesConfig) NoticeMode() string {
	switch strings.ToLower(strings.TrimSpace(c.Mode)) {
	case AntiSpamNoticeDigest:
		return AntiSpamNoticeDigest
	case AntiSpamNoticeOff:
		return AntiSpamNoticeOff
	default:
		return AntiSpamNoticeImmediate
	}
}

// UnmarshalYAML applies per-field defaults. The legacy `management: false`
// setting is still accepted and maps to `mode: off`; an explicit mode wins.
func (c *AntiSpamNoticesConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Mode           *string     `yaml:"mode"`
		DigestInterval TTLDuration `yaml:"digest_interval"`
		Management     *bool       `yaml:"management"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	cfg := DefaultAntiSpamNoticesConfig()
	if raw.Mode != nil {
		cfg.Mode = strings.ToLower(strings.TrimSpace(*raw.Mode))
	} else if raw.Management != nil && !*raw.Management {
		cfg.Mode = AntiSpamNoticeOff
	}
	if raw.DigestInterval > 0 {
		cfg.DigestInterval = raw.DigestInterval
	}
	*c = cfg
	return nil
}

// AntiSpamSenderClassesConfig scores and routes messages by sender class.
type AntiSpamSenderClassesConfig struct {
	// ShortcodeWeight is added for unknown 3-6 digit short codes.
	ShortcodeWeight int `yaml:"shortcode_weight,omitempty"`
	// TollFreeWeight is added for unknown NANP toll-free numbers.
	TollFreeWeight int `yaml:"toll_free_weight,omitempty"`
	// AlphanumericWeight is added for unknown alphanumeric sender IDs.
	AlphanumericWeight int `yaml:"alphanumeric_weight,omitempty"`
	// InternationalWeight is added for unknown numbers outside HomeCountry.
	InternationalWeight int `yaml:"international_weight,omitempty"`
	// HoldClasses puts unknown senders of these classes into the hold path
	// (like unknown_sender_policy: hold) regardless of their score.
	HoldClasses []string `yaml:"hold_classes,omitempty"`
	// BlockClasses drops messages from these classes like a block pattern.
	BlockClasses []string `yaml:"block_classes,omitempty"`
	// HomeCountry is the E.164 calling code of the home country ("1", "+1",
	// "49"). Empty disables the international classification.
	HomeCountry string `yaml:"home_country,omitempty"`
}

// DefaultAntiSpamSenderClassesConfig returns the sender-class defaults.
func DefaultAntiSpamSenderClassesConfig() AntiSpamSenderClassesConfig {
	return AntiSpamSenderClassesConfig{
		ShortcodeWeight:     defaultAntiSpamShortcodeWeight,
		TollFreeWeight:      defaultAntiSpamTollFreeWeight,
		AlphanumericWeight:  defaultAntiSpamAlphanumericWeight,
		InternationalWeight: defaultAntiSpamInternationalWeight,
		HomeCountry:         defaultAntiSpamHomeCountry,
	}
}

// senderClassWeight returns the configured score for a class name, or 0.
func (c AntiSpamSenderClassesConfig) senderClassWeight(class string) int {
	switch class {
	case SpamSenderClassShortcode:
		return c.ShortcodeWeight
	case SpamSenderClassTollFree:
		return c.TollFreeWeight
	case SpamSenderClassAlphanumeric:
		return c.AlphanumericWeight
	case SpamSenderClassInternational:
		return c.InternationalWeight
	default:
		return 0
	}
}

// UnmarshalYAML applies per-field defaults for a partial mapping.
func (c *AntiSpamSenderClassesConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		ShortcodeWeight     *int      `yaml:"shortcode_weight"`
		TollFreeWeight      *int      `yaml:"toll_free_weight"`
		AlphanumericWeight  *int      `yaml:"alphanumeric_weight"`
		InternationalWeight *int      `yaml:"international_weight"`
		HoldClasses         *[]string `yaml:"hold_classes"`
		BlockClasses        *[]string `yaml:"block_classes"`
		HomeCountry         *string   `yaml:"home_country"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	cfg := DefaultAntiSpamSenderClassesConfig()
	if raw.ShortcodeWeight != nil {
		cfg.ShortcodeWeight = *raw.ShortcodeWeight
	}
	if raw.TollFreeWeight != nil {
		cfg.TollFreeWeight = *raw.TollFreeWeight
	}
	if raw.AlphanumericWeight != nil {
		cfg.AlphanumericWeight = *raw.AlphanumericWeight
	}
	if raw.InternationalWeight != nil {
		cfg.InternationalWeight = *raw.InternationalWeight
	}
	if raw.HoldClasses != nil {
		cfg.HoldClasses = normalizeSpamStrings(*raw.HoldClasses, true)
	}
	if raw.BlockClasses != nil {
		cfg.BlockClasses = normalizeSpamStrings(*raw.BlockClasses, true)
	}
	if raw.HomeCountry != nil {
		cfg.HomeCountry = normalizeSpamHomeCountry(*raw.HomeCountry)
	}
	*c = cfg
	return nil
}

// AntiSpamReputationConfig turns the bridge's own history with a sender into a
// score contribution (anti_spam.reputation).
type AntiSpamReputationConfig struct {
	// Weight multiplies the reputation factor. 0 disables reputation scoring.
	Weight int `yaml:"weight,omitempty"`
	// FlagWeight is the factor added per previously flagged/held message.
	FlagWeight int `yaml:"flag_weight,omitempty"`
	// BlockWeight is the factor added per previously blocked message.
	BlockWeight int `yaml:"block_weight,omitempty"`
	// VolumeThreshold is the messages/day value above which the excess adds
	// to the factor. 0 disables the volume term.
	VolumeThreshold int `yaml:"volume_threshold,omitempty"`
	// MaxScore caps the score added by reputation. 0 means uncapped.
	MaxScore int `yaml:"max_score,omitempty"`
}

// DefaultAntiSpamReputationConfig returns the reputation defaults.
func DefaultAntiSpamReputationConfig() AntiSpamReputationConfig {
	return AntiSpamReputationConfig{
		Weight:          defaultAntiSpamReputationWeight,
		FlagWeight:      defaultAntiSpamReputationFlagWeight,
		BlockWeight:     defaultAntiSpamReputationBlockWeight,
		VolumeThreshold: defaultAntiSpamReputationVolumeThreshold,
		MaxScore:        defaultAntiSpamReputationMaxScore,
	}
}

// UnmarshalYAML applies per-field defaults for a partial mapping.
func (c *AntiSpamReputationConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Weight          *int `yaml:"weight"`
		FlagWeight      *int `yaml:"flag_weight"`
		BlockWeight     *int `yaml:"block_weight"`
		VolumeThreshold *int `yaml:"volume_threshold"`
		MaxScore        *int `yaml:"max_score"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	cfg := DefaultAntiSpamReputationConfig()
	if raw.Weight != nil {
		cfg.Weight = *raw.Weight
	}
	if raw.FlagWeight != nil {
		cfg.FlagWeight = *raw.FlagWeight
	}
	if raw.BlockWeight != nil {
		cfg.BlockWeight = *raw.BlockWeight
	}
	if raw.VolumeThreshold != nil {
		cfg.VolumeThreshold = *raw.VolumeThreshold
	}
	if raw.MaxScore != nil {
		cfg.MaxScore = *raw.MaxScore
	}
	*c = cfg
	return nil
}

// AntiSpamFeedsConfig configures offline feed matching. Feed files are local:
// the bridge never fetches anything.
type AntiSpamFeedsConfig struct {
	// Paths are local files with one domain or sender per line, imported at
	// startup and with `spam import`. Plain URLhaus-style line lists work.
	Paths []string `yaml:"paths,omitempty"`
	// Weight is added to the score per matching feed signal (sender or URL)
	// in a message. 0 disables feed scoring, not importing.
	Weight int `yaml:"weight,omitempty"`
}

// DefaultAntiSpamFeedsConfig returns the feed defaults.
func DefaultAntiSpamFeedsConfig() AntiSpamFeedsConfig {
	return AntiSpamFeedsConfig{Weight: defaultAntiSpamFeedWeight}
}

// UnmarshalYAML applies per-field defaults for a partial mapping.
func (c *AntiSpamFeedsConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Paths  *[]string `yaml:"paths"`
		Weight *int      `yaml:"weight"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	cfg := DefaultAntiSpamFeedsConfig()
	if raw.Paths != nil {
		cfg.Paths = normalizeSpamPaths(*raw.Paths)
	}
	if raw.Weight != nil {
		cfg.Weight = *raw.Weight
	}
	*c = cfg
	return nil
}

// normalizeSpamPaths trims feed file paths and drops blanks. Unlike label or
// host lists, leading dots are meaningful in file paths.
func normalizeSpamPaths(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeSpamHomeCountry canonicalizes a configured E.164 calling code.
func normalizeSpamHomeCountry(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "+")
	value = strings.TrimPrefix(value, "00")
	return strings.TrimSpace(value)
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
	// SenderClasses scores short codes, toll-free numbers, alphanumeric
	// sender IDs and international numbers, and can hold/block whole classes.
	SenderClasses AntiSpamSenderClassesConfig `yaml:"sender_classes,omitempty"`
	// Reputation turns the bridge's own history with a sender into a score.
	Reputation AntiSpamReputationConfig `yaml:"reputation,omitempty"`
	// Feeds imports local line-based domain/sender lists for offline matching.
	Feeds AntiSpamFeedsConfig `yaml:"feeds,omitempty"`
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
		SenderClasses:       DefaultAntiSpamSenderClassesConfig(),
		Reputation:          DefaultAntiSpamReputationConfig(),
		Feeds:               DefaultAntiSpamFeedsConfig(),
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
		Enabled             *bool                        `yaml:"enabled"`
		DryRun              *bool                        `yaml:"dry_run"`
		UnknownSenderPolicy *string                      `yaml:"unknown_sender_policy"`
		AllowLabels         *[]string                    `yaml:"allow_labels"`
		Threshold           *int                         `yaml:"threshold"`
		Patterns            *[]AntiSpamPatternConfig     `yaml:"patterns"`
		FirstContact        *AntiSpamFirstContactConfig  `yaml:"first_contact"`
		URLs                *AntiSpamURLConfig           `yaml:"urls"`
		Duplicates          *AntiSpamDuplicateConfig     `yaml:"duplicates"`
		Notices             *AntiSpamNoticesConfig       `yaml:"notices"`
		SenderClasses       *AntiSpamSenderClassesConfig `yaml:"sender_classes"`
		Reputation          *AntiSpamReputationConfig    `yaml:"reputation"`
		Feeds               *AntiSpamFeedsConfig         `yaml:"feeds"`
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
	if raw.SenderClasses != nil {
		cfg.SenderClasses = *raw.SenderClasses
	}
	if raw.Reputation != nil {
		cfg.Reputation = *raw.Reputation
	}
	if raw.Feeds != nil {
		cfg.Feeds = *raw.Feeds
	}
	cfg.URLs.SuspiciousTLDs = normalizeSpamStrings(cfg.URLs.SuspiciousTLDs, true)
	cfg.URLs.Shorteners = normalizeSpamStrings(cfg.URLs.Shorteners, true)
	cfg.SenderClasses.HoldClasses = normalizeSpamStrings(cfg.SenderClasses.HoldClasses, true)
	cfg.SenderClasses.BlockClasses = normalizeSpamStrings(cfg.SenderClasses.BlockClasses, true)
	cfg.SenderClasses.HomeCountry = normalizeSpamHomeCountry(cfg.SenderClasses.HomeCountry)
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
	if mode := strings.ToLower(strings.TrimSpace(cfg.Notices.Mode)); mode != "" {
		switch mode {
		case AntiSpamNoticeImmediate, AntiSpamNoticeDigest, AntiSpamNoticeOff:
		default:
			return fmt.Errorf("anti_spam.notices.mode must be one of immediate, digest, off (got %q)", cfg.Notices.Mode)
		}
	}
	if cfg.Notices.DigestInterval < 0 {
		return fmt.Errorf("anti_spam.notices.digest_interval cannot be negative")
	}
	if cfg.SenderClasses.ShortcodeWeight < 0 || cfg.SenderClasses.TollFreeWeight < 0 ||
		cfg.SenderClasses.AlphanumericWeight < 0 || cfg.SenderClasses.InternationalWeight < 0 {
		return fmt.Errorf("anti_spam.sender_classes weights cannot be negative")
	}
	homeCountry := normalizeSpamHomeCountry(cfg.SenderClasses.HomeCountry)
	if homeCountry != "" {
		for _, r := range homeCountry {
			if r < '0' || r > '9' {
				return fmt.Errorf("anti_spam.sender_classes.home_country must be an E.164 calling code (got %q)", cfg.SenderClasses.HomeCountry)
			}
		}
	}
	holdClasses := make(map[string]struct{}, len(cfg.SenderClasses.HoldClasses))
	for _, class := range cfg.SenderClasses.HoldClasses {
		if !validSpamSenderClass(class) {
			return fmt.Errorf("anti_spam.sender_classes.hold_classes contains unknown class %q (valid: %s)", class, strings.Join(spamSenderClassNames(), ", "))
		}
		holdClasses[class] = struct{}{}
	}
	for _, class := range cfg.SenderClasses.BlockClasses {
		if !validSpamSenderClass(class) {
			return fmt.Errorf("anti_spam.sender_classes.block_classes contains unknown class %q (valid: %s)", class, strings.Join(spamSenderClassNames(), ", "))
		}
		if _, ok := holdClasses[class]; ok {
			return fmt.Errorf("anti_spam.sender_classes class %q cannot be in both hold_classes and block_classes", class)
		}
	}
	if cfg.Reputation.Weight < 0 || cfg.Reputation.FlagWeight < 0 || cfg.Reputation.BlockWeight < 0 ||
		cfg.Reputation.VolumeThreshold < 0 || cfg.Reputation.MaxScore < 0 {
		return fmt.Errorf("anti_spam.reputation values cannot be negative")
	}
	if cfg.Feeds.Weight < 0 {
		return fmt.Errorf("anti_spam.feeds.weight cannot be negative")
	}
	return nil
}

// spamSenderClassNames returns the known class names, sorted for stable output.
func spamSenderClassNames() []string {
	names := []string{
		SpamSenderClassAlphanumeric,
		SpamSenderClassHome,
		SpamSenderClassInternational,
		SpamSenderClassShortcode,
		SpamSenderClassTollFree,
	}
	sort.Strings(names)
	return names
}

// validSpamSenderClass reports whether a name is a known sender class.
func validSpamSenderClass(class string) bool {
	switch class {
	case SpamSenderClassShortcode, SpamSenderClassTollFree, SpamSenderClassAlphanumeric,
		SpamSenderClassInternational, SpamSenderClassHome:
		return true
	default:
		return false
	}
}

// validSpamContactPolicy reports whether a value is a per-contact override
// (without the "clear" pseudo-value).
func validSpamContactPolicy(policy string) bool {
	switch policy {
	case SpamContactPolicyAllow, SpamContactPolicyBridge, SpamContactPolicyLabel,
		SpamContactPolicyHold, SpamContactPolicyBlock:
		return true
	default:
		return false
	}
}

// antiSpamConfig returns the configured anti-spam settings (never zero).
func (c *MyConnector) antiSpamConfig() AntiSpamConfig {
	if c == nil || c.config == nil {
		return DefaultAntiSpamConfig()
	}
	return c.config.AntiSpam
}
