// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	// defaultHistoryTTLInterval is how often the retention cleanup runs.
	defaultHistoryTTLInterval = 6 * time.Hour
	// defaultHistoryTTLScanWindow bounds the recent-message scan used for
	// pattern matching (messages older than this and older than the retention
	// period are handled by the retention pass).
	defaultHistoryTTLScanWindow = 30 * 24 * time.Hour
	// defaultHistoryTTLScanLimit caps how many messages one portal contributes
	// to a single cleanup run.
	defaultHistoryTTLScanLimit = 2000
	// ttlInitialDelay delays the first cleanup after startup so a restart storm
	// does not compete with backfill and event processing.
	ttlInitialDelay = 5 * time.Minute
	// ttlCommandTimeout bounds a manually triggered cleanup.
	ttlCommandTimeout = 2 * time.Minute
)

// ttlRedactPresets is the named pattern set. Patterns match message bodies of
// bridged messages and redact them regardless of the configured retention.
// Keep them conservative: false positives delete user history.
var ttlRedactPresets = map[string]string{
	// One-time / verification codes: a 4-8 digit number near a code keyword.
	"otp": `(?i)(?:\b(?:otp|one[- ]?time|verification|security|auth(?:entication)?|login|confirm(?:ation)?|passcode)\b[^\n\d]{0,32}\d{4,8}\b|\b\d{4,8}\b[^\n\d]{0,32}\b(?:otp|code|pin|passcode)\b)`,
	// 13-16 digit card-like numbers, optionally separated by spaces/dashes.
	"credit-card": `\b(?:\d{4}[ -]?){3}\d{1,4}\b`,
	// US Social Security numbers.
	"ssn": `\b\d{3}-\d{2}-\d{4}\b`,
}

// ttlRedactPattern is a compiled redaction pattern.
type ttlRedactPattern struct {
	Name string
	Re   *regexp.Regexp
}

// ttlPresetNames returns the available preset names, sorted for stable output.
func ttlPresetNames() []string {
	names := make([]string, 0, len(ttlRedactPresets))
	for name := range ttlRedactPresets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateTTLPatterns rejects unknown preset names and invalid custom regexes
// at config parse time.
func validateTTLPatterns(names []string, custom map[string]string) error {
	for _, name := range names {
		if _, ok := ttlRedactPresets[name]; !ok {
			return fmt.Errorf("unknown history_ttl pattern %q (available presets: %s)", name, strings.Join(ttlPresetNames(), ", "))
		}
	}
	for name, expr := range custom {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("history_ttl.custom_patterns contains an empty name")
		}
		if _, err := regexp.Compile(expr); err != nil {
			return fmt.Errorf("invalid history_ttl.custom_patterns[%q]: %w", name, err)
		}
	}
	return nil
}

// compileTTLPatterns turns the configured preset names and custom extension
// map into a deterministic, compiled pattern list.
func compileTTLPatterns(names []string, custom map[string]string) ([]ttlRedactPattern, error) {
	if len(names) == 0 && len(custom) == 0 {
		return nil, nil
	}
	if err := validateTTLPatterns(names, custom); err != nil {
		return nil, err
	}
	patterns := make([]ttlRedactPattern, 0, len(names)+len(custom))
	for _, name := range names {
		expr := ttlRedactPresets[name]
		compiled, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("compile preset %q: %w", name, err)
		}
		patterns = append(patterns, ttlRedactPattern{Name: name, Re: compiled})
	}
	customNames := make([]string, 0, len(custom))
	for name := range custom {
		customNames = append(customNames, name)
	}
	sort.Strings(customNames)
	for _, name := range customNames {
		compiled, err := regexp.Compile(custom[name])
		if err != nil {
			return nil, fmt.Errorf("compile custom pattern %q: %w", name, err)
		}
		patterns = append(patterns, ttlRedactPattern{Name: name, Re: compiled})
	}
	return patterns, nil
}

