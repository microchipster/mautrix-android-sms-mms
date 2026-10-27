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
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// CommandPing shows the logins's bridge state and agent health.
var CommandPing = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No login found")
			return
		}
		client, ok := login.Client.(*MyNetworkClient)
		if !ok || client == nil {
			ce.Reply("Network client not available")
			return
		}
		lines := []string{
			fmt.Sprintf("Login: %s", login.ID),
			fmt.Sprintf("Remote: %s", strings.TrimSpace(login.RemoteName)),
			fmt.Sprintf("Bridge state: %s", loginBridgeStateSummary(login)),
		}
		if meta, ok := login.Metadata.(*LoginMetadata); ok && meta != nil {
			if meta.Transport == LoginTransportApp {
				lines = append(lines,
					"Transport: app pairing",
					fmt.Sprintf("Device: %s (%s)", firstNotEmpty(meta.DeviceName, meta.DeviceModel), meta.DeviceID),
					fmt.Sprintf("Paired at: %s", formatDeviceTimeMS(meta.PairedAtMS)),
				)
			} else {
				lines = append(lines,
					fmt.Sprintf("Agent URL: %s", meta.AgentURL),
					fmt.Sprintf("Device: %s (%s)", meta.DeviceModel, meta.DeviceSerial),
					fmt.Sprintf("Send backend: %s", meta.SendBackend),
				)
			}
		}
		healthCtx, cancel := context.WithTimeout(ce.Ctx, 10*time.Second)
		defer cancel()
		health, err := client.Health(healthCtx, 0)
		switch {
		case err != nil:
			lines = append(lines, fmt.Sprintf("Agent health: unreachable (%v)", err))
		case !health.OK:
			lines = append(lines, fmt.Sprintf("Agent health: not ok (%s)", health.Error))
		default:
			lines = append(lines, fmt.Sprintf(
				"Agent health: ok (protocol %d, root=%t, send_mms=%t, mark_read=%t)",
				health.ProtocolVersion, health.Root, health.Backend.SendMMS, health.Backend.CanMarkRead))
		}
		ce.Reply("%s", strings.Join(lines, "\n"))
	},
	Name:    "ping",
	Aliases: []string{"status", "login-status"},
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Show login state and ping the phone agent",
	},
	RequiresLogin: true,
}

// CommandSync re-queues history backfill for all portals of the login.
var CommandSync = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No login found")
			return
		}
		client, ok := login.Client.(*MyNetworkClient)
		if !ok || client == nil {
			ce.Reply("Network client not available")
			return
		}
		full, lookback, err := parseSyncArgs(ce.Args, client.connector.historyDefaultDays())
		if err != nil {
			ce.Reply("%v", err)
			return
		}
		notify := func(msg string) {
			sendNotice(ce.Bot, ce.OrigRoomID, msg)
		}
		if err := client.StartFullSync(ce.Ctx, notify, full, lookback); err != nil {
			ce.Reply("%v", err)
			return
		}
		ce.Reply("Sync queued. I will post progress here.")
	},
	Name: "sync",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Re-queue history backfill for bridged rooms. Usage: sync [full|<duration>] (default: configured window, usually 30d)",
	},
	RequiresLogin: true,
}

// CommandSyncStatus reports how much history is bridged.
var CommandSyncStatus = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No login found")
			return
		}
		rows, err := ce.Bridge.DB.UserPortal.GetAllForLogin(ce.Ctx, login.UserLogin)
		if err != nil {
			ce.Reply("Failed to list portals: %v", err)
			return
		}
		rooms, synced := 0, 0
		for _, row := range rows {
			if row == nil {
				continue
			}
			portal, err := ce.Bridge.GetExistingPortalByKey(ce.Ctx, row.Portal)
			if err != nil || portal == nil {
				continue
			}
			if portal.MXID != "" {
				rooms++
			}
			if meta, ok := portal.Metadata.(*PortalMetadata); ok && meta != nil && meta.LastSyncedAt != nil {
				synced++
			}
		}
		ce.Reply("Sync status: %d bridged room(s), %d with recorded history progress.", rooms, synced)
	},
	Name: "sync-status",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Show how many rooms are bridged and how far history sync has progressed",
	},
	RequiresLogin: true,
}

