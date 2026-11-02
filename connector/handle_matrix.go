// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/util/random"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

var (
	_ bridgev2.ReactionHandlingNetworkAPI    = (*MyNetworkClient)(nil)
	_ bridgev2.ReadReceiptHandlingNetworkAPI = (*MyNetworkClient)(nil)
)

// errMMSNeedsAppBackend is returned when an attachment cannot be sent because
// neither the file service link path nor the phone's MMS backend can carry
// it.
var errMMSNeedsAppBackend = errors.New("MMS sending needs the app backend (not available on the shell backend yet)")

var htmlTagRegex = regexp.MustCompile(`<[^>]+>`)

// HandleMatrixMessage handles messages sent from Matrix to the phone.
func (nc *MyNetworkClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if msg == nil || msg.Event == nil || msg.Portal == nil {
		return nil, fmt.Errorf("invalid Matrix message")
	}
	if nc.login == nil || nc.login.User == nil {
		return nil, fmt.Errorf("login is not loaded")
	}
	if msg.Event.Sender != nc.login.User.MXID {
		return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("only the bridge user can send messages in this room")).
			WithSendNotice(true)
	}
	meta, _ := msg.Portal.Metadata.(*PortalMetadata)
	if targetMeta := nc.mergedPortalMetadata(ctx, msg.Portal); targetMeta != nil {
		// The room was merged away; replies still reach the target thread.
		meta = targetMeta
	}
	if meta == nil || meta.ThreadID == 0 {
		return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("this room is not linked to a phone thread yet; send a message from the phone first")).
			WithSendNotice(true)
	}

	// The phone may be down; give it a grace period like jmpchat does for XMPP.
	healthCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	health, err := nc.waitForAgentHealth(healthCtx)
	if err != nil {
		errMsg := "The phone is not reachable; message was not sent"
		nc.log.Warn().Err(err).Msg(errMsg)
		sendNotice(nc.bridge.Bot, msg.Portal.MXID, errMsg)
		return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("%s: %v", errMsg, err)).WithSendNotice(true)
	}

	isAttachment := msg.Content.MsgType == event.MsgImage ||
		msg.Content.MsgType == event.MsgVideo ||
		msg.Content.MsgType == event.MsgAudio ||
		msg.Content.MsgType == event.MsgFile

	text := strings.TrimSpace(msg.Content.Body)
	if text == "" && msg.Content.FormattedBody != "" {
		text = htmlToPlainText(msg.Content.FormattedBody)
	}

	var attachments []SendAttachment
	if isAttachment {
		// The file service can turn an attachment into a text link, so MMS
		// support is only required when the media has to travel as an MMS.
		if !health.Backend.SendMMS && !nc.cdnLinkMode() {
			return nil, bridgev2.WrapErrorInStatus(errMMSNeedsAppBackend).WithSendNotice(true)
		}
		data, contentType, fileName, err := nc.downloadMatrixAttachment(ctx, msg, nc.outgoingAttachmentLimitBytes())
		if err != nil {
			return nil, bridgev2.WrapErrorInStatus(err).WithSendNotice(true)
		}
		linked := false
		text, attachments, linked, err = nc.prepareOutgoingAttachment(text, data, contentType, fileName, health.Backend.SendMMS)
		if err != nil {
			return nil, bridgev2.WrapErrorInStatus(err).WithSendNotice(true)
		}
		if linked {
			nc.log.Info().
				Str("portal_id", string(msg.Portal.ID)).
				Int("bytes", len(data)).
				Str("file_name", fileName).
				Msg("Stored an outgoing attachment in the file service; sending its link as text")
		}
	}
	if text == "" && len(attachments) == 0 {
		return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("message is empty")).WithSendNotice(true)
	}

	recipients := meta.Recipients
	if len(recipients) == 0 && meta.OtherUserID != "" {
		recipients = []string{UnescapeAddress(string(meta.OtherUserID))}
	}
	if len(recipients) == 0 {
		return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("no recipients known for this phone thread")).WithSendNotice(true)
	}

	sendID := string(msg.Event.ID)
	if sendID == "" {
		sendID = random.String(16)
	}
	// Remember the send as soon as it is dispatched: the app's message.new
	// echo can reach the event loop before the matching send.result is
	// processed, so the echo suppression must not depend on which of the two
	// arrives first.
	nc.recordCdnRefsFromText(cdnRefDirectionSent, strings.Join(recipients, ", "), string(msg.Portal.MXID), text, time.Now().UTC())
	nc.trackOutboundSend(sendID)
	req := SendRequest{
		SendID:      sendID,
		ThreadID:    meta.ThreadID,
		SubID:       meta.SubID,
		Recipients:  recipients,
		Text:        text,
		Attachments: attachments,
	}
	resp, err := nc.sendToPhone(ctx, req)
	if err != nil {
		nc.log.Err(err).Str("portal_id", string(msg.Portal.ID)).Msg("Phone send failed")
		return nil, mapAgentSendError(err)
	}

	messageID := string(nc.recordOutgoingSend(resp, sendID))

	nc.log.Info().
		Str("portal_id", string(msg.Portal.ID)).
		Str("message_id", messageID).
		Int("attachment_count", len(attachments)).
		Bool("pending", resp.Pending).
		Msg("Successfully sent message to phone")
	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:         networkid.MessageID(messageID),
			SenderID:   nc.ownUserID(),
			SenderMXID: msg.Event.Sender,
			Timestamp:  time.Now().UTC(),
			Metadata: &MessageMetadata{
				Body:          text,
				HasAttachment: len(attachments) > 0,
			},
		},
	}, nil
}

