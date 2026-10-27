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
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
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
	// spamRecentCapacity bounds the persisted `spam recent` log and its
	// in-memory mirror.
	spamRecentCapacity = 200
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
	NoticeMode          string
	DigestInterval      time.Duration
	ClassWeights        map[string]int
	ClassHold           map[string]struct{}
	ClassBlock          map[string]struct{}
	HomeCountry         string
	Reputation          AntiSpamReputationConfig
	FeedWeight          int
	FeedPaths           []string
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
		NoticeMode:          cfg.Notices.NoticeMode(),
		DigestInterval:      cfg.Notices.DigestInterval.Duration(),
		HomeCountry:         normalizeSpamHomeCountry(cfg.SenderClasses.HomeCountry),
		Reputation:          cfg.Reputation,
		FeedWeight:          cfg.Feeds.Weight,
		FeedPaths:           append([]string(nil), cfg.Feeds.Paths...),
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
	if policy.DigestInterval <= 0 {
		policy.DigestInterval = defaultAntiSpamDigestInterval
	}
	if policy.URLWeight < 0 {
		policy.URLWeight = 0
	}
	if policy.Reputation.Weight < 0 {
		policy.Reputation.Weight = 0
	}
	if policy.FeedWeight < 0 {
		policy.FeedWeight = 0
	}
	policy.ClassWeights = map[string]int{
		SpamSenderClassShortcode:     cfg.SenderClasses.ShortcodeWeight,
		SpamSenderClassTollFree:      cfg.SenderClasses.TollFreeWeight,
		SpamSenderClassAlphanumeric:  cfg.SenderClasses.AlphanumericWeight,
		SpamSenderClassInternational: cfg.SenderClasses.InternationalWeight,
	}
	policy.ClassHold = make(map[string]struct{}, len(cfg.SenderClasses.HoldClasses))
	for _, class := range cfg.SenderClasses.HoldClasses {
		policy.ClassHold[strings.ToLower(strings.TrimSpace(class))] = struct{}{}
	}
	policy.ClassBlock = make(map[string]struct{}, len(cfg.SenderClasses.BlockClasses))
	for _, class := range cfg.SenderClasses.BlockClasses {
		policy.ClassBlock[strings.ToLower(strings.TrimSpace(class))] = struct{}{}
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
// (first-contact rate, duplicate count, reputation, feed matches, sender
// class) are computed by the caller so the decision itself stays testable
// without clocks or counters.
type spamEvaluation struct {
	Body            string
	KnownContact    bool
	Labels          []string
	Override        string
	RateCount       int
	DuplicateCount  int
	SenderClass     string
	ClassWeight     int
	ClassHold       bool
	ClassBlock      bool
	ReputationScore int
	FeedSignals     []string
}

// evaluateSpam applies the configured policy to one message. Precedence:
// per-contact override "allow", block rules (pattern/class/override), hold for
// unknown senders, then score/duplicate/rate flags, then the unknown-sender
// label.
func evaluateSpam(policy spamPolicy, ev spamEvaluation) spamDecision {
	decision := spamDecision{Action: spamActionAllow}
	if !policy.Enabled {
		return decision
	}
	if ev.Override == SpamContactPolicyAllow {
		return decision
	}
	allowed := ev.KnownContact || hasSpamAllowLabel(ev.Labels, policy.AllowLabels)
	switch ev.Override {
	case SpamContactPolicyBridge:
		allowed = true
	case SpamContactPolicyLabel, SpamContactPolicyHold, SpamContactPolicyBlock:
		allowed = false
	}
	if ev.Override != "" && ev.Override != SpamContactPolicyAllow {
		decision.Reasons = append(decision.Reasons, "override:"+ev.Override)
	}
	blocked := ev.ClassBlock
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
	for _, signal := range ev.FeedSignals {
		decision.Score += policy.FeedWeight
		decision.Reasons = append(decision.Reasons, "feed:"+signal)
	}
	if ev.SenderClass != "" && ev.ClassWeight > 0 {
		decision.Score += ev.ClassWeight
		decision.Reasons = append(decision.Reasons, "class:"+ev.SenderClass)
	}
	if ev.ReputationScore > 0 {
		decision.Score += ev.ReputationScore
		decision.Reasons = append(decision.Reasons, "reputation")
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
	unknownPolicy := policy.UnknownSenderPolicy
	switch ev.Override {
	case SpamContactPolicyLabel:
		unknownPolicy = AntiSpamPolicyLabel
	case SpamContactPolicyHold:
		unknownPolicy = AntiSpamPolicyHold
	}
	if ev.ClassHold && !allowed {
		unknownPolicy = AntiSpamPolicyHold
	}

	if blocked || ev.Override == SpamContactPolicyBlock {
		decision.Action = spamActionBlock
		decision.Notify = true
		return decision
	}
	if !allowed {
		decision.Reasons = append(decision.Reasons, "unknown-sender")
		if unknownPolicy == AntiSpamPolicyHold {
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
		decision.Notify = unknownPolicy == AntiSpamPolicyBridge
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

// ensureSpamStore lazily creates and caches the persistent anti-spam state
// store in the bridge database. It returns an error when the bridge database
// is not available (e.g. in unit tests of the pure decision engine); callers
// fall back to zero state in that case.
func (c *MyConnector) ensureSpamStore(ctx context.Context) (*spamStore, error) {
	if c == nil || c.bridge == nil || c.bridge.DB == nil {
		return nil, fmt.Errorf("bridge database is not available")
	}
	c.spamMu.Lock()
	if c.spamStore == nil {
		c.spamStore = newSpamStore(c.bridge.DB.Database, c.bridge.DB.BridgeID)
	}
	store := c.spamStore
	c.spamMu.Unlock()
	if err := store.ensure(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

// loadSpamRecent seeds the in-memory recent-decisions mirror from the
// persistent store, so `spam recent` survives restarts.
func (c *MyConnector) loadSpamRecent(ctx context.Context) {
	store, err := c.ensureSpamStore(ctx)
	if err != nil {
		return
	}
	records, err := store.recentDecisions(ctx, spamRecentCapacity)
	if err != nil {
		c.log.Warn().Err(err).Msg("Failed to load persisted anti-spam decisions")
		return
	}
	c.spamMu.Lock()
	c.spamRecent = records
	c.spamMu.Unlock()
}

// importConfiguredSpamFeeds imports every anti_spam.feeds.paths file. It only
// reads local files; the bridge never fetches feeds itself.
func (c *MyConnector) importConfiguredSpamFeeds(ctx context.Context, paths []string) {
	if c == nil || len(paths) == 0 {
		return
	}
	store, err := c.ensureSpamStore(ctx)
	if err != nil {
		c.log.Warn().Err(err).Msg("Anti-spam feed paths are configured but the state store is unavailable")
		return
	}
	for _, path := range paths {
		result, err := store.importFeedFile(ctx, path)
		if err != nil {
			c.log.Warn().Err(err).Str("path", path).Msg("Failed to import a configured anti-spam feed")
			continue
		}
		c.log.Info().Str("path", path).Str("source", result.Source).
			Int("added", result.Added).Int("total", result.Total).Msg("Imported anti-spam feed file")
	}
}

// recordSpamDigest folds one decision into the pending digest. It is a no-op
// unless notices.mode is "digest".
func (c *MyConnector) recordSpamDigest(record spamRecord, room id.RoomID) {
	if c == nil || c.antiSpamConfig().Notices.NoticeMode() != AntiSpamNoticeDigest {
		return
	}
	c.spamMu.Lock()
	c.spamDigest.add(record, room)
	c.spamMu.Unlock()
}

// spamDigestPending reports how many decisions are waiting for the next
// digest flush.
func (c *MyConnector) spamDigestPending() int {
	if c == nil {
		return 0
	}
	c.spamMu.Lock()
	defer c.spamMu.Unlock()
	return c.spamDigest.Total
}

// takeSpamDigest renders and resets the pending digest.
func (c *MyConnector) takeSpamDigest(now time.Time) (string, id.RoomID, bool) {
	if c == nil {
		return "", "", false
	}
	c.spamMu.Lock()
	defer c.spamMu.Unlock()
	return c.spamDigest.take(now)
}

// flushSpamDigest posts the pending digest to the management room it was
// accumulated from, then resets it.
func (c *MyConnector) flushSpamDigest(ctx context.Context) {
	if c == nil {
		return
	}
	text, room, ok := c.takeSpamDigest(time.Now().UTC())
	if !ok {
		return
	}
	c.log.Info().Str("room_id", string(room)).Msg("Posting anti-spam digest")
	if c.bridge == nil || c.bridge.Bot == nil || room == "" {
		return
	}
	sendNotice(c.bridge.Bot, room, text)
}

// startSpamLoop loads the persisted anti-spam state, imports the configured
// local feed files and starts the digest scheduler when digest mode is on.
func (c *MyConnector) startSpamLoop() {
	if c == nil || c.bridge == nil {
		return
	}
	cfg := c.antiSpamConfig()
	if !cfg.Enabled {
		return
	}
	ctx := context.Background()
	c.loadSpamRecent(ctx)
	c.importConfiguredSpamFeeds(ctx, cfg.Feeds.Paths)
	if cfg.Notices.NoticeMode() != AntiSpamNoticeDigest {
		return
	}
	interval := cfg.Notices.DigestInterval.Duration()
	if interval <= 0 {
		interval = defaultAntiSpamDigestInterval
	}
	c.spamMu.Lock()
	if c.spamDigestCancel != nil {
		c.spamMu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	c.spamDigestCancel = cancel
	c.spamMu.Unlock()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				c.flushSpamDigest(loopCtx)
			}
		}
	}()
	c.log.Info().Dur("interval", interval).Msg("Started anti-spam digest loop")
}

// stopSpamLoop cancels the digest scheduler.
func (c *MyConnector) stopSpamLoop() {
	if c == nil {
		return
	}
	c.spamMu.Lock()
	cancel := c.spamDigestCancel
	c.spamDigestCancel = nil
	c.spamMu.Unlock()
	if cancel != nil {
		cancel()
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

// recordSpamDecision stores one decision in the in-memory counters and recent
// mirror and, best-effort, in the persistent store. Allow decisions are
// counted but not kept in the recent ring or in the store.
func (c *MyConnector) recordSpamDecision(ctx context.Context, record spamRecord) {
	if c == nil {
		return
	}
	c.spamMu.Lock()
	c.spamCounters.add(record.Action, record.DryRun)
	c.spamLast = record.Time
	if record.Action != spamActionAllow {
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
	c.spamMu.Unlock()
	if record.Action == spamActionAllow {
		return
	}
	store, err := c.ensureSpamStore(ctx)
	if err != nil {
		return
	}
	if err := store.addDecision(ctx, record); err != nil {
		c.log.Warn().Err(err).Msg("Failed to persist anti-spam decision")
	}
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

	// The persistent store keeps first-contact rate, duplicates, decisions,
	// reputation and per-contact overrides across restarts. Without it (no
	// database), the gate degrades to zero state instead of failing.
	store, err := nc.connector.ensureSpamStore(ctx)
	if err != nil {
		nc.log.Debug().Err(err).Msg("Anti-spam state store is not available; continuing without persisted state")
		store = nil
	}
	contact := spamContactStats{}
	if store != nil {
		if stored, ok, err := store.contact(ctx, canonical); err != nil {
			nc.log.Warn().Err(err).Str("sender", canonical).Msg("Failed to load anti-spam contact history")
		} else if ok {
			contact = stored
		}
	}

	// Per-contact override first, then the global policy: allow/bridge vouch
	// for the sender, label/hold/block force the unknown-sender paths.
	vetted := known || hasSpamAllowLabel(labels, policy.AllowLabels)
	switch contact.Policy {
	case SpamContactPolicyAllow, SpamContactPolicyBridge:
		vetted = true
	case SpamContactPolicyLabel, SpamContactPolicyHold, SpamContactPolicyBlock:
		vetted = false
	}

	rateCount := 0
	if !vetted && policy.FirstContactLimit > 0 && store != nil {
		rateCount, err = store.recordRate(ctx, canonical, ts, policy.FirstContactWindow, spamStoreMaxSenders)
		if err != nil {
			nc.log.Warn().Err(err).Str("sender", canonical).Msg("Failed to record first-contact rate")
			rateCount = 0
		}
	}
	duplicateCount := 0
	if !group && store != nil {
		duplicateCount, err = store.recordDuplicate(ctx, spamBodyHash(body), canonical, ts, policy.DuplicateWindow, spamStoreMaxBodies)
		if err != nil {
			nc.log.Warn().Err(err).Str("sender", canonical).Msg("Failed to record duplicate body")
			duplicateCount = 0
		}
	}

	class := spamSenderClass(canonical, policy.HomeCountry)
	classWeight, classHold, classBlock := 0, false, false
	reputationScore := 0
	if !vetted {
		classWeight = policy.ClassWeights[class]
		_, classHold = policy.ClassHold[class]
		_, classBlock = policy.ClassBlock[class]
		reputationScore, _ = spamReputationScore(policy.Reputation, contact, ts)
	}
	feedSignals := spamFeedSignals(ctx, store, canonical, body)

	decision := evaluateSpam(policy, spamEvaluation{
		Body:            body,
		KnownContact:    vetted,
		Labels:          labels,
		Override:        contact.Policy,
		RateCount:       rateCount,
		DuplicateCount:  duplicateCount,
		SenderClass:     class,
		ClassWeight:     classWeight,
		ClassHold:       classHold,
		ClassBlock:      classBlock,
		ReputationScore: reputationScore,
		FeedSignals:     feedSignals,
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
	if store != nil {
		if err := store.recordContactMessage(ctx, canonical, ts); err != nil {
			nc.log.Warn().Err(err).Str("sender", canonical).Msg("Failed to record anti-spam contact history")
		}
	}
	return decision, nc.applySpamDecision(ctx, canonical, decision, record)
}

// spamFeedSignals matches the sender and the URLs in the body against the
// imported offline feeds. Feed entries are local data; nothing is fetched.
func spamFeedSignals(ctx context.Context, store *spamStore, sender, body string) []string {
	if store == nil {
		return nil
	}
	entries, err := store.feedEntries(ctx)
	if err != nil || len(entries) == 0 {
		return nil
	}
	return spamFeedMatches(entries, sender, body)
}

// applySpamDecision logs the decision, records counters and (unless dry-run)
// applies the ghost label, posts a management notice and reports whether the
// message must be dropped.
func (nc *MyNetworkClient) applySpamDecision(ctx context.Context, sender string, decision spamDecision, record spamRecord) bool {
	if nc == nil || nc.connector == nil {
		return false
	}
	nc.connector.recordSpamDecision(ctx, record)
	room := id.RoomID("")
	if nc.login != nil && nc.login.User != nil {
		room = nc.login.User.ManagementRoom
	}
	nc.connector.recordSpamDigest(record, room)
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
	// Dry runs do not count towards the sender's reputation: nothing was
	// enforced and the flag may be a false positive being tuned away.
	// flagged covers every enforced spam verdict (flag/hold/block); blocked
	// is the subset that was dropped.
	if decision.Action == spamActionFlag || decision.Action == spamActionHold || decision.Action == spamActionBlock {
		if store, err := nc.connector.ensureSpamStore(ctx); err == nil {
			blocked := decision.Action == spamActionBlock
			if err := store.recordContactOutcome(ctx, sender, true, blocked); err != nil {
				nc.log.Warn().Err(err).Str("sender", sender).Msg("Failed to record anti-spam contact outcome")
			}
		}
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
	if err != nil {
		return
	}
	// Digest mode batches decisions into the periodic summary; off mode
	// disables management notices entirely.
	if policy.NoticeMode != AntiSpamNoticeImmediate || !decision.Notify {
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

// spamCommandUsage is the `spam` command usage line.
const spamCommandUsage = "Usage: spam status | spam recent [count] | spam why <number> | " +
	"spam policy <number> <allow|bridge|label|hold|block|clear> | spam import <file>"

// CommandSpam reports the anti-spam configuration and counters and manages the
// persistent per-contact policies and offline feeds (ids only, never bodies).
var CommandSpam = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		connector, _ := ce.Bridge.Network.(*MyConnector)
		if connector == nil {
			ce.Reply("Network connector is not available")
			return
		}
		if len(ce.Args) == 0 || strings.EqualFold(ce.Args[0], "status") {
			ce.Reply("%s", connector.formatSpamStatus(ce.Ctx))
			return
		}
		switch strings.ToLower(strings.TrimSpace(ce.Args[0])) {
		case "recent":
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
				ce.Reply("%s", spamCommandUsage)
				return
			}
			ce.Reply("%s", connector.formatSpamRecent(count))
		case "why":
			if len(ce.Args) != 2 {
				ce.Reply("%s", spamCommandUsage)
				return
			}
			canonical := CanonicalAddress(ce.Args[1])
			if canonical == "" {
				ce.Reply("Invalid phone number or sender ID %q", ce.Args[1])
				return
			}
			ce.Reply("%s", connector.formatSpamWhy(ce.Ctx, ce.Bridge, ce.User, canonical))
		case "policy":
			if len(ce.Args) != 3 {
				ce.Reply("%s", spamCommandUsage)
				return
			}
			canonical := CanonicalAddress(ce.Args[1])
			if canonical == "" {
				ce.Reply("Invalid phone number or sender ID %q", ce.Args[1])
				return
			}
			value := strings.ToLower(strings.TrimSpace(ce.Args[2]))
			if value != SpamContactPolicyClear && !validSpamContactPolicy(value) {
				ce.Reply("%s", spamCommandUsage)
				return
			}
			store, err := connector.ensureSpamStore(ce.Ctx)
			if err != nil {
				ce.Reply("Failed to open the anti-spam state store: %v", err)
				return
			}
			storedPolicy := value
			if value == SpamContactPolicyClear {
				storedPolicy = ""
			}
			if err := store.setContactPolicy(ce.Ctx, canonical, storedPolicy); err != nil {
				ce.Reply("Failed to save the per-contact policy: %v", err)
				return
			}
			if value == SpamContactPolicyClear {
				ce.Reply("Cleared the per-contact anti-spam policy for %s.", canonical)
				return
			}
			ce.Reply("Per-contact anti-spam policy for %s: %s. Use `spam status` to list overrides.", canonical, value)
		case "import":
			if len(ce.Args) < 2 {
				ce.Reply("%s", spamCommandUsage)
				return
			}
			path := strings.TrimSpace(strings.Join(ce.Args[1:], " "))
			store, err := connector.ensureSpamStore(ce.Ctx)
			if err != nil {
				ce.Reply("Failed to open the anti-spam state store: %v", err)
				return
			}
			result, err := store.importFeedFile(ce.Ctx, path)
			if err != nil {
				ce.Reply("Failed to import feed file: %v", err)
				return
			}
			ce.Reply("Imported %d new entry/entries from %s (%d entries in the feed store). The bridge never fetches feeds itself.",
				result.Added, path, result.Total)
		default:
			ce.Reply("%s", spamCommandUsage)
		}
	},
	Name: "spam",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAdmin,
		Description: "Show the local anti-spam state, explain decisions and manage policies/feeds (ids only)",
		Args:        "status | recent [count] | why <number> | policy <number> <policy> | import <file>",
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

// formatSpamStatus renders the effective configuration, counters and the
// persistent state summary (overrides, feed entries, pending digest).
func (c *MyConnector) formatSpamStatus(ctx context.Context) string {
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
		fmt.Sprintf("Sender classes: home country %s; weights shortcode %d, toll-free %d, alphanumeric %d, international %d; hold: %s; block: %s",
			firstNotEmpty(cfg.SenderClasses.HomeCountry, "unset"),
			cfg.SenderClasses.ShortcodeWeight, cfg.SenderClasses.TollFreeWeight,
			cfg.SenderClasses.AlphanumericWeight, cfg.SenderClasses.InternationalWeight,
			formatSpamList(cfg.SenderClasses.HoldClasses), formatSpamList(cfg.SenderClasses.BlockClasses)),
		fmt.Sprintf("Reputation: weight %d per factor (flag %d, block %d, volume above %d msg/day, cap %d)",
			cfg.Reputation.Weight, cfg.Reputation.FlagWeight, cfg.Reputation.BlockWeight,
			cfg.Reputation.VolumeThreshold, cfg.Reputation.MaxScore),
		fmt.Sprintf("Feeds: %d configured path(s), weight %d", len(cfg.Feeds.Paths), cfg.Feeds.Weight),
		fmt.Sprintf("Management notices: mode %s (digest interval %s)",
			cfg.Notices.NoticeMode(), formatTTLDuration(cfg.Notices.DigestInterval.Duration())),
		fmt.Sprintf("Decisions: %d evaluated — %d allowed, %d labeled, %d flagged, %d held, %d blocked (%d dry run)",
			counters.Evaluated, counters.Allowed, counters.Labeled, counters.Flagged, counters.Held, counters.Blocked, counters.DryRun),
	}
	if pending := c.spamDigestPending(); pending > 0 {
		lines = append(lines, fmt.Sprintf("Pending digest decisions: %d", pending))
	}
	if store, err := c.ensureSpamStore(ctx); err == nil {
		if entries, err := store.feedEntries(ctx); err == nil && len(entries) > 0 {
			lines = append(lines, fmt.Sprintf("Feed store: %d entr(ies) imported", len(entries)))
		}
		if overrides, err := store.contactPolicies(ctx); err == nil && len(overrides) > 0 {
			lines = append(lines, fmt.Sprintf("Per-contact policies: %d override(s)", len(overrides)))
			for index, override := range overrides {
				if index >= spamStatusOverrideLimit {
					lines = append(lines, fmt.Sprintf("- … and %d more (use `spam why <number>`)", len(overrides)-index))
					break
				}
				lines = append(lines, fmt.Sprintf("- %s: %s", override.Sender, override.Policy))
			}
		}
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

// spamStatusOverrideLimit bounds how many per-contact overrides `spam status`
// lists inline.
const spamStatusOverrideLimit = 20

// spamWhyReport is the collected explanation for one sender.
type spamWhyReport struct {
	Sender             string
	Class              string
	ClassWeight        int
	Override           string
	Labels             []string
	AllowLabel         string
	Known              bool
	RateCount          int
	RateLimit          int
	RateWindow         time.Duration
	DuplicateHashes    int
	DuplicateMax       int
	DuplicateThreshold int
	DuplicateWindow    time.Duration
	Contact            spamContactStats
	ContactFound       bool
	ReputationScore    int
	FeedSignals        []string
	FeedWeight         int
	Recent             []spamRecord
	Verdict            spamDecision
}

// formatSpamWhy explains the current verdict for one sender: labels,
// allowlist source, per-contact override, sender class, rate/duplicate state,
// reputation, feed matches and the recent decisions (ids only).
func (c *MyConnector) formatSpamWhy(ctx context.Context, br *bridgev2.Bridge, user *bridgev2.User, sender string) string {
	cfg := c.antiSpamConfig()
	report := spamWhyReport{Sender: sender, Class: spamSenderClass(sender, cfg.SenderClasses.HomeCountry)}
	if !cfg.Enabled {
		return fmt.Sprintf("Anti-spam is disabled; %s is never evaluated.", sender)
	}
	policy, err := compileSpamPolicy(cfg)
	if err != nil {
		return fmt.Sprintf("Anti-spam configuration is invalid: %v", err)
	}
	report.ClassWeight = policy.ClassWeights[report.Class]

	if store, err := c.ensureSpamStore(ctx); err == nil {
		if contact, ok, err := store.contact(ctx, sender); err == nil {
			report.Contact, report.ContactFound = contact, ok
			report.Override = contact.Policy
		}
	}
	report.Labels = c.connectorGhostLabels(ctx, br, sender)
	report.AllowLabel = spamAllowLabelMatch(report.Labels, policy.AllowLabels)
	if br != nil {
		if portal, err := br.GetExistingPortalByKey(ctx, networkid.PortalKey{ID: MakeAddressPortalID(sender)}); err == nil && portal != nil {
			report.Known = true
		}
		if !report.Known && user != nil {
			if login := user.GetDefaultLogin(); login != nil {
				if dm, err := br.GetDMPortal(ctx, login.ID, MakeUserID(sender)); err == nil && dm != nil {
					report.Known = true
				}
			}
		}
	}
	vetted := report.Known || report.AllowLabel != ""
	switch report.Override {
	case SpamContactPolicyAllow, SpamContactPolicyBridge:
		vetted = true
	case SpamContactPolicyLabel, SpamContactPolicyHold, SpamContactPolicyBlock:
		vetted = false
	}
	now := time.Now().UTC()
	report.RateLimit = policy.FirstContactLimit
	report.RateWindow = policy.FirstContactWindow
	report.DuplicateThreshold = policy.DuplicateCount
	report.DuplicateWindow = policy.DuplicateWindow
	if store, err := c.ensureSpamStore(ctx); err == nil {
		if count, err := store.rateCount(ctx, sender, now.Add(-policy.FirstContactWindow)); err == nil {
			report.RateCount = count
		}
		if hashes, maxSenders, err := store.duplicateState(ctx, sender, now.Add(-policy.DuplicateWindow)); err == nil {
			report.DuplicateHashes, report.DuplicateMax = hashes, maxSenders
		}
		report.FeedSignals = spamFeedSignals(ctx, store, sender, "")
	}
	report.FeedWeight = policy.FeedWeight
	report.Recent = c.spamRecentForSender(ctx, sender, spamWhyRecentLimit)
	classWeight, classHold, classBlock := 0, false, false
	reputationScore := 0
	if !vetted {
		classWeight = report.ClassWeight
		_, classHold = policy.ClassHold[report.Class]
		_, classBlock = policy.ClassBlock[report.Class]
		reputationScore, _ = spamReputationScore(policy.Reputation, report.Contact, now)
	}
	report.ReputationScore = reputationScore
	report.Verdict = evaluateSpam(policy, spamEvaluation{
		KnownContact:    vetted,
		Override:        report.Override,
		RateCount:       report.RateCount,
		DuplicateCount:  report.DuplicateMax,
		SenderClass:     report.Class,
		ClassWeight:     classWeight,
		ClassHold:       classHold,
		ClassBlock:      classBlock,
		ReputationScore: reputationScore,
		FeedSignals:     report.FeedSignals,
	})
	return formatSpamWhyReport(report)
}

// spamWhyRecentLimit is how many recent decisions `spam why` shows.
const spamWhyRecentLimit = 5

// connectorGhostLabels loads ghost labels without a network client.
func (c *MyConnector) connectorGhostLabels(ctx context.Context, br *bridgev2.Bridge, sender string) []string {
	if c == nil || br == nil || sender == "" {
		return nil
	}
	ghost, err := br.GetExistingGhostByID(ctx, MakeUserID(sender))
	if err != nil || ghost == nil {
		return nil
	}
	meta, _ := ghost.Metadata.(*GhostMetadata)
	if meta == nil {
		return nil
	}
	return append([]string(nil), meta.Labels...)
}

// spamRecentForSender returns the newest decisions for one sender, from the
// persistent store when available and the in-memory mirror otherwise.
func (c *MyConnector) spamRecentForSender(ctx context.Context, sender string, limit int) []spamRecord {
	if c == nil || limit <= 0 {
		return nil
	}
	if store, err := c.ensureSpamStore(ctx); err == nil {
		if records, err := store.recentDecisionsForSender(ctx, sender, limit); err == nil {
			return records
		}
	}
	_, recent, _ := c.spamSnapshot()
	records := make([]spamRecord, 0, limit)
	for i := len(recent) - 1; i >= 0 && len(records) < limit; i-- {
		if recent[i].Sender == sender {
			records = append(records, recent[i])
		}
	}
	return records
}

// spamAllowLabelMatch returns the allowlist label carried by the ghost, or "".
func spamAllowLabelMatch(labels []string, allow map[string]struct{}) string {
	for _, label := range labels {
		normalized := strings.ToLower(strings.TrimSpace(label))
		if _, ok := allow[normalized]; ok {
			return label
		}
	}
	return ""
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
