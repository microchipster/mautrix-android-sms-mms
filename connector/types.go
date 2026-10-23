// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// LoginMetadata stores the agent (phone host) connection and sync state for a
// bridge login. One login == one phone/device managed by one android-sms-mms-adb-agent.
type LoginMetadata struct {
	// AgentURL / AgentToken point at the android-sms-mms-adb-agent that manages the phone.
	AgentURL   string `json:"agent_url,omitempty"`
	AgentToken string `json:"agent_token,omitempty"`

	// Device identity reported by the agent.
	DeviceSerial   string `json:"device_serial,omitempty"`
	DeviceModel    string `json:"device_model,omitempty"`
	AndroidVersion string `json:"android_version,omitempty"`
	DefaultSMSApp  string `json:"default_sms_app,omitempty"`

	// SendBackend is the mechanism the agent uses to send SMS/MMS
	// (e.g. "termux", "helper-app", "app-api").
	SendBackend string `json:"send_backend,omitempty"`

	// AgentProtocolVersion and AgentCapabilities describe the agent API surface
	// so the bridge can degrade gracefully with older agents.
	AgentProtocolVersion int      `json:"agent_protocol_version,omitempty"`
	AgentCapabilities    []string `json:"agent_capabilities,omitempty"`

	// SIM subscriptions: sub_id -> display name/number (dual-SIM support).
	Subscriptions map[int]SubscriptionInfo `json:"subscriptions,omitempty"`

	LastSyncAt *time.Time `json:"last_sync_at,omitempty"`
}

// SubscriptionInfo describes one SIM slot.
type SubscriptionInfo struct {
	SubID        int    `json:"sub_id"`
	PhoneNumber  string `json:"phone_number,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	IsDefaultSMS bool   `json:"is_default_sms,omitempty"`
}

// New creates a new instance for database registration.
func (m *LoginMetadata) New() any {
	return &LoginMetadata{}
}

// GhostMetadata stores additional remote metadata for a Matrix ghost
// (a phone number / short code / sender ID that talks to the phone).
type GhostMetadata struct {
	RemoteUserID string `json:"remote_user_id,omitempty"` // E.164, short code or alphanumeric sender ID
	RemoteName   string `json:"remote_name,omitempty"`    // contact name as known by the phone
	AvatarURL    string `json:"avatar_url,omitempty"`
	CustomName   string `json:"custom_name,omitempty"`
}

// New creates a new instance for database registration.
func (m *GhostMetadata) New() any {
	return &GhostMetadata{}
}

// PortalMetadata stores additional metadata for a Matrix room that maps to an
// Android telephony thread.
type PortalMetadata struct {
	// ThreadID is the telephony thread ID (content://mms-sms threads), and
	// SubID the SIM subscription. Together they are the stable conversation
	// identity: the Matrix room lives for as long as the phone thread does.
	ThreadID int64 `json:"thread_id,omitempty"`
	SubID    int   `json:"sub_id,omitempty"`

	// Recipients is the current participant set (canonical addresses).
	// For DMs it has exactly one entry and OtherUserID is set.
	Recipients []string `json:"recipients,omitempty"`

	OtherUserID         networkid.UserID    `json:"other_user_id,omitempty"` // For DMs
	InitialName         string              `json:"initial_name,omitempty"`
	InitialAvatar       id.ContentURIString `json:"initial_avatar_mxc,omitempty"`
	CreatedAt           time.Time           `json:"created_at,omitempty"`
	LastSyncedAt        *time.Time          `json:"last_synced_at,omitempty"`
	RemoteTopic         string              `json:"remote_topic,omitempty"`
	LastMessageID       string              `json:"last_message_id,omitempty"`
	LastServerMessageID string              `json:"last_server_message_id,omitempty"`
	Tags                []string            `json:"tags,omitempty"`
	Notes               map[string]string   `json:"notes,omitempty"`

	// IsGroup is true for group MMS threads (more than one recipient).
	IsGroup bool `json:"is_group,omitempty"`

	// ReadState is the last telephony message ID we consider read on the phone.
	ReadState string `json:"read_state,omitempty"`
}

// New creates a new instance for database registration.
func (m *PortalMetadata) New() any {
	return &PortalMetadata{}
}

// MessageMetadata stores per-message data the bridge needs later.
//
// BridgeV2 does not persist message bodies by default, but iMessage-style
// tapbacks (SMS texts such as `Liked “see you at 8”`) only carry the quoted
// text of the message they react to. Keeping the body around lets the bridge
// find that message and turn the tapback into a Matrix reaction instead of
// forwarding it as a confusing text message. HasAttachment additionally lets
// the bridge refuse tapbacks against media-only messages.
type MessageMetadata struct {
	Body          string `json:"body,omitempty"`
	HasAttachment bool   `json:"has_attachment,omitempty"`
}

// New creates a new instance for database registration.
func (m *MessageMetadata) New() any {
	return &MessageMetadata{}
}
