// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestAntiSpamConfigDefaults(t *testing.T) {
	cfg := unmarshalNetworkConfig(t, "network: {}\n").AntiSpam
	if cfg.Enabled {
		t.Error("anti-spam should be disabled by default")
	}
	if cfg.DryRun {
		t.Error("dry run should be off by default")
	}
	if cfg.UnknownSenderPolicy != AntiSpamPolicyBridge {
		t.Errorf("unknown sender policy = %q, want %q", cfg.UnknownSenderPolicy, AntiSpamPolicyBridge)
	}
	if len(cfg.AllowLabels) == 0 {
		t.Error("allow labels should have defaults")
	}
	if cfg.Threshold != defaultAntiSpamThreshold {
		t.Errorf("threshold = %d, want %d", cfg.Threshold, defaultAntiSpamThreshold)
	}
	if len(cfg.Patterns) != len(defaultAntiSpamPatterns) {
		t.Errorf("patterns = %d, want the %d built-in defaults", len(cfg.Patterns), len(defaultAntiSpamPatterns))
	}
	for _, pattern := range cfg.Patterns {
		if pattern.Name == "" || pattern.Regex == "" || pattern.Action != AntiSpamActionFlag {
			t.Errorf("default pattern is incomplete: %+v", pattern)
		}
	}
	if cfg.FirstContact.Limit != defaultAntiSpamFirstContactLimit {
		t.Errorf("first_contact.limit = %d, want %d", cfg.FirstContact.Limit, defaultAntiSpamFirstContactLimit)
	}
	if cfg.FirstContact.Window.Duration() != defaultAntiSpamFirstContactWindow {
		t.Errorf("first_contact.window = %s, want %s", cfg.FirstContact.Window.Duration(), defaultAntiSpamFirstContactWindow)
	}
	if len(cfg.URLs.SuspiciousTLDs) != len(defaultAntiSpamSuspiciousTLDs) || len(cfg.URLs.Shorteners) != len(defaultAntiSpamShorteners) {
		t.Errorf("URL lists = %+v", cfg.URLs)
	}
	if cfg.URLs.Weight != defaultAntiSpamURLWeight {
		t.Errorf("urls.weight = %d, want %d", cfg.URLs.Weight, defaultAntiSpamURLWeight)
	}
	if cfg.Duplicates.Count != defaultAntiSpamDuplicateCount || cfg.Duplicates.Window.Duration() != defaultAntiSpamDuplicateWindow {
		t.Errorf("duplicates = %+v", cfg.Duplicates)
	}
	if cfg.Notices.NoticeMode() != AntiSpamNoticeImmediate {
		t.Errorf("notices.mode = %q, want immediate", cfg.Notices.Mode)
	}
	if cfg.Notices.DigestInterval.Duration() != defaultAntiSpamDigestInterval {
		t.Errorf("notices.digest_interval = %s, want %s", cfg.Notices.DigestInterval.Duration(), defaultAntiSpamDigestInterval)
	}
	if cfg.SenderClasses.HomeCountry != defaultAntiSpamHomeCountry {
		t.Errorf("sender_classes.home_country = %q, want %q", cfg.SenderClasses.HomeCountry, defaultAntiSpamHomeCountry)
	}
	if cfg.SenderClasses.ShortcodeWeight != defaultAntiSpamShortcodeWeight ||
		cfg.SenderClasses.TollFreeWeight != defaultAntiSpamTollFreeWeight ||
		cfg.SenderClasses.AlphanumericWeight != defaultAntiSpamAlphanumericWeight ||
		cfg.SenderClasses.InternationalWeight != defaultAntiSpamInternationalWeight {
		t.Errorf("sender class weights = %+v", cfg.SenderClasses)
	}
	if cfg.Reputation.Weight != defaultAntiSpamReputationWeight ||
		cfg.Reputation.FlagWeight != defaultAntiSpamReputationFlagWeight ||
		cfg.Reputation.BlockWeight != defaultAntiSpamReputationBlockWeight ||
		cfg.Reputation.VolumeThreshold != defaultAntiSpamReputationVolumeThreshold ||
		cfg.Reputation.MaxScore != defaultAntiSpamReputationMaxScore {
		t.Errorf("reputation = %+v", cfg.Reputation)
	}
	if cfg.Feeds.Weight != defaultAntiSpamFeedWeight || len(cfg.Feeds.Paths) != 0 {
		t.Errorf("feeds = %+v", cfg.Feeds)
	}
}

