// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
)

// TestInitAttachesPairingServerLogger guards against the pairing server
// keeping the Nop logger it was created with: before the fix, `App pairing
// API listening` / `Pairing session started by app` never reached the bridge
// log, which the local E2E run exposed.
func TestInitAttachesPairingServerLogger(t *testing.T) {
	var buf bytes.Buffer
	connector := NewMyConnector(zerolog.Nop())
	connector.Init(&bridgev2.Bridge{
		Log:    zerolog.New(&buf),
		Config: &bridgeconfig.BridgeConfig{},
	})

	connector.pairingServer().log.Info().Str("pairing_id", "pr_test").Msg("Pairing session started by app")

	logged := buf.String()
	if !strings.Contains(logged, "Pairing session started by app") || !strings.Contains(logged, "pr_test") {
		t.Fatalf("pairing server logs did not reach the bridge logger, got: %q", logged)
	}
	if !strings.Contains(logged, `"component":"app-pairing"`) {
		t.Fatalf("pairing server logger is missing the app-pairing component, got: %q", logged)
	}
}
