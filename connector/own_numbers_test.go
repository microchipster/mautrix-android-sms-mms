// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"slices"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

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

// TestOwnNumbersStableSubscriptionOrder covers ownUserID stability:
// Subscriptions is a map, so iteration order is random, but the own numbers
// (and therefore the outgoing sender ghost) must be stable across restarts.
func TestOwnNumbersStableSubscriptionOrder(t *testing.T) {
	t.Parallel()
	client := &MyNetworkClient{login: &bridgev2.UserLogin{UserLogin: &database.UserLogin{Metadata: &LoginMetadata{
		Subscriptions: map[int]SubscriptionInfo{
			2: {SubID: 2, PhoneNumber: "+15550002222"},
			1: {SubID: 1, PhoneNumber: "+15550001111"},
		},
	}}}}

	want := []string{"+15550001111", "+15550002222"}
	for i := 0; i < 5; i++ {
		if got := client.ownNumbers(); !slices.Equal(got, want) {
			t.Fatalf("ownNumbers() = %v, want %v (sub ID order)", got, want)
		}
	}
	if got := client.ownUserID(); got != MakeUserID("+15550001111") {
		t.Fatalf("ownUserID() = %q, want the lowest sub ID number", got)
	}
}

// TestOwnNumbersIgnoreEmptySubscriptionNumbers covers SIM slots the app
// reports without a number (or with a placeholder): they must not become the
// sender ghost, and ownUserID must fall through to a real number.
func TestOwnNumbersIgnoreEmptySubscriptionNumbers(t *testing.T) {
	t.Parallel()
	client := &MyNetworkClient{login: &bridgev2.UserLogin{UserLogin: &database.UserLogin{Metadata: &LoginMetadata{
		Subscriptions: map[int]SubscriptionInfo{
			1: {SubID: 1, PhoneNumber: ""},
			2: {SubID: 2, PhoneNumber: "+15550002222"},
		},
	}}}}

	if got := client.ownNumbers(); !slices.Equal(got, []string{"+15550002222"}) {
		t.Fatalf("ownNumbers() = %v, want only the reported number", got)
	}
	if got := client.ownUserID(); got != MakeUserID("+15550002222") {
		t.Fatalf("ownUserID() = %q, want the reported number's ghost", got)
	}
	ctx := context.Background()
	if !client.IsThisUser(ctx, MakeUserID("+15550002222")) {
		t.Error("IsThisUser(own number) = false")
	}
	if client.IsThisUser(ctx, MakeUserID("+15551112222")) {
		t.Error("IsThisUser(other number) = true")
	}
	if client.IsThisUser(ctx, networkid.UserID("")) {
		t.Error("IsThisUser(empty) = true")
	}
}

// TestSelfThreadStaysSendableAfterPortalMapping covers the portal metadata of
// a self-thread: the own number must survive as the only recipient (and as
// OtherUserID), otherwise Matrix-to-phone sends fail with "no recipients
// known for this phone thread".
func TestSelfThreadStaysSendableAfterPortalMapping(t *testing.T) {
	t.Parallel()
	client := &MyNetworkClient{
		connector: &MyConnector{config: &SimpleConfig{OwnNumbers: []string{groupTestOwn}}},
		login: &bridgev2.UserLogin{UserLogin: &database.UserLogin{Metadata: &LoginMetadata{
			DeviceSerial: "ABC123",
		}}},
	}
	portal := groupTestPortal("thread-" + DeviceScope("ABC123") + "-1-44")

	if !client.applyPortalThread(portal, agentapi.Thread{ID: 44, SubID: 1, Recipients: []string{"15550100100"}}) {
		t.Fatal("applyPortalThread(self-thread) reported no change")
	}
	meta := portal.Metadata.(*PortalMetadata)
	if !slices.Equal(meta.Recipients, []string{groupTestOwn}) {
		t.Fatalf("self-thread Recipients = %v, want the own number", meta.Recipients)
	}
	if meta.OtherUserID != MakeUserID(groupTestOwn) || meta.IsGroup {
		t.Fatalf("self-thread metadata = %+v, want a DM with the own-number ghost", meta)
	}
	// HandleMatrixMessage takes meta.Recipients (or OtherUserID as fallback),
	// so at least one of them must hold the own number.
	if len(meta.Recipients) == 0 && meta.OtherUserID == "" {
		t.Fatal("self-thread has no send target")
	}
}

// TestOwnNumberNeverBecomesGroupMember covers the membership side of the
// own-number filter: even when the phone leaks its own number into a group
// participant list, no own-number ghost is added to the room.
func TestOwnNumberNeverBecomesGroupMember(t *testing.T) {
	t.Parallel()
	client := &MyNetworkClient{
		connector: &MyConnector{config: &SimpleConfig{OwnNumbers: []string{groupTestOwn}}},
	}

	info := client.chatInfoForThread(context.Background(), agentapi.Thread{
		ID: 45, SubID: 1,
		Recipients: []string{groupTestOwn, groupTestAlice, groupTestBob},
	})
	ownGhost := MakeUserID(groupTestOwn)
	for _, member := range info.Members.Members {
		if member.Sender == ownGhost {
			t.Fatalf("own number became a room member: %+v", member)
		}
	}
	if info.Type == nil || *info.Type != database.RoomTypeDefault {
		t.Fatalf("room type = %v, want a group room after dropping the own number", info.Type)
	}
}
