// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func unmarshalNetworkConfig(t *testing.T, content string) *SimpleConfig {
	t.Helper()
	var wrapper struct {
		Network *SimpleConfig `yaml:"network"`
	}
	if err := yaml.Unmarshal([]byte(content), &wrapper); err != nil {
		t.Fatalf("failed to unmarshal network config: %v", err)
	}
	if wrapper.Network == nil {
		t.Fatal("network config was not parsed")
	}
	return wrapper.Network
}

func TestSimpleConfigUnmarshalScalarFilePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "simple-config.yaml")
	if err := os.WriteFile(path, []byte("send_reactions_as_text: true\nhistory_default_days: 7\n"), 0o600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg := unmarshalNetworkConfig(t, "network: "+path+"\n")
	if !cfg.SendReactionsAsText {
		t.Error("send_reactions_as_text was not read from the referenced file")
	}
	if cfg.HistoryDefaultDays != 7 {
		t.Errorf("history_default_days = %d, want 7", cfg.HistoryDefaultDays)
	}
	// Fields absent from the file keep their defaults.
	if cfg.DefaultAgentURL != defaultAgentURL {
		t.Errorf("default_agent_url = %q, want default %q", cfg.DefaultAgentURL, defaultAgentURL)
	}
	if cfg.MaxAttachmentSizeMB != defaultMaxAttachmentSizeMB {
		t.Errorf("max_attachment_size_mb = %d, want default %d", cfg.MaxAttachmentSizeMB, defaultMaxAttachmentSizeMB)
	}
}

func TestSimpleConfigUnmarshalScalarFileRelativeName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "simple-config.yaml"), []byte("device_name: Pixel\n"), 0o600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("failed to restore working directory: %v", err)
		}
	}()

	cfg := unmarshalNetworkConfig(t, "network: simple-config.yaml\n")
	if cfg.DeviceName != "Pixel" {
		t.Errorf("device_name = %q, want %q", cfg.DeviceName, "Pixel")
	}
}

func TestSimpleConfigUnmarshalMissingFile(t *testing.T) {
	var wrapper struct {
		Network *SimpleConfig `yaml:"network"`
	}
	err := yaml.Unmarshal([]byte("network: /nonexistent/custom-config.yaml\n"), &wrapper)
	if err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

func TestSimpleConfigUnmarshalMissingCanonicalFileUsesDefaults(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("failed to restore working directory: %v", err)
		}
	}()

	cfg := unmarshalNetworkConfig(t, "network: simple-config.yaml\n")
	if cfg.DefaultAgentURL != defaultAgentURL || cfg.HistoryDefaultDays != defaultHistoryDefaultDays {
		t.Errorf("defaults not applied when the canonical file is missing: %+v", cfg)
	}
}

func TestSimpleConfigUnmarshalInlineMapping(t *testing.T) {
	cfg := unmarshalNetworkConfig(t, "network:\n  max_attachment_size_mb: 10\n  default_agent_url: http://10.0.0.5:8093\n")
	if cfg.MaxAttachmentSizeMB != 10 {
		t.Errorf("max_attachment_size_mb = %d, want 10", cfg.MaxAttachmentSizeMB)
	}
	if cfg.DefaultAgentURL != "http://10.0.0.5:8093" {
		t.Errorf("default_agent_url = %q", cfg.DefaultAgentURL)
	}
	if cfg.HistoryDefaultDays != defaultHistoryDefaultDays {
		t.Errorf("history_default_days = %d, want default %d", cfg.HistoryDefaultDays, defaultHistoryDefaultDays)
	}
}

func TestSimpleConfigUnmarshalEmptyMappingKeepsDefaults(t *testing.T) {
	cfg := unmarshalNetworkConfig(t, "network: {}\n")
	if cfg.DefaultAgentURL != defaultAgentURL || cfg.HistoryDefaultDays != defaultHistoryDefaultDays || cfg.MaxAttachmentSizeMB != defaultMaxAttachmentSizeMB {
		t.Errorf("defaults not applied for empty mapping: %+v", cfg)
	}
}

