// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"sort"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
)

const (
	// contactNameSourceCustom marks names set with `set-contact-name`.
	contactNameSourceCustom = "custom"
	// contactNameSourceCardDAV marks names imported from an address book.
	contactNameSourceCardDAV = "carddav"
	// contactNameSourcePhone marks names reported by the phone.
	contactNameSourcePhone = "phone"
)

// parseContactNameSource maps a user-supplied source name to a known source.
func parseContactNameSource(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", true
	case "custom", "explicit", "customname", "set-contact-name":
		return contactNameSourceCustom, true
	case "carddav", "addressbook", "address-book":
		return contactNameSourceCardDAV, true
	case "phone", "remote":
		return contactNameSourcePhone, true
	}
	return "", false
}

// setGhostCardDAVName stores a CardDAV contact name on a ghost. It reports
// whether an explicit custom name takes precedence over the imported one.
func (nc *MyNetworkClient) setGhostCardDAVName(ctx context.Context, address, name string) bool {
	trimmed := normalizeContactName(name)
	if trimmed == "" || nc == nil || nc.bridge == nil {
		return false
	}
	canonical := CanonicalAddress(address)
	if canonical == "" {
		canonical = address
	}
	ghost, err := nc.bridge.GetGhostByID(ctx, MakeUserID(canonical))
	if err != nil || ghost == nil {
		return false
	}
	meta, _ := ghost.Metadata.(*GhostMetadata)
	if meta == nil {
		meta = &GhostMetadata{}
	}
	conflict := strings.TrimSpace(meta.CustomName) != "" && strings.TrimSpace(meta.CustomName) != trimmed
	if meta.CardDAVName == trimmed {
		return conflict
	}
	meta.CardDAVName = trimmed
	ghost.Metadata = meta
	if err := nc.bridge.DB.Ghost.Update(ctx, ghost.Ghost); err != nil {
		nc.log.Warn().Err(err).Str("address", canonical).Msg("Failed to store CardDAV contact name")
	}
	return conflict
}

