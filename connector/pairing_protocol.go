// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

// This file defines the wire types and helpers of the app pairing protocol
// described in docs/PAIRING_PROTOCOL.md (v1). The bridge exposes these over
// its public listener; the app (Fossify Messages fork) is the other side.

const (
	pairingProtocolVersion  = 1
	pairingPathPrefix       = "/_smsmms/v1"
	pairingDefaultListen    = ":29322"
	pairingDefaultPublicURL = ""

	// pairingWaitMaxMS caps the queue long-poll timeout the app may request.
	pairingWaitMaxMS = 30000

	// pairingSessionTTL is how long a displayed pairing code stays valid.
	pairingSessionTTL = 10 * time.Minute
	// pairingMaxAttempts is how many failed completions/restarts a session
	// tolerates before it is burned.
	pairingMaxAttempts = 5
	// pairingCodeLength is the number of characters in a pairing code.
	pairingCodeLength = 8

	// pairingSigningPrefix starts the string the device signs.
	pairingSigningPrefix = "smsmms-pair-v1"

	// pairingDeviceOnlineWindow is how long a device may stay quiet (no queue
	// poll, event push or heartbeat) before the bridge considers it offline.
	pairingDeviceOnlineWindow = 75 * time.Second
	// pairingCommandRetention bounds how long undelivered queue commands are
	// kept for a reconnecting device; pairingQueueMaxCommands caps the queue.
	pairingCommandRetention = 24 * time.Hour
	pairingQueueMaxCommands = 512
	// pairingEventInboxMax caps the retained app -> bridge event inbox.
	pairingEventInboxMax = 1024
	// pairingSendTimeout is how long HandleMatrixMessage waits for a
	// send.result before falling back to the pending placeholders.
	pairingSendTimeout = 30 * time.Second
	// pairingPendingSendRetention bounds how long a dispatched send is
	// remembered for echo suppression by send_id.
	pairingPendingSendRetention = 30 * time.Minute
	// pairingAttachmentMaxBase64 caps one base64 attachment payload.
	pairingAttachmentMaxBase64 = 10485760
	// pairingMaxBodyBytes caps request bodies of the pairing API.
	pairingMaxBodyBytes = 4 << 20

	// pairCommandFetchMessage asks the app to push a message again with its
	// full payload (inline MMS part bytes). The bridge sends it when an event
	// arrives without the payload — for example a replayed state snapshot —
	// because it cannot fetch parts from a NATed phone itself.
	pairCommandFetchMessage = "fetch_message"
	// pairingPayloadRetryAfter is how long a fetch_message waits for an answer
	// before a replayed event asks again (the command can be skipped by a queue
	// reset or pruned while the phone is offline).
	pairingPayloadRetryAfter = 5 * time.Minute
	// pairingPayloadRequestRetention bounds how long a fetch_message request is
	// remembered, so replays of the same event do not request (or mirror) it
	// again in a loop.
	pairingPayloadRequestRetention = 30 * time.Minute

	// pairEventMergeThreads is a `merge_threads` request from the app (app ->
	// bridge): the phone threads of one fractured conversation the user
	// selected in the app, plus the thread whose room should become the merged
	// target. The bridge answers with a pairCommandMergeResult queue command.
	pairEventMergeThreads = "merge_threads"
	// pairCommandMergeResult delivers the outcome of a `merge_threads` request
	// (bridge -> app): the target room ID on success, or the validation or
	// execution error. It echoes the request's thread IDs and send id.
	pairCommandMergeResult = "merge_threads.result"
	// Error codes of a merge_threads.result: the selection was invalid
	// (`bad_request`) or applying the valid selection failed (`internal`).
	pairMergeErrorInvalid  = "bad_request"
	pairMergeErrorInternal = "internal"

	// pairingRateLimitMax / pairingRateLimitWindow throttle pairing attempts
	// per source IP (brute force protection for the 8 character code).
	pairingRateLimitMax    = 10
	pairingRateLimitWindow = time.Minute

	// pairingDeviceIDPrefix is prepended to the device ID to build the
	// deterministic login ID (stable across re-pairing).
	pairingDeviceIDPrefix = "app:"
)

// crockfordAlphabet is the Crockford base32 alphabet (no I, L, O, U).
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newPairingCode generates a random 8 character Crockford base32 code.
func newPairingCode() (string, error) {
	buf := make([]byte, pairingCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate pairing code: %w", err)
	}
	var b strings.Builder
	for _, v := range buf {
		// 256 is a multiple of 32, so the modulo is unbiased.
		b.WriteByte(crockfordAlphabet[int(v)%len(crockfordAlphabet)])
	}
	return b.String(), nil
}

// normalizePairingCode canonicalizes a user-typed code: uppercased, dashes
// and spaces removed, and the Crockford confusables I/L mapped to 1 and O to
// 0. It reports ok=false when the input cannot be a valid code.
func normalizePairingCode(code string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(code)) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		}
		if !strings.ContainsRune(crockfordAlphabet, r) {
			return "", false
		}
		b.WriteRune(r)
	}
	normalized := b.String()
	if len(normalized) != pairingCodeLength {
		return "", false
	}
	return normalized, true
}

