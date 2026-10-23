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
	// Pairing configures the public app pairing API.
	Pairing PairingConfig `yaml:"pairing,omitempty"`
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
		DefaultAgentURL:     defaultAgentURL,
		HistoryDefaultDays:  defaultHistoryDefaultDays,
		MaxAttachmentSizeMB: defaultMaxAttachmentSizeMB,
		Pairing:             DefaultPairingConfig(),
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

// homeserverPublicURL returns the homeserver address the app should use. The
// public address wins so reverse-proxied setups do not leak the internal one.
func (c *MyConnector) homeserverPublicURL() string {
	if c == nil || c.bridge == nil || c.bridge.Matrix == nil {
		return ""
	}
	matrixConnector, ok := c.bridge.Matrix.(*matrix.Connector)
	if !ok || matrixConnector.Config == nil {
		return ""
	}
	if public := strings.TrimSpace(matrixConnector.Config.Homeserver.PublicAddress); public != "" {
		return public
	}
	return strings.TrimSpace(matrixConnector.Config.Homeserver.Address)
}
