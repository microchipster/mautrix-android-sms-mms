// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// testSpamPolicy compiles an enabled policy with the given mutations.
func testSpamPolicy(t *testing.T, mutate func(*AntiSpamConfig)) spamPolicy {
	t.Helper()
	cfg := DefaultAntiSpamConfig()
	cfg.Enabled = true
	if mutate != nil {
		mutate(&cfg)
	}
	policy, err := compileSpamPolicy(cfg)
	if err != nil {
		t.Fatalf("compileSpamPolicy() error: %v", err)
	}
	return policy
}

func TestCompileSpamPolicy(t *testing.T) {
	policy, err := compileSpamPolicy(AntiSpamConfig{Enabled: true, UnknownSenderPolicy: AntiSpamPolicyHold})
	if err != nil {
		t.Fatalf("compileSpamPolicy() error: %v", err)
	}
	if policy.UnknownSenderPolicy != AntiSpamPolicyHold {
		t.Errorf("policy = %q", policy.UnknownSenderPolicy)
	}
	if policy.FirstContactWindow != defaultAntiSpamFirstContactWindow || policy.DuplicateWindow != defaultAntiSpamDuplicateWindow {
		t.Errorf("windows not defaulted: %+v", policy)
	}
	if _, err := compileSpamPolicy(AntiSpamConfig{Enabled: true, Patterns: []AntiSpamPatternConfig{{Name: "bad", Regex: "("}}}); err == nil {
		t.Error("invalid regex must fail compilation")
	}

	partial, err := compileSpamPolicy(AntiSpamConfig{
		Enabled:     true,
		AllowLabels: []string{"VIP"},
		URLs: AntiSpamURLConfig{
			SuspiciousTLDs: []string{".ZIP"},
			Shorteners:     []string{"Bit.Ly"},
			Weight:         3,
		},
	})
	if err != nil {
		t.Fatalf("compileSpamPolicy(partial) error: %v", err)
	}
	if _, ok := partial.AllowLabels["vip"]; !ok {
		t.Errorf("allow labels not normalized: %v", partial.AllowLabels)
	}
	if _, ok := partial.SuspiciousTLDs["zip"]; !ok {
		t.Errorf("TLDs not normalized: %v", partial.SuspiciousTLDs)
	}
	if _, ok := partial.Shorteners["bit.ly"]; !ok {
		t.Errorf("shorteners not normalized: %v", partial.Shorteners)
	}
}

func TestEvaluateSpamDisabled(t *testing.T) {
	policy, err := compileSpamPolicy(DefaultAntiSpamConfig())
	if err != nil {
		t.Fatalf("compileSpamPolicy() error: %v", err)
	}
	decision := evaluateSpam(policy, spamEvaluation{Body: "URGENT wire transfer"})
	if decision.Action != spamActionAllow || decision.Score != 0 {
		t.Errorf("disabled policy decision = %+v, want allow", decision)
	}
}

func TestEvaluateSpamUnknownSenderPolicy(t *testing.T) {
	tests := []struct {
		name       string
		policy     string
		wantAction string
		wantNotify bool
	}{
		{name: "bridge", policy: AntiSpamPolicyBridge, wantAction: spamActionLabel, wantNotify: true},
		{name: "label", policy: AntiSpamPolicyLabel, wantAction: spamActionLabel, wantNotify: false},
		{name: "hold", policy: AntiSpamPolicyHold, wantAction: spamActionHold, wantNotify: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := testSpamPolicy(t, func(cfg *AntiSpamConfig) {
				cfg.UnknownSenderPolicy = test.policy
				cfg.Patterns = nil
			})
			decision := evaluateSpam(policy, spamEvaluation{Body: "see you at 8"})
			if decision.Action != test.wantAction || decision.Notify != test.wantNotify {
				t.Fatalf("decision = %+v, want action %q notify %t", decision, test.wantAction, test.wantNotify)
			}
			if test.policy == AntiSpamPolicyHold && !decision.dropped() {
				t.Error("hold decision must drop the message")
			}
			if !hasReason(decision, "unknown-sender") {
				t.Errorf("reasons = %v, want unknown-sender", decision.Reasons)
			}
		})
	}
}

