// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// pairingServer is the public pairing API listener described in
// docs/PAIRING_PROTOCOL.md. It owns in-memory pairing sessions and per-device
// queues/inboxes. Sessions are intentionally not persisted: a bridge restart
// invalidates in-flight pairings and the user simply runs `login app-pair`
// again.
type pairingServer struct {
	log       zerolog.Logger
	connector *MyConnector

	lifecycleMu sync.Mutex
	httpServer  *http.Server
	listener    net.Listener
	cleanupDone chan struct{}

	mu            sync.Mutex
	sessions      map[string]*pairingSession
	sessionByCode map[string]*pairingSession
	devices       map[string]*pairedDevice
	tokenIndex    map[string]string // token SHA-256 hex -> device ID
	startAttempts map[string][]time.Time

	// Tunables, overridable by tests.
	ttl              time.Duration
	maxAttempts      int
	rateLimitMax     int
	rateLimitWindow  time.Duration
	commandRetention time.Duration
}

// newPairingServer creates the pairing manager for a connector. Start() must
// be called to bind the listener; Handler() can be used without it (tests).
func newPairingServer(log zerolog.Logger, connector *MyConnector) *pairingServer {
	return &pairingServer{
		log:              log.With().Str("component", "app-pairing").Logger(),
		connector:        connector,
		sessions:         make(map[string]*pairingSession),
		sessionByCode:    make(map[string]*pairingSession),
		devices:          make(map[string]*pairedDevice),
		tokenIndex:       make(map[string]string),
		startAttempts:    make(map[string][]time.Time),
		ttl:              pairingSessionTTL,
		maxAttempts:      pairingMaxAttempts,
		rateLimitMax:     pairingRateLimitMax,
		rateLimitWindow:  pairingRateLimitWindow,
		commandRetention: pairingCommandRetention,
	}
}

// enabled reports whether pairing is enabled by the connector config.
func (s *pairingServer) enabled() bool {
	if s.connector == nil {
		return true
	}
	return s.connector.pairingEnabled()
}

// Start binds the listener and serves the pairing API until Stop is called.
func (s *pairingServer) Start() error {
	s.lifecycleMu.Lock()
	if s.httpServer != nil {
		s.lifecycleMu.Unlock()
		return nil
	}
	listen := pairingDefaultListen
	if s.connector != nil {
		listen = s.connector.pairingListen()
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("failed to listen on %q: %w", listen, err)
	}
	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	s.listener = listener
	s.httpServer = server
	s.lifecycleMu.Unlock()

	go func() {
		s.log.Info().Str("listen", listener.Addr().String()).Msg("App pairing API listening")
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error().Err(err).Msg("App pairing API server stopped unexpectedly")
		}
	}()
	cleanupDone := make(chan struct{})
	s.lifecycleMu.Lock()
	s.cleanupDone = cleanupDone
	s.lifecycleMu.Unlock()
	go s.cleanupLoop(cleanupDone)
	return nil
}

// Stop gracefully shuts the pairing API down.
func (s *pairingServer) Stop(ctx context.Context) error {
	s.lifecycleMu.Lock()
	server := s.httpServer
	cleanupDone := s.cleanupDone
	s.httpServer = nil
	s.listener = nil
	s.cleanupDone = nil
	s.lifecycleMu.Unlock()
	if cleanupDone != nil {
		close(cleanupDone)
	}
	if server == nil {
		return nil
	}
	s.log.Info().Msg("Stopping app pairing API")
	return server.Shutdown(ctx)
}

// cleanupLoop drops expired sessions and stale rate limit entries.
func (s *pairingServer) cleanupLoop(done <-chan struct{}) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			s.cleanupExpired(now)
		}
	}
}

func (s *pairingServer) cleanupExpired(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, session := range s.sessions {
		if now.After(session.expiresAt) {
			delete(s.sessions, id)
			delete(s.sessionByCode, session.code)
		}
	}
	window := s.rateLimitWindow
	if window <= 0 {
		window = pairingRateLimitWindow
	}
	for ip, attempts := range s.startAttempts {
		kept := attempts[:0]
		for _, at := range attempts {
			if now.Sub(at) <= window {
				kept = append(kept, at)
			}
		}
		if len(kept) == 0 {
			delete(s.startAttempts, ip)
		} else {
			s.startAttempts[ip] = kept
		}
	}
}