// CommandListLogins lists the logins of this Matrix user.
var CommandListLogins = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		logins := ce.User.GetUserLogins()
		if len(logins) == 0 {
			ce.Reply("No logins found")
			return
		}
		entries := make([]string, 0, len(logins))
		for _, login := range logins {
			if login == nil {
				continue
			}
			state := "unknown"
			if login.BridgeState != nil {
				prev := login.BridgeState.GetPrev()
				if prev.StateEvent != "" {
					state = string(prev.StateEvent)
				}
			}
			remote := strings.TrimSpace(login.RemoteName)
			if remote == "" {
				remote = "(unknown)"
			}
			entries = append(entries, fmt.Sprintf("%s - %s (%s)", login.ID, remote, state))
		}
		sort.Strings(entries)
		ce.Reply("%s", strings.Join(entries, "\n"))
	},
	Name: "list-logins",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAdmin,
		Description: "List login IDs for this Matrix user",
	},
	RequiresLogin: true,
}

// CommandListUsers lists the phone numbers you have DM rooms with.
var CommandListUsers = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No login found")
			return
		}
		rows, err := ce.Bridge.DB.UserPortal.GetAllForLogin(ce.Ctx, login.UserLogin)
		if err != nil {
			ce.Reply("Failed to list portals: %v", err)
			return
		}
		users := map[string]struct{}{}
		for _, row := range rows {
			portal, err := ce.Bridge.GetPortalByKey(ce.Ctx, row.Portal)
			if err != nil || portal == nil {
				continue
			}
			if portal.RoomType != database.RoomTypeDM {
				continue
			}
			address := UnescapeAddress(string(portal.OtherUserID))
			if address == "" {
				continue
			}
			users[address] = struct{}{}
		}
		if len(users) == 0 {
			ce.Reply("No users found")
			return
		}
		list := make([]string, 0, len(users))
		for address := range users {
			list = append(list, address)
		}
		sort.Strings(list)
		ce.Reply("%s", strings.Join(list, "\n"))
	},
	Name: "list-users",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "List phone numbers you have DM rooms with",
	},
	RequiresLogin: true,
}

// CommandDeleteAllPortals removes all bridged rooms and logs the phone out.
var CommandDeleteAllPortals = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if len(ce.Args) == 0 || (ce.Args[0] != "yes" && ce.Args[0] != "--yes") {
			ce.Reply("This will remove all bridged rooms and the space. Run: delete-all-portals yes")
			return
		}
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No login found")
			return
		}
		if err := deleteAllPortals(ce, login); err != nil {
			ce.Reply("%v", err)
			return
		}
		ce.Reply("Deleted all bridged rooms and the space. Management room kept. You can log in again with `login`.")
	},
	Name: "delete-all-portals",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAdmin,
		Description: "Remove all bridged rooms, delete the space, and log out (use: delete-all-portals yes)",
	},
	RequiresLogin: true,
}

// CommandLogout logs a phone login out without deleting rooms by default.
var CommandLogout = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		loginID, purge, err := parseLogoutArgs(ce.Args)
		if err != nil {
			ce.Reply("%v", err)
			return
		}
		login, err := resolveLoginForCommand(ce, loginID)
		if err != nil {
			ce.Reply("%v", err)
			return
		}
		if purge {
			if err := deleteAllPortals(ce, login); err != nil {
				ce.Reply("%v", err)
				return
			}
			ce.Reply("Deleted all bridged rooms and the space. Management room kept. You can log in again with `login`.")
			return
		}
		if login.Client != nil {
			login.Client.LogoutRemote(ce.Ctx)
			login.Client.Disconnect()
		}
		if login.BridgeState != nil {
			login.BridgeState.Send(status.BridgeState{StateEvent: status.StateLoggedOut})
		}
		ce.Reply("Logged out without removing rooms. Use `logout purge` to delete rooms and the space.")
	},
	Name: "logout",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAdmin,
		Description: "Log out without removing rooms (use: logout [login_id] [purge])",
	},
	RequiresLogin: true,
}

