// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

const defaultStatsWindow = 30 * 24 * time.Hour

// statsWindow is the parsed `stats` argument.
type statsWindow struct {
	Label    string
	Duration time.Duration
	All      bool
}

// Bounds returns the inclusive/exclusive time range the command reports on.
func (w statsWindow) Bounds() (start, end time.Time) {
	end = time.Now().UTC()
	if w.All {
		return time.Unix(0, 0).UTC(), end
	}
	return end.Add(-w.Duration), end
}

// parseStatsWindow parses `stats [24h|7d|30d|all]`. The default is 30d.
func parseStatsWindow(args []string) (statsWindow, error) {
	if len(args) > 1 {
		return statsWindow{}, fmt.Errorf("Usage: stats [24h|7d|30d|all]")
	}
	label := "30d"
	if len(args) == 1 {
		label = strings.ToLower(strings.TrimSpace(args[0]))
	}
	if label == "" {
		return statsWindow{}, fmt.Errorf("Usage: stats [24h|7d|30d|all]")
	}
	if label == "all" {
		return statsWindow{Label: "all time", All: true}, nil
	}
	duration, err := parseLookbackDuration(label)
	if err != nil || duration <= 0 {
		return statsWindow{}, fmt.Errorf("Invalid window %q (use 24h, 7d, 30d, or all)", label)
	}
	return statsWindow{Label: label, Duration: duration}, nil
}

// statsMessage is one bridged message reduced to the fields the aggregation
// needs.
type statsMessage struct {
	SubID  int
	IsMMS  bool
	FromMe bool
}

// statsCounts is a direction/type breakdown.
type statsCounts struct {
	Total int
	In    int
	Out   int
	SMS   int
	MMS   int
}

func (c *statsCounts) add(msg statsMessage) {
	c.Total++
	if msg.FromMe {
		c.Out++
	} else {
		c.In++
	}
	if msg.IsMMS {
		c.MMS++
	} else {
		c.SMS++
	}
}

// statsCarrierStats is the per-subscription (carrier) breakdown.
type statsCarrierStats struct {
	SubID       int
	Carrier     string
	Number      string
	DisplayName string
	Counts      statsCounts
}

// statsReport is the aggregated result shown in the management room.
type statsReport struct {
	Window   string
	Counts   statsCounts
	Carriers []statsCarrierStats
}

// aggregateStats groups message counts by subscription. Subscriptions without
// any traffic are included with zero counts so dual-SIM users can see both
// slots; messages whose sub_id is unknown are grouped under SubID 0.
func aggregateStats(messages []statsMessage, subscriptions map[int]SubscriptionInfo) statsReport {
	report := statsReport{}
	bySub := make(map[int]statsCarrierStats, len(subscriptions)+1)
	for subID, info := range subscriptions {
		bySub[subID] = statsCarrierStats{
			SubID:       subID,
			Carrier:     info.Carrier,
			Number:      CanonicalAddress(info.PhoneNumber),
			DisplayName: info.DisplayName,
		}
	}
	for _, msg := range messages {
		report.Counts.add(msg)
		entry, ok := bySub[msg.SubID]
		if !ok {
			entry = statsCarrierStats{SubID: msg.SubID}
		}
		entry.Counts.add(msg)
		bySub[msg.SubID] = entry
	}
	subIDs := make([]int, 0, len(bySub))
	for subID := range bySub {
		subIDs = append(subIDs, subID)
	}
	sort.Ints(subIDs)
	for _, subID := range subIDs {
		report.Carriers = append(report.Carriers, bySub[subID])
	}
	return report
}

// formatStatsReport renders the aggregated report.
func formatStatsReport(report statsReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Message statistics (%s): %d total (in %d / out %d), SMS %d / MMS %d",
		report.Window, report.Counts.Total, report.Counts.In, report.Counts.Out, report.Counts.SMS, report.Counts.MMS)
	if len(report.Carriers) == 0 {
		b.WriteString("\nNo subscriptions known yet; the phone reports SIM information in its heartbeat.")
		return b.String()
	}
	b.WriteString("\n\nBy carrier/SIM:")
	for _, carrier := range report.Carriers {
		fmt.Fprintf(&b, "\n- %s: %d (in %d / out %d), SMS %d / MMS %d",
			statsCarrierLabel(carrier), carrier.Counts.Total,
			carrier.Counts.In, carrier.Counts.Out, carrier.Counts.SMS, carrier.Counts.MMS)
	}
	return b.String()
}

func statsCarrierLabel(carrier statsCarrierStats) string {
	if carrier.SubID == 0 {
		return "unknown SIM"
	}
	parts := []string{fmt.Sprintf("sub %d", carrier.SubID)}
	if carrier.Carrier != "" {
		parts = append(parts, carrier.Carrier)
	}
	if carrier.DisplayName != "" && carrier.DisplayName != carrier.Carrier {
		parts = append(parts, carrier.DisplayName)
	}
	if carrier.Number != "" {
		parts = append(parts, carrier.Number)
	}
	if len(parts) == 1 {
		parts = append(parts, "unknown carrier")
	}
	return strings.Join(parts, " — ")
}

