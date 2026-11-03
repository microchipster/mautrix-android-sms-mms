// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

const (
	mergeTestTargetRoom = "!target:example.com"
	mergeTestSourceRoom = "!source:example.com"
	mergeTestThirdRoom  = "!third:example.com"

	mergeAlice = "+15551110001"
	mergeBob   = "+15551110002"
	mergeCarol = "+15551110003"
	mergeDave  = "+15551110004"
)

// mergeTestRoom builds a resolved mergeRoom with a portal ID derived from the
// room ID, so room and portal identity stay aligned in the pure tests.
func mergeTestRoom(roomID string, threadID int64, recipients ...string) mergeRoom {
	return mergeRoom{
		Input:      roomID,
		RoomID:     id.RoomID(roomID),
		PortalID:   networkid.PortalID("portal-" + strings.TrimPrefix(roomID, "!")),
		Name:       roomID,
		Recipients: CanonicalRecipients(recipients),
		ThreadID:   threadID,
	}
}

func TestPlanMergeValidationMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		rooms            []mergeRoom
		target           int
		wantErr          string
		wantSources      int
		wantAlready      int
		wantConfirmation string
		wantDisconnected bool
	}{
		{
			name:    "single room",
			rooms:   []mergeRoom{mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice)},
			target:  0,
			wantErr: "at least two",
		},
		{
			name: "management room",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				func() mergeRoom {
					room := mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice)
					room.IsManagementRoom = true
					return room
				}(),
			},
			target:  0,
			wantErr: "management room",
		},
		{
			name: "space",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				func() mergeRoom {
					room := mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice)
					room.IsSpace = true
					return room
				}(),
			},
			target:  0,
			wantErr: "is a space",
		},
		{
			name: "not a portal",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				func() mergeRoom {
					room := mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice)
					room.PortalID = ""
					return room
				}(),
			},
			target:  0,
			wantErr: "not a bridge portal",
		},
		{
			name: "portal without room",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				func() mergeRoom {
					room := mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice)
					room.RoomID = ""
					return room
				}(),
			},
			target:  0,
			wantErr: "has no Matrix room yet",
		},
		{
			name: "duplicate room",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
			},
			target:  0,
			wantErr: "selected twice",
		},
		{
			name: "target merged elsewhere",
			rooms: []mergeRoom{
				func() mergeRoom {
					room := mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice)
					room.MergedInto = "portal-elsewhere:example.com"
					return room
				}(),
				mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice),
			},
			target:  0,
			wantErr: "itself merged into",
		},
		{
			name: "target without thread",
			rooms: []mergeRoom{
				func() mergeRoom {
					room := mergeTestRoom(mergeTestTargetRoom, 0, mergeAlice)
					return room
				}(),
				mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice),
			},
			target:  0,
			wantErr: "not linked to a phone thread",
		},
		{
			name: "two identical DMs need no confirmation",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice),
			},
			target:      0,
			wantSources: 1,
		},
		{
			name: "source already merged into target is a no-op",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				func() mergeRoom {
					room := mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice)
					room.MergedInto = networkid.PortalID("portal-target:example.com")
					return room
				}(),
			},
			target:      0,
			wantSources: 0,
			wantAlready: 1,
		},
		{
			name: "re-pointing an already merged source needs confirmation",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				func() mergeRoom {
					room := mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice)
					room.MergedInto = "portal-elsewhere:example.com"
					return room
				}(),
			},
			target:           0,
			wantSources:      1,
			wantConfirmation: "--yes re-points",
		},
		{
			name: "source that is itself a target needs confirmation",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				func() mergeRoom {
					room := mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice)
					room.MergedFromCount = 2
					return room
				}(),
			},
			target:           0,
			wantSources:      1,
			wantConfirmation: "itself a merge target",
		},
		{
			name: "three rooms need confirmation",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice),
				mergeTestRoom(mergeTestThirdRoom, 3, mergeAlice),
			},
			target:           0,
			wantSources:      2,
			wantConfirmation: "merging 3 rooms",
		},
		{
			name: "different participant sets need confirmation",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice, mergeBob),
			},
			target:           0,
			wantSources:      1,
			wantConfirmation: "different participant sets",
		},
		{
			name: "disjoint rooms are refused without force",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				mergeTestRoom(mergeTestSourceRoom, 2, mergeBob),
			},
			target:           0,
			wantSources:      1,
			wantConfirmation: "do not all share a participant",
			wantDisconnected: true,
		},
		{
			name: "chained participants connect every room",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice, mergeBob),
				mergeTestRoom(mergeTestSourceRoom, 2, mergeBob, mergeCarol),
				mergeTestRoom(mergeTestThirdRoom, 3, mergeCarol, mergeDave),
			},
			target:           0,
			wantSources:      2,
			wantConfirmation: "merging 3 rooms",
		},
		{
			name: "two disjoint pairs stay disconnected",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice, mergeBob),
				mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice, mergeBob),
				mergeTestRoom(mergeTestThirdRoom, 3, mergeCarol, mergeDave),
				mergeTestRoom("!fourth:example.com", 4, mergeCarol, mergeDave),
			},
			target:           0,
			wantSources:      3,
			wantConfirmation: "do not all share a participant",
			wantDisconnected: true,
		},
		{
			name: "--into picks the second room as target",
			rooms: []mergeRoom{
				mergeTestRoom(mergeTestTargetRoom, 1, mergeAlice),
				mergeTestRoom(mergeTestSourceRoom, 2, mergeAlice),
			},
			target:      1,
			wantSources: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := planMerge(tc.rooms, tc.target)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("planMerge() error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("planMerge() unexpected error: %v", err)
			}
			if len(plan.Sources) != tc.wantSources {
				t.Fatalf("Sources = %v, want %d entries", plan.Sources, tc.wantSources)
			}
			if len(plan.AlreadyMerged) != tc.wantAlready {
				t.Fatalf("AlreadyMerged = %v, want %d entries", plan.AlreadyMerged, tc.wantAlready)
			}
			if plan.Disconnected != tc.wantDisconnected {
				t.Fatalf("Disconnected = %t, want %t", plan.Disconnected, tc.wantDisconnected)
			}
			if tc.wantConfirmation != "" {
				if !strings.Contains(strings.Join(plan.ConfirmationReasons, "; "), tc.wantConfirmation) {
					t.Fatalf("ConfirmationReasons = %v, want containing %q", plan.ConfirmationReasons, tc.wantConfirmation)
				}
				if !plan.RequiresConfirmation() {
					t.Fatal("RequiresConfirmation() = false with reasons present")
				}
			} else if plan.RequiresConfirmation() {
				t.Fatalf("RequiresConfirmation() = true without reasons: %v", plan.ConfirmationReasons)
			}
			wantSets := make([][]string, 0, len(tc.rooms))
			for _, room := range tc.rooms {
				wantSets = append(wantSets, room.Recipients)
			}
			wantUnion := mergeRecipientUnion(wantSets...)
			if !slices.Equal(plan.Participants, wantUnion) {
				t.Fatalf("Participants = %v, want %v", plan.Participants, wantUnion)
			}
			for _, index := range plan.Sources {
				if index == plan.Target {
					t.Fatalf("target %d listed as a source", plan.Target)
				}
			}
		})
	}
}

