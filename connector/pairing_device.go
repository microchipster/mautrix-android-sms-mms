// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

// pairedDevice is the bridge-side runtime state of one app-paired device:
// the work queue the app polls (bridge -> app), the event inbox the app
// pushes into (app -> bridge), liveness/heartbeat information and the pending
// send results. The device row itself is persisted on the bridgev2 login
// metadata; everything here is in-memory and rebuilt on restart.
type pairedDevice struct {
	server   *pairingServer
	deviceID string

	mu         sync.Mutex
	tokenHash  string
	deviceName string
	platform   string
	appVersion string
	pairedAtMS int64

	// Bridge -> app command queue.
	commands      []pairCommand
	nextSeq       int64
	droppedBefore int64
	commandWake   chan struct{}

	// App -> bridge event inbox.
	events              []remoteEvent
	lastEventSeq        int64
	eventsDroppedBefore int64
	eventsWake          chan struct{}

	// Liveness.
	lastSeen      time.Time
	lastPersisted time.Time
	heartbeat     *pairHeartbeatRequest
	heartbeatAt   time.Time
	// Last SIM subscriptions reported by the app; re-applied when the login
	// finishes loading if the first heartbeat arrived earlier.
	lastSubscriptions []agentapi.Subscription

	// Outgoing sends waiting for a send.result from the app.
	waiters      map[string]chan pairSendResult
	lateResults  map[string]time.Time
	pendingSends map[string]time.Time

	// Payload fetches requested from the app for events that arrived without
	// their full message payload (see requestPayload).
	payloadRequests map[string]*payloadRequest

	runtime *MyNetworkClient
}

// payloadRequest is one outstanding fetch_message request: the original event,
// used to mirror the message without media when the app cannot provide the
// payload, and whether the app already reported that failure.
type payloadRequest struct {
	event       remoteEvent
	requestedAt time.Time
	unavailable bool
}

func newPairedDevice(server *pairingServer, deviceID string) *pairedDevice {
	return &pairedDevice{
		server:       server,
		deviceID:     deviceID,
		waiters:      make(map[string]chan pairSendResult),
		lateResults:  make(map[string]time.Time),
		pendingSends: make(map[string]time.Time),
	}
}

// applyResult stores the identity of a freshly paired device and returns the
// previous token hash (for index cleanup).
func (d *pairedDevice) applyResult(result pairSessionResult) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	oldTokenHash := d.tokenHash
	d.tokenHash = result.TokenSHA256
	d.deviceName = result.DeviceName
	d.platform = result.DevicePlatform
	d.appVersion = result.AppVersion
	d.pairedAtMS = result.PairedAtMS
	d.lastSeen = time.Now()
	return oldTokenHash
}

// applyLoginMetadata restores device state from a persisted login.
func (d *pairedDevice) applyLoginMetadata(meta *LoginMetadata) string {
	if meta == nil {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	oldTokenHash := d.tokenHash
	if meta.TokenSHA256 != "" {
		d.tokenHash = meta.TokenSHA256
	}
	if meta.DeviceName != "" {
		d.deviceName = meta.DeviceName
	}
	if meta.DeviceModel != "" {
		d.platform = meta.DeviceModel
	}
	if meta.AppVersion != "" {
		d.appVersion = meta.AppVersion
	}
	if meta.PairedAtMS != 0 {
		d.pairedAtMS = meta.PairedAtMS
	}
	if meta.LastSeenMS != 0 {
		d.lastSeen = time.UnixMilli(meta.LastSeenMS)
	}
	return oldTokenHash
}

func (d *pairedDevice) tokenHashValue() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tokenHash
}

func (d *pairedDevice) setTokenHash(hash string) {
	d.mu.Lock()
	d.tokenHash = hash
	d.mu.Unlock()
}

