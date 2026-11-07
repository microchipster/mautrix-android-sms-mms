// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"fmt"
	"sort"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
)

// spamRoomMatch is one bridged room whose contact is marked as spam.
type spamRoomMatch struct {
	sender   string
	portalID string
	portal   *bridgev2.Portal
}

// spamPolicyRooms finds this login's portals for contacts with a hold/block
// per-contact anti-spam policy.
func spamPolicyRooms(ce *commands.Event, login *bridgev2.UserLogin, entries []spamContactPolicyEntry) ([]spamRoomMatch, error) {
	targets := make(map[string]string, len(entries))
	for _, entry := range entries {
		canonical := CanonicalAddress(entry.Sender)
		if canonical == "" {
			continue
		}
		targets[canonical] = entry.Sender
	}
	rows, err := ce.Bridge.DB.UserPortal.GetAllForLogin(ce.Ctx, login.UserLogin)
	if err != nil {
		return nil, fmt.Errorf("failed to list portals: %w", err)
	}
	var matches []spamRoomMatch
	for _, up := range rows {
		portal, err := ce.Bridge.GetExistingPortalByKey(ce.Ctx, up.Portal)
		if err != nil || portal == nil || portal.MXID == "" {
			continue
		}
		if ce.User.ManagementRoom != "" && portal.MXID == ce.User.ManagementRoom {
			continue
		}
		if spaceRoom, err := login.GetSpaceRoom(ce.Ctx); err == nil && spaceRoom != "" && portal.MXID == spaceRoom {
			continue
		}
		sender, ok := targets[UnescapeAddress(string(portal.OtherUserID))]
		if !ok {
			for canonical, original := range targets {
				if string(up.Portal.ID) == string(MakeAddressPortalID(canonical)) {
					sender, ok = original, true
					break
				}
			}
		}
		if !ok {
			continue
		}
		matches = append(matches, spamRoomMatch{sender: sender, portalID: string(up.Portal.ID), portal: portal})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].sender < matches[j].sender })
	return matches, nil
}

// CommandDeleteSpamRooms deletes the bridged rooms of contacts marked with a
// hold or block per-contact anti-spam policy.
var CommandDeleteSpamRooms = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No active login.")
			return
		}
		connector, _ := ce.Bridge.Network.(*MyConnector)
		if connector == nil {
			ce.Reply("Network connector is not available")
			return
		}
		store, err := connector.ensureSpamStore(ce.Ctx)
		if err != nil {
			ce.Reply("Failed to open the anti-spam state store: %v", err)
			return
		}
		entries, err := store.contactPolicies(ce.Ctx)
		if err != nil {
			ce.Reply("Failed to read the per-contact anti-spam policies: %v", err)
			return
		}
		var marked []spamContactPolicyEntry
		for _, entry := range entries {
			if entry.Policy == SpamContactPolicyHold || entry.Policy == SpamContactPolicyBlock {
				marked = append(marked, entry)
			}
		}
		if len(marked) == 0 {
			ce.Reply("No contacts are marked as spam. Mark one with `spam policy <number> hold` or `spam policy <number> block`.")
			return
		}
		matches, err := spamPolicyRooms(ce, login, marked)
		if err != nil {
			ce.Reply("%v", err)
			return
		}
		if len(matches) == 0 {
			ce.Reply("No bridged rooms found for the %d contact(s) marked as spam.", len(marked))
			return
		}
		confirm := len(ce.Args) > 0 && (ce.Args[0] == "yes" || ce.Args[0] == "--yes")
		if !confirm {
			lines := []string{fmt.Sprintf("Found %d bridged room(s) for %d contact(s) marked as spam:", len(matches), len(marked))}
			for _, m := range matches {
				lines = append(lines, fmt.Sprintf("- %s — %s", m.sender, m.portal.MXID))
			}
			lines = append(lines, "Run: delete-spam-rooms yes to delete them.")
			ce.Reply("%s", strings.Join(lines, "\n"))
			return
		}
		portals := make([]*bridgev2.Portal, 0, len(matches))
		matchedIDs := make(map[string]struct{}, len(matches))
		for _, m := range matches {
			portals = append(portals, m.portal)
			matchedIDs[m.portalID] = struct{}{}
		}
		bridgev2.DeleteManyPortals(ce.Ctx, portals, nil)
		rows, err := ce.Bridge.DB.UserPortal.GetAllForLogin(ce.Ctx, login.UserLogin)
		if err != nil {
			ce.Reply("Failed to list portals for cleanup: %v", err)
			return
		}
		for _, up := range rows {
			if _, ok := matchedIDs[string(up.Portal.ID)]; ok {
				_ = ce.Bridge.DB.UserPortal.Delete(ce.Ctx, up)
			}
		}
		ce.Reply("Deleted %d bridged room(s) for contacts marked as spam. The management room and space are kept.", len(matches))
	},
	Name: "delete-spam-rooms",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAdmin,
		Description: "Remove bridged rooms for contacts marked as spam (preview; use: delete-spam-rooms yes)",
	},
	RequiresLogin: true,
}
