// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/id"
)

// pairingError is an error of the public pairing API. It carries the machine
// code and HTTP status that end up in the {"error","message"} envelope.
type pairingError struct {
	status  int
	code    string
	message string
}

func (e *pairingError) Error() string {
	if e == nil {
		return "pairing error"
	}
	if e.message != "" {
		return fmt.Sprintf("pairing %s: %s", e.code, e.message)
	}
	return "pairing " + e.code
}

func newPairingError(status int, code, message string) *pairingError {
	return &pairingError{status: status, code: code, message: message}
}

// asPairingError returns the pairing error wrapped by err, if any.
func asPairingError(err error) (*pairingError, bool) {
	var perr *pairingError
	if errors.As(err, &perr) {
		return perr, true
	}
	return nil, false
}

// errPairingExpired is returned to the waiting login flow when the code TTL
// runs out before the app completes pairing.
var errPairingExpired = errors.New("the pairing code expired; run `login app-pair` again for a fresh code")

// errPairingCancelled is returned when the login flow is cancelled while
// waiting for the app.
var errPairingCancelled = errors.New("pairing was cancelled")

// pairingSession is one in-flight pairing: the code shown to the user, the
// nonces exchanged with the app, and the channel that hands the finished
// login back to the waiting flow.
type pairingSession struct {
	id          string
	code        string
	mxid        id.UserID
	displayName string
	createdAt   time.Time
	expiresAt   time.Time
	maxAttempts int

	mu          sync.Mutex
	attempts    int
	started     bool
	used        bool
	device      pairDeviceInfo
	pubkeyDER   []byte
	pubkeySPKI  string
	clientNonce string
	serverNonce string

	result   chan pairSessionResult
	done     chan struct{}
	doneOnce sync.Once
}

func newPairingSession(mxid id.UserID, displayName string, ttl time.Duration, maxAttempts int) *pairingSession {
	now := time.Now()
	return &pairingSession{
		mxid:        mxid,
		displayName: displayName,
		createdAt:   now,
		expiresAt:   now.Add(ttl),
		maxAttempts: maxAttempts,
		result:      make(chan pairSessionResult, 1),
		done:        make(chan struct{}),
	}
}

// begin records the device data of a pair/start request and returns the
// server nonce the device must sign. Identical retries of the same start
// request keep the original nonce so a lost HTTP response does not break
// signing; any other restart counts against the attempt budget.
func (ps *pairingSession) begin(req pairStartRequest, pubkeyDER []byte, serverNonce string) (string, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	now := time.Now()
	if ps.used {
		return "", newPairingError(404, "not_found", "unknown or already used pairing code")
	}
	if now.After(ps.expiresAt) {
		return "", newPairingError(410, "expired", "the pairing code has expired")
	}
	if ps.attempts >= ps.maxAttempts {
		return "", newPairingError(403, "too_many_attempts", "too many pairing attempts; run `login app-pair` again")
	}
	if ps.started {
		if ps.device.ID == req.Device.ID && bytes.Equal(ps.pubkeyDER, pubkeyDER) && ps.clientNonce == req.ClientNonce {
			return ps.serverNonce, nil
		}
		// A restart invalidates the previous nonces; count it against the
		// attempt budget so a wrong client nonce cannot be retried forever.
		ps.attempts++
	}
	ps.started = true
	ps.device = req.Device
	ps.pubkeyDER = pubkeyDER
	ps.pubkeySPKI = base64.RawURLEncoding.EncodeToString(pubkeyDER)
	ps.clientNonce = req.ClientNonce
	ps.serverNonce = serverNonce
	return serverNonce, nil
}

// finish verifies the device signature and, on success, marks the session
// used and returns the data for the login flow.
func (ps *pairingSession) finish(signature []byte) (pairSessionResult, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.used {
		return pairSessionResult{}, newPairingError(404, "not_found", "unknown or already used pairing code")
	}
	if time.Now().After(ps.expiresAt) {
		return pairSessionResult{}, newPairingError(410, "expired", "the pairing code has expired")
	}
	if !ps.started {
		return pairSessionResult{}, newPairingError(400, "bad_request", "pairing has not been started")
	}
	if ps.attempts >= ps.maxAttempts {
		return pairSessionResult{}, newPairingError(403, "too_many_attempts", "too many pairing attempts; run `login app-pair` again")
	}
	if err := ps.verifySignatureLocked(signature); err != nil {
		ps.attempts++
		return pairSessionResult{}, err
	}
	ps.used = true
	return pairSessionResult{
		DeviceID:       ps.device.ID,
		DeviceName:     firstNotEmpty(ps.device.Name, ps.device.ID),
		DevicePlatform: ps.device.Platform,
		AppVersion:     ps.device.AppVersion,
		PubkeySPKI:     ps.pubkeySPKI,
		PairedAtMS:     time.Now().UnixMilli(),
	}, nil
}

// verifySignatureLocked checks the ECDSA P-256 signature over the pairing
// challenge. It must be called with ps.mu held.
func (ps *pairingSession) verifySignatureLocked(signature []byte) error {
	if len(signature) == 0 {
		return newPairingError(400, "bad_request", "missing device signature")
	}
	parsed, err := x509.ParsePKIXPublicKey(ps.pubkeyDER)
	if err != nil {
		return newPairingError(400, "bad_request", "pubkey is not a valid SPKI DER key")
	}
	publicKey, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return newPairingError(400, "bad_request", "pubkey is not an ECDSA key")
	}
	digest := sha256.Sum256([]byte(pairingSigningInput(ps.id, ps.serverNonce, ps.clientNonce, ps.device.ID)))
	if !ecdsa.VerifyASN1(publicKey, digest[:], signature) {
		return newPairingError(401, "unauthorized", "invalid device signature")
	}
	return nil
}

// deliver hands the finished pairing to the waiting login flow without
// blocking the HTTP handler.
func (ps *pairingSession) deliver(result pairSessionResult) {
	select {
	case ps.result <- result:
	default:
	}
}

// abandon unblocks the waiting login flow when the session is removed.
func (ps *pairingSession) abandon() {
	ps.doneOnce.Do(func() {
		close(ps.done)
	})
}

// waitResult blocks until the app completes pairing, the session expires, the
// flow is cancelled, or the session is abandoned.
func (ps *pairingSession) waitResult(ctx context.Context) (pairSessionResult, error) {
	remaining := time.Until(ps.expiresAt)
	if remaining <= 0 {
		return pairSessionResult{}, errPairingExpired
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case result := <-ps.result:
		return result, nil
	case <-ps.done:
		return pairSessionResult{}, errPairingCancelled
	case <-timer.C:
		return pairSessionResult{}, errPairingExpired
	case <-ctx.Done():
		cause := context.Cause(ctx)
		if errors.Is(cause, bridgev2.ErrLoginStepCancelled) || errors.Is(cause, context.Canceled) {
			return pairSessionResult{}, bridgev2.ErrLoginStepCancelled
		}
		return pairSessionResult{}, fmt.Errorf("pairing wait interrupted: %w", cause)
	}
}

// firstNotEmpty returns the first non-empty trimmed value.
func firstNotEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
