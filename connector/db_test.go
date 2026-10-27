// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

const testBridgeID = networkid.BridgeID("android-sms-mms")

// testBridgeDB is a real SQLite bridge database with the bridgev2 schema,
// used to exercise the raw SQL behind stats and the TTL cleanup.
type testBridgeDB struct {
	db     *database.Database
	bridge *bridgev2.Bridge
}

func newTestBridgeDB(t *testing.T) *testBridgeDB {
	t.Helper()
	raw, err := dbutil.NewWithDialect("file:"+filepath.Join(t.TempDir(), "bridge.db"), "sqlite3")
	if err != nil {
		t.Fatalf("open sqlite database: %v", err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close sqlite database: %v", err)
		}
	})
	connector := NewMyConnector(zerolog.Nop())
	db := database.New(testBridgeID, connector.GetDBMetaTypes(), raw)
	if err := raw.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade bridge schema: %v", err)
	}
	return &testBridgeDB{
		db:     db,
		bridge: &bridgev2.Bridge{ID: testBridgeID, DB: db, Log: zerolog.Nop()},
	}
}

func (b *testBridgeDB) seedGhost(t *testing.T, ghostID string) {
	t.Helper()
	if err := b.db.Ghost.Insert(context.Background(), &database.Ghost{
		ID:       networkid.UserID(ghostID),
		Metadata: &GhostMetadata{},
	}); err != nil {
		t.Fatalf("insert ghost %q: %v", ghostID, err)
	}
}

func (b *testBridgeDB) seedPortal(t *testing.T, portalID networkid.PortalID, roomID id.RoomID, meta *PortalMetadata) *database.Portal {
	t.Helper()
	portal := &database.Portal{
		ID:       portalID,
		MXID:     roomID,
		RoomType: database.RoomTypeDM,
		Metadata: meta,
	}
	if err := b.db.Portal.Insert(context.Background(), portal); err != nil {
		t.Fatalf("insert portal %q: %v", portalID, err)
	}
	return portal
}

func (b *testBridgeDB) seedUserPortal(t *testing.T, userMXID id.UserID, loginID networkid.UserLoginID, portalID networkid.PortalID) {
	t.Helper()
	err := b.db.UserPortal.Put(context.Background(), &database.UserPortal{
		UserMXID: userMXID,
		LoginID:  loginID,
		Portal:   networkid.PortalKey{ID: portalID},
	})
	if err != nil {
		t.Fatalf("insert user portal %q: %v", portalID, err)
	}
}

func (b *testBridgeDB) seedMessage(t *testing.T, portalID networkid.PortalID, messageID, partID string, sender networkid.UserID, ts time.Time, meta *MessageMetadata) {
	t.Helper()
	mxid := id.EventID("$" + strings.ReplaceAll(messageID, ":", "-") + "-part-" + partID)
	err := b.db.Message.Insert(context.Background(), &database.Message{
		ID:        networkid.MessageID(messageID),
		PartID:    networkid.PartID(partID),
		MXID:      mxid,
		Room:      networkid.PortalKey{ID: portalID},
		SenderID:  sender,
		Timestamp: ts,
		Metadata:  meta,
	})
	if err != nil {
		t.Fatalf("insert message %q: %v", messageID, err)
	}
}