func TestEvaluateSpamKnownOrAllowlistedSender(t *testing.T) {
	policy := testSpamPolicy(t, func(cfg *AntiSpamConfig) {
		cfg.UnknownSenderPolicy = AntiSpamPolicyHold
		cfg.Patterns = nil
	})
	if decision := evaluateSpam(policy, spamEvaluation{Body: "hi", KnownContact: true}); decision.Action != spamActionAllow {
		t.Errorf("known contact decision = %+v, want allow", decision)
	}
	if decision := evaluateSpam(policy, spamEvaluation{Body: "hi", Labels: []string{"Family"}}); decision.Action != spamActionAllow {
		t.Errorf("allowlisted label decision = %+v, want allow", decision)
	}
	// "new contact" (the automatic label) is not an allowlist entry.
	if decision := evaluateSpam(policy, spamEvaluation{Body: "hi", Labels: []string{NewContactLabel}}); decision.Action != spamActionHold {
		t.Errorf("new-contact-only decision = %+v, want hold", decision)
	}
}

func TestEvaluateSpamPatternScoring(t *testing.T) {
	policy := testSpamPolicy(t, func(cfg *AntiSpamConfig) {
		cfg.Threshold = 5
		cfg.Patterns = []AntiSpamPatternConfig{
			{Name: "kw", Regex: `(?i)urgent`, Weight: 3, Action: AntiSpamActionFlag},
			{Name: "money", Regex: `(?i)wire`, Weight: 4, Action: AntiSpamActionFlag},
		}
	})
	// Both patterns match: score 7 >= threshold 5.
	decision := evaluateSpam(policy, spamEvaluation{Body: "urgent wire", KnownContact: true})
	if decision.Action != spamActionFlag || decision.Score != 7 {
		t.Fatalf("decision = %+v, want flag with score 7", decision)
	}
	if !hasReason(decision, "pattern:kw") || !hasReason(decision, "pattern:money") || !hasReason(decision, "score") {
		t.Errorf("reasons = %v", decision.Reasons)
	}
	// One pattern matches: score 3 < threshold 5 stays allow for a known sender.
	decision = evaluateSpam(policy, spamEvaluation{Body: "urgent", KnownContact: true})
	if decision.Action != spamActionAllow || decision.Score != 3 {
		t.Fatalf("decision = %+v, want allow with score 3", decision)
	}
}

func TestEvaluateSpamPatternBlockAction(t *testing.T) {
	policy := testSpamPolicy(t, func(cfg *AntiSpamConfig) {
		cfg.Threshold = 10
		cfg.Patterns = []AntiSpamPatternConfig{
			{Name: "crypto", Regex: `(?i)bitcoin`, Weight: 1, Action: AntiSpamActionBlock},
		}
	})
	decision := evaluateSpam(policy, spamEvaluation{Body: "send bitcoin", KnownContact: true})
	if decision.Action != spamActionBlock || !decision.dropped() || !decision.Notify {
		t.Fatalf("decision = %+v, want a notified block", decision)
	}
	if !hasReason(decision, "pattern:crypto") {
		t.Errorf("reasons = %v", decision.Reasons)
	}
	// Below-threshold non-block patterns stay allow.
	decision = evaluateSpam(policy, spamEvaluation{Body: "hello", KnownContact: true})
	if decision.Action != spamActionAllow {
		t.Fatalf("decision = %+v, want allow", decision)
	}
}