// ttlPolicy is the compiled form of the retention configuration.
type ttlPolicy struct {
	Global      time.Duration
	PerRoom     map[string]time.Duration
	PerContact  map[string]time.Duration
	DeleteMedia bool
	Patterns    []ttlRedactPattern
}

// ttlCandidate is one bridged message considered by the planner.
type ttlCandidate struct {
	RowID      int64
	MXID       id.EventID
	MatrixRoom id.RoomID
	Timestamp  time.Time
	Contact    string
	Body       string
	HasMedia   bool
}

// ttlDecision is a message the planner wants redacted.
type ttlDecision struct {
	Candidate ttlCandidate
	Reason    string
}

// resolveTTLFor applies the override precedence contact > room > global.
// ok is false when no retention is configured for the portal (nothing to do);
// ok with a zero duration means "keep forever" (explicit exemption).
func resolveTTLFor(policy ttlPolicy, roomID id.RoomID, contact string) (time.Duration, bool) {
	if contact != "" {
		if ttl, ok := policy.PerContact[contact]; ok {
			return ttl, true
		}
	}
	if roomID != "" {
		if ttl, ok := policy.PerRoom[string(roomID)]; ok {
			return ttl, true
		}
	}
	if policy.Global > 0 {
		return policy.Global, true
	}
	return 0, false
}

// matchTTLPattern returns the name of the first matching redaction pattern, or
// an empty string.
func matchTTLPattern(patterns []ttlRedactPattern, body string) string {
	if body == "" {
		return ""
	}
	for _, pattern := range patterns {
		if pattern.Re.MatchString(body) {
			return pattern.Name
		}
	}
	return ""
}

// planTTLRedactions decides which of the candidate messages must be redacted.
// Messages matching a redaction pattern are always redacted; other messages
// are redacted once they are older than the retention period resolved for
// their room/contact. Messages carrying media are only subject to the time
// based retention when delete_media is enabled.
func planTTLRedactions(policy ttlPolicy, candidates []ttlCandidate, now time.Time) []ttlDecision {
	decisions := make([]ttlDecision, 0)
	for _, candidate := range candidates {
		if name := matchTTLPattern(policy.Patterns, candidate.Body); name != "" {
			decisions = append(decisions, ttlDecision{Candidate: candidate, Reason: "pattern:" + name})
			continue
		}
		ttl, ok := resolveTTLFor(policy, candidate.MatrixRoom, candidate.Contact)
		if !ok || ttl <= 0 {
			continue
		}
		if candidate.HasMedia && !policy.DeleteMedia {
			continue
		}
		if now.Sub(candidate.Timestamp) > ttl {
			decisions = append(decisions, ttlDecision{Candidate: candidate, Reason: "ttl"})
		}
	}
	return decisions
}

// ttlRunResult summarizes one cleanup run.
type ttlRunResult struct {
	Portals  int
	Messages int
	Redacted int
	Errors   int
	Skipped  bool
}

// ttlState tracks the last cleanup run for the `ttl status` command.
type ttlState struct {
	mu         sync.Mutex
	running    bool
	lastRun    time.Time
	lastDryRun bool
	lastResult ttlRunResult
	lastError  string
}

func (c *MyConnector) ttlSnapshot() (time.Time, bool, ttlRunResult, string, bool) {
	c.ttlMu.Lock()
	defer c.ttlMu.Unlock()
	return c.ttlState.lastRun, c.ttlState.lastDryRun, c.ttlState.lastResult, c.ttlState.lastError, c.ttlState.running
}

// executeHistoryTTL runs the cleanup (at most once at a time) and records the
// outcome for `ttl status`.
func (c *MyConnector) executeHistoryTTL(ctx context.Context, dryRun bool) ttlRunResult {
	c.ttlMu.Lock()
	if c.ttlState.running {
		c.ttlMu.Unlock()
		return ttlRunResult{Skipped: true}
	}
	c.ttlState.running = true
	c.ttlState.lastError = ""
	c.ttlMu.Unlock()

	result := c.runHistoryTTL(ctx, dryRun)

	c.ttlMu.Lock()
	c.ttlState.running = false
	c.ttlState.lastRun = time.Now().UTC()
	c.ttlState.lastDryRun = dryRun
	c.ttlState.lastResult = result
	c.ttlMu.Unlock()
	return result
}

