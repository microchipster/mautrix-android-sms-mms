// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command pairsim is a development-only "phone simulator" for the SMS/MMS app
// pairing protocol described in docs/PAIRING_PROTOCOL.md. It acts like the
// Fossify Messages bridge app: it pairs against a locally running bridge,
// then long-polls the work queue, acknowledges commands and pushes events.
//
// It exists so the whole app-pairing flow can be exercised end to end without
// a cloud homeserver or an actual phone. See docs/testing-e2e-local.md for
// the full local runbook. Do not run this against a production bridge: the
// device IDs and phone numbers it invents are fake and it answers sends with
// fabricated SMS rows.
//
// Typical use:
//
//	pairsim -bridge-url http://127.0.0.1:29322 -code ABCD1234
//
// The pairing code is shown by `!sms login app-pair` in the bridge
// management room (also printed in the bridge log). By default pairsim posts
// one fake incoming SMS after pairing so a portal room exists, then serves
// Matrix -> phone sends until interrupted.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	pairingPathPrefix     = "/_smsmms/v1"
	pairingSigningPrefix  = "smsmms-pair-v1"
	defaultDevicePlatform = "android"
	defaultAppVersion     = "pairsim/0.1.0"
	// heartbeatInterval must stay below the bridge's 75s online window.
	heartbeatInterval = 30 * time.Second
)

type simConfig struct {
	bridgeURL  string
	code       string
	mode       string
	deviceID   string
	deviceName string
	platform   string
	appVersion string

	waitMS          int
	seed            bool
	simSubID        int
	simThreadID     int64
	firstRow        int64
	firstSeq        int64
	simLocalNumber  string
	simRemoteNumber string
}

type sim struct {
	cfg simConfig

	http        *http.Client
	token       string
	seq         int64 // app -> bridge event sequence
	eventCursor int64 // app -> bridge event cursor (last accepted seq)
	queueCursor int64 // bridge -> app command cursor (last seen command seq)
	nextRow     int64 // fake provider row IDs for messages the app "sends"
}

// ---------------------------------------------------------------------------
// Wire types (mirrors docs/PAIRING_PROTOCOL.md; duplicated here so the tool
// stays a standalone stdlib-only binary).

type pairInfoResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	BridgeName      string `json:"bridge_name"`
	HomeserverURL   string `json:"homeserver_url"`
	PairingEnabled  bool   `json:"pairing_enabled"`
}

type pairDeviceInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Platform   string `json:"platform,omitempty"`
	AppVersion string `json:"app_version,omitempty"`
}

type pairStartRequest struct {
	Code        string         `json:"code"`
	HSURL       string         `json:"hs_url,omitempty"`
	Device      pairDeviceInfo `json:"device"`
	Pubkey      string         `json:"pubkey"`
	ClientNonce string         `json:"client_nonce"`
}

