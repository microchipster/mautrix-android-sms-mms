// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
)

// cardDAVClientFromEvent resolves the SMS network client behind a command.
func cardDAVClientFromEvent(ce *commands.Event) (*bridgev2.UserLogin, *MyNetworkClient, bool) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No active login.")
		return nil, nil, false
	}
	client, ok := login.Client.(*MyNetworkClient)
	if !ok || client == nil {
		ce.Reply("This command is only available on the SMS connection.")
		return nil, nil, false
	}
	return login, client, true
}

const cardDAVLoginUsage = "carddav-login <addressbook URL> <username> <password>"

// CommandCardDAVLogin stores CardDAV credentials after a connection test.
var CommandCardDAVLogin = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if len(ce.Args) < 3 {
			ce.Reply("Usage: %s", cardDAVLoginUsage)
			return
		}
		ce.Redact()
		_, client, ok := cardDAVClientFromEvent(ce)
		if !ok {
			return
		}
		cfg := client.cardDAVConfig()
		creds := &CardDAVCredentials{
			URL:      strings.TrimSpace(ce.Args[0]),
			Username: strings.TrimSpace(ce.Args[1]),
			Password: strings.Join(ce.Args[2:], " "),
		}
		if err := validateCardDAVURL(cfg, creds.URL); err != nil {
			ce.Reply("%v", err)
			return
		}
		data, err := fetchCardDAVAddressbook(ce.Ctx, cfg, creds)
		if err != nil {
			ce.Reply("CardDAV connection test failed: %v — credentials were not saved.", err)
			return
		}
		contacts := parseVCards(data)
		numbers := 0
		for _, contact := range contacts {
			for _, raw := range contact.Phones {
				if _, ok := normalizeCardDAVPhoneNumber(raw, cfg.DefaultCountryCode); ok {
					numbers++
				}
			}
		}
		if err := client.setCardDAVCredentials(ce.Ctx, creds); err != nil {
			ce.Reply("Failed to store CardDAV credentials: %v", err)
			return
		}
		ce.Reply("CardDAV connection test passed: %d contact(s) with %d phone number(s). Credentials stored for %s.\n\nRun `carddav-sync --no-apply` to preview the import, then `carddav-sync` to apply the names (and photos) to your rooms.",
			len(contacts), numbers, creds.URL)
	},
	Name: "carddav-login",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Connect a CardDAV address book for contact names and photos",
		Args:        "<addressbook URL> <username> <password>",
	},
	RequiresLogin: true,
}

// CommandCardDAVLogout removes stored CardDAV credentials.
var CommandCardDAVLogout = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		_, client, ok := cardDAVClientFromEvent(ce)
		if !ok {
			return
		}
		if err := client.clearCardDAVCredentials(ce.Ctx); err != nil {
			ce.Reply("Failed to remove CardDAV credentials: %v", err)
			return
		}
		ce.Reply("CardDAV credentials removed.")
	},
	Name: "carddav-logout",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Remove the stored CardDAV credentials",
	},
	RequiresLogin: true,
}

// CommandCardDAVStatus reports the CardDAV configuration and sync state.
var CommandCardDAVStatus = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		login, client, ok := cardDAVClientFromEvent(ce)
		if !ok {
			return
		}
		cfg := client.cardDAVConfig()
		creds := client.cardDAVCredentials()
		custom, carddav, phone := client.countContactNameSources(ce.Ctx, login)
		lines := []string{
			fmt.Sprintf("CardDAV default country code: +%s", cfg.DefaultCountryCode),
			fmt.Sprintf("Allow insecure HTTP: %t", cfg.AllowInsecureHTTP),
			fmt.Sprintf("Allow private hosts: %t", cfg.AllowPrivateHosts),
		}
		if creds == nil || strings.TrimSpace(creds.URL) == "" {
			lines = append(lines, "Credentials: not configured (run `carddav-login`)")
		} else {
			lines = append(lines, fmt.Sprintf("Address book: %s", creds.URL))
			if strings.TrimSpace(creds.Username) != "" {
				lines = append(lines, fmt.Sprintf("Username: %s", creds.Username))
			}
			if creds.LastSyncAt != nil {
				lines = append(lines, fmt.Sprintf("Last sync: %s (%d contact(s) added)", creds.LastSyncAt.Format("2006-01-02 15:04:05 MST"), creds.LastSyncCount))
			} else {
				lines = append(lines, "Last sync: never")
			}
		}
		lines = append(lines, fmt.Sprintf("Contact names: %d explicit, %d CardDAV, %d from the phone", custom, carddav, phone))
		ce.Reply("%s", strings.Join(lines, "\n"))
	},
	Name: "carddav-status",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Show the CardDAV connection and contact name sources",
	},
	RequiresLogin: true,
}

