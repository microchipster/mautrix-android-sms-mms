// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"maunium.net/go/mautrix/id"
)

// spamSenderClass classifies a canonical sender address. homeCountry is an
// E.164 calling code without the "+" ("1", "49"); an empty value disables the
// international/home distinction. The class is informational for vetted
// senders and scored for unknown ones.
func spamSenderClass(sender, homeCountry string) string {
	if sender == "" || strings.Contains(sender, "@") {
		return ""
	}
	if !strings.HasPrefix(sender, "+") {
		digits := true
		for _, r := range sender {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if digits && len(sender) >= 3 && len(sender) <= 6 {
			return SpamSenderClassShortcode
		}
		return SpamSenderClassAlphanumeric
	}
	number := strings.TrimPrefix(sender, "+")
	if isSpamTollFreeNumber(number) {
		return SpamSenderClassTollFree
	}
	if home := normalizeSpamHomeCountry(homeCountry); home != "" && !strings.HasPrefix(number, home) {
		return SpamSenderClassInternational
	}
	return SpamSenderClassHome
}

// isSpamTollFreeNumber reports whether an E.164 number without "+" is a NANP
// toll-free number.
func isSpamTollFreeNumber(number string) bool {
	if len(number) != 11 || number[0] != '1' {
		return false
	}
	for _, r := range number {
		if r < '0' || r > '9' {
			return false
		}
	}
	switch number[1:4] {
	case "800", "833", "844", "855", "866", "877", "888":
		return true
	default:
		return false
	}
}

// hasSpamFeedURLPrefix reports whether a feed token looks like a URL rather
// than a bare host or sender.
func hasSpamFeedURLPrefix(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "://") || strings.HasPrefix(lower, "www.")
}

// parseSpamFeedLine extracts one entry from a plain line-based feed file:
// a bare domain, a URL (reduced to its host), or a phone number/short code.
// Blank lines and lines starting with # or ; are ignored; comma-separated
// fields are accepted and the first URL field wins over numeric ids, so
// URLhaus-style export lines work.
func parseSpamFeedLine(line string) (string, bool) {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
		return "", false
	}
	if idx := strings.Index(line, " #"); idx >= 0 {
		line = line[:idx]
	}
	candidate := ""
	for _, field := range strings.Split(line, ",") {
		field = strings.Trim(strings.TrimSpace(field), `"'`)
		if field == "" {
			continue
		}
		if hasSpamFeedURLPrefix(field) {
			candidate = field
			break
		}
		if candidate == "" {
			candidate = field
		}
	}
	if candidate == "" {
		return "", false
	}
	if hasSpamFeedURLPrefix(candidate) {
		candidate = spamURLHost(candidate)
	}
	candidate = strings.Trim(candidate, `"'<>()[]`)
	candidate = strings.TrimPrefix(candidate, "*.")
	if isPhoneLike(candidate) {
		candidate = CanonicalAddress(candidate)
	} else {
		if idx := strings.IndexAny(candidate, " \t"); idx >= 0 {
			candidate = candidate[:idx]
		}
		candidate = strings.TrimPrefix(candidate, "www.")
		candidate = strings.TrimSuffix(candidate, ".")
		candidate = strings.ToLower(candidate)
	}
	if candidate == "" {
		return "", false
	}
	return candidate, true
}

// spamFeedMatches matches a sender address and the URLs in a body against the
// imported feed entries. Domain entries match subdomains; one signal per kind
// is returned, sorted for stable output.
func spamFeedMatches(entries map[string]string, sender, body string) []string {
	if len(entries) == 0 {
		return nil
	}
	found := make(map[string]struct{}, 2)
	if sender != "" {
		if _, ok := entries[sender]; ok {
			found["sender"] = struct{}{}
		}
	}
	for _, host := range spamURLHosts(body) {
		if spamFeedHostMatch(entries, host) {
			found["url"] = struct{}{}
			break
		}
	}
	if len(found) == 0 {
		return nil
	}
	out := make([]string, 0, len(found))
	for signal := range found {
		out = append(out, signal)
	}
	sort.Strings(out)
	return out
}

// spamFeedHostMatch checks a host and all of its parent domains against the
// feed entries.
func spamFeedHostMatch(entries map[string]string, host string) bool {
	for host != "" {
		if _, ok := entries[host]; ok {
			return true
		}
		idx := strings.IndexByte(host, '.')
		if idx < 0 {
			return false
		}
		host = host[idx+1:]
	}
	return false
}

// spamContactStats is one sender's persisted own-history counters.
type spamContactStats struct {
	Sender    string
	FirstSeen time.Time
	LastSeen  time.Time
	Total     int
	Flagged   int
	Blocked   int
	Policy    string
}