func TestPairingConfigDefaults(t *testing.T) {
	cfg := unmarshalNetworkConfig(t, "network: {}\n")
	if !cfg.Pairing.Enabled {
		t.Error("pairing should be enabled by default")
	}
	if cfg.Pairing.Listen != pairingDefaultListen {
		t.Errorf("pairing listen = %q, want %q", cfg.Pairing.Listen, pairingDefaultListen)
	}
	if cfg.Pairing.PublicURL != "" {
		t.Errorf("pairing public_url = %q, want empty", cfg.Pairing.PublicURL)
	}
}

func TestPairingConfigPartialMappingKeepsDefaults(t *testing.T) {
	cfg := unmarshalNetworkConfig(t, "network:\n  pairing:\n    listen: \"127.0.0.1:12345\"\n")
	if !cfg.Pairing.Enabled {
		t.Error("pairing should stay enabled when the mapping only sets listen")
	}
	if cfg.Pairing.Listen != "127.0.0.1:12345" {
		t.Errorf("pairing listen = %q, want 127.0.0.1:12345", cfg.Pairing.Listen)
	}
}

func TestPairingConfigExplicitValues(t *testing.T) {
	cfg := unmarshalNetworkConfig(t, "network:\n  pairing:\n    enabled: false\n    listen: \":9999\"\n    public_url: https://bridge.example.com\n")
	if cfg.Pairing.Enabled {
		t.Error("pairing should be disabled when enabled: false")
	}
	if cfg.Pairing.Listen != ":9999" {
		t.Errorf("pairing listen = %q, want :9999", cfg.Pairing.Listen)
	}
	if cfg.Pairing.PublicURL != "https://bridge.example.com" {
		t.Errorf("pairing public_url = %q", cfg.Pairing.PublicURL)
	}
}

func TestPairingConfigFromSidecarFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "simple-config.yaml")
	if err := os.WriteFile(path, []byte("pairing:\n  public_url: https://bridge.example.com\n"), 0o600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	cfg := unmarshalNetworkConfig(t, "network: "+path+"\n")
	if !cfg.Pairing.Enabled || cfg.Pairing.Listen != pairingDefaultListen {
		t.Errorf("pairing defaults not kept from the sidecar file: %+v", cfg.Pairing)
	}
	if cfg.Pairing.PublicURL != "https://bridge.example.com" {
		t.Errorf("pairing public_url = %q", cfg.Pairing.PublicURL)
	}
}

func TestHomeserverPublicURLPrecedence(t *testing.T) {
	// An explicit network override wins, even without a bridge.
	nc := &MyConnector{config: &SimpleConfig{PublicHomeserverURL: "https://hs.example.com"}}
	if got := nc.homeserverPublicURL(); got != "https://hs.example.com" {
		t.Errorf("homeserverPublicURL() = %q, want the explicit network override", got)
	}

	// Without an override the pairing public URL is used, because the playbook
	// serves the pairing API on the homeserver's public hostname.
	nc = &MyConnector{config: &SimpleConfig{Pairing: PairingConfig{PublicURL: "https://matrix.example.com"}}}
	if got := nc.homeserverPublicURL(); got != "https://matrix.example.com" {
		t.Errorf("homeserverPublicURL() = %q, want the pairing public URL", got)
	}

	// Nothing configured: stay empty rather than advertise a wrong address.
	nc = &MyConnector{config: &SimpleConfig{}}
	if got := nc.homeserverPublicURL(); got != "" {
		t.Errorf("homeserverPublicURL() = %q, want empty", got)
	}

	// Nil receiver stays safe.
	var nilConnector *MyConnector
	if got := nilConnector.homeserverPublicURL(); got != "" {
		t.Errorf("nil homeserverPublicURL() = %q, want empty", got)
	}
}