// deviceSnapshot is a consistent read of the identity/liveness fields used by
// the management commands and health reporting.
type deviceSnapshot struct {
	DeviceID     string
	Name         string
	Platform     string
	AppVersion   string
	PairedAtMS   int64
	LastSeen     time.Time
	LastEventSeq int64
	Online       bool
	BatteryPct   int
	HasHeartbeat bool
}

func (d *pairedDevice) snapshot() deviceSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	snapshot := deviceSnapshot{
		DeviceID:     d.deviceID,
		Name:         d.deviceName,
		Platform:     d.platform,
		AppVersion:   d.appVersion,
		PairedAtMS:   d.pairedAtMS,
		LastSeen:     d.lastSeen,
		LastEventSeq: d.lastEventSeq,
		Online:       d.onlineLocked(time.Now()),
	}
	if d.heartbeat != nil {
		snapshot.HasHeartbeat = true
		snapshot.BatteryPct = d.heartbeat.BatteryPct
	}
	return snapshot
}

func (d *pairedDevice) attachRuntime(nc *MyNetworkClient) {
	d.mu.Lock()
	d.runtime = nc
	subscriptions := append([]agentapi.Subscription(nil), d.lastSubscriptions...)
	d.mu.Unlock()
	if len(subscriptions) > 0 {
		// The app may send its first heartbeat before the login has finished
		// loading; re-apply the remembered subscriptions now that the login
		// metadata can be saved.
		go d.applyHeartbeatSubscriptions(context.Background(), subscriptions)
	}
}

func (d *pairedDevice) runtimeHandler() *MyNetworkClient {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runtime
}

func (d *pairedDevice) markSeen(now time.Time) {
	d.mu.Lock()
	d.lastSeen = now
	d.mu.Unlock()
}

func (d *pairedDevice) onlineLocked(now time.Time) bool {
	if d.lastSeen.IsZero() {
		return false
	}
	return now.Sub(d.lastSeen) <= pairingDeviceOnlineWindow
}

func (d *pairedDevice) setHeartbeat(req pairHeartbeatRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	reqCopy := req
	d.heartbeat = &reqCopy
	d.heartbeatAt = now
	d.lastSeen = now
	if req.AppVersion != "" {
		d.appVersion = req.AppVersion
	}
}

// persistLastSeen writes the liveness timestamp back to the login metadata,
// throttled so heartbeats do not cause a database write each time.
func (d *pairedDevice) persistLastSeen(ctx context.Context, now time.Time) {
	d.mu.Lock()
	if !d.lastPersisted.IsZero() && now.Sub(d.lastPersisted) < 5*time.Minute {
		d.mu.Unlock()
		return
	}
	d.lastPersisted = now
	runtime := d.runtime
	d.mu.Unlock()
	if runtime == nil || runtime.login == nil {
		return
	}
	meta, _ := runtime.login.Metadata.(*LoginMetadata)
	if meta == nil {
		return
	}
	meta.LastSeenMS = now.UnixMilli()
	if err := runtime.login.Save(ctx); err != nil {
		runtime.log.Warn().Err(err).Str("device_id", d.deviceID).Msg("Failed to persist app device last-seen timestamp")
	}
}

// applyHeartbeatSubscriptions stores the SIM subscriptions reported by the
// app so the bridge can exclude the phone's own numbers from participants.
func (d *pairedDevice) applyHeartbeatSubscriptions(ctx context.Context, subs []agentapi.Subscription) {
	if len(subs) == 0 {
		return
	}
	d.mu.Lock()
	// Remember the reported subscriptions so they can be applied once the
	// login finishes loading (the app may heartbeat before that happens).
	d.lastSubscriptions = append([]agentapi.Subscription(nil), subs...)
	runtime := d.runtime
	d.mu.Unlock()
	if runtime == nil || runtime.login == nil {
		return
	}
	meta, _ := runtime.login.Metadata.(*LoginMetadata)
	if meta == nil {
		return
	}
	if meta.Subscriptions == nil {
		meta.Subscriptions = make(map[int]SubscriptionInfo, len(subs))
	}
	changed := false
	for i, sub := range subs {
		id := sub.SubID
		if id == 0 {
			id = i + 1
		}
		info := SubscriptionInfo{
			SubID:        id,
			PhoneNumber:  sub.Number,
			DisplayName:  sub.DisplayName,
			IsDefaultSMS: sub.DefaultSMS,
		}
		if existing, ok := meta.Subscriptions[id]; ok && existing == info {
			continue
		}
		meta.Subscriptions[id] = info
		changed = true
	}
	if !changed {
		return
	}
	if err := runtime.login.Save(ctx); err != nil {
		runtime.log.Warn().Err(err).Str("device_id", d.deviceID).Msg("Failed to persist app device subscriptions")
	}
}

