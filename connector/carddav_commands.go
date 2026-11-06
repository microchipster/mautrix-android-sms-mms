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

const cardDAVLoginUsage = "carddav-login <email or addressbook URL> [username] <password>"

// CommandCardDAVLogin stores CardDAV credentials after a connection test.
// It accepts an email address and finds the address book URL automatically,
// or a full addressbook URL with username and password.
var CommandCardDAVLogin = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if len(ce.Args) < 2 {
			ce.Reply("Usage: %s", cardDAVLoginUsage)
			ce.Reply("With just an email address and password, the bridge tries the common CardDAV locations automatically. Arguments are removed from the room after the command runs; nothing is stored unless the connection test succeeds.")
			return
		}
		ce.Redact()
		_, client, ok := cardDAVClientFromEvent(ce)
		if !ok {
			return
		}
		cfg := client.cardDAVConfig()
		if client.cardDAVLocked() {
			ce.Reply("CardDAV is locked. Run `carddav-unlock <secret>` first to restore the in-memory key (nothing on disk can unlock it).")
			return
		}
		var creds *CardDAVCredentials
		var data []byte
		if cardDAVLooksLikeURL(ce.Args[0]) {
			if len(ce.Args) < 3 {
				ce.Reply("Usage: %s", cardDAVLoginUsage)
				return
			}
			creds = &CardDAVCredentials{
				URL:      strings.TrimSpace(ce.Args[0]),
				Username: strings.TrimSpace(ce.Args[1]),
				Password: strings.Join(ce.Args[2:], " "),
			}
			if err := validateCardDAVURL(cfg, creds.URL); err != nil {
				ce.Reply("%v", err)
				return
			}
			var err error
			data, err = fetchCardDAVAddressbook(ce.Ctx, cfg, creds)
			if err != nil {
				ce.Reply("CardDAV connection test failed: %v — credentials were not saved.", err)
				return
			}
		} else {
			hint := strings.TrimSpace(ce.Args[0])
			if !strings.Contains(hint, "@") {
				ce.Reply("That does not look like an email address. Use: %s", cardDAVLoginUsage)
				return
			}
			username := hint
			password := ce.Args[1]
			if len(ce.Args) > 2 {
				username = strings.TrimSpace(ce.Args[1])
				password = strings.Join(ce.Args[2:], " ")
			}
			if strings.TrimSpace(password) == "" {
				ce.Reply("A password is required.")
				return
			}
			candidates := cardDAVCandidateURLs(hint, username)
			if len(candidates) == 0 {
				ce.Reply("Could not work out any address book URLs for %s. If you know the URL, use: carddav-login <addressbook URL> <username> <password>.", hint)
				return
			}
			ce.Reply("Looking for your CardDAV address book for %s (trying %d common addresses)…", hint, len(candidates))
			url, found, tried, err := discoverCardDAVAddressbook(ce.Ctx, cfg, candidates, username, password)
			if err != nil {
				ce.Reply("Could not find your CardDAV address book automatically (%d addresses tried). If you know the URL, use: carddav-login <addressbook URL> <username> <password>.", tried)
				return
			}
			ce.Reply("Found your address book: %s", url)
			creds = &CardDAVCredentials{URL: url, Username: username, Password: password}
			data = found
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
		ce.Reply("CardDAV connection test passed: %d contact(s) with %d phone number(s). Credentials stored for %s.\n\nRun `carddav-sync --no-apply` to preview the import, then `carddav-sync` to apply the names (and photos) to your rooms.\n\n%s", len(contacts), numbers, creds.URL, client.cardDAVEncryptionNote())
	},
	Name: "carddav-login",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Connect a CardDAV address book for contact names and photos (an email address is enough; the URL is found automatically)",
		Args:        "[email or addressbook URL] [username] <password>",
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
		if client.cardDAVLocked() {
			lines = append(lines, "Encryption: locked — run `carddav-unlock <secret>` (the key is kept in memory only)")
		} else if meta, _ := login.Metadata.(*LoginMetadata); meta != nil && meta.CardDAVKeyMode == cardDAVKeyModeMemory {
			lines = append(lines, "Encryption: unlocked (in-memory key; not stored on disk)")
		} else if cardDAVLegacyKeyFileExists() {
			lines = append(lines, "Encryption: legacy key file (/data/carddav.key) — run `carddav-unlock <secret>` to switch to an in-memory key")
		} else {
			lines = append(lines, "Encryption: no key set (run `carddav-login` or `carddav-unlock <secret>`)")
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
		if client.cardDAVLocked() {
			ce.Reply("CardDAV is locked. Run `carddav-unlock <secret>` first (the key is kept in memory only).")
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

var CommandCardDAVUnlock = &commands.FullHandler{
	Name: "carddav-unlock",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Derive the CardDAV encryption key from a secret and keep it in memory only",
	},
	RequiresLogin: true,
	Func: func(ce *commands.Event) {
		if len(ce.Args) < 1 {
			ce.Reply("Usage: `carddav-unlock <secret>`")
			ce.Reply("The secret derives the key that encrypts your stored CardDAV password and contact names. The key is kept in memory only, so nothing readable is written to disk. Run this after every bridge restart, in an encrypted room. If you ever lose the secret: `carddav-reset yes`, then `carddav-login` again to refetch from your CardDAV server.")
			return
		}
		ce.Redact()
		login, client, ok := cardDAVClientFromEvent(ce)
		if !ok {
			return
		}
		secret := strings.TrimSpace(strings.Join(ce.Args, " "))
		if secret == "" {
			ce.Reply("Usage: `carddav-unlock <secret>`")
			return
		}
		oldKey, _ := cardDAVProcessKey()
		newKey := cardDAVUnlockKeyFromSecret(secret)
		meta, _ := login.Metadata.(*LoginMetadata)
		if meta == nil {
			meta = &LoginMetadata{}
		}
		staged := meta.CardDAV
		if staged != nil {
			clone := *staged
			if clone.Password != "" {
				if cardDAVNameIsEncrypted(clone.Password) {
					if len(oldKey) == 0 {
						ce.Reply("The stored CardDAV password is encrypted with a key that is not available any more. Run `carddav-reset yes`, then `carddav-login` again to refetch from your CardDAV server.")
						return
					}
					plain, err := decryptCardDAVWithKey(oldKey, clone.Password)
					if err != nil {
						ce.Reply("Failed to decrypt the stored CardDAV password: %v", err)
						return
					}
					clone.Password = plain
				}
				encrypted, err := encryptCardDAVWithKey(newKey, clone.Password)
				if err != nil {
					ce.Reply("Failed to encrypt the stored CardDAV password: %v", err)
					return
				}
				clone.Password = encrypted
			}
			staged = &clone
		}
		setCardDAVRAMKey(newKey)
		meta.CardDAV = staged
		meta.CardDAVKeyMode = cardDAVKeyModeMemory
		login.Metadata = meta
		if err := login.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to store the CardDAV unlock state: %v", err)
			return
		}
		legacy := ""
		if cardDAVLegacyKeyFileExists() {
			legacy = " The legacy key file /data/carddav.key is no longer needed and can be deleted."
		}
		if creds := client.cardDAVCredentials(); creds != nil && strings.TrimSpace(creds.URL) != "" {
			result, err := client.syncCardDAV(ce.Ctx, true)
			if err != nil {
				ce.Reply("CardDAV is unlocked. Refresh it later with `carddav-sync` (now: %v).%s", err, legacy)
				return
			}
			ce.Reply("CardDAV is unlocked and the address book was re-synced (%d updated, %d added).%s", result.Updated, result.Added, legacy)
			return
		}
		ce.Reply("CardDAV is unlocked. Run `carddav-login <email> <password>` to connect your address book.%s", legacy)
	},
}