// parseCardDAVSyncArgs parses `carddav-sync` flags.
func parseCardDAVSyncArgs(args []string) (apply bool, force bool, ok bool) {
	apply = true
	ok = true
	for _, arg := range args {
		switch strings.ToLower(strings.TrimSpace(arg)) {
		case "", "apply", "--apply":
		case "dry", "dry-run", "--dry-run", "--no-apply":
			apply = false
		case "force-names", "--force-names":
			force = true
		default:
			return false, false, false
		}
	}
	return apply, force, true
}

// CommandCardDAVSync imports contact names from the address book.
var CommandCardDAVSync = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		apply, force, ok := parseCardDAVSyncArgs(ce.Args)
		if !ok {
			ce.Reply("Usage: carddav-sync [--no-apply] [--force-names]")
			return
		}
		_, client, ok := cardDAVClientFromEvent(ce)
		if !ok {
			return
		}
		result, err := client.syncCardDAV(ce.Ctx, apply)
		if err != nil {
			ce.Reply("CardDAV sync failed: %v", err)
			return
		}
		if !apply {
			ce.Reply("CardDAV dry run: %d contact(s), %d phone number(s), %d already imported, %d would be added.",
				result.Contacts, result.Numbers, result.Skipped, result.Candidates)
			return
		}
		lines := []string{
			fmt.Sprintf("CardDAV sync done: %d contact(s), %d phone number(s), %d added, %d updated, %d already imported, %d photo(s).",
				result.Contacts, result.Numbers, result.Added, result.Updated, result.Skipped, result.Photos),
		}
		if result.Conflicts > 0 {
			lines = append(lines, fmt.Sprintf("%d contact(s) keep an explicit name from `set-contact-name`; use `clear-contact-names --source explicit` to let CardDAV win.", result.Conflicts))
		}
		if result.Added > 0 || result.Updated > 0 || force {
			changed, err := refreshAutoNamedPortalsForUser(ce.Ctx, ce.Bridge, ce.User)
			if err != nil {
				lines = append(lines, fmt.Sprintf("Room name refresh failed: %v", err))
			} else {
				lines = append(lines, fmt.Sprintf("Queued renames for %d auto-named room(s).", changed))
			}
		}
		ce.Reply("%s", strings.Join(lines, "\n"))
	},
	Name: "carddav-sync",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Import contact names (and photos) from the address book",
		Args:        "[--no-apply] [--force-names]",
	},
	RequiresLogin: true,
}

// CommandCardDAVClear removes all imported CardDAV names and photos.
var CommandCardDAVClear = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		login, client, ok := cardDAVClientFromEvent(ce)
		if !ok {
			return
		}
		cleared, err := client.clearContactNameSources(ce.Ctx, login, contactNameSourceCardDAV, "")
		if err != nil {
			ce.Reply("Removing CardDAV names failed: %v", err)
			return
		}
		ce.Reply("Removed %d CardDAV name(s) (and photos) from the contact list.", cleared)
	},
	Name: "carddav-clear",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Remove all imported CardDAV names and photos",
	},
	RequiresLogin: true,
}

// CommandClearContactNames clears names from one source, for one contact or all.
var CommandClearContactNames = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		source := ""
		address := ""
		args := ce.Args
		for len(args) > 0 {
			arg := strings.TrimSpace(args[0])
			switch {
			case arg == "--source":
				if len(args) < 2 {
					ce.Reply("Usage: clear-contact-names [--source explicit|carddav|phone] [<phone number>]")
					return
				}
				parsed, ok := parseContactNameSource(args[1])
				if !ok {
					ce.Reply("Unknown name source %q.", args[1])
					return
				}
				source = parsed
				args = args[2:]
				continue
			case strings.HasPrefix(arg, "--source="):
				parsed, ok := parseContactNameSource(strings.TrimPrefix(arg, "--source="))
				if !ok {
					ce.Reply("Unknown name source %q.", strings.TrimPrefix(arg, "--source="))
					return
				}
				source = parsed
				args = args[1:]
				continue
			default:
				address = arg
				args = args[1:]
			}
		}
		login, client, ok := cardDAVClientFromEvent(ce)
		if !ok {
			return
		}
		cleared, err := client.clearContactNameSources(ce.Ctx, login, source, address)
		if err != nil {
			ce.Reply("Clearing contact names failed: %v", err)
			return
		}
		ce.Reply("Cleared names for %d contact(s).", cleared)
	},
	Name: "clear-contact-names",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Clear contact names from one source (explicit, carddav or phone)",
		Args:        "[--source explicit|carddav|phone] [<phone number>]",
	},
	RequiresLogin: true,
}