// runHistoryTTL performs one cleanup pass over every Matrix room in the bridge
// database. Callers must serialize runs (executeHistoryTTL does).
func (c *MyConnector) runHistoryTTL(ctx context.Context, dryRun bool) ttlRunResult {
	var result ttlRunResult
	if c == nil || c.bridge == nil || c.bridge.DB == nil {
		result.Errors++
		return result
	}
	cfg := c.historyTTLConfig()
	patterns, err := compileTTLPatterns(cfg.Patterns, cfg.CustomPatterns)
	if err != nil {
		c.log.Error().Err(err).Msg("History TTL has invalid patterns; skipping cleanup")
		c.recordTTLError(err)
		result.Errors++
		return result
	}
	policy := ttlPolicy{
		Global:      cfg.Global.Duration(),
		PerRoom:     ttlDurations(cfg.PerRoom),
		PerContact:  ttlDurations(cfg.PerContact),
		DeleteMedia: cfg.DeleteMedia,
		Patterns:    patterns,
	}
	portals, err := c.bridge.GetAllPortals(ctx)
	if err != nil {
		c.log.Error().Err(err).Msg("Failed to list portals for history TTL cleanup")
		c.recordTTLError(err)
		result.Errors++
		return result
	}
	now := time.Now().UTC()
	for _, portal := range portals {
		if portal == nil || portal.MXID == "" {
			continue
		}
		contact := ttlContactForPortal(portal)
		ttl, hasTTL := resolveTTLFor(policy, portal.MXID, contact)
		if !hasTTL && len(patterns) == 0 {
			continue
		}
		candidates, err := c.collectTTLCandidates(ctx, portal, cfg, contact, ttl, hasTTL, len(patterns) > 0, now)
		if err != nil {
			c.log.Warn().Err(err).Str("portal_id", string(portal.ID)).Msg("Failed to scan portal for history TTL cleanup")
			result.Errors++
			continue
		}
		result.Portals++
		result.Messages += len(candidates)
		decisions := planTTLRedactions(policy, candidates, now)
		for _, decision := range decisions {
			if dryRun {
				result.Redacted++
				continue
			}
			if err := c.redactTTLMessage(ctx, portal, decision); err != nil {
				c.log.Warn().Err(err).
					Str("portal_id", string(portal.ID)).
					Int64("message_row_id", decision.Candidate.RowID).
					Msg("Failed to redact message for history TTL cleanup")
				result.Errors++
				continue
			}
			result.Redacted++
		}
	}
	c.log.Info().
		Bool("dry_run", dryRun).
		Int("portals", result.Portals).
		Int("messages", result.Messages).
		Int("redacted", result.Redacted).
		Int("errors", result.Errors).
		Msg("History TTL cleanup finished")
	return result
}