func TestEvaluateSpamURLSignals(t *testing.T) {
	policy := testSpamPolicy(t, func(cfg *AntiSpamConfig) {
		cfg.Threshold = 8
		cfg.Patterns = nil
		cfg.URLs = AntiSpamURLConfig{
			SuspiciousTLDs: []string{"zip"},
			Shorteners:     []string{"bit.ly"},
			Weight:         4,
		}
	})
	tests := []struct {
		name       string
		body       string
		wantScore  int
		wantSignal string
		wantAction string
	}{
		{name: "suspicious tld", body: "get it at http://promo.zip/x", wantScore: 4, wantSignal: "url:suspicious-tld", wantAction: spamActionAllow},
		{name: "shortener", body: "short http://bit.ly/abc", wantScore: 4, wantSignal: "url:shortener", wantAction: spamActionAllow},
		{name: "both", body: "http://bit.ly/abc and http://promo.zip", wantScore: 8, wantSignal: "url:shortener", wantAction: spamActionFlag},
		{name: "signal counted once", body: "http://bit.ly/a and http://bit.ly/b", wantScore: 4, wantSignal: "url:shortener", wantAction: spamActionAllow},
		{name: "clean url", body: "https://example.com/page", wantScore: 0, wantSignal: "", wantAction: spamActionAllow},
		{name: "subdomain shortener", body: "https://www.bit.ly/abc", wantScore: 4, wantSignal: "url:shortener", wantAction: spamActionAllow},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := evaluateSpam(policy, spamEvaluation{Body: test.body, KnownContact: true})
			if decision.Score != test.wantScore {
				t.Errorf("score = %d, want %d (%v)", decision.Score, test.wantScore, decision.Reasons)
			}
			if test.wantSignal != "" && !hasReason(decision, test.wantSignal) {
				t.Errorf("reasons = %v, want %s", decision.Reasons, test.wantSignal)
			}
			if decision.Action != test.wantAction {
				t.Errorf("action = %q, want %q", decision.Action, test.wantAction)
			}
		})
	}
}

func TestEvaluateSpamDuplicates(t *testing.T) {
	policy := testSpamPolicy(t, func(cfg *AntiSpamConfig) {
		cfg.Patterns = nil
		cfg.Threshold = 0
		cfg.Duplicates = AntiSpamDuplicateConfig{Window: TTLDuration(time.Hour), Count: 3}
	})
	if decision := evaluateSpam(policy, spamEvaluation{Body: "party", KnownContact: true, DuplicateCount: 2}); decision.Action != spamActionAllow {
		t.Errorf("two senders must not flag: %+v", decision)
	}
	decision := evaluateSpam(policy, spamEvaluation{Body: "party", KnownContact: true, DuplicateCount: 3})
	if decision.Action != spamActionFlag || !hasReason(decision, "duplicate") {
		t.Errorf("three senders must flag: %+v", decision)
	}
}

func TestEvaluateSpamFirstContactRate(t *testing.T) {
	policy := testSpamPolicy(t, func(cfg *AntiSpamConfig) {
		cfg.Patterns = nil
		cfg.Threshold = 0
		cfg.FirstContact = AntiSpamFirstContactConfig{Limit: 5, Window: TTLDuration(10 * time.Minute)}
	})
	if decision := evaluateSpam(policy, spamEvaluation{Body: "hi", RateCount: 5}); decision.Action != spamActionLabel {
		t.Errorf("five messages must stay label: %+v", decision)
	}
	decision := evaluateSpam(policy, spamEvaluation{Body: "hi", RateCount: 6})
	if decision.Action != spamActionFlag || !hasReason(decision, "first-contact-rate") {
		t.Errorf("six messages must flag: %+v", decision)
	}
	// A known sender is never rate limited.
	if decision := evaluateSpam(policy, spamEvaluation{Body: "hi", KnownContact: true, RateCount: 99}); decision.Action != spamActionAllow {
		t.Errorf("known sender must not be rate limited: %+v", decision)
	}
}

func TestSpamBodyHash(t *testing.T) {
	if spamBodyHash("") != "" || spamBodyHash("   ") != "" {
		t.Error("empty bodies must hash to an empty string")
	}
	if spamBodyHash("Hello  World") != spamBodyHash("hello world") {
		t.Error("case and whitespace must normalize")
	}
	if spamBodyHash("hello") == spamBodyHash("hello!") {
		t.Error("different bodies must hash differently")
	}
}

func TestSpamRateLimiter(t *testing.T) {
	limiter := newSpamRateLimiter(10 * time.Minute)
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for i, want := range []int{1, 2, 3} {
		if got := limiter.record("+15551234567", base.Add(time.Duration(i)*time.Minute)); got != want {
			t.Fatalf("record %d = %d, want %d", i, got, want)
		}
	}
	if got := limiter.record("+15550000000", base); got != 1 {
		t.Errorf("other sender count = %d, want 1", got)
	}
	// Window expiry: only this message remains inside the window.
	if got := limiter.record("+15551234567", base.Add(21*time.Minute)); got != 1 {
		t.Errorf("after window count = %d, want 1", got)
	}
	if got := limiter.record("", base); got != 0 {
		t.Errorf("empty sender count = %d, want 0", got)
	}
}