func TestPlanMergeUnionParticipants(t *testing.T) {
	t.Parallel()
	plan, err := planMerge([]mergeRoom{
		mergeTestRoom(mergeTestTargetRoom, 1, mergeCarol, mergeAlice),
		mergeTestRoom(mergeTestSourceRoom, 2, "1 (555) 111-0002"),
	}, 0)
	if err != nil {
		t.Fatalf("planMerge() error: %v", err)
	}
	want := []string{mergeAlice, mergeBob, mergeCarol}
	if !slices.Equal(plan.Participants, want) {
		t.Fatalf("Participants = %v, want %v", plan.Participants, want)
	}
}

func TestResolveMergeTargetRouting(t *testing.T) {
	t.Parallel()
	registry := map[networkid.PortalID]*PortalMetadata{
		"source": {MergedInto: "target"},
		"target": {MergedFrom: []networkid.PortalID{"source"}},
	}
	lookup := func(portalID networkid.PortalID) *PortalMetadata {
		return registry[portalID]
	}

	// Source routes to the target; the target is unaffected.
	got, merged := resolveMergeTarget("source", lookup)
	if !merged || got != "target" {
		t.Fatalf("resolveMergeTarget(source) = (%q, %t), want (target, true)", got, merged)
	}
	if _, merged := resolveMergeTarget("target", lookup); merged {
		t.Fatal("resolveMergeTarget(target) reported a merge, target must stay in place")
	}

	// Unmerge restores the source's own routing.
	registry["source"].MergedInto = ""
	registry["target"].MergedFrom = nil
	if _, merged := resolveMergeTarget("source", lookup); merged {
		t.Fatal("resolveMergeTarget() still merges after unmerge")
	}

	// Chains resolve to the final target.
	registry["source"].MergedInto = "middle"
	registry["middle"] = &PortalMetadata{MergedInto: "target"}
	if got, merged := resolveMergeTarget("source", lookup); !merged || got != "target" {
		t.Fatalf("chain resolution = (%q, %t), want (target, true)", got, merged)
	}

	// Broken registries never route and never loop.
	registry["cycle-a"] = &PortalMetadata{MergedInto: "cycle-b"}
	registry["cycle-b"] = &PortalMetadata{MergedInto: "cycle-a"}
	registry["dangling"] = &PortalMetadata{MergedInto: "missing"}
	registry["self"] = &PortalMetadata{MergedInto: "self"}
	for _, start := range []networkid.PortalID{"cycle-a", "cycle-b", "dangling", "self", "unknown", ""} {
		if _, merged := resolveMergeTarget(start, lookup); merged {
			t.Fatalf("resolveMergeTarget(%q) routed through a broken registry", start)
		}
	}
}

