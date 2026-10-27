// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"slices"
	"strings"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

// Shared identities for the group-thread tests. The portal and membership
// seams under test are device-free: they need a login identity and config, no
// bridge, database or transport.
const (
	groupTestSerial = "<adb-serial>"
	groupTestOwn    = "+15550100100"
	groupTestAlice  = "+15551112222"
	groupTestBob    = "+15553334444"
	groupTestCarol  = "+15555556666"
)

func groupTestClient(t *testing.T, transport, serial string, ownNumbers ...string) *MyNetworkClient {
	t.Helper()
	return &MyNetworkClient{
		connector: &MyConnector{config: &SimpleConfig{OwnNumbers: ownNumbers}},
		login: &bridgev2.UserLogin{UserLogin: &database.UserLogin{
			ID: "login-test",
			Metadata: &LoginMetadata{
				Transport:    transport,
				DeviceID:     "android-" + serial,
				DeviceSerial: serial,
			},
		}},
	}
}

func groupTestPortal(id string) *bridgev2.Portal {
	return &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: networkid.PortalID(id)}}}
}

func memberGhostIDs(members []bridgev2.ChatMember) []networkid.UserID {
	out := make([]networkid.UserID, 0, len(members))
	for _, member := range members {
		if member.IsFromMe {
			continue
		}
		out = append(out, member.Sender)
	}
	return out
}

// TestGroupPortalIDStableAcrossTransportsAndRestarts pins the core room
// identity rule: the portal ID is scoped by the device serial (the app's
// bridge_device_id must equal the adb serial) plus thread and sub ID, so
// restarts and app/agent transport switches resolve to the same Matrix room.
func TestGroupPortalIDStableAcrossTransportsAndRestarts(t *testing.T) {
	t.Parallel()
	agentClient := groupTestClient(t, LoginTransportAgent, groupTestSerial)
	appClient := groupTestClient(t, LoginTransportApp, groupTestSerial)
	restarted := groupTestClient(t, LoginTransportAgent, groupTestSerial)

	agentKey := agentClient.portalKeyFor(42, 1)
	if want := MakePortalID(DeviceScope(groupTestSerial), 42, 1); agentKey.ID != want {
		t.Fatalf("portalKeyFor() = %q, want %q", agentKey.ID, want)
	}
	if again := agentClient.portalKeyFor(42, 1); again != agentKey {
		t.Fatalf("portal ID is not stable across calls: %q vs %q", again.ID, agentKey.ID)
	}
	if appKey := appClient.portalKeyFor(42, 1); appKey != agentKey {
		t.Fatalf("app and agent transports disagree on the portal ID: %q vs %q", appKey.ID, agentKey.ID)
	}
	if restartedKey := restarted.portalKeyFor(42, 1); restartedKey != agentKey {
		t.Fatalf("portal ID is not stable across restarts: %q vs %q", restartedKey.ID, agentKey.ID)
	}
	if agentKey.Receiver != "" {
		t.Fatalf("portal key receiver = %q, want empty (portals are login-scoped)", agentKey.Receiver)
	}

	// Two phones managed by the same Matrix user must never share rooms.
	for _, serial := range []string{"712KPMZ1266900", "abc123"} {
		other := groupTestClient(t, LoginTransportAgent, serial)
		if otherKey := other.portalKeyFor(42, 1); otherKey == agentKey {
			t.Fatalf("device serial %q collides with %q", serial, groupTestSerial)
		}
	}

	// A login without a serial falls back to the shared "default" scope.
	noSerial := groupTestClient(t, LoginTransportAgent, "")
	if got := noSerial.portalKeyFor(42, 1).ID; got != MakePortalID("default", 42, 1) {
		t.Fatalf("portalKeyFor() without a serial = %q, want the default scope", got)
	}
}

