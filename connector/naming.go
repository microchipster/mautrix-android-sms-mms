// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"strconv"
	"strings"

	"maunium.net/go/mautrix/bridgev2/networkid"
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
