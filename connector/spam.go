// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// Anti-spam decision actions.
const (
	spamActionAllow = "allow"
	spamActionLabel = "label"
	spamActionFlag  = "flag"
	spamActionHold  = "hold"
	spamActionBlock = "block"
)

// Ghost labels applied by the anti-spam gate.
const (
	// SpamFlagLabel marks a sender whose message was flagged, held or blocked.
	SpamFlagLabel = "spam"
	// SpamUnknownLabel marks a sender the bridge had not seen before.
	SpamUnknownLabel = "unknown sender"
)

const (
	// spamRecentCapacity bounds the in-memory `spam recent` ring.
	spamRecentCapacity = 200
	// spamRateLimiterMaxSenders bounds the first-contact limiter state.
	spamRateLimiterMaxSenders = 2048
	// spamDuplicateMaxBodies bounds the duplicate tracker state.
	spamDuplicateMaxBodies = 1024
	// spamRecentDefaultCount is how many records `spam recent` shows.
	spamRecentDefaultCount = 20
)

// spamPattern is a compiled scoring pattern.
type spamPattern struct {
	Name   string
	Re     *regexp.Regexp
	Weight int
	Action string
}

// spamPolicy is the compiled form of the anti-spam configuration.
type spamPolicy struct {
	Enabled             bool
	DryRun              bool
	UnknownSenderPolicy string
	AllowLabels         map[string]struct{}
	FirstContactLimit   int
	FirstContactWindow  time.Duration
	Threshold           int
	Patterns            []spamPattern
	SuspiciousTLDs      map[string]struct{}
	Shorteners          map[string]struct{}
	URLWeight           int
	DuplicateWindow     time.Duration
	DuplicateCount      int
	Notices             bool
}

// compileSpamPolicy validates and compiles the anti-spam configuration. The
// config parser already rejects invalid regexes; compiling again here keeps
// runtime errors (a hand-edited config) from breaking message handling.
func compileSpamPolicy(cfg AntiSpamConfig) (spamPolicy, error) {
	policy := spamPolicy{
		Enabled:             cfg.Enabled,
		DryRun:              cfg.DryRun,
		UnknownSenderPolicy: cfg.UnknownSenderPolicy,
		AllowLabels:         make(map[string]struct{}, len(cfg.AllowLabels)),
		FirstContactLimit:   cfg.FirstContact.Limit,
		FirstContactWindow:  cfg.FirstContact.Window.Duration(),
		Threshold:           cfg.Threshold,
		SuspiciousTLDs:      make(map[string]struct{}, len(cfg.URLs.SuspiciousTLDs)),
		Shorteners:          make(map[string]struct{}, len(cfg.URLs.Shorteners)),
		URLWeight:           cfg.URLs.Weight,
		DuplicateWindow:     cfg.Duplicates.Window.Duration(),
		DuplicateCount:      cfg.Duplicates.Count,
		Notices:             cfg.Notices.Management,
	}
	if policy.UnknownSenderPolicy == "" {
		policy.UnknownSenderPolicy = AntiSpamPolicyBridge
	}
	if policy.FirstContactWindow <= 0 {
		policy.FirstContactWindow = defaultAntiSpamFirstContactWindow
	}
	if policy.DuplicateWindow <= 0 {
		policy.DuplicateWindow = defaultAntiSpamDuplicateWindow
	}
	if policy.URLWeight < 0 {
		policy.URLWeight = 0
	}
	for _, label := range cfg.AllowLabels {
		policy.AllowLabels[strings.ToLower(strings.TrimSpace(label))] = struct{}{}
	}
	for _, tld := range cfg.URLs.SuspiciousTLDs {
		policy.SuspiciousTLDs[strings.TrimPrefix(strings.ToLower(strings.TrimSpace(tld)), ".")] = struct{}{}
	}
	for _, shortener := range cfg.URLs.Shorteners {
		policy.Shorteners[strings.ToLower(strings.TrimSpace(shortener))] = struct{}{}
	}
	for _, pattern := range cfg.Patterns {
		compiled, err := regexp.Compile(pattern.Regex)
		if err != nil {
			return spamPolicy{}, fmt.Errorf("compile anti_spam pattern %q: %w", pattern.Name, err)
		}
		action := pattern.Action
		if action == "" {
			action = AntiSpamActionFlag
		}
		policy.Patterns = append(policy.Patterns, spamPattern{
			Name:   pattern.Name,
			Re:     compiled,
			Weight: pattern.Weight,
			Action: action,
		})
	}
	return policy, nil
}

