// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Provider reads and writes the Android telephony state for one device.
type Provider interface {
	Threads(ctx context.Context) ([]Thread, error)
	ThreadRecipients(ctx context.Context, threadID int64) []string
	ThreadMessages(ctx context.Context, threadID int64, beforeMS, afterMS int64, limit int) ([]Message, error)
	Message(ctx context.Context, messageID string) (Message, error)
	PartBytes(ctx context.Context, mmsID, partID int64) ([]byte, string, error)
	MarkRead(ctx context.Context, threadID int64, upToMS int64) (int, error)
	SweepSMS(ctx context.Context, sinceMS int64) ([]Message, error)
	SweepMMS(ctx context.Context, sinceMS int64) ([]Message, error)
	Subscriptions(ctx context.Context) ([]Subscription, error)
	Root() bool
}

// ContentProvider implements Provider using `content query|read|update` over ADB.
// This works without root (com.android.shell holds telephony permissions on AOSP).
type ContentProvider struct {
	adb        *Adb
	root       bool
	ownNumbers map[string]struct{}
	subIDs     map[int]struct{}

	addrMu    sync.Mutex
	addrCache map[int64]string
}

// NewContentProvider creates a provider for the given adb device.
func NewContentProvider(adb *Adb, root bool, ownNumbers []string, subIDs []int) *ContentProvider {
	own := make(map[string]struct{}, len(ownNumbers))
	for _, n := range ownNumbers {
		own[CanonicalAddress(n)] = struct{}{}
	}
	subs := make(map[int]struct{}, len(subIDs))
	for _, s := range subIDs {
		subs[s] = struct{}{}
	}
	return &ContentProvider{adb: adb, root: root, ownNumbers: own, subIDs: subs, addrCache: map[int64]string{}}
}

// Root reports whether the provider runs with root privileges.
func (p *ContentProvider) Root() bool { return p.root }

// Content query projections. Ambiguous columns (body/snippet/text) go last:
// `content query` does not escape values, so only the last column may contain ", ".
var (
	smsProjection   = []string{"_id", "thread_id", "address", "date", "date_sent", "type", "read", "seen", "status", "sub_id", "body"}
	threadProjection = []string{"_id", "date", "message_count", "recipient_ids", "read", "has_attachment", "snippet"}
	mmsProjection   = []string{"_id", "thread_id", "date", "msg_box", "m_type", "m_size", "m_id", "sub", "sub_id", "read", "seen", "ct_t", "text_only"}
	partProjection  = []string{"_id", "mid", "seq", "ct", "name", "fn", "cid", "cl", "_data", "text"}
	addrProjection  = []string{"address", "type", "charset"}
)

func (p *ContentProvider) query(ctx context.Context, uri string, projection []string, where, sortBy string, limit int) ([]map[string]string, error) {
	script := "content query --uri " + ShellQuote(uri) +
		" --projection " + ShellQuote(strings.Join(projection, ":"))
	if where != "" {
		script += " --where " + ShellQuote(where)
	}
	if sortBy != "" {
		script += " --sort " + ShellQuote(sortBy)
	}
	// The `content` CLI has no --limit (verified on LineageOS 21 / Android 14:
	// passing it prints usage and returns nothing), so limits are applied
	// client-side. Only newest-first (DESC) and single-row queries are
	// truncated; ascending sweeps are bounded by their date windows instead.
	out, err := p.adb.Shell(ctx, script)
	if err != nil {
		return nil, err
	}
	if strings.Contains(out, "Error:") || strings.Contains(out, "Exception") {
		return nil, fmt.Errorf("content query failed for %s: %s", uri, strings.TrimSpace(out))
	}
	rows := ParseContentQuery(out, projection)
	if limit > 0 && len(rows) > limit && (sortBy == "" || strings.Contains(strings.ToUpper(sortBy), "DESC")) {
		rows = rows[:limit]
	}
	return rows, nil
}

func (p *ContentProvider) allowedSub(subID int) bool {
	if len(p.subIDs) == 0 {
		return true
	}
	_, ok := p.subIDs[subID]
	return ok
}

