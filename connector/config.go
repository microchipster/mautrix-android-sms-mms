// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/bridgev2/matrix"
)

const (
	defaultAgentURL            = "http://127.0.0.1:8093"
	defaultHistoryDefaultDays  = 30
	defaultMaxAttachmentSizeMB = 50
)

// PairingConfig holds the public app pairing API settings
// (see docs/PAIRING_PROTOCOL.md).
type PairingConfig struct {
	// Enabled controls whether the pairing API listener is started and new
	// app logins are accepted. Defaults to true.
	Enabled bool `yaml:"enabled"`
	// Listen is the address the pairing API binds to, e.g. ":29322" or
	// "127.0.0.1:29322" when a reverse proxy on the same host forwards to it.
	Listen string `yaml:"listen,omitempty"`
	// PublicURL is the externally reachable bridge base URL, e.g.
	// "https://matrix.example.com". It is embedded in the pairing payload so
	// the app knows where to poll. When empty, the homeserver public address
	// is used (the common reverse-proxy setup).
	PublicURL string `yaml:"public_url,omitempty"`
}

// DefaultPairingConfig returns the pairing configuration defaults.
func DefaultPairingConfig() PairingConfig {
	return PairingConfig{
		Enabled:   true,
		Listen:    pairingDefaultListen,
		PublicURL: pairingDefaultPublicURL,
	}
}