// Handler returns the HTTP handler of the pairing API.
func (s *pairingServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pairingPathPrefix+"/info", s.handleInfo)
	mux.HandleFunc("POST "+pairingPathPrefix+"/pair/start", s.handlePairStart)
	mux.HandleFunc("POST "+pairingPathPrefix+"/pair/complete", s.handlePairComplete)
	mux.HandleFunc("GET "+pairingPathPrefix+"/queue", s.handleQueue)
	mux.HandleFunc("POST "+pairingPathPrefix+"/events", s.handleEvents)
	mux.HandleFunc("POST "+pairingPathPrefix+"/heartbeat", s.handleHeartbeat)
	return mux
}

// ---------------------------------------------------------------------------
// Session management

// newSession creates a pairing session bound to the given Matrix user.
func (s *pairingServer) newSession(mxid id.UserID, displayName string) (*pairingSession, error) {
	session := newPairingSession(mxid, displayName, s.ttl, s.maxAttempts)
	pairingID, err := randomB64(9)
	if err != nil {
		return nil, err
	}
	session.id = "pr_" + strings.NewReplacer("-", "", "_", "").Replace(pairingID)

	code, err := newPairingCode()
	if err != nil {
		return nil, err
	}
	session.code = code

	s.mu.Lock()
	defer s.mu.Unlock()
	for s.sessionByCode[session.code] != nil {
		if session.code, err = newPairingCode(); err != nil {
			return nil, err
		}
	}
	s.sessions[session.id] = session
	s.sessionByCode[session.code] = session
	return session, nil
}

// sessionByID returns an active (not removed) pairing session.
func (s *pairingServer) sessionByID(pairingID string) *pairingSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[pairingID]
}

// findSessionByCode looks up a session by normalized code, removing expired
// ones so their codes can never be reused after the TTL. expired reports
// whether a session existed but had already run out of time.
func (s *pairingServer) findSessionByCode(code string) (session *pairingSession, expired bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session = s.sessionByCode[code]
	if session == nil {
		return nil, false
	}
	if time.Now().After(session.expiresAt) {
		delete(s.sessionByCode, code)
		delete(s.sessions, session.id)
		return nil, true
	}
	return session, false
}