// randomB64 returns n random bytes as unpadded base64url.
func randomB64(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// randomPairingToken generates the per-device bearer token (32 bytes).
func randomPairingToken() (string, error) {
	return randomB64(32)
}

// hashPairingToken returns the SHA-256 of a bearer token as lowercase hex.
// Only this hash is stored; the token itself is returned to the app once.
func hashPairingToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// decodeBase64URL decodes standard and URL-safe, padded and unpadded base64
// so the app can use whichever variant its platform produces.
func decodeBase64URL(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("empty value")
	}
	encodings := []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	}
	for _, encoding := range encodings {
		if data, err := encoding.DecodeString(value); err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("invalid base64 value")
}

// pairingSigningInput is the UTF-8 string the device signs when completing
// pairing: smsmms-pair-v1|<pairing_id>|<server_nonce>|<client_nonce>|<device_id>
func pairingSigningInput(pairingID, serverNonce, clientNonce, deviceID string) string {
	return strings.Join([]string{
		pairingSigningPrefix,
		pairingID,
		serverNonce,
		clientNonce,
		deviceID,
	}, "|")
}

// buildPairingPayload renders the smsmms://pair?... QR/manual payload.
func buildPairingPayload(homeserverURL, bridgeURL, code string) string {
	query := url.Values{}
	query.Set("v", "1")
	if homeserverURL = strings.TrimSpace(homeserverURL); homeserverURL != "" {
		query.Set("hs", homeserverURL)
	}
	if bridgeURL = strings.TrimSpace(bridgeURL); bridgeURL != "" {
		query.Set("b", bridgeURL)
	}
	query.Set("code", code)
	return (&url.URL{Scheme: "smsmms", Host: "pair", RawQuery: query.Encode()}).String()
}

// remoteEvent is the agent Event shape the protocol reuses, plus the extra
// payloads the pairing API adds: the full message for message.* events, and
// the outcome fields of a send.result.
type remoteEvent struct {
	agentapi.Event
	Message    *agentapi.Message `json:"message,omitempty"`
	Recipients []string          `json:"recipients,omitempty"`
	PartsSent  int               `json:"parts_sent,omitempty"`
	Pending    bool              `json:"pending,omitempty"`
	Error      *remoteEventError `json:"error,omitempty"`
	// ThreadIDs and TargetThread carry a `merge_threads` request (app ->
	// bridge). The send id correlates the request with the
	// `merge_threads.result` command the bridge answers with.
	ThreadIDs    []int64 `json:"thread_ids,omitempty"`
	TargetThread int64   `json:"target_thread,omitempty"`
	// CommandSeq is set on `ack` events and identifies the queue command the
	// device acknowledged, so the command can be pruned without parsing the
	// `ack-<seq>` send id.
	CommandSeq int64 `json:"command_seq,omitempty"`
}

// remoteEventError is the error object of a send.result event.
type remoteEventError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// remoteEventPage is one poll of the remote event source (agent HTTP API or
// the pairing server inbox).
type remoteEventPage struct {
	Events []remoteEvent
	Cursor int64
	Reset  bool
	// Online reports whether the remote side is reachable. Always true for
	// the agent transport; for app logins it tracks queue polls/heartbeats.
	Online bool
}

// pairDeviceInfo is the device block of pair/start.
type pairDeviceInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Platform   string `json:"platform,omitempty"`
	AppVersion string `json:"app_version,omitempty"`
}

// pairStartRequest is the POST /_smsmms/v1/pair/start body.
type pairStartRequest struct {
	Code        string         `json:"code"`
	HSURL       string         `json:"hs_url,omitempty"`
	Device      pairDeviceInfo `json:"device"`
	Pubkey      string         `json:"pubkey"`
	ClientNonce string         `json:"client_nonce"`
}

