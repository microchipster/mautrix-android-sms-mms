// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestSpamStore opens a real SQLite bridge database and returns an
// anti-spam store bound to it.
func newTestSpamStore(t *testing.T) (*testBridgeDB, *spamStore) {
	t.Helper()
	b := newTestBridgeDB(t)
	return b, newSpamStore(b.db.Database, b.db.BridgeID)
}

func TestSpamStoreRatePersistsAcrossRestart(t *testing.T) {
	_, store := newTestSpamStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	const sender = "+15551234567"
	for i, want := range []int{1, 2, 3} {
		count, err := store.recordRate(ctx, sender, base.Add(time.Duration(i)*time.Minute), 10*time.Minute, spamStoreMaxSenders)
		if err != nil {
			t.Fatalf("recordRate(%d) error: %v", i, err)
		}
		if count != want {
			t.Fatalf("recordRate(%d) = %d, want %d", i, count, want)
		}
	}

	// A fresh store over the same database is a restart: the counter continues.
	restarted := newSpamStore(store.db, store.bridgeID)
	count, err := restarted.recordRate(ctx, sender, base.Add(3*time.Minute), 10*time.Minute, spamStoreMaxSenders)
	if err != nil {
		t.Fatalf("restart recordRate error: %v", err)
	}
	if count != 4 {
		t.Errorf("restart count = %d, want 4", count)
	}
	// The window still applies after a restart.
	count, err = restarted.recordRate(ctx, sender, base.Add(21*time.Minute), 10*time.Minute, spamStoreMaxSenders)
	if err != nil {
		t.Fatalf("window recordRate error: %v", err)
	}
	if count != 1 {
		t.Errorf("after window count = %d, want 1", count)
	}
	// Another sender is tracked separately; empty senders are ignored.
	if count, err = restarted.recordRate(ctx, "+15550000000", base, 10*time.Minute, spamStoreMaxSenders); err != nil || count != 1 {
		t.Errorf("other sender count = %d, %v; want 1", count, err)
	}
	if count, err = restarted.recordRate(ctx, "", base, 10*time.Minute, spamStoreMaxSenders); err != nil || count != 0 {
		t.Errorf("empty sender count = %d, %v; want 0", count, err)
	}
	if count, err = restarted.rateCount(ctx, sender, base); err != nil || count == 0 {
		t.Errorf("rateCount = %d, %v; want a non-zero count", count, err)
	}
}

func TestSpamStoreDuplicatePersistsAcrossRestart(t *testing.T) {
	_, store := newTestSpamStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	hash := spamBodyHash("same body")
	if hash == "" {
		t.Fatal("body hash is empty")
	}
	if count, err := store.recordDuplicate(ctx, hash, "+15551111111", base, time.Hour, spamStoreMaxBodies); err != nil || count != 1 {
		t.Fatalf("first sender count = %d, %v; want 1", count, err)
	}
	// Normalization happens before hashing; the same normalized body yields
	// the same hash.
	if spamBodyHash("SAME   body") != hash {
		t.Fatal("body normalization changed the hash")
	}
	if count, err := store.recordDuplicate(ctx, hash, "+15552222222", base.Add(time.Minute), time.Hour, spamStoreMaxBodies); err != nil || count != 2 {
		t.Fatalf("second sender count = %d, %v; want 2", count, err)
	}

	restarted := newSpamStore(store.db, store.bridgeID)
	if count, err := restarted.recordDuplicate(ctx, hash, "+15552222222", base.Add(2*time.Minute), time.Hour, spamStoreMaxBodies); err != nil || count != 2 {
		t.Errorf("repeat sender after restart = %d, %v; want 2", count, err)
	}
	if count, err := restarted.recordDuplicate(ctx, spamBodyHash("other"), "+15553333333", base, time.Hour, spamStoreMaxBodies); err != nil || count != 1 {
		t.Errorf("other body = %d, %v; want 1", count, err)
	}
	if count, err := restarted.recordDuplicate(ctx, "", "+15554444444", base, time.Hour, spamStoreMaxBodies); err != nil || count != 0 {
		t.Errorf("empty hash = %d, %v; want 0", count, err)
	}
	hashes, maxSenders, err := restarted.duplicateState(ctx, "+15552222222", base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("duplicateState error: %v", err)
	}
	if hashes != 1 || maxSenders != 2 {
		t.Errorf("duplicateState = (%d, %d), want (1, 2)", hashes, maxSenders)
	}
	// The window expires the earlier observations.
	if count, err := restarted.recordDuplicate(ctx, hash, "+15559999999", base.Add(2*time.Hour), time.Hour, spamStoreMaxBodies); err != nil || count != 1 {
		t.Errorf("after window = %d, %v; want 1", count, err)
	}
}

