// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"path/filepath"
	"strings"
	"unicode"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

// errNoIntent is returned when media cannot be uploaded because the caller had
// no Matrix intent.
var errNoIntent = errors.New("no Matrix intent available for media upload")

// classifyContentTypeMessageType maps an MMS part content type to a Matrix
// message type. When treatGenericAsFile is false, unknown content types come
// back as text so the caller can fall back to a different representation.
func classifyContentTypeMessageType(contentType string, treatGenericAsFile bool) event.MessageType {
	contentType = strings.TrimSpace(strings.ToLower(contentType))
	if idx := strings.IndexByte(contentType, ';'); idx != -1 {
		contentType = strings.TrimSpace(contentType[:idx])
	}
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return event.MsgImage
	case strings.HasPrefix(contentType, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(contentType, "audio/"):
		return event.MsgAudio
	case strings.HasPrefix(contentType, "text/"):
		return event.MsgText
	case contentType != "" && treatGenericAsFile:
		return event.MsgFile
	default:
		return event.MsgText
	}
}

// sanitizeFileName makes an MMS part filename safe for Matrix: no directory
// components, no control characters, and never empty.
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.TrimSpace(strings.Trim(name, "."))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == 0:
			continue
		case unicode.IsControl(r):
			continue
		default:
			b.WriteRune(r)
		}
	}
	name = strings.TrimSpace(b.String())
	if name == "" || name == "." || name == ".." {
		return ""
	}
	if len(name) > 128 {
		name = name[:128]
	}
	return name
}

// attachmentData returns the bytes of one MMS attachment.
//
// Transports that cannot serve parts over HTTP after the event is delivered
// inline them as base64 (app transport). The adb agent transport leaves the
// inline data empty and serves bytes via GET /v1/mms/{id}/parts/{part_id}.
func attachmentData(ctx context.Context, agent *AgentClient, mmsID int64, attachment agentapi.Attachment) ([]byte, string, error) {
	if attachment.Base64 != "" {
		data, err := base64.StdEncoding.DecodeString(attachment.Base64)
		if err != nil {
			return nil, "", fmt.Errorf("decode inline attachment for part %d: %w", attachment.PartID, err)
		}
		return data, attachment.ContentType, nil
	}
	if attachment.PartID == 0 || mmsID == 0 {
		return nil, "", fmt.Errorf("attachment has no part id and no inline data")
	}
	if agent == nil {
		// App-paired logins have no inbound HTTP agent: the phone must inline
		// attachment bytes. Never dereference a nil client (this used to panic).
		return nil, "", fmt.Errorf("attachment part %d has no inline data and the login has no agent client", attachment.PartID)
	}
	return agent.PartBytes(ctx, mmsID, attachment.PartID)
}

// attachmentFileName picks a display filename for an MMS attachment, adding a
// content-type based extension when the phone did not provide a usable name.
func attachmentFileName(attachment agentapi.Attachment) string {
	if name := sanitizeFileName(attachment.FileName); name != "" {
		return name
	}
	ext := extensionForContentType(attachment.ContentType)
	return "attachment" + ext
}

func extensionForContentType(contentType string) string {
	contentType = strings.TrimSpace(strings.ToLower(contentType))
	if idx := strings.IndexByte(contentType, ';'); idx != -1 {
		contentType = strings.TrimSpace(contentType[:idx])
	}
	if contentType == "" {
		return ""
	}
	switch contentType {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "video/3gpp":
		return ".3gp"
	case "audio/mpeg":
		return ".mp3"
	case "audio/amr":
		return ".amr"
	case "audio/ogg":
		return ".ogg"
	case "text/plain":
		return ".txt"
	case "application/pdf":
		return ".pdf"
	}
	if exts, err := mime.ExtensionsByType(contentType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// bodyFromFileName produces a non-empty message body for an attachment-only
// message.
func bodyFromFileName(fileName string) string {
	fileName = strings.TrimSpace(fileName)
	if fileName == "" {
		return "(attachment)"
	}
	return fileName
}

// attachmentDataURIless base64-encodes attachment bytes for the agent send
// request, which expects the raw base64 payload without a data: URI prefix.
func attachmentDataURIless(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// uploadAttachmentData uploads bytes to the portal room and returns the mxc
// URI, the optional encryption info and the FileInfo to attach to the event.
// It returns an error when the upload fails, and the error can be used to
// decide whether the message can still be sent as text.
func uploadAttachmentData(ctx context.Context, intent bridgev2.MatrixAPI, roomID id.RoomID, data []byte, fileName, contentType string) (id.ContentURIString, *event.EncryptedFileInfo, *event.FileInfo, error) {
	if intent == nil {
		return "", nil, nil, errNoIntent
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if fileName == "" {
		fileName = "attachment" + extensionForContentType(contentType)
	}
	mxc, encryptedFile, err := intent.UploadMedia(ctx, roomID, data, fileName, contentType)
	if err != nil {
		return "", nil, nil, err
	}
	info := &event.FileInfo{
		MimeType: contentType,
		Size:     len(data),
	}
	return mxc, encryptedFile, info, nil
}