// sendToPhone dispatches an outgoing send to the active transport: the ADB
// agent HTTP API or the app pairing command queue.
func (nc *MyNetworkClient) sendToPhone(ctx context.Context, req SendRequest) (SendResponse, error) {
	if nc.device != nil {
		return nc.sendViaDevice(ctx, req)
	}
	nc.ensureAgent()
	if nc.agent == nil || strings.TrimSpace(nc.agent.BaseURL) == "" {
		return SendResponse{}, fmt.Errorf("no phone transport is configured for this login")
	}
	return nc.agent.Send(ctx, req)
}

// sendViaDevice enqueues a send command for the paired app and waits for the
// matching send.result. When the app does not answer in time the command stays
// queued and the caller falls back to a pending placeholder.
func (nc *MyNetworkClient) sendViaDevice(ctx context.Context, req SendRequest) (SendResponse, error) {
	for _, attachment := range req.Attachments {
		if len(attachment.Base64) > pairingAttachmentMaxBase64 {
			return SendResponse{}, &APIError{
				Code: "unsupported",
				Message: fmt.Sprintf("attachment %q exceeds the %d byte MMS limit",
					attachment.FileName, pairingAttachmentMaxBase64),
			}
		}
	}
	commandType := "send_sms"
	if len(req.Attachments) > 0 {
		commandType = "send_mms"
	}
	command := pairCommand{
		Type:        commandType,
		SendID:      req.SendID,
		ThreadID:    req.ThreadID,
		SubID:       req.SubID,
		Recipients:  req.Recipients,
		Text:        req.Text,
		Attachments: req.Attachments,
	}
	result, timedOut, err := nc.device.requestSend(ctx, command, pairingSendTimeout)
	if err != nil {
		return SendResponse{}, err
	}
	if timedOut {
		nc.log.Warn().
			Str("send_id", req.SendID).
			Msg("Paired app did not acknowledge the send before the timeout; using a pending placeholder")
		return SendResponse{SendID: req.SendID, Pending: true}, nil
	}
	if result.Error != nil {
		return SendResponse{}, &APIError{Code: result.Error.Code, Message: result.Error.Message}
	}
	return SendResponse{
		SendID:    req.SendID,
		MessageID: result.MessageID,
		ThreadID:  result.ThreadID,
		PartsSent: result.PartsSent,
		Pending:   result.Pending,
	}, nil
}

// trackOutboundSend registers a send this bridge is dispatching to the phone,
// so the app's message.new echo is recognized by send_id no matter whether the
// echo or the send.result is processed first. Agent-transport sends resolve
// synchronously and need no such tracking.
func (nc *MyNetworkClient) trackOutboundSend(sendID string) {
	if nc.device == nil || sendID == "" {
		return
	}
	nc.device.trackPendingSend(sendID)
}

