// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/configupgrade"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Ensure MyConnector implements NetworkConnector.
var _ bridgev2.NetworkConnector = (*MyConnector)(nil)

// MyConnector implements the bridgev2.NetworkConnector for Android SMS/MMS.
type MyConnector struct {
	log     zerolog.Logger
	bridge  *bridgev2.Bridge
	config  *SimpleConfig
	pairing *pairingServer
	cdn     *CdnService

	ttlMu     sync.Mutex
	ttlCancel context.CancelFunc
	ttlState  ttlState

	spamMu           sync.Mutex
	spamStore        *spamStore
	spamDigest       spamDigestState
	spamDigestCancel context.CancelFunc
	spamRecent       []spamRecord
	spamCounters     spamCounters
	spamLast         time.Time
}

// NewMyConnector creates a new instance of MyConnector.
func NewMyConnector(log zerolog.Logger) *MyConnector {
	connector := &MyConnector{
		log:    log.With().Str("component", "network-connector").Logger(),
		config: NewSimpleConfig(),
	}
	connector.pairing = newPairingServer(connector.log, connector)
	return connector
}

// pairingServer returns the app pairing manager, creating it on first use for
// connectors built as a plain struct literal.
func (c *MyConnector) pairingServer() *pairingServer {
	if c.pairing == nil {
		c.pairing = newPairingServer(c.log, c)
	}
	return c.pairing
}

// StartPairingServer starts the public app pairing API unless it is disabled
// in the config. The listener also serves the CDN file service, so it is kept
// running (and started) when either feature needs it.
func (c *MyConnector) StartPairingServer() error {
	if !c.pairingEnabled() && c.cdn == nil {
		c.log.Info().Msg("App pairing API is disabled by configuration")
		return nil
	}
	return c.pairingServer().Start()
}

// Init initializes the connector with the bridge instance.
func (c *MyConnector) Init(br *bridgev2.Bridge) {
	c.bridge = br
	c.log = br.Log.With().Str("component", "network-connector").Logger()
	// The pairing server is created in NewMyConnector before the bridge
	// logger exists (and NewMyConnector is typically called with a Nop
	// logger), so its logger must be re-attached here or every pairing API
	// log line is silently dropped.
	c.pairingServer().log = c.log.With().Str("component", "app-pairing").Logger()
	if c.config == nil {
		c.config = NewSimpleConfig()
	}
	if cfg := c.cdnConfig(); cfg.Active() {
		service, err := NewCdnService(c.log, cfg, c.pairingServer().cdnAuthFunc(br))
		if err != nil {
			c.log.Error().Err(err).Msg("File service disabled: invalid cdn configuration")
		} else {
			c.cdn = service
		}
	}
	c.log.Info().Msg("MyConnector Init called")
	if !br.Config.PersonalFilteringSpaces {
		br.Config.PersonalFilteringSpaces = true
		c.log.Info().Msg("Enabled personal_filtering_spaces by default")
	}
	if proc, ok := br.Commands.(*commands.Processor); ok {
		proc.AddHandler(CommandPing)
		proc.AddHandler(CommandSync)
		proc.AddHandler(CommandSyncStatus)
		proc.AddHandler(CommandListLogins)
		proc.AddHandler(CommandListUsers)
		proc.AddHandler(CommandDeleteAllPortals)
		proc.AddHandler(CommandLogout)
		proc.AddHandler(CommandRebindRoom)
		proc.AddHandler(CommandDevices)
		proc.AddHandler(CommandUnpair)
		proc.AddHandler(CommandStats)
		proc.AddHandler(CommandLabel)
		proc.AddHandler(CommandLabels)
		proc.AddHandler(CommandNumberLabel)
		proc.AddHandler(CommandNumberLabels)
		proc.AddHandler(CommandTTL)
		proc.AddHandler(CommandSpam)
		proc.AddHandler(CommandMerge)
		proc.AddHandler(CommandMerges)
		proc.AddHandler(CommandUnmerge)
		proc.AddHandler(CommandMergeCleanup)
		proc.AddHandler(NewCdnCommand(c.cdn))
		c.log.Info().Msg("Registered custom commands")
	} else {
		c.log.Warn().Msg("Command processor not available for custom commands")
	}
}