// healthSnapshot synthesizes the agent Health shape for an app login so the
// rest of the connector (send gating, capabilities, `!sms ping`) works
// unchanged.
func (d *pairedDevice) healthSnapshot() agentapi.Health {
	d.mu.Lock()
	now := time.Now()
	online := d.onlineLocked(now)
	lastSeen := d.lastSeen
	var heartbeat pairHeartbeatRequest
	hasHeartbeat := d.heartbeat != nil
	if hasHeartbeat {
		heartbeat = *d.heartbeat
	}
	name := d.deviceName
	platform := d.platform
	appVersion := d.appVersion
	d.mu.Unlock()

	sendMMS := hasHeartbeat && heartbeat.Permissions["send_mms"]
	canMarkRead := !hasHeartbeat || heartbeat.Permissions["mark_read"] || heartbeat.DefaultSMSApp
	defaultSMSApp := "app"
	if hasHeartbeat && !heartbeat.DefaultSMSApp {
		defaultSMSApp = ""
	}

	health := agentapi.Health{
		OK:              online,
		ProtocolVersion: pairingProtocolVersion,
		Device: agentapi.DeviceInfo{
			Serial: d.deviceID,
			Model:  strings.TrimSpace(firstNotEmpty(name, platform)),
		},
		Backend: agentapi.BackendInfo{
			Read:          "app",
			SendSMS:       "app",
			SendMMS:       sendMMS,
			CanMarkRead:   canMarkRead,
			DefaultSMSApp: defaultSMSApp,
		},
		DefaultSMSApp: defaultSMSApp,
		Capabilities:  []string{"threads", "messages", "mark-read", "send-sms", "app-pairing"},
	}
	if sendMMS {
		health.Capabilities = append(health.Capabilities, "send-mms")
	}
	if appVersion != "" {
		health.Capabilities = append(health.Capabilities, "app-version:"+appVersion)
	}
	if !online {
		if lastSeen.IsZero() {
			health.Error = "the paired phone app has not contacted the bridge yet"
		} else {
			health.Error = fmt.Sprintf("the paired phone app has not contacted the bridge for %s", time.Since(lastSeen).Round(time.Second))
		}
	}
	return health
}

// ---------------------------------------------------------------------------
// Bridge -> app command queue

func (d *pairedDevice) enqueue(cmd pairCommand) pairCommand {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.enqueueLocked(cmd)
}

// enqueueLocked appends a command to the queue. The caller must hold the
// device lock.
func (d *pairedDevice) enqueueLocked(cmd pairCommand) pairCommand {
	now := time.Now()
	d.pruneCommandsLocked(now)
	d.nextSeq++
	cmd.Seq = d.nextSeq
	cmd.EnqueuedAt = now
	d.commands = append(d.commands, cmd)
	d.broadcastCommandLocked()
	return cmd
}