// collectContactAddresses lists every canonical address the login has portals
// for, suitable for a bulk contact-name operation.
func (nc *MyNetworkClient) collectContactAddresses(ctx context.Context, login *bridgev2.UserLogin) ([]string, error) {
	if nc == nil || nc.bridge == nil || login == nil {
		return nil, nil
	}
	rows, err := nc.bridge.DB.UserPortal.GetAllForLogin(ctx, login.UserLogin)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, row := range rows {
		portal, err := nc.bridge.GetExistingPortalByKey(ctx, row.Portal)
		if err != nil || portal == nil {
			continue
		}
		meta, _ := portal.Metadata.(*PortalMetadata)
		if meta == nil {
			continue
		}
		for _, recipient := range portalParticipantList(meta) {
			if canonical := CanonicalAddress(recipient); canonical != "" {
				seen[canonical] = struct{}{}
			}
		}
	}
	addresses := make([]string, 0, len(seen))
	for address := range seen {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	return addresses, nil
}

// countContactNameSources counts contacts that carry a custom, CardDAV or
// phone-reported name.
func (nc *MyNetworkClient) countContactNameSources(ctx context.Context, login *bridgev2.UserLogin) (custom int, carddav int, phone int) {
	addresses, err := nc.collectContactAddresses(ctx, login)
	if err != nil {
		return 0, 0, 0
	}
	for _, address := range addresses {
		ghost, err := nc.bridge.GetExistingGhostByID(ctx, MakeUserID(address))
		if err != nil || ghost == nil {
			continue
		}
		meta, _ := ghost.Metadata.(*GhostMetadata)
		if meta == nil {
			continue
		}
		if strings.TrimSpace(meta.CustomName) != "" {
			custom++
		}
		if strings.TrimSpace(meta.CardDAVName) != "" {
			carddav++
		}
		if strings.TrimSpace(meta.RemoteName) != "" {
			phone++
		}
	}
	return custom, carddav, phone
}

// clearContactNameSources removes names from one source (or every source) for
// one address, or for every known address when the address is empty.
func (nc *MyNetworkClient) clearContactNameSources(ctx context.Context, login *bridgev2.UserLogin, source, address string) (int, error) {
	var addresses []string
	if strings.TrimSpace(address) != "" {
		canonical := CanonicalAddress(address)
		if canonical == "" {
			canonical = strings.TrimSpace(address)
		}
		addresses = []string{canonical}
	} else {
		collected, err := nc.collectContactAddresses(ctx, login)
		if err != nil {
			return 0, err
		}
		addresses = collected
	}
	cleared := 0
	for _, current := range addresses {
		ghost, err := nc.bridge.GetExistingGhostByID(ctx, MakeUserID(current))
		if err != nil || ghost == nil {
			continue
		}
		meta, _ := ghost.Metadata.(*GhostMetadata)
		if meta == nil {
			continue
		}
		changed := false
		clearPhoto := false
		if source == "" || source == contactNameSourceCustom {
			if strings.TrimSpace(meta.CustomName) != "" {
				meta.CustomName = ""
				changed = true
			}
		}
		if source == "" || source == contactNameSourceCardDAV {
			if strings.TrimSpace(meta.CardDAVName) != "" {
				meta.CardDAVName = ""
				changed = true
			}
			if strings.TrimSpace(meta.CardDAVPhotoHash) != "" {
				meta.CardDAVPhotoHash = ""
				changed = true
				clearPhoto = true
			}
		}
		if source == "" || source == contactNameSourcePhone {
			if strings.TrimSpace(meta.RemoteName) != "" {
				meta.RemoteName = ""
				changed = true
			}
		}
		if !changed {
			continue
		}
		ghost.Metadata = meta
		fallback := strings.TrimSpace(GhostDisplayName(meta, current))
		info := &bridgev2.UserInfo{}
		if fallback != "" {
			info.Name = &fallback
		}
		if clearPhoto && strings.HasPrefix(string(ghost.AvatarID), "carddav:") {
			info.Avatar = &bridgev2.Avatar{Remove: true}
		}
		ghost.UpdateInfo(ctx, info)
		cleared++
	}
	if cleared > 0 {
		if _, err := nc.refreshAutoNamedPortals(ctx, login); err != nil {
			return cleared, err
		}
	}
	return cleared, nil
}

const setContactNameUsage = "set-contact-name <phone number> [<display name> | --clear]"

// CommandSetContactName sets or clears a custom display name for a contact.
var CommandSetContactName = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if len(ce.Args) < 2 {
			ce.Reply("Usage: %s", setContactNameUsage)
			return
		}
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No active login.")
			return
		}
		client, ok := login.Client.(*MyNetworkClient)
		if !ok || client == nil {
			ce.Reply("This command is only available on the SMS connection.")
			return
		}
		address := CanonicalAddress(ce.Args[0])
		if address == "" {
			address = strings.TrimSpace(ce.Args[0])
		}
		ghost, err := ce.Bridge.GetGhostByID(ce.Ctx, MakeUserID(address))
		if err != nil || ghost == nil {
			ce.Reply("Failed to load contact %s: %v", address, err)
			return
		}
		meta, _ := ghost.Metadata.(*GhostMetadata)
		if meta == nil {
			meta = &GhostMetadata{}
		}
		if strings.EqualFold(ce.Args[1], "--clear") {
			if strings.TrimSpace(meta.CustomName) == "" {
				ce.Reply("No custom display name is set for %s.", address)
				return
			}
			meta.CustomName = ""
			ghost.Metadata = meta
			fallback := strings.TrimSpace(GhostDisplayName(meta, address))
			ghost.UpdateInfo(ce.Ctx, &bridgev2.UserInfo{Name: &fallback})
			changed, err := refreshAutoNamedPortalsForUser(ce.Ctx, ce.Bridge, ce.User)
			if err != nil {
				ce.Reply("Cleared custom display name for %s, but room refresh failed: %v", address, err)
				return
			}
			ce.Reply("Cleared custom display name for %s (now using %s). Refreshed %d auto-named room(s).", address, fallback, changed)
			return
		}
		name := normalizeContactName(strings.Join(ce.Args[1:], " "))
		if name == "" {
			ce.Reply("Display name cannot be empty.")
			return
		}
		meta.CustomName = name
		ghost.Metadata = meta
		ghost.UpdateInfo(ce.Ctx, &bridgev2.UserInfo{Name: &name})
		changed, err := refreshAutoNamedPortalsForUser(ce.Ctx, ce.Bridge, ce.User)
		if err != nil {
			ce.Reply("Set display name for %s, but room refresh failed: %v", address, err)
			return
		}
		ce.Reply("Set display name for %s. Refreshed %d auto-named room(s).", address, changed)
	},
	Name:    "set-contact-name",
	Aliases: []string{"set-name"},
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Set a custom display name for a contact (--clear removes it)",
		Args:        "<phone number> [<display name> | --clear]",
	},
	RequiresLogin: true,
}

// CommandRefreshNames re-applies contact names to auto-named rooms.
var CommandRefreshNames = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No active login.")
			return
		}
		client, ok := login.Client.(*MyNetworkClient)
		if !ok || client == nil {
			ce.Reply("This command is only available on the SMS connection.")
			return
		}
		changed, err := refreshAutoNamedPortalsForUser(ce.Ctx, ce.Bridge, ce.User)
		if err != nil {
			ce.Reply("Room name refresh failed: %v", err)
			return
		}
		ce.Reply("Queued renames for %d auto-named room(s).", changed)
	},
	Name: "refresh-names",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAdmin,
		Description: "Re-apply contact names to auto-named rooms",
	},
	RequiresLogin: true,
}