// GetName implements bridgev2.NetworkConnector.
func (c *MyConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "Android SMS/MMS",
		NetworkURL:           "",
		NetworkIcon:          "",
		NetworkID:            "android-sms-mms",
		BeeperBridgeType:     "android-sms-mms",
		DefaultPort:          29321,
		DefaultCommandPrefix: "!sms",
	}
}

// GetNetworkID implements bridgev2.NetworkConnector.
func (c *MyConnector) GetNetworkID() string {
	return c.GetName().NetworkID
}

// GetCapabilities implements bridgev2.NetworkConnector.
func (c *MyConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		Provisioning: bridgev2.ProvisioningCapabilities{
			GroupCreation: map[string]bridgev2.GroupTypeCapabilities{
				"default": {
					TypeDescription: "Phone-number group MMS",
					Name: bridgev2.GroupFieldCapability{
						Allowed: true,
					},
					Participants: bridgev2.GroupFieldCapability{
						Allowed:   true,
						Required:  true,
						MinLength: 2,
					},
				},
			},
		},
	}
}

// GetDBMetaTypes implements bridgev2.NetworkConnector.
func (c *MyConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Portal: func() any {
			return &PortalMetadata{}
		},
		Ghost: func() any {
			return &GhostMetadata{}
		},
		Message: func() any {
			return &MessageMetadata{}
		},
		UserLogin: func() any {
			return &LoginMetadata{}
		},
	}
}

// GetLoginFlows implements bridgev2.NetworkConnector.
func (c *MyConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{
		{
			ID:   LoginFlowIDAppPair,
			Name: "App pairing (QR)",
			Description: "Pair the Fossify Messages bridge app by scanning a QR code or entering " +
				"the pairing code shown in the management room.",
		},
		{
			ID:   LoginFlowIDAgent,
			Name: "ADB agent",
			Description: "Connect to an android-sms-mms-adb-agent by URL and bearer token. " +
				"The agent is the program that reads SMS/MMS from the phone over ADB.",
		},
	}
}