// pairStartResponse is the POST /_smsmms/v1/pair/start response.
type pairStartResponse struct {
	PairingID   string `json:"pairing_id"`
	ServerNonce string `json:"server_nonce"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
}

// pairCompleteRequest is the POST /_smsmms/v1/pair/complete body.
type pairCompleteRequest struct {
	PairingID string `json:"pairing_id"`
	Signature string `json:"signature"`
}

type pairUserInfo struct {
	MXID        string `json:"mxid"`
	Displayname string `json:"displayname,omitempty"`
}

type pairBridgeInfo struct {
	BaseURL         string `json:"base_url,omitempty"`
	ProtocolVersion int    `json:"protocol_version"`
}

type pairQueueInfo struct {
	WaitMaxMS int `json:"wait_max_ms"`
}

// pairCompleteResponse is the POST /_smsmms/v1/pair/complete response. The
// token is returned exactly once.
type pairCompleteResponse struct {
	DeviceID      string         `json:"device_id"`
	Token         string         `json:"token"`
	User          pairUserInfo   `json:"user"`
	HomeserverURL string         `json:"homeserver_url,omitempty"`
	Bridge        pairBridgeInfo `json:"bridge"`
	Queue         pairQueueInfo  `json:"queue"`
	// CDNBaseURL advertises the bridge-hosted encrypted file service. The app
	// only offers URL uploads when it is present.
	CDNBaseURL string `json:"cdn_base_url,omitempty"`
}

// pairInfoResponse is the GET /_smsmms/v1/info response.
type pairInfoResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	BridgeName      string `json:"bridge_name"`
	HomeserverURL   string `json:"homeserver_url,omitempty"`
	PairingEnabled  bool   `json:"pairing_enabled"`
	CDNBaseURL      string `json:"cdn_base_url,omitempty"`
}

// pairCommand is one work item in the bridge -> app queue. The send fields
// mirror the agent's POST /v1/send body.
type pairCommand struct {
	Seq         int64            `json:"seq"`
	Type        string           `json:"type"` // send_sms, send_mms, mark_read, ping, fetch_message, merge_threads.result
	SendID      string           `json:"send_id,omitempty"`
	MessageID   string           `json:"message_id,omitempty"` // fetch_message only
	ThreadID    int64            `json:"thread_id,omitempty"`
	SubID       int              `json:"sub_id,omitempty"`
	Recipients  []string         `json:"recipients,omitempty"`
	Text        string           `json:"text,omitempty"`
	Attachments []SendAttachment `json:"attachments,omitempty"`
	UpToMS      int64            `json:"up_to_ms,omitempty"`

	// merge_threads.result only: the outcome of an app-requested merge. The
	// thread IDs and target echo the request, room_id is the merged target room
	// and error is set when the merge was refused or failed.
	ThreadIDs    []int64           `json:"thread_ids,omitempty"`
	TargetThread int64             `json:"target_thread,omitempty"`
	RoomID       string            `json:"room_id,omitempty"`
	Error        *remoteEventError `json:"error,omitempty"`

	// EnqueuedAt is local bookkeeping for retention, never sent to the app.
	EnqueuedAt time.Time `json:"-"`
}

// pairQueueResponse is the GET /_smsmms/v1/queue response.
type pairQueueResponse struct {
	Cursor   int64         `json:"cursor"`
	Reset    bool          `json:"reset"`
	Commands []pairCommand `json:"commands"`
}

// pairEventsRequest is the POST /_smsmms/v1/events body.
type pairEventsRequest struct {
	DeviceID string        `json:"device_id,omitempty"`
	Cursor   int64         `json:"cursor,omitempty"`
	Events   []remoteEvent `json:"events"`
}

// pairEventsResponse is the POST /_smsmms/v1/events response.
type pairEventsResponse struct {
	Cursor   int64 `json:"cursor"`
	Accepted int   `json:"accepted"`
}

// pairHeartbeatRequest is the POST /_smsmms/v1/heartbeat body.
type pairHeartbeatRequest struct {
	DeviceID      string          `json:"device_id,omitempty"`
	BatteryPct    int             `json:"battery_pct,omitempty"`
	DefaultSMSApp bool            `json:"default_sms_app,omitempty"`
	Permissions   map[string]bool `json:"permissions,omitempty"`
	AppVersion    string          `json:"app_version,omitempty"`
	// Subscriptions optionally reports the SIMs of the phone. The bridge
	// stores them so the phone's own numbers are excluded from participants
	// (app-paired logins cannot read the adb agent's own_numbers config).
	Subscriptions []agentapi.Subscription `json:"subscriptions,omitempty"`
}

// pairHeartbeatResponse is the POST /_smsmms/v1/heartbeat response.
type pairHeartbeatResponse struct {
	OK           bool  `json:"ok"`
	ServerTimeMS int64 `json:"server_time_ms"`
}

// pairSessionResult is what a completed pairing hands to the waiting login
// flow: everything needed to create the bridgev2 login.
type pairSessionResult struct {
	DeviceID       string
	DeviceName     string
	DevicePlatform string
	AppVersion     string
	PubkeySPKI     string
	TokenSHA256    string
	PairedAtMS     int64
}

// pairSendResult is a resolved (or timed out) outgoing send.
type pairSendResult struct {
	SendID    string
	MessageID string
	ThreadID  int64
	PartsSent int
	Pending   bool
	Error     *remoteEventError
}

// mxidDisplayName returns a human-readable name for an MXID (localpart).
func mxidDisplayName(mxid string) string {
	mxid = strings.TrimSpace(mxid)
	if mxid == "" {
		return ""
	}
	withoutSigil := strings.TrimPrefix(mxid, "@")
	if idx := strings.IndexByte(withoutSigil, ':'); idx > 0 {
		return withoutSigil[:idx]
	}
	return withoutSigil
}