// spamDecision is the outcome of evaluating one message.
type spamDecision struct {
	Action  string
	Score   int
	Reasons []string
	// Notify reports whether the configured notice policy should announce the
	// decision in the management room.
	Notify bool
}

// dropped reports whether the decision removes the message from bridging.
func (d spamDecision) dropped() bool {
	return d.Action == spamActionHold || d.Action == spamActionBlock
}

// spamEvaluation is the pure input of the decision engine. Stateful inputs
// (first-contact rate, duplicate count) are computed by the caller so the
// decision itself stays testable without clocks or counters.
type spamEvaluation struct {
	Body           string
	KnownContact   bool
	Labels         []string
	RateCount      int
	DuplicateCount int
}

// evaluateSpam applies the configured policy to one message. Precedence:
// block patterns, then hold for unknown senders, then score/duplicate/rate
// flags, then the unknown-sender label.
func evaluateSpam(policy spamPolicy, ev spamEvaluation) spamDecision {
	decision := spamDecision{Action: spamActionAllow}
	if !policy.Enabled {
		return decision
	}
	allowed := ev.KnownContact || hasSpamAllowLabel(ev.Labels, policy.AllowLabels)
	blocked := false
	for _, pattern := range policy.Patterns {
		if !pattern.Re.MatchString(ev.Body) {
			continue
		}
		decision.Score += pattern.Weight
		decision.Reasons = append(decision.Reasons, "pattern:"+pattern.Name)
		if pattern.Action == AntiSpamActionBlock {
			blocked = true
		}
	}
	for _, signal := range spamURLSignals(policy, ev.Body) {
		decision.Score += policy.URLWeight
		decision.Reasons = append(decision.Reasons, "url:"+signal)
	}
	duplicate := policy.DuplicateCount > 0 && ev.DuplicateCount >= policy.DuplicateCount
	if duplicate {
		decision.Reasons = append(decision.Reasons, "duplicate")
	}
	rateExceeded := !allowed && policy.FirstContactLimit > 0 && ev.RateCount > policy.FirstContactLimit
	if rateExceeded {
		decision.Reasons = append(decision.Reasons, "first-contact-rate")
	}
	overThreshold := policy.Threshold > 0 && decision.Score >= policy.Threshold
	if overThreshold {
		decision.Reasons = append(decision.Reasons, "score")
	}

	if blocked {
		decision.Action = spamActionBlock
		decision.Notify = true
		return decision
	}
	if !allowed {
		decision.Reasons = append(decision.Reasons, "unknown-sender")
		if policy.UnknownSenderPolicy == AntiSpamPolicyHold {
			decision.Action = spamActionHold
			decision.Notify = true
			return decision
		}
	}
	switch {
	case overThreshold || duplicate || rateExceeded:
		decision.Action = spamActionFlag
		decision.Notify = true
	case !allowed:
		decision.Action = spamActionLabel
		decision.Notify = policy.UnknownSenderPolicy == AntiSpamPolicyBridge
	}
	return decision
}

// hasSpamAllowLabel reports whether any ghost label is on the allowlist.
func hasSpamAllowLabel(labels []string, allow map[string]struct{}) bool {
	if len(labels) == 0 || len(allow) == 0 {
		return false
	}
	for _, label := range labels {
		if _, ok := allow[strings.ToLower(strings.TrimSpace(label))]; ok {
			return true
		}
	}
	return false
}

// spamURLRegexp extracts http(s):// and www. URLs without any network access.
var spamURLRegexp = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>"'()\[\]]+`)