func TestSpamDuplicateTracker(t *testing.T) {
	tracker := newSpamDuplicateTracker(time.Hour)
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if got := tracker.record("same body", "+15551111111", base); got != 1 {
		t.Fatalf("first sender count = %d, want 1", got)
	}
	if got := tracker.record("SAME   body", "+15552222222", base.Add(time.Minute)); got != 2 {
		t.Errorf("second sender count = %d, want 2", got)
	}
	if got := tracker.record("same body", "+15552222222", base.Add(2*time.Minute)); got != 2 {
		t.Errorf("repeat sender count = %d, want 2", got)
	}
	if got := tracker.record("other", "+15553333333", base); got != 1 {
		t.Errorf("other body count = %d, want 1", got)
	}
	if got := tracker.record("", "+15554444444", base); got != 0 {
		t.Errorf("empty body count = %d, want 0", got)
	}
	// The window expires all earlier observations.
	if got := tracker.record("same body", "+15559999999", base.Add(2*time.Hour)); got != 1 {
		t.Errorf("after window count = %d, want 1", got)
	}
}

func TestFormatSpamNotice(t *testing.T) {
	record := spamRecord{Sender: "+15551234567", Score: 12, Reasons: []string{"pattern:urgency", "url:shortener"}}
	tests := []struct {
		name     string
		decision spamDecision
		dryRun   bool
		want     []string
		notWant  string
	}{
		{name: "flag", decision: spamDecision{Action: spamActionFlag}, want: []string{"flagged", "+15551234567", "pattern:urgency"}},
		{name: "hold", decision: spamDecision{Action: spamActionHold}, want: []string{"held", "not bridged"}},
		{name: "block", decision: spamDecision{Action: spamActionBlock}, want: []string{"blocked", "not bridged"}},
		{name: "hold dry run", decision: spamDecision{Action: spamActionHold}, dryRun: true, want: []string{"dry run", "would hold", "bridged anyway"}, notWant: "was not bridged"},
		{name: "block dry run", decision: spamDecision{Action: spamActionBlock}, dryRun: true, want: []string{"dry run", "would block", "bridged anyway"}, notWant: "was not bridged"},
		{name: "label", decision: spamDecision{Action: spamActionLabel}, want: []string{"new sender", SpamUnknownLabel}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := formatSpamNotice(record, test.decision, test.dryRun)
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Errorf("notice %q missing %q", got, want)
				}
			}
			if test.notWant != "" && strings.Contains(got, test.notWant) {
				t.Errorf("notice %q must not contain %q", got, test.notWant)
			}
		})
	}
}

func TestFormatSpamStatusAndRecent(t *testing.T) {
	cfg := DefaultAntiSpamConfig()
	cfg.Enabled = true
	cfg.DryRun = true
	connector := &MyConnector{config: &SimpleConfig{AntiSpam: cfg}}
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	connector.recordSpamDecision(spamRecord{Time: base, Sender: "+15551111111", Action: spamActionAllow})
	connector.recordSpamDecision(spamRecord{
		Time: base.Add(time.Minute), Sender: "+15552222222", MessageID: "sms:1:1",
		Action: spamActionFlag, Score: 12, Reasons: []string{"pattern:urgency"}, DryRun: true,
	})

	status := connector.formatSpamStatus()
	for _, want := range []string{
		"Anti-spam: enabled (dry run: true)",
		"Unknown sender policy: bridge",
		"Score threshold: 10",
		"Decisions: 2 evaluated — 1 allowed, 0 labeled, 1 flagged, 0 held, 0 blocked (1 dry run)",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}

	recent := connector.formatSpamRecent(10)
	if !strings.Contains(recent, "+15552222222") || !strings.Contains(recent, "flag") || !strings.Contains(recent, "pattern:urgency") {
		t.Errorf("recent = %q", recent)
	}
	if strings.Contains(recent, "+15551111111") {
		t.Error("allow decisions must not appear in `spam recent`")
	}
	if !strings.Contains(recent, "[dry run]") {
		t.Error("dry-run marker missing from recent decisions")
	}

	// Newest first.
	connector.recordSpamDecision(spamRecord{Time: base.Add(2 * time.Minute), Sender: "+15553333333", Action: spamActionBlock})
	recent = connector.formatSpamRecent(10)
	if strings.Index(recent, "+15553333333") > strings.Index(recent, "+15552222222") {
		t.Errorf("decisions are not newest-first:\n%s", recent)
	}
	if got := connector.formatSpamRecent(1); strings.Contains(got, "+15552222222") {
		t.Errorf("recent(1) = %q, want only the newest", got)
	}
}

