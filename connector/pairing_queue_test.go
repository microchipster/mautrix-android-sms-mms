// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

// newTestPairedDevice registers a device runtime through the same path the
// pairing flow uses (login metadata with a token hash).
func newTestPairedDevice(t *testing.T, server *pairingServer, deviceID, token string) *pairedDevice {
	t.Helper()
	device := server.ensureDevice(&LoginMetadata{
		Transport:   LoginTransportApp,
		DeviceID:    deviceID,
		DeviceName:  "Queue Phone",
		DeviceModel: "android",
		TokenSHA256: hashPairingToken(token),
		PairedAtMS:  time.Now().UnixMilli(),
	})
	if device == nil {
		t.Fatal("ensureDevice() returned nil")
	}
	return device
}

func pairAuthorizedRequest(t *testing.T, method, url, token string, body any) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal request: %v", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("failed to build %s request: %v", method, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// pairPostJSONAuth posts a JSON body with a bearer token.
func pairPostJSONAuth(t *testing.T, url, token string, body any) (int, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(pairAuthorizedRequest(t, http.MethodPost, url, token, body))
	if err != nil {
		t.Fatalf("POST %s failed: %v", url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response of %s: %v", url, err)
	}
	return resp.StatusCode, data
}

func TestPairingQueueLongPollWakeup(t *testing.T) {
	server, httpServer := newTestPairingServerHTTP(t)
	device := newTestPairedDevice(t, server, "android-queue", "queue-token")

	type queueResult struct {
		status   int
		response pairQueueResponse
		err      error
	}
	resultCh := make(chan queueResult, 1)
	go func() {
		req := pairAuthorizedRequest(t, http.MethodGet, httpServer.URL+"/_smsmms/v1/queue?since=0&wait_ms=5000", "queue-token", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			resultCh <- queueResult{err: err}
			return
		}
		defer resp.Body.Close()
		var queue pairQueueResponse
		err = json.NewDecoder(resp.Body).Decode(&queue)
		resultCh <- queueResult{status: resp.StatusCode, response: queue, err: err}
	}()

	// Give the long-poll a moment to park before enqueuing work.
	time.Sleep(50 * time.Millisecond)
	device.enqueue(pairCommand{
		Type:       "send_sms",
		SendID:     "send-1",
		Recipients: []string{"+15551234567"},
		Text:       "hello",
	})

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("queue long-poll failed: %v", result.err)
		}
		if result.status != http.StatusOK {
			t.Fatalf("queue status = %d, want 200", result.status)
		}
		if result.response.Cursor != 1 || result.response.Reset {
			t.Fatalf("unexpected queue cursor/reset: %+v", result.response)
		}
		if len(result.response.Commands) != 1 {
			t.Fatalf("expected 1 command, got %+v", result.response.Commands)
		}
		command := result.response.Commands[0]
		if command.Type != "send_sms" || command.Seq != 1 || command.SendID != "send-1" || command.Text != "hello" {
			t.Fatalf("unexpected command: %+v", command)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queue long-poll did not wake up when a command was enqueued")
	}

	if !device.snapshot().Online {
		t.Error("device should be online after a queue long-poll")
	}
}