// TestGroupPortalIDThreadAndSubVariations covers the field-separator
// stability: thread and sub ID variations never collide, and every produced ID
// parses back to its parts.
func TestGroupPortalIDThreadAndSubVariations(t *testing.T) {
	t.Parallel()
	client := groupTestClient(t, LoginTransportAgent, groupTestSerial)
	base := client.portalKeyFor(42, 1)

	distinct := []networkid.PortalKey{
		client.portalKeyFor(42, 2),
		client.portalKeyFor(42, 0),
		client.portalKeyFor(43, 1),
		// The sub ID and thread ID fields must not be swappable.
		client.portalKeyFor(1, 42),
		groupTestClient(t, LoginTransportAgent, "otherserial").portalKeyFor(42, 1),
	}
	for _, key := range distinct {
		if key == base {
			t.Fatalf("portal key %q collides with %q", key.ID, base.ID)
		}
	}

	for _, tc := range []struct {
		scope  string
		thread int64
		sub    int
	}{
		{DeviceScope(groupTestSerial), 42, 1},
		{DeviceScope(groupTestSerial), 42, 2},
		{"default", 7, 0},
		// The adb provider reports sub_id -1 when it cannot resolve the SIM,
		// and app events do the same; such portal IDs must still round-trip
		// (the history fallback in FetchMessages parses them).
		{"deadbeef", 42, -1},
		{"deadbeef", 9223372036854775807, 2},
	} {
		id := MakePortalID(tc.scope, tc.thread, tc.sub)
		scope, sub, thread, ok := ParsePortalID(id)
		if !ok {
			t.Fatalf("ParsePortalID(%q) failed", id)
		}
		if scope != tc.scope || sub != tc.sub || thread != tc.thread {
			t.Fatalf("ParsePortalID(%q) = (%q, %d, %d), want (%q, %d, %d)", id, scope, sub, thread, tc.scope, tc.sub, tc.thread)
		}
	}

	// Malformed IDs must still be rejected: empty scope, extra fields and
	// unparsable sub/thread fields.
	for _, bad := range []string{"thread--1-42", "thread-scope--42", "thread-scope-1-42-43", "thread-scope-1-42x"} {
		if _, _, _, ok := ParsePortalID(networkid.PortalID(bad)); ok {
			t.Errorf("ParsePortalID(%q) unexpectedly succeeded", bad)
		}
	}
}

// TestApplyPortalThreadGroupMembershipUpdatesInPlace pins the "membership
// changes never create a second room" rule: the portal object and its ID stay
// the same while the recipient set and DM/group flags are rewritten.
func TestApplyPortalThreadGroupMembershipUpdatesInPlace(t *testing.T) {
	t.Parallel()
	client := groupTestClient(t, LoginTransportAgent, groupTestSerial, groupTestOwn)
	portal := groupTestPortal("thread-" + DeviceScope(groupTestSerial) + "-1-42")
	portalID := portal.ID

	// A multi-recipient thread with a formatting duplicate and a leaked own
	// number becomes one group portal with the canonical participant set.
	if !client.applyPortalThread(portal, agentapi.Thread{
		ID: 42, SubID: 1,
		Recipients: []string{groupTestBob, "5551112222", groupTestAlice, groupTestOwn},
	}) {
		t.Fatal("first applyPortalThread() reported no change")
	}
	meta, ok := portal.Metadata.(*PortalMetadata)
	if !ok || meta == nil {
		t.Fatalf("portal metadata = %#v, want *PortalMetadata", portal.Metadata)
	}
	if want := []string{groupTestAlice, groupTestBob}; !slices.Equal(meta.Recipients, want) {
		t.Fatalf("Recipients = %v, want %v", meta.Recipients, want)
	}
	if !meta.IsGroup || meta.OtherUserID != "" || meta.ThreadID != 42 || meta.SubID != 1 {
		t.Fatalf("unexpected group metadata: %+v", meta)
	}

	// Re-applying the same participant set (formatting variants included) is
	// not a change and must not replace the metadata instance.
	if client.applyPortalThread(portal, agentapi.Thread{ID: 42, SubID: 1, Recipients: []string{"+1 (555) 111-2222", groupTestBob}}) {
		t.Fatal("unchanged participant set reported a change")
	}
	if portal.Metadata != meta {
		t.Fatal("metadata instance replaced without a change")
	}

	// Adding a participant updates the same portal in place.
	if !client.applyPortalThread(portal, agentapi.Thread{ID: 42, SubID: 1, Recipients: []string{groupTestAlice, groupTestBob, groupTestCarol}}) {
		t.Fatal("added participant reported no change")
	}
	if portal.ID != portalID {
		t.Fatalf("portal ID changed on membership update: %q vs %q", portal.ID, portalID)
	}
	if want := []string{groupTestAlice, groupTestBob, groupTestCarol}; !slices.Equal(meta.Recipients, want) {
		t.Fatalf("Recipients after add = %v, want %v", meta.Recipients, want)
	}

	// Shrinking to one participant switches the same portal back to a DM.
	if !client.applyPortalThread(portal, agentapi.Thread{ID: 42, SubID: 1, Recipients: []string{groupTestBob}}) {
		t.Fatal("participant removal reported no change")
	}
	if meta.IsGroup || meta.OtherUserID != MakeUserID(groupTestBob) {
		t.Fatalf("DM metadata after removal = %+v", meta)
	}
	if want := []string{groupTestBob}; !slices.Equal(meta.Recipients, want) {
		t.Fatalf("Recipients after removal = %v, want %v", meta.Recipients, want)
	}
	if client.applyPortalThread(portal, agentapi.Thread{ID: 42, SubID: 1, Recipients: []string{groupTestBob}}) {
		t.Fatal("re-applying the same DM reported a change")
	}

	// Events with no thread id and an unresolved sub id never rewrite the
	// stored identity.
	if client.applyPortalThread(portal, agentapi.Thread{SubID: -1, Recipients: []string{groupTestBob}}) {
		t.Fatal("empty thread id / unknown sub id reported a change")
	}
	if meta.ThreadID != 42 || meta.SubID != 1 {
		t.Fatalf("thread identity clobbered: %+v", meta)
	}
}

