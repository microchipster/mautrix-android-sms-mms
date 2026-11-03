// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// Login transport identifiers stored in LoginMetadata.Transport.
const (
	// LoginTransportAgent is the ADB agent transport (also the default when
	// the field is empty, for logins created before app pairing existed).
	LoginTransportAgent = "adb"
	// LoginTransportApp is the in-app transport: the Fossify Messages fork
	// polls the bridge's pairing API for work and pushes events back.
	LoginTransportApp = "app"
)

// LoginMetadata stores the agent (phone host) connection and sync state for a
// bridge login. One login == one phone/device managed by one android-sms-mms-adb-agent.
type LoginMetadata struct {
	// AgentURL / AgentToken point at the android-sms-mms-adb-agent that manages the phone.
	AgentURL   string `json:"agent_url,omitempty"`
	AgentToken string `json:"agent_token,omitempty"`

	// Transport distinguishes app-paired logins ("app") from agent logins
	// ("adb", or empty for historical logins). App logins use the pairing
	// API instead of an ADB agent HTTP endpoint.
	Transport string `json:"transport,omitempty"`

	// App pairing identity (set for Transport "app").
	DeviceID    string `json:"device_id,omitempty"`
	DeviceName  string `json:"device_name,omitempty"`
	PubkeySPKI  string `json:"pubkey_spki,omitempty"`
	TokenSHA256 string `json:"token_sha256,omitempty"`
	PairedAtMS  int64  `json:"paired_at_ms,omitempty"`
	LastSeenMS  int64  `json:"last_seen_ms,omitempty"`
	AppVersion  string `json:"app_version,omitempty"`

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

	// NumberLabels maps the phone's own numbers (canonical E.164) to short
	// labels used in room-name suffixes, e.g. "(sms-personal)". Assigned with
	// the number-label command; empty means every room just ends in "(sms)".
	NumberLabels map[string]string `json:"number_labels,omitempty"`

	// NumberLabelsPrompted records that the bridge has already asked the user
	// to assign number labels (so the prompt is only sent once per login).
	NumberLabelsPrompted bool `json:"number_labels_prompted,omitempty"`

	LastSyncAt *time.Time `json:"last_sync_at,omitempty"`

	// CardDAV stores the address book credentials and sync state for this login.
	CardDAV *CardDAVCredentials `json:"carddav,omitempty"`
}

// SubscriptionInfo describes one SIM slot.
type SubscriptionInfo struct {
	SubID        int    `json:"sub_id"`
	PhoneNumber  string `json:"phone_number,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	Carrier      string `json:"carrier,omitempty"`
	IsDefaultSMS bool   `json:"is_default_sms,omitempty"`
}

// New creates a new instance for database registration.
func (m *LoginMetadata) New() any {
	return &LoginMetadata{}
}

// GhostMetadata stores additional remote metadata for a Matrix ghost
// (a phone number / short code / sender ID that talks to the phone).
type GhostMetadata struct {
	RemoteUserID     string `json:"remote_user_id,omitempty"` // E.164, short code or alphanumeric sender ID
	RemoteName       string `json:"remote_name,omitempty"`    // contact name as known by the phone
	AvatarURL        string `json:"avatar_url,omitempty"`
	CustomName       string `json:"custom_name,omitempty"`
	CardDAVName      string `json:"carddav_name,omitempty"`
	CardDAVPhotoHash string `json:"carddav_photo_hash,omitempty"`
	// Labels are free-form tags assigned with the `label` command (or the
	// automatic "new contact" label). They are metadata-only and never sent
	// to the phone.
	Labels []string `json:"labels,omitempty"`
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

	// MergedInto is set when this portal was merged into another portal (the
	// target). Incoming events for the merged-away thread are then routed to
	// the target room; the source portal keeps its own identity and metadata.
	MergedInto networkid.PortalID `json:"merged_into,omitempty"`
	// MergedFrom lists the portals that were merged into this one. It is the
	// reverse index of MergedInto and keeps the target's participant union
	// stable across thread updates of the merged sources.
	MergedFrom []networkid.PortalID `json:"merged_from,omitempty"`
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
	// SpamAction, SpamScore and SpamReasons record the anti-spam verdict for
	// the message (empty SpamAction means no verdict was recorded). They carry
	// no message content, only ids/pattern names for `spam recent` and
	// ad-hoc SQL.
	SpamAction  string   `json:"spam_action,omitempty"`
	SpamScore   int      `json:"spam_score,omitempty"`
	SpamReasons []string `json:"spam_reasons,omitempty"`
}

// New creates a new instance for database registration.
func (m *MessageMetadata) New() any {
	return &MessageMetadata{}
}