func TestPairingQueueEmptyLongPollDoesNotWakeEarly(t *testing.T) {
	server, httpServer := newTestPairingServerHTTP(t)
	newTestPairedDevice(t, server, "android-idle", "idle-token")

	start := time.Now()
	req := pairAuthorizedRequest(t, http.MethodGet, httpServer.URL+"/_smsmms/v1/queue?since=0&wait_ms=200", "idle-token", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("queue request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queue status = %d, want 200", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("empty long-poll returned after %s, want it to wait for wait_ms", elapsed)
	}
	var queue pairQueueResponse
	if err := json.NewDecoder(resp.Body).Decode(&queue); err != nil {
		t.Fatalf("failed to decode queue response: %v", err)
	}
	if len(queue.Commands) != 0 || queue.Reset || queue.Cursor != 0 {
		t.Fatalf("unexpected idle queue response: %+v", queue)
	}
}

func TestPairingQueueCursorAndReset(t *testing.T) {
	server := newTestPairingServer(t)
	server.commandRetention = 10 * time.Millisecond
	device := newTestPairedDevice(t, server, "android-cursor", "cursor-token")

	device.enqueue(pairCommand{Type: "send_sms", SendID: "send-1"})
	device.enqueue(pairCommand{Type: "send_sms", SendID: "send-2"})

	device.mu.Lock()
	commands, cursor, reset := device.commandsSinceLocked(1)
	device.mu.Unlock()
	if reset || cursor != 2 || len(commands) != 1 || commands[0].SendID != "send-2" {
		t.Fatalf("cursor=1 result: reset=%v cursor=%d commands=%+v", reset, cursor, commands)
	}

	device.mu.Lock()
	commands, cursor, reset = device.commandsSinceLocked(2)
	device.mu.Unlock()
	if reset || cursor != 2 || len(commands) != 0 {
		t.Fatalf("cursor=2 result: reset=%v cursor=%d commands=%+v", reset, cursor, commands)
	}

	// Aging the commands out of the retention window forces a reset.
	time.Sleep(20 * time.Millisecond)
	device.enqueue(pairCommand{Type: "send_sms", SendID: "send-3"})
	device.mu.Lock()
	commands, cursor, reset = device.commandsSinceLocked(0)
	device.mu.Unlock()
	if !reset || cursor != 3 {
		t.Fatalf("stale cursor result: reset=%v cursor=%d commands=%+v", reset, cursor, commands)
	}
	if commands != nil {
		t.Fatalf("reset response should carry no commands, got %+v", commands)
	}

	// A cursor ahead of the bridge's queue (bridge restart) also resets.
	device.mu.Lock()
	commands, cursor, reset = device.commandsSinceLocked(99)
	device.mu.Unlock()
	if !reset || cursor != 3 || commands != nil {
		t.Fatalf("future cursor result: reset=%v cursor=%d commands=%+v", reset, cursor, commands)
	}
}

func TestPairingAcknowledgedSendRemovedFromQueue(t *testing.T) {
	server := newTestPairingServer(t)
	device := newTestPairedDevice(t, server, "android-ack", "ack-token")
	device.enqueue(pairCommand{Type: "send_sms", SendID: "send-ack"})
	device.removeCommand("send-ack")
	device.mu.Lock()
	count := len(device.commands)
	device.mu.Unlock()
	if count != 0 {
		t.Fatalf("acknowledged command still queued: %+v", device.commands)
	}
}

func TestPairingEventsRoundTrip(t *testing.T) {
	server, httpServer := newTestPairingServerHTTP(t)
	device := newTestPairedDevice(t, server, "android-events", "events-token")

	// The runtime long-polls the inbox in the background.
	pageCh := make(chan remoteEventPage, 1)
	go func() {
		page, err := device.waitEvents(context.Background(), 0, 5*time.Second)
		if err != nil {
			pageCh <- remoteEventPage{Reset: true}
			return
		}
		pageCh <- page
	}()

	time.Sleep(20 * time.Millisecond)
	message := agentapi.Message{
		ID:          "sms:1:66",
		ThreadID:    33,
		SubID:       1,
		TimestampMS: 1792749554448,
		Sender:      "+15551234567",
		Body:        "see you at 8",
		Recipients:  []string{"+15550001111"},
	}
	eventsBody := pairEventsRequest{
		DeviceID: "android-events",
		Cursor:   1,
		Events: []remoteEvent{{
			Event: agentapi.Event{
				Seq:         1,
				Type:        "message.new",
				MessageID:   "sms:1:66",
				ThreadID:    33,
				SubID:       1,
				TimestampMS: 1792749554448,
				Sender:      "+15551234567",
			},
			Message:    &message,
			Recipients: []string{"+15550001111"},
		}},
	}
	status, data := pairPostJSONAuth(t, httpServer.URL+"/_smsmms/v1/events", "events-token", eventsBody)
	if status != http.StatusOK {
		t.Fatalf("events status = %d (%s)", status, data)
	}
	var eventsResponse pairEventsResponse
	if err := json.Unmarshal(data, &eventsResponse); err != nil {
		t.Fatalf("failed to decode events response: %v", err)
	}
	if eventsResponse.Accepted != 1 || eventsResponse.Cursor != 1 {
		t.Fatalf("unexpected events response: %+v", eventsResponse)
	}

	select {
	case page := <-pageCh:
		if page.Reset || page.Cursor != 1 || len(page.Events) != 1 {
			t.Fatalf("unexpected inbox page: %+v", page)
		}
		if page.Events[0].Message == nil || page.Events[0].Message.Body != "see you at 8" {
			t.Fatalf("full message payload was not preserved: %+v", page.Events[0])
		}
		if !page.Online {
			t.Error("page should report the device online after an event push")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inbox long-poll did not wake up")
	}

	// A duplicated sequence is dropped.
	status, data = pairPostJSONAuth(t, httpServer.URL+"/_smsmms/v1/events", "events-token", eventsBody)
	if status != http.StatusOK {
		t.Fatalf("duplicate events status = %d (%s)", status, data)
	}
	if err := json.Unmarshal(data, &eventsResponse); err != nil {
		t.Fatalf("failed to decode duplicate events response: %v", err)
	}
	if eventsResponse.Accepted != 0 || eventsResponse.Cursor != 1 {
		t.Fatalf("unexpected duplicate events response: %+v", eventsResponse)
	}

	// A wrong device_id is rejected.
	eventsBody.DeviceID = "android-other"
	status, data = pairPostJSONAuth(t, httpServer.URL+"/_smsmms/v1/events", "events-token", eventsBody)
	if status != http.StatusUnauthorized {
		t.Fatalf("mismatched device_id status = %d (%s), want 401", status, data)
	}
}

func TestPairingSendResultResolvesWaiter(t *testing.T) {
	server, httpServer := newTestPairingServerHTTP(t)
	device := newTestPairedDevice(t, server, "android-send", "send-token")

	type sendOutcome struct {
		result   pairSendResult
		timedOut bool
		err      error
	}
	outcomeCh := make(chan sendOutcome, 1)
	go func() {
		result, timedOut, err := device.requestSend(context.Background(), pairCommand{
			Type:       "send_sms",
			SendID:     "send-42",
			Recipients: []string{"+15551234567"},
			Text:       "hi",
		}, 3*time.Second)
		outcomeCh <- sendOutcome{result: result, timedOut: timedOut, err: err}
	}()

	// Wait until the command is queued before acknowledging it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		device.mu.Lock()
		queued := len(device.commands)
		device.mu.Unlock()
		if queued > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("send command was not queued")
		}
		time.Sleep(5 * time.Millisecond)
	}

	status, data := pairPostJSONAuth(t, httpServer.URL+"/_smsmms/v1/events", "send-token", pairEventsRequest{
		DeviceID: "android-send",
		Cursor:   2,
		Events: []remoteEvent{{
			Event: agentapi.Event{
				Seq:       2,
				Type:      "send.result",
				SendID:    "send-42",
				MessageID: "sms:1:77",
				ThreadID:  33,
			},
			PartsSent: 1,
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("send.result events status = %d (%s)", status, data)
	}

	select {
	case outcome := <-outcomeCh:
		if outcome.err != nil {
			t.Fatalf("requestSend() error: %v", outcome.err)
		}
		if outcome.timedOut {
			t.Fatal("requestSend() timed out despite a send.result")
		}
		if outcome.result.SendID != "send-42" || outcome.result.MessageID != "sms:1:77" {
			t.Fatalf("unexpected send result: %+v", outcome.result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("requestSend() did not resolve")
	}

	device.mu.Lock()
	queued := len(device.commands)
	device.mu.Unlock()
	if queued != 0 {
		t.Fatalf("acknowledged command still queued: %+v", device.commands)
	}
}

func TestPairingHeartbeatGatesMMS(t *testing.T) {
	server, httpServer := newTestPairingServerHTTP(t)
	device := newTestPairedDevice(t, server, "android-hb", "hb-token")

	// Before any contact the device is offline and MMS sending is not
	// advertised (the app must report its permissions first).
	health := device.healthSnapshot()
	if health.OK {
		t.Fatal("device without any contact should not be healthy")
	}
	if health.Backend.SendMMS {
		t.Fatal("MMS should not be advertised before the app reports send_mms")
	}

	status, data := pairPostJSONAuth(t, httpServer.URL+"/_smsmms/v1/heartbeat", "hb-token", pairHeartbeatRequest{
		DeviceID:      "android-hb",
		BatteryPct:    88,
		DefaultSMSApp: true,
		Permissions:   map[string]bool{"receive_sms": true, "send_mms": true},
		AppVersion:    "1.10.1-bridge1",
	})
	if status != http.StatusOK {
		t.Fatalf("heartbeat status = %d (%s)", status, data)
	}
	health = device.healthSnapshot()
	if !health.OK || !health.Backend.SendMMS || !health.Backend.CanMarkRead {
		t.Fatalf("unexpected health after heartbeat: %+v", health)
	}
	if device.snapshot().BatteryPct != 88 {
		t.Fatalf("battery percent not recorded: %+v", device.snapshot())
	}

	// Heartbeat with a foreign device_id is rejected.
	status, data = pairPostJSONAuth(t, httpServer.URL+"/_smsmms/v1/heartbeat", "hb-token", pairHeartbeatRequest{DeviceID: "android-other"})
	if status != http.StatusUnauthorized {
		t.Fatalf("mismatched heartbeat device_id status = %d (%s), want 401", status, data)
	}
}

func TestPairingPendingSendEchoSuppression(t *testing.T) {
	server := newTestPairingServer(t)
	device := newTestPairedDevice(t, server, "android-echo", "echo-token")
	device.trackPendingSend("send-9")
	if !device.consumePendingSend("send-9") {
		t.Fatal("pending send was not tracked")
	}
	if device.consumePendingSend("send-9") {
		t.Fatal("pending send was consumed twice")
	}
}

func TestPairingRevokedTokenRejected(t *testing.T) {
	server, httpServer := newTestPairingServerHTTP(t)
	newTestPairedDevice(t, server, "android-revoke", "revoke-token")
	server.revokeDevice("android-revoke")

	req := pairAuthorizedRequest(t, http.MethodGet, httpServer.URL+"/_smsmms/v1/queue?wait_ms=0", "revoke-token", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("queue request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want 401", resp.StatusCode)
	}
}

func TestPairingAckPrunesQueuedCommands(t *testing.T) {
	server, httpServer := newTestPairingServerHTTP(t)
	device := newTestPairedDevice(t, server, "android-ack-prune", "ack-prune-token")

	read := device.enqueue(pairCommand{Type: "mark_read", ThreadID: 33, UpToMS: 123})
	ping := device.enqueue(pairCommand{Type: "ping"})
	if read.Seq != 1 || ping.Seq != 2 {
		t.Fatalf("unexpected command seqs: %d, %d", read.Seq, ping.Seq)
	}

	queued := func() []pairCommand {
		device.mu.Lock()
		defer device.mu.Unlock()
		return append([]pairCommand(nil), device.commands...)
	}
	if got := queued(); len(got) != 2 {
		t.Fatalf("expected 2 queued commands, got %+v", got)
	}

	body := pairEventsRequest{
		DeviceID: "android-ack-prune",
		Events: []remoteEvent{{
			Event: agentapi.Event{Type: "ack", SendID: "ack-1", ThreadID: 33, SubID: 1},
		}},
	}
	status, data := pairPostJSONAuth(t, httpServer.URL+"/_smsmms/v1/events", "ack-prune-token", body)
	if status != http.StatusOK {
		t.Fatalf("ack events status = %d (%s)", status, data)
	}
	var response pairEventsResponse
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("failed to decode events response: %v", err)
	}
	if response.Accepted != 1 {
		t.Fatalf("unexpected events response: %+v", response)
	}
	if got := queued(); len(got) != 1 || got[0].Type != "ping" || got[0].Seq != 2 {
		t.Fatalf("ack-1 should only prune the mark_read command, got %+v", got)
	}

	// An ack reporting an error still prunes the command.
	body.Events = []remoteEvent{{
		Event: agentapi.Event{Type: "ack", SendID: "ack-2"},
		Error: &remoteEventError{Code: "unsupported", Message: "unknown command"},
	}}
	status, data = pairPostJSONAuth(t, httpServer.URL+"/_smsmms/v1/events", "ack-prune-token", body)
	if status != http.StatusOK {
		t.Fatalf("ack error events status = %d (%s)", status, data)
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("failed to decode events response: %v", err)
	}
	if response.Accepted != 1 {
		t.Fatalf("unexpected events response: %+v", response)
	}
	if got := queued(); len(got) != 0 {
		t.Fatalf("all acked commands should be pruned, got %+v", got)
	}
}