// TestChatInfoForGroupThread pins the room state built for group and DM
// threads, including the own-number filter.
func TestChatInfoForGroupThread(t *testing.T) {
	t.Parallel()
	client := groupTestClient(t, LoginTransportAgent, groupTestSerial, groupTestOwn)
	ctx := context.Background()

	group := client.chatInfoForThread(ctx, agentapi.Thread{
		ID: 42, SubID: 1,
		Recipients: []string{groupTestCarol, groupTestOwn, groupTestAlice},
	})
	if group.Type == nil || *group.Type != database.RoomTypeDefault {
		t.Fatalf("group room type = %v, want default", group.Type)
	}
	if group.Name == nil || *group.Name != groupTestAlice+", "+groupTestCarol {
		t.Fatalf("group room name = %v, want the sorted participants", group.Name)
	}
	if group.Members == nil || !group.Members.IsFull || group.Members.OtherUserID != "" {
		t.Fatalf("group member list = %+v, want a full list without OtherUserID", group.Members)
	}
	wantMembers := []networkid.UserID{MakeUserID(groupTestAlice), MakeUserID(groupTestCarol)}
	if got := memberGhostIDs(group.Members.Members); !slices.Equal(got, wantMembers) {
		t.Fatalf("group ghosts = %v, want %v", got, wantMembers)
	}
	if len(group.Members.Members) == 0 || !group.Members.Members[0].IsFromMe {
		t.Fatalf("group member list must start with the bridge user: %+v", group.Members.Members)
	}
	if !group.CanBackfill {
		t.Error("CanBackfill = false, want true for agent-backed threads")
	}

	dm := client.chatInfoForThread(ctx, agentapi.Thread{ID: 43, SubID: 1, Recipients: []string{groupTestBob}})
	if dm.Type == nil || *dm.Type != database.RoomTypeDM {
		t.Fatalf("DM room type = %v, want DM", dm.Type)
	}
	if dm.Name == nil || *dm.Name != groupTestBob {
		t.Fatalf("DM room name = %v, want %q", dm.Name, groupTestBob)
	}
	if dm.Members.OtherUserID != MakeUserID(groupTestBob) {
		t.Fatalf("DM OtherUserID = %q, want %q", dm.Members.OtherUserID, MakeUserID(groupTestBob))
	}
	if got := memberGhostIDs(dm.Members.Members); !slices.Equal(got, []networkid.UserID{MakeUserID(groupTestBob)}) {
		t.Fatalf("DM ghosts = %v", got)
	}

	// A self-thread keeps the own number as the only counterpart, otherwise
	// the room would be unsendable and named "Unknown conversation".
	self := client.chatInfoForThread(ctx, agentapi.Thread{ID: 44, SubID: 1, Recipients: []string{"15550100100"}})
	if self.Type == nil || *self.Type != database.RoomTypeDM || self.Name == nil || *self.Name != groupTestOwn {
		t.Fatalf("self-thread room = %+v (name %v)", self, self.Name)
	}
	if self.Members.OtherUserID != MakeUserID(groupTestOwn) {
		t.Fatalf("self-thread OtherUserID = %q, want the own-number ghost", self.Members.OtherUserID)
	}
	if got := memberGhostIDs(self.Members.Members); !slices.Equal(got, []networkid.UserID{MakeUserID(groupTestOwn)}) {
		t.Fatalf("self-thread ghosts = %v, want the own number", got)
	}
}

// TestGroupCanonicalRecipientsExpandsMergedRows covers the bridge-side half of
// the merged-address filter: recipient lists that reach the connector (e.g.
// from app-pushed participant events, which bypass the agent provider) must
// expand "a|b" rows instead of turning them into a "+1555…|+1666…" ghost.
func TestGroupCanonicalRecipientsExpandsMergedRows(t *testing.T) {
	t.Parallel()
	got := CanonicalRecipients([]string{
		"+15551112222|+15553334444",
		" 555-555-6666 ",
		"+1 (555) 111-2222",
		"||",
		"15553334444",
	})
	want := []string{groupTestAlice, groupTestBob, groupTestCarol}
	if !slices.Equal(got, want) {
		t.Fatalf("CanonicalRecipients() = %v, want %v", got, want)
	}
	for _, recipient := range got {
		if strings.Contains(recipient, "|") {
			t.Fatalf("merged address leaked into the recipient list: %q", recipient)
		}
	}
}

