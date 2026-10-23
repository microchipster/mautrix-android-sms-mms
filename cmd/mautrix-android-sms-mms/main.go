// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"strings"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
	"maunium.net/go/mautrix/event"

	"mautrix-android-sms-mms/connector"
)

var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	connector := &connector.MyConnector{}

	m := mxmain.BridgeMain{
		Name:        "mautrix-android-sms-mms",
		Description: "A Matrix bridge to Android SMS/MMS",
		Version:     "0.1.0",
		URL:         "",
		Connector:   connector,
	}
	m.PostInit = func() {
		installDirectMessageCommandHandler(&m)
	}

	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}

func installDirectMessageCommandHandler(main *mxmain.BridgeMain) {
	if main.Matrix == nil || main.Matrix.EventProcessor == nil || main.Bridge == nil {
		return
	}

	// The bridge queue runs after this handler. AsyncHandlers would race the
	// body rewrite, so keep per-event handler ordering when using async AS
	// transactions.
	if main.Matrix.EventProcessor.ExecMode == appservice.AsyncHandlers {
		main.Matrix.EventProcessor.ExecMode = appservice.AsyncLoop
	}

	gate := newRoomEncryptionGate()
	resolver := newManagementRoomResolver()

	main.Matrix.EventProcessor.PrependHandler(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		if evt.Sender == "" || evt.RoomID == "" || evt.Sender == main.Bridge.Bot.GetMXID() {
			return
		}

		message := evt.Content.AsMessage()
		if message == nil || !message.MsgType.IsText() || strings.TrimSpace(message.Body) == "" {
			return
		}

		// Skip portal rooms immediately - command prefixing and management room encryption
		// are only for non-portal direct management rooms.
		portal, err := main.Bridge.GetPortalByMXID(ctx, evt.RoomID)
		if err == nil && portal != nil {
			return
		}

		user, err := main.Bridge.GetUserByMXID(ctx, evt.Sender)
		if err != nil || user == nil {
			return
		}

		// The Matrix leg to the bot must be encrypted in every management room. Verify or
		// establish encryption before any command routing happens;
		// a failure to prove encryption drops the message (fail closed).
		establishedNow, err := gate.ensureBotRoomEncrypted(ctx, main, evt.RoomID)
		if err != nil {
			main.Matrix.Log.Error().Err(err).Str("room_id", string(evt.RoomID)).Msg("Blocked message because room encryption could not be verified")
			_, _ = main.Bridge.Bot.SendMessage(ctx, evt.RoomID, event.EventMessage, &event.Content{Parsed: &event.MessageEventContent{
				MsgType: event.MsgNotice,
				Body:    fmt.Sprintf("Cannot process message: room encryption could not be verified (%v).", err),
			}}, nil)
			return
		}
		if establishedNow {
			main.Matrix.Log.Info().Str("room_id", string(evt.RoomID)).Msg("Enabled encryption in previously unencrypted room")
			_, _ = main.Bridge.Bot.SendMessage(ctx, evt.RoomID, event.EventMessage, &event.Content{Parsed: &event.MessageEventContent{
				MsgType: event.MsgNotice,
				Body:    "This room is now encrypted with the bridge. Please send your message again.",
			}}, nil)
			return
		}

		members, err := main.Matrix.GetMembers(ctx, evt.RoomID)
		if err != nil || len(members) != 2 {
			return
		}
		if !isJoinedOrInvited(members[evt.Sender]) || !isJoinedOrInvited(members[main.Bridge.Bot.GetMXID()]) {
			return
		}

		// A private two-member room with the bridge bot is a management room candidate.
		// Automatically adopt it or bias towards the most recent candidate with warnings.
		resolver.resolveManagementRoom(ctx, main, user, evt.RoomID)

		prefix := main.Bridge.Config.CommandPrefix
		if prefix == "" || strings.HasPrefix(message.Body, prefix) {
			return
		}

		// Prefixing lets normal bridgev2 command routing handle
		// permissions, replies, and command errors.
		message.Body = prefix + " " + message.Body
	})
}

func isJoinedOrInvited(member *event.MemberEventContent) bool {
	return member != nil && member.Membership.IsInviteOrJoin()
}