func (p *ContentProvider) isOwn(address string) bool {
	_, ok := p.ownNumbers[CanonicalAddress(address)]
	return ok
}

// canonicalAddresses resolves canonical_addresses row IDs (with cache).
func (p *ContentProvider) canonicalAddresses(ctx context.Context, ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, canonicalID := range ids {
		p.addrMu.Lock()
		address, ok := p.addrCache[canonicalID]
		p.addrMu.Unlock()
		if !ok {
			rows, err := p.query(ctx, fmt.Sprintf("content://mms-sms/canonical-address/%d", canonicalID), []string{"address"}, "", "", 0)
			if err != nil || len(rows) == 0 {
				continue
			}
			address = fieldString(rows[0], "address")
			p.addrMu.Lock()
			p.addrCache[canonicalID] = address
			p.addrMu.Unlock()
		}
		if address != "" && !p.isOwn(address) {
			out = append(out, CanonicalAddress(address))
		}
	}
	return out
}

// Threads lists all conversations.
func (p *ContentProvider) Threads(ctx context.Context) ([]Thread, error) {
	rows, err := p.query(ctx, "content://mms-sms/conversations?simple=true", threadProjection, "", "date DESC", 0)
	if err != nil {
		return nil, err
	}
	threads := make([]Thread, 0, len(rows))
	for _, row := range rows {
		thread := Thread{
			ID:            fieldInt64(row, "_id"),
			DateMS:        fieldInt64(row, "date"),
			MessageCount:  int(fieldInt64(row, "message_count")),
			Snippet:       fieldString(row, "snippet"),
			Read:          fieldBool(row, "read"),
			HasAttachment: fieldBool(row, "has_attachment"),
			SubID:         -1,
		}
		thread.Recipients = p.canonicalAddresses(ctx, splitSpaceList(fieldString(row, "recipient_ids")))
		threads = append(threads, thread)
	}
	return threads, nil
}

// ThreadRecipients returns the participant set for a thread (canonical, own numbers excluded).
func (p *ContentProvider) ThreadRecipients(ctx context.Context, threadID int64) []string {
	rows, err := p.query(ctx, "content://mms-sms/conversations?simple=true", threadProjection,
		"_id="+strconv.FormatInt(threadID, 10), "", 1)
	if err != nil || len(rows) == 0 {
		return nil
	}
	return p.canonicalAddresses(ctx, splitSpaceList(fieldString(rows[0], "recipient_ids")))
}

// smsMessages converts sms rows to Messages.
func (p *ContentProvider) smsMessages(ctx context.Context, rows []map[string]string) []Message {
	out := make([]Message, 0, len(rows))
	for _, row := range rows {
		subID := int(fieldInt64(row, "sub_id"))
		if subID < 0 {
			subID = -1
		}
		if !p.allowedSub(subID) {
			continue
		}
		id := fieldInt64(row, "_id")
		msgType := int(fieldInt64(row, "type"))
		address := CanonicalAddress(fieldString(row, "address"))
		msg := Message{
			ID:              fmt.Sprintf("sms:%d:%d", subID, id),
			ThreadID:        fieldInt64(row, "thread_id"),
			SubID:           subID,
			TimestampMS:     fieldInt64(row, "date"),
			TimestampSentMS: fieldInt64(row, "date_sent"),
			Type:            msgType,
			Read:            fieldBool(row, "read"),
			Seen:            fieldBool(row, "seen"),
			Status:          int(fieldInt64(row, "status")),
			Body:            fieldString(row, "body"),
		}
		if msgType == 1 && address != "" && !p.isOwn(address) {
			msg.Sender = address
		}
		out = append(out, msg)
	}
	return out
}

// SweepSMS returns sms rows newer than sinceMS (inbox + sent, failed/queued included
// for status updates).
func (p *ContentProvider) SweepSMS(ctx context.Context, sinceMS int64) ([]Message, error) {
	rows, err := p.query(ctx, "content://sms", smsProjection,
		fmt.Sprintf("date > %d", sinceMS), "date ASC", 0)
	if err != nil {
		return nil, err
	}
	return p.smsMessages(ctx, rows), nil
}