var CommandCardDAVLock = &commands.FullHandler{
	Name: "carddav-lock",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Forget the in-memory CardDAV encryption key and pause syncing",
	},
	RequiresLogin: true,
	Func: func(ce *commands.Event) {
		_, client, ok := cardDAVClientFromEvent(ce)
		if !ok {
			return
		}
		clearCardDAVRAMKey()
		if client.cardDAVLocked() {
			ce.Reply("CardDAV is locked: the in-memory key was forgotten and background syncing is paused. Run `carddav-unlock <secret>` with the same secret to unlock again; nothing is lost.")
			return
		}
		ce.Reply("The in-memory CardDAV key was forgotten. If a legacy key file is in use, encryption continues with that file.")
	},
}

var CommandCardDAVReset = &commands.FullHandler{
	Name: "carddav-reset",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Remove stored CardDAV credentials, imported names and the in-memory key (use: carddav-reset yes)",
	},
	RequiresLogin: true,
	Func: func(ce *commands.Event) {
		if len(ce.Args) != 1 || strings.ToLower(ce.Args[0]) != "yes" {
			ce.Reply("Usage: `carddav-reset yes`")
			ce.Reply("This forgets the stored CardDAV credentials, removes all imported names and photos, and locks the encryption key. The address book stays on your CardDAV server: run `carddav-unlock <secret>`, `carddav-login <email> <password>` and `carddav-sync` to fetch it again.")
			return
		}
		login, client, ok := cardDAVClientFromEvent(ce)
		if !ok {
			return
		}
		cleared, err := client.clearContactNameSources(ce.Ctx, login, contactNameSourceCardDAV, "")
		if err != nil {
			ce.Reply("Failed to remove the imported names: %v", err)
			return
		}
		if err := client.clearCardDAVCredentials(ce.Ctx); err != nil {
			ce.Reply("Failed to remove the CardDAV credentials: %v", err)
			return
		}
		if meta, _ := login.Metadata.(*LoginMetadata); meta != nil {
			meta.CardDAVKeyMode = ""
			login.Metadata = meta
			if err := login.Save(ce.Ctx); err != nil {
				ce.Reply("Failed to store the reset state: %v", err)
				return
			}
		}
		clearCardDAVRAMKey()
		ce.Reply("CardDAV was reset: credentials, imported names and photos removed (%d name(s)), and the encryption key forgotten. The address book still lives on your CardDAV server — run `carddav-unlock <secret>`, `carddav-login <email> <password>` and `carddav-sync` to fetch it again.", cleared)
	},
}
