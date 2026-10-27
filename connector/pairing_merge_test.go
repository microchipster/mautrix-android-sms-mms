// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

const (
	mergeAppLoginID  = networkid.UserLoginID("app:merge-test")
	mergeAppMXID     = id.UserID("@alice:example.com")
	mergeAppDeviceID = "android-merge"
	mergeAppToken    = "merge-token"
)

// newMergeThreadsTestDevice wires a paired device to a real bridge database and
// login, so the app-requested merge path resolves portals exactly like a live
// login would.
func newMergeThreadsTestDevice(t *testing.T) (*testBridgeDB, *httptest.Server) {
	t.Helper()
	b, bridge := newMergeRoutingTestBridge(t)
	login := &bridgev2.UserLogin{
		UserLogin: &database.UserLogin{ID: mergeAppLoginID, UserMXID: mergeAppMXID},
		User:      &bridgev2.User{User: &database.User{MXID: mergeAppMXID}},
	}
	client := &MyNetworkClient{bridge: bridge, login: login, log: zerolog.Nop()}
	server, httpServer := newTestPairingServerHTTP(t)
	device := newTestPairedDevice(t, server, mergeAppDeviceID, mergeAppToken)
	device.attachRuntime(client)
	return b, httpServer
}

// seedMergeThreadPortal registers one bridged phone thread for the app login.
func seedMergeThreadPortal(t *testing.T, b *testBridgeDB, threadID int64, roomID string, recipients ...string) {
	t.Helper()
	portalID := MakePortalID("scope", threadID, 1)
	b.seedPortal(t, portalID, id.RoomID(roomID), &PortalMetadata{
		ThreadID:   threadID,
		SubID:      1,
		Recipients: recipients,
	})
	b.seedUserPortal(t, mergeAppMXID, mergeAppLoginID, portalID)
}

// postMergeThreadsRequest sends one `merge_threads` events batch and returns
// the status and body.
func postMergeThreadsRequest(t *testing.T, httpServer *httptest.Server, seq int64, threadIDs []int64, targetThread int64, sendID string) (int, []byte) {
	t.Helper()
	body := pairEventsRequest{
		DeviceID: mergeAppDeviceID,
		Events: []remoteEvent{{
			Event:        agentapi.Event{Seq: seq, Type: pairEventMergeThreads, SendID: sendID},
			ThreadIDs:    threadIDs,
			TargetThread: targetThread,
		}},
	}
	return pairPostJSONAuth(t, httpServer.URL+"/_smsmms/v1/events", mergeAppToken, body)
}