// collectTTLCandidates gathers the messages one portal contributes to a run:
// the oldest retention-eligible messages (bounded, oldest first, so deleted
// rows make progress) plus the newest messages inside the pattern scan window.
func (c *MyConnector) collectTTLCandidates(ctx context.Context, portal *bridgev2.Portal, cfg HistoryTTLConfig, contact string, ttl time.Duration, hasTTL, hasPatterns bool, now time.Time) ([]ttlCandidate, error) {
	limit := cfg.ScanLimit
	if limit <= 0 {
		limit = defaultHistoryTTLScanLimit
	}
	candidates := make([]ttlCandidate, 0, limit*2)
	seen := make(map[int64]struct{}, limit*2)
	appendMessages := func(messages []*database.Message) {
		for _, msg := range messages {
			if msg == nil {
				continue
			}
			if _, ok := seen[msg.RowID]; ok {
				continue
			}
			seen[msg.RowID] = struct{}{}
			candidates = append(candidates, ttlCandidateFromMessage(msg, contact, portal.MXID))
		}
	}

	if hasTTL && ttl > 0 {
		query := ttlOldMessagesQuery
		if !cfg.DeleteMedia {
			query = ttlOldMessagesQueryBase + ttlOldMessagesNoMediaFilter + ttlOldMessagesOrder
		}
		messages, err := c.bridge.DB.Message.QueryMany(ctx, query,
			c.bridge.DB.BridgeID, portal.ID, portal.Receiver, now.Add(-ttl).UnixNano(), limit)
		if err != nil {
			return nil, fmt.Errorf("query retention-eligible messages: %w", err)
		}
		appendMessages(messages)
	}

	if hasPatterns {
		window := cfg.ScanWindow.Duration()
		if window <= 0 {
			window = defaultHistoryTTLScanWindow
		}
		messages, err := c.bridge.DB.Message.QueryMany(ctx, ttlRecentMessagesQuery,
			c.bridge.DB.BridgeID, portal.ID, portal.Receiver, now.Add(-window).UnixNano(), limit)
		if err != nil {
			return nil, fmt.Errorf("query pattern scan window: %w", err)
		}
		appendMessages(messages)
	}
	return candidates, nil
}

// redactTTLMessage redacts the Matrix event (when one exists) and removes the
// bridge database row. The row is kept when the redaction fails, so the next
// run retries it.
func (c *MyConnector) redactTTLMessage(ctx context.Context, portal *bridgev2.Portal, decision ttlDecision) error {
	candidate := decision.Candidate
	if candidate.MXID != "" &&
		!strings.HasPrefix(string(candidate.MXID), database.FakeMXIDPrefix) &&
		c.bridge.Bot != nil && portal.MXID != "" {
		_, err := c.bridge.Bot.SendMessage(ctx, portal.MXID, event.EventRedaction, &event.Content{
			Parsed: &event.RedactionEventContent{
				Redacts: candidate.MXID,
				Reason:  "History retention (" + decision.Reason + ")",
			},
		}, nil)
		if err != nil {
			return fmt.Errorf("redact event %s: %w", candidate.MXID, err)
		}
	}
	if err := c.bridge.DB.Message.Delete(ctx, candidate.RowID); err != nil {
		return fmt.Errorf("delete message row %d: %w", candidate.RowID, err)
	}
	return nil
}

// ttlCandidateFromMessage converts a bridgev2 message row into the planner
// input, dropping the body when there is nothing to match against.
func ttlCandidateFromMessage(msg *database.Message, contact string, roomID id.RoomID) ttlCandidate {
	candidate := ttlCandidate{
		RowID:      msg.RowID,
		MXID:       msg.MXID,
		MatrixRoom: roomID,
		Timestamp:  msg.Timestamp.UTC(),
		Contact:    contact,
	}
	if meta, ok := msg.Metadata.(*MessageMetadata); ok && meta != nil {
		candidate.Body = meta.Body
		candidate.HasMedia = meta.HasAttachment
	}
	return candidate
}

// ttlContactForPortal resolves the per-contact override key for a portal. Only
// DMs have a single contact; group rooms use their room override or global.
func ttlContactForPortal(portal *bridgev2.Portal) string {
	if portal == nil {
		return ""
	}
	meta, _ := portal.Metadata.(*PortalMetadata)
	if meta == nil {
		return ""
	}
	if len(meta.Recipients) == 1 && !meta.IsGroup {
		return CanonicalAddress(meta.Recipients[0])
	}
	if meta.OtherUserID != "" && !meta.IsGroup {
		return CanonicalAddress(UnescapeAddress(string(meta.OtherUserID)))
	}
	return ""
}

func ttlDurations(input map[string]TTLDuration) map[string]time.Duration {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]time.Duration, len(input))
	for key, ttl := range input {
		out[key] = ttl.Duration()
	}
	return out
}

