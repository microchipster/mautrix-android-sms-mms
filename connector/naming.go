// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
)

// maxGroupNameRecipients is how many participant names a group room name
// shows before collapsing the rest into "and N more".
const maxGroupNameRecipients = 5

// PortalName builds a stable, human-readable room name for a phone thread.
//
// The phone's own thread title (if any) wins. Otherwise group threads list the
// participants (up to five, then "and N more") and DMs use the single
// recipient's canonical address. ownNumbers (the phone's own SIM numbers) are
// excluded from the participant list: the phone provider sometimes leaks them
// into recipient lists.
func PortalName(threadTitle string, recipients []string, ownNumbers ...string) string {
	if title := strings.TrimSpace(threadTitle); title != "" {
		return title
	}
	participants := filterOwnNumbers(recipients, ownNumbers)
	switch len(participants) {
	case 0:
		return "Unknown conversation"
	case 1:
		return participants[0]
	default:
		return groupName(participants)
	}
}

// filterOwnNumbers canonicalizes and deduplicates recipients, dropping the
// given own numbers.
func filterOwnNumbers(recipients []string, ownNumbers []string) []string {
	own := make(map[string]struct{}, len(ownNumbers))
	for _, number := range ownNumbers {
		if canonical := CanonicalAddress(number); canonical != "" {
			own[canonical] = struct{}{}
		}
	}
	all := CanonicalRecipients(recipients)
	out := make([]string, 0, len(all))
	for _, recipient := range all {
		if _, isOwn := own[recipient]; isOwn {
			continue
		}
		out = append(out, recipient)
	}
	return out
}

func groupName(participants []string) string {
	if len(participants) <= maxGroupNameRecipients {
		return strings.Join(participants, ", ")
	}
	shown := participants[:maxGroupNameRecipients]
	return strings.Join(shown, ", ") + ", and " + strconv.Itoa(len(participants)-maxGroupNameRecipients) + " more"
}

// GhostDisplayName picks the display name for a phone-number ghost: a manually
// set custom name wins, then the phone's contact name, then the canonical
// address itself.
func GhostDisplayName(meta *GhostMetadata, address string) string {
	if meta != nil {
		if name := strings.TrimSpace(meta.CustomName); name != "" {
			return name
		}
		if name := strings.TrimSpace(meta.CardDAVName); name != "" {
			return name
		}
		if name := strings.TrimSpace(meta.RemoteName); name != "" {
			return name
		}
	}
	return CanonicalAddress(address)
}

// DMPortalName builds the name for a DM portal, preferring the ghost's known
// contact name over the raw address.
func DMPortalName(otherUser networkid.UserID, meta *GhostMetadata) string {
	address := UnescapeAddress(string(otherUser))
	if name := GhostDisplayName(meta, address); name != "" {
		return name
	}
	return CanonicalAddress(address)
}

// contactInitial returns the first letter or digit of a contact label, uppercased.
func contactInitial(label string) string {
	for _, r := range strings.TrimSpace(label) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return strings.ToUpper(string(r))
		}
		return ""
	}
	return ""
}

// groupInitialsFromLabels joins the initials of every label (WhatsApp style).
func groupInitialsFromLabels(labels []string) string {
	var out strings.Builder
	for _, label := range labels {
		out.WriteString(contactInitial(label))
	}
	return out.String()
}

// initialsLikeName reports whether a name looks like generated initials.
func initialsLikeName(name string, contacts int) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || contacts < 1 || len([]rune(trimmed)) > contacts {
		return false
	}
	for _, r := range trimmed {
		if !unicode.IsUpper(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// normalizeContactName trims and collapses whitespace in a contact name.
func normalizeContactName(name string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(name)), " ")
}

// stripRoomNameSuffix removes a trailing "(sms...)" marker from a room name.
func stripRoomNameSuffix(name string) string {
	trimmed := strings.TrimSpace(name)
	if !strings.HasSuffix(trimmed, ")") {
		return trimmed
	}
	if index := strings.LastIndex(trimmed, "("); index > 0 {
		if strings.HasPrefix(trimmed[index:], "(sms") {
			return strings.TrimSpace(trimmed[:index])
		}
	}
	return trimmed
}

