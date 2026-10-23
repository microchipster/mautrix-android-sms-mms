// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"slices"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"

	agentapi "mautrix-android-sms-mms/internal/agent"
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

// TestThreadRecipientsSelfThread covers a self-thread (the phone texting its
// own number, e.g. a note to self): filtering must not empty the recipient
// list, or the portal becomes unsendable with "no recipients known for this
// phone thread".
func TestThreadRecipientsSelfThread(t *testing.T) {
	client := &MyNetworkClient{connector: &MyConnector{config: &SimpleConfig{OwnNumbers: []string{"+15550100100"}}}}

	self := client.threadRecipients(agentapi.Thread{Recipients: []string{"15550100100"}})
	if !slices.Equal(self, []string{"+15550100100"}) {
		t.Fatalf("threadRecipients(self) = %v, want [+15550100100]", self)
	}

	// Duplicates and formatting differences collapse to the canonical number.
	selfDupes := client.threadRecipients(agentapi.Thread{Recipients: []string{"+1 (555) 010-0100", "+15550100100"}})
	if !slices.Equal(selfDupes, []string{"+15550100100"}) {
		t.Fatalf("threadRecipients(self, dupes) = %v, want [+15550100100]", selfDupes)
	}

	// A thread with any other participant keeps the usual group behaviour.
	mixed := client.threadRecipients(agentapi.Thread{Recipients: []string{"+15550100100", "+15551112222"}})
	if !slices.Equal(mixed, []string{"+15551112222"}) {
		t.Fatalf("threadRecipients(mixed) = %v, want [+15551112222]", mixed)
	}
}
