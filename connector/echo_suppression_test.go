// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

// Identities used by the echo-suppression regression tests. They mirror the
// live incident: a bridge send into a two-participant phone thread whose
// message.new echo raced its send.result and wiped the room.
const (
	echoTestSendID = "$matrixEventID"
	echoTestRowID  = "mms:2:57"
	echoTestThread = 2
	echoTestSub    = 2
	echoAlice      = "+15550100203"
	echoBob        = "+15550100201"
)

// echoTestClient builds an app-paired client for the suppression-only paths:
// no bridge is needed as long as the events are recognized as echoes.
func echoTestClient(t *testing.T, pendingSendIDs ...string) (*MyNetworkClient, *pairedDevice) {
	t.Helper()
	server := newTestPairingServer(t)
	device := newTestPairedDevice(t, server, "android-echo", "echo-token")
	nc := &MyNetworkClient{log: zerolog.Nop(), device: device}
	for _, sendID := range pendingSendIDs {
		nc.trackOutboundSend(sendID)
	}
	return nc, device
}

// echoEvent is the payload-less message.new the app pushes for a send
// performed over the pairing API (BridgeEvents.onMessageSent).
func echoEvent(messageID, sendID string) remoteEvent {
	return remoteEvent{Event: agentapi.Event{
		Type:      "message.new",
		MessageID: messageID,
		ThreadID:  echoTestThread,
		SubID:     echoTestSub,
		Sender:    "",
		SendID:    sendID,
	}}
}

// sendResultEvent is the app's send.result for the same send.
func sendResultEvent(sendID, messageID string) remoteEvent {
	return remoteEvent{Event: agentapi.Event{
		Type:      "send.result",
		SendID:    sendID,
		MessageID: messageID,
		ThreadID:  echoTestThread,
		SubID:     echoTestSub,
	}}
}

// TestEchoSuppressedWhenMessageNewArrivesBeforeSendResult covers the live
// ordering: the app's message.new echo reaches the event loop before the
// send.result was resolved. The send id is tracked when the send is dispatched,
// so the echo is recognized and never bridged as a phone-originated message.
func TestEchoSuppressedWhenMessageNewArrivesBeforeSendResult(t *testing.T) {
	nc, device := echoTestClient(t, echoTestSendID)
	ctx := context.Background()

	nc.handleRemoteEvent(ctx, echoEvent(echoTestRowID, echoTestSendID))

	if device.hasPendingSend(echoTestSendID) {
		t.Fatal("the echo did not consume the pending send")
	}
	if !nc.consumeSentFromMatrix(networkid.MessageID(echoTestRowID)) {
		t.Fatal("the echo did not mark the phone row id as sent from Matrix")
	}

	// The send.result still arrives afterwards; it must resolve cleanly (no
	// live waiter) and keep the suppression state for replays.
	nc.handleRemoteEvent(ctx, sendResultEvent(echoTestSendID, echoTestRowID))
	if !nc.consumeSentFromMatrix(networkid.MessageID(echoTestRowID)) {
		t.Fatal("a replayed echo without send correlation would not be suppressed")
	}
}

// TestEchoSuppressedWhenSendResultArrivesFirst covers the opposite ordering:
// the send.result is processed first and the echo follows, both with and
// without the send-id correlation (older app versions).
func TestEchoSuppressedWhenSendResultArrivesFirst(t *testing.T) {
	ctx := context.Background()

	t.Run("correlated echo", func(t *testing.T) {
		nc, _ := echoTestClient(t, echoTestSendID)
		nc.handleRemoteEvent(ctx, sendResultEvent(echoTestSendID, echoTestRowID))
		if got := nc.classifyOutboundEcho(agentapi.Message{ID: echoTestRowID, ThreadID: echoTestThread, SubID: echoTestSub}, echoTestSendID); got != bridgeEchoSuppressed {
			t.Fatalf("classifyOutboundEcho() = %v, want bridgeEchoSuppressed", got)
		}
	})

	t.Run("uncorrelated echo", func(t *testing.T) {
		nc, _ := echoTestClient(t, echoTestSendID)
		nc.handleRemoteEvent(ctx, sendResultEvent(echoTestSendID, echoTestRowID))
		if got := nc.classifyOutboundEcho(agentapi.Message{ID: echoTestRowID, ThreadID: echoTestThread, SubID: echoTestSub}, ""); got != bridgeEchoSuppressed {
			t.Fatalf("classifyOutboundEcho() = %v, want bridgeEchoSuppressed", got)
		}
	})
}