// CreateLogin implements bridgev2.NetworkConnector.
func (c *MyConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	switch flowID {
	case LoginFlowIDAppPair:
		return &PairLogin{
			User: user,
			Main: c,
			Log:  user.Log.With().Str("action", "login").Str("flow", flowID).Logger(),
		}, nil
	case LoginFlowIDAgent:
		return &SimpleLogin{
			User: user,
			Main: c,
			Log:  user.Log.With().Str("action", "login").Str("flow", flowID).Logger(),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported login flow ID: %s", flowID)
	}
}

// GetConfig implements bridgev2.NetworkConnector.
func (c *MyConnector) GetConfig() (string, any, configupgrade.Upgrader) {
	if c.config == nil {
		c.config = NewSimpleConfig()
	}
	return "simple-config.yaml", c.config, nil
}

// GetBridgeInfoVersion implements bridgev2.NetworkConnector.
func (c *MyConnector) GetBridgeInfoVersion() (int, int) {
	return 1, 0
}

// Start implements bridgev2.NetworkConnector. It only logs the stored login
// inventory; per-login work happens in LoadUserLogin/Connect.
func (c *MyConnector) Start(ctx context.Context) error {
	c.log.Info().Msg("MyConnector Start called")
	if c.bridge == nil {
		c.log.Error().Msg("MyConnector started without bridge instance")
		return nil
	}
	userIDs, err := c.bridge.DB.UserLogin.GetAllUserIDsWithLogins(ctx)
	if err != nil {
		c.log.Error().Err(err).Msg("Failed to inspect stored Android SMS/MMS logins at startup")
		return nil
	}
	c.log.Info().Int("login_count", len(userIDs)).Msg("Android SMS/MMS startup login inventory")
	if len(userIDs) == 0 {
		c.log.Warn().Msg("No stored Android SMS/MMS logins found; no agent event feed will be started")
	}
	c.startHistoryTTLLoop()
	c.startSpamLoop()
	if c.cdn != nil {
		c.cdn.StartGC()
	}
	return nil
}

// Stop implements bridgev2.NetworkConnector. It shuts the pairing API down
// alongside the rest of the bridge.
func (c *MyConnector) Stop() {
	c.log.Info().Msg("MyConnector Stop called")
	c.stopHistoryTTLLoop()
	c.stopSpamLoop()
	if c.cdn != nil {
		c.cdn.StopGC()
	}
	if c.pairing == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.pairing.Stop(ctx); err != nil {
		c.log.Warn().Err(err).Msg("Failed to stop the app pairing API cleanly")
	}
}

// LoadUserLogin implements bridgev2.NetworkConnector. Per-login work must not
// block here, so the event loop is started in its own goroutine.
func (c *MyConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	mxid := ""
	if login.User != nil {
		mxid = string(login.User.MXID)
	}
	c.log.Info().
		Str("user_id", string(login.ID)).
		Str("remote_name", login.RemoteName).
		Str("mxid", mxid).
		Msg("LoadUserLogin called")

	client := NewMyNetworkClient(
		c.log.With().Str("user_id", string(login.ID)).Logger(),
		c.bridge,
		login,
		c,
	)
	login.Client = client
	// App-paired logins register their device (token hash, queue, inbox) with
	// the pairing server and attach this client as the event consumer.
	if meta, _ := login.Metadata.(*LoginMetadata); meta != nil && meta.Transport == LoginTransportApp {
		if device := c.pairingServer().ensureDevice(meta); device != nil {
			device.attachRuntime(client)
		}
	}
	client.Start()

	c.log.Info().
		Str("user_id", string(login.ID)).
		Str("remote_name", login.RemoteName).
		Msg("Created and started MyNetworkClient")
	return nil
}

// ensureSpaceRoomAndPopulate adds the management room and existing portals to
// the login's personal filtering space.
func (c *MyConnector) ensureSpaceRoomAndPopulate(login *bridgev2.UserLogin) {
	if c.bridge == nil || !c.bridge.Config.PersonalFilteringSpaces {
		return
	}
	ctx := context.Background()
	user := login.User
	if user == nil {
		return
	}
	log := c.log.With().Str("user_mxid", string(user.MXID)).Str("login_id", string(login.ID)).Logger()
	ctx = log.WithContext(ctx)

	hadMgmtRoom := user.ManagementRoom != ""
	spaceRoom, err := login.GetSpaceRoom(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to get space room")
		return
	}
	if spaceRoom == "" {
		log.Debug().Msg("Space room not available")
		return
	}
	log.Info().Stringer("space_room", spaceRoom).Msg("Ensured personal space room")

	c.ensureManagementRoomInSpace(ctx, login, spaceRoom, !hadMgmtRoom)
	c.addExistingPortalsToSpace(ctx, login)
}

func (c *MyConnector) ensureManagementRoomInSpace(ctx context.Context, login *bridgev2.UserLogin, spaceRoom id.RoomID, sendHelp bool) {
	user := login.User
	log := zerolog.Ctx(ctx)
	mgmtRoom := user.ManagementRoom
	if mgmtRoom == "" {
		log.Debug().Msg("Management room not available yet, skipping space link")
		return
	}
	if sendHelp {
		sendNotice(c.bridge.Bot, mgmtRoom, managementRoomHelpText())
	}

	_, err := c.bridge.Bot.SendState(ctx, spaceRoom, event.StateSpaceChild, mgmtRoom.String(), &event.Content{
		Parsed: &event.SpaceChildEventContent{
			Via: []string{c.bridge.Matrix.ServerName()},
		},
	}, time.Now())
	if err != nil {
		log.Err(err).Stringer("space_room", spaceRoom).Stringer("mgmt_room", mgmtRoom).Msg("Failed to add management room to space")
		return
	}
	log.Debug().Stringer("space_room", spaceRoom).Stringer("mgmt_room", mgmtRoom).Msg("Added management room to space")
}

func (c *MyConnector) addExistingPortalsToSpace(ctx context.Context, login *bridgev2.UserLogin) {
	log := zerolog.Ctx(ctx)
	userPortals, err := c.bridge.DB.UserPortal.GetAllForLogin(ctx, login.UserLogin)
	if err != nil {
		log.Err(err).Msg("Failed to list portals for space sync")
		return
	}
	for _, userPortal := range userPortals {
		portal, err := c.bridge.GetExistingPortalByKey(ctx, userPortal.Portal)
		if err != nil || portal == nil {
			continue
		}
		if err := login.AddPortalToSpace(ctx, portal, userPortal.CopyWithoutValues()); err != nil {
			log.Err(err).Msg("Failed to add portal to space")
		}
	}
}