func (c *MyConnector) recordTTLError(err error) {
	if err == nil || c == nil {
		return
	}
	c.ttlMu.Lock()
	c.ttlState.lastError = err.Error()
	c.ttlMu.Unlock()
}

// startHistoryTTLLoop starts the periodic retention cleanup. It is a no-op
// when the feature is disabled.
func (c *MyConnector) startHistoryTTLLoop() {
	if c == nil || c.bridge == nil {
		return
	}
	cfg := c.historyTTLConfig()
	if !cfg.Enabled {
		c.log.Info().Msg("History TTL cleanup is disabled")
		return
	}
	c.ttlMu.Lock()
	if c.ttlCancel != nil {
		c.ttlMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.ttlCancel = cancel
	c.ttlMu.Unlock()

	interval := cfg.Interval.Duration()
	if interval <= 0 {
		interval = defaultHistoryTTLInterval
	}
	c.log.Info().Dur("interval", interval).Bool("dry_run", cfg.DryRun).Msg("Starting history TTL cleanup loop")
	go func() {
		timer := time.NewTimer(ttlInitialDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				c.executeHistoryTTL(ctx, c.historyTTLConfig().DryRun)
				timer.Reset(interval)
			}
		}
	}()
}

// stopHistoryTTLLoop cancels the periodic cleanup.
func (c *MyConnector) stopHistoryTTLLoop() {
	if c == nil {
		return
	}
	c.ttlMu.Lock()
	cancel := c.ttlCancel
	c.ttlCancel = nil
	c.ttlMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// CommandTTL shows retention status and can trigger a cleanup run.
var CommandTTL = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		connector, _ := ce.Bridge.Network.(*MyConnector)
		if connector == nil {
			ce.Reply("Network connector is not available")
			return
		}
		if len(ce.Args) == 0 || strings.EqualFold(ce.Args[0], "status") {
			ce.Reply("%s", connector.formatTTLStatus())
			return
		}
		if !strings.EqualFold(ce.Args[0], "run") {
			ce.Reply("Usage: ttl status | ttl run [--dry-run|--apply]")
			return
		}
		cfg := connector.historyTTLConfig()
		dryRun := cfg.DryRun
		for _, arg := range ce.Args[1:] {
			switch strings.ToLower(arg) {
			case "--dry-run", "dry-run":
				dryRun = true
			case "--apply", "apply":
				dryRun = false
			default:
				ce.Reply("Usage: ttl status | ttl run [--dry-run|--apply]")
				return
			}
		}
		if !dryRun && !cfg.Enabled {
			ce.Reply("History TTL is disabled. Set `history_ttl.enabled: true` in the network config, or use `ttl run --dry-run`.")
			return
		}
		ctx, cancel := context.WithTimeout(ce.Ctx, ttlCommandTimeout)
		defer cancel()
		result := connector.executeHistoryTTL(ctx, dryRun)
		if result.Skipped {
			ce.Reply("A history TTL cleanup is already running; try again later.")
			return
		}
		ce.Reply("%s", formatTTLRunResult(result, dryRun))
	},
	Name: "ttl",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAdmin,
		Description: "Show or run the Matrix history retention cleanup",
		Args:        "status | run [--dry-run|--apply]",
	},
	RequiresAdmin: true,
}

