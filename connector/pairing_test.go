// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// ---------------------------------------------------------------------------
// Test helpers

func newTestPairingServer(t *testing.T) *pairingServer {
	t.Helper()
	server := newPairingServer(zerolog.Nop(), nil)
	// Tests issue many requests from the same IP; keep rate limiting out of
	// the way unless a test targets it explicitly.
	server.rateLimitMax = 1000
	return server
}

func newTestPairingServerHTTP(t *testing.T) (*pairingServer, *httptest.Server) {
	t.Helper()
	server := newTestPairingServer(t)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return server, httpServer
}

type pairTestKey struct {
	key       *ecdsa.PrivateKey
	pubkeyB64 string
}

func newTestPairKey(t *testing.T) pairTestKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate P-256 key: %v", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	return pairTestKey{key: key, pubkeyB64: base64.RawURLEncoding.EncodeToString(spki)}
}

func (k pairTestKey) sign(t *testing.T, pairingID, serverNonce, clientNonce, deviceID string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(pairingSigningInput(pairingID, serverNonce, clientNonce, deviceID)))
	signature, err := ecdsa.SignASN1(rand.Reader, k.key, digest[:])
	if err != nil {
		t.Fatalf("failed to sign pairing challenge: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(signature)
}

func testNonce(t *testing.T, value string) string {
	t.Helper()
	if len(value) > 16 {
		value = value[:16]
	}
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func pairPostJSON(t *testing.T, url string, body any) (int, []byte) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST %s failed: %v", url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response of %s: %v", url, err)
	}
	return resp.StatusCode, data
}

func pairDecodeErrorCode(t *testing.T, data []byte) string {
	t.Helper()
	var envelope struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("failed to decode error envelope %q: %v", data, err)
	}
	return envelope.Error
}

func pairStartBody(session *pairingSession, key pairTestKey, deviceID, clientNonce string) pairStartRequest {
	return pairStartRequest{
		Code: strings.ToLower(session.code),
		Device: pairDeviceInfo{
			ID:         deviceID,
			Name:       "Pixel Test",
			Platform:   "android",
			AppVersion: "1.10.1-bridge1",
		},
		Pubkey:      key.pubkeyB64,
		ClientNonce: clientNonce,
	}
}

func pairStartSession(t *testing.T, baseURL string, session *pairingSession, key pairTestKey, deviceID string) (pairStartResponse, string) {
	t.Helper()
	clientNonce := testNonce(t, "client-nonce-01")
	status, data := pairPostJSON(t, baseURL+"/_smsmms/v1/pair/start", pairStartBody(session, key, deviceID, clientNonce))
	if status != http.StatusOK {
		t.Fatalf("pair/start status = %d (%s)", status, data)
	}
	var response pairStartResponse
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("failed to decode pair/start response: %v", err)
	}
	if response.PairingID != session.id || response.ServerNonce == "" {
		t.Fatalf("unexpected pair/start response: %+v", response)
	}
	return response, clientNonce
}

// ---------------------------------------------------------------------------
// Code and session lifecycle

func TestPairingCodeFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		code, err := newPairingCode()
		if err != nil {
			t.Fatalf("newPairingCode() error: %v", err)
		}
		if len(code) != pairingCodeLength {
			t.Fatalf("code %q has length %d, want %d", code, len(code), pairingCodeLength)
		}
		if strings.ContainsAny(code, "ILOU") {
			t.Fatalf("code %q contains excluded Crockford characters", code)
		}
		for _, r := range code {
			if !strings.ContainsRune(crockfordAlphabet, r) {
				t.Fatalf("code %q contains %q outside the Crockford alphabet", code, r)
			}
		}
		if seen[code] {
			t.Fatalf("duplicate code %q", code)
		}
		seen[code] = true
	}
}

func TestNormalizePairingCode(t *testing.T) {
	normalized, ok := normalizePairingCode("il0o-2345")
	if !ok {
		t.Fatal("normalizePairingCode() rejected a code with Crockford confusables")
	}
	if normalized != "11002345" {
		t.Fatalf("normalizePairingCode(il0o-2345) = %q, want 11002345", normalized)
	}
	if _, ok := normalizePairingCode("ABC"); ok {
		t.Error("normalizePairingCode() accepted a too-short code")
	}
	if _, ok := normalizePairingCode("ABC#2345"); ok {
		t.Error("normalizePairingCode() accepted an invalid character")
	}
}