func TestSpamStoreDecisionLogRoundTrip(t *testing.T) {
	_, store := newTestSpamStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if err := store.addDecision(ctx, spamRecord{
		Time: base, Sender: "+15551111111", MessageID: "sms:1:1", PortalID: "thread-x",
		Action: spamActionFlag, Score: 12, Reasons: []string{"pattern:urgency", "url:shortener"}, DryRun: true,
	}); err != nil {
		t.Fatalf("addDecision(1) error: %v", err)
	}
	if err := store.addDecision(ctx, spamRecord{
		Time: base.Add(time.Minute), Sender: "+15552222222", MessageID: "sms:1:2",
		Action: spamActionBlock, Score: 20, Reasons: []string{"override:block"},
	}); err != nil {
		t.Fatalf("addDecision(2) error: %v", err)
	}

	// A restarted store still returns the decisions, oldest first.
	restarted := newSpamStore(store.db, store.bridgeID)
	records, err := restarted.recentDecisions(ctx, 10)
	if err != nil {
		t.Fatalf("recentDecisions error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %+v, want 2", records)
	}
	if records[0].Sender != "+15551111111" || records[0].Action != spamActionFlag || !records[0].DryRun ||
		len(records[0].Reasons) != 2 || records[0].Time.UnixNano() != base.UnixNano() {
		t.Errorf("first record = %+v", records[0])
	}
	if records[1].Action != spamActionBlock || records[1].Score != 20 || records[1].PortalID != "" {
		t.Errorf("second record = %+v", records[1])
	}
	forSender, err := restarted.recentDecisionsForSender(ctx, "+15551111111", 5)
	if err != nil || len(forSender) != 1 || forSender[0].MessageID != "sms:1:1" || !forSender[0].DryRun {
		t.Errorf("recentDecisionsForSender = %+v, %v", forSender, err)
	}
}

func TestSpamStoreDecisionLogCap(t *testing.T) {
	_, store := newTestSpamStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for i := 0; i < spamRecentCapacity+5; i++ {
		if err := store.addDecision(ctx, spamRecord{
			Time: base.Add(time.Duration(i) * time.Second), Sender: "+15551234567",
			MessageID: fmt.Sprintf("sms:1:%d", i), Action: spamActionFlag, Score: i,
		}); err != nil {
			t.Fatalf("addDecision(%d) error: %v", i, err)
		}
	}
	records, err := store.recentDecisions(ctx, spamRecentCapacity)
	if err != nil {
		t.Fatalf("recentDecisions error: %v", err)
	}
	if len(records) != spamRecentCapacity {
		t.Fatalf("records = %d, want the %d cap", len(records), spamRecentCapacity)
	}
	if records[len(records)-1].Score != spamRecentCapacity+4 {
		t.Errorf("newest score = %d, want %d", records[len(records)-1].Score, spamRecentCapacity+4)
	}
	if records[0].Score != 5 {
		t.Errorf("oldest kept score = %d, want 5", records[0].Score)
	}
}

func TestSpamStoreContactReputationAndPolicy(t *testing.T) {
	_, store := newTestSpamStore(t)
	ctx := context.Background()
	const sender = "+15551234567"
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	if _, ok, err := store.contact(ctx, sender); err != nil || ok {
		t.Fatalf("contact before any message = ok %t, err %v", ok, err)
	}
	if err := store.recordContactMessage(ctx, sender, base); err != nil {
		t.Fatalf("recordContactMessage error: %v", err)
	}
	if err := store.recordContactMessage(ctx, sender, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("recordContactMessage error: %v", err)
	}
	if err := store.recordContactOutcome(ctx, sender, true, false); err != nil {
		t.Fatalf("recordContactOutcome error: %v", err)
	}
	if err := store.recordContactOutcome(ctx, sender, true, true); err != nil {
		t.Fatalf("recordContactOutcome error: %v", err)
	}
	if err := store.setContactPolicy(ctx, sender, SpamContactPolicyHold); err != nil {
		t.Fatalf("setContactPolicy error: %v", err)
	}

	restarted := newSpamStore(store.db, store.bridgeID)
	stats, ok, err := restarted.contact(ctx, sender)
	if err != nil || !ok {
		t.Fatalf("contact after restart = ok %t, err %v", ok, err)
	}
	if stats.Total != 2 || stats.Flagged != 2 || stats.Blocked != 1 || stats.Policy != SpamContactPolicyHold {
		t.Errorf("stats = %+v", stats)
	}
	if stats.FirstSeen.UnixNano() != base.UnixNano() || stats.LastSeen.UnixNano() != base.Add(2*time.Hour).UnixNano() {
		t.Errorf("first/last seen = %s / %s", stats.FirstSeen, stats.LastSeen)
	}
	overrides, err := restarted.contactPolicies(ctx)
	if err != nil || len(overrides) != 1 || overrides[0].Sender != sender || overrides[0].Policy != SpamContactPolicyHold {
		t.Errorf("overrides = %+v, %v", overrides, err)
	}
	if err := restarted.setContactPolicy(ctx, sender, ""); err != nil {
		t.Fatalf("clear policy error: %v", err)
	}
	if overrides, err = restarted.contactPolicies(ctx); err != nil || len(overrides) != 0 {
		t.Errorf("overrides after clear = %+v, %v", overrides, err)
	}
	if stats, ok, err = restarted.contact(ctx, sender); err != nil || !ok || stats.Policy != "" || stats.Total != 2 {
		t.Errorf("contact after clear = %+v, ok %t, err %v", stats, ok, err)
	}

	// A policy can be set for a sender with no history; the first real
	// message then sets the first-seen time.
	unseen := "+15559998888"
	if err := restarted.setContactPolicy(ctx, unseen, SpamContactPolicyBlock); err != nil {
		t.Fatalf("setContactPolicy(unseen) error: %v", err)
	}
	if err := restarted.recordContactMessage(ctx, unseen, base); err != nil {
		t.Fatalf("recordContactMessage(unseen) error: %v", err)
	}
	stats, ok, err = restarted.contact(ctx, unseen)
	if err != nil || !ok || stats.Policy != SpamContactPolicyBlock || stats.Total != 1 ||
		stats.FirstSeen.UnixNano() != base.UnixNano() {
		t.Errorf("unseen contact = %+v, ok %t, err %v", stats, ok, err)
	}
}

