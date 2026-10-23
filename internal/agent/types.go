// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

// Domain + API types shared between the agent's provider layer and HTTP server.

// Attachment is a non-text MMS part.
type Attachment struct {
	PartID      int64  `json:"part_id"`
	ContentType string `json:"content_type"`
	FileName    string `json:"file_name,omitempty"`
	Size        int64  `json:"size,omitempty"`
	URL         string `json:"url,omitempty"`
	// Base64 carries the part bytes inline for transports where the bridge
	// cannot fetch them over HTTP afterwards (the app transport: the phone
	// polls the bridge and is not reachable from it). The adb agent transport
	// leaves it empty and serves bytes via GET /v1/mms/{id}/parts/{part_id}.
	Base64 string `json:"base64,omitempty"`
}

// Message is the normalized representation of an SMS or MMS message.
type Message struct {
	ID              string       `json:"id"` // sms:<sub_id>:<row_id> | mms:<sub_id>:<row_id>
	ThreadID        int64        `json:"thread_id"`
	SubID           int          `json:"sub_id"`
	TimestampMS     int64        `json:"timestamp_ms"`
	TimestampSentMS int64        `json:"timestamp_sent_ms,omitempty"`
	Sender          string       `json:"sender,omitempty"` // empty = sent from this phone
	Recipients      []string     `json:"recipients"`
	Body            string       `json:"body,omitempty"`
	IsMMS           bool         `json:"is_mms"`
	Type            int          `json:"type"` // 1 inbox, 2 sent, 3 draft, 4 outbox, 5 failed, 6 queued
	Read            bool         `json:"read"`
	Seen            bool         `json:"seen"`
	Status          int          `json:"status"`
	TextOnly        bool         `json:"text_only,omitempty"`
	Attachments     []Attachment `json:"attachments,omitempty"`
}

// Thread maps to one Android telephony conversation (one Matrix room).
type Thread struct {
	ID            int64    `json:"id"`
	SubID         int      `json:"sub_id"`
	DateMS        int64    `json:"date_ms"`
	MessageCount  int      `json:"message_count"`
	Recipients    []string `json:"recipients"`
	Snippet       string   `json:"snippet,omitempty"`
	Read          bool     `json:"read"`
	HasAttachment bool     `json:"has_attachment"`
	UnreadCount   int      `json:"unread_count,omitempty"`
}

// Event is an incremental change notification for the bridge.
type Event struct {
	Seq           int64  `json:"seq"`
	Type          string `json:"type"` // message.new, message.updated, mms.downloaded, thread.updated
	MessageID     string `json:"message_id,omitempty"`
	ThreadID      int64  `json:"thread_id"`
	SubID         int    `json:"sub_id"`
	TimestampMS   int64  `json:"timestamp_ms"`
	Sender        string `json:"sender,omitempty"`
	IsMMS         bool   `json:"is_mms"`
	HasAttachment bool   `json:"has_attachment"`
	Preview       string `json:"preview,omitempty"`
	SendID        string `json:"send_id,omitempty"`
}

// Subscription describes one SIM.
type Subscription struct {
	SubID       int    `json:"sub_id"`
	SimSlot     int    `json:"sim_slot,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Carrier     string `json:"carrier,omitempty"`
	Number      string `json:"number,omitempty"`
	DefaultSMS  bool   `json:"default_sms,omitempty"`
}

// DeviceInfo describes the attached phone.
type DeviceInfo struct {
	Serial         string `json:"serial"`
	Model          string `json:"model,omitempty"`
	AndroidSDK     int    `json:"android_sdk,omitempty"`
	AndroidRelease string `json:"android_release,omitempty"`
}

// BackendInfo tells the bridge which operations the agent can actually perform.
type BackendInfo struct {
	Read         string `json:"read"` // content-provider | sqlite
	SendSMS      string `json:"send_sms,omitempty"`
	SendMMS      bool   `json:"send_mms"`
	CanMarkRead  bool   `json:"can_mark_read"`
	Root         bool   `json:"root"`
	DefaultSMSApp string `json:"default_sms_app,omitempty"`
}

// Health is the /v1/health payload.
type Health struct {
	OK              bool          `json:"ok"`
	ProtocolVersion int           `json:"protocol_version"`
	Device          DeviceInfo    `json:"device"`
	Root            bool          `json:"root"`
	Backend         BackendInfo   `json:"backend"`
	DefaultSMSApp   string        `json:"default_sms_app,omitempty"`
	Subscriptions   []Subscription `json:"subscriptions,omitempty"`
	Capabilities    []string      `json:"capabilities"`
	Error           string        `json:"error,omitempty"`
}

// ProtocolVersion is the current agent API version.
const ProtocolVersion = 1

// ThreadRecipient is one participant of a thread (canonical address + optional contact name).
type ThreadRecipient struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
}