func TestAntiSpamConfigPartialMappingKeepsDefaults(t *testing.T) {
	cfg := unmarshalNetworkConfig(t, `
network:
  anti_spam:
    enabled: true
    first_contact:
      limit: 2
    duplicates:
      count: 5
    urls:
      weight: 9
`).AntiSpam
	if !cfg.Enabled {
		t.Error("enabled was not applied")
	}
	if cfg.FirstContact.Limit != 2 {
		t.Errorf("first_contact.limit = %d, want 2", cfg.FirstContact.Limit)
	}
	if cfg.FirstContact.Window.Duration() != defaultAntiSpamFirstContactWindow {
		t.Errorf("first_contact.window = %s, want the default", cfg.FirstContact.Window.Duration())
	}
	if cfg.Duplicates.Count != 5 {
		t.Errorf("duplicates.count = %d, want 5", cfg.Duplicates.Count)
	}
	if cfg.Duplicates.Window.Duration() != defaultAntiSpamDuplicateWindow {
		t.Errorf("duplicates.window = %s, want the default", cfg.Duplicates.Window.Duration())
	}
	if cfg.URLs.Weight != 9 {
		t.Errorf("urls.weight = %d, want 9", cfg.URLs.Weight)
	}
	if len(cfg.URLs.SuspiciousTLDs) != len(defaultAntiSpamSuspiciousTLDs) {
		t.Error("urls.suspicious_tlds should keep the defaults")
	}
	if cfg.UnknownSenderPolicy != AntiSpamPolicyBridge || cfg.Threshold != defaultAntiSpamThreshold {
		t.Errorf("unset scalars lost their defaults: %+v", cfg)
	}
}

func TestAntiSpamConfigExplicitValues(t *testing.T) {
	cfg := unmarshalNetworkConfig(t, `
network:
  anti_spam:
    enabled: true
    dry_run: true
    unknown_sender_policy: HOLD
    allow_labels: [" family ", "", "vip"]
    threshold: 3
    patterns:
      - name: custom
        regex: 'foo+'
        weight: 7
        action: block
      - name: implicit-action
        regex: 'bar'
    first_contact:
      limit: 2
      window: 5m
    urls:
      suspicious_tlds: [".ZIP", "  ", "xyz"]
      shorteners: [Exa.Mple]
      weight: 2
    duplicates:
      count: 5
      window: 2h
    notices:
      management: false
`).AntiSpam
	if !cfg.Enabled || !cfg.DryRun {
		t.Error("enabled/dry_run were not applied")
	}
	if cfg.UnknownSenderPolicy != AntiSpamPolicyHold {
		t.Errorf("unknown_sender_policy = %q, want hold", cfg.UnknownSenderPolicy)
	}
	if len(cfg.AllowLabels) != 2 || cfg.AllowLabels[0] != "family" || cfg.AllowLabels[1] != "vip" {
		t.Errorf("allow_labels = %v", cfg.AllowLabels)
	}
	if cfg.Threshold != 3 {
		t.Errorf("threshold = %d, want 3", cfg.Threshold)
	}
	if len(cfg.Patterns) != 2 {
		t.Fatalf("patterns = %+v, want 2", cfg.Patterns)
	}
	if cfg.Patterns[0].Action != AntiSpamActionBlock || cfg.Patterns[1].Action != AntiSpamActionFlag {
		t.Errorf("pattern actions = %q, %q", cfg.Patterns[0].Action, cfg.Patterns[1].Action)
	}
	if cfg.FirstContact.Limit != 2 || cfg.FirstContact.Window.Duration() != 5*time.Minute {
		t.Errorf("first_contact = %+v", cfg.FirstContact)
	}
	if len(cfg.URLs.SuspiciousTLDs) != 2 || cfg.URLs.SuspiciousTLDs[0] != "zip" || cfg.URLs.SuspiciousTLDs[1] != "xyz" {
		t.Errorf("suspicious_tlds = %v", cfg.URLs.SuspiciousTLDs)
	}
	if len(cfg.URLs.Shorteners) != 1 || cfg.URLs.Shorteners[0] != "exa.mple" {
		t.Errorf("shorteners = %v", cfg.URLs.Shorteners)
	}
	if cfg.URLs.Weight != 2 {
		t.Errorf("urls.weight = %d, want 2", cfg.URLs.Weight)
	}
	if cfg.Duplicates.Count != 5 || cfg.Duplicates.Window.Duration() != 2*time.Hour {
		t.Errorf("duplicates = %+v", cfg.Duplicates)
	}
	if cfg.Notices.NoticeMode() != AntiSpamNoticeOff {
		t.Error("legacy notices.management: false should map to mode off")
	}
	if cfg.SenderClasses.HomeCountry != defaultAntiSpamHomeCountry || cfg.Reputation.Weight != defaultAntiSpamReputationWeight {
		t.Errorf("new sections lost their defaults: %+v %+v", cfg.SenderClasses, cfg.Reputation)
	}
}