// commandsSinceLocked returns the commands after the app's cursor.
func (d *pairedDevice) commandsSinceLocked(since int64) ([]pairCommand, int64, bool) {
	d.pruneCommandsLocked(time.Now())
	cursor := d.nextSeq
	if since < d.droppedBefore || since > d.nextSeq {
		// The cursor is older than the retained window, or the bridge's queue
		// was lost (restart) and the app is ahead: force a state re-sync.
		return nil, cursor, true
	}
	commands := make([]pairCommand, 0, len(d.commands))
	for _, cmd := range d.commands {
		if cmd.Seq > since {
			commands = append(commands, cmd)
		}
	}
	return commands, cursor, false
}

func (d *pairedDevice) pruneCommandsLocked(now time.Time) {
	retention := pairingCommandRetention
	if d.server != nil && d.server.commandRetention > 0 {
		retention = d.server.commandRetention
	}
	cutoff := now.Add(-retention)
	drop := 0
	for _, cmd := range d.commands {
		if !cmd.EnqueuedAt.Before(cutoff) {
			break
		}
		drop++
	}
	if excess := len(d.commands) - drop - pairingQueueMaxCommands; excess > 0 {
		drop += excess
	}
	if drop <= 0 {
		return
	}
	if seq := d.commands[drop-1].Seq; seq > d.droppedBefore {
		d.droppedBefore = seq
	}
	d.commands = append([]pairCommand(nil), d.commands[drop:]...)
}

// removeCommand drops an acknowledged command from the queue.
func (d *pairedDevice) removeCommand(sendID string) {
	if sendID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removeCommandLocked(func(cmd pairCommand) bool { return cmd.SendID == sendID })
}

// removeCommandBySeq drops a queued command by its sequence number. Ack events
// for commands without a send id carry `ack-<seq>` instead; see ackCommandSeq
// in pairing_server.go.
func (d *pairedDevice) removeCommandBySeq(seq int64) {
	if seq <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removeCommandLocked(func(cmd pairCommand) bool { return cmd.Seq == seq })
}

// removeCommandLocked drops the first command matching match. The caller must
// hold the device lock.
func (d *pairedDevice) removeCommandLocked(match func(pairCommand) bool) {
	for i, cmd := range d.commands {
		if match(cmd) {
			d.commands = append(d.commands[:i], d.commands[i+1:]...)
			return
		}
	}
}

// commandWakeLocked returns the channel the queue handler must wait on; it is
// created under the device lock so an enqueue cannot be missed.
func (d *pairedDevice) commandWakeLocked() chan struct{} {
	if d.commandWake == nil {
		d.commandWake = make(chan struct{})
	}
	return d.commandWake
}

func (d *pairedDevice) broadcastCommandLocked() {
	if d.commandWake != nil {
		close(d.commandWake)
		d.commandWake = nil
	}
}

// waitQueue long-polls the queue until commands are available, the deadline
// passes, or the context ends.
func (d *pairedDevice) waitQueue(ctx context.Context, since int64, deadline time.Time) (pairQueueResponse, error) {
	for {
		d.mu.Lock()
		d.lastSeen = time.Now()
		commands, cursor, reset := d.commandsSinceLocked(since)
		if len(commands) > 0 || reset || !time.Now().Before(deadline) {
			d.mu.Unlock()
			return pairQueueResponse{Cursor: cursor, Reset: reset, Commands: commands}, nil
		}
		wake := d.commandWakeLocked()
		d.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return pairQueueResponse{}, ctx.Err()
		case <-wake:
		case <-time.After(remaining):
		}
	}
}

// ---------------------------------------------------------------------------
// Missing message payloads (replayed app events)

// payloadRequestState is the outcome of asking the paired app for the full
// payload of an event that arrived without one.
type payloadRequestState int

const (
	// payloadRequested: a fetch_message command was enqueued now; the app will
	// push the message with inline parts, so the event must not be mirrored.
	payloadRequested payloadRequestState = iota
	// payloadInFlight: the same message was requested before and the app has
	// not answered yet.
	payloadInFlight
	// payloadUnavailable: the app reported that it cannot provide the payload;
	// the event is mirrored without media (once).
	payloadUnavailable
)