// recordOutgoingSend stores the echo-suppression state for a send the phone
// accepted and returns the message ID the bridge stores in its database.
func (nc *MyNetworkClient) recordOutgoingSend(resp SendResponse, sendID string) networkid.MessageID {
	messageID := strings.TrimSpace(resp.MessageID)
	if messageID == "" {
		// The phone row hasn't appeared yet; keep a stable placeholder ID so
		// the Matrix event can be stored and later deduplicated by send_id.
		messageID = "pending-" + sendID
		nc.trackOutboundSend(sendID)
	}
	nc.markSentFromMatrix(networkid.MessageID(resp.MessageID), networkid.MessageID(messageID))
	return networkid.MessageID(messageID)
}

// downloadMatrixAttachment downloads the media of an outgoing Matrix message
// and returns its bytes, MIME type and display filename. maxBytes caps the
// accepted plaintext (0 disables the cap); the cap is raised for attachments
// that can travel through the file service instead of MMS.
func (nc *MyNetworkClient) downloadMatrixAttachment(ctx context.Context, msg *bridgev2.MatrixMessage, maxBytes int64) ([]byte, string, string, error) {
	contentURL := msg.Content.URL
	if contentURL == "" && msg.Content.File != nil {
		contentURL = msg.Content.File.URL
	}
	if contentURL == "" {
		return nil, "", "", fmt.Errorf("media event is missing its content URL")
	}
	if nc.bridge == nil || nc.bridge.Bot == nil {
		return nil, "", "", fmt.Errorf("bridge bot is not available for media download")
	}
	// The event carries the plaintext size; reject an oversized attachment
	// before pulling its bytes over the network.
	if maxBytes > 0 && msg.Content.Info != nil && int64(msg.Content.Info.Size) > maxBytes {
		return nil, "", "", fmt.Errorf(
			"attachment is too large (%d bytes); the limit is %d MB",
			msg.Content.Info.Size, maxBytes/(1024*1024))
	}
	data, err := nc.bridge.Bot.DownloadMedia(ctx, contentURL, msg.Content.File)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to download media from Matrix: %w", err)
	}
	if len(data) == 0 {
		return nil, "", "", fmt.Errorf("downloaded media is empty")
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, "", "", fmt.Errorf(
			"attachment is too large (%d bytes); the limit is %d MB",
			len(data), maxBytes/(1024*1024))
	}
	contentType := ""
	if msg.Content.Info != nil {
		contentType = msg.Content.Info.MimeType
	}
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	fileName := sanitizeFileName(msg.Content.GetFileName())
	if fileName == "" {
		fileName = "attachment" + extensionForContentType(contentType)
	}
	return data, contentType, fileName, nil
}

func mapAgentSendError(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Code == "mms_send_unsupported" || apiErr.Status == http.StatusNotImplemented:
			return bridgev2.WrapErrorInStatus(fmt.Errorf("MMS sending needs the app backend (not available on the shell backend yet)")).
				WithSendNotice(true)
		case apiErr.Code == "too_long":
			return bridgev2.WrapErrorInStatus(fmt.Errorf("the message is too long for the phone's SMS backend; split it into several messages or shorten it")).
				WithSendNotice(true)
		case apiErr.Code == "unsupported":
			return bridgev2.WrapErrorInStatus(fmt.Errorf("the phone backend rejected the message: %s", apiErr.Message)).
				WithSendNotice(true)
		case apiErr.Code == "send_failed":
			return bridgev2.WrapErrorInStatus(fmt.Errorf("the phone failed to send the message: %s", apiErr.Message)).
				WithSendNotice(true)
		case apiErr.Status == http.StatusUnauthorized:
			return bridgev2.WrapErrorInStatus(fmt.Errorf("the agent rejected the bridge's token; check the agent token in the bridge login")).
				WithSendNotice(true)
		}
	}
	return bridgev2.WrapErrorInStatus(fmt.Errorf("failed to send message via the phone: %w", err)).WithSendNotice(true)
}

// PreHandleMatrixReaction implements bridgev2.ReactionHandlingNetworkAPI.
func (nc *MyNetworkClient) PreHandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	rel := msg.Content.GetRelatesTo()
	emoji := rel.GetAnnotationKey()
	return bridgev2.MatrixReactionPreResponse{
		SenderID: nc.ownUserID(),
		EmojiID:  networkid.EmojiID(emoji),
		Emoji:    emoji,
	}, nil
}