type pairStartResponse struct {
	PairingID   string `json:"pairing_id"`
	ServerNonce string `json:"server_nonce"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
}

type pairCompleteRequest struct {
	PairingID string `json:"pairing_id"`
	Signature string `json:"signature"`
}

type pairCompleteResponse struct {
	DeviceID string `json:"device_id"`
	Token    string `json:"token"`
	User     struct {
		MXID        string `json:"mxid"`
		Displayname string `json:"displayname"`
	} `json:"user"`
	HomeserverURL string `json:"homeserver_url"`
	Bridge        struct {
		BaseURL         string `json:"base_url"`
		ProtocolVersion int    `json:"protocol_version"`
	} `json:"bridge"`
	Queue struct {
		WaitMaxMS int `json:"wait_max_ms"`
	} `json:"queue"`
}

type pairCommand struct {
	Seq         int64    `json:"seq"`
	Type        string   `json:"type"`
	SendID      string   `json:"send_id,omitempty"`
	ThreadID    int64    `json:"thread_id,omitempty"`
	SubID       int      `json:"sub_id,omitempty"`
	Recipients  []string `json:"recipients,omitempty"`
	Text        string   `json:"text,omitempty"`
	UpToMS      int64    `json:"up_to_ms,omitempty"`
	Attachments []struct {
		ContentType string `json:"content_type"`
		FileName    string `json:"file_name,omitempty"`
		Base64      string `json:"base64,omitempty"`
	} `json:"attachments,omitempty"`
}

type pairQueueResponse struct {
	Cursor   int64         `json:"cursor"`
	Reset    bool          `json:"reset"`
	Commands []pairCommand `json:"commands"`
}

type pairEventsRequest struct {
	DeviceID string        `json:"device_id,omitempty"`
	Cursor   int64         `json:"cursor,omitempty"`
	Events   []postedEvent `json:"events"`
}

type pairEventsResponse struct {
	Cursor   int64 `json:"cursor"`
	Accepted int   `json:"accepted"`
}

type pairHeartbeatRequest struct {
	DeviceID      string          `json:"device_id,omitempty"`
	BatteryPct    int             `json:"battery_pct,omitempty"`
	DefaultSMSApp bool            `json:"default_sms_app,omitempty"`
	Permissions   map[string]bool `json:"permissions,omitempty"`
	AppVersion    string          `json:"app_version,omitempty"`
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// postedEvent is the agent Event shape plus the extra pairing payloads.
type postedEvent struct {
	Seq           int64       `json:"seq"`
	Type          string      `json:"type"`
	MessageID     string      `json:"message_id,omitempty"`
	ThreadID      int64       `json:"thread_id,omitempty"`
	SubID         int         `json:"sub_id,omitempty"`
	TimestampMS   int64       `json:"timestamp_ms,omitempty"`
	Sender        string      `json:"sender,omitempty"`
	IsMMS         bool        `json:"is_mms,omitempty"`
	HasAttachment bool        `json:"has_attachment,omitempty"`
	Preview       string      `json:"preview,omitempty"`
	SendID        string      `json:"send_id,omitempty"`
	PartsSent     int         `json:"parts_sent,omitempty"`
	Pending       bool        `json:"pending,omitempty"`
	Recipients    []string    `json:"recipients,omitempty"`
	Message       *simMessage `json:"message,omitempty"`
}

// simMessage is the agent Message shape of one phone-side row.
type simMessage struct {
	ID              string   `json:"id"`
	ThreadID        int64    `json:"thread_id"`
	SubID           int      `json:"sub_id"`
	TimestampMS     int64    `json:"timestamp_ms"`
	TimestampSentMS int64    `json:"timestamp_sent_ms,omitempty"`
	Sender          string   `json:"sender"`
	Recipients      []string `json:"recipients"`
	Body            string   `json:"body,omitempty"`
	IsMMS           bool     `json:"is_mms"`
	Type            int      `json:"type"` // 1 inbox, 2 sent
	Read            bool     `json:"read"`
	Seen            bool     `json:"seen"`
	Status          int      `json:"status"`
}

// ---------------------------------------------------------------------------
// Flag parsing

func parseFlags() simConfig {
	cfg := simConfig{}
	flag.StringVar(&cfg.bridgeURL, "bridge-url", "http://127.0.0.1:29322", "Bridge pairing API base URL")
	flag.StringVar(&cfg.code, "code", "", "Pairing code displayed by the bridge management room login flow (required in pair/listen mode)")
	flag.StringVar(&cfg.mode, "mode", "listen", "What to do after `pair`: `listen` (queue/heartbeat loop) or `pair` (exit after printing the token)")
	flag.StringVar(&cfg.deviceID, "device-id", "", "Device ID to report (default android-sim-<random>)")
	flag.StringVar(&cfg.deviceName, "device-name", "Pairsim Pixel", "Device display name to report")
	flag.StringVar(&cfg.platform, "platform", defaultDevicePlatform, "Device platform to report")
	flag.StringVar(&cfg.appVersion, "app-version", defaultAppVersion, "App version to report")
	flag.IntVar(&cfg.waitMS, "wait-ms", 25000, "Queue long-poll wait in milliseconds (capped by the bridge)")
	flag.BoolVar(&cfg.seed, "seed", true, "After pairing, post one fake incoming SMS so a portal room exists")
	flag.IntVar(&cfg.simSubID, "sub-id", 1, "SIM subscription ID used for fake messages")
	flag.Int64Var(&cfg.simThreadID, "thread-id", 7, "Phone thread ID used for fake messages")
	flag.Int64Var(&cfg.firstRow, "first-row", 9001, "First fake telephony provider row ID (bump it to avoid clashing with previous runs)")
	flag.Int64Var(&cfg.firstSeq, "first-seq", 0, "First app->bridge event sequence (bump it when re-running against the same device)")
	flag.StringVar(&cfg.simLocalNumber, "local-number", "+15550001111", "Simulated phone's own number")
	flag.StringVar(&cfg.simRemoteNumber, "remote-number", "+15551234567", "Remote number used for fake incoming messages")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "pairsim pairs with a local mautrix-android-sms-mms bridge like the phone app would,\n")
		fmt.Fprintf(out, "then answers queue commands and pushes events until interrupted (dev tool).\n\n")
		fmt.Fprintf(out, "Flow: GET /info -> POST /pair/start (ephemeral ECDSA P-256 key + nonce) ->\n")
		fmt.Fprintf(out, "      sign `%s|<pairing_id>|<server_nonce>|<client_nonce>|<device_id>` ->\n", pairingSigningPrefix)
		fmt.Fprintf(out, "      POST /pair/complete -> token -> GET /queue long-poll -> POST /events, POST /heartbeat.\n\n")
		fmt.Fprintf(out, "Usage of pairsim:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if cfg.deviceID == "" {
		cfg.deviceID = "android-sim-" + randomHex(4)
	}
	if cfg.mode != "pair" && cfg.mode != "listen" {
		fmt.Fprintf(os.Stderr, "pairsim: -mode must be `pair` or `listen`, got %q\n", cfg.mode)
		os.Exit(2)
	}
	if strings.TrimSpace(cfg.code) == "" {
		fmt.Fprintln(os.Stderr, "pairsim: -code is required (run `!sms login app-pair` in the management room)")
		os.Exit(2)
	}
	if cfg.waitMS < 0 {
		cfg.waitMS = 0
	}
	return cfg
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func randomB64URL(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("pairsim: cannot read random bytes: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// ---------------------------------------------------------------------------
// Main

func main() {
	log.SetFlags(log.Ltime)
	cfg := parseFlags()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s := &sim{
		cfg:     cfg,
		http:    &http.Client{},
		nextRow: cfg.firstRow,
		seq:     cfg.firstSeq,
	}
	if err := s.run(ctx); err != nil {
		log.Fatalf("pairsim: %v", err)
	}
}

func (s *sim) run(ctx context.Context) error {
	log.Printf("using bridge %s as device %q (%s)", s.cfg.bridgeURL, s.cfg.deviceID, s.cfg.deviceName)

	info, err := s.getInfo(ctx)
	if err != nil {
		return err
	}
	log.Printf("bridge info: name=%q protocol=%d pairing_enabled=%t homeserver=%q",
		info.BridgeName, info.ProtocolVersion, info.PairingEnabled, info.HomeserverURL)
	if !info.PairingEnabled {
		return errors.New("the bridge reports that app pairing is disabled")
	}

	complete, err := s.pair(ctx, info)
	if err != nil {
		return err
	}
	s.token = complete.Token
	log.Printf("paired: device_id=%q user=%q displayname=%q", complete.DeviceID, complete.User.MXID, complete.User.Displayname)
	log.Printf("device token: %s", complete.Token)
	log.Printf("homeserver=%q bridge=%q protocol=%d queue.wait_max_ms=%d",
		complete.HomeserverURL, complete.Bridge.BaseURL, complete.Bridge.ProtocolVersion, complete.Queue.WaitMaxMS)

	if s.cfg.mode == "pair" {
		log.Printf("mode=pair: exiting; token is not persisted")
		return nil
	}

	go s.heartbeatLoop(ctx)
	if s.cfg.seed {
		if err := s.seedPortal(ctx); err != nil {
			log.Printf("seed message failed: %v", err)
		}
	}
	return s.listenLoop(ctx)
}

// ---------------------------------------------------------------------------
// Pairing

func (s *sim) getInfo(ctx context.Context) (*pairInfoResponse, error) {
	var info pairInfoResponse
	if _, err := s.doJSON(ctx, http.MethodGet, pairingPathPrefix+"/info", nil, false, &info); err != nil {
		return nil, fmt.Errorf("GET /info: %w", err)
	}
	return &info, nil
}

func (s *sim) pair(ctx context.Context, info *pairInfoResponse) (*pairCompleteResponse, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ECDSA P-256 key: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	pubkey := base64.RawURLEncoding.EncodeToString(spki)

	clientNonce := randomB64URL(16)
	startReq := pairStartRequest{
		Code:  s.cfg.code,
		HSURL: info.HomeserverURL,
		Device: pairDeviceInfo{
			ID:         s.cfg.deviceID,
			Name:       s.cfg.deviceName,
			Platform:   s.cfg.platform,
			AppVersion: s.cfg.appVersion,
		},
		Pubkey:      pubkey,
		ClientNonce: clientNonce,
	}
	var start pairStartResponse
	if _, err := s.doJSON(ctx, http.MethodPost, pairingPathPrefix+"/pair/start", startReq, false, &start); err != nil {
		return nil, fmt.Errorf("POST /pair/start: %w", err)
	}
	log.Printf("pairing session %s started, expires_at_ms=%d", start.PairingID, start.ExpiresAtMS)

	// smsmms-pair-v1|<pairing_id>|<server_nonce>|<client_nonce>|<device_id>
	signingInput := strings.Join([]string{
		pairingSigningPrefix, start.PairingID, start.ServerNonce, clientNonce, s.cfg.deviceID,
	}, "|")
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign pairing challenge: %w", err)
	}
	var complete pairCompleteResponse
	if _, err := s.doJSON(ctx, http.MethodPost, pairingPathPrefix+"/pair/complete", pairCompleteRequest{
		PairingID: start.PairingID,
		Signature: base64.RawURLEncoding.EncodeToString(signature),
	}, false, &complete); err != nil {
		return nil, fmt.Errorf("POST /pair/complete: %w", err)
	}
	if complete.DeviceID == "" || complete.Token == "" {
		return nil, errors.New("bridge did not return a device ID and token")
	}
	return &complete, nil
}

// ---------------------------------------------------------------------------
// Queue loop

func (s *sim) listenLoop(ctx context.Context) error {
	log.Printf("listening for commands (Ctrl-C to stop)")
	for {
		if ctx.Err() != nil {
			return nil
		}
		queue, err := s.queue(ctx, s.queueCursor)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("queue poll failed: %v", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if queue.Reset {
			log.Printf("queue reset: command cursor too old, re-syncing from %d", queue.Cursor)
		}
		s.queueCursor = queue.Cursor
		for _, cmd := range queue.Commands {
			if ctx.Err() != nil {
				return nil
			}
			if err := s.handleCommand(ctx, cmd); err != nil {
				log.Printf("command %d (%s) failed: %v", cmd.Seq, cmd.Type, err)
			}
		}
	}
}

func (s *sim) queue(ctx context.Context, since int64) (*pairQueueResponse, error) {
	waitMS := s.cfg.waitMS
	timeout := time.Duration(waitMS)*time.Millisecond + 15*time.Second
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	path := fmt.Sprintf("%s/queue?since=%d&wait_ms=%d", pairingPathPrefix, since, waitMS)
	var queue pairQueueResponse
	if _, err := s.doJSON(reqCtx, http.MethodGet, path, nil, true, &queue); err != nil {
		return nil, err
	}
	return &queue, nil
}

func (s *sim) handleCommand(ctx context.Context, cmd pairCommand) error {
	payload, _ := json.Marshal(cmd)
	log.Printf("command seq=%d type=%s %s", cmd.Seq, cmd.Type, string(payload))
	switch cmd.Type {
	case "send_sms", "send_mms":
		return s.handleSend(ctx, cmd)
	case "mark_read":
		events := []postedEvent{
			{
				Type:     "thread.updated",
				ThreadID: cmd.ThreadID,
				Preview:  fmt.Sprintf("read up to %d", cmd.UpToMS),
			},
			{Type: "ack", ThreadID: cmd.ThreadID, SendID: cmd.SendID},
		}
		return s.postEvents(ctx, events)
	case "ping":
		return s.postEvents(ctx, []postedEvent{{Type: "ack", SendID: cmd.SendID}})
	default:
		log.Printf("unknown command type %q; acknowledging it", cmd.Type)
		return s.postEvents(ctx, []postedEvent{{Type: "ack", SendID: cmd.SendID}})
	}
}

// handleSend answers a send command like the phone would: a send.result with
// the resolved provider row plus the message.new event carrying the full row.
func (s *sim) handleSend(ctx context.Context, cmd pairCommand) error {
	subID := cmd.SubID
	if subID == 0 {
		subID = s.cfg.simSubID
	}
	threadID := cmd.ThreadID
	if threadID == 0 {
		threadID = s.cfg.simThreadID
	}
	prefix := "sms"
	if cmd.Type == "send_mms" {
		prefix = "mms"
	}
	messageID := fmt.Sprintf("%s:%d:%d", prefix, subID, s.nextRow)
	s.nextRow++

	recipients := cmd.Recipients
	if len(recipients) == 0 {
		recipients = []string{s.cfg.simRemoteNumber}
	}
	nowMS := time.Now().UnixMilli()
	// The phone's own copy of the message: sender empty, type 2 (sent).
	message := &simMessage{
		ID:              messageID,
		ThreadID:        threadID,
		SubID:           subID,
		TimestampMS:     nowMS,
		TimestampSentMS: nowMS,
		Sender:          "",
		Recipients:      recipients,
		Body:            cmd.Text,
		IsMMS:           cmd.Type == "send_mms",
		Type:            2,
		Read:            true,
		Seen:            true,
	}
	events := []postedEvent{
		{
			Type:      "send.result",
			SendID:    cmd.SendID,
			MessageID: messageID,
			ThreadID:  threadID,
			PartsSent: 1,
			Pending:   false,
		},
		{
			Type:          "message.new",
			MessageID:     messageID,
			ThreadID:      threadID,
			SubID:         subID,
			TimestampMS:   nowMS,
			Sender:        "",
			IsMMS:         cmd.Type == "send_mms",
			HasAttachment: false,
			Preview:       cmd.Text,
			SendID:        cmd.SendID,
			Recipients:    recipients,
			Message:       message,
		},
	}
	if err := s.postEvents(ctx, events); err != nil {
		return err
	}
	log.Printf("answered %s send_id=%q with %s (row exists, pending=false)", cmd.Type, cmd.SendID, messageID)
	return nil
}

// seedPortal posts one fake incoming SMS so the bridge creates a portal room
// that can be used for the Matrix -> phone direction.
func (s *sim) seedPortal(ctx context.Context) error {
	nowMS := time.Now().UnixMilli()
	messageID := fmt.Sprintf("sms:%d:%d", s.cfg.simSubID, s.nextRow)
	s.nextRow++
	message := &simMessage{
		ID:          messageID,
		ThreadID:    s.cfg.simThreadID,
		SubID:       s.cfg.simSubID,
		TimestampMS: nowMS,
		Sender:      s.cfg.simRemoteNumber,
		Recipients:  []string{s.cfg.simLocalNumber},
		Body:        "Hello from the simulated phone",
		Type:        1,
		Read:        false,
	}
	err := s.postEvents(ctx, []postedEvent{{
		Type:          "message.new",
		MessageID:     messageID,
		ThreadID:      s.cfg.simThreadID,
		SubID:         s.cfg.simSubID,
		TimestampMS:   nowMS,
		Sender:        s.cfg.simRemoteNumber,
		HasAttachment: false,
		Preview:       message.Body,
		Recipients:    message.Recipients,
		Message:       message,
	}})
	if err != nil {
		return err
	}
	log.Printf("seeded incoming SMS %s from %s in thread %d", messageID, s.cfg.simRemoteNumber, s.cfg.simThreadID)
	return nil
}

// ---------------------------------------------------------------------------
// Events and heartbeat

func (s *sim) postEvents(ctx context.Context, events []postedEvent) error {
	for i := range events {
		s.seq++
		events[i].Seq = s.seq
	}
	req := pairEventsRequest{
		DeviceID: s.cfg.deviceID,
		Cursor:   s.eventCursor,
		Events:   events,
	}
	var resp pairEventsResponse
	if _, err := s.doJSON(ctx, http.MethodPost, pairingPathPrefix+"/events", req, true, &resp); err != nil {
		return fmt.Errorf("POST /events: %w", err)
	}
	s.eventCursor = resp.Cursor
	log.Printf("posted %d event(s), accepted=%d event_cursor=%d", len(events), resp.Accepted, resp.Cursor)
	return nil
}

func (s *sim) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	beat := func() {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		_, err := s.doJSON(reqCtx, http.MethodPost, pairingPathPrefix+"/heartbeat", pairHeartbeatRequest{
			DeviceID:      s.cfg.deviceID,
			BatteryPct:    88,
			DefaultSMSApp: true,
			Permissions:   map[string]bool{"receive_sms": true, "send_mms": true, "mark_read": true},
			AppVersion:    s.cfg.appVersion,
		}, true, nil)
		if err != nil {
			log.Printf("heartbeat failed: %v", err)
			return
		}
		log.Printf("heartbeat sent")
	}
	beat()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			beat()
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP helper

// doJSON performs a JSON request against the pairing API and decodes the
// response into out (when non-nil). Error responses use the protocol's
// {"error","message"} envelope.
func (s *sim) doJSON(ctx context.Context, method, path string, in any, auth bool, out any) ([]byte, error) {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	url := strings.TrimRight(s.cfg.bridgeURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		if s.token == "" {
			return nil, errors.New("not paired yet: no bearer token")
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr errorResponse
		if json.Unmarshal(data, &apiErr) == nil && apiErr.Error != "" {
			return data, fmt.Errorf("HTTP %d: %s: %s", resp.StatusCode, apiErr.Error, apiErr.Message)
		}
		return data, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return data, fmt.Errorf("decode response: %w", err)
		}
	}
	return data, nil
}