// TestPendingSendTracking covers the dispatch-time registry the echo
// suppression reads: entries are visible until consumed, unknown ids are not
// reported, the agent transport is a no-op, and stale entries are pruned.
func TestPendingSendTracking(t *testing.T) {
	nc, device := echoTestClient(t)
	nc.trackOutboundSend("send-1")
	nc.trackOutboundSend("send-2")
	if !device.hasPendingSend("send-1") || !device.hasPendingSends() {
		t.Fatal("a dispatched send was not tracked")
	}
	if device.hasPendingSend("") || device.hasPendingSend("send-unknown") {
		t.Fatal("an unknown send id was reported as pending")
	}
	if !device.consumePendingSend("send-1") {
		t.Fatal("pending send was not consumable")
	}
	if device.hasPendingSend("send-1") {
		t.Fatal("a consumed send is still pending")
	}
	if !device.consumePendingSend("send-2") || device.hasPendingSends() {
		t.Fatal("the pending send set was not drained")
	}

	// The agent transport has no pairing device; tracking is a no-op there.
	agentClient := &MyNetworkClient{}
	agentClient.trackOutboundSend("send-3")
	if agentClient.device != nil {
		t.Fatal("an agent-transport client must not track pairing sends")
	}

	// Entries older than the retention window are pruned.
	device.mu.Lock()
	device.pendingSends["stale"] = time.Now().Add(-2 * pairingPendingSendRetention)
	device.mu.Unlock()
	if device.hasPendingSends() {
		t.Fatal("a stale pending send was not pruned")
	}
}

// TestDuplicateEchoSuppressedTwice covers replays: the first echo is matched by
// the send id tracked at dispatch, a duplicate without the correlation by the
// phone row id the first one marked. Later replays are deduplicated by the
// stored message row.
func TestDuplicateEchoSuppressedTwice(t *testing.T) {
	nc, device := echoTestClient(t, echoTestSendID)
	msg := agentapi.Message{ID: echoTestRowID, ThreadID: echoTestThread, SubID: echoTestSub}

	if got := nc.classifyOutboundEcho(msg, echoTestSendID); got != bridgeEchoSuppressed {
		t.Fatalf("first echo classification = %v, want bridgeEchoSuppressed", got)
	}
	if device.hasPendingSend(echoTestSendID) {
		t.Fatal("the echo did not consume the pending send")
	}

	// A duplicate re-push (state snapshot) carries no send id.
	if got := nc.classifyOutboundEcho(msg, ""); got != bridgeEchoSuppressed {
		t.Fatalf("duplicate echo classification = %v, want bridgeEchoSuppressed", got)
	}

	// One more replay is not bridged from the in-memory sets anymore; it is
	// deduplicated by the message row the bridge stored for the Matrix send.
	if got := nc.classifyOutboundEcho(msg, ""); got == bridgeEchoSuppressed {
		t.Fatal("suppression state was not consumed exactly once per correlation")
	}
}

