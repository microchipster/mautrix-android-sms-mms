// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

const (
	// spamStoreMaxSenders bounds the persisted first-contact limiter state.
	spamStoreMaxSenders = 2048
	// spamStoreMaxBodies bounds the persisted duplicate-tracker state.
	spamStoreMaxBodies = 1024
)

// spamStore persists the anti-spam gate's state in small side tables of the
// bridge database: first-contact rate observations, duplicate body hashes,
// recent decisions, per-contact reputation/policies and imported offline feed
// entries. Everything is pruned by age or capped, so the tables stay small.
type spamStore struct {
	db       *dbutil.Database
	bridgeID networkid.BridgeID

	mu    sync.Mutex
	ready bool
	feeds map[string]string
}

// newSpamStore returns a store bound to the bridge's database.
func newSpamStore(db *dbutil.Database, bridgeID networkid.BridgeID) *spamStore {
	return &spamStore{db: db, bridgeID: bridgeID}
}

// ensure creates the side tables once. Dialect-specific bits are limited to
// the autoincrement column of the decision log.
func (s *spamStore) ensure(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("anti-spam store has no database")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		return nil
	}
	idColumn := "INTEGER PRIMARY KEY AUTOINCREMENT"
	if s.db.Dialect == dbutil.Postgres {
		idColumn = "BIGSERIAL PRIMARY KEY"
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS anti_spam_rate (
			bridge_id TEXT NOT NULL,
			sender TEXT NOT NULL,
			ts BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS anti_spam_rate_lookup ON anti_spam_rate (bridge_id, sender, ts)`,
		`CREATE INDEX IF NOT EXISTS anti_spam_rate_age ON anti_spam_rate (bridge_id, ts)`,
		`CREATE TABLE IF NOT EXISTS anti_spam_duplicate (
			bridge_id TEXT NOT NULL,
			hash TEXT NOT NULL,
			sender TEXT NOT NULL,
			ts BIGINT NOT NULL,
			PRIMARY KEY (bridge_id, hash, sender)
		)`,
		`CREATE INDEX IF NOT EXISTS anti_spam_duplicate_hash ON anti_spam_duplicate (bridge_id, hash, ts)`,
		`CREATE INDEX IF NOT EXISTS anti_spam_duplicate_sender ON anti_spam_duplicate (bridge_id, sender, ts)`,
		`CREATE INDEX IF NOT EXISTS anti_spam_duplicate_age ON anti_spam_duplicate (bridge_id, ts)`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS anti_spam_decision (
			id %s,
			bridge_id TEXT NOT NULL,
			ts BIGINT NOT NULL,
			sender TEXT NOT NULL,
			message_id TEXT NOT NULL,
			portal_id TEXT NOT NULL,
			action TEXT NOT NULL,
			score INTEGER NOT NULL,
			reasons TEXT NOT NULL,
			dry_run INTEGER NOT NULL
		)`, idColumn),
		`CREATE INDEX IF NOT EXISTS anti_spam_decision_sender ON anti_spam_decision (bridge_id, sender, ts)`,
		`CREATE TABLE IF NOT EXISTS anti_spam_contact (
			bridge_id TEXT NOT NULL,
			sender TEXT NOT NULL,
			first_seen BIGINT NOT NULL,
			last_seen BIGINT NOT NULL,
			total INTEGER NOT NULL,
			flagged INTEGER NOT NULL,
			blocked INTEGER NOT NULL,
			policy TEXT NOT NULL,
			PRIMARY KEY (bridge_id, sender)
		)`,
		`CREATE TABLE IF NOT EXISTS anti_spam_feed (
			bridge_id TEXT NOT NULL,
			entry TEXT NOT NULL,
			source TEXT NOT NULL,
			imported_at BIGINT NOT NULL,
			PRIMARY KEY (bridge_id, entry)
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(ctx, statement); err != nil {
			return fmt.Errorf("create anti-spam storage: %w", err)
		}
	}
	s.ready = true
	return nil
}

// recordRate adds one first-contact observation and returns how many messages
// from the sender are inside the window (including this one). Observations
// older than the window and senders outside the cap are pruned.
func (s *spamStore) recordRate(ctx context.Context, sender string, ts time.Time, window time.Duration, maxSenders int) (int, error) {
	if s == nil || sender == "" {
		return 0, nil
	}
	if err := s.ensure(ctx); err != nil {
		return 0, err
	}
	cutoff := ts.Add(-window).UnixNano()
	if _, err := s.db.Exec(ctx, `DELETE FROM anti_spam_rate WHERE bridge_id=$1 AND ts<$2`, s.bridgeID, cutoff); err != nil {
		return 0, err
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO anti_spam_rate (bridge_id, sender, ts) VALUES ($1, $2, $3)`, s.bridgeID, sender, ts.UnixNano()); err != nil {
		return 0, err
	}
	var count int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM anti_spam_rate WHERE bridge_id=$1 AND sender=$2 AND ts>=$3`, s.bridgeID, sender, cutoff).Scan(&count); err != nil {
		return 0, err
	}
	if maxSenders > 0 {
		var senders int
		if err := s.db.QueryRow(ctx, `SELECT COUNT(DISTINCT sender) FROM anti_spam_rate WHERE bridge_id=$1`, s.bridgeID).Scan(&senders); err != nil {
			return count, err
		}
		if senders > maxSenders {
			if _, err := s.db.Exec(ctx, `DELETE FROM anti_spam_rate WHERE bridge_id=$1 AND sender NOT IN (
				SELECT sender FROM (
					SELECT sender, MAX(ts) AS newest FROM anti_spam_rate WHERE bridge_id=$1 GROUP BY sender ORDER BY newest DESC LIMIT $2
				) AS keep
			)`, s.bridgeID, maxSenders); err != nil {
				return count, err
			}
		}
	}
	return count, nil
}

// rateCount returns how many observations the sender has since the cutoff.
func (s *spamStore) rateCount(ctx context.Context, sender string, since time.Time) (int, error) {
	if s == nil || sender == "" {
		return 0, nil
	}
	if err := s.ensure(ctx); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM anti_spam_rate WHERE bridge_id=$1 AND sender=$2 AND ts>=$3`,
		s.bridgeID, sender, since.UnixNano()).Scan(&count)
	return count, err
}