// CommandRebindRoom points a phone thread portal at an existing Matrix room.
// This is how thread-ID changes (phone restore/reinstall) are handled without
// creating a new room.
var CommandRebindRoom = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if len(ce.Args) != 2 {
			ce.Reply("Usage: rebind-room <thread_key_or_address> <matrix_room_id>")
			return
		}
		target := strings.TrimSpace(ce.Args[0])
		roomID := id.RoomID(strings.TrimSpace(ce.Args[1]))
		if roomID == "" {
			ce.Reply("Matrix room ID cannot be empty")
			return
		}
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No login found")
			return
		}
		client, _ := login.Client.(*MyNetworkClient)

		portalKey, err := resolveRebindPortalKey(ce.Ctx, ce.Bridge, login, target)
		if err != nil {
			ce.Reply("%v", err)
			return
		}
		portal, err := ce.Bridge.GetPortalByKey(ce.Ctx, portalKey)
		if err != nil || portal == nil {
			ce.Reply("Failed to load or create portal %q: %v", portalKey.ID, err)
			return
		}

		var supersededNote string
		if existing, err := ce.Bridge.GetPortalByMXID(ce.Ctx, roomID); err == nil && existing != nil && existing.PortalKey != portal.PortalKey {
			oldPortalID := existing.ID
			existing.MXID = ""
			if err := existing.Save(ce.Ctx); err != nil {
				ce.Reply("Failed to unbind portal %s from room %s: %v", oldPortalID, roomID, err)
				return
			}
			supersededNote = fmt.Sprintf("\n- Unbound previous portal: `%s`", oldPortalID)
		}

		oldRoom := portal.MXID
		portal.MXID = roomID
		if err := portal.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save rebound portal: %v", err)
			return
		}
		if client != nil {
			client.ensureBackfillTaskForPortal(ce.Ctx, portal)
		}
		ce.Bridge.QueueRemoteEvent(login, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatResync,
				PortalKey: portalKey,
				Timestamp: time.Now(),
			},
			ChatInfo: nil,
		})
		var oldNote string
		if oldRoom != "" && oldRoom != roomID {
			oldNote = fmt.Sprintf("\n- Previous room: `%s`", oldRoom)
		}
		ce.Reply("Rebound **%s** to Matrix room `%s`.%s%s\nHistory backfill was queued.", portalKey.ID, roomID, oldNote, supersededNote)
	},
	Name: "rebind-room",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Rebind a phone thread (or address) to an existing Matrix room (usage: rebind-room <thread_key_or_address> <matrix_room_id>)",
		Args:        "<thread_key_or_address> <matrix_room_id>",
	},
	RequiresLogin: true,
}