func TestAntiSpamConfigEmptyListsClearDefaults(t *testing.T) {
	cfg := unmarshalNetworkConfig(t, `
network:
  anti_spam:
    allow_labels: []
    patterns: []
    urls:
      suspicious_tlds: []
      shorteners: []
`).AntiSpam
	if len(cfg.AllowLabels) != 0 || len(cfg.Patterns) != 0 {
		t.Errorf("explicit empty lists should clear the defaults: %+v", cfg.AllowLabels)
	}
	if len(cfg.URLs.SuspiciousTLDs) != 0 || len(cfg.URLs.Shorteners) != 0 {
		t.Errorf("explicit empty URL lists should clear the defaults: %+v", cfg.URLs)
	}
}

func TestAntiSpamConfigRejectsBadValues(t *testing.T) {
	tests := []string{
		"network:\n  anti_spam:\n    unknown_sender_policy: nope\n",
		"network:\n  anti_spam:\n    threshold: -1\n",
		"network:\n  anti_spam:\n    urls:\n      weight: -1\n",
		"network:\n  anti_spam:\n    first_contact:\n      limit: -1\n",
		"network:\n  anti_spam:\n    duplicates:\n      count: -1\n",
		"network:\n  anti_spam:\n    patterns:\n      - regex: 'x'\n",
		"network:\n  anti_spam:\n    patterns:\n      - name: bad\n",
		"network:\n  anti_spam:\n    patterns:\n      - name: bad\n        regex: '('\n",
		"network:\n  anti_spam:\n    patterns:\n      - name: bad\n        regex: 'x'\n        action: drop\n",
		"network:\n  anti_spam:\n    patterns:\n      - name: bad\n        regex: 'x'\n        weight: -1\n",
		"network:\n  anti_spam:\n    patterns:\n      - name: dup\n        regex: 'x'\n      - name: DUP\n        regex: 'y'\n",
	}
	for _, content := range tests {
		var wrapper struct {
			Network *SimpleConfig `yaml:"network"`
		}
		if err := yaml.Unmarshal([]byte(content), &wrapper); err == nil {
			t.Errorf("expected an error for config:\n%s", content)
		}
	}
}

