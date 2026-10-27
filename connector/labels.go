// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// NewContactLabel is the label applied automatically to the ghost of a sender
// whose first inbound message creates a portal (auto_label_new_contacts).
const NewContactLabel = "new contact"

// Ensure MyNetworkClient implements bridgev2.UserBlockingNetworkAPI so the
// built-in `block-user` command can be used against SMS/MMS senders.
var _ bridgev2.UserBlockingNetworkAPI = (*MyNetworkClient)(nil)

// HandleMatrixBlockUser implements bridgev2.UserBlockingNetworkAPI.
//
// SMS/MMS has no remote block list the bridge could write to, so blocking is
// enforced bridge-side: bridgev2 stores the blocked state on the portal and
// dropBlockedInbound discards inbound messages from blocked DM contacts. The
// phone still receives blocked messages until the user blocks the sender in
// the Android messages app too.
func (nc *MyNetworkClient) HandleMatrixBlockUser(ctx context.Context, msg *bridgev2.MatrixBlockUser) error {
	if msg == nil || msg.Portal == nil {
		return fmt.Errorf("portal is missing")
	}
	return nil
}

// dropBlockedInbound applies the anti-spam gate to an inbound message before it
// is bridged. It returns true when the message must be dropped because the
// sender (or the thread portal) is blocked. New contacts that are not blocked
// are auto-labeled.
func (nc *MyNetworkClient) dropBlockedInbound(ctx context.Context, portalKey networkid.PortalKey, sender string) bool {
	if nc == nil || nc.bridge == nil || nc.login == nil || sender == "" {
		return false
	}
	portal, err := nc.bridge.GetExistingPortalByKey(ctx, portalKey)
	if err != nil {
		nc.log.Debug().Err(err).Str("portal_id", string(portalKey.ID)).Msg("Failed to load portal for block check")
		portal = nil
	}
	if portal != nil {
		if portal.UserBlocked {
			nc.log.Info().
				Str("portal_id", string(portalKey.ID)).
				Str("sender", sender).
				Msg("Dropping inbound message from a blocked contact")
			return true
		}
		return false
	}

	ghostID := MakeUserID(sender)
	dmPortal, err := nc.bridge.GetDMPortal(ctx, nc.login.ID, ghostID)
	if err != nil {
		nc.log.Debug().Err(err).Str("ghost_id", string(ghostID)).Msg("Failed to check for an existing DM portal")
	}
	if dmPortal != nil {
		if dmPortal.UserBlocked {
			nc.log.Info().
				Str("portal_id", string(dmPortal.ID)).
				Str("sender", sender).
				Msg("Dropping inbound message from a blocked contact")
			return true
		}
		return false
	}
	nc.labelNewContact(ctx, ghostID, sender)
	return false
}

// labelNewContact stores the automatic "new contact" label on a ghost and,
// when configured, posts a management-room notice. Only the address is ever
// included in the notice, never the message body.
func (nc *MyNetworkClient) labelNewContact(ctx context.Context, ghostID networkid.UserID, address string) {
	if nc == nil || nc.bridge == nil || !nc.connector.autoLabelNewContacts() {
		return
	}
	canonical := CanonicalAddress(address)
	if canonical == "" {
		return
	}
	for _, own := range nc.ownNumbers() {
		if own == canonical {
			return
		}
	}
	ghost, err := nc.bridge.GetExistingGhostByID(ctx, ghostID)
	if err != nil {
		nc.log.Debug().Err(err).Str("ghost_id", string(ghostID)).Msg("Failed to load ghost for auto-labeling")
		return
	}
	if ghost == nil {
		ghost, err = nc.bridge.GetGhostByID(ctx, ghostID)
		if err != nil {
			nc.log.Warn().Err(err).Str("ghost_id", string(ghostID)).Msg("Failed to create ghost for auto-labeling")
			return
		}
	}
	meta, _ := ghost.Metadata.(*GhostMetadata)
	if meta == nil {
		meta = &GhostMetadata{}
		ghost.Metadata = meta
	}
	if len(meta.Labels) > 0 {
		return
	}
	meta.Labels = append(meta.Labels, NewContactLabel)
	if err := nc.bridge.DB.Ghost.Update(ctx, ghost.Ghost); err != nil {
		nc.log.Warn().Err(err).Str("ghost_id", string(ghostID)).Msg("Failed to save new-contact label")
		return
	}
	nc.log.Info().Str("ghost_id", string(ghostID)).Msg("Labeled new contact")
	if nc.connector.newContactNotice() && nc.login.User != nil {
		sendNotice(nc.bridge.Bot, nc.login.User.ManagementRoom,
			fmt.Sprintf("New contact: %s (first inbound message). Use `label %s <label>` to tag them.", canonical, canonical))
	}
}

// addLabel appends a label unless an equivalent one (case-insensitive, trimmed)
// is already present.
func addLabel(labels []string, label string) []string {
	label = strings.TrimSpace(label)
	if label == "" {
		return labels
	}
	for _, existing := range labels {
		if strings.EqualFold(strings.TrimSpace(existing), label) {
			return labels
		}
	}
	return append(labels, label)
}