// requestPayload enqueues a fetch_message command for a message the app pushed
// without its payload. It reports whether the request was sent now, is already
// in flight, or is known to be impossible, so the caller can decide between
// waiting and mirroring the event without media. The request is remembered per
// message: it is retried when a replayed event arrives after the answer
// window (a queue reset or pruning can swallow the command) and forgotten
// after the retention window.
func (d *pairedDevice) requestPayload(ev remoteEvent) payloadRequestState {
	if ev.MessageID == "" {
		return payloadUnavailable
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if d.payloadRequests == nil {
		d.payloadRequests = make(map[string]*payloadRequest)
	}
	d.prunePayloadRequestsLocked(now)
	request, ok := d.payloadRequests[ev.MessageID]
	switch {
	case !ok:
		d.payloadRequests[ev.MessageID] = &payloadRequest{event: ev, requestedAt: now}
	case request.unavailable:
		return payloadUnavailable
	case now.Sub(request.requestedAt) < pairingPayloadRetryAfter:
		return payloadInFlight
	default:
		request.requestedAt = now
	}
	d.enqueueLocked(pairCommand{Type: pairCommandFetchMessage, MessageID: ev.MessageID})
	return payloadRequested
}

// failPayload resolves a fetch_message request the app could not satisfy. It
// returns the original event and whether the fallback still has to be mirrored
// (false for later failures of the same message).
func (d *pairedDevice) failPayload(messageID string) (remoteEvent, bool) {
	if messageID == "" {
		return remoteEvent{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prunePayloadRequestsLocked(time.Now())
	request := d.payloadRequests[messageID]
	if request == nil {
		return remoteEvent{}, false
	}
	request.requestedAt = time.Now()
	if request.unavailable {
		return remoteEvent{}, false
	}
	request.unavailable = true
	return request.event, true
}

// clearPayload forgets a fetch_message request whose payload arrived.
func (d *pairedDevice) clearPayload(messageID string) {
	if messageID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.payloadRequests, messageID)
}

// messageIDForCommandSeq returns the message id of a queued command, so a
// fetch_message request can still be identified when the app does not echo the
// message id on its ack (older app versions).
func (d *pairedDevice) messageIDForCommandSeq(seq int64) string {
	if seq <= 0 {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, cmd := range d.commands {
		if cmd.Seq == seq {
			return cmd.MessageID
		}
	}
	return ""
}

// prunePayloadRequestsLocked drops requests older than the retention window.
// The caller must hold the device lock.
func (d *pairedDevice) prunePayloadRequestsLocked(now time.Time) {
	for messageID, request := range d.payloadRequests {
		if now.Sub(request.requestedAt) > pairingPayloadRequestRetention {
			delete(d.payloadRequests, messageID)
		}
	}
}

// ---------------------------------------------------------------------------
// App -> bridge event inbox

// pushEvent accepts one app event into the inbox. It reports whether the
// event was accepted; events with a sequence at or below the last accepted
// one are treated as duplicates.
func (d *pairedDevice) pushEvent(ev remoteEvent) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastSeen = time.Now()
	if ev.Seq <= 0 {
		ev.Seq = d.lastEventSeq + 1
	}
	if ev.Seq <= d.lastEventSeq {
		return false
	}
	d.lastEventSeq = ev.Seq
	d.events = append(d.events, ev)
	if excess := len(d.events) - pairingEventInboxMax; excess > 0 {
		if seq := d.events[excess-1].Seq; seq > d.eventsDroppedBefore {
			d.eventsDroppedBefore = seq
		}
		d.events = append([]remoteEvent(nil), d.events[excess:]...)
	}
	d.broadcastEventsLocked()
	return true
}

// takePendingEventsLocked returns the retained events after the cursor.
func (d *pairedDevice) takePendingEventsLocked(since int64) ([]remoteEvent, int64, bool) {
	cursor := d.lastEventSeq
	if since < d.eventsDroppedBefore {
		return nil, cursor, true
	}
	events := make([]remoteEvent, 0, len(d.events))
	for _, ev := range d.events {
		if ev.Seq > since {
			events = append(events, ev)
		}
	}
	return events, cursor, false
}

func (d *pairedDevice) eventsWakeLocked() chan struct{} {
	if d.eventsWake == nil {
		d.eventsWake = make(chan struct{})
	}
	return d.eventsWake
}

func (d *pairedDevice) broadcastEventsLocked() {
	if d.eventsWake != nil {
		close(d.eventsWake)
		d.eventsWake = nil
	}
}

// waitEvents long-polls the inbox for events after `since`.
func (d *pairedDevice) waitEvents(ctx context.Context, since int64, timeout time.Duration) (remoteEventPage, error) {
	deadline := time.Now().Add(timeout)
	for {
		d.mu.Lock()
		events, cursor, reset := d.takePendingEventsLocked(since)
		online := d.onlineLocked(time.Now())
		if len(events) > 0 || reset || !time.Now().Before(deadline) {
			d.mu.Unlock()
			return remoteEventPage{Events: events, Cursor: cursor, Reset: reset, Online: online}, nil
		}
		wake := d.eventsWakeLocked()
		d.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return remoteEventPage{Online: online}, ctx.Err()
		case <-wake:
		case <-time.After(remaining):
		}
	}
}

// ---------------------------------------------------------------------------
// Outgoing sends

// requestSend enqueues a send command and waits for the matching send.result.
// It returns timedOut=true when the app did not answer before the timeout; the
// command stays queued and the caller falls back to a pending placeholder.
func (d *pairedDevice) requestSend(ctx context.Context, cmd pairCommand, timeout time.Duration) (pairSendResult, bool, error) {
	if timeout <= 0 {
		timeout = pairingSendTimeout
	}
	d.mu.Lock()
	waiter := make(chan pairSendResult, 1)
	d.waiters[cmd.SendID] = waiter
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.waiters, cmd.SendID)
		d.mu.Unlock()
	}()

	d.enqueue(cmd)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return pairSendResult{}, false, ctx.Err()
	case result := <-waiter:
		return result, false, nil
	case <-timer.C:
		return pairSendResult{SendID: cmd.SendID, Pending: true}, true, nil
	}
}

