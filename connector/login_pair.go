// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/id"
)

const (
	// LoginFlowIDAppPair is the login flow that pairs the Fossify Messages
	// fork app via QR code or manual code entry.
	LoginFlowIDAppPair = "app-pair"
	// LoginStepIDAppPairWait is the display_and_wait step showing the QR.
	LoginStepIDAppPairWait = "app-pair-wait"
)

// PairLogin is the app pairing login flow (bridgev2.LoginProcessDisplayAndWait).
// It shows a QR code / manual code, waits for the app to complete the pairing
// over the public API, and then creates the bridgev2 login.
type PairLogin struct {
	User *bridgev2.User
	Main *MyConnector
	Log  zerolog.Logger

	mu      sync.Mutex
	session *pairingSession
}

// Ensure PairLogin implements the required interface.
var _ bridgev2.LoginProcessDisplayAndWait = (*PairLogin)(nil)

// Start creates the pairing session and returns the display_and_wait step.
func (pl *PairLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	server := pl.Main.pairingServer()
	if !server.enabled() {
		return nil, fmt.Errorf("app pairing is disabled in the bridge configuration")
	}
	mxid := id.UserID("")
	if pl.User != nil && pl.User.User != nil {
		mxid = pl.User.MXID
	}
	session, err := server.newSession(mxid, mxidDisplayName(string(mxid)))
	if err != nil {
		return nil, fmt.Errorf("failed to create a pairing session: %w", err)
	}
	pl.mu.Lock()
	pl.session = session
	pl.mu.Unlock()

	payload := buildPairingPayload(pl.Main.homeserverPublicURL(), pl.Main.pairingBridgeURL(), session.code)
	instructions := strings.Join([]string{
		"Scan the QR code with the Fossify Messages bridge app (Settings -> Bridge pairing).",
		fmt.Sprintf("Or enter the pairing code manually: %s", session.code),
		fmt.Sprintf("Pairing code expires at %s.", session.expiresAt.UTC().Format("2006-01-02 15:04:05 MST")),
		fmt.Sprintf("Pairing payload: %s", payload),
	}, "\n")
	pl.Log.Info().
		Str("pairing_id", session.id).
		Str("user_mxid", string(mxid)).
		Time("expires_at", session.expiresAt).
		Msg("Started app pairing login flow")
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeDisplayAndWait,
		StepID:       LoginStepIDAppPairWait,
		Instructions: instructions,
		DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{
			Type:      bridgev2.LoginDisplayTypeQR,
			Data:      payload,
			CanCancel: true,
		},
	}, nil
}

// Wait blocks until the app completes pairing (or the code expires / the flow
// is cancelled) and then creates the bridgev2 login.
func (pl *PairLogin) Wait(ctx context.Context) (*bridgev2.LoginStep, error) {
	pl.mu.Lock()
	session := pl.session
	pl.mu.Unlock()
	if session == nil {
		return nil, fmt.Errorf("pairing session is not available; run `login %s` again", LoginFlowIDAppPair)
	}
	result, err := session.waitResult(ctx)
	if err != nil {
		if errors.Is(err, bridgev2.ErrLoginStepCancelled) {
			return nil, bridgev2.ErrLoginStepCancelled
		}
		if errors.Is(err, errPairingExpired) {
			// The session expired; drop it so its code cannot be reused.
			pl.Main.pairingServer().releaseSession(session)
			session.abandon()
		}
		return nil, err
	}
	ul, err := pl.createPairedLogin(ctx, result)
	if err != nil {
		// The token was handed to the app already; revoke it so a failed
		// login cannot leave an authorized device behind.
		pl.Main.pairingServer().revokeDevice(result.DeviceID)
		return nil, err
	}
	pl.announcePairing(ul, result)
	step := &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeComplete,
		StepID: LoginStepIDComplete,
		Instructions: fmt.Sprintf(
			"Paired the phone app %s (%s). Messages will now sync over the app pairing transport.",
			deviceDisplayName(result), result.DeviceID),
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}
	return step, nil
}

// Cancel abandons a pending pairing session.
func (pl *PairLogin) Cancel() {
	pl.mu.Lock()
	session := pl.session
	pl.session = nil
	pl.mu.Unlock()
	if session == nil {
		return
	}
	pl.Main.pairingServer().releaseSession(session)
	session.abandon()
	pl.Log.Debug().Str("pairing_id", session.id).Msg("App pairing login process cancelled")
}

// createPairedLogin builds the bridgev2 login for a completed pairing. The
// login ID is deterministic per device so re-pairing reuses the existing
// login (and its rooms).
func (pl *PairLogin) createPairedLogin(ctx context.Context, result pairSessionResult) (*bridgev2.UserLogin, error) {
	if pl.User == nil || pl.User.User == nil || pl.User.Bridge == nil {
		return nil, fmt.Errorf("the bridge is not ready to create logins")
	}
	deviceName := deviceDisplayName(result)
	if override := pl.Main.deviceNameOverride(); override != "" {
		deviceName = override
	}
	meta := &LoginMetadata{
		Transport:    LoginTransportApp,
		DeviceID:     result.DeviceID,
		DeviceName:   deviceName,
		DeviceSerial: result.DeviceID,
		DeviceModel:  strings.TrimSpace(result.DevicePlatform),
		AppVersion:   strings.TrimSpace(result.AppVersion),
		PubkeySPKI:   result.PubkeySPKI,
		TokenSHA256:  result.TokenSHA256,
		PairedAtMS:   result.PairedAtMS,
		LastSeenMS:   result.PairedAtMS,
		// These keep the generic health checks working for app logins.
		AgentProtocolVersion: pairingProtocolVersion,
		AgentCapabilities:    []string{"app-pairing", "queue-events"},
	}
	loginID := networkid.UserLoginID(stableLoginID(pairingDeviceIDPrefix + result.DeviceID))
	ul, err := pl.User.NewLogin(ctx, &database.UserLogin{
		ID:            loginID,
		RemoteName:    deviceName,
		RemoteProfile: status.RemoteProfile{Name: deviceName, Username: strings.TrimSpace(result.DevicePlatform)},
		Metadata:      meta,
	}, &bridgev2.NewLoginParams{})
	if err != nil {
		return nil, fmt.Errorf("failed to create the paired device login: %w", err)
	}
	pl.Log.Info().
		Str("login_id", string(ul.ID)).
		Str("device_id", result.DeviceID).
		Str("device_name", deviceName).
		Msg("Created app-paired login")
	// NewLogin persists the row only after LoadUserLogin returns; run the
	// space setup afterwards like the agent flow does.
	go pl.Main.ensureSpaceRoomAndPopulate(ul)
	return ul, nil
}

// announcePairing posts a confirmation into the management room so the user
// sees what got paired.
func (pl *PairLogin) announcePairing(ul *bridgev2.UserLogin, result pairSessionResult) {
	if pl.User == nil || pl.User.ManagementRoom == "" || pl.Main.bridge == nil {
		return
	}
	loginID := ""
	if ul != nil {
		loginID = string(ul.ID)
	}
	sendNotice(pl.Main.bridge.Bot, pl.User.ManagementRoom, fmt.Sprintf(
		"Paired phone app **%s** (`%s`, login `%s`). It now syncs SMS/MMS over the app pairing transport.\nUse `unpair %s` to revoke it.",
		deviceDisplayName(result), result.DeviceID, loginID, result.DeviceID))
}

func deviceDisplayName(result pairSessionResult) string {
	return firstNotEmpty(result.DeviceName, result.DeviceID)
}
