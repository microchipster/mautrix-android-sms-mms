// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

// This file implements the bridge's own outgoing-attachment route through the
// file service described in docs/cdn-file-service.md. When the service is
// active in "link" mode, an outgoing Matrix attachment is encrypted
// (AES-256-GCM, exactly like the app's uploader) and stored in the service,
// and the phone receives the link as message text instead of the MMS
// attachment. Every attachment takes the link path by default; a positive
// send_threshold keeps smaller media on the carrier. Any encryption or storage
// failure falls back to the ordinary attachment path, so a send is never lost
// because the service is down.
//
// Unlike the app, the bridge holds the plaintext before it encrypts it (it
// downloads the media from Matrix), so the fragment key exists in bridge
// memory for the duration of the upload. The stored object and every public
// response still carry ciphertext only.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// Outgoing-attachment modes of the file service (cdn.send_mode).
const (
	// cdnSendModeLink uploads outgoing attachments to the file service and
	// sends their link as text (above send_threshold when one is set).
	cdnSendModeLink = "link"
	// cdnSendModeNever always sends attachments through the phone.
	cdnSendModeNever = "never"
)

// cdnSendViaLink is the pure routing decision: in link mode a non-empty
// plaintext is linked. A positive threshold restricts the link path to
// attachments above it; zero (the default) links every attachment, and mode
// "never" links none.
func cdnSendViaLink(mode string, thresholdBytes, plaintextBytes int64) bool {
	if mode != cdnSendModeLink || plaintextBytes <= 0 {
		return false
	}
	return thresholdBytes <= 0 || plaintextBytes > thresholdBytes
}

// cdnEncryptObject encrypts one plaintext for the file service exactly like
// the app's uploader: a fresh random 32-byte AES-256 key and 12-byte GCM
// nonce, no additional authenticated data, and the stored body
// nonce || ciphertext || tag. The key becomes the link fragment and is never
// sent to the service.
func cdnEncryptObject(plaintext []byte) (key, object []byte, err error) {
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, nil, fmt.Errorf("generate cdn key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("create cdn cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("create cdn cipher mode: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate cdn nonce: %w", err)
	}
	return key, gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// cdnLinkMessageText renders the phone message that replaces a linked
// attachment: the caption (when the Matrix event had one and it is not just
// the filename again) plus one labeled link line, so the recipient knows what
// the link holds.
func cdnLinkMessageText(caption, fileName, link string) string {
	line := strings.TrimSpace(link)
	label := strings.TrimSpace(fileName)
	if label != "" && line != "" {
		line = label + ": " + line
	}
	caption = strings.TrimSpace(caption)
	if caption != "" && strings.EqualFold(caption, label) {
		// Clients without a real caption repeat the filename as the event
		// body; the labeled link line already names the file.
		caption = ""
	}
	if caption == "" || line == "" {
		return caption + line
	}
	return caption + "\n" + line
}

// cdnLinkMode reports whether outgoing attachments are routed through the
// file service: an active service in "link" mode.
func (nc *MyNetworkClient) cdnLinkMode() bool {
	return nc != nil && nc.connector.cdnSendMode() == cdnSendModeLink
}

// outgoingAttachmentLimitBytes returns the largest plaintext the bridge
// downloads for one outgoing attachment: the carrier MMS limit, raised to the
// file service's per-object capacity when link mode can carry bigger files.
func (nc *MyNetworkClient) outgoingAttachmentLimitBytes() int64 {
	limit := nc.connector.maxAttachmentSizeBytes()
	if !nc.cdnLinkMode() {
		return limit
	}
	if cdnLimit := nc.connector.cdnPlaintextCapacityBytes(); cdnLimit > limit {
		limit = cdnLimit
	}
	return limit
}

// cdnLinkForAttachment encrypts and stores one downloaded attachment when
// link mode applies, returning the shareable link. An empty link with a nil
// error means "send it as an attachment"; an error means link mode applied
// but the upload failed and the caller should fall back.
func (nc *MyNetworkClient) cdnLinkForAttachment(data []byte) (string, error) {
	service := nc.connector.cdnService()
	if service == nil {
		return "", nil
	}
	if !cdnSendViaLink(nc.connector.cdnSendMode(), nc.connector.cdnSendThresholdBytes(), int64(len(data))) {
		return "", nil
	}
	owner := ""
	if nc.login != nil && nc.login.User != nil {
		owner = string(nc.login.User.MXID)
	}
	if owner == "" {
		return "", fmt.Errorf("no Matrix user available to own the file service entry")
	}
	key, object, err := cdnEncryptObject(data)
	if err != nil {
		return "", err
	}
	upload, err := service.Store(owner, object, nc.connector.cdnSendExpiry())
	if err != nil {
		return "", err
	}
	return service.FileURL(upload.ID) + "#" + base64.RawURLEncoding.EncodeToString(key), nil
}

// prepareOutgoingAttachment decides how one downloaded outgoing attachment is
// sent: as a file-service link (text) when link mode applies, or as an
// ordinary attachment. A failed encryption or upload falls back to the
// attachment path, so the send is never lost; when the phone cannot send MMS
// a fallback instead fails with the MMS-unavailable error, because nothing
// else can carry the media.
func (nc *MyNetworkClient) prepareOutgoingAttachment(caption string, data []byte, contentType, fileName string, sendMMS bool) (string, []SendAttachment, bool, error) {
	link, err := nc.cdnLinkForAttachment(data)
	if err == nil && link != "" {
		return cdnLinkMessageText(caption, fileName, link), nil, true, nil
	}
	if err != nil {
		nc.log.Warn().Err(err).
			Int("bytes", len(data)).
			Msg("Failed to store an outgoing attachment in the file service; sending it as an attachment instead")
	}
	if !sendMMS {
		if err != nil {
			return caption, nil, false, fmt.Errorf("%w (the file service upload also failed: %v)", errMMSNeedsAppBackend, err)
		}
		return caption, nil, false, errMMSNeedsAppBackend
	}
	// The download cap was raised for the link path, so the fallback must
	// re-apply the carrier MMS limit: an attachment the phone cannot send
	// fails with the size error instead of a transport rejection.
	if limit := nc.connector.maxAttachmentSizeBytes(); limit > 0 && int64(len(data)) > limit {
		return caption, nil, false, fmt.Errorf(
			"attachment is too large (%d bytes); the limit is %d MB", len(data), limit/(1024*1024))
	}
	return caption, []SendAttachment{sendAttachmentFromData(data, contentType, fileName)}, false, nil
}