func TestAntiSpamConfigRoundTrip(t *testing.T) {
	cfg := unmarshalNetworkConfig(t, `
network:
  anti_spam:
    enabled: true
    dry_run: true
    unknown_sender_policy: hold
    allow_labels: [family]
    threshold: 4
    patterns:
      - name: custom
        regex: 'foo+'
        weight: 7
        action: block
    first_contact:
      limit: 2
      window: 5m
    duplicates:
      count: 4
      window: 2h
    notices:
      mode: digest
      digest_interval: 30m
    sender_classes:
      shortcode_weight: 1
      hold_classes: [alphanumeric]
      home_country: "49"
    reputation:
      weight: 5
      max_score: 40
    feeds:
      paths: [/tmp/urlhaus.txt]
      weight: 7
`).AntiSpam
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var back AntiSpamConfig
	if err := yaml.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal error: %v (data: %s)", err, data)
	}
	if !back.Enabled || !back.DryRun || back.UnknownSenderPolicy != AntiSpamPolicyHold {
		t.Errorf("flags/policy mismatch after round trip:\n%s", data)
	}
	if back.Threshold != 4 || len(back.Patterns) != 1 || back.Patterns[0].Action != AntiSpamActionBlock ||
		back.Patterns[0].Weight != 7 || back.Patterns[0].Regex != `foo+` {
		t.Errorf("patterns/threshold mismatch after round trip:\n%s", data)
	}
	if back.FirstContact.Limit != 2 || back.FirstContact.Window.Duration() != 5*time.Minute {
		t.Errorf("first_contact mismatch after round trip: %+v\n%s", back.FirstContact, data)
	}
	if back.Duplicates.Count != 4 || back.Duplicates.Window.Duration() != 2*time.Hour {
		t.Errorf("duplicates mismatch after round trip: %+v\n%s", back.Duplicates, data)
	}
	if len(back.AllowLabels) != 1 || back.AllowLabels[0] != "family" {
		t.Errorf("allow_labels mismatch after round trip: %v", back.AllowLabels)
	}
	if back.URLs.Weight != defaultAntiSpamURLWeight {
		t.Errorf("urls defaults lost after round trip: %+v", back.URLs)
	}
	if back.Notices.NoticeMode() != AntiSpamNoticeDigest || back.Notices.DigestInterval.Duration() != 30*time.Minute {
		t.Errorf("notices mismatch after round trip: %+v", back.Notices)
	}
	if back.SenderClasses.ShortcodeWeight != 1 || back.SenderClasses.HomeCountry != "49" {
		t.Errorf("sender_classes mismatch after round trip: %+v", back.SenderClasses)
	}
	if len(back.SenderClasses.HoldClasses) != 1 || back.SenderClasses.HoldClasses[0] != "alphanumeric" {
		t.Errorf("hold_classes mismatch after round trip: %v", back.SenderClasses.HoldClasses)
	}
	if back.Reputation.Weight != 5 || back.Reputation.MaxScore != 40 || back.Reputation.FlagWeight != defaultAntiSpamReputationFlagWeight {
		t.Errorf("reputation mismatch after round trip: %+v", back.Reputation)
	}
	if len(back.Feeds.Paths) != 1 || back.Feeds.Paths[0] != "/tmp/urlhaus.txt" || back.Feeds.Weight != 7 {
		t.Errorf("feeds mismatch after round trip: %+v", back.Feeds)
	}
}

func TestAntiSpamConfigNoticesModes(t *testing.T) {
	digest := unmarshalNetworkConfig(t, `
network:
  anti_spam:
    notices:
      mode: DIGEST
      digest_interval: 30m
`).AntiSpam
	if digest.Notices.NoticeMode() != AntiSpamNoticeDigest || digest.Notices.DigestInterval.Duration() != 30*time.Minute {
		t.Errorf("digest notices = %+v", digest.Notices)
	}
	off := unmarshalNetworkConfig(t, "network:\n  anti_spam:\n    notices:\n      mode: off\n").AntiSpam
	if off.Notices.NoticeMode() != AntiSpamNoticeOff {
		t.Errorf("off notices = %+v", off.Notices)
	}
	if off.Notices.DigestInterval.Duration() != defaultAntiSpamDigestInterval {
		t.Errorf("off notices lost the digest interval default: %+v", off.Notices)
	}
	// An explicit mode wins over the legacy management flag.
	legacy := unmarshalNetworkConfig(t, `
network:
  anti_spam:
    notices:
      management: false
      mode: digest
`).AntiSpam
	if legacy.Notices.NoticeMode() != AntiSpamNoticeDigest {
		t.Errorf("explicit mode must win over management: %+v", legacy.Notices)
	}
}