// releaseSession removes a session from the maps (used on success, cancel and
// expiry). It is safe to call more than once.
func (s *pairingServer) releaseSession(session *pairingSession) {
	if session == nil {
		return
	}
	s.mu.Lock()
	if s.sessions[session.id] == session {
		delete(s.sessions, session.id)
	}
	if s.sessionByCode[session.code] == session {
		delete(s.sessionByCode, session.code)
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Device management

// ensureDevice returns the runtime state of a device, creating and indexing
// it from the persisted login metadata when needed.
func (s *pairingServer) ensureDevice(meta *LoginMetadata) *pairedDevice {
	if meta == nil || strings.TrimSpace(meta.DeviceID) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	device := s.devices[meta.DeviceID]
	if device == nil {
		device = newPairedDevice(s, meta.DeviceID)
		s.devices[meta.DeviceID] = device
	}
	oldTokenHash := device.applyLoginMetadata(meta)
	if oldTokenHash != "" && oldTokenHash != meta.TokenSHA256 {
		delete(s.tokenIndex, oldTokenHash)
	}
	if meta.TokenSHA256 != "" {
		s.tokenIndex[meta.TokenSHA256] = device.deviceID
	}
	return device
}

// activateDevice registers a freshly paired device and its token hash.
func (s *pairingServer) activateDevice(result pairSessionResult, tokenHash string) *pairedDevice {
	s.mu.Lock()
	defer s.mu.Unlock()
	device := s.devices[result.DeviceID]
	if device == nil {
		device = newPairedDevice(s, result.DeviceID)
		s.devices[result.DeviceID] = device
	}
	oldTokenHash := device.applyResult(result)
	if oldTokenHash != "" && oldTokenHash != tokenHash {
		delete(s.tokenIndex, oldTokenHash)
	}
	device.setTokenHash(tokenHash)
	s.tokenIndex[tokenHash] = device.deviceID
	return device
}

// device returns the runtime state of a device, if it is registered.
func (s *pairingServer) device(deviceID string) *pairedDevice {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devices[deviceID]
}

// revokeDevice deletes a device and its token, immediately invalidating the
// bearer token. Called by `!sms unpair`.
func (s *pairingServer) revokeDevice(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	device := s.devices[deviceID]
	if device == nil {
		return
	}
	if tokenHash := device.tokenHashValue(); tokenHash != "" {
		delete(s.tokenIndex, tokenHash)
	}
	delete(s.devices, deviceID)
}

// devicesForUser returns the registered app devices of a Matrix user. It
// resolves them through the user's logins so stale in-memory rows cannot
// leak devices that were already logged out.
func (s *pairingServer) devicesForUser(ctx context.Context, br *bridgev2.Bridge, mxid id.UserID) []*pairedDevice {
	if br == nil {
		return nil
	}
	logins, err := br.DB.UserLogin.GetAllForUser(ctx, mxid)
	if err != nil {
		return nil
	}
	devices := make([]*pairedDevice, 0, len(logins))
	for _, login := range logins {
		meta, _ := login.Metadata.(*LoginMetadata)
		if meta == nil || meta.Transport != LoginTransportApp || meta.DeviceID == "" {
			continue
		}
		if device := s.ensureDevice(meta); device != nil {
			devices = append(devices, device)
		}
	}
	return devices
}

// ---------------------------------------------------------------------------
// HTTP handlers

func (s *pairingServer) handleInfo(w http.ResponseWriter, r *http.Request) {
	homeserverURL := ""
	if s.connector != nil {
		homeserverURL = s.connector.homeserverPublicURL()
	}
	writePairJSON(w, http.StatusOK, pairInfoResponse{
		ProtocolVersion: pairingProtocolVersion,
		BridgeName:      "mautrix-android-sms-mms",
		HomeserverURL:   homeserverURL,
		PairingEnabled:  s.enabled(),
	})
}

func (s *pairingServer) handlePairStart(w http.ResponseWriter, r *http.Request) {
	if !s.enabled() {
		writePairError(w, newPairingError(http.StatusNotImplemented, "unsupported", "pairing is disabled on this bridge"))
		return
	}
	if !s.allowPairRequest(remoteIP(r), time.Now()) {
		writePairError(w, newPairingError(http.StatusTooManyRequests, "rate_limited", "too many pairing requests from this address; try again later"))
		return
	}
	var req pairStartRequest
	if !decodePairBody(w, r, &req) {
		return
	}

	code, ok := normalizePairingCode(req.Code)
	if !ok {
		// Malformed and unknown codes deliberately share one response so the
		// API cannot be used to probe which codes exist.
		writePairError(w, newPairingError(http.StatusNotFound, "not_found", "unknown or already used pairing code"))
		return
	}
	if strings.TrimSpace(req.Device.ID) == "" {
		writePairError(w, newPairingError(http.StatusBadRequest, "bad_request", "device.id is required"))
		return
	}
	pubkeyDER, err := decodeBase64URL(req.Pubkey)
	if err != nil {
		writePairError(w, newPairingError(http.StatusBadRequest, "bad_request", "pubkey must be base64url SPKI DER"))
		return
	}
	parsedPubkey, err := x509.ParsePKIXPublicKey(pubkeyDER)
	if err != nil {
		writePairError(w, newPairingError(http.StatusBadRequest, "bad_request", "pubkey is not a valid SPKI DER key"))
		return
	}
	publicKey, ok := parsedPubkey.(*ecdsa.PublicKey)
	if !ok {
		writePairError(w, newPairingError(http.StatusBadRequest, "bad_request", "pubkey is not an ECDSA key"))
		return
	}
	if publicKey.Curve.Params().Name != "P-256" {
		writePairError(w, newPairingError(http.StatusBadRequest, "bad_request", "pubkey must use curve P-256"))
		return
	}
	if clientNonce, err := decodeBase64URL(req.ClientNonce); err != nil || len(clientNonce) == 0 {
		writePairError(w, newPairingError(http.StatusBadRequest, "bad_request", "client_nonce must be base64url"))
		return
	}

	session, expired := s.findSessionByCode(code)
	if expired {
		writePairError(w, newPairingError(http.StatusGone, "expired", "the pairing code has expired"))
		return
	}
	if session == nil {
		writePairError(w, newPairingError(http.StatusNotFound, "not_found", "unknown or already used pairing code"))
		return
	}
	serverNonce, err := randomB64(16)
	if err != nil {
		writePairError(w, newPairingError(http.StatusInternalServerError, "internal", "failed to generate server nonce"))
		return
	}
	effectiveNonce, err := session.begin(req, pubkeyDER, serverNonce)
	if err != nil {
		perr, _ := asPairingError(err)
		if perr != nil && perr.code == "expired" {
			s.releaseSession(session)
		}
		writePairError(w, err)
		return
	}
	s.log.Info().
		Str("pairing_id", session.id).
		Str("device_id", req.Device.ID).
		Str("device_name", req.Device.Name).
		Msg("Pairing session started by app")
	writePairJSON(w, http.StatusOK, pairStartResponse{
		PairingID:   session.id,
		ServerNonce: effectiveNonce,
		ExpiresAtMS: session.expiresAt.UnixMilli(),
	})
}

func (s *pairingServer) handlePairComplete(w http.ResponseWriter, r *http.Request) {
	if !s.enabled() {
		writePairError(w, newPairingError(http.StatusNotImplemented, "unsupported", "pairing is disabled on this bridge"))
		return
	}
	if !s.allowPairRequest(remoteIP(r), time.Now()) {
		writePairError(w, newPairingError(http.StatusTooManyRequests, "rate_limited", "too many pairing requests from this address; try again later"))
		return
	}
	var req pairCompleteRequest
	if !decodePairBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.PairingID) == "" {
		writePairError(w, newPairingError(http.StatusBadRequest, "bad_request", "pairing_id is required"))
		return
	}
	signature, err := decodeBase64URL(req.Signature)
	if err != nil {
		writePairError(w, newPairingError(http.StatusBadRequest, "bad_request", "signature must be base64url ECDSA"))
		return
	}

	session := s.sessionByID(req.PairingID)
	if session == nil {
		writePairError(w, newPairingError(http.StatusNotFound, "not_found", "unknown or already used pairing session"))
		return
	}
	result, err := session.finish(signature)
	if err != nil {
		perr, _ := asPairingError(err)
		if perr != nil && (perr.code == "expired" || perr.code == "not_found") {
			s.releaseSession(session)
		}
		writePairError(w, err)
		return
	}

	token, err := randomPairingToken()
	if err != nil {
		writePairError(w, newPairingError(http.StatusInternalServerError, "internal", "failed to generate device token"))
		return
	}
	result.TokenSHA256 = hashPairingToken(token)
	s.activateDevice(result, result.TokenSHA256)
	s.releaseSession(session)
	session.deliver(result)

	bridgeURL := ""
	homeserverURL := ""
	if s.connector != nil {
		bridgeURL = s.connector.pairingPublicURL()
		homeserverURL = s.connector.homeserverPublicURL()
		if bridgeURL == "" {
			bridgeURL = homeserverURL
		}
	}
	s.log.Info().
		Str("pairing_id", session.id).
		Str("device_id", result.DeviceID).
		Str("mxid", string(session.mxid)).
		Msg("App completed pairing")
	writePairJSON(w, http.StatusOK, pairCompleteResponse{
		DeviceID: result.DeviceID,
		Token:    token,
		User: pairUserInfo{
			MXID:        string(session.mxid),
			Displayname: session.displayName,
		},
		HomeserverURL: homeserverURL,
		Bridge: pairBridgeInfo{
			BaseURL:         bridgeURL,
			ProtocolVersion: pairingProtocolVersion,
		},
		Queue: pairQueueInfo{WaitMaxMS: pairingWaitMaxMS},
	})
}

func (s *pairingServer) handleQueue(w http.ResponseWriter, r *http.Request) {
	device, perr := s.authenticate(r)
	if perr != nil {
		writePairError(w, perr)
		return
	}
	since := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writePairError(w, newPairingError(http.StatusBadRequest, "bad_request", "since must be a non-negative integer"))
			return
		}
		since = parsed
	}
	waitMS := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("wait_ms")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writePairError(w, newPairingError(http.StatusBadRequest, "bad_request", "wait_ms must be a non-negative integer"))
			return
		}
		waitMS = parsed
	}
	if waitMS > pairingWaitMaxMS {
		waitMS = pairingWaitMaxMS
	}

	device.markSeen(time.Now())
	deadline := time.Now().Add(time.Duration(waitMS) * time.Millisecond)
	response, err := device.waitQueue(r.Context(), since, deadline)
	if err != nil {
		// The client went away; nothing sensible to send anymore.
		return
	}
	if response.Commands == nil {
		response.Commands = []pairCommand{}
	}
	device.markSeen(time.Now())
	writePairJSON(w, http.StatusOK, response)
}