// mmsMessages converts pdu rows to Messages (notification rows are excluded).
func (p *ContentProvider) mmsMessages(ctx context.Context, rows []map[string]string) []Message {
	out := make([]Message, 0, len(rows))
	for _, row := range rows {
		subID := int(fieldInt64(row, "sub_id"))
		if subID < 0 {
			subID = -1
		}
		if !p.allowedSub(subID) {
			continue
		}
		id := fieldInt64(row, "_id")
		msgType := fieldInt64(row, "m_type")
		// 128 SEND_REQ, 130 NOTIF_IND, 131 NOTIF_RESP, 132 RETRIEVE_CONF, 134 DELIVERY_IND.
		if msgType == 130 || msgType == 131 || msgType == 128 || msgType == 134 {
			continue
		}
		out = append(out, Message{
			ID:          fmt.Sprintf("mms:%d:%d", subID, id),
			ThreadID:    fieldInt64(row, "thread_id"),
			SubID:       subID,
			TimestampMS: fieldInt64(row, "date") * 1000,
			Type:        int(fieldInt64(row, "msg_box")),
			Read:        fieldBool(row, "read"),
			Seen:        fieldBool(row, "seen"),
			IsMMS:       true,
			TextOnly:    fieldBool(row, "text_only"),
		})
	}
	return out
}

// SweepMMS returns mms rows newer than sinceMS (seconds precision; the caller passes ms).
func (p *ContentProvider) SweepMMS(ctx context.Context, sinceMS int64) ([]Message, error) {
	rows, err := p.query(ctx, "content://mms", mmsProjection,
		fmt.Sprintf("date > %d", sinceMS/1000), "date ASC", 0)
	if err != nil {
		return nil, err
	}
	return p.mmsMessages(ctx, rows), nil
}

// mmsDetail loads addr + part rows for one pdu.
func (p *ContentProvider) mmsDetail(ctx context.Context, mmsID int64) (Message, error) {
	rows, err := p.query(ctx, "content://mms", mmsProjection,
		"_id="+strconv.FormatInt(mmsID, 10), "", 1)
	if err != nil {
		return Message{}, err
	}
	msgs := p.mmsMessages(ctx, rows)
	if len(msgs) == 0 {
		return Message{}, fmt.Errorf("mms %d not found", mmsID)
	}
	msg := msgs[0]

	addrRows, err := p.query(ctx, fmt.Sprintf("content://mms/%d/addr", mmsID), addrProjection, "", "", 0)
	if err != nil {
		return Message{}, err
	}
	var recipients []string
	for _, row := range addrRows {
		address := CanonicalAddress(fieldString(row, "address"))
		if address == "" || p.isOwn(address) {
			continue
		}
		switch fieldInt64(row, "type") {
		case 137: // FROM
			if msg.Type == 1 {
				msg.Sender = address
			} else {
				recipients = append(recipients, address)
			}
		case 151, 130: // TO, CC
			recipients = appendUnique(recipients, address)
		}
	}
	if msg.Sender != "" {
		recipients = removeAddress(recipients, msg.Sender)
	}
	sort.Strings(recipients)
	msg.Recipients = recipients

	partRows, err := p.query(ctx, fmt.Sprintf("content://mms/%d/part", mmsID), partProjection, "", "seq ASC", 0)
	if err != nil {
		return Message{}, err
	}
	for _, row := range partRows {
		contentType := strings.ToLower(fieldString(row, "ct"))
		partID := fieldInt64(row, "_id")
		if contentType == "text/plain" {
			if msg.Body == "" {
				msg.Body = fieldString(row, "text")
			}
			continue
		}
		if contentType == "application/smil" {
			continue
		}
		fileName := firstNonEmpty(fieldString(row, "name"), fieldString(row, "fn"), fieldString(row, "cl"), fieldString(row, "cid"))
		attachment := Attachment{
			PartID:      partID,
			ContentType: contentType,
			FileName:    fileName,
			URL:         fmt.Sprintf("/v1/mms/%d/parts/%d", mmsID, partID),
		}
		msg.Attachments = append(msg.Attachments, attachment)
	}
	if len(msg.Attachments) == 0 {
		msg.TextOnly = true
	}
	return msg, nil
}