func TestMergeThreadRoutingHelpers(t *testing.T) {
	t.Parallel()
	target := &PortalMetadata{
		ThreadID:   77,
		SubID:      2,
		Recipients: CanonicalRecipients([]string{mergeAlice, mergeBob}),
		MergedFrom: []networkid.PortalID{"source"},
	}

	// A merged-away thread keeps its sender but takes the target identity and
	// the participant union.
	routed := mergeRoutedThread(target, agentapi.Thread{
		ID: 12, SubID: 1,
		Recipients: []string{mergeBob, mergeCarol},
	})
	if routed.ID != 77 || routed.SubID != 2 {
		t.Fatalf("routed thread identity = (%d, %d), want (77, 2)", routed.ID, routed.SubID)
	}
	if want := []string{mergeAlice, mergeBob, mergeCarol}; !slices.Equal(routed.Recipients, want) {
		t.Fatalf("routed recipients = %v, want %v", routed.Recipients, want)
	}

	// A target's own thread update accumulates participants, it never shrinks
	// the union back to that thread's list.
	accumulated := mergeAccumulateRecipients(target, agentapi.Thread{
		ID: 77, SubID: 2,
		Recipients: []string{mergeAlice},
	})
	if want := []string{mergeAlice, mergeBob}; !slices.Equal(accumulated.Recipients, want) {
		t.Fatalf("accumulated recipients = %v, want %v", accumulated.Recipients, want)
	}

	// Plain portals keep replace semantics.
	plain := &PortalMetadata{ThreadID: 5, Recipients: CanonicalRecipients([]string{mergeAlice})}
	updated := mergeAccumulateRecipients(plain, agentapi.Thread{ID: 5, Recipients: []string{mergeCarol}})
	if want := []string{mergeCarol}; !slices.Equal(updated.Recipients, want) {
		t.Fatalf("plain portal recipients = %v, want %v", updated.Recipients, want)
	}

	// A thread with no participants keeps the stored set for a target.
	empty := mergeAccumulateRecipients(target, agentapi.Thread{ID: 77})
	if want := []string{mergeAlice, mergeBob}; !slices.Equal(empty.Recipients, want) {
		t.Fatalf("empty thread recipients = %v, want %v", empty.Recipients, want)
	}

	// Missing metadata is a no-op.
	if got := mergeRoutedThread(nil, agentapi.Thread{ID: 3}); got.ID != 3 {
		t.Fatalf("mergeRoutedThread(nil) changed the thread: %+v", got)
	}
}