// queuedMergeResultCommands returns every queued merge result as raw JSON
// keyed by send id, so wire fields can be asserted without the Go structs.
func queuedMergeResultCommands(t *testing.T, httpServer *httptest.Server) map[string]map[string]any {
	t.Helper()
	req := pairAuthorizedRequest(t, http.MethodGet, httpServer.URL+"/_smsmms/v1/queue?since=0&wait_ms=0", mergeAppToken, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("queue request failed: %v", err)
	}
	defer resp.Body.Close()
	var raw struct {
		Commands []map[string]any `json:"commands"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("failed to decode queue response: %v", err)
	}
	commands := make(map[string]map[string]any, len(raw.Commands))
	for _, command := range raw.Commands {
		if command["type"] != pairCommandMergeResult {
			continue
		}
		sendID, _ := command["send_id"].(string)
		commands[sendID] = command
	}
	return commands
}

// TestMergeThreadsWireFormat pins the JSON field names of both directions: the
// request event the app pushes and the result command the bridge queues.
func TestMergeThreadsWireFormat(t *testing.T) {
	t.Parallel()
	var event remoteEvent
	request := `{"seq":1,"type":"merge_threads","send_id":"merge-1","thread_ids":[11,12],"target_thread":12}`
	if err := json.Unmarshal([]byte(request), &event); err != nil {
		t.Fatalf("failed to decode merge_threads event: %v", err)
	}
	if event.Type != pairEventMergeThreads || event.SendID != "merge-1" {
		t.Fatalf("decoded event type/send id = %q/%q", event.Type, event.SendID)
	}
	if len(event.ThreadIDs) != 2 || event.ThreadIDs[0] != 11 || event.ThreadIDs[1] != 12 || event.TargetThread != 12 {
		t.Fatalf("decoded selection = %v -> %d", event.ThreadIDs, event.TargetThread)
	}

	payload, err := json.Marshal(pairCommand{
		Type:         pairCommandMergeResult,
		SendID:       "merge-1",
		ThreadIDs:    []int64{11, 12},
		TargetThread: 12,
		RoomID:       mergeTestTargetRoom,
	})
	if err != nil {
		t.Fatalf("failed to encode merge result: %v", err)
	}
	var command map[string]any
	if err := json.Unmarshal(payload, &command); err != nil {
		t.Fatalf("failed to decode merge result: %v", err)
	}
	if command["type"] != pairCommandMergeResult || command["send_id"] != "merge-1" {
		t.Fatalf("encoded result type/send id = %v/%v", command["type"], command["send_id"])
	}
	if command["room_id"] != mergeTestTargetRoom {
		t.Fatalf("encoded result room_id = %v", command["room_id"])
	}
	if _, hasError := command["error"]; hasError {
		t.Fatalf("a successful result must omit the error field: %s", payload)
	}
	refused, err := json.Marshal(pairCommand{
		Type:   pairCommandMergeResult,
		SendID: "merge-2",
		Error:  mergeThreadsError(pairMergeErrorInvalid, "nope"),
	})
	if err != nil {
		t.Fatalf("failed to encode refused merge result: %v", err)
	}
	if !strings.Contains(string(refused), `"error":{"code":"bad_request"`) {
		t.Fatalf("refused result does not carry the error envelope: %s", refused)
	}
}

// TestPairingMergeThreadsOnWire covers the app -> bridge merge request: the
// request event carries the selected threads, the bridge merges their portals
// and answers with a `merge_threads.result` command naming the target room.
func TestPairingMergeThreadsOnWire(t *testing.T) {
	b, httpServer := newMergeThreadsTestDevice(t)
	seedMergeThreadPortal(t, b, 11, "!source:example.com", mergeAlice)
	seedMergeThreadPortal(t, b, 12, "!target:example.com", mergeAlice, mergeBob)

	status, data := postMergeThreadsRequest(t, httpServer, 1, []int64{11, 12}, 12, "merge-1")
	if status != http.StatusOK {
		t.Fatalf("merge_threads events status = %d (%s)", status, data)
	}
	var response pairEventsResponse
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("failed to decode events response: %v", err)
	}
	if response.Accepted != 1 {
		t.Fatalf("unexpected events response: %+v", response)
	}

	command := queuedMergeResultCommands(t, httpServer)["merge-1"]
	if command == nil {
		t.Fatal("no merge_threads.result command was queued")
	}
	if command["room_id"] != "!target:example.com" {
		t.Fatalf("result room_id = %v, want the target room", command["room_id"])
	}
	if _, hasError := command["error"]; hasError {
		t.Fatalf("successful merge must not carry an error: %+v", command)
	}
	threadIDs, _ := command["thread_ids"].([]any)
	if len(threadIDs) != 2 || threadIDs[0].(float64) != 11 || threadIDs[1].(float64) != 12 {
		t.Fatalf("result thread_ids = %v, want the request's selection", command["thread_ids"])
	}
	if targetThread, ok := command["target_thread"].(float64); !ok || targetThread != 12 {
		t.Fatalf("result target_thread = %v, want 12", command["target_thread"])
	}

	// The source portal routes to the target and the target records the merge.
	source, err := b.db.Portal.GetByKey(context.Background(), networkid.PortalKey{ID: MakePortalID("scope", 11, 1)})
	if err != nil || source == nil {
		t.Fatalf("reload source portal: %v, %v", source, err)
	}
	sourceMeta, _ := source.Metadata.(*PortalMetadata)
	if sourceMeta == nil || sourceMeta.MergedInto != MakePortalID("scope", 12, 1) {
		t.Fatalf("source MergedInto = %+v, want the target portal", sourceMeta)
	}
	home, err := b.db.Portal.GetByKey(context.Background(), networkid.PortalKey{ID: MakePortalID("scope", 12, 1)})
	if err != nil || home == nil {
		t.Fatalf("reload target portal: %v, %v", home, err)
	}
	homeMeta, _ := home.Metadata.(*PortalMetadata)
	if homeMeta == nil || len(homeMeta.MergedFrom) != 1 || homeMeta.MergedFrom[0] != MakePortalID("scope", 11, 1) {
		t.Fatalf("target MergedFrom = %+v, want the merged source", homeMeta)
	}
}

// TestPairingMergeThreadsValidationPassthrough covers the app error replies:
// every refusal is the thread -> portal resolution or the planMerge validation
// of the same path `!sms merge` uses, reported as a merge_threads.result error.
func TestPairingMergeThreadsValidationPassthrough(t *testing.T) {
	b, httpServer := newMergeThreadsTestDevice(t)
	seedMergeThreadPortal(t, b, 11, "!source:example.com", mergeAlice)
	seedMergeThreadPortal(t, b, 12, "!target:example.com", mergeAlice, mergeBob)
	// A portal whose room is a Matrix space must be refused like in the command.
	spaceID := networkid.PortalID(MakePortalID("scope", 13, 1))
	spaceRow := &database.Portal{
		ID:       spaceID,
		MXID:     "!space:example.com",
		RoomType: database.RoomTypeSpace,
		Metadata: &PortalMetadata{ThreadID: 13, SubID: 1},
	}
	if err := b.db.Portal.Insert(context.Background(), spaceRow); err != nil {
		t.Fatalf("insert space portal: %v", err)
	}
	b.seedUserPortal(t, mergeAppMXID, mergeAppLoginID, spaceID)

	tests := []struct {
		name       string
		threadIDs  []int64
		target     int64
		sendID     string
		wantDetail string
	}{
		{
			name:       "single thread",
			threadIDs:  []int64{11},
			sendID:     "merge-single",
			wantDetail: "at least two",
		},
		{
			name:       "unknown thread",
			threadIDs:  []int64{11, 99},
			sendID:     "merge-unknown",
			wantDetail: "no bridged room found for phone thread 99",
		},
		{
			name:       "duplicate thread",
			threadIDs:  []int64{11, 11},
			sendID:     "merge-duplicate",
			wantDetail: "selected twice",
		},
		{
			name:       "target not selected",
			threadIDs:  []int64{11, 12},
			target:     99,
			sendID:     "merge-target",
			wantDetail: "not part of the selection",
		},
		{
			name:       "space portal",
			threadIDs:  []int64{11, 13},
			sendID:     "merge-space",
			wantDetail: "is a space",
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, data := postMergeThreadsRequest(t, httpServer, int64(i+1), tc.threadIDs, tc.target, tc.sendID)
			if status != http.StatusOK {
				t.Fatalf("events status = %d (%s)", status, data)
			}
			command := queuedMergeResultCommands(t, httpServer)[tc.sendID]
			if command == nil {
				t.Fatal("no merge_threads.result command was queued")
			}
			if command["room_id"] != nil && command["room_id"] != "" {
				t.Fatalf("refused merge reported a room: %+v", command)
			}
			rawError, ok := command["error"].(map[string]any)
			if !ok {
				t.Fatalf("refused merge carries no error object: %+v", command)
			}
			if rawError["code"] != pairMergeErrorInvalid {
				t.Fatalf("error code = %v, want %q", rawError["code"], pairMergeErrorInvalid)
			}
			message, _ := rawError["message"].(string)
			if !strings.Contains(message, tc.wantDetail) {
				t.Fatalf("error message = %q, want it to contain %q", message, tc.wantDetail)
			}
		})
	}

	// No refused request may have merged anything.
	straggler, err := b.db.Portal.GetByKey(context.Background(), networkid.PortalKey{ID: MakePortalID("scope", 12, 1)})
	if err != nil || straggler == nil {
		t.Fatalf("reload source portal: %v, %v", straggler, err)
	}
	if meta, _ := straggler.Metadata.(*PortalMetadata); meta == nil || meta.MergedInto != "" {
		t.Fatalf("a refused request merged the source: %+v", meta)
	}
}