func TestCollectLoginStatsAgainstDatabase(t *testing.T) {
	b := newTestBridgeDB(t)
	now := time.Now().UTC()
	incoming := MakeUserID("+15551112222")
	ownLogin := networkid.UserID("app:test-login")
	b.seedGhost(t, string(incoming))
	b.seedGhost(t, string(ownLogin))
	b.seedPortal(t, "thread-scope-1-1", "!room1:example.com", &PortalMetadata{ThreadID: 1, SubID: 1, Recipients: []string{"+15551112222"}})
	b.seedPortal(t, "thread-scope-1-2", "!room2:example.com", &PortalMetadata{ThreadID: 2, SubID: 2, Recipients: []string{"+15551113333"}})
	b.seedUserPortal(t, "@alice:example.com", "app:test-login", "thread-scope-1-1")
	// A second login's portal must never leak into the first login's stats.
	b.seedUserPortal(t, "@bob:example.com", "app:other-login", "thread-scope-1-2")

	// Multi-part MMS: one message, two rows.
	b.seedMessage(t, "thread-scope-1-1", "mms:1:100", "0", incoming, now.Add(-1*time.Hour), &MessageMetadata{Body: "photo", HasAttachment: true})
	b.seedMessage(t, "thread-scope-1-1", "mms:1:100", "1", incoming, now.Add(-1*time.Hour), &MessageMetadata{HasAttachment: true})
	b.seedMessage(t, "thread-scope-1-1", "sms:1:101", "0", ownLogin, now.Add(-2*time.Hour), &MessageMetadata{Body: "hello"})
	b.seedMessage(t, "thread-scope-1-1", "sms:1:102", "0", incoming, now.Add(-30*time.Hour), &MessageMetadata{Body: "too old"})
	b.seedMessage(t, "thread-scope-1-2", "sms:2:200", "0", incoming, now.Add(-1*time.Hour), &MessageMetadata{Body: "other login"})

	login := &bridgev2.UserLogin{UserLogin: &database.UserLogin{
		ID:       "app:test-login",
		UserMXID: "@alice:example.com",
		Metadata: &LoginMetadata{Subscriptions: map[int]SubscriptionInfo{
			1: {SubID: 1, Carrier: "ExampleMobile", PhoneNumber: "+15550100100"},
			2: {SubID: 2, Carrier: "SecondCarrier"},
		}},
	}}
	user := &bridgev2.User{User: &database.User{MXID: "@alice:example.com"}}

	messages, subscriptions, err := collectLoginStats(context.Background(), b.bridge, login, user, statsWindow{Label: "24h", Duration: 24 * time.Hour})
	if err != nil {
		t.Fatalf("collectLoginStats error: %v", err)
	}
	report := aggregateStats(messages, subscriptions)
	if report.Counts.Total != 2 || report.Counts.In != 1 || report.Counts.Out != 1 {
		t.Errorf("total/in/out = %d/%d/%d, want 2/1/1", report.Counts.Total, report.Counts.In, report.Counts.Out)
	}
	if report.Counts.SMS != 1 || report.Counts.MMS != 1 {
		t.Errorf("sms/mms = %d/%d, want 1/1", report.Counts.SMS, report.Counts.MMS)
	}
	if len(report.Carriers) != 2 {
		t.Fatalf("carriers = %+v, want sub 1 and sub 2", report.Carriers)
	}
	if report.Carriers[0].SubID != 1 || report.Carriers[0].Carrier != "ExampleMobile" || report.Carriers[0].Counts.Total != 2 {
		t.Errorf("sub 1 = %+v", report.Carriers[0])
	}
	if report.Carriers[1].SubID != 2 || report.Carriers[1].Counts.Total != 0 {
		t.Errorf("sub 2 = %+v", report.Carriers[1])
	}
}