func TestMergeNameAndTopicRendering(t *testing.T) {
	t.Parallel()
	displayName := func(address string) string {
		if address == mergeAlice {
			return "Alice"
		}
		return ""
	}

	if got := mergeTargetName([]string{mergeAlice}, displayName, ""); got != "Alice" {
		t.Fatalf("single-participant name = %q, want the contact name", got)
	}
	if got := mergeTargetName([]string{mergeBob}, displayName, ""); got != mergeBob {
		t.Fatalf("single-participant name = %q, want the address fallback", got)
	}
	if got := mergeTargetName([]string{mergeAlice, mergeBob}, displayName, ""); got != "Alice, "+mergeBob {
		t.Fatalf("group name = %q, want the label list (contact names where known)", got)
	}
	if got := mergeTargetName(nil, displayName, ""); got != "Unknown conversation" {
		t.Fatalf("empty name = %q", got)
	}

	many := []string{"+15551110001", "+15551110002", "+15551110003", "+15551110004", "+15551110005", "+15551110006"}
	if got := mergeTargetName(many, nil, ""); !strings.HasSuffix(got, "and 1 more") {
		t.Fatalf("long group name = %q, want the collapsed suffix", got)
	}

	if got := mergeTargetTopic(3, []string{mergeAlice, mergeBob}); got != "Merged phone conversation (3 phone threads): "+mergeAlice+", "+mergeBob {
		t.Fatalf("topic = %q", got)
	}
	if got := mergeTargetTopic(2, nil); got != "Merged phone conversation (2 phone threads)" {
		t.Fatalf("empty topic = %q", got)
	}
}