// CommandDevices lists the app-paired devices of this Matrix user.
var CommandDevices = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		logins := appPairedLogins(ce.User)
		if len(logins) == 0 {
			ce.Reply("No paired phone app devices found. Run `login app-pair` to pair one.")
			return
		}
		connector, _ := ce.Bridge.Network.(*MyConnector)
		deviceIndex := map[string]*pairedDevice{}
		if connector != nil {
			for _, device := range connector.pairingServer().devicesForUser(ce.Ctx, ce.Bridge, ce.User.MXID) {
				deviceIndex[device.deviceID] = device
			}
		}
		lines := make([]string, 0, len(logins))
		for _, login := range logins {
			meta, _ := login.Metadata.(*LoginMetadata)
			if meta == nil {
				continue
			}
			lastSeen := meta.LastSeenMS
			online := false
			appVersion := strings.TrimSpace(meta.AppVersion)
			if device := deviceIndex[meta.DeviceID]; device != nil {
				snapshot := device.snapshot()
				if !snapshot.LastSeen.IsZero() {
					lastSeen = snapshot.LastSeen.UnixMilli()
				}
				online = snapshot.Online
				if snapshot.AppVersion != "" {
					appVersion = snapshot.AppVersion
				}
			}
			liveness := "offline"
			if online {
				liveness = "online"
			}
			line := fmt.Sprintf("- `%s` %s — %s, last seen %s, paired %s",
				meta.DeviceID,
				firstNotEmpty(meta.DeviceName, meta.DeviceModel, "(unknown)"),
				liveness,
				formatDeviceTimeMS(lastSeen),
				formatDeviceTimeMS(meta.PairedAtMS),
			)
			if appVersion != "" {
				line += fmt.Sprintf(", app %s", appVersion)
			}
			line += fmt.Sprintf(", login `%s`", login.ID)
			lines = append(lines, line)
		}
		sort.Strings(lines)
		ce.Reply("%s\n\nUse `unpair <device_id|all>` to revoke a device.", strings.Join(lines, "\n"))
	},
	Name:    "devices",
	Aliases: []string{"paired-devices"},
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "List app-paired phone devices",
	},
	RequiresLogin: true,
}

// CommandUnpair revokes a paired device's token and removes its bridge login.
var CommandUnpair = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if len(ce.Args) != 1 {
			ce.Reply("Usage: unpair <device_id|all>")
			return
		}
		target := strings.TrimSpace(ce.Args[0])
		if target == "" {
			ce.Reply("Usage: unpair <device_id|all>")
			return
		}
		logins := appPairedLogins(ce.User)
		if target != "all" {
			filtered := make([]*bridgev2.UserLogin, 0, 1)
			for _, login := range logins {
				meta, _ := login.Metadata.(*LoginMetadata)
				if meta != nil && (meta.DeviceID == target || string(login.ID) == target) {
					filtered = append(filtered, login)
				}
			}
			logins = filtered
		}
		if len(logins) == 0 {
			if target == "all" {
				ce.Reply("No paired phone app devices found.")
			} else {
				ce.Reply("No paired device found with ID %q. Run `devices` to list them.", target)
			}
			return
		}
		connector, _ := ce.Bridge.Network.(*MyConnector)
		unpaired := make([]string, 0, len(logins))
		for _, login := range logins {
			meta, _ := login.Metadata.(*LoginMetadata)
			deviceID := string(login.ID)
			if meta != nil && meta.DeviceID != "" {
				deviceID = meta.DeviceID
			}
			if connector != nil && meta != nil && meta.DeviceID != "" {
				// Drop the token immediately so in-flight requests fail even
				// before the login deletion finishes.
				connector.pairingServer().revokeDevice(meta.DeviceID)
			}
			login.Delete(ce.Ctx, status.BridgeState{StateEvent: status.StateLoggedOut}, bridgev2.DeleteOpts{
				LogoutRemote: true,
			})
			unpaired = append(unpaired, deviceID)
		}
		sort.Strings(unpaired)
		ce.Reply("Unpaired %d device(s): %s", len(unpaired), strings.Join(unpaired, ", "))
	},
	Name: "unpair",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAuth,
		Description: "Revoke an app-paired device (usage: unpair <device_id|all>)",
		Args:        "<device_id|all>",
	},
	RequiresLogin: true,
}

// appPairedLogins returns the user's app-paired (transport "app") logins.
func appPairedLogins(user *bridgev2.User) []*bridgev2.UserLogin {
	if user == nil {
		return nil
	}
	logins := make([]*bridgev2.UserLogin, 0, 1)
	for _, login := range user.GetUserLogins() {
		if login == nil {
			continue
		}
		meta, _ := login.Metadata.(*LoginMetadata)
		if meta == nil || meta.Transport != LoginTransportApp {
			continue
		}
		logins = append(logins, login)
	}
	return logins
}

// formatDeviceTimeMS renders a millisecond timestamp for the management room.
func formatDeviceTimeMS(value int64) string {
	if value <= 0 {
		return "never"
	}
	return time.UnixMilli(value).UTC().Format("2006-01-02 15:04:05 MST")
}

