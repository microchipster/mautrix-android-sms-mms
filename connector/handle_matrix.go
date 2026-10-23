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
	if meta == nil || meta.ThreadID == 0 {
		return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("this room is not linked to a phone thread yet; send a message from the phone first")).
			WithSendNotice(true)
	}

	// The agent may be down; give it a grace period like jmpchat does for XMPP.
	healthCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	health, err := nc.waitForAgentHealth(healthCtx)
	if err != nil {
		errMsg := "The phone agent is not reachable; message was not sent"
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
		if !health.Backend.SendMMS {
			return nil, bridgev2.WrapErrorInStatus(fmt.Errorf("MMS sending needs the app backend (not available on the shell backend yet)")).
				WithSendNotice(true)
		}
		attachment, err := nc.downloadMatrixAttachment(ctx, msg)
		if err != nil {
			return nil, bridgev2.WrapErrorInStatus(err).WithSendNotice(true)
		}
		attachments = append(attachments, attachment)
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
	req := SendRequest{
		SendID:      sendID,
		ThreadID:    meta.ThreadID,
		SubID:       meta.SubID,
		Recipients:  recipients,
		Text:        text,
		Attachments: attachments,
	}
	resp, err := nc.agent.Send(ctx, req)
	if err != nil {
		nc.log.Err(err).Str("portal_id", string(msg.Portal.ID)).Msg("Agent send failed")
		return nil, mapAgentSendError(err)
	}

	messageID := strings.TrimSpace(resp.MessageID)
	if messageID == "" {
		// The phone row hasn't appeared yet; keep a stable placeholder ID so
		// the Matrix event can be stored and later deduplicated.
		messageID = "pending-" + sendID
	}
	nc.markSentFromMatrix(networkid.MessageID(resp.MessageID), networkid.MessageID(messageID))

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

// downloadMatrixAttachment downloads the media of an outgoing Matrix message
// and packages it for the agent send API.
func (nc *MyNetworkClient) downloadMatrixAttachment(ctx context.Context, msg *bridgev2.MatrixMessage) (SendAttachment, error) {
	contentURL := msg.Content.URL
	if contentURL == "" && msg.Content.File != nil {
		contentURL = msg.Content.File.URL
	}
	if contentURL == "" {
		return SendAttachment{}, fmt.Errorf("media event is missing its content URL")
	}
	if nc.bridge == nil || nc.bridge.Bot == nil {
		return SendAttachment{}, fmt.Errorf("bridge bot is not available for media download")
	}
	data, err := nc.bridge.Bot.DownloadMedia(ctx, contentURL, msg.Content.File)
	if err != nil {
		return SendAttachment{}, fmt.Errorf("failed to download media from Matrix: %w", err)
	}
	if len(data) == 0 {
		return SendAttachment{}, fmt.Errorf("downloaded media is empty")
	}
	if maxSize := nc.connector.maxAttachmentSizeBytes(); maxSize > 0 && int64(len(data)) > maxSize {
		return SendAttachment{}, fmt.Errorf(
			"attachment is too large (%d bytes); the limit is %d MB",
			len(data), maxSize/(1024*1024))
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
	return SendAttachment{
		ContentType: contentType,
		FileName:    fileName,
		Base64:      attachmentDataURIless(data),
	}, nil
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
		case apiErr.Status == http.StatusUnauthorized:
			return bridgev2.WrapErrorInStatus(fmt.Errorf("the agent rejected the bridge's token; check the agent token in the bridge login")).
				WithSendNotice(true)
		}
	}
	return bridgev2.WrapErrorInStatus(fmt.Errorf("failed to send message via agent: %w", err)).WithSendNotice(true)
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
	resp, err := nc.agent.Send(ctx, SendRequest{
		SendID:     "reaction-" + random.String(12),
		ThreadID:   meta.ThreadID,
		SubID:      meta.SubID,
		Recipients: recipients,
		Text:       text,
	})
	if err != nil {
		return mapAgentSendError(err)
	}
	nc.markSentFromMatrix(networkid.MessageID(resp.MessageID))
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
	if meta == nil || meta.ThreadID == 0 {
		return nil
	}
	upToMS := msg.ReadUpTo.UnixMilli()
	if upToMS <= 0 {
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