// formatTTLStatus renders the configured retention policy and the last run.
func (c *MyConnector) formatTTLStatus() string {
	cfg := c.historyTTLConfig()
	lastRun, lastDryRun, lastResult, lastError, running := c.ttlSnapshot()
	lines := []string{fmt.Sprintf("History TTL: %s", enabledWord(cfg.Enabled))}
	if cfg.Global > 0 {
		lines = append(lines, fmt.Sprintf("Global retention: %s", formatTTLDuration(cfg.Global.Duration())))
	} else {
		lines = append(lines, "Global retention: none")
	}
	lines = append(lines,
		fmt.Sprintf("Overrides: %d room(s), %d contact(s)", len(cfg.PerRoom), len(cfg.PerContact)),
		fmt.Sprintf("Patterns: %s", formatTTLPatterns(cfg.Patterns, cfg.CustomPatterns)),
		fmt.Sprintf("Delete media: %t", cfg.DeleteMedia),
		fmt.Sprintf("Dry run: %t", cfg.DryRun),
		fmt.Sprintf("Interval: %s; scan window: %s; scan limit: %d",
			formatTTLDuration(cfg.Interval.Duration()), formatTTLDuration(cfg.ScanWindow.Duration()), cfg.ScanLimit),
	)
	switch {
	case running:
		lines = append(lines, "Cleanup: running now")
	case lastRun.IsZero():
		lines = append(lines, "Last run: never")
	default:
		lines = append(lines, fmt.Sprintf("Last run: %s (%s) — %d portal(s), %d message(s), %d redaction(s), %d error(s)",
			lastRun.Format("2006-01-02 15:04:05 MST"),
			dryRunWord(lastDryRun),
			lastResult.Portals, lastResult.Messages, lastResult.Redacted, lastResult.Errors))
	}
	if lastError != "" {
		lines = append(lines, "Last error: "+lastError)
	}
	return strings.Join(lines, "\n")
}

// formatTTLRunResult renders one cleanup outcome.
func formatTTLRunResult(result ttlRunResult, dryRun bool) string {
	mode := dryRunWord(dryRun)
	return fmt.Sprintf("History TTL %s finished: %d portal(s) scanned, %d message(s) inspected, %d redaction(s), %d error(s).",
		mode, result.Portals, result.Messages, result.Redacted, result.Errors)
}

func formatTTLPatterns(presets []string, custom map[string]string) string {
	if len(presets) == 0 && len(custom) == 0 {
		return "none (retention only)"
	}
	parts := make([]string, 0, len(presets)+1)
	for _, preset := range presets {
		parts = append(parts, preset)
	}
	if len(custom) > 0 {
		names := make([]string, 0, len(custom))
		for name := range custom {
			names = append(names, name)
		}
		sort.Strings(names)
		parts = append(parts, fmt.Sprintf("%d custom (%s)", len(names), strings.Join(names, ", ")))
	}
	return strings.Join(parts, ", ")
}

func enabledWord(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func dryRunWord(dryRun bool) string {
	if dryRun {
		return "dry run"
	}
	return "applied"
}

// SQL used by the retention cleanup. The column list matches
// database.MessageQuery's scan order, so QueryMany can parse the rows with the
// connector's MessageMetadata type. `metadata->>'has_attachment'` is the same
// JSON access bridgev2 itself uses for ghost metadata lookups.
const ttlMessageBaseQuery = `
SELECT rowid, bridge_id, id, part_id, mxid, room_id, room_receiver, sender_id, sender_mxid,
       timestamp, edit_count, double_puppeted, thread_root_id, reply_to_id, reply_to_part_id,
       send_txn_id, metadata
FROM message
`

const ttlOldMessagesQueryBase = ttlMessageBaseQuery + `
WHERE bridge_id=$1 AND room_id=$2 AND room_receiver=$3 AND timestamp<=$4`

const ttlOldMessagesOrder = `
ORDER BY timestamp ASC, id ASC, part_id ASC LIMIT $5`

const ttlOldMessagesQuery = ttlOldMessagesQueryBase + ttlOldMessagesOrder

const ttlOldMessagesNoMediaFilter = `
  AND (metadata->>'has_attachment' IS NULL OR CAST(metadata->>'has_attachment' AS TEXT) NOT IN ('true', '1'))`

const ttlRecentMessagesQuery = ttlMessageBaseQuery + `
WHERE bridge_id=$1 AND room_id=$2 AND room_receiver=$3 AND timestamp>$4
ORDER BY timestamp DESC, id DESC, part_id DESC LIMIT $5`
