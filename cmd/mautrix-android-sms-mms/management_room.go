// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"sync"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type managementRoomResolver struct {
	mu           sync.Mutex
	lastNotified map[id.UserID]id.RoomID
}

func newManagementRoomResolver() *managementRoomResolver {
	return &managementRoomResolver{
		lastNotified: make(map[id.UserID]id.RoomID),
	}
}

// chooseManagementRoom determines whether the current room should be adopted as the
// user's active management room or whether a warning should be issued.
func chooseManagementRoom(storedRoom, currentRoom id.RoomID, storedLive, currentLive, currentNewerOrEqual bool) (adopt bool, shouldWarn bool, reason string) {
	if storedRoom == currentRoom {
		return false, false, "already_current"
	}
	if !currentLive {
		return false, false, "current_not_live"
	}
	if storedRoom == "" || !storedLive {
		return true, false, "stored_empty_or_dead"
	}
	if currentNewerOrEqual {
		return true, true, "current_is_newer_candidate"
	}
	return false, true, "stored_is_newer_candidate"
}

// resolveManagementRoom checks whether currentRoom is a 2-member bot DM that should be
// marked as the user's management room. If an older/stale management room exists, it
// auto-adopts the newer room and informs the user. If multiple live management room
// candidates exist, it biases towards the most recent and warns the user.
func (r *managementRoomResolver) resolveManagementRoom(ctx context.Context, main *mxmain.BridgeMain, user *bridgev2.User, currentRoom id.RoomID) {
	if user == nil || currentRoom == "" || main == nil || main.Bridge == nil || main.Matrix == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	storedRoom := user.ManagementRoom
	if storedRoom == currentRoom {
		return
	}

	botMXID := main.Bridge.Bot.GetMXID()
	currentLive, currentTS := isLiveBotDMRoom(ctx, main, currentRoom, user.MXID, botMXID)
	if !currentLive {
		return
	}

	storedLive, storedTS := isLiveBotDMRoom(ctx, main, storedRoom, user.MXID, botMXID)
	currentNewerOrEqual := (currentTS >= storedTS)

	adopt, shouldWarn, reason := chooseManagementRoom(storedRoom, currentRoom, storedLive, currentLive, currentNewerOrEqual)
	if !adopt && !shouldWarn {
		return
	}

	if adopt {
		user.ManagementRoom = currentRoom
		if err := main.Bridge.DB.User.Update(ctx, user.User); err != nil {
			main.Matrix.Log.Error().Err(err).
				Str("user_id", string(user.MXID)).
				Str("room_id", string(currentRoom)).
				Msg("Failed to persist updated management room in database")
			return
		}

		main.Matrix.Log.Info().
			Str("user_id", string(user.MXID)).
			Str("old_room", string(storedRoom)).
			Str("new_room", string(currentRoom)).
			Str("reason", reason).
			Msg("Adopted direct message room as active management room")

		if r.lastNotified[user.MXID] != currentRoom {
			r.lastNotified[user.MXID] = currentRoom
			var noticeMsg string
			if storedLive && storedRoom != "" {
				noticeMsg = fmt.Sprintf("Notice: This room has been set as your active management room (previous candidate: `%s`). Commands can be run here without prefix.", storedRoom)
			} else if storedRoom != "" {
				noticeMsg = fmt.Sprintf("Notice: This room has been set as your active management room (replaced stale room `%s`). Commands can be run here without prefix.", storedRoom)
			} else {
				noticeMsg = "Notice: This room has been set as your active management room. Commands can be run here without prefix."
			}
			_, _ = main.Bridge.Bot.SendMessage(ctx, currentRoom, event.EventMessage, &event.Content{Parsed: &event.MessageEventContent{
				MsgType: event.MsgNotice,
				Body:    noticeMsg,
			}}, nil)
		}
	} else if shouldWarn {
		if r.lastNotified[user.MXID] != currentRoom {
			r.lastNotified[user.MXID] = currentRoom
			noticeMsg := fmt.Sprintf("Notice: Your active management room is `%s`. Commands here require the `%s` prefix, or you can run `%s set-management-room` in this room to switch.", storedRoom, main.Bridge.Config.CommandPrefix, main.Bridge.Config.CommandPrefix)
			_, _ = main.Bridge.Bot.SendMessage(ctx, currentRoom, event.EventMessage, &event.Content{Parsed: &event.MessageEventContent{
				MsgType: event.MsgNotice,
				Body:    noticeMsg,
			}}, nil)
		}
	}
}

func isLiveBotDMRoom(ctx context.Context, main *mxmain.BridgeMain, roomID id.RoomID, userMXID, botMXID id.UserID) (bool, int64) {
	if roomID == "" || main == nil || main.Matrix == nil {
		return false, 0
	}
	members, err := main.Matrix.GetMembers(ctx, roomID)
	if err != nil || len(members) != 2 {
		return false, 0
	}
	userMem, okUser := members[userMXID]
	botMem, okBot := members[botMXID]
	if !okUser || !okBot || !isJoinedOrInvited(userMem) || !isJoinedOrInvited(botMem) {
		return false, 0
	}
	ts := getRoomMaxMemberTS(ctx, main, roomID, userMXID, botMXID)
	return true, ts
}

func getRoomMaxMemberTS(ctx context.Context, main *mxmain.BridgeMain, roomID id.RoomID, userMXID, botMXID id.UserID) int64 {
	if main == nil || main.Matrix == nil || main.Matrix.AS == nil {
		return 0
	}
	resp, err := main.Matrix.AS.BotClient().Members(ctx, roomID)
	if err != nil || resp == nil {
		return 0
	}
	var maxTS int64
	for _, evt := range resp.Chunk {
		if evt != nil && (evt.Sender == userMXID || evt.Sender == botMXID) {
			if evt.Timestamp > maxTS {
				maxTS = evt.Timestamp
			}
		}
	}
	return maxTS
}
