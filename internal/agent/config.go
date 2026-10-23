// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the agent configuration (agent.yaml).
type Config struct {
	// AdbPath is the adb binary to use.
	AdbPath string `yaml:"adb_path"`
	// Serial selects the device ("-s"). Empty = single attached device.
	Serial string `yaml:"serial"`
	// Listen is the HTTP bind address.
	Listen string `yaml:"listen"`
	// Token is the bearer token required by all routes except /v1/health.
	Token string `yaml:"token"`
	// PollIntervalMS is the sweep interval (default 3000).
	PollIntervalMS int `yaml:"poll_interval_ms"`
	// SweepOverlapMS is how far back each sweep re-reads (default 10 minutes).
	SweepOverlapMS int64 `yaml:"sweep_overlap_ms"`
	// OwnNumbers are excluded from thread recipients (usually the phone's own number).
	OwnNumbers []string `yaml:"own_numbers"`
	// SubIDs limits which SIM subscriptions are bridged. Empty = all.
	SubIDs []int `yaml:"sub_ids"`
	// DataDir stores watermarks and dedupe state.
	DataDir string `yaml:"data_dir"`
	// HistoryLimit caps history pages returned per request (default 200).
	HistoryLimit int `yaml:"history_limit"`
	// Send configures outgoing messages.
	Send SendConfig `yaml:"send"`
}

// SendConfig configures the send backends.
type SendConfig struct {
	Enabled bool `yaml:"enabled"`
	// TxCode overrides the `service call isms` transaction code (0 = auto-detect from SDK).
	TxCode int `yaml:"isms_send_tx_code"`
	// AllowSMSSplit permits splitting messages over 160 UTF-16 code units into multiple
	// SMS (the shell backend cannot do real multipart). Off by default so long messages
	// fail loudly instead of silently fracturing.
	AllowSMSSplit bool `yaml:"allow_sms_split"`
	// CallerPackage is passed to the telephony service as the calling package.
	CallerPackage string `yaml:"caller_package"`
	// ResolveTimeoutMS is how long a send waits for the provider row to appear.
	ResolveTimeoutMS int `yaml:"resolve_timeout_ms"`
}

// DefaultConfig returns the built-in defaults.
func DefaultConfig() Config {
	return Config{
		AdbPath:        "adb",
		Listen:         "127.0.0.1:8093",
		PollIntervalMS: 3000,
		SweepOverlapMS: int64((10 * time.Minute) / time.Millisecond),
		DataDir:        "./agent-data",
		HistoryLimit:   200,
		Send: SendConfig{
			Enabled:          true,
			CallerPackage:    "com.android.shell",
			ResolveTimeoutMS: 30000,
		},
	}
}

// LoadConfig reads the config file and applies defaults for missing fields.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err = yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if cfg.AdbPath == "" {
		cfg.AdbPath = "adb"
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8093"
	}
	if cfg.PollIntervalMS <= 0 {
		cfg.PollIntervalMS = 3000
	}
	if cfg.SweepOverlapMS <= 0 {
		cfg.SweepOverlapMS = int64((10 * time.Minute) / time.Millisecond)
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "./agent-data"
	}
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = 200
	}
	if cfg.Send.CallerPackage == "" {
		cfg.Send.CallerPackage = "com.android.shell"
	}
	if cfg.Send.ResolveTimeoutMS <= 0 {
		cfg.Send.ResolveTimeoutMS = 30000
	}
	return &cfg, nil
}

// ExampleConfig renders an example config file with comments.
func ExampleConfig() string {
	cfg := DefaultConfig()
	out, _ := yaml.Marshal(&cfg)
	return `# sms-mms-adb-agent configuration
# See docs/AGENT_API.md for the HTTP API served by this agent.
` + string(out)
}