func TestMergeMetadataPersistenceAgainstDatabase(t *testing.T) {
	b := newTestBridgeDB(t)
	ctx := context.Background()
	targetRow := b.seedPortal(t, "thread-scope-1-1", id.RoomID(mergeTestTargetRoom), &PortalMetadata{
		ThreadID: 1, SubID: 1, Recipients: []string{mergeAlice},
	})
	sourceRow := b.seedPortal(t, "thread-scope-1-2", id.RoomID(mergeTestSourceRoom), &PortalMetadata{
		ThreadID: 2, SubID: 1, Recipients: []string{mergeBob},
	})

	targetPortal := &bridgev2.Portal{Portal: targetRow, Bridge: b.bridge}
	sourcePortal := &bridgev2.Portal{Portal: sourceRow, Bridge: b.bridge}
	rooms := []mergeRoom{
		mergeRoomFromPortalRow(targetRow, mergeRoom{Input: "!target"}),
		mergeRoomFromPortalRow(sourceRow, mergeRoom{Input: "!source"}),
	}
	rooms[0].Portal = targetPortal
	rooms[1].Portal = sourcePortal

	plan, err := planMerge(rooms, 0)
	if err != nil {
		t.Fatalf("planMerge() error: %v", err)
	}
	if err := applyMergePlanMetadata(plan); err != nil {
		t.Fatalf("applyMergePlanMetadata() error: %v", err)
	}
	if err := targetPortal.Save(ctx); err != nil {
		t.Fatalf("save target portal: %v", err)
	}
	if err := sourcePortal.Save(ctx); err != nil {
		t.Fatalf("save source portal: %v", err)
	}

	reloadedTarget, err := b.db.Portal.GetByKey(ctx, networkid.PortalKey{ID: "thread-scope-1-1"})
	if err != nil || reloadedTarget == nil {
		t.Fatalf("reload target: %v, %v", reloadedTarget, err)
	}
	targetMeta, _ := reloadedTarget.Metadata.(*PortalMetadata)
	if targetMeta == nil {
		t.Fatal("target metadata missing after reload")
	}
	if want := []string{mergeAlice, mergeBob}; !slices.Equal(targetMeta.Recipients, want) {
		t.Fatalf("target Recipients = %v, want %v", targetMeta.Recipients, want)
	}
	if !targetMeta.IsGroup || targetMeta.OtherUserID != "" {
		t.Fatalf("target group metadata = %+v", targetMeta)
	}
	if want := []networkid.PortalID{"thread-scope-1-2"}; !slices.Equal(targetMeta.MergedFrom, want) {
		t.Fatalf("target MergedFrom = %v, want %v", targetMeta.MergedFrom, want)
	}

	reloadedSource, err := b.db.Portal.GetByKey(ctx, networkid.PortalKey{ID: "thread-scope-1-2"})
	if err != nil || reloadedSource == nil {
		t.Fatalf("reload source: %v, %v", reloadedSource, err)
	}
	sourceMeta, _ := reloadedSource.Metadata.(*PortalMetadata)
	if sourceMeta == nil || sourceMeta.MergedInto != "thread-scope-1-1" {
		t.Fatalf("source MergedInto = %+v, want thread-scope-1-1", sourceMeta)
	}

	// The registry listing reads the same persisted state.
	entries, err := listActiveMerges(ctx, b.bridge)
	if err != nil {
		t.Fatalf("listActiveMerges() error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("listActiveMerges() = %+v, want one entry", entries)
	}
	if entries[0].Source.PortalID != "thread-scope-1-2" || entries[0].Target.PortalID != "thread-scope-1-1" {
		t.Fatalf("merge entry = %+v", entries[0])
	}
	rendered := formatMergeEntries(entries)
	if !strings.Contains(rendered, mergeTestSourceRoom) || !strings.Contains(rendered, mergeTestTargetRoom) {
		t.Fatalf("formatMergeEntries() = %q, want both room IDs", rendered)
	}
	if got := formatMergeEntries(nil); !strings.Contains(got, "No active merges") {
		t.Fatalf("formatMergeEntries(nil) = %q", got)
	}

	// Unmerge: clear the source pointer and drop it from the target list.
	sourceMeta.MergedInto = ""
	if err := b.db.Portal.Update(ctx, reloadedSource); err != nil {
		t.Fatalf("save unmerged source: %v", err)
	}
	if err := removeMergedSource(ctx, b.bridge, "thread-scope-1-1", "thread-scope-1-2"); err != nil {
		t.Fatalf("removeMergedSource() error: %v", err)
	}
	afterUnmerge, err := b.db.Portal.GetByKey(ctx, networkid.PortalKey{ID: "thread-scope-1-1"})
	if err != nil || afterUnmerge == nil {
		t.Fatalf("reload target after unmerge: %v, %v", afterUnmerge, err)
	}
	afterMeta, _ := afterUnmerge.Metadata.(*PortalMetadata)
	if afterMeta == nil || len(afterMeta.MergedFrom) != 0 {
		t.Fatalf("target MergedFrom after unmerge = %+v, want empty", afterMeta)
	}
	entries, err = listActiveMerges(ctx, b.bridge)
	if err != nil {
		t.Fatalf("listActiveMerges() after unmerge error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("listActiveMerges() after unmerge = %+v, want none", entries)
	}
}

// stubMatrixConnector is a no-op bridgev2.MatrixConnector for tests that need a
// real bridge object (portal cache, database) without a homeserver.
type stubMatrixConnector struct{}

func (s *stubMatrixConnector) Init(*bridgev2.Bridge)         {}
func (s *stubMatrixConnector) Start(context.Context) error   { return nil }
func (s *stubMatrixConnector) PreStop()                      {}
func (s *stubMatrixConnector) Stop()                         {}
func (s *stubMatrixConnector) BotIntent() bridgev2.MatrixAPI { return nil }
func (s *stubMatrixConnector) SendBridgeStatus(context.Context, *status.BridgeState) error {
	return nil
}
func (s *stubMatrixConnector) SendMessageStatus(context.Context, *bridgev2.MessageStatus, *bridgev2.MessageStatusEventInfo) {
}
func (s *stubMatrixConnector) GetCapabilities() *bridgev2.MatrixCapabilities {
	return &bridgev2.MatrixCapabilities{}
}
func (s *stubMatrixConnector) ParseGhostMXID(id.UserID) (networkid.UserID, bool) {
	return "", false
}
func (s *stubMatrixConnector) FormatGhostMXID(networkid.UserID) id.UserID { return "" }
func (s *stubMatrixConnector) GhostIntent(networkid.UserID) bridgev2.MatrixAPI {
	return nil
}
func (s *stubMatrixConnector) NewUserIntent(context.Context, id.UserID, string) (bridgev2.MatrixAPI, string, error) {
	return nil, "", nil
}
func (s *stubMatrixConnector) GenerateContentURI(context.Context, networkid.MediaID) (id.ContentURIString, error) {
	return "", nil
}
func (s *stubMatrixConnector) ParseContentURI(context.Context, id.ContentURIString) (networkid.MediaID, error) {
	return nil, nil
}
func (s *stubMatrixConnector) GetPowerLevels(context.Context, id.RoomID) (*event.PowerLevelsEventContent, error) {
	return nil, nil
}
func (s *stubMatrixConnector) GetMembers(context.Context, id.RoomID) (map[id.UserID]*event.MemberEventContent, error) {
	return nil, nil
}
func (s *stubMatrixConnector) GetMemberInfo(context.Context, id.RoomID, id.UserID) (*event.MemberEventContent, error) {
	return nil, nil
}
func (s *stubMatrixConnector) BatchSend(context.Context, id.RoomID, *mautrix.ReqBeeperBatchSend, []*bridgev2.MatrixSendExtra) (*mautrix.RespBeeperBatchSend, error) {
	return nil, nil
}
func (s *stubMatrixConnector) GenerateDeterministicRoomID(networkid.PortalKey) id.RoomID {
	return ""
}
func (s *stubMatrixConnector) GenerateDeterministicEventID(id.RoomID, networkid.PortalKey, networkid.MessageID, networkid.PartID) id.EventID {
	return ""
}
func (s *stubMatrixConnector) GenerateReactionEventID(id.RoomID, *database.Message, networkid.UserID, networkid.EmojiID) id.EventID {
	return ""
}
func (s *stubMatrixConnector) ServerName() string { return "example.com" }

// newMergeRoutingTestBridge builds a real bridgev2.Bridge over a SQLite test
// database, so portal loading, caching and metadata persistence are exercised.
func newMergeRoutingTestBridge(t *testing.T) (*testBridgeDB, *bridgev2.Bridge) {
	t.Helper()
	raw, err := dbutil.NewWithDialect("file:"+filepath.Join(t.TempDir(), "routing.db"), "sqlite3")
	if err != nil {
		t.Fatalf("open sqlite database: %v", err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close sqlite database: %v", err)
		}
	})
	connector := NewMyConnector(zerolog.Nop())
	bridge := bridgev2.NewBridge(
		testBridgeID, raw, zerolog.Nop(),
		&bridgeconfig.BridgeConfig{CommandPrefix: "!sms"},
		&stubMatrixConnector{}, connector, commands.NewProcessor,
	)
	bridge.BackgroundCtx = context.Background()
	if err := raw.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade bridge schema: %v", err)
	}
	return &testBridgeDB{db: bridge.DB, bridge: bridge}, bridge
}