// Message loads one message by its wire ID.
func (p *ContentProvider) Message(ctx context.Context, messageID string) (Message, error) {
	kind, _, rowID, err := ParseMessageID(messageID)
	if err != nil {
		return Message{}, err
	}
	switch kind {
	case "sms":
		rows, err := p.query(ctx, "content://sms", smsProjection, "_id="+strconv.FormatInt(rowID, 10), "", 1)
		if err != nil {
			return Message{}, err
		}
		msgs := p.smsMessages(ctx, rows)
		if len(msgs) == 0 {
			return Message{}, fmt.Errorf("sms %d not found", rowID)
		}
		msg := msgs[0]
		msg.Recipients = p.threadParticipantsForMessage(ctx, msg)
		return msg, nil
	case "mms":
		msg, err := p.mmsDetail(ctx, rowID)
		if err != nil {
			return Message{}, err
		}
		if len(msg.Recipients) == 0 {
			msg.Recipients = p.threadParticipantsForMessage(ctx, msg)
		}
		return msg, nil
	default:
		return Message{}, fmt.Errorf("invalid message id %q", messageID)
	}
}

// threadParticipantsForMessage fills recipients from the thread when the message
// itself has none (SMS and outgoing MMS).
func (p *ContentProvider) threadParticipantsForMessage(ctx context.Context, msg Message) []string {
	recipients := p.ThreadRecipients(ctx, msg.ThreadID)
	if msg.Sender != "" {
		recipients = removeAddress(recipients, msg.Sender)
	}
	return recipients
}

// ThreadMessages loads messages for one thread (oldest first).
func (p *ContentProvider) ThreadMessages(ctx context.Context, threadID int64, beforeMS, afterMS int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 200
	}
	where := "thread_id=" + strconv.FormatInt(threadID, 10)
	if beforeMS > 0 {
		where += fmt.Sprintf(" AND date < %d", beforeMS/1000)
	}
	if afterMS > 0 {
		where += fmt.Sprintf(" AND date >= %d", afterMS/1000)
	}
	pduRows, err := p.query(ctx, "content://mms", mmsProjection, where, "date DESC", limit)
	if err != nil {
		return nil, err
	}
	pdus := p.mmsMessages(ctx, pduRows)

	whereSMS := "thread_id=" + strconv.FormatInt(threadID, 10)
	if beforeMS > 0 {
		whereSMS += fmt.Sprintf(" AND date < %d", beforeMS)
	}
	if afterMS > 0 {
		whereSMS += fmt.Sprintf(" AND date >= %d", afterMS)
	}
	smsRows, err := p.query(ctx, "content://sms", smsProjection, whereSMS, "date DESC", limit)
	if err != nil {
		return nil, err
	}
	smss := p.smsMessages(ctx, smsRows)

	recipients := p.ThreadRecipients(ctx, threadID)
	out := make([]Message, 0, len(pdus)+len(smss))
	for _, msg := range append(pdus, smss...) {
		if msg.Sender != "" {
			msg.Recipients = removeAddress(recipients, msg.Sender)
		} else {
			msg.Recipients = recipients
		}
		out = append(out, msg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimestampMS < out[j].TimestampMS })
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// PartBytes streams one MMS part.
func (p *ContentProvider) PartBytes(ctx context.Context, mmsID, partID int64) ([]byte, string, error) {
	rows, err := p.query(ctx, fmt.Sprintf("content://mms/%d/part", mmsID), partProjection, "_id="+strconv.FormatInt(partID, 10), "", 1)
	if err != nil {
		return nil, "", err
	}
	if len(rows) == 0 {
		return nil, "", fmt.Errorf("part %d not found", partID)
	}
	contentType := fieldString(rows[0], "ct")
	path := fieldString(rows[0], "_data")
	// `content read` is the only byte-exact path. Do NOT use `su -c "cat"` for
	// binary data: Magisk's su runs the command in a pty that translates line
	// feeds to CRLF, appending one byte per 0x0A byte (observed +159 bytes on a
	// 46 KB JPEG on LineageOS 21). Plain `cat` (without su) is safe because MMS
	// part files are world-readable, and is kept only as a fallback.
	data, err := p.adb.ExecOut(ctx, "content read --uri "+ShellQuote(fmt.Sprintf("content://mms/part/%d", partID)))
	if err == nil {
		return data, contentType, nil
	}
	if path != "" && !strings.ContainsAny(path, "'\" $") {
		data, err2 := p.adb.ExecOut(ctx, "cat "+path)
		if err2 == nil {
			return data, contentType, nil
		}
	}
	return nil, "", err
}