func TestAntiSpamConfigNewSectionsExplicitValues(t *testing.T) {
	cfg := unmarshalNetworkConfig(t, `
network:
  anti_spam:
    sender_classes:
      shortcode_weight: 1
      toll_free_weight: 2
      alphanumeric_weight: 3
      international_weight: 4
      hold_classes: ["ALPHANUMERIC"]
      block_classes: [shortcode]
      home_country: "+49"
    reputation:
      weight: 5
      flag_weight: 2
      block_weight: 3
      volume_threshold: 20
      max_score: 50
    feeds:
      paths: [" /tmp/urlhaus.txt ", "", " .feeds/urlhaus.txt "]
      weight: 9
`).AntiSpam
	if cfg.SenderClasses.ShortcodeWeight != 1 || cfg.SenderClasses.TollFreeWeight != 2 ||
		cfg.SenderClasses.AlphanumericWeight != 3 || cfg.SenderClasses.InternationalWeight != 4 {
		t.Errorf("sender class weights = %+v", cfg.SenderClasses)
	}
	if len(cfg.SenderClasses.HoldClasses) != 1 || cfg.SenderClasses.HoldClasses[0] != "alphanumeric" {
		t.Errorf("hold_classes = %v", cfg.SenderClasses.HoldClasses)
	}
	if len(cfg.SenderClasses.BlockClasses) != 1 || cfg.SenderClasses.BlockClasses[0] != "shortcode" {
		t.Errorf("block_classes = %v", cfg.SenderClasses.BlockClasses)
	}
	if cfg.SenderClasses.HomeCountry != "49" {
		t.Errorf("home_country = %q, want 49", cfg.SenderClasses.HomeCountry)
	}
	if cfg.Reputation.Weight != 5 || cfg.Reputation.FlagWeight != 2 || cfg.Reputation.BlockWeight != 3 ||
		cfg.Reputation.VolumeThreshold != 20 || cfg.Reputation.MaxScore != 50 {
		t.Errorf("reputation = %+v", cfg.Reputation)
	}
	if len(cfg.Feeds.Paths) != 2 || cfg.Feeds.Paths[0] != "/tmp/urlhaus.txt" || cfg.Feeds.Paths[1] != ".feeds/urlhaus.txt" || cfg.Feeds.Weight != 9 {
		t.Errorf("feeds = %+v", cfg.Feeds)
	}
}

func TestAntiSpamConfigRejectsBadNewSections(t *testing.T) {
	tests := []string{
		"network:\n  anti_spam:\n    notices:\n      mode: nope\n",
		"network:\n  anti_spam:\n    notices:\n      digest_interval: -1m\n",
		"network:\n  anti_spam:\n    sender_classes:\n      shortcode_weight: -1\n",
		"network:\n  anti_spam:\n    sender_classes:\n      hold_classes: [nope]\n",
		"network:\n  anti_spam:\n    sender_classes:\n      block_classes: [nope]\n",
		"network:\n  anti_spam:\n    sender_classes:\n      hold_classes: [shortcode]\n      block_classes: [shortcode]\n",
		"network:\n  anti_spam:\n    sender_classes:\n      home_country: US\n",
		"network:\n  anti_spam:\n    reputation:\n      weight: -1\n",
		"network:\n  anti_spam:\n    feeds:\n      weight: -1\n",
	}
	for _, content := range tests {
		var wrapper struct {
			Network *SimpleConfig `yaml:"network"`
		}
		if err := yaml.Unmarshal([]byte(content), &wrapper); err == nil {
			t.Errorf("expected an error for config:\n%s", content)
		}
	}
}

func TestAntiSpamConfigEmptyMappingKeepsDefaults(t *testing.T) {
	cfg := unmarshalNetworkConfig(t, "network:\n  anti_spam: {}\n").AntiSpam
	if cfg.UnknownSenderPolicy != AntiSpamPolicyBridge || len(cfg.Patterns) != len(defaultAntiSpamPatterns) {
		t.Errorf("empty anti_spam mapping lost defaults: %+v", cfg)
	}
}

func TestAntiSpamPatternDefaultsCompileAndMatch(t *testing.T) {
	cfg := DefaultAntiSpamConfig()
	cfg.Enabled = true
	policy, err := compileSpamPolicy(cfg)
	if err != nil {
		t.Fatalf("compileSpamPolicy(defaults) error: %v", err)
	}
	if len(policy.Patterns) != len(defaultAntiSpamPatterns) {
		t.Fatalf("compiled patterns = %d, want %d", len(policy.Patterns), len(defaultAntiSpamPatterns))
	}
	decision := evaluateSpam(policy, spamEvaluation{Body: "This is URGENT, wire transfer now!"})
	if decision.Score < 10 {
		t.Errorf("default urgency+payment patterns scored %d, want >= 10", decision.Score)
	}
	found := 0
	for _, reason := range decision.Reasons {
		if reason == "pattern:urgency" || reason == "pattern:payment" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("reasons = %v, want urgency and payment", decision.Reasons)
	}
	if !strings.Contains(formatSpamNotice(spamRecord{Sender: "+15551234567", Reasons: decision.Reasons, Score: decision.Score}, decision, false), "+15551234567") {
		t.Error("notice should contain the sender address")
	}
}