func sendNotice(bot bridgev2.MatrixAPI, roomID id.RoomID, msg string) {
	if bot == nil || roomID == "" || msg == "" {
		return
	}
	content := &event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    msg,
	}
	_, _ = bot.SendMessage(context.Background(), roomID, event.EventMessage, &event.Content{Parsed: content}, nil)
}

func loginBridgeStateSummary(login *bridgev2.UserLogin) string {
	if login == nil || login.BridgeState == nil {
		return "unknown"
	}
	prev := login.BridgeState.GetPrev()
	state := string(prev.StateEvent)
	if state == "" {
		state = "unknown"
	}
	if prev.Message != "" {
		state += " (" + prev.Message + ")"
	}
	return state
}

func parseSyncArgs(args []string, defaultLookback time.Duration) (bool, time.Duration, error) {
	if len(args) == 0 {
		return false, defaultLookback, nil
	}
	if len(args) > 1 {
		return false, 0, fmt.Errorf("Usage: sync [full|<duration>] (e.g. sync 30d, sync full)")
	}
	arg := strings.ToLower(strings.TrimSpace(args[0]))
	if arg == "full" {
		return true, 0, nil
	}
	lookback, err := parseLookbackDuration(arg)
	if err != nil {
		return false, 0, fmt.Errorf("Invalid duration %q (use 7d, 30d, 12h, or full)", arg)
	}
	return false, lookback, nil
}