func TestParseSpamRecentCount(t *testing.T) {
	if count, err := parseSpamRecentCount("5"); err != nil || count != 5 {
		t.Errorf("parseSpamRecentCount(5) = %d, %v", count, err)
	}
	if _, err := parseSpamRecentCount("nope"); err == nil {
		t.Error("non-numeric count must fail")
	}
	if _, err := parseSpamRecentCount("0"); err == nil {
		t.Error("zero count must fail")
	}
	if count, err := parseSpamRecentCount("100000"); err != nil || count != spamRecentCapacity {
		t.Errorf("parseSpamRecentCount(big) = %d, %v; want the capacity cap", count, err)
	}
}

func TestApplySpamMessageMetadata(t *testing.T) {
	converted := &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
		DBMetadata: &MessageMetadata{Body: "text"},
	}}}
	applySpamMessageMetadata(converted, spamDecision{
		Action:  spamActionFlag,
		Score:   9,
		Reasons: []string{"pattern:urgency"},
	})
	meta, _ := converted.Parts[0].DBMetadata.(*MessageMetadata)
	if meta == nil || meta.SpamAction != spamActionFlag || meta.SpamScore != 9 || len(meta.SpamReasons) != 1 {
		t.Fatalf("spam metadata = %+v", meta)
	}
	// Allow decisions leave the metadata untouched.
	applySpamMessageMetadata(converted, spamDecision{Action: spamActionAllow})
	if meta.SpamAction != spamActionFlag {
		t.Error("allow decision must not overwrite recorded metadata")
	}
}

// newSpamGateTestClient builds a real SQLite-backed bridge with an anti-spam
// configuration, mirroring how the gate runs in production.
func newSpamGateTestClient(t *testing.T, mutate func(*AntiSpamConfig)) (*testBridgeDB, *MyNetworkClient, *bridgev2.Bridge, *MyConnector) {
	t.Helper()
	b, bridge := newMergeRoutingTestBridge(t)
	connector := bridge.Network.(*MyConnector)
	cfg := DefaultAntiSpamConfig()
	cfg.Enabled = true
	if mutate != nil {
		mutate(&cfg)
	}
	connector.config.AntiSpam = cfg
	login := &bridgev2.UserLogin{UserLogin: &database.UserLogin{
		ID:       "app:test-login",
		Metadata: &LoginMetadata{Transport: LoginTransportApp},
	}}
	nc := &MyNetworkClient{log: zerolog.Nop(), bridge: bridge, login: login, connector: connector}
	return b, nc, bridge, connector
}

func TestSpamGateFlagsAndLabelsAgainstDatabase(t *testing.T) {
	b, nc, bridge, connector := newSpamGateTestClient(t, nil)
	ctx := context.Background()
	sender := "+15551234567"
	b.seedGhost(t, string(MakeUserID(sender)))

	decision, drop := nc.spamGate(ctx, networkid.PortalKey{ID: "thread-scope-1-1"}, sender, "sms:1:1",
		"URGENT: wire transfer now", time.Now().UTC(), false)
	if decision.Action != spamActionFlag {
		t.Fatalf("decision = %+v, want flag", decision)
	}
	if drop {
		t.Fatal("a flagged message must still bridge")
	}
	ghost, err := bridge.GetExistingGhostByID(ctx, MakeUserID(sender))
	if err != nil || ghost == nil {
		t.Fatalf("load ghost: %v, %v", ghost, err)
	}
	meta, _ := ghost.Metadata.(*GhostMetadata)
	if meta == nil || !hasLabel(meta.Labels, SpamFlagLabel) {
		t.Fatalf("ghost labels = %+v, want %q", meta, SpamFlagLabel)
	}
	counters, recent, _ := connector.spamSnapshot()
	if counters.Evaluated != 1 || counters.Flagged != 1 {
		t.Errorf("counters = %+v", counters)
	}
	if len(recent) != 1 || recent[0].Action != spamActionFlag || recent[0].Sender != sender {
		t.Errorf("recent = %+v", recent)
	}

	// A known contact with a benign body passes without a decision.
	b.seedPortal(t, "thread-scope-1-1", "!room1:example.com", &PortalMetadata{
		ThreadID: 1, SubID: 1, Recipients: []string{sender},
	})
	decision, drop = nc.spamGate(ctx, networkid.PortalKey{ID: "thread-scope-1-1"}, sender, "sms:1:2", "see you at 8", time.Now().UTC(), false)
	if decision.Action != spamActionAllow || drop {
		t.Fatalf("known contact decision = %+v", decision)
	}
	counters, _, _ = connector.spamSnapshot()
	if counters.Evaluated != 2 || counters.Allowed != 1 {
		t.Errorf("counters after allow = %+v", counters)
	}
}