// recordDuplicate adds one body-hash/sender observation and returns how many
// distinct senders sent the same body inside the window (including this one).
// Observations older than the window and bodies outside the cap are pruned.
func (s *spamStore) recordDuplicate(ctx context.Context, hash, sender string, ts time.Time, window time.Duration, maxBodies int) (int, error) {
	if s == nil || hash == "" || sender == "" {
		return 0, nil
	}
	if err := s.ensure(ctx); err != nil {
		return 0, err
	}
	cutoff := ts.Add(-window).UnixNano()
	if _, err := s.db.Exec(ctx, `DELETE FROM anti_spam_duplicate WHERE bridge_id=$1 AND ts<$2`, s.bridgeID, cutoff); err != nil {
		return 0, err
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO anti_spam_duplicate (bridge_id, hash, sender, ts) VALUES ($1, $2, $3, $4)
		ON CONFLICT (bridge_id, hash, sender) DO NOTHING`, s.bridgeID, hash, sender, ts.UnixNano()); err != nil {
		return 0, err
	}
	var count int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM anti_spam_duplicate WHERE bridge_id=$1 AND hash=$2 AND ts>=$3`,
		s.bridgeID, hash, cutoff).Scan(&count); err != nil {
		return 0, err
	}
	if maxBodies > 0 {
		var rows int
		if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM anti_spam_duplicate WHERE bridge_id=$1`, s.bridgeID).Scan(&rows); err != nil {
			return count, err
		}
		if rows > maxBodies {
			if _, err := s.db.Exec(ctx, `DELETE FROM anti_spam_duplicate WHERE bridge_id=$1 AND hash NOT IN (
				SELECT hash FROM (
					SELECT hash, MAX(ts) AS newest FROM anti_spam_duplicate WHERE bridge_id=$1 GROUP BY hash ORDER BY newest DESC LIMIT $2
				) AS keep
			)`, s.bridgeID, maxBodies); err != nil {
				return count, err
			}
		}
	}
	return count, nil
}

// duplicateState summarizes the duplicate tracker for one sender since the
// cutoff: how many distinct body hashes the sender used and the highest
// distinct-sender count among them.
func (s *spamStore) duplicateState(ctx context.Context, sender string, since time.Time) (hashes int, maxSenders int, err error) {
	if s == nil || sender == "" {
		return 0, 0, nil
	}
	if err := s.ensure(ctx); err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRow(ctx, `SELECT COUNT(DISTINCT d.hash), COALESCE(MAX(h.distinct_senders), 0)
		FROM anti_spam_duplicate d
		JOIN (
			SELECT hash, COUNT(DISTINCT sender) AS distinct_senders FROM anti_spam_duplicate
			WHERE bridge_id=$1 AND ts>=$2 GROUP BY hash
		) AS h ON h.hash = d.hash
		WHERE d.bridge_id=$1 AND d.sender=$3 AND d.ts>=$2`, s.bridgeID, since.UnixNano(), sender).Scan(&hashes, &maxSenders)
	return hashes, maxSenders, err
}

// addDecision persists one non-allow decision and prunes the log to the
// recent-decisions cap.
func (s *spamStore) addDecision(ctx context.Context, record spamRecord) error {
	if s == nil {
		return nil
	}
	if err := s.ensure(ctx); err != nil {
		return err
	}
	reasons, err := json.Marshal(record.Reasons)
	if err != nil {
		reasons = []byte("[]")
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO anti_spam_decision
			(bridge_id, ts, sender, message_id, portal_id, action, score, reasons, dry_run)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		s.bridgeID, record.Time.UnixNano(), record.Sender, record.MessageID, record.PortalID,
		record.Action, record.Score, string(reasons), boolInt(record.DryRun)); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `DELETE FROM anti_spam_decision WHERE bridge_id=$1 AND id NOT IN (
		SELECT id FROM (SELECT id FROM anti_spam_decision WHERE bridge_id=$1 ORDER BY id DESC LIMIT $2) AS keep
	)`, s.bridgeID, spamRecentCapacity)
	return err
}

// recentDecisions returns up to limit decisions, oldest first.
func (s *spamStore) recentDecisions(ctx context.Context, limit int) ([]spamRecord, error) {
	if s == nil || limit <= 0 {
		return nil, nil
	}
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT ts, sender, message_id, portal_id, action, score, reasons, dry_run FROM (
			SELECT id, ts, sender, message_id, portal_id, action, score, reasons, dry_run
			FROM anti_spam_decision WHERE bridge_id=$1 ORDER BY id DESC LIMIT $2
		) AS recent ORDER BY id ASC`, s.bridgeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]spamRecord, 0, limit)
	for rows.Next() {
		record, err := scanSpamDecision(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// recentDecisionsForSender returns up to limit decisions for one sender,
// newest first.
func (s *spamStore) recentDecisionsForSender(ctx context.Context, sender string, limit int) ([]spamRecord, error) {
	if s == nil || sender == "" || limit <= 0 {
		return nil, nil
	}
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT ts, message_id, portal_id, action, score, reasons, dry_run
		FROM anti_spam_decision WHERE bridge_id=$1 AND sender=$2 ORDER BY id DESC LIMIT $3`, s.bridgeID, sender, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]spamRecord, 0, limit)
	for rows.Next() {
		var (
			ts        int64
			messageID string
			portalID  string
			action    string
			score     int
			reasons   string
			dryRun    int
		)
		if err := rows.Scan(&ts, &messageID, &portalID, &action, &score, &reasons, &dryRun); err != nil {
			return nil, err
		}
		record := spamRecord{
			Time:      time.Unix(0, ts).UTC(),
			Sender:    sender,
			MessageID: messageID,
			PortalID:  portalID,
			Action:    action,
			Score:     score,
			DryRun:    dryRun != 0,
		}
		_ = json.Unmarshal([]byte(reasons), &record.Reasons)
		records = append(records, record)
	}
	return records, rows.Err()
}

// scanSpamDecision scans one full decision row (with sender).
func scanSpamDecision(row interface{ Scan(dest ...any) error }) (spamRecord, error) {
	var (
		ts        int64
		sender    string
		messageID string
		portalID  string
		action    string
		score     int
		reasons   string
		dryRun    int
	)
	if err := row.Scan(&ts, &sender, &messageID, &portalID, &action, &score, &reasons, &dryRun); err != nil {
		return spamRecord{}, err
	}
	record := spamRecord{
		Time:      time.Unix(0, ts).UTC(),
		Sender:    sender,
		MessageID: messageID,
		PortalID:  portalID,
		Action:    action,
		Score:     score,
		DryRun:    dryRun != 0,
	}
	_ = json.Unmarshal([]byte(reasons), &record.Reasons)
	return record, nil
}

// contact returns one sender's reputation counters and policy override.
func (s *spamStore) contact(ctx context.Context, sender string) (spamContactStats, bool, error) {
	if s == nil || sender == "" {
		return spamContactStats{}, false, nil
	}
	if err := s.ensure(ctx); err != nil {
		return spamContactStats{}, false, err
	}
	var (
		firstSeen, lastSeen int64
		stats               spamContactStats
	)
	err := s.db.QueryRow(ctx, `SELECT first_seen, last_seen, total, flagged, blocked, policy
		FROM anti_spam_contact WHERE bridge_id=$1 AND sender=$2`, s.bridgeID, sender).
		Scan(&firstSeen, &lastSeen, &stats.Total, &stats.Flagged, &stats.Blocked, &stats.Policy)
	if errors.Is(err, sql.ErrNoRows) {
		return spamContactStats{}, false, nil
	}
	if err != nil {
		return spamContactStats{}, false, err
	}
	stats.Sender = sender
	if firstSeen > 0 {
		stats.FirstSeen = time.Unix(0, firstSeen).UTC()
	}
	if lastSeen > 0 {
		stats.LastSeen = time.Unix(0, lastSeen).UTC()
	}
	return stats, true, nil
}

// recordContactMessage updates the sender's own-history counters.
func (s *spamStore) recordContactMessage(ctx context.Context, sender string, ts time.Time) error {
	if s == nil || sender == "" {
		return nil
	}
	if err := s.ensure(ctx); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO anti_spam_contact
			(bridge_id, sender, first_seen, last_seen, total, flagged, blocked, policy)
		VALUES ($1, $2, $3, $3, 1, 0, 0, '')
		ON CONFLICT (bridge_id, sender) DO UPDATE SET
			first_seen = CASE WHEN anti_spam_contact.first_seen = 0 OR anti_spam_contact.first_seen > $3
				THEN $3 ELSE anti_spam_contact.first_seen END,
			last_seen = CASE WHEN anti_spam_contact.last_seen < $3 THEN $3 ELSE anti_spam_contact.last_seen END,
			total = anti_spam_contact.total + 1`,
		s.bridgeID, sender, ts.UnixNano())
	return err
}

// recordContactOutcome bumps the flagged/blocked counters of a sender.
func (s *spamStore) recordContactOutcome(ctx context.Context, sender string, flagged, blocked bool) error {
	if s == nil || sender == "" {
		return nil
	}
	if err := s.ensure(ctx); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE anti_spam_contact SET flagged = flagged + $3, blocked = blocked + $4
		WHERE bridge_id=$1 AND sender=$2`, s.bridgeID, sender, boolInt(flagged), boolInt(blocked))
	return err
}

// setContactPolicy stores (or with an empty policy clears) one per-contact
// override.
func (s *spamStore) setContactPolicy(ctx context.Context, sender, policy string) error {
	if s == nil || sender == "" {
		return nil
	}
	if err := s.ensure(ctx); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO anti_spam_contact
			(bridge_id, sender, first_seen, last_seen, total, flagged, blocked, policy)
		VALUES ($1, $2, 0, 0, 0, 0, 0, $3)
		ON CONFLICT (bridge_id, sender) DO UPDATE SET policy=$3`, s.bridgeID, sender, policy)
	return err
}