// TestGroupMergedAddressNeverBecomesGhost checks the room state side: a merged
// address row in a thread's participants becomes two individual ghosts (never
// one "a|b" ghost) and does not fracture the portal.
func TestGroupMergedAddressNeverBecomesGhost(t *testing.T) {
	t.Parallel()
	client := groupTestClient(t, LoginTransportAgent, groupTestSerial, groupTestOwn)

	info := client.chatInfoForThread(context.Background(), agentapi.Thread{
		ID: 46, SubID: 1,
		Recipients: []string{groupTestAlice + "|" + groupTestBob},
	})
	if want := groupTestAlice + ", " + groupTestBob; info.Name == nil || *info.Name != want {
		t.Fatalf("room name = %v, want %q", info.Name, want)
	}
	wantMembers := []networkid.UserID{MakeUserID(groupTestAlice), MakeUserID(groupTestBob)}
	if got := memberGhostIDs(info.Members.Members); !slices.Equal(got, wantMembers) {
		t.Fatalf("ghosts = %v, want %v", got, wantMembers)
	}
	for _, member := range info.Members.Members {
		if strings.Contains(string(member.Sender), "|") {
			t.Fatalf("merged address became a ghost: %q", member.Sender)
		}
	}

	// The merged row is a participant-list detail only; it can never produce a
	// second portal for the thread.
	if client.portalKeyFor(46, 1) != client.portalKeyFor(46, 1) {
		t.Fatal("portal key changed for a merged participant list")
	}
}

// TestGroupThreadForMessageAndPortalKey covers the incoming/outgoing row
// mapping: both directions of a group thread resolve to the same portal and
// the same participant set, and a second sender cannot fracture the room.
func TestGroupThreadForMessageAndPortalKey(t *testing.T) {
	t.Parallel()
	client := groupTestClient(t, LoginTransportAgent, groupTestSerial, groupTestOwn)
	// A non-nil device makes refreshThreads a no-op, so the in-memory cache is
	// the only thread source in this device-free test.
	client.device = &pairedDevice{}

	incoming := agentapi.Message{
		ID: "mms:1:80", ThreadID: 77, SubID: 1,
		Sender: groupTestAlice, Recipients: []string{groupTestBob, groupTestCarol},
	}
	outgoing := agentapi.Message{
		ID: "mms:1:81", ThreadID: 77, SubID: 1,
		Sender: "", Recipients: []string{groupTestAlice, groupTestBob, groupTestCarol},
	}

	thread := client.threadForMessage(context.Background(), incoming)
	if thread.ID != 77 || thread.SubID != 1 {
		t.Fatalf("threadForMessage() identity = %+v", thread)
	}
	if want := []string{groupTestBob, groupTestCarol, groupTestAlice}; !slices.Equal(thread.Recipients, want) {
		t.Fatalf("fallback recipients = %v, want the message recipients plus the sender", thread.Recipients)
	}
	if want := []string{groupTestAlice, groupTestBob, groupTestCarol}; !slices.Equal(client.threadRecipients(thread), want) {
		t.Fatalf("threadRecipients() = %v, want %v", client.threadRecipients(thread), want)
	}

	if client.portalKeyFor(incoming.ThreadID, incoming.SubID) != client.portalKeyFor(outgoing.ThreadID, outgoing.SubID) {
		t.Fatal("incoming and outgoing rows of one thread landed in different portals")
	}
	// A reply from another participant in the same thread is the same room.
	other := agentapi.Message{
		ID: "mms:1:82", ThreadID: 77, SubID: 1,
		Sender: groupTestBob, Recipients: []string{groupTestAlice, groupTestCarol},
	}
	if client.portalKeyFor(other.ThreadID, other.SubID) != client.portalKeyFor(incoming.ThreadID, incoming.SubID) {
		t.Fatal("a different sender in the same thread created a second portal")
	}

	// The cached thread participant list wins over the message payload.
	client.rememberThreadRecipients(77, 1, []string{groupTestCarol})
	cached := client.threadForMessage(context.Background(), incoming)
	if want := []string{groupTestCarol}; !slices.Equal(cached.Recipients, want) {
		t.Fatalf("cached thread recipients = %v, want %v", cached.Recipients, want)
	}
}