func parseLookbackDuration(arg string) (time.Duration, error) {
	if strings.HasSuffix(arg, "d") {
		num := strings.TrimSuffix(arg, "d")
		if num == "" {
			return 0, fmt.Errorf("empty duration")
		}
		days, err := strconv.Atoi(num)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid day value")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(arg)
}

func parseLogoutArgs(args []string) (string, bool, error) {
	if len(args) == 0 {
		return "", false, nil
	}
	if len(args) > 2 {
		return "", false, fmt.Errorf("Usage: logout [login_id] [purge]")
	}
	if strings.EqualFold(args[0], "purge") {
		if len(args) > 1 {
			return "", false, fmt.Errorf("Usage: logout [login_id] [purge]")
		}
		return "", true, nil
	}
	loginID := strings.TrimSpace(args[0])
	if loginID == "" {
		return "", false, fmt.Errorf("Usage: logout [login_id] [purge]")
	}
	if len(args) == 2 {
		if !strings.EqualFold(args[1], "purge") {
			return "", false, fmt.Errorf("Usage: logout [login_id] [purge]")
		}
		return loginID, true, nil
	}
	return loginID, false, nil
}

func resolveLoginForCommand(ce *commands.Event, loginID string) (*bridgev2.UserLogin, error) {
	if loginID == "" {
		logins := ce.User.GetUserLoginIDs()
		if len(logins) == 0 {
			return nil, fmt.Errorf("No login found")
		}
		if len(logins) > 1 {
			return nil, fmt.Errorf("Multiple logins found. Use: logout <login_id> [purge]")
		}
		login := ce.User.GetDefaultLogin()
		if login == nil {
			return nil, fmt.Errorf("No login found")
		}
		return login, nil
	}
	login, err := ce.Bridge.GetExistingUserLoginByID(ce.Ctx, networkid.UserLoginID(loginID))
	if err != nil {
		return nil, fmt.Errorf("Failed to load login %q: %w", loginID, err)
	}
	if login == nil || login.User == nil || login.User.MXID != ce.User.MXID {
		return nil, fmt.Errorf("Login %q not found", loginID)
	}
	return login, nil
}

func deleteAllPortals(ce *commands.Event, login *bridgev2.UserLogin) error {
	userPortals, err := ce.Bridge.DB.UserPortal.GetAllForLogin(ce.Ctx, login.UserLogin)
	if err != nil {
		return fmt.Errorf("failed to list portals: %w", err)
	}
	portals := make([]*bridgev2.Portal, 0, len(userPortals))
	for _, up := range userPortals {
		portal, err := ce.Bridge.GetExistingPortalByKey(ce.Ctx, up.Portal)
		if err != nil || portal == nil {
			continue
		}
		portals = append(portals, portal)
	}

	bridgev2.DeleteManyPortals(ce.Ctx, portals, nil)
	for _, up := range userPortals {
		_ = ce.Bridge.DB.UserPortal.Delete(ce.Ctx, up)
	}

	if spaceRoom, err := login.GetSpaceRoom(ce.Ctx); err == nil && spaceRoom != "" {
		_ = ce.Bridge.Bot.DeleteRoom(ce.Ctx, spaceRoom, false)
	}

	login.Delete(ce.Ctx, status.BridgeState{StateEvent: status.StateLoggedOut}, bridgev2.DeleteOpts{
		LogoutRemote:     true,
		DontCleanupRooms: true,
		BlockingCleanup:  true,
	})
	return nil
}

// resolveRebindPortalKey turns a user-provided target into a portal key. It
// accepts an existing portal ID (thread-… or addr-…), a phone number, a short
// code or another canonical address.
func resolveRebindPortalKey(ctx context.Context, br *bridgev2.Bridge, login *bridgev2.UserLogin, target string) (networkid.PortalKey, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return networkid.PortalKey{}, fmt.Errorf("thread key or address cannot be empty")
	}
	if strings.HasPrefix(target, "thread-") || strings.HasPrefix(target, "addr-") {
		return networkid.PortalKey{ID: networkid.PortalID(target)}, nil
	}
	address := CanonicalAddress(target)
	if address == "" {
		return networkid.PortalKey{}, fmt.Errorf("invalid thread key or address %q", target)
	}
	addressKey := networkid.PortalKey{ID: MakeAddressPortalID(address)}
	if portal, err := br.GetExistingPortalByKey(ctx, addressKey); err == nil && portal != nil {
		return addressKey, nil
	}
	// No address portal yet: look for a DM portal of this login that talks to
	// the address.
	if login != nil {
		rows, err := br.DB.UserPortal.GetAllForLogin(ctx, login.UserLogin)
		if err == nil {
			for _, row := range rows {
				portal, err := br.GetExistingPortalByKey(ctx, row.Portal)
				if err != nil || portal == nil {
					continue
				}
				if UnescapeAddress(string(portal.OtherUserID)) == address {
					return portal.PortalKey, nil
				}
			}
		}
	}
	return addressKey, nil
}

func managementRoomHelpText() string {
	return "Quick help (Android SMS/MMS):\n" +
		"- ping\n" +
		"- sync [full|30d] (default: configured window; 'full' fetches the whole phone history)\n" +
		"- sync-status\n" +
		"- rebind-room <thread_key_or_address> <matrix_room_id>\n" +
		"- list-logins\n" +
		"- list-users\n" +
		"- stats [24h|7d|30d|all]\n" +
		"- label <phone> <label|--clear>\n" +
		"- labels\n" +
		"- spam status | spam recent [count]\n" +
		"- spam why <number> | spam policy <number> <policy|clear>\n" +
		"- spam import <file>\n" +
		"- ttl status | ttl run [--dry-run|--apply]\n" +
		"- merge <room-id> <room-id> [<room-id> ...] [--into <room-id>] [--yes]\n" +
		"- merges\n" +
		"- unmerge <room-id>\n" +
		"- devices\n" +
		"- unpair <device_id|all>\n" +
		"- logout [login_id]\n" +
		"- logout [login_id] purge\n" +
		"- delete-all-portals yes\n" +
		"- cdn list | revoke <id> | stats | gc\n" +
		"\n" +
		"One Matrix room is maintained per phone thread; participant changes update the room " +
		"instead of creating a new one. If a phone restore changes thread IDs, use rebind-room " +
		"to point the new thread at the existing room. If Android fractures one conversation " +
		"across several threads, use merge to route them into one room."
}