// TestMergedRoutingAgainstBridgeDatabase pins the routing seam against real
// portal loading: a merged source resolves to the target, the target stays in
// place, and unmerging restores the source's own routing.
func TestMergedRoutingAgainstBridgeDatabase(t *testing.T) {
	b, bridge := newMergeRoutingTestBridge(t)
	ctx := context.Background()
	b.seedPortal(t, "thread-scope-1-1", id.RoomID(mergeTestSourceRoom), &PortalMetadata{
		ThreadID: 1, SubID: 1, Recipients: []string{mergeAlice},
	})
	b.seedPortal(t, "thread-scope-1-2", id.RoomID(mergeTestTargetRoom), &PortalMetadata{
		ThreadID: 2, SubID: 1, Recipients: []string{mergeAlice, mergeBob},
	})

	client := &MyNetworkClient{bridge: bridge, log: zerolog.Nop()}
	sourceKey := networkid.PortalKey{ID: "thread-scope-1-1"}
	targetKey := networkid.PortalKey{ID: "thread-scope-1-2"}

	// Before the merge the source is its own portal; the target never routes.
	if _, key, merged := client.resolveMergedPortal(ctx, sourceKey); merged || key != sourceKey {
		t.Fatalf("resolveMergedPortal(before merge) = (%q, %t), want (source, false)", key, merged)
	}
	if portal, key, merged := client.resolveMergedPortal(ctx, targetKey); merged || key != targetKey || portal == nil {
		t.Fatalf("resolveMergedPortal(target before merge) = (%v, %q, %t)", portal, key, merged)
	}

	// Merge the source into the target through the cached portal objects.
	sourcePortal, err := bridge.GetExistingPortalByKey(ctx, sourceKey)
	if err != nil || sourcePortal == nil {
		t.Fatalf("load source portal: %v, %v", sourcePortal, err)
	}
	sourceMeta := portalMetadataOf(sourcePortal)
	sourceMeta.MergedInto = targetKey.ID
	if err := sourcePortal.Save(ctx); err != nil {
		t.Fatalf("save source portal: %v", err)
	}

	// The source now routes to the target and reports the target's metadata.
	portal, key, merged := client.resolveMergedPortal(ctx, sourceKey)
	if !merged || key != targetKey || portal == nil || portal.ID != targetKey.ID {
		t.Fatalf("resolveMergedPortal(after merge) = (%v, %q, %t), want target", portal, key, merged)
	}
	if meta, _ := portal.Metadata.(*PortalMetadata); meta == nil || meta.ThreadID != 2 {
		t.Fatalf("routed portal metadata = %+v, want the target's", portal.Metadata)
	}

	// mergedTargetForPortal resolves loaded portals and leaves the target alone.
	targetPortal, targetMeta, ok := client.mergedTargetForPortal(ctx, sourcePortal)
	if !ok || targetPortal == nil || targetKey.ID != targetPortal.ID || targetMeta == nil || targetMeta.ThreadID != 2 {
		t.Fatalf("mergedTargetForPortal(source) = (%v, %+v, %t)", targetPortal, targetMeta, ok)
	}
	loadedTarget, err := bridge.GetExistingPortalByKey(ctx, targetKey)
	if err != nil || loadedTarget == nil {
		t.Fatalf("load target portal: %v, %v", loadedTarget, err)
	}
	if _, _, ok := client.mergedTargetForPortal(ctx, loadedTarget); ok {
		t.Fatal("mergedTargetForPortal(target) reported a merge")
	}

	// Unmerge restores the source's own routing.
	sourceMeta.MergedInto = ""
	if err := sourcePortal.Save(ctx); err != nil {
		t.Fatalf("save unmerged source portal: %v", err)
	}
	if _, key, merged := client.resolveMergedPortal(ctx, sourceKey); merged || key != sourceKey {
		t.Fatalf("resolveMergedPortal(after unmerge) = (%q, %t), want (source, false)", key, merged)
	}
}