// TestEchoHeldUntilSendResultResolves covers the uncorrelated case with a send
// still in flight: the from-me message without recipients is held, and the
// send.result that arrives shortly after resolves it as an echo.
func TestEchoHeldUntilSendResultResolves(t *testing.T) {
	nc, _ := echoTestClient(t, echoTestSendID)
	nc.echoHoldWait = 2 * time.Second
	nc.echoHoldPollInterval = 5 * time.Millisecond
	ctx := context.Background()
	msg := agentapi.Message{ID: echoTestRowID, ThreadID: echoTestThread, SubID: echoTestSub}

	if got := nc.classifyOutboundEcho(msg, ""); got != bridgeEchoHeld {
		t.Fatalf("classifyOutboundEcho() = %v, want bridgeEchoHeld while a send is unresolved", got)
	}

	done := make(chan bool, 1)
	go func() { done <- nc.waitForEchoResolution(msg, "") }()

	time.Sleep(20 * time.Millisecond)
	nc.handleSendResult(ctx, sendResultEvent(echoTestSendID, echoTestRowID))

	select {
	case matched := <-done:
		if !matched {
			t.Fatal("held echo was not resolved by the send.result")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitForEchoResolution did not finish")
	}
}

// TestFromMeWithoutRecipientsNotHeldWithoutPendingSends pins the bound: a
// phone-originated message (no unresolved send) is bridged immediately, and a
// message that carries recipients is never held either.
func TestFromMeWithoutRecipientsNotHeldWithoutPendingSends(t *testing.T) {
	nc, _ := echoTestClient(t)

	msg := agentapi.Message{ID: echoTestRowID, ThreadID: echoTestThread, SubID: echoTestSub}
	if got := nc.classifyOutboundEcho(msg, ""); got != bridgeEchoNotOurs {
		t.Fatalf("classifyOutboundEcho() = %v, want bridgeEchoNotOurs without pending sends", got)
	}

	nc2, _ := echoTestClient(t, echoTestSendID)
	inbound := agentapi.Message{ID: "sms:2:9", ThreadID: echoTestThread, SubID: echoTestSub, Sender: echoAlice, Recipients: []string{echoBob}}
	if got := nc2.classifyOutboundEcho(inbound, ""); got != bridgeEchoNotOurs {
		t.Fatalf("classifyOutboundEcho(inbound) = %v, want bridgeEchoNotOurs", got)
	}
}

// TestCacheThreadForMessageKeepsKnownRecipients pins the cache guard: the
// payload-less echo must not replace a known participant list with an empty
// one, because the room reconciliation would then clear the members.
func TestCacheThreadForMessageKeepsKnownRecipients(t *testing.T) {
	nc := &MyNetworkClient{}
	nc.cacheThreadForMessage(agentapi.Message{ID: "sms:2:1", ThreadID: echoTestThread, SubID: echoTestSub, Sender: echoAlice})

	// The from-me echo of the bridge's own send carries no recipients.
	nc.cacheThreadForMessage(agentapi.Message{ID: echoTestRowID, ThreadID: echoTestThread, SubID: echoTestSub})

	thread, ok := nc.cachedThread(echoTestThread)
	if !ok {
		t.Fatal("thread was dropped from the cache")
	}
	if !slices.Equal(thread.Recipients, []string{echoAlice}) {
		t.Fatalf("cached recipients = %v, want the known list", thread.Recipients)
	}

	// A thread update repopulates the cache normally.
	nc.rememberThreadRecipients(echoTestThread, echoTestSub, []string{echoAlice, echoBob})
	thread, _ = nc.cachedThread(echoTestThread)
	if !slices.Equal(thread.Recipients, []string{echoAlice, echoBob}) {
		t.Fatalf("cached recipients after update = %v", thread.Recipients)
	}
}

// TestApplyPortalThreadNeverClearsOnEmptyUpdate pins the metadata guard: an
// event without recipients must not clear the stored participants, flags or
// DM/group state.
func TestApplyPortalThreadNeverClearsOnEmptyUpdate(t *testing.T) {
	client := groupTestClient(t, LoginTransportApp, groupTestSerial)
	portal := groupTestPortal("thread-" + DeviceScope(groupTestSerial) + "-2-42")
	if !client.applyPortalThread(portal, agentapi.Thread{
		ID: 42, SubID: 2, Recipients: []string{groupTestAlice, groupTestBob},
	}) {
		t.Fatal("initial applyPortalThread() reported no change")
	}

	if client.applyPortalThread(portal, agentapi.Thread{ID: 42, SubID: 2}) {
		t.Fatal("an empty participant update reported a change")
	}
	meta := portalMetadataOf(portal)
	if !slices.Equal(meta.Recipients, []string{groupTestAlice, groupTestBob}) {
		t.Fatalf("stored recipients were cleared: %v", meta.Recipients)
	}
	if !meta.IsGroup || meta.OtherUserID != "" {
		t.Fatalf("group flags changed on an empty update: %+v", meta)
	}
	if meta.ThreadID != 42 || meta.SubID != 2 {
		t.Fatalf("thread identity changed on an empty update: %+v", meta)
	}
}

// TestWithStoredRecipientsBackfillsEchoThread covers the room-state path: a
// reconciliation triggered by the echo (or a replay) must reuse the portal's
// stored participants instead of producing a full member list that clears the
// room.
func TestWithStoredRecipientsBackfillsEchoThread(t *testing.T) {
	b, bridge := newMergeRoutingTestBridge(t)
	ctx := context.Background()
	key := networkid.PortalKey{ID: "thread-scope-2-2"}
	b.seedPortal(t, key.ID, id.RoomID("!wiped:example.com"), &PortalMetadata{
		ThreadID: 2, SubID: 2,
		Recipients: []string{echoAlice, echoBob},
		IsGroup:    true,
	})

	client := &MyNetworkClient{bridge: bridge, log: zerolog.Nop()}
	thread := client.withStoredRecipients(ctx, key, agentapi.Thread{ID: 2, SubID: 2})
	if !slices.Equal(thread.Recipients, []string{echoAlice, echoBob}) {
		t.Fatalf("backfilled recipients = %v, want the stored list", thread.Recipients)
	}

	// An explicit recipient list always wins over the stored one.
	thread = client.withStoredRecipients(ctx, key, agentapi.Thread{ID: 2, SubID: 2, Recipients: []string{echoBob}})
	if !slices.Equal(thread.Recipients, []string{echoBob}) {
		t.Fatalf("explicit recipients were replaced: %v", thread.Recipients)
	}

	chatInfo := client.chatInfoForThread(ctx, thread)
	if chatInfo.Name == nil || *chatInfo.Name != echoBob {
		t.Fatalf("room name = %v, want the phone-reported participant", chatInfo.Name)
	}
}

// TestEnsurePortalNeverQueuesMembershipClear covers the end of the wipe chain:
// a portal with stored participants must not be reconciled from a thread
// without any, so the sender's metadata and the room state stay intact.
func TestEnsurePortalNeverQueuesMembershipClear(t *testing.T) {
	b, bridge := newMergeRoutingTestBridge(t)
	ctx := context.Background()
	key := networkid.PortalKey{ID: "thread-scope-2-2"}
	b.seedPortal(t, key.ID, id.RoomID("!wiped:example.com"), &PortalMetadata{
		ThreadID: 2, SubID: 2,
		Recipients: []string{echoAlice, echoBob},
		IsGroup:    true,
	})

	client := &MyNetworkClient{bridge: bridge, log: zerolog.Nop()}
	thread := agentapi.Thread{ID: 2, SubID: 2}
	chatInfo := client.chatInfoForThread(ctx, thread)
	client.ensurePortal(ctx, key, chatInfo, thread)

	portal, err := bridge.GetExistingPortalByKey(ctx, key)
	if err != nil || portal == nil {
		t.Fatalf("failed to reload the portal: %v, %v", portal, err)
	}
	meta := portalMetadataOf(portal)
	if !slices.Equal(meta.Recipients, []string{echoAlice, echoBob}) {
		t.Fatalf("stored recipients were cleared: %v", meta.Recipients)
	}
	if !meta.IsGroup {
		t.Fatal("group flag was cleared by an empty participant update")
	}
}