// spamContactPolicyEntry is one stored per-contact override.
type spamContactPolicyEntry struct {
	Sender string
	Policy string
}

// contactPolicies lists the stored per-contact overrides, sorted by sender.
func (s *spamStore) contactPolicies(ctx context.Context) ([]spamContactPolicyEntry, error) {
	if s == nil {
		return nil, nil
	}
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT sender, policy FROM anti_spam_contact
		WHERE bridge_id=$1 AND policy != '' ORDER BY sender`, s.bridgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	overrides := make([]spamContactPolicyEntry, 0)
	for rows.Next() {
		var entry spamContactPolicyEntry
		if err := rows.Scan(&entry.Sender, &entry.Policy); err != nil {
			return nil, err
		}
		overrides = append(overrides, entry)
	}
	return overrides, rows.Err()
}

// spamFeedImportResult reports one `spam import` run.
type spamFeedImportResult struct {
	Source string
	Added  int
	Total  int
}

// importFeedFile reads a local line-based feed file and stores its entries.
// The bridge never fetches feeds: importing is an explicit local operation
// (or the startup import of anti_spam.feeds.paths).
func (s *spamStore) importFeedFile(ctx context.Context, path string) (spamFeedImportResult, error) {
	result := spamFeedImportResult{}
	if s == nil {
		return result, errors.New("anti-spam store is not available")
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return result, errors.New("feed file path cannot be empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	if err := s.ensure(ctx); err != nil {
		return result, err
	}
	result.Source = filepath.Base(path)
	entries := make(map[string]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		entry, ok := parseSpamFeedLine(line)
		if !ok {
			continue
		}
		entries[entry] = struct{}{}
	}
	now := time.Now().UTC().UnixNano()
	for entry := range entries {
		res, err := s.db.Exec(ctx, `INSERT INTO anti_spam_feed (bridge_id, entry, source, imported_at)
			VALUES ($1, $2, $3, $4) ON CONFLICT (bridge_id, entry) DO NOTHING`,
			s.bridgeID, entry, result.Source, now)
		if err != nil {
			return result, err
		}
		if affected, err := res.RowsAffected(); err == nil && affected > 0 {
			result.Added++
		}
	}
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM anti_spam_feed WHERE bridge_id=$1`, s.bridgeID).Scan(&result.Total); err != nil {
		return result, err
	}
	s.mu.Lock()
	s.feeds = nil // invalidate the in-memory cache
	s.mu.Unlock()
	return result, nil
}

// feedEntries returns the imported feed entries (entry -> source file). The
// returned map is cached and must not be mutated by the caller.
func (s *spamStore) feedEntries(ctx context.Context) (map[string]string, error) {
	if s == nil {
		return nil, nil
	}
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.feeds != nil {
		entries := s.feeds
		s.mu.Unlock()
		return entries, nil
	}
	s.mu.Unlock()
	rows, err := s.db.Query(ctx, `SELECT entry, source FROM anti_spam_feed WHERE bridge_id=$1`, s.bridgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make(map[string]string)
	for rows.Next() {
		var entry, source string
		if err := rows.Scan(&entry, &source); err != nil {
			return nil, err
		}
		entries[entry] = source
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.feeds == nil {
		s.feeds = entries
	}
	entries = s.feeds
	s.mu.Unlock()
	return entries, nil
}

// boolInt renders a bool as the 0/1 integer used in the side tables.
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