func TestSpamStoreFeedImportRoundTrip(t *testing.T) {
	_, store := newTestSpamStore(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "urlhaus.txt")
	content := strings.Join([]string{
		"# comment",
		"; other comment",
		"",
		"http://evil.example/path/malware.bin",
		"static.example.com",
		"*.wild.example.com",
		"+15551234567",
		"55555",
		"12345,http://csv.example/payload",
		"spam.example.com some trailing words",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write feed file: %v", err)
	}
	result, err := store.importFeedFile(ctx, path)
	if err != nil {
		t.Fatalf("importFeedFile error: %v", err)
	}
	if result.Source != "urlhaus.txt" || result.Added != 7 || result.Total != 7 {
		t.Errorf("import result = %+v, want 7 added from urlhaus.txt", result)
	}
	// Re-importing is idempotent.
	result, err = store.importFeedFile(ctx, path)
	if err != nil || result.Added != 0 || result.Total != 7 {
		t.Errorf("second import = %+v, %v; want 0 added", result, err)
	}

	restarted := newSpamStore(store.db, store.bridgeID)
	entries, err := restarted.feedEntries(ctx)
	if err != nil {
		t.Fatalf("feedEntries error: %v", err)
	}
	for _, want := range []string{"evil.example", "static.example.com", "wild.example.com", "+15551234567", "55555", "csv.example"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("feed entries missing %q: %v", want, entries)
		}
	}
	if signals := spamFeedMatches(entries, "+15551234567", "download http://evil.example/x"); len(signals) != 2 ||
		signals[0] != "sender" || signals[1] != "url" {
		t.Errorf("feed signals = %v, want [sender url]", signals)
	}
	if signals := spamFeedMatches(entries, "+15550000000", "http://a.wild.example.com/x"); len(signals) != 1 || signals[0] != "url" {
		t.Errorf("subdomain feed signals = %v, want [url]", signals)
	}
	if signals := spamFeedMatches(entries, "+15550000000", "hello"); len(signals) != 0 {
		t.Errorf("clean feed signals = %v, want none", signals)
	}
	if _, err := restarted.importFeedFile(ctx, filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Error("importing a missing file must fail")
	}
}

func TestParseSpamFeedLine(t *testing.T) {
	tests := []struct {
		line  string
		want  string
		valid bool
	}{
		{line: "", valid: false},
		{line: "# comment", valid: false},
		{line: "; comment", valid: false},
		{line: "  http://evil.example/path  ", want: "evil.example", valid: true},
		{line: "https://sub.evil.example/x#frag", want: "sub.evil.example", valid: true},
		{line: "www.evil.example", want: "evil.example", valid: true},
		{line: "EVIL.Example.COM", want: "evil.example.com", valid: true},
		{line: "*.evil.example.com", want: "evil.example.com", valid: true},
		{line: "evil.example. # trailing comment", want: "evil.example", valid: true},
		{line: "+1 (555) 123-4567", want: "+15551234567", valid: true},
		{line: "12345,http://csv.example/payload", want: "csv.example", valid: true},
		{line: "55555", want: "55555", valid: true},
	}
	for _, test := range tests {
		got, ok := parseSpamFeedLine(test.line)
		if ok != test.valid || got != test.want {
			t.Errorf("parseSpamFeedLine(%q) = %q, %t; want %q, %t", test.line, got, ok, test.want, test.valid)
		}
	}
}