func (s *pairingServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	device, perr := s.authenticate(r)
	if perr != nil {
		writePairError(w, perr)
		return
	}
	var req pairEventsRequest
	if !decodePairBody(w, r, &req) {
		return
	}
	if req.DeviceID != "" && req.DeviceID != device.deviceID {
		writePairError(w, newPairingError(http.StatusUnauthorized, "unauthorized", "device_id does not match the bearer token"))
		return
	}
	now := time.Now()
	device.markSeen(now)
	accepted := 0
	for _, ev := range req.Events {
		if ev.Type == "send.result" {
			result := pairSendResult{
				SendID:    ev.SendID,
				MessageID: ev.MessageID,
				ThreadID:  ev.ThreadID,
				PartsSent: ev.PartsSent,
				Pending:   ev.Pending,
				Error:     ev.Error,
			}
			if !device.resolveSend(result) {
				s.handleLateSendResult(device, result)
			}
			device.removeCommand(ev.SendID)
			accepted++
			continue
		}
		if ev.Type == "ack" {
			// The app acknowledges commands whose result is not covered by
			// send.result (mark_read, ping). Acks for commands without a
			// send id carry `ack-<seq>`; prune both ways so acked commands
			// cannot be re-delivered after a reconnect.
			if ev.Error != nil {
				s.log.Warn().
					Str("device_id", device.deviceID).
					Str("send_id", ev.SendID).
					Str("error", ev.Error.Code).
					Str("message", ev.Error.Message).
					Msg("Device reported a command failure")
			}
			device.removeCommand(ev.SendID)
			device.removeCommandBySeq(ackCommandSeq(ev.SendID))
			accepted++
			continue
		}
		if device.pushEvent(ev) {
			accepted++
		}
	}
	cursor := maxCursor(req.Cursor, device.snapshot().LastEventSeq)
	writePairJSON(w, http.StatusOK, pairEventsResponse{Cursor: cursor, Accepted: accepted})
}