// HandleMatrixReaction implements bridgev2.ReactionHandlingNetworkAPI.
//
// SMS/MMS has no reaction concept, so reactions are accepted and stored
// locally. When send_reactions_as_text is enabled, a text message is sent to
// the phone instead.
func (nc *MyNetworkClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	rel := msg.Content.GetRelatesTo()
	emoji := strings.TrimSpace(rel.GetAnnotationKey())
	if emoji == "" {
		return nil, fmt.Errorf("reaction emoji is empty")
	}
	if msg.TargetMessage == nil {
		return nil, fmt.Errorf("target message is missing")
	}
	if nc.connector.sendReactionsAsText() {
		if err := nc.sendReactionAsText(ctx, msg, emoji); err != nil {
			return nil, err
		}
	}
	return &database.Reaction{
		MessageID:  msg.TargetMessage.ID,
		SenderID:   nc.ownUserID(),
		SenderMXID: msg.Event.Sender,
		EmojiID:    networkid.EmojiID(emoji),
		Emoji:      emoji,
		Timestamp:  time.UnixMilli(msg.Event.Timestamp),
	}, nil
}

// HandleMatrixReactionRemove implements bridgev2.ReactionHandlingNetworkAPI.
// Since reactions are never sent to the phone by default, removal is a no-op.
func (nc *MyNetworkClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	return nil
}

func (nc *MyNetworkClient) sendReactionAsText(ctx context.Context, msg *bridgev2.MatrixReaction, emoji string) error {
	if msg.Portal == nil {
		return fmt.Errorf("portal is missing")
	}
	meta, _ := msg.Portal.Metadata.(*PortalMetadata)
	if targetMeta := nc.mergedPortalMetadata(ctx, msg.Portal); targetMeta != nil {
		meta = targetMeta
	}
	if meta == nil || meta.ThreadID == 0 {
		return fmt.Errorf("portal is not linked to a phone thread")
	}
	recipients := meta.Recipients
	if len(recipients) == 0 && meta.OtherUserID != "" {
		recipients = []string{UnescapeAddress(string(meta.OtherUserID))}
	}
	if len(recipients) == 0 {
		return fmt.Errorf("no recipients known for this phone thread")
	}
	targetBody := ""
	if msg.TargetMessage != nil {
		if targetMeta, ok := msg.TargetMessage.Metadata.(*MessageMetadata); ok && targetMeta != nil {
			targetBody = strings.TrimSpace(targetMeta.Body)
		}
	}
	var text string
	if targetBody != "" {
		text = fmt.Sprintf("Reacted %s to %q", emoji, truncateRunes(targetBody, 80))
	} else {
		text = fmt.Sprintf("Reacted %s", emoji)
	}
	sendID := "reaction-" + random.String(12)
	nc.trackOutboundSend(sendID)
	resp, err := nc.sendToPhone(ctx, SendRequest{
		SendID:     sendID,
		ThreadID:   meta.ThreadID,
		SubID:      meta.SubID,
		Recipients: recipients,
		Text:       text,
	})
	if err != nil {
		return mapAgentSendError(err)
	}
	nc.recordOutgoingSend(resp, sendID)
	return nil
}

func truncateRunes(input string, max int) string {
	runes := []rune(input)
	if len(runes) <= max {
		return input
	}
	return string(runes[:max]) + "…"
}

// GetUserInfo implements bridgev2.NetworkAPI.
func (nc *MyNetworkClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	if ghost == nil || ghost.ID == "" {
		return &bridgev2.UserInfo{}, nil
	}
	address := UnescapeAddress(string(ghost.ID))
	meta, _ := ghost.Metadata.(*GhostMetadata)
	name := GhostDisplayName(meta, address)
	if name == "" {
		name = address
	}
	return &bridgev2.UserInfo{
		Name:        &name,
		Identifiers: []string{address},
	}, nil
}

// GetChatInfo implements bridgev2.NetworkAPI. It always returns the full
// desired room state, so removed participants are de-ghosted/left correctly.
func (nc *MyNetworkClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	if portal == nil {
		return nil, fmt.Errorf("portal is nil")
	}
	thread := agentapi.Thread{}
	thread.ID, thread.SubID, thread.Recipients = nc.portalThreadSnapshot(portal)
	if len(thread.Recipients) == 0 && thread.ID != 0 {
		if cached, ok := nc.cachedThread(thread.ID); ok {
			thread.Recipients = cached.Recipients
			if thread.SubID == 0 {
				thread.SubID = cached.SubID
			}
		}
	}
	if thread.Recipients == nil {
		thread.Recipients = []string{}
	}
	return nc.chatInfoForThread(ctx, thread), nil
}

