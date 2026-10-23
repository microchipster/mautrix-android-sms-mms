// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

func newTestPairLogin(t *testing.T) *PairLogin {
	t.Helper()
	connector := NewMyConnector(zerolog.Nop())
	user := &bridgev2.User{
		User: &database.User{MXID: "@alice:example.com"},
		Log:  zerolog.Nop(),
	}
	return &PairLogin{
		User: user,
		Main: connector,
		Log:  zerolog.Nop(),
	}
}

func TestPairLoginStartPayload(t *testing.T) {
	login := newTestPairLogin(t)
	step, err := login.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if step.Type != bridgev2.LoginStepTypeDisplayAndWait {
		t.Fatalf("step.Type = %q, want display_and_wait", step.Type)
	}
	if step.StepID != LoginStepIDAppPairWait {
		t.Fatalf("step.StepID = %q, want %q", step.StepID, LoginStepIDAppPairWait)
	}
	if step.DisplayAndWaitParams == nil || step.DisplayAndWaitParams.Type != bridgev2.LoginDisplayTypeQR {
		t.Fatalf("unexpected display params: %+v", step.DisplayAndWaitParams)
	}
	if !step.DisplayAndWaitParams.CanCancel {
		t.Error("the QR step should be cancellable")
	}

	payload := step.DisplayAndWaitParams.Data
	parsed, err := url.Parse(payload)
	if err != nil {
		t.Fatalf("payload %q is not a valid URL: %v", payload, err)
	}
	if parsed.Scheme != "smsmms" || parsed.Host != "pair" {
		t.Fatalf("payload %q is not an smsmms://pair URL", payload)
	}
	login.mu.Lock()
	session := login.session
	login.mu.Unlock()
	if session == nil {
		t.Fatal("Start() did not create a pairing session")
	}
	if code := parsed.Query().Get("code"); code != session.code {
		t.Fatalf("payload code = %q, want %q", code, session.code)
	}
	if parsed.Query().Get("v") != "1" {
		t.Fatalf("payload version = %q, want 1", parsed.Query().Get("v"))
	}
	if !strings.Contains(step.Instructions, session.code) {
		t.Fatalf("instructions do not include the manual code: %q", step.Instructions)
	}
	if !strings.Contains(step.Instructions, payload) {
		t.Fatalf("instructions do not include the payload: %q", step.Instructions)
	}
	if login.Main.pairingServer().sessionByID(session.id) == nil {
		t.Fatal("session was not registered on the pairing server")
	}

	// Cancel removes the session so the code cannot be reused.
	login.Cancel()
	if login.Main.pairingServer().sessionByID(session.id) != nil {
		t.Fatal("session still registered after Cancel()")
	}
	if _, err := login.Wait(context.Background()); err == nil {
		t.Fatal("Wait() after Cancel() should fail")
	}
}

func TestPairLoginWaitCancelled(t *testing.T) {
	login := newTestPairLogin(t)
	if _, err := login.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel(bridgev2.ErrLoginStepCancelled)
	}()
	_, err := login.Wait(ctx)
	if !errors.Is(err, bridgev2.ErrLoginStepCancelled) {
		t.Fatalf("Wait() error = %v, want ErrLoginStepCancelled", err)
	}
}

func TestPairLoginWaitExpiry(t *testing.T) {
	login := newTestPairLogin(t)
	login.Main.pairingServer().ttl = 40 * time.Millisecond
	if _, err := login.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	_, err := login.Wait(context.Background())
	if !errors.Is(err, errPairingExpired) {
		t.Fatalf("Wait() error = %v, want errPairingExpired", err)
	}
	login.mu.Lock()
	session := login.session
	login.mu.Unlock()
	if session != nil && login.Main.pairingServer().sessionByID(session.id) != nil {
		t.Fatal("expired session was not released")
	}
}

func TestPairLoginWaitsForResult(t *testing.T) {
	login := newTestPairLogin(t)
	if _, err := login.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	login.mu.Lock()
	session := login.session
	login.mu.Unlock()

	resultCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Wait will fail because there is no bridge instance to create the
		// login on, but it must first deliver the pairing result.
		_, err := login.Wait(ctx)
		resultCh <- err
	}()
	result := pairSessionResult{
		DeviceID:       "android-login",
		DeviceName:     "Login Phone",
		DevicePlatform: "android",
		TokenSHA256:    hashPairingToken("token"),
		PubkeySPKI:     "pubkey",
		PairedAtMS:     time.Now().UnixMilli(),
	}
	login.Main.pairingServer().activateDevice(result, result.TokenSHA256)
	session.deliver(result)
	select {
	case err := <-resultCh:
		if err == nil {
			t.Fatal("Wait() should fail without a bridge instance")
		}
		if !strings.Contains(err.Error(), "bridge") {
			t.Fatalf("unexpected Wait() error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait() did not return after the result was delivered")
	}
	// A failed login flow must revoke the token that was handed to the app.
	if login.Main.pairingServer().device("android-login") != nil {
		t.Fatal("device token was not revoked after a failed login creation")
	}
}