// ackCommandSeq extracts the command sequence number from an ack send id.
// Ack events for commands without a send id use `ack-<seq>` (see the app's
// BridgeCommandExecutor); acks for send commands carry the original send id
// and are pruned by removeCommand instead.
func ackCommandSeq(sendID string) int64 {
	if !strings.HasPrefix(sendID, "ack-") {
		return 0
	}
	seq, err := strconv.ParseInt(strings.TrimPrefix(sendID, "ack-"), 10, 64)
	if err != nil {
		return 0
	}
	return seq
}

func (s *pairingServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	device, perr := s.authenticate(r)
	if perr != nil {
		writePairError(w, perr)
		return
	}
	var req pairHeartbeatRequest
	if !decodePairBody(w, r, &req) {
		return
	}
	if req.DeviceID != "" && req.DeviceID != device.deviceID {
		writePairError(w, newPairingError(http.StatusUnauthorized, "unauthorized", "device_id does not match the bearer token"))
		return
	}
	device.setHeartbeat(req)
	now := time.Now()
	device.persistLastSeen(r.Context(), now)
	writePairJSON(w, http.StatusOK, pairHeartbeatResponse{OK: true, ServerTimeMS: now.UnixMilli()})
}

// handleLateSendResult reports a send.result that arrived after the bridge
// stopped waiting for it, so the echo of the phone's own message is still
// suppressed and errors are visible in the log.
func (s *pairingServer) handleLateSendResult(device *pairedDevice, result pairSendResult) {
	runtime := device.runtimeHandler()
	if result.Error != nil {
		s.log.Warn().
			Str("device_id", device.deviceID).
			Str("send_id", result.SendID).
			Str("error", result.Error.Code).
			Str("message", result.Error.Message).
			Msg("Late send.result error from app")
		return
	}
	if runtime != nil {
		runtime.handleLateSendResult(result)
	}
}