// spamURLSignals returns the distinct lexical signals found in the message
// body, sorted for stable output. Each signal type adds its weight once per
// message, so a wall of links cannot blow up the score.
func spamURLSignals(policy spamPolicy, body string) []string {
	hosts := spamURLHosts(body)
	if len(hosts) == 0 {
		return nil
	}
	signals := make(map[string]struct{}, 2)
	for _, host := range hosts {
		if _, ok := policy.Shorteners[host]; ok {
			signals["shortener"] = struct{}{}
			continue
		}
		for shortener := range policy.Shorteners {
			if strings.HasSuffix(host, "."+shortener) {
				signals["shortener"] = struct{}{}
				break
			}
		}
		if tld := spamHostTLD(host); tld != "" {
			if _, ok := policy.SuspiciousTLDs[tld]; ok {
				signals["suspicious-tld"] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(signals))
	for signal := range signals {
		out = append(out, signal)
	}
	sort.Strings(out)
	return out
}

// spamURLHosts extracts normalized hosts from the URLs in a body.
func spamURLHosts(body string) []string {
	matches := spamURLRegexp.FindAllString(body, -1)
	if len(matches) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(matches))
	for _, match := range matches {
		if host := spamURLHost(match); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// spamURLHost reduces a raw URL into its lowercase host.
func spamURLHost(raw string) string {
	host := strings.TrimSpace(raw)
	if idx := strings.Index(host, "://"); idx >= 0 {
		host = host[idx+3:]
	}
	host = strings.TrimPrefix(host, "//")
	if idx := strings.IndexAny(host, "/?#"); idx >= 0 {
		host = host[:idx]
	}
	if idx := strings.LastIndexByte(host, '@'); idx >= 0 {
		host = host[idx+1:]
	}
	if strings.HasPrefix(host, "[") {
		// IPv6 literal: keep the address without brackets/port.
		if end := strings.IndexByte(host, ']'); end > 0 {
			host = host[1:end]
		}
	} else if idx := strings.LastIndexByte(host, ':'); idx >= 0 {
		host = host[:idx]
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// spamHostTLD returns the final label of a host, or an empty string for IP
// literals and single-label hosts.
func spamHostTLD(host string) string {
	if host == "" || net.ParseIP(host) != nil {
		return ""
	}
	idx := strings.LastIndexByte(host, '.')
	if idx < 0 || idx == len(host)-1 {
		return ""
	}
	return host[idx+1:]
}

// spamBodyHash normalizes a body (lowercase, collapsed whitespace) and
// returns a truncated SHA-256 hex digest. Empty bodies return "".
func spamBodyHash(body string) string {
	normalized := strings.Join(strings.Fields(strings.ToLower(body)), " ")
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:16])
}

// spamRateLimiter counts recent messages per sender in a sliding window.
type spamRateLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	entries map[string][]time.Time
}

func newSpamRateLimiter(window time.Duration) *spamRateLimiter {
	if window <= 0 {
		window = defaultAntiSpamFirstContactWindow
	}
	return &spamRateLimiter{window: window, entries: make(map[string][]time.Time)}
}

// record adds one message from sender and returns how many messages from that
// sender are inside the window (including this one).
func (r *spamRateLimiter) record(sender string, now time.Time) int {
	if r == nil || sender == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := now.Add(-r.window)
	kept := r.entries[sender][:0]
	for _, ts := range r.entries[sender] {
		if !ts.Before(cutoff) {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	r.entries[sender] = kept
	if len(r.entries) > spamRateLimiterMaxSenders {
		r.pruneLocked(cutoff)
	}
	return len(kept)
}

func (r *spamRateLimiter) pruneLocked(cutoff time.Time) {
	for sender, timestamps := range r.entries {
		kept := timestamps[:0]
		for _, ts := range timestamps {
			if !ts.Before(cutoff) {
				kept = append(kept, ts)
			}
		}
		if len(kept) == 0 {
			delete(r.entries, sender)
			continue
		}
		r.entries[sender] = kept
	}
	for sender := range r.entries {
		if len(r.entries) <= spamRateLimiterMaxSenders {
			break
		}
		delete(r.entries, sender)
	}
}

// spamDuplicateTracker counts distinct senders per normalized body hash.
type spamDuplicateTracker struct {
	mu      sync.Mutex
	window  time.Duration
	entries map[string]map[string]time.Time
}

func newSpamDuplicateTracker(window time.Duration) *spamDuplicateTracker {
	if window <= 0 {
		window = defaultAntiSpamDuplicateWindow
	}
	return &spamDuplicateTracker{window: window, entries: make(map[string]map[string]time.Time)}
}

// record adds one body/sender observation and returns how many distinct
// senders sent the same body inside the window (including this one).
func (d *spamDuplicateTracker) record(body, sender string, now time.Time) int {
	if d == nil || sender == "" {
		return 0
	}
	hash := spamBodyHash(body)
	if hash == "" {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cutoff := now.Add(-d.window)
	senders, ok := d.entries[hash]
	if !ok {
		senders = make(map[string]time.Time)
		d.entries[hash] = senders
	}
	for existing, firstSeen := range senders {
		if firstSeen.Before(cutoff) {
			delete(senders, existing)
		}
	}
	if _, seen := senders[sender]; !seen {
		senders[sender] = now
	}
	if len(d.entries) > spamDuplicateMaxBodies {
		d.pruneLocked(cutoff)
	}
	return len(senders)
}

func (d *spamDuplicateTracker) pruneLocked(cutoff time.Time) {
	for hash, senders := range d.entries {
		for sender, firstSeen := range senders {
			if firstSeen.Before(cutoff) {
				delete(senders, sender)
			}
		}
		if len(senders) == 0 {
			delete(d.entries, hash)
		}
	}
	for hash := range d.entries {
		if len(d.entries) <= spamDuplicateMaxBodies {
			break
		}
		delete(d.entries, hash)
	}
}

// spamRecord is one logged decision.
type spamRecord struct {
	Time      time.Time
	Sender    string
	MessageID string
	PortalID  string
	Action    string
	Score     int
	Reasons   []string
	DryRun    bool
}

// spamCounters aggregates decisions since startup.
type spamCounters struct {
	Evaluated int
	Allowed   int
	Labeled   int
	Flagged   int
	Held      int
	Blocked   int
	DryRun    int
}

func (c *spamCounters) add(action string, dryRun bool) {
	c.Evaluated++
	switch action {
	case spamActionAllow:
		c.Allowed++
	case spamActionLabel:
		c.Labeled++
	case spamActionFlag:
		c.Flagged++
	case spamActionHold:
		c.Held++
	case spamActionBlock:
		c.Blocked++
	}
	if dryRun && action != spamActionAllow {
		c.DryRun++
	}
}

// spamRateLimiter returns the connector's first-contact limiter, recreating it
// when the configured window changes.
func (c *MyConnector) spamRateLimiter(window time.Duration) *spamRateLimiter {
	c.spamMu.Lock()
	defer c.spamMu.Unlock()
	if c.spamRate == nil || c.spamRate.window != window {
		c.spamRate = newSpamRateLimiter(window)
	}
	return c.spamRate
}

// spamDuplicateTracker returns the connector's duplicate detector, recreating
// it when the configured window changes.
func (c *MyConnector) spamDuplicateTracker(window time.Duration) *spamDuplicateTracker {
	c.spamMu.Lock()
	defer c.spamMu.Unlock()
	if c.spamDupes == nil || c.spamDupes.window != window {
		c.spamDupes = newSpamDuplicateTracker(window)
	}
	return c.spamDupes
}

// recordSpamDecision stores one decision in the counters and the recent ring.
// Allow decisions are counted but not kept in the ring.
func (c *MyConnector) recordSpamDecision(record spamRecord) {
	if c == nil {
		return
	}
	c.spamMu.Lock()
	defer c.spamMu.Unlock()
	c.spamCounters.add(record.Action, record.DryRun)
	c.spamLast = record.Time
	if record.Action == spamActionAllow {
		return
	}
	if len(c.spamRecent) >= spamRecentCapacity {
		// Drop the oldest half to keep memory bounded without a ring index.
		keep := spamRecentCapacity / 2
		copy(c.spamRecent, c.spamRecent[len(c.spamRecent)-keep:])
		c.spamRecent = c.spamRecent[:keep]
	}
	stored := record
	stored.Reasons = append([]string(nil), record.Reasons...)
	c.spamRecent = append(c.spamRecent, stored)
}

// spamSnapshot returns the counters, a copy of the recent decisions (oldest
// first) and the last decision time.
func (c *MyConnector) spamSnapshot() (spamCounters, []spamRecord, time.Time) {
	if c == nil {
		return spamCounters{}, nil, time.Time{}
	}
	c.spamMu.Lock()
	defer c.spamMu.Unlock()
	recent := make([]spamRecord, len(c.spamRecent))
	copy(recent, c.spamRecent)
	return c.spamCounters, recent, c.spamLast
}

// spamGate evaluates one inbound message and applies the decision. It returns
// the decision and whether the message must be dropped before bridging.
func (nc *MyNetworkClient) spamGate(ctx context.Context, portalKey networkid.PortalKey, sender, messageID, body string, ts time.Time, group bool) (spamDecision, bool) {
	allow := spamDecision{Action: spamActionAllow}
	if nc == nil || nc.connector == nil || nc.bridge == nil {
		return allow, false
	}
	cfg := nc.connector.antiSpamConfig()
	if !cfg.Enabled {
		return allow, false
	}
	policy, err := compileSpamPolicy(cfg)
	if err != nil {
		nc.log.Warn().Err(err).Msg("Anti-spam configuration is invalid; allowing the message")
		return allow, false
	}
	canonical := CanonicalAddress(sender)
	if canonical == "" {
		return allow, false
	}
	for _, own := range nc.ownNumbers() {
		if own == canonical {
			return allow, false
		}
	}

	known := group
	if !known {
		if portal, err := nc.bridge.GetExistingPortalByKey(ctx, portalKey); err == nil && portal != nil {
			known = true
		}
	}
	if !known && nc.login != nil {
		if dm, err := nc.bridge.GetDMPortal(ctx, nc.login.ID, MakeUserID(canonical)); err == nil && dm != nil {
			known = true
		}
	}
	labels := nc.ghostLabels(ctx, MakeUserID(canonical))

	rateCount := 0
	if !known {
		rateCount = nc.connector.spamRateLimiter(policy.FirstContactWindow).record(canonical, ts)
	}
	duplicateCount := 0
	if !group {
		duplicateCount = nc.connector.spamDuplicateTracker(policy.DuplicateWindow).record(body, canonical, ts)
	}

	decision := evaluateSpam(policy, spamEvaluation{
		Body:           body,
		KnownContact:   known,
		Labels:         labels,
		RateCount:      rateCount,
		DuplicateCount: duplicateCount,
	})
	record := spamRecord{
		Time:      ts.UTC(),
		Sender:    canonical,
		MessageID: messageID,
		PortalID:  string(portalKey.ID),
		Action:    decision.Action,
		Score:     decision.Score,
		Reasons:   decision.Reasons,
		DryRun:    policy.DryRun,
	}
	return decision, nc.applySpamDecision(ctx, canonical, decision, record)
}

// applySpamDecision logs the decision, records counters and (unless dry-run)
// applies the ghost label, posts a management notice and reports whether the
// message must be dropped.
func (nc *MyNetworkClient) applySpamDecision(ctx context.Context, sender string, decision spamDecision, record spamRecord) bool {
	if nc == nil || nc.connector == nil {
		return false
	}
	nc.connector.recordSpamDecision(record)
	if decision.Action == spamActionAllow {
		return false
	}
	nc.log.Info().
		Str("action", record.Action).
		Str("sender", record.Sender).
		Str("message_id", record.MessageID).
		Str("portal_id", record.PortalID).
		Int("score", record.Score).
		Strs("reasons", record.Reasons).
		Bool("dry_run", record.DryRun).
		Msg("Anti-spam decision")
	if record.DryRun {
		nc.sendSpamNotice(ctx, decision, record, true)
		return false
	}
	nc.applyGhostLabel(ctx, MakeUserID(sender), spamGhostLabel(decision))
	if decision.Notify {
		nc.sendSpamNotice(ctx, decision, record, false)
	}
	return decision.dropped()
}

// spamGhostLabel picks the ghost label for a decision: "unknown sender" for
// the unknown-sender paths, "spam" for scored/duplicate/blocked messages.
func spamGhostLabel(decision spamDecision) string {
	if decision.Action == spamActionLabel {
		return SpamUnknownLabel
	}
	if decision.Action == spamActionHold && len(decision.Reasons) == 1 && decision.Reasons[0] == "unknown-sender" {
		return SpamUnknownLabel
	}
	return SpamFlagLabel
}

// ghostLabels returns the labels stored on a ghost, or nil when it does not
// exist yet.
func (nc *MyNetworkClient) ghostLabels(ctx context.Context, ghostID networkid.UserID) []string {
	if nc == nil || nc.bridge == nil || ghostID == "" {
		return nil
	}
	ghost, err := nc.bridge.GetExistingGhostByID(ctx, ghostID)
	if err != nil || ghost == nil {
		return nil
	}
	meta, _ := ghost.Metadata.(*GhostMetadata)
	if meta == nil {
		return nil
	}
	return append([]string(nil), meta.Labels...)
}

// applyGhostLabel adds a label to a ghost, creating the ghost when needed.
func (nc *MyNetworkClient) applyGhostLabel(ctx context.Context, ghostID networkid.UserID, label string) {
	if nc == nil || nc.bridge == nil || ghostID == "" || label == "" {
		return
	}
	ghost, err := nc.bridge.GetExistingGhostByID(ctx, ghostID)
	if err != nil {
		nc.log.Debug().Err(err).Str("ghost_id", string(ghostID)).Msg("Failed to load ghost for anti-spam labeling")
		return
	}
	if ghost == nil {
		ghost, err = nc.bridge.GetGhostByID(ctx, ghostID)
		if err != nil {
			nc.log.Warn().Err(err).Str("ghost_id", string(ghostID)).Msg("Failed to create ghost for anti-spam labeling")
			return
		}
	}
	meta, _ := ghost.Metadata.(*GhostMetadata)
	if meta == nil {
		meta = &GhostMetadata{}
		ghost.Metadata = meta
	}
	labels := addLabel(meta.Labels, label)
	if len(labels) == len(meta.Labels) {
		return
	}
	meta.Labels = labels
	if err := nc.bridge.DB.Ghost.Update(ctx, ghost.Ghost); err != nil {
		nc.log.Warn().Err(err).Str("ghost_id", string(ghostID)).Msg("Failed to save anti-spam label")
	}
}

// sendSpamNotice posts the management-room digest line for a decision. Only
// ids and pattern names are included, never the message body.
func (nc *MyNetworkClient) sendSpamNotice(ctx context.Context, decision spamDecision, record spamRecord, dryRun bool) {
	if nc == nil || nc.connector == nil || nc.bridge == nil {
		return
	}
	policy, err := compileSpamPolicy(nc.connector.antiSpamConfig())
	if err != nil || !policy.Notices || !decision.Notify {
		return
	}
	if nc.login == nil || nc.login.User == nil {
		return
	}
	sendNotice(nc.bridge.Bot, nc.login.User.ManagementRoom, formatSpamNotice(record, decision, dryRun))
}

// formatSpamNotice renders one management-room digest line.
func formatSpamNotice(record spamRecord, decision spamDecision, dryRun bool) string {
	reasons := strings.Join(record.Reasons, ", ")
	if reasons == "" {
		reasons = "none"
	}
	prefix := "Anti-spam"
	if dryRun {
		prefix += " (dry run)"
	}
	switch decision.Action {
	case spamActionHold:
		if dryRun {
			return fmt.Sprintf("%s: would hold message from %s (%s; score %d); it was bridged anyway.", prefix, record.Sender, reasons, record.Score)
		}
		return fmt.Sprintf("%s: held message from %s (%s; score %d); it was not bridged.", prefix, record.Sender, reasons, record.Score)
	case spamActionBlock:
		if dryRun {
			return fmt.Sprintf("%s: would block message from %s (%s; score %d); it was bridged anyway.", prefix, record.Sender, reasons, record.Score)
		}
		return fmt.Sprintf("%s: blocked message from %s (%s; score %d); it was not bridged.", prefix, record.Sender, reasons, record.Score)
	case spamActionFlag:
		return fmt.Sprintf("%s: flagged message from %s (%s; score %d). Use `spam recent` for details.", prefix, record.Sender, reasons, record.Score)
	default:
		if dryRun {
			return fmt.Sprintf("%s: would label new sender %s as %q.", prefix, record.Sender, SpamUnknownLabel)
		}
		return fmt.Sprintf("%s: new sender %s labeled %q. Use `label %s allow` to allowlist.", prefix, record.Sender, SpamUnknownLabel, record.Sender)
	}
}

// applySpamMessageMetadata stores the decision on the bridged message so
// `spam recent` and ad-hoc SQL can inspect it later. Messages without a
// metadata part are left untouched.
func applySpamMessageMetadata(converted *bridgev2.ConvertedMessage, decision spamDecision) {
	if converted == nil || decision.Action == "" || decision.Action == spamActionAllow {
		return
	}
	for _, part := range converted.Parts {
		if part == nil {
			continue
		}
		meta, ok := part.DBMetadata.(*MessageMetadata)
		if !ok || meta == nil {
			continue
		}
		meta.SpamAction = decision.Action
		meta.SpamScore = decision.Score
		meta.SpamReasons = append([]string(nil), decision.Reasons...)
		return
	}
}

// CommandSpam reports the anti-spam configuration and counters and lists the
// recent decisions (ids only).
var CommandSpam = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		connector, _ := ce.Bridge.Network.(*MyConnector)
		if connector == nil {
			ce.Reply("Network connector is not available")
			return
		}
		if len(ce.Args) == 0 || strings.EqualFold(ce.Args[0], "status") {
			ce.Reply("%s", connector.formatSpamStatus())
			return
		}
		if !strings.EqualFold(ce.Args[0], "recent") {
			ce.Reply("Usage: spam status | spam recent [count]")
			return
		}
		count := spamRecentDefaultCount
		if len(ce.Args) > 1 {
			parsed, err := parseSpamRecentCount(ce.Args[1])
			if err != nil {
				ce.Reply("%v", err)
				return
			}
			count = parsed
		}
		if len(ce.Args) > 2 {
			ce.Reply("Usage: spam status | spam recent [count]")
			return
		}
		ce.Reply("%s", connector.formatSpamRecent(count))
	},
	Name: "spam",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAdmin,
		Description: "Show the local anti-spam state and recent decisions (ids only)",
		Args:        "status | recent [count]",
	},
	RequiresAdmin: true,
}

func parseSpamRecentCount(arg string) (int, error) {
	count, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || count <= 0 {
		return 0, fmt.Errorf("Usage: spam recent [count] (count is a positive integer)")
	}
	if count > spamRecentCapacity {
		count = spamRecentCapacity
	}
	return count, nil
}

// formatSpamStatus renders the effective configuration and counters.
func (c *MyConnector) formatSpamStatus() string {
	cfg := c.antiSpamConfig()
	counters, recent, last := c.spamSnapshot()
	lines := []string{
		fmt.Sprintf("Anti-spam: %s (dry run: %t)", enabledWord(cfg.Enabled), cfg.DryRun),
		fmt.Sprintf("Unknown sender policy: %s; allow labels: %s", cfg.UnknownSenderPolicy, formatSpamList(cfg.AllowLabels)),
		fmt.Sprintf("Score threshold: %d; patterns: %d (%s)", cfg.Threshold, len(cfg.Patterns), formatSpamPatternNames(cfg.Patterns)),
		fmt.Sprintf("First contact: %d message(s) per %s", cfg.FirstContact.Limit, formatTTLDuration(cfg.FirstContact.Window.Duration())),
		fmt.Sprintf("URLs: %d suspicious TLD(s), %d shortener(s), weight %d",
			len(cfg.URLs.SuspiciousTLDs), len(cfg.URLs.Shorteners), cfg.URLs.Weight),
		fmt.Sprintf("Duplicates: %d sender(s) with the same body within %s",
			cfg.Duplicates.Count, formatTTLDuration(cfg.Duplicates.Window.Duration())),
		fmt.Sprintf("Management notices: %t", cfg.Notices.Management),
		fmt.Sprintf("Decisions: %d evaluated — %d allowed, %d labeled, %d flagged, %d held, %d blocked (%d dry run)",
			counters.Evaluated, counters.Allowed, counters.Labeled, counters.Flagged, counters.Held, counters.Blocked, counters.DryRun),
	}
	if len(recent) > 0 {
		lines = append(lines, fmt.Sprintf("Recent decisions kept: %d", len(recent)))
	}
	if last.IsZero() {
		lines = append(lines, "Last decision: never")
	} else {
		lines = append(lines, fmt.Sprintf("Last decision: %s", last.Format("2006-01-02 15:04:05 MST")))
	}
	return strings.Join(lines, "\n")
}

// formatSpamRecent renders the most recent decisions, newest first.
func (c *MyConnector) formatSpamRecent(count int) string {
	_, recent, _ := c.spamSnapshot()
	if len(recent) == 0 {
		return "No anti-spam decisions yet."
	}
	if count <= 0 || count > len(recent) {
		count = len(recent)
	}
	lines := make([]string, 0, count)
	for i := len(recent) - 1; i >= len(recent)-count; i-- {
		record := recent[i]
		reasons := strings.Join(record.Reasons, ", ")
		if reasons == "" {
			reasons = "none"
		}
		mode := ""
		if record.DryRun {
			mode = " [dry run]"
		}
		lines = append(lines, fmt.Sprintf("- %s %s%s -> %s (score %d; %s)",
			record.Time.Format("2006-01-02 15:04:05 MST"),
			record.Sender, mode, record.Action, record.Score, reasons))
	}
	return strings.Join(lines, "\n")
}

func formatSpamList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

func formatSpamPatternNames(patterns []AntiSpamPatternConfig) string {
	if len(patterns) == 0 {
		return "none"
	}
	names := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		names = append(names, pattern.Name)
	}
	return strings.Join(names, ", ")
}