// GetCapabilities implements bridgev2.NetworkAPI. File/MMS support is only
// advertised when the active agent backend can actually send MMS.
func (nc *MyNetworkClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	filesSupported := false
	if health, err := nc.Health(ctx, agentHealthTTL); err == nil && health.OK && health.Backend.SendMMS {
		filesSupported = true
	}
	return buildRoomFeatures(filesSupported)
}

func buildRoomFeatures(filesSupported bool) *event.RoomFeatures {
	fileLevel := event.CapLevelUnsupported
	if filesSupported {
		fileLevel = event.CapLevelFullySupported
	}
	fileFeatures := func() *event.FileFeatures {
		return &event.FileFeatures{
			MimeTypes: map[string]event.CapabilitySupportLevel{"*/*": fileLevel},
			Caption:   fileLevel,
		}
	}
	return &event.RoomFeatures{
		MaxTextLength: 65536,
		Formatting: event.FormattingFeatureMap{
			event.FmtBold:          event.CapLevelUnsupported,
			event.FmtItalic:        event.CapLevelUnsupported,
			event.FmtUnderline:     event.CapLevelUnsupported,
			event.FmtStrikethrough: event.CapLevelUnsupported,
			event.FmtInlineCode:    event.CapLevelUnsupported,
			event.FmtCodeBlock:     event.CapLevelUnsupported,
		},
		File: map[event.CapabilityMsgType]*event.FileFeatures{
			event.MsgImage:    fileFeatures(),
			event.MsgVideo:    fileFeatures(),
			event.MsgAudio:    fileFeatures(),
			event.CapMsgVoice: fileFeatures(),
			event.MsgFile:     fileFeatures(),
		},
		Edit:     event.CapLevelUnsupported,
		Reply:    event.CapLevelPartialSupport,
		Thread:   event.CapLevelUnsupported,
		Reaction: event.CapLevelPartialSupport,
	}
}

// HandleMatrixReadReceipt implements bridgev2.ReadReceiptHandlingNetworkAPI by
// marking the corresponding phone thread as read.
func (nc *MyNetworkClient) HandleMatrixReadReceipt(ctx context.Context, msg *bridgev2.MatrixReadReceipt) error {
	if msg == nil || msg.Portal == nil {
		return nil
	}
	meta, _ := msg.Portal.Metadata.(*PortalMetadata)
	if targetMeta := nc.mergedPortalMetadata(ctx, msg.Portal); targetMeta != nil {
		meta = targetMeta
	}
	if meta == nil || meta.ThreadID == 0 {
		return nil
	}
	upToMS := msg.ReadUpTo.UnixMilli()
	if upToMS <= 0 {
		return nil
	}
	if nc.device != nil {
		// Fire-and-forget: the read command is queued and acknowledged by the
		// app's next events batch.
		nc.device.enqueue(pairCommand{Type: "mark_read", ThreadID: meta.ThreadID, SubID: meta.SubID, UpToMS: upToMS})
		return nil
	}
	if _, err := nc.agent.Read(ctx, meta.ThreadID, upToMS, nil); err != nil {
		return fmt.Errorf("mark thread read on phone: %w", err)
	}
	return nil
}

// htmlToPlainText converts a Matrix formatted body into plain text for SMS.
func htmlToPlainText(input string) string {
	replacer := strings.NewReplacer(
		"<br>", "\n",
		"<br/>", "\n",
		"<br />", "\n",
		"</p>", "\n",
		"</div>", "\n",
	)
	plain := replacer.Replace(input)
	plain = htmlTagRegex.ReplaceAllString(plain, "")
	plain = strings.ReplaceAll(plain, "&nbsp;", " ")
	plain = strings.ReplaceAll(plain, "&amp;", "&")
	plain = strings.ReplaceAll(plain, "&lt;", "<")
	plain = strings.ReplaceAll(plain, "&gt;", ">")
	plain = strings.ReplaceAll(plain, "&quot;", `"`)
	plain = strings.ReplaceAll(plain, "&#39;", "'")
	lines := strings.Split(plain, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