// resolveSend delivers a send.result to the waiting sender. It reports
// whether a live waiter consumed it; late results are pruned lazily.
func (d *pairedDevice) resolveSend(result pairSendResult) bool {
	d.mu.Lock()
	waiter := d.waiters[result.SendID]
	delete(d.waiters, result.SendID)
	now := time.Now()
	if waiter == nil && result.SendID != "" {
		if d.lateResults == nil {
			d.lateResults = make(map[string]time.Time)
		}
		d.lateResults[result.SendID] = now
	}
	for sendID, at := range d.lateResults {
		if now.Sub(at) > 30*time.Minute {
			delete(d.lateResults, sendID)
		}
	}
	d.mu.Unlock()
	if waiter != nil {
		waiter <- result
		return true
	}
	return false
}

// trackPendingSend remembers a send whose phone row has not been resolved
// yet, so the eventual message.new echo can be suppressed by send_id.
func (d *pairedDevice) trackPendingSend(sendID string) {
	if sendID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if d.pendingSends == nil {
		d.pendingSends = make(map[string]time.Time)
	}
	d.pendingSends[sendID] = now
	for id, at := range d.pendingSends {
		if now.Sub(at) > 30*time.Minute {
			delete(d.pendingSends, id)
		}
	}
}

// consumePendingSend reports whether the given send_id belongs to a pending
// outgoing send and removes it from the set.
func (d *pairedDevice) consumePendingSend(sendID string) bool {
	if sendID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.pendingSends[sendID]; !ok {
		return false
	}
	delete(d.pendingSends, sendID)
	return true
}