func TestPairSessionTTLExpiry(t *testing.T) {
	// waitResult honors the TTL.
	server := newTestPairingServer(t)
	server.ttl = 30 * time.Millisecond
	session, err := server.newSession("@alice:example.com", "Alice")
	if err != nil {
		t.Fatalf("newSession() error: %v", err)
	}
	if _, err = session.waitResult(context.Background()); !errors.Is(err, errPairingExpired) {
		t.Fatalf("waitResult() error = %v, want errPairingExpired", err)
	}

	// The HTTP side reports expired once and not_found afterwards (the
	// session is dropped so its code cannot be reused).
	httpServerObj, httpServer := newTestPairingServerHTTP(t)
	httpServerObj.ttl = 30 * time.Millisecond
	key := newTestPairKey(t)
	expiringSession, err := httpServerObj.newSession("@alice:example.com", "Alice")
	if err != nil {
		t.Fatalf("newSession() error: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	body := pairStartBody(expiringSession, key, "android-ttl", testNonce(t, "client-nonce-01"))
	status, data := pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/start", body)
	if status != http.StatusGone {
		t.Fatalf("pair/start on expired session status = %d (%s), want 410", status, data)
	}
	if code := pairDecodeErrorCode(t, data); code != "expired" {
		t.Fatalf("pair/start on expired session error = %q, want expired", code)
	}
	status, data = pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/start", body)
	if status != http.StatusNotFound {
		t.Fatalf("pair/start after expiry cleanup status = %d (%s), want 404", status, data)
	}
}

func TestPairingSessionHappyPath(t *testing.T) {
	server, httpServer := newTestPairingServerHTTP(t)
	session, err := server.newSession("@alice:example.com", "Alice")
	if err != nil {
		t.Fatalf("newSession() error: %v", err)
	}
	key := newTestPairKey(t)
	deviceID := "android-ab12"
	start, clientNonce := pairStartSession(t, httpServer.URL, session, key, deviceID)

	// A retried start with the same fields must return the same nonce.
	status, data := pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/start", pairStartBody(session, key, deviceID, clientNonce))
	if status != http.StatusOK {
		t.Fatalf("pair/start retry status = %d (%s)", status, data)
	}
	var retryResponse pairStartResponse
	if err := json.Unmarshal(data, &retryResponse); err != nil {
		t.Fatalf("failed to decode retry response: %v", err)
	}
	if retryResponse.ServerNonce != start.ServerNonce {
		t.Fatalf("retried pair/start changed the server nonce: %q vs %q", retryResponse.ServerNonce, start.ServerNonce)
	}

	signature := key.sign(t, session.id, start.ServerNonce, clientNonce, deviceID)
	status, data = pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/complete", pairCompleteRequest{
		PairingID: session.id,
		Signature: signature,
	})
	if status != http.StatusOK {
		t.Fatalf("pair/complete status = %d (%s)", status, data)
	}
	var complete pairCompleteResponse
	if err := json.Unmarshal(data, &complete); err != nil {
		t.Fatalf("failed to decode pair/complete response: %v", err)
	}
	if complete.DeviceID != deviceID {
		t.Fatalf("pair/complete device_id = %q, want %q", complete.DeviceID, deviceID)
	}
	if complete.User.MXID != "@alice:example.com" || complete.User.Displayname != "Alice" {
		t.Fatalf("unexpected pair/complete user: %+v", complete.User)
	}
	if complete.Bridge.ProtocolVersion != pairingProtocolVersion || complete.Queue.WaitMaxMS != pairingWaitMaxMS {
		t.Fatalf("unexpected pair/complete protocol fields: %+v", complete)
	}
	rawToken, err := decodeBase64URL(complete.Token)
	if err != nil || len(rawToken) != 32 {
		t.Fatalf("pair/complete token = %q (decoded %d bytes, err %v), want 32 random bytes", complete.Token, len(rawToken), err)
	}
	device := server.device(deviceID)
	if device == nil {
		t.Fatal("device was not registered after completion")
	}
	if device.tokenHashValue() != hashPairingToken(complete.Token) {
		t.Fatal("stored token hash does not match the returned token")
	}

	// The login flow must receive the result exactly once.
	result, err := session.waitResult(context.Background())
	if err != nil {
		t.Fatalf("waitResult() error: %v", err)
	}
	if result.DeviceID != deviceID || result.DeviceName != "Pixel Test" || result.PubkeySPKI != key.pubkeyB64 {
		t.Fatalf("unexpected session result: %+v", result)
	}
	if result.TokenSHA256 != hashPairingToken(complete.Token) {
		t.Fatalf("session result token hash mismatch: %q vs %q", result.TokenSHA256, hashPairingToken(complete.Token))
	}

	// The token authenticates queue requests.
	req, err := http.NewRequest(http.MethodGet, httpServer.URL+"/_smsmms/v1/queue?since=0&wait_ms=0", nil)
	if err != nil {
		t.Fatalf("failed to build queue request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+complete.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("queue request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queue with valid token status = %d", resp.StatusCode)
	}
	var queue pairQueueResponse
	if err := json.NewDecoder(resp.Body).Decode(&queue); err != nil {
		t.Fatalf("failed to decode queue response: %v", err)
	}
	if queue.Cursor != 0 || queue.Reset || len(queue.Commands) != 0 {
		t.Fatalf("unexpected empty queue: %+v", queue)
	}

	// Single use: a second complete with the same (valid) signature fails.
	status, data = pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/complete", pairCompleteRequest{
		PairingID: session.id,
		Signature: signature,
	})
	if status != http.StatusNotFound {
		t.Fatalf("repeated pair/complete status = %d (%s), want 404", status, data)
	}
	if code := pairDecodeErrorCode(t, data); code != "not_found" {
		t.Fatalf("repeated pair/complete error = %q, want not_found", code)
	}
	// The one-time code is gone too.
	status, data = pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/start", pairStartBody(session, key, deviceID, clientNonce))
	if status != http.StatusNotFound {
		t.Fatalf("pair/start after completion status = %d (%s), want 404", status, data)
	}
}