// groupDisplayName renders a group room name from participant addresses and
// their labels: the labels while some are unknown, WhatsApp-style initials once
// every participant has a name.
func groupDisplayName(participants, labels []string) string {
	switch len(participants) {
	case 0:
		return "Unknown conversation"
	case 1:
		if len(labels) > 0 {
			if label := strings.TrimSpace(labels[0]); label != "" {
				return label
			}
		}
		return participants[0]
	}
	names := make([]string, len(participants))
	allNamed := true
	for index, participant := range participants {
		name := participant
		if index < len(labels) {
			if label := strings.TrimSpace(labels[index]); label != "" {
				name = label
			}
		}
		names[index] = name
		if name == participant {
			allNamed = false
		}
	}
	if allNamed {
		if initials := groupInitialsFromLabels(names); initials != "" {
			return initials
		}
	}
	if len(names) <= maxGroupNameRecipients {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:maxGroupNameRecipients], ", ") +
		", and " + strconv.Itoa(len(names)-maxGroupNameRecipients) + " more"
}

// nameForRecipients renders the room name for a conversation with the given
// canonical recipients, using contact names where they are known.
func (nc *MyNetworkClient) nameForRecipients(ctx context.Context, recipients []string) string {
	switch len(recipients) {
	case 0:
		return ""
	case 1:
		return nc.dmDisplayName(ctx, recipients[0])
	}
	labels := make([]string, len(recipients))
	for index, recipient := range recipients {
		labels[index] = nc.dmDisplayName(ctx, recipient)
	}
	return groupDisplayName(recipients, labels)
}

// portalLooksAutoNamed reports whether a room name was generated by the bridge
// (a number, a number list or initials) rather than chosen by the user.
func (nc *MyNetworkClient) portalLooksAutoNamed(ctx context.Context, portal *bridgev2.Portal) bool {
	if portal == nil || portal.NameIsCustom {
		return false
	}
	meta, _ := portal.Metadata.(*PortalMetadata)
	recipients := portalParticipantList(meta)
	name := stripRoomNameSuffix(portal.Name)
	if name == "" {
		return true
	}
	if len(recipients) == 0 {
		return false
	}
	if desired := strings.TrimSpace(nc.nameForRecipients(ctx, recipients)); desired != "" && name == desired {
		return true
	}
	if len(recipients) == 1 {
		number := CanonicalAddress(recipients[0])
		if number == "" {
			number = recipients[0]
		}
		if name == number || name == recipients[0] {
			return true
		}
		return initialsLikeName(name, 1)
	}
	if strings.ContainsAny(name, "@;") {
		return true
	}
	numbers := append([]string(nil), recipients...)
	if name == groupName(numbers) || name == strings.Join(numbers, ", ") {
		return true
	}
	return initialsLikeName(name, len(recipients))
}

// refreshAutoNamedPortals queues a room name update for every auto-named room
// whose computed name no longer matches, e.g. after new contact names arrive.
func (nc *MyNetworkClient) refreshAutoNamedPortals(ctx context.Context, login *bridgev2.UserLogin) (int, error) {
	if nc == nil || nc.bridge == nil || nc.bridge.DB == nil || login == nil {
		return 0, fmt.Errorf("bridge is not available")
	}
	rows, err := nc.bridge.DB.UserPortal.GetAllForLogin(ctx, login.UserLogin)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, row := range rows {
		portal, err := nc.bridge.GetExistingPortalByKey(ctx, row.Portal)
		if err != nil || portal == nil || portal.MXID == "" {
			continue
		}
		meta, _ := portal.Metadata.(*PortalMetadata)
		recipients := portalParticipantList(meta)
		if len(recipients) == 0 {
			continue
		}
		if !nc.portalLooksAutoNamed(ctx, portal) {
			continue
		}
		desired := strings.TrimSpace(nc.nameForRecipients(ctx, recipients))
		if desired == "" {
			continue
		}
		desired = strings.TrimSpace(desired + " " + nc.roomNameSuffix(meta.SubID))
		if desired == strings.TrimSpace(portal.Name) {
			continue
		}
		name := desired
		nc.bridge.QueueRemoteEvent(login, &simplevent.ChatInfoChange{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatInfoChange,
				PortalKey: networkid.PortalKey{ID: portal.ID},
				Timestamp: time.Now(),
			},
			ChatInfoChange: &bridgev2.ChatInfoChange{ChatInfo: &bridgev2.ChatInfo{Name: &name}},
		})
		changed++
	}
	return changed, nil
}