// UnmarshalYAML applies the defaults field-by-field so a partial mapping
// (e.g. only `listen`) never silently disables pairing.
func (p *PairingConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Enabled   *bool   `yaml:"enabled"`
		Listen    *string `yaml:"listen"`
		PublicURL *string `yaml:"public_url"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	*p = DefaultPairingConfig()
	if raw.Enabled != nil {
		p.Enabled = *raw.Enabled
	}
	if raw.Listen != nil {
		p.Listen = strings.TrimSpace(*raw.Listen)
	}
	if raw.PublicURL != nil {
		p.PublicURL = strings.TrimSpace(*raw.PublicURL)
	}
	return nil
}

// SimpleConfig stores connector-level configuration from simple-config.yaml.
//
// Per-login settings (the agent URL and bearer token) live in the login
// metadata, because one bridge instance can manage several phones. This file
// only holds defaults and behavior toggles shared by all logins.
type SimpleConfig struct {
	// DefaultAgentURL pre-fills the agent URL in the login flow.
	DefaultAgentURL string `yaml:"default_agent_url,omitempty"`
	// DefaultAgentToken pre-fills the agent bearer token in the login flow.
	DefaultAgentToken string `yaml:"default_agent_token,omitempty"`
	// SendReactionsAsText sends Matrix reactions as plain text messages
	// (`Reacted 👍 to "…"`) instead of dropping them. Incoming tapbacks are
	// always bridged as real Matrix reactions.
	SendReactionsAsText bool `yaml:"send_reactions_as_text,omitempty"`
	// HistoryDefaultDays is the automatic catch-up/backfill window in days.
	HistoryDefaultDays int `yaml:"history_default_days,omitempty"`
	// MaxAttachmentSizeMB caps outgoing attachment size (MMS carrier limit).
	MaxAttachmentSizeMB int `yaml:"max_attachment_size_mb,omitempty"`
	// DeviceName manually overrides the device display name used for the
	// login's remote name / room topic.
	DeviceName string `yaml:"device_name,omitempty"`
	// PublicHomeserverURL is the externally reachable homeserver URL advertised
	// to the phone (pairing payloads and `GET /_smsmms/v1/info`). Leave it empty
	// for the common single-host deployment: `homeserver.public_address` is used
	// when set, then the pairing public URL, then the internal address.
	PublicHomeserverURL string `yaml:"public_homeserver_url,omitempty"`
	// OwnNumbers lists the phone's own numbers (E.164 or local format).
	// Participants matching these are excluded from rooms. The adb agent
	// filters own numbers itself; app-paired logins normally learn them from
	// the app's heartbeat, and this list is the fallback for when that fails.
	OwnNumbers []string `yaml:"own_numbers,omitempty"`
	// AutoLabelNewContacts tags the ghost of a sender with the "new contact"
	// label when their first inbound message creates a portal. Labels are
	// metadata-only and can be managed with the `label`/`labels` commands.
	AutoLabelNewContacts bool `yaml:"auto_label_new_contacts"`
	// NewContactNotice posts a management-room notice (address only, never the
	// message body) when a new contact is auto-labeled.
	NewContactNotice bool `yaml:"new_contact_notice,omitempty"`
	// HistoryTTL configures the Matrix-history retention cleanup.
	HistoryTTL HistoryTTLConfig `yaml:"history_ttl,omitempty"`
	// AntiSpam configures the local-only inbound anti-spam gate
	// (see docs/anti-spam.md). Disabled unless enabled: true.
	AntiSpam AntiSpamConfig `yaml:"anti_spam,omitempty"`
	// Pairing configures the public app pairing API.
	Pairing PairingConfig `yaml:"pairing,omitempty"`
	// CDN configures the bridge-hosted encrypted file service.
	CDN CdnConfig `yaml:"cdn,omitempty"`
}

// UnmarshalYAML accepts both an inline mapping and a scalar path to a YAML
// file. mxmain writes the appservice config with `network: simple-config.yaml`
// (a scalar), so without this handler the bridge would fail to start with
// `cannot unmarshal !!str ... into connector.SimpleConfig`. Defaults are
// preserved for fields absent from the file.
func (c *SimpleConfig) UnmarshalYAML(unmarshal func(any) error) error {
	type simpleConfigAlias SimpleConfig

	var scalar string
	if err := unmarshal(&scalar); err == nil {
		value := strings.TrimSpace(scalar)
		if value == "" {
			*c = *NewSimpleConfig()
			return nil
		}
		data, err := os.ReadFile(value)
		if err != nil {
			if os.IsNotExist(err) && filepath.Base(value) == "simple-config.yaml" {
				// mxmain generates the main config but not this file. A missing
				// canonical simple-config.yaml means "use built-in defaults".
				*c = *NewSimpleConfig()
				return nil
			}
			return fmt.Errorf("read network config file %q: %w", value, err)
		}
		alias := simpleConfigAlias(*NewSimpleConfig())
		if err := yaml.Unmarshal(data, &alias); err != nil {
			return fmt.Errorf("parse network config file %q: %w", value, err)
		}
		*c = SimpleConfig(alias)
		return nil
	}

	alias := simpleConfigAlias(*NewSimpleConfig())
	if err := unmarshal(&alias); err != nil {
		return err
	}
	*c = SimpleConfig(alias)
	return nil
}

// NewSimpleConfig returns the configuration with all defaults filled in.
func NewSimpleConfig() *SimpleConfig {
	return &SimpleConfig{
		DefaultAgentURL:      defaultAgentURL,
		HistoryDefaultDays:   defaultHistoryDefaultDays,
		MaxAttachmentSizeMB:  defaultMaxAttachmentSizeMB,
		AutoLabelNewContacts: true,
		HistoryTTL:           DefaultHistoryTTLConfig(),
		AntiSpam:             DefaultAntiSpamConfig(),
		Pairing:              DefaultPairingConfig(),
		CDN:                  DefaultCdnConfig(),
	}
}

// agentURLDefault returns the configured default agent URL (never empty).
func (c *MyConnector) agentURLDefault() string {
	if c != nil && c.config != nil {
		if url := strings.TrimSpace(c.config.DefaultAgentURL); url != "" {
			return url
		}
	}
	return defaultAgentURL
}

func (c *MyConnector) agentTokenDefault() string {
	if c != nil && c.config != nil {
		return strings.TrimSpace(c.config.DefaultAgentToken)
	}
	return ""
}

func (c *MyConnector) historyDefaultDays() time.Duration {
	days := defaultHistoryDefaultDays
	if c != nil && c.config != nil && c.config.HistoryDefaultDays > 0 {
		days = c.config.HistoryDefaultDays
	}
	return time.Duration(days) * 24 * time.Hour
}

func (c *MyConnector) maxAttachmentSizeBytes() int64 {
	mb := defaultMaxAttachmentSizeMB
	if c != nil && c.config != nil && c.config.MaxAttachmentSizeMB > 0 {
		mb = c.config.MaxAttachmentSizeMB
	}
	return int64(mb) * 1024 * 1024
}

func (c *MyConnector) sendReactionsAsText() bool {
	return c != nil && c.config != nil && c.config.SendReactionsAsText
}

func (c *MyConnector) deviceNameOverride() string {
	if c != nil && c.config != nil {
		return strings.TrimSpace(c.config.DeviceName)
	}
	return ""
}

// ownNumbers returns the configured fallback list of the phone's own numbers.
func (c *MyConnector) ownNumbers() []string {
	if c == nil || c.config == nil {
		return nil
	}
	numbers := make([]string, 0, len(c.config.OwnNumbers))
	for _, number := range c.config.OwnNumbers {
		if canonical := CanonicalAddress(number); canonical != "" {
			numbers = append(numbers, canonical)
		}
	}
	return numbers
}

// autoLabelNewContacts reports whether first-contact auto-labeling is enabled.
func (c *MyConnector) autoLabelNewContacts() bool {
	if c == nil || c.config == nil {
		return true
	}
	return c.config.AutoLabelNewContacts
}

// newContactNotice reports whether new-contact auto-labeling also posts a
// management-room notice.
func (c *MyConnector) newContactNotice() bool {
	return c != nil && c.config != nil && c.config.NewContactNotice
}

// historyTTLConfig returns the configured history retention settings.
func (c *MyConnector) historyTTLConfig() HistoryTTLConfig {
	if c == nil || c.config == nil {
		return DefaultHistoryTTLConfig()
	}
	return c.config.HistoryTTL
}

// pairingEnabled reports whether the public pairing API should run. Older
// configs without a pairing section keep the feature enabled (default true).
func (c *MyConnector) pairingEnabled() bool {
	if c == nil || c.config == nil {
		return true
	}
	return c.config.Pairing.Enabled
}

// pairingListen returns the configured pairing API bind address (never empty).
func (c *MyConnector) pairingListen() string {
	if c != nil && c.config != nil {
		if listen := strings.TrimSpace(c.config.Pairing.Listen); listen != "" {
			return listen
		}
	}
	return pairingDefaultListen
}

// pairingPublicURL returns the externally reachable bridge base URL, or an
// empty string when it is unknown.
func (c *MyConnector) pairingPublicURL() string {
	if c != nil && c.config != nil {
		return strings.TrimSpace(c.config.Pairing.PublicURL)
	}
	return ""
}

// pairingBridgeURL returns the bridge base URL to embed in the pairing
// payload. When no public URL is configured the homeserver public address is
// used, which is the common reverse-proxied setup.
func (c *MyConnector) pairingBridgeURL() string {
	if public := c.pairingPublicURL(); public != "" {
		return public
	}
	return c.homeserverPublicURL()
}

// cdnConfig returns the file service configuration (never nil).
func (c *MyConnector) cdnConfig() CdnConfig {
	if c == nil || c.config == nil {
		return DefaultCdnConfig()
	}
	return c.config.CDN
}

// cdnService returns the active file service, or nil when the feature is off.
func (c *MyConnector) cdnService() *CdnService {
	if c == nil {
		return nil
	}
	return c.cdn
}

// cdnSendMode returns the effective outgoing-attachment mode: "link" while
// the file service is active and no mode is configured, otherwise the
// configured mode (and "never" without a service).
func (c *MyConnector) cdnSendMode() string {
	if c == nil || c.cdn == nil {
		return cdnSendModeNever
	}
	return c.cdnConfig().EffectiveSendMode()
}

// cdnSendThresholdBytes returns the plaintext size above which an outgoing
// attachment becomes a link. Zero (the default) links every attachment; a
// positive value is the operator's explicit threshold.
func (c *MyConnector) cdnSendThresholdBytes() int64 {
	if c == nil {
		return 0
	}
	return c.cdnConfig().SendThresholdBytes
}

// cdnPlaintextCapacityBytes returns the largest plaintext one file-service
// object can hold (the ciphertext cap minus the GCM framing), or 0 when the
// service is not active.
func (c *MyConnector) cdnPlaintextCapacityBytes() int64 {
	if c == nil || c.cdn == nil {
		return 0
	}
	capacity := c.cdnConfig().MaxFileBytes - cdnObjectOverheadBytes
	if capacity <= 0 {
		return 0
	}
	return capacity
}

// cdnSendExpiry returns the configured lifetime of links the bridge creates
// for its own outgoing attachments; zero means the service default
// (cdn.default_expiry).
func (c *MyConnector) cdnSendExpiry() time.Duration {
	if c == nil {
		return 0
	}
	return c.cdnConfig().SendExpiry.Duration()
}

// homeserverPublicURL returns the homeserver address the app should use. The
// public address wins so reverse-proxied setups do not leak the internal one;
// the pairing public URL is a good stand-in when it is not configured, because
// the playbook serves the pairing API on the homeserver's public hostname.
func (c *MyConnector) homeserverPublicURL() string {
	if c == nil {
		return ""
	}
	if c.config != nil {
		if public := strings.TrimSpace(c.config.PublicHomeserverURL); public != "" {
			return public
		}
	}
	if c.bridge == nil || c.bridge.Matrix == nil {
		return c.pairingPublicURL()
	}
	matrixConnector, ok := c.bridge.Matrix.(*matrix.Connector)
	if !ok || matrixConnector.Config == nil {
		return c.pairingPublicURL()
	}
	if public := strings.TrimSpace(matrixConnector.Config.Homeserver.PublicAddress); public != "" {
		return public
	}
	if public := c.pairingPublicURL(); public != "" {
		return public
	}
	return strings.TrimSpace(matrixConnector.Config.Homeserver.Address)
}