// messagesPerDay returns the average message volume since first contact,
// counting the first day as a full day so a burst today is not diluted.
func (s spamContactStats) messagesPerDay(now time.Time) float64 {
	if s.Total <= 0 || s.FirstSeen.IsZero() {
		return 0
	}
	days := now.Sub(s.FirstSeen).Hours() / 24
	if days < 1 {
		days = 1
	}
	return float64(s.Total) / days
}

// spamReputationScore computes the score contributed by the bridge's own
// history with a sender: flagged/blocked counters plus high message volume,
// multiplied by the configured weight and capped by max_score.
func spamReputationScore(cfg AntiSpamReputationConfig, stats spamContactStats, now time.Time) (score, factor int) {
	if cfg.Weight <= 0 {
		return 0, 0
	}
	factor = stats.Flagged*cfg.FlagWeight + stats.Blocked*cfg.BlockWeight
	if cfg.VolumeThreshold > 0 {
		if excess := int(stats.messagesPerDay(now)) - cfg.VolumeThreshold; excess > 0 {
			factor += excess
		}
	}
	if factor <= 0 {
		return 0, 0
	}
	score = factor * cfg.Weight
	if cfg.MaxScore > 0 && score > cfg.MaxScore {
		score = cfg.MaxScore
	}
	return score, factor
}

// spamDigestState accumulates decisions between digest flushes.
type spamDigestState struct {
	Since   time.Time
	Room    id.RoomID
	Total   int
	DryRun  int
	Counts  map[string]int
	Reasons map[string]int
	Senders map[string]int
}

// add folds one decision into the digest. Allowed decisions count towards the
// total but contribute neither reasons nor senders.
func (d *spamDigestState) add(record spamRecord, room id.RoomID) {
	if d == nil {
		return
	}
	if d.Counts == nil {
		d.Counts = make(map[string]int)
		d.Reasons = make(map[string]int)
		d.Senders = make(map[string]int)
	}
	if d.Since.IsZero() {
		d.Since = record.Time
	}
	if room != "" {
		d.Room = room
	}
	d.Total++
	d.Counts[record.Action]++
	if record.DryRun {
		d.DryRun++
	}
	if record.Action == spamActionAllow {
		return
	}
	for _, reason := range record.Reasons {
		d.Reasons[reason]++
	}
	d.Senders[record.Sender]++
}

// take renders the accumulated digest, resets the state and reports whether
// anything was pending. The returned text only carries counts, pattern names
// and sender ids, never message bodies.
func (d *spamDigestState) take(now time.Time) (string, id.RoomID, bool) {
	if d == nil || d.Total == 0 {
		return "", "", false
	}
	text := formatSpamDigest(d, now)
	room := d.Room
	*d = spamDigestState{}
	return text, room, true
}

// spamDigestTopN bounds the "top reasons" and "senders" lists in a digest.
const spamDigestTopN = 5

// formatSpamDigest renders one digest summary line set.
func formatSpamDigest(d *spamDigestState, now time.Time) string {
	if d == nil || d.Total == 0 {
		return ""
	}
	since := "startup"
	if !d.Since.IsZero() {
		window := now.Sub(d.Since)
		if window < 0 {
			window = 0
		}
		since = fmt.Sprintf("the last %s", formatTTLDuration(window.Round(time.Second)))
	}
	lines := []string{fmt.Sprintf(
		"Anti-spam digest: %d decision(s) in %s — %d allowed, %d labeled, %d flagged, %d held, %d blocked (%d dry run).",
		d.Total, since,
		d.Counts[spamActionAllow], d.Counts[spamActionLabel], d.Counts[spamActionFlag],
		d.Counts[spamActionHold], d.Counts[spamActionBlock], d.DryRun)}
	if reasons := formatSpamTopCounter(d.Reasons, spamDigestTopN); reasons != "" {
		lines = append(lines, fmt.Sprintf("Top reasons: %s.", reasons))
	}
	if senders := formatSpamTopCounter(d.Senders, spamDigestTopN); senders != "" {
		lines = append(lines, fmt.Sprintf("Senders: %s.", senders))
	}
	lines = append(lines, "Use `spam recent` for details.")
	return strings.Join(lines, "\n")
}

