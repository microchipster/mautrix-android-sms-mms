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
)

const (
	defaultAgentURL            = "http://127.0.0.1:8093"
	defaultHistoryDefaultDays  = 30
	defaultMaxAttachmentSizeMB = 50
)

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
