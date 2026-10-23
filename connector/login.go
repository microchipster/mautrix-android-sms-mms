// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
)

const (
	LoginFlowIDAgent      = "adb-agent"
	LoginStepIDAgentInput = "adb-agent-input"
	LoginStepIDComplete   = "complete"

	// adbLoginNamespace is the UUID namespace used to derive deterministic
	// login IDs from the device serial (one login per phone).
	adbLoginNamespace = "f7a4f3e3-5d5a-4a9e-8d8a-3b0b9e8a1b2c"
)

// SimpleLogin represents an in-progress agent login (URL + bearer token).
type SimpleLogin struct {
	User *bridgev2.User
	Main *MyConnector
	Log  zerolog.Logger
}

// Ensure SimpleLogin implements the required interface.
var _ bridgev2.LoginProcessUserInput = (*SimpleLogin)(nil)

// Start implements bridgev2.LoginProcessUserInput.
func (sl *SimpleLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	sl.Log.Debug().Msg("Starting ADB agent login flow")
	agentTokenDefault := sl.Main.agentTokenDefault()
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepIDAgentInput,
		Instructions: "Enter the URL and bearer token of the android-sms-mms-adb-agent running on the " +
			"computer the phone is plugged into (default http://127.0.0.1:8093).",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type:         bridgev2.LoginInputFieldTypeURL,
					ID:           "agent_url",
					Name:         "Agent URL",
					Description:  "Base URL of the android-sms-mms-adb-agent, e.g. http://127.0.0.1:8093.",
					DefaultValue: sl.Main.agentURLDefault(),
				},
				{
					Type:         bridgev2.LoginInputFieldTypeToken,
					ID:           "agent_token",
					Name:         "Agent token",
					Description:  "Bearer token configured in the agent (leave empty if the agent has no token).",
					DefaultValue: agentTokenDefault,
					Optional:     agentTokenDefault == "",
				},
			},
		},
	}, nil
}

// SubmitUserInput implements bridgev2.LoginProcessUserInput. It validates the
// agent connection, derives a deterministic login ID from the device serial and
// creates the user login. Long-lived work (the event loop, space setup) is
// started by LoadUserLogin and after NewLogin returns.
func (sl *SimpleLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	agentURL := strings.TrimSpace(input["agent_url"])
	if agentURL == "" {
		return nil, fmt.Errorf("agent URL cannot be empty")
	}
	agentToken := strings.TrimSpace(input["agent_token"])

	sl.Log.Info().Str("agent_url", agentURL).Msg("Testing connection to android-sms-mms agent")
	client := NewAgentClient(agentURL, agentToken)
	healthCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	health, err := client.Health(healthCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to reach the agent at %s: %w", client.BaseURL, err)
	}
	if !health.OK {
		message := strings.TrimSpace(health.Error)
		if message == "" {
			message = "unknown error"
		}
		return nil, fmt.Errorf("the agent reports that it is not healthy: %s", message)
	}

	serial := strings.TrimSpace(health.Device.Serial)
	if serial == "" {
		return nil, fmt.Errorf("the agent did not report a device serial; cannot create a stable login for this phone")
	}
	loginID := networkid.UserLoginID(stableLoginID("adb:" + serial))

	existingLogin, err := sl.Main.bridge.GetExistingUserLoginByID(ctx, loginID)
	if err != nil {
		sl.Log.Err(err).Str("login_id", string(loginID)).Msg("Failed to check for existing user login")
		return nil, fmt.Errorf("failed to check existing login for device %q: %w", serial, err)
	}
	if existingLogin != nil {
		switch {
		case existingLogin.User == nil:
			return nil, fmt.Errorf("an existing login for device %q is in an invalid state; please remove it before logging in again", serial)
		case existingLogin.User.MXID != sl.User.MXID:
			return nil, fmt.Errorf("the phone with serial %q is already bridged by another Matrix user", serial)
		case existingLogin.ID != "":
			return nil, fmt.Errorf("the phone with serial %q is already logged in for this Matrix user as login %s; use `list-logins` or `logout %s` first", serial, existingLogin.ID, existingLogin.ID)
		default:
			return nil, fmt.Errorf("the phone with serial %q is already logged in", serial)
		}
	}

	deviceName := sl.Main.deviceNameOverride()
	if deviceName == "" {
		deviceName = strings.TrimSpace(health.Device.Model)
	}
	if deviceName == "" {
		deviceName = "Android device " + serial
	}
	profile := status.RemoteProfile{Name: deviceName}
	subscriptions := make(map[int]SubscriptionInfo, len(health.Subscriptions))
	for _, sub := range health.Subscriptions {
		subscriptions[sub.SubID] = SubscriptionInfo{
			SubID:        sub.SubID,
			PhoneNumber:  sub.Number,
			DisplayName:  sub.DisplayName,
			IsDefaultSMS: sub.DefaultSMS,
		}
		if profile.Phone == "" && sub.Number != "" {
			profile.Phone = sub.Number
		}
	}
	defaultSMSApp := strings.TrimSpace(health.DefaultSMSApp)
	if defaultSMSApp == "" {
		defaultSMSApp = strings.TrimSpace(health.Backend.DefaultSMSApp)
	}

	ul, err := sl.User.NewLogin(ctx, &database.UserLogin{
		ID:            loginID,
		RemoteName:    deviceName,
		RemoteProfile: profile,
		Metadata: &LoginMetadata{
			AgentURL:             client.BaseURL,
			AgentToken:           agentToken,
			DeviceSerial:         serial,
			DeviceModel:          strings.TrimSpace(health.Device.Model),
			AndroidVersion:       strings.TrimSpace(health.Device.AndroidRelease),
			DefaultSMSApp:        defaultSMSApp,
			SendBackend:          strings.TrimSpace(health.Backend.SendSMS),
			AgentProtocolVersion: health.ProtocolVersion,
			AgentCapabilities:    health.Capabilities,
			Subscriptions:        subscriptions,
		},
	}, &bridgev2.NewLoginParams{})
	if err != nil {
		sl.Log.Err(err).Msg("Failed to create user login entry")
		return nil, fmt.Errorf("failed to create user login: %w", err)
	}

	sl.Log.Info().
		Str("login_id", string(ul.ID)).
		Str("device_serial", serial).
		Str("agent_url", client.BaseURL).
		Msg("Successfully connected to the phone agent and created user login")
	// NewLogin persists the row only after LoadUserLogin returns, so the space
	// work has to run afterwards. The event loop is started by LoadUserLogin.
	go sl.Main.ensureSpaceRoomAndPopulate(ul)

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       LoginStepIDComplete,
		Instructions: fmt.Sprintf("Successfully connected to the phone agent managing %s (%s).", deviceName, serial),
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}

// Cancel implements bridgev2.LoginProcessUserInput.
func (sl *SimpleLogin) Cancel() {
	sl.Log.Debug().Msg("Login process cancelled")
}

// stableLoginID produces a deterministic UUIDv5-style identifier
// (sha1(namespace || name), version 5 + RFC 4122 variant) without pulling in a
// UUID dependency.
func stableLoginID(name string) string {
	var namespace [16]byte
	rawNamespace, err := hex.DecodeString(strings.ReplaceAll(adbLoginNamespace, "-", ""))
	if err == nil && len(rawNamespace) == 16 {
		copy(namespace[:], rawNamespace)
	}
	sum := sha1.Sum(append(namespace[:], []byte(name)...))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(sum[0:4]),
		hex.EncodeToString(sum[4:6]),
		hex.EncodeToString(sum[6:8]),
		hex.EncodeToString(sum[8:10]),
		hex.EncodeToString(sum[10:16]))
}