func TestPairingSessionSignatureRejected(t *testing.T) {
	server, httpServer := newTestPairingServerHTTP(t)
	session, err := server.newSession("@alice:example.com", "Alice")
	if err != nil {
		t.Fatalf("newSession() error: %v", err)
	}
	key := newTestPairKey(t)
	otherKey := newTestPairKey(t)
	deviceID := "android-ab13"
	start, clientNonce := pairStartSession(t, httpServer.URL, session, key, deviceID)

	wrongSignature := otherKey.sign(t, session.id, start.ServerNonce, clientNonce, deviceID)
	status, data := pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/complete", pairCompleteRequest{
		PairingID: session.id,
		Signature: wrongSignature,
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("pair/complete with wrong key status = %d (%s), want 401", status, data)
	}
	if code := pairDecodeErrorCode(t, data); code != "unauthorized" {
		t.Fatalf("pair/complete with wrong key error = %q, want unauthorized", code)
	}
	// No device may be created and no result delivered.
	if server.device(deviceID) != nil {
		t.Fatal("device was registered despite an invalid signature")
	}
	select {
	case result := <-session.result:
		t.Fatalf("session delivered a result despite an invalid signature: %+v", result)
	default:
	}
}

func TestPairingSessionAttemptsLimit(t *testing.T) {
	server, httpServer := newTestPairingServerHTTP(t)
	session, err := server.newSession("@alice:example.com", "Alice")
	if err != nil {
		t.Fatalf("newSession() error: %v", err)
	}
	key := newTestPairKey(t)
	deviceID := "android-ab14"
	start, clientNonce := pairStartSession(t, httpServer.URL, session, key, deviceID)
	badSignature := base64.RawURLEncoding.EncodeToString([]byte("not a valid signature"))

	for attempt := 1; attempt <= pairingMaxAttempts; attempt++ {
		status, data := pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/complete", pairCompleteRequest{
			PairingID: session.id,
			Signature: badSignature,
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d (%s), want 401", attempt, status, data)
		}
	}
	status, data := pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/complete", pairCompleteRequest{
		PairingID: session.id,
		Signature: badSignature,
	})
	if status != http.StatusForbidden {
		t.Fatalf("attempt %d status = %d (%s), want 403 too_many_attempts", pairingMaxAttempts+1, status, data)
	}
	if code := pairDecodeErrorCode(t, data); code != "too_many_attempts" {
		t.Fatalf("attempt %d error = %q, want too_many_attempts", pairingMaxAttempts+1, code)
	}
	// Even the correct signature is refused once the budget is used up.
	status, data = pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/complete", pairCompleteRequest{
		PairingID: session.id,
		Signature: key.sign(t, session.id, start.ServerNonce, clientNonce, deviceID),
	})
	if status != http.StatusForbidden {
		t.Fatalf("valid signature after exhausted attempts status = %d (%s), want 403", status, data)
	}
}

func TestPairingRateLimit(t *testing.T) {
	server := newTestPairingServer(t)
	server.rateLimitMax = 2
	server.rateLimitWindow = time.Minute
	session, err := server.newSession("@alice:example.com", "Alice")
	if err != nil {
		t.Fatalf("newSession() error: %v", err)
	}
	key := newTestPairKey(t)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	body := pairStartBody(session, key, "android-rate", testNonce(t, "client-nonce-01"))
	for i := 0; i < 2; i++ {
		status, data := pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/start", body)
		if status == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate limited too early (%s)", i+1, data)
		}
	}
	status, data := pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/start", body)
	if status != http.StatusTooManyRequests {
		t.Fatalf("third pair/start status = %d (%s), want 429", status, data)
	}
	if code := pairDecodeErrorCode(t, data); code != "rate_limited" {
		t.Fatalf("third pair/start error = %q, want rate_limited", code)
	}
}