// statsFromMe classifies a stored message as outgoing. Phone-originated
// messages store the own-number ghost as sender_id; messages sent from Matrix
// store the Matrix user as sender_mxid (and also the own-number ghost).
func statsFromMe(senderID networkid.UserID, senderMXID id.UserID, ownSenderIDs map[networkid.UserID]bool, userMXID id.UserID) bool {
	if senderID != "" && ownSenderIDs[senderID] {
		return true
	}
	return senderMXID != "" && senderMXID == userMXID
}

// messageSubID extracts the subscription ID from a phone message ID of the
// form "sms:<sub_id>:<row_id>" / "mms:<sub_id>:<row_id>". Returns 0 (unknown)
// for placeholder or malformed IDs.
func messageSubID(messageID string) int {
	parts := strings.Split(messageID, ":")
	if len(parts) != 3 {
		return 0
	}
	if parts[0] != "sms" && parts[0] != "mms" {
		return 0
	}
	subID, err := strconv.Atoi(parts[1])
	if err != nil || subID < 0 {
		return 0
	}
	return subID
}

// collectLoginStats reads the bridged messages of one login inside the window
// and reduces them to aggregation input.
func collectLoginStats(ctx context.Context, br *bridgev2.Bridge, login *bridgev2.UserLogin, user *bridgev2.User, window statsWindow) ([]statsMessage, map[int]SubscriptionInfo, error) {
	if br == nil || br.DB == nil || login == nil || login.UserLogin == nil {
		return nil, nil, fmt.Errorf("bridge database or login is not available")
	}
	userPortals, err := br.DB.UserPortal.GetAllForLogin(ctx, login.UserLogin)
	if err != nil {
		return nil, nil, fmt.Errorf("list portals: %w", err)
	}
	portalKeys := make(map[networkid.PortalKey]struct{}, len(userPortals))
	for _, userPortal := range userPortals {
		if userPortal != nil {
			portalKeys[userPortal.Portal] = struct{}{}
		}
	}

	ownSenderIDs := make(map[networkid.UserID]bool)
	if client, ok := login.Client.(*MyNetworkClient); ok && client != nil {
		for _, number := range client.ownNumbers() {
			ownSenderIDs[MakeUserID(number)] = true
		}
		if ownUserID := client.ownUserID(); ownUserID != "" {
			ownSenderIDs[ownUserID] = true
		}
	}
	if login.ID != "" {
		// Historical logins without known SIM numbers used the login ID as the
		// outgoing sender fallback.
		ownSenderIDs[networkid.UserID(login.ID)] = true
	}
	var userMXID id.UserID
	if user != nil {
		userMXID = user.MXID
	}

	start, end := window.Bounds()
	rows, err := br.DB.Query(ctx, statsMessagesQuery, br.DB.BridgeID, start.UnixNano(), end.UnixNano())
	if err != nil {
		return nil, nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	messages := make([]statsMessage, 0)
	seen := make(map[string]struct{})
	for rows.Next() {
		var messageID, senderID, senderMXID, roomID, receiver string
		if err := rows.Scan(&messageID, &senderID, &senderMXID, &roomID, &receiver); err != nil {
			return nil, nil, fmt.Errorf("scan message row: %w", err)
		}
		// Multi-attachment MMS store one row per part; count the message once.
		key := networkid.PortalKey{ID: networkid.PortalID(roomID), Receiver: networkid.UserLoginID(receiver)}
		if _, ok := portalKeys[key]; !ok {
			continue
		}
		if _, ok := seen[messageID]; ok {
			continue
		}
		seen[messageID] = struct{}{}
		messages = append(messages, statsMessage{
			SubID:  messageSubID(messageID),
			IsMMS:  strings.HasPrefix(messageID, "mms:"),
			FromMe: statsFromMe(networkid.UserID(senderID), id.UserID(senderMXID), ownSenderIDs, userMXID),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate message rows: %w", err)
	}

	subscriptions := make(map[int]SubscriptionInfo)
	if meta, _ := login.Metadata.(*LoginMetadata); meta != nil {
		for subID, info := range meta.Subscriptions {
			subscriptions[subID] = info
		}
	}
	return messages, subscriptions, nil
}

// CommandStats reports message counts by direction, type and carrier/SIM.
var CommandStats = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		window, err := parseStatsWindow(ce.Args)
		if err != nil {
			ce.Reply("%v", err)
			return
		}
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No login found")
			return
		}
		messages, subscriptions, err := collectLoginStats(ce.Ctx, ce.Bridge, login, ce.User, window)
		if err != nil {
			ce.Reply("Failed to collect message statistics: %v", err)
			return
		}
		report := aggregateStats(messages, subscriptions)
		report.Window = window.Label
		ce.Reply("%s", formatStatsReport(report))
	},
	Name:    "stats",
	Aliases: []string{"carrier-stats", "usage"},
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Show message counts by direction, SMS/MMS and carrier/SIM (usage: stats [24h|7d|30d|all])",
		Args:        "[24h|7d|30d|all]",
	},
	RequiresLogin: true,
}

// SQL used by the stats command. timestamp is stored in nanoseconds by
// bridgev2; the room_* columns match the portal key.
const statsMessagesQuery = `
SELECT id, sender_id, sender_mxid, room_id, room_receiver
FROM message
WHERE bridge_id=$1 AND timestamp>$2 AND timestamp<=$3`