// handleLateSendResult on the runtime applies the echo suppression for a
// send.result that arrived after the bridge stopped waiting for it, and logs
// failures the app reports late.
func (nc *MyNetworkClient) handleLateSendResult(result pairSendResult) {
	if result.Error != nil {
		nc.log.Warn().
			Str("send_id", result.SendID).
			Str("error", result.Error.Code).
			Str("message", result.Error.Message).
			Msg("Phone reported a failed send after the bridge stopped waiting")
		return
	}
	if result.MessageID != "" {
		nc.markSentFromMatrix(networkid.MessageID(result.MessageID))
	}
}

// ---------------------------------------------------------------------------
// Authentication and rate limiting

// authenticate resolves the bearer token to a registered device.
func (s *pairingServer) authenticate(r *http.Request) (*pairedDevice, *pairingError) {
	token := bearerToken(r)
	if token == "" {
		return nil, newPairingError(http.StatusUnauthorized, "unauthorized", "missing bearer token")
	}
	tokenHash := hashPairingToken(token)
	s.mu.Lock()
	deviceID, ok := s.tokenIndex[tokenHash]
	device := s.devices[deviceID]
	s.mu.Unlock()
	if !ok || device == nil || device.tokenHashValue() != tokenHash {
		return nil, newPairingError(http.StatusUnauthorized, "unauthorized", "unknown or revoked device token")
	}
	return device, nil
}

func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// allowPairRequest implements the per-IP sliding window rate limit.
func (s *pairingServer) allowPairRequest(ip string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	window := s.rateLimitWindow
	if window <= 0 {
		window = pairingRateLimitWindow
	}
	cutoff := now.Add(-window)
	attempts := s.startAttempts[ip]
	kept := attempts[:0]
	for _, at := range attempts {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) >= s.rateLimitMax {
		s.startAttempts[ip] = kept
		return false
	}
	s.startAttempts[ip] = append(kept, now)
	return true
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// Helpers

func writePairJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writePairError(w http.ResponseWriter, err error) {
	if perr, ok := asPairingError(err); ok {
		writePairJSON(w, perr.status, map[string]string{"error": perr.code, "message": perr.message})
		return
	}
	writePairJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": "internal error"})
}

// decodePairBody reads a size-limited JSON request body, replying with the
// error envelope on failure.
func decodePairBody(w http.ResponseWriter, r *http.Request, out any) bool {
	body := http.MaxBytesReader(w, r.Body, pairingMaxBodyBytes)
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			writePairError(w, newPairingError(http.StatusBadRequest, "bad_request", "request body is empty"))
		} else {
			writePairError(w, newPairingError(http.StatusBadRequest, "bad_request", "invalid JSON request body"))
		}
		return false
	}
	return true
}

func maxCursor(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