func TestPairingInfoAndAuth(t *testing.T) {
	_, httpServer := newTestPairingServerHTTP(t)
	resp, err := http.Get(httpServer.URL + "/_smsmms/v1/info")
	if err != nil {
		t.Fatalf("GET info failed: %v", err)
	}
	defer resp.Body.Close()
	var info pairInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("failed to decode info: %v", err)
	}
	if info.ProtocolVersion != pairingProtocolVersion || !info.PairingEnabled || info.BridgeName == "" {
		t.Fatalf("unexpected info response: %+v", info)
	}

	// Queue/events/heartbeat without a token are rejected with the envelope.
	queueResp, err := http.Get(httpServer.URL + "/_smsmms/v1/queue")
	if err != nil {
		t.Fatalf("GET queue failed: %v", err)
	}
	defer queueResp.Body.Close()
	if queueResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated queue status = %d, want 401", queueResp.StatusCode)
	}
	data, _ := io.ReadAll(queueResp.Body)
	if code := pairDecodeErrorCode(t, data); code != "unauthorized" {
		t.Fatalf("unauthenticated queue error = %q, want unauthorized", code)
	}
}

func TestPairingDisabled(t *testing.T) {
	conn := NewMyConnector(zerolog.Nop())
	conn.config.Pairing.Enabled = false
	server := newPairingServer(zerolog.Nop(), conn)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	session, err := server.newSession("@alice:example.com", "Alice")
	if err != nil {
		t.Fatalf("newSession() error: %v", err)
	}
	key := newTestPairKey(t)
	status, data := pairPostJSON(t, httpServer.URL+"/_smsmms/v1/pair/start", pairStartBody(session, key, "android-off", testNonce(t, "client-nonce-01")))
	if status != http.StatusNotImplemented {
		t.Fatalf("disabled pair/start status = %d (%s), want 501", status, data)
	}
	if code := pairDecodeErrorCode(t, data); code != "unsupported" {
		t.Fatalf("disabled pair/start error = %q, want unsupported", code)
	}

	resp, err := http.Get(httpServer.URL + "/_smsmms/v1/info")
	if err != nil {
		t.Fatalf("GET info failed: %v", err)
	}
	defer resp.Body.Close()
	var info pairInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("failed to decode info: %v", err)
	}
	if info.PairingEnabled {
		t.Fatal("info reports pairing_enabled despite the config disabling it")
	}
}

func TestBuildPairingPayload(t *testing.T) {
	payload := buildPairingPayload("https://matrix.example.com", "https://bridge.example.com", "ABCD1234")
	parsed, err := url.Parse(payload)
	if err != nil {
		t.Fatalf("payload is not a valid URL: %v", err)
	}
	if parsed.Scheme != "smsmms" || parsed.Host != "pair" {
		t.Fatalf("payload = %q, want smsmms://pair?...", payload)
	}
	query := parsed.Query()
	if query.Get("hs") != "https://matrix.example.com" || query.Get("b") != "https://bridge.example.com" {
		t.Fatalf("unexpected payload query: %v", query)
	}
	if query.Get("code") != "ABCD1234" || query.Get("v") != "1" {
		t.Fatalf("unexpected payload code/version: %v", query)
	}
	noBridge := buildPairingPayload("", "", "ABCD1234")
	parsedNoBridge, err := url.Parse(noBridge)
	if err != nil {
		t.Fatalf("payload without URLs is invalid: %v", err)
	}
	if parsedNoBridge.Query().Get("hs") != "" || parsedNoBridge.Query().Get("b") != "" {
		t.Fatalf("expected hs/b to be omitted, got %q", noBridge)
	}
}

func TestBuildPairingLink(t *testing.T) {
	link := buildPairingLink("https://matrix.example.com", "https://bridge.example.com", "ABCD1234")
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("link is not a valid URL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "matrix.example.com" || parsed.Path != "/_smsmms/pair" {
		t.Fatalf("link = %q, want https://matrix.example.com/_smsmms/pair?...", link)
	}
	query := parsed.Query()
	if query.Get("hs") != "https://matrix.example.com" || query.Get("b") != "https://bridge.example.com" {
		t.Fatalf("unexpected link query: %v", query)
	}
	if query.Get("code") != "ABCD1234" || query.Get("v") != "1" {
		t.Fatalf("unexpected link code/version: %v", query)
	}
	if fallback := buildPairingLink("", "https://bridge.example.com", "ABCD1234"); fallback == "" {
		t.Fatal("link with only a bridge URL should not be empty")
	}
	if buildPairingLink("", "", "ABCD1234") != "" {
		t.Fatal("link without any URL should be empty")
	}
}