const labelUsage = "Usage: label <phone> <label|--clear>"

// parseLabelArgs parses `label <phone> <label|--clear>`. The label may contain
// spaces.
func parseLabelArgs(args []string) (address string, label string, clear bool, err error) {
	if len(args) < 2 {
		return "", "", false, fmt.Errorf("%s", labelUsage)
	}
	address = strings.TrimSpace(args[0])
	if address == "" {
		return "", "", false, fmt.Errorf("%s", labelUsage)
	}
	rest := strings.TrimSpace(strings.Join(args[1:], " "))
	switch {
	case rest == "":
		return "", "", false, fmt.Errorf("%s", labelUsage)
	case strings.EqualFold(rest, "--clear"):
		return address, "", true, nil
	case strings.HasPrefix(rest, "--"):
		return "", "", false, fmt.Errorf("unknown option %q. %s", rest, labelUsage)
	}
	return address, rest, false, nil
}

// labeledContact is a ghost with its labels, for the `labels` listing.
type labeledContact struct {
	Address string
	Labels  []string
}

// CommandLabel assigns or clears labels on a phone number / sender.
var CommandLabel = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		address, label, clear, err := parseLabelArgs(ce.Args)
		if err != nil {
			ce.Reply("%v", err)
			return
		}
		canonical := CanonicalAddress(address)
		if canonical == "" {
			ce.Reply("Invalid phone number or sender ID %q", address)
			return
		}
		ghost, err := ce.Bridge.GetGhostByID(ce.Ctx, MakeUserID(canonical))
		if err != nil || ghost == nil {
			ce.Reply("Failed to load contact %s: %v", canonical, err)
			return
		}
		meta, _ := ghost.Metadata.(*GhostMetadata)
		if meta == nil {
			meta = &GhostMetadata{}
			ghost.Metadata = meta
		}
		if clear {
			meta.Labels = nil
		} else {
			meta.Labels = addLabel(meta.Labels, label)
		}
		if err := ce.Bridge.DB.Ghost.Update(ce.Ctx, ghost.Ghost); err != nil {
			ce.Reply("Failed to save label for %s: %v", canonical, err)
			return
		}
		if clear {
			ce.Reply("Cleared labels for %s.", canonical)
			return
		}
		ce.Reply("Labels for %s: %s", canonical, strings.Join(meta.Labels, ", "))
	},
	Name: "label",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Label a phone number or sender ID (usage: label <phone> <label|--clear>)",
		Args:        "<phone> <label|--clear>",
	},
	RequiresLogin: true,
}

// CommandLabels lists every labeled contact.
var CommandLabels = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		contacts, err := listLabeledContacts(ce.Ctx, ce.Bridge)
		if err != nil {
			ce.Reply("Failed to list labels: %v", err)
			return
		}
		ce.Reply("%s", formatLabeledContacts(contacts))
	},
	Name:    "labels",
	Aliases: []string{"list-labels"},
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "List labeled phone numbers and senders",
	},
	RequiresLogin: true,
}

// ghostsWithLabelsQuery selects ghosts with a non-empty labels metadata field.
// `metadata->>` is the JSON access bridgev2 itself uses, so it works on both
// SQLite and PostgreSQL.
const ghostsWithLabelsQuery = `
SELECT bridge_id, id, name, avatar_id, avatar_hash, avatar_mxc,
       name_set, avatar_set, contact_info_set, is_bot, identifiers, extra_profile, metadata
FROM ghost
WHERE bridge_id=$1 AND metadata->>'labels' IS NOT NULL AND metadata->>'labels' NOT IN ('null', '[]')`

// listLabeledContacts returns all labeled ghosts, sorted by address.
func listLabeledContacts(ctx context.Context, br *bridgev2.Bridge) ([]labeledContact, error) {
	if br == nil || br.DB == nil {
		return nil, fmt.Errorf("bridge database is not available")
	}
	ghosts, err := br.DB.Ghost.QueryMany(ctx, ghostsWithLabelsQuery, br.DB.BridgeID)
	if err != nil {
		return nil, err
	}
	contacts := make([]labeledContact, 0, len(ghosts))
	for _, ghost := range ghosts {
		if ghost == nil {
			continue
		}
		meta, _ := ghost.Metadata.(*GhostMetadata)
		if meta == nil || len(meta.Labels) == 0 {
			continue
		}
		labels := append([]string(nil), meta.Labels...)
		sort.Strings(labels)
		contacts = append(contacts, labeledContact{
			Address: UnescapeAddress(string(ghost.ID)),
			Labels:  labels,
		})
	}
	sort.Slice(contacts, func(i, j int) bool { return contacts[i].Address < contacts[j].Address })
	return contacts, nil
}

// formatLabeledContacts renders the `labels` command output.
func formatLabeledContacts(contacts []labeledContact) string {
	if len(contacts) == 0 {
		return "No labels yet. Use `label <phone> <label>` to add one."
	}
	lines := make([]string, 0, len(contacts))
	for _, contact := range contacts {
		lines = append(lines, fmt.Sprintf("- %s: %s", contact.Address, strings.Join(contact.Labels, ", ")))
	}
	return strings.Join(lines, "\n")
}