func TestSpamGateDryRunDoesNotApply(t *testing.T) {
	b, nc, bridge, connector := newSpamGateTestClient(t, func(cfg *AntiSpamConfig) {
		cfg.DryRun = true
	})
	ctx := context.Background()
	sender := "+15551234567"
	b.seedGhost(t, string(MakeUserID(sender)))

	decision, drop := nc.spamGate(ctx, networkid.PortalKey{ID: "thread-scope-1-1"}, sender, "sms:1:1",
		"URGENT: wire transfer now", time.Now().UTC(), false)
	if decision.Action != spamActionFlag || drop {
		t.Fatalf("dry-run decision = %+v, drop %t", decision, drop)
	}
	ghost, err := bridge.GetExistingGhostByID(ctx, MakeUserID(sender))
	if err != nil || ghost == nil {
		t.Fatalf("load ghost: %v, %v", ghost, err)
	}
	meta, _ := ghost.Metadata.(*GhostMetadata)
	if meta != nil && hasLabel(meta.Labels, SpamFlagLabel) {
		t.Fatalf("dry run must not apply labels: %+v", meta.Labels)
	}
	counters, recent, _ := connector.spamSnapshot()
	if counters.DryRun != 1 || counters.Flagged != 1 {
		t.Errorf("dry-run counters = %+v", counters)
	}
	if len(recent) != 1 || !recent[0].DryRun {
		t.Errorf("recent = %+v", recent)
	}
}

func TestSpamGateHoldsUnknownSender(t *testing.T) {
	b, nc, bridge, _ := newSpamGateTestClient(t, func(cfg *AntiSpamConfig) {
		cfg.UnknownSenderPolicy = AntiSpamPolicyHold
	})
	ctx := context.Background()
	sender := "+15551234567"
	b.seedGhost(t, string(MakeUserID(sender)))

	decision, drop := nc.spamGate(ctx, networkid.PortalKey{ID: "thread-scope-1-1"}, sender, "sms:1:1", "see you at 8", time.Now().UTC(), false)
	if decision.Action != spamActionHold || !drop {
		t.Fatalf("hold decision = %+v, drop %t", decision, drop)
	}
	ghost, err := bridge.GetExistingGhostByID(ctx, MakeUserID(sender))
	if err != nil || ghost == nil {
		t.Fatalf("load ghost: %v, %v", ghost, err)
	}
	meta, _ := ghost.Metadata.(*GhostMetadata)
	if meta == nil || !hasLabel(meta.Labels, SpamUnknownLabel) {
		t.Fatalf("hold must label the sender %q: %+v", SpamUnknownLabel, meta)
	}
}

func TestSpamGateAllowlistLabelBypassesUnknownPolicy(t *testing.T) {
	b, nc, _, _ := newSpamGateTestClient(t, func(cfg *AntiSpamConfig) {
		cfg.UnknownSenderPolicy = AntiSpamPolicyHold
	})
	ctx := context.Background()
	sender := "+15551234567"
	ghostID := MakeUserID(sender)
	b.seedGhost(t, string(ghostID))
	ghost, err := nc.bridge.GetExistingGhostByID(ctx, ghostID)
	if err != nil || ghost == nil {
		t.Fatalf("load ghost: %v, %v", ghost, err)
	}
	meta, _ := ghost.Metadata.(*GhostMetadata)
	meta.Labels = []string{"family"}
	if err := nc.bridge.DB.Ghost.Update(ctx, ghost.Ghost); err != nil {
		t.Fatalf("update ghost labels: %v", err)
	}

	decision, drop := nc.spamGate(ctx, networkid.PortalKey{ID: "thread-scope-1-1"}, sender, "sms:1:1", "see you at 8", time.Now().UTC(), false)
	if decision.Action != spamActionAllow || drop {
		t.Fatalf("allowlisted sender decision = %+v, drop %t", decision, drop)
	}
}