func TestCollectTTLCandidatesAgainstDatabase(t *testing.T) {
	b := newTestBridgeDB(t)
	now := time.Now().UTC()
	incoming := MakeUserID("+15551112222")
	b.seedGhost(t, string(incoming))
	dbPortal := b.seedPortal(t, "thread-scope-1-1", "!room1:example.com", &PortalMetadata{ThreadID: 1, SubID: 1, Recipients: []string{"+15551112222"}})
	portal := &bridgev2.Portal{Portal: dbPortal}
	contact := ttlContactForPortal(portal)

	b.seedMessage(t, "thread-scope-1-1", "sms:1:300", "0", incoming, now.Add(-40*24*time.Hour), &MessageMetadata{Body: "old text"})
	b.seedMessage(t, "thread-scope-1-1", "mms:1:301", "0", incoming, now.Add(-40*24*time.Hour), &MessageMetadata{Body: "old media", HasAttachment: true})
	b.seedMessage(t, "thread-scope-1-1", "sms:1:302", "0", incoming, now.Add(-1*time.Hour), &MessageMetadata{Body: "Your verification code is 123456"})

	cfg := DefaultHistoryTTLConfig()
	cfg.Enabled = true
	cfg.Global = TTLDuration(30 * 24 * time.Hour)
	cfg.Patterns = []string{"otp"}
	cfg.DeleteMedia = false
	cfg.ScanLimit = 100
	ttl, hasTTL := resolveTTLFor(ttlPolicy{Global: 30 * 24 * time.Hour}, portal.MXID, contact)
	connector := &MyConnector{bridge: b.bridge, log: zerolog.Nop(), config: &SimpleConfig{HistoryTTL: cfg}}

	candidates, err := connector.collectTTLCandidates(context.Background(), portal, cfg, contact, ttl, hasTTL, true, now)
	if err != nil {
		t.Fatalf("collectTTLCandidates error: %v", err)
	}
	// Old media is excluded by the no-media SQL filter, the recent OTP message
	// comes from the pattern scan window.
	if len(candidates) != 2 {
		t.Fatalf("candidates = %+v, want old text + recent OTP", candidates)
	}
	policy := ttlPolicy{
		Global:      30 * 24 * time.Hour,
		DeleteMedia: false,
		Patterns:    mustCompileTTLPatterns(t, []string{"otp"}, nil),
	}
	decisions := planTTLRedactions(policy, candidates, now)
	reasons := make(map[int64]string, len(decisions))
	for _, decision := range decisions {
		reasons[decision.Candidate.RowID] = decision.Reason
	}
	if len(decisions) != 2 {
		t.Fatalf("decisions = %+v, want otp + ttl", decisions)
	}
	for rowID, reason := range reasons {
		if reason != "ttl" && reason != "pattern:otp" {
			t.Errorf("row %d reason = %q", rowID, reason)
		}
	}

	// Deleting a redacted message removes its bridge row.
	var oldRowID int64
	for _, decision := range decisions {
		if decision.Reason == "ttl" {
			oldRowID = decision.Candidate.RowID
		}
	}
	if oldRowID == 0 {
		t.Fatal("no ttl decision found")
	}
	if err := connector.redactTTLMessage(context.Background(), portal, ttlDecision{Candidate: ttlCandidate{RowID: oldRowID}, Reason: "ttl"}); err != nil {
		t.Fatalf("redactTTLMessage error: %v", err)
	}
	if msg, err := b.db.Message.GetByRowID(context.Background(), oldRowID); err != nil || msg != nil {
		t.Errorf("deleted row still present: %v, %v", msg, err)
	}

	// With delete_media enabled the old MMS is also a candidate.
	cfg.DeleteMedia = true
	candidates, err = connector.collectTTLCandidates(context.Background(), portal, cfg, contact, ttl, hasTTL, true, now)
	if err != nil {
		t.Fatalf("collectTTLCandidates (with media) error: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates with media = %+v, want old media + recent OTP (old text was deleted)", candidates)
	}

	// scan_limit bounds how many rows one pass inspects. The oldest row was
	// deleted above and old media is filtered out, so the only candidate left
	// is the newest pattern-window row.
	cfg.DeleteMedia = false
	cfg.ScanLimit = 1
	candidates, err = connector.collectTTLCandidates(context.Background(), portal, cfg, contact, ttl, hasTTL, true, now)
	if err != nil {
		t.Fatalf("collectTTLCandidates (limited) error: %v", err)
	}
	if len(candidates) != 1 || !strings.Contains(candidates[0].Body, "verification code") {
		t.Fatalf("limited candidates = %+v, want only the newest pattern-window row", candidates)
	}
}