// MarkRead marks all messages in a thread up to a timestamp as read+seen.
func (p *ContentProvider) MarkRead(ctx context.Context, threadID int64, upToMS int64) (int, error) {
	updated := 0
	targets := []struct {
		uri   string
		where string
	}{
		{"content://sms", fmt.Sprintf("thread_id=%d AND date<=%d", threadID, upToMS)},
		{"content://mms", fmt.Sprintf("thread_id=%d AND date<=%d", threadID, upToMS/1000)},
	}
	for _, target := range targets {
		script := "content update --uri " + ShellQuote(target.uri) +
			" --where " + ShellQuote(target.where) +
			" --bind read:i:1 --bind seen:i:1"
		out, err := p.adb.Shell(ctx, script)
		if err != nil {
			return updated, err
		}
		if n := parseUpdatedCount(out); n > 0 {
			updated += n
		}
	}
	return updated, nil
}

// Subscriptions lists SIM subscriptions.
func (p *ContentProvider) Subscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := p.query(ctx, "content://telephony/siminfo",
		[]string{"_id", "sim_id", "display_name", "carrier_name", "number"}, "", "", 0)
	if err != nil {
		return nil, err
	}
	subs := make([]Subscription, 0, len(rows))
	for _, row := range rows {
		subs = append(subs, Subscription{
			SubID:       int(fieldInt64(row, "_id")),
			SimSlot:     int(fieldInt64(row, "sim_id")),
			DisplayName: fieldString(row, "display_name"),
			Carrier:     fieldString(row, "carrier_name"),
			Number:      CanonicalAddress(fieldString(row, "number")),
		})
	}
	return subs, nil
}

// ParseMessageID splits sms:<sub>:<row> / mms:<sub>:<row>.
func ParseMessageID(messageID string) (kind string, subID int, rowID int64, err error) {
	parts := strings.Split(messageID, ":")
	if len(parts) != 3 {
		return "", 0, 0, fmt.Errorf("invalid message id %q", messageID)
	}
	subID = atoi(parts[1])
	rowID, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("invalid message id %q", messageID)
	}
	return parts[0], subID, rowID, nil
}

func parseUpdatedCount(out string) int {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Updated: ") {
			return atoi(strings.TrimPrefix(line, "Updated: "))
		}
	}
	return 0
}

func appendUnique(list []string, value string) []string {
	for _, v := range list {
		if v == value {
			return list
		}
	}
	return append(list, value)
}

func removeAddress(list []string, address string) []string {
	out := list[:0]
	for _, v := range list {
		if v != address {
			out = append(out, v)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// CanonicalAddress normalizes a phone number / sender ID (mirrors the bridge side).
func CanonicalAddress(address string) string {
	address = strings.TrimSpace(address)
	if address == "" || address == "NULL" {
		return ""
	}
	if i := strings.Index(address, ";"); i != -1 {
		address = strings.TrimSpace(address[:i])
	}
	if strings.Contains(address, "@") && strings.Contains(address, ".") {
		return strings.ToLower(address)
	}
	hasPlus := strings.HasPrefix(address, "+")
	var digits strings.Builder
	phoneLike := true
	for i, r := range address {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' && i == 0:
		case r == '(' || r == ')' || r == '-' || r == '.' || r == ' ' || r == '\t' || r == '/':
		default:
			phoneLike = false
		}
	}
	if !phoneLike || digits.Len() == 0 {
		return strings.ToUpper(address)
	}
	value := digits.String()
	if !hasPlus && len(value) >= 3 && len(value) <= 6 {
		return value
	}
	if !hasPlus && (len(value) == 7 || len(value) == 10) {
		value = "1" + value
	}
	return "+" + value
}