func TestSpamGateDuplicateAcrossSenders(t *testing.T) {
	b, nc, _, _ := newSpamGateTestClient(t, func(cfg *AntiSpamConfig) {
		cfg.Duplicates = AntiSpamDuplicateConfig{Window: TTLDuration(time.Hour), Count: 2}
	})
	ctx := context.Background()
	first, second := "+15551111111", "+15552222222"
	b.seedGhost(t, string(MakeUserID(first)))
	b.seedGhost(t, string(MakeUserID(second)))

	decision, _ := nc.spamGate(ctx, networkid.PortalKey{ID: "thread-scope-1-1"}, first, "sms:1:1", "party at 8pm", time.Now().UTC(), false)
	if decision.Action != spamActionLabel {
		t.Fatalf("first sender decision = %+v, want label", decision)
	}
	decision, _ = nc.spamGate(ctx, networkid.PortalKey{ID: "thread-scope-1-2"}, second, "sms:1:2", "party at 8pm", time.Now().UTC(), false)
	if decision.Action != spamActionFlag || !hasReason(decision, "duplicate") {
		t.Fatalf("second sender decision = %+v, want duplicate flag", decision)
	}
}

func TestSpamGateGroupAndOwnNumberBypassUnknownPolicy(t *testing.T) {
	b, nc, _, connector := newSpamGateTestClient(t, func(cfg *AntiSpamConfig) {
		cfg.UnknownSenderPolicy = AntiSpamPolicyHold
	})
	ctx := context.Background()
	b.seedGhost(t, string(MakeUserID("+15551111111")))

	// A group message from an unseen participant is bridged: holding one
	// member would break the thread for everybody.
	decision, drop := nc.spamGate(ctx, networkid.PortalKey{ID: "thread-scope-1-1"}, "+15551111111", "sms:1:1", "hey", time.Now().UTC(), true)
	if decision.Action != spamActionAllow || drop {
		t.Fatalf("group decision = %+v, drop %t", decision, drop)
	}

	// The phone's own number never enters the gate.
	connector.config.OwnNumbers = []string{"+15550001111"}
	before, _, _ := connector.spamSnapshot()
	decision, drop = nc.spamGate(ctx, networkid.PortalKey{ID: "thread-scope-1-1"}, "+15550001111", "sms:1:2", "note to self", time.Now().UTC(), false)
	after, _, _ := connector.spamSnapshot()
	if decision.Action != spamActionAllow || drop || after.Evaluated != before.Evaluated {
		t.Fatalf("own-number decision = %+v, drop %t, counters %+v vs %+v", decision, drop, after, before)
	}
}

func TestSpamGateDisabledConfigDoesNothing(t *testing.T) {
	b, bridge := newMergeRoutingTestBridge(t)
	connector := bridge.Network.(*MyConnector)
	connector.config.AntiSpam = DefaultAntiSpamConfig() // disabled
	nc := &MyNetworkClient{log: zerolog.Nop(), bridge: bridge, connector: connector}
	b.seedGhost(t, string(MakeUserID("+15551234567")))

	decision, drop := nc.spamGate(context.Background(), networkid.PortalKey{ID: "thread-scope-1-1"}, "+15551234567", "sms:1:1",
		"URGENT wire transfer", time.Now().UTC(), false)
	if decision.Action != spamActionAllow || drop {
		t.Fatalf("disabled gate decision = %+v, drop %t", decision, drop)
	}
	counters, _, _ := connector.spamSnapshot()
	if counters.Evaluated != 0 {
		t.Errorf("disabled gate must not record decisions: %+v", counters)
	}
}

func hasReason(decision spamDecision, reason string) bool {
	for _, candidate := range decision.Reasons {
		if candidate == reason {
			return true
		}
	}
	return false
}

func hasLabel(labels []string, label string) bool {
	for _, candidate := range labels {
		if strings.EqualFold(candidate, label) {
			return true
		}
	}
	return false
}