func TestMergeIDListHelpers(t *testing.T) {
	t.Parallel()
	got := mergeSortedIDs([]networkid.PortalID{"b", "a", "", "b", "c"})
	want := []networkid.PortalID{"a", "b", "c"}
	if !slices.Equal(got, want) {
		t.Fatalf("mergeSortedIDs() = %v, want %v", got, want)
	}
	if got := mergeSortedIDs(nil); got != nil {
		t.Fatalf("mergeSortedIDs(nil) = %v, want nil", got)
	}
	if got := mergeRemoveID([]networkid.PortalID{"a", "b", "c"}, "b"); !slices.Equal(got, []networkid.PortalID{"a", "c"}) {
		t.Fatalf("mergeRemoveID() = %v, want [a c]", got)
	}
	if got := mergeRemoveID([]networkid.PortalID{"a"}, "a"); got != nil {
		t.Fatalf("mergeRemoveID(last) = %v, want nil", got)
	}
}

func TestParseMergeRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		wantRooms []string
		wantInto  string
		wantYes   bool
		wantErr   string
	}{
		{
			name:      "two rooms default target",
			args:      []string{"!a:example.com", "!b:example.com"},
			wantRooms: []string{"!a:example.com", "!b:example.com"},
		},
		{
			name:      "into and yes flags",
			args:      []string{"!a:example.com", "--into", "!b:example.com", "--yes"},
			wantRooms: []string{"!a:example.com"},
			wantInto:  "!b:example.com",
			wantYes:   true,
		},
		{
			name:      "alias and portal ID",
			args:      []string{"#alias:example.com", "thread-scope-1-42"},
			wantRooms: []string{"#alias:example.com", "thread-scope-1-42"},
		},
		{
			name:    "single room",
			args:    []string{"!a:example.com"},
			wantErr: "at least two",
		},
		{
			name:    "no rooms",
			args:    []string{"--yes"},
			wantErr: "Usage:",
		},
		{
			name:    "missing into value",
			args:    []string{"!a:example.com", "!b:example.com", "--into"},
			wantErr: "--into needs a room",
		},
		{
			name:    "unknown option",
			args:    []string{"!a:example.com", "!b:example.com", "--force"},
			wantErr: "unknown option",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := parseMergeRequest(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseMergeRequest() error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMergeRequest() unexpected error: %v", err)
			}
			if !slices.Equal(req.Rooms, tc.wantRooms) {
				t.Fatalf("Rooms = %v, want %v", req.Rooms, tc.wantRooms)
			}
			if req.Into != tc.wantInto || req.Yes != tc.wantYes {
				t.Fatalf("Into/Yes = %q/%t, want %q/%t", req.Into, req.Yes, tc.wantInto, tc.wantYes)
			}
		})
	}
}