// formatSpamTopCounter renders the top entries of a counter map, ties broken
// by name for stable output.
func formatSpamTopCounter(counters map[string]int, limit int) string {
	if len(counters) == 0 || limit <= 0 {
		return ""
	}
	type entry struct {
		Name  string
		Count int
	}
	entries := make([]entry, 0, len(counters))
	for name, count := range counters {
		entries = append(entries, entry{Name: name, Count: count})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count != entries[j].Count {
			return entries[i].Count > entries[j].Count
		}
		return entries[i].Name < entries[j].Name
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	parts := make([]string, 0, len(entries))
	for _, item := range entries {
		parts = append(parts, fmt.Sprintf("%s (%d)", item.Name, item.Count))
	}
	return strings.Join(parts, ", ")
}

// formatSpamWhyReport renders the `spam why` explanation (ids only).
func formatSpamWhyReport(report spamWhyReport) string {
	lines := []string{fmt.Sprintf("Anti-spam why %s:", report.Sender)}
	if report.Class != "" {
		lines = append(lines, fmt.Sprintf("- Sender class: %s%s", report.Class, formatSpamWhyWeight(report.ClassWeight)))
	} else {
		lines = append(lines, "- Sender class: unclassified")
	}
	if report.Override != "" {
		lines = append(lines, fmt.Sprintf("- Per-contact policy: %s (source: `spam policy`)", report.Override))
	} else {
		lines = append(lines, "- Per-contact policy: none")
	}
	if len(report.Labels) > 0 {
		lines = append(lines, fmt.Sprintf("- Labels: %s", strings.Join(report.Labels, ", ")))
	} else {
		lines = append(lines, "- Labels: none")
	}
	if report.AllowLabel != "" {
		lines = append(lines, fmt.Sprintf("- Allowlist: %q matches allow_labels", report.AllowLabel))
	} else {
		lines = append(lines, "- Allowlist: no matching label")
	}
	lines = append(lines, fmt.Sprintf("- Known contact: %s", yesNo(report.Known)))
	if report.RateLimit > 0 {
		lines = append(lines, fmt.Sprintf("- First contact: %d message(s) in the last %s (limit %d)",
			report.RateCount, formatTTLDuration(report.RateWindow), report.RateLimit))
	} else {
		lines = append(lines, "- First contact: limiter disabled")
	}
	if report.DuplicateThreshold > 0 {
		lines = append(lines, fmt.Sprintf("- Duplicates: %d body hash(es) from this sender in the last %s; highest distinct-sender count %d (threshold %d)",
			report.DuplicateHashes, formatTTLDuration(report.DuplicateWindow), report.DuplicateMax, report.DuplicateThreshold))
	} else {
		lines = append(lines, "- Duplicates: detection disabled")
	}
	if report.ContactFound {
		lines = append(lines, fmt.Sprintf("- History: first seen %s, last seen %s, %d message(s), %d flagged, %d blocked (%.1f/day)",
			report.Contact.FirstSeen.Format("2006-01-02 15:04 MST"),
			report.Contact.LastSeen.Format("2006-01-02 15:04 MST"),
			report.Contact.Total, report.Contact.Flagged, report.Contact.Blocked,
			report.Contact.messagesPerDay(time.Now().UTC())))
	} else {
		lines = append(lines, "- History: no recorded messages")
	}
	if report.ReputationScore > 0 {
		lines = append(lines, fmt.Sprintf("- Reputation: contributes %d to the score", report.ReputationScore))
	} else {
		lines = append(lines, "- Reputation: contributes 0 to the score")
	}
	if len(report.FeedSignals) > 0 {
		lines = append(lines, fmt.Sprintf("- Offline feeds: matched %s (weight %d per signal)",
			strings.Join(report.FeedSignals, ", "), report.FeedWeight))
	} else {
		lines = append(lines, "- Offline feeds: no match")
	}
	if len(report.Recent) == 0 {
		lines = append(lines, "- Recent decisions: none")
	} else {
		for _, record := range report.Recent {
			reasons := strings.Join(record.Reasons, ", ")
			if reasons == "" {
				reasons = "none"
			}
			mode := ""
			if record.DryRun {
				mode = " [dry run]"
			}
			lines = append(lines, fmt.Sprintf("- Recent: %s %s%s -> %s (score %d; %s)",
				record.Time.Format("2006-01-02 15:04:05 MST"), record.Action, mode, record.Sender, record.Score, reasons))
		}
	}
	verdictReasons := strings.Join(report.Verdict.Reasons, ", ")
	if verdictReasons == "" {
		verdictReasons = "none"
	}
	lines = append(lines, fmt.Sprintf("- Verdict for a new empty message: %s (score %d; reasons: %s)",
		report.Verdict.Action, report.Verdict.Score, verdictReasons))
	return strings.Join(lines, "\n")
}

// formatSpamWhyWeight renders a sender-class weight for `spam why`.
func formatSpamWhyWeight(weight int) string {
	if weight <= 0 {
		return " (no weight)"
	}
	return fmt.Sprintf(" (weight %d for unknown senders)", weight)
}

// yesNo renders a bool for the management room.
func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
