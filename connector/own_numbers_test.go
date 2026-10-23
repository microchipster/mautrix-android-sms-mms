// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"slices"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// TestOwnNumbersConfigFallback covers app-paired logins, which learn their own
// numbers from the heartbeat subscriptions plus the configured fallback list.
func TestOwnNumbersConfigFallback(t *testing.T) {
	connector := &MyConnector{config: &SimpleConfig{OwnNumbers: []string{"+1 (555) 000-1111", "15550002222"}}}
	client := &MyNetworkClient{connector: connector}
	got := client.ownNumbers()
	want := []string{"+15550001111", "+15550002222"}
	if !slices.Equal(got, want) {
		t.Fatalf("ownNumbers() = %v, want %v", got, want)
	}

	// The configured number must be excluded from participants.
	filtered := filterOwnNumbers([]string{"+15551112222", "+15550001111", "+15550002222"}, got)
	if !slices.Equal(filtered, []string{"+15551112222"}) {
		t.Fatalf("filterOwnNumbers() = %v, want [+15551112222]", filtered)
	}
}

// TestOwnNumbersHeartbeatSubscriptions covers the app-reported SIM numbers and
// deduplication against the configured fallback list.
func TestOwnNumbersHeartbeatSubscriptions(t *testing.T) {
	connector := &MyConnector{config: &SimpleConfig{OwnNumbers: []string{"+15550100100"}}}
	client := &MyNetworkClient{
		connector: connector,
		login: &bridgev2.UserLogin{UserLogin: &database.UserLogin{Metadata: &LoginMetadata{
			Subscriptions: map[int]SubscriptionInfo{
				1: {SubID: 1, PhoneNumber: "15550100100"},
				2: {SubID: 2, PhoneNumber: "+15550009999"},
			},
		}}},
	}
	got := client.ownNumbers()
	want := []string{"+15550100100", "+15550009999"}
	if !slices.Equal(got, want) {
		t.Fatalf("ownNumbers() = %v, want %v", got, want)
	}

	// A thread mixing the phone's own number with somebody else keeps only the
	// other participant: exactly the group-membership bug this guards against.
	filtered := filterOwnNumbers([]string{"+15550009999", "+15551234567", "+15550100100"}, got)
	if !slices.Equal(filtered, []string{"+15551234567"}) {
		t.Fatalf("filterOwnNumbers() = %v, want [+15551234567]", filtered)
	}
}
