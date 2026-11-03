// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

// Ensure MyNetworkClient implements the core bridgev2 interfaces.
var (
	_ bridgev2.NetworkAPI                 = (*MyNetworkClient)(nil)
	_ bridgev2.BackfillingNetworkAPI      = (*MyNetworkClient)(nil)
	_ bridgev2.ReactionHandlingNetworkAPI = (*MyNetworkClient)(nil)
)

const (
	// agentEventPollTimeoutMS matches the agent's maximum long-poll timeout.
	agentEventPollTimeoutMS = 25000
	// agentHealthTTL is how long a /v1/health response is cached.
	agentHealthTTL = 30 * time.Second
	// agentReconnectMaxBackoff caps the event-loop error backoff.
	agentReconnectMaxBackoff = time.Minute
)

// MyNetworkClient implements bridgev2.NetworkAPI for one phone login: it
// long-polls the agent event feed (or, for app-paired logins, the pairing
// server inbox), bridges messages into Matrix rooms and implements the
// Matrix -> phone direction.
type MyNetworkClient struct {
	log       zerolog.Logger
	bridge    *bridgev2.Bridge
	login     *bridgev2.UserLogin
	connector *MyConnector
	agent     *AgentClient
	device    *pairedDevice

	startMu  sync.Mutex
	started  bool
	cancel   context.CancelFunc
	stopCh   chan struct{}
	loopDone chan struct{}

	cursorMu sync.Mutex
	cursor   int64

	threadMu sync.Mutex
	threads  map[int64]agentapi.Thread

	portalMetaMu sync.Mutex

	ownInitialMu    sync.Mutex
	ownInitialCache string
	ownInitialAt    time.Time

	msgIndexMu sync.Mutex
	msgIndex   map[networkid.PortalKey][]indexedMessage

	healthMu       sync.Mutex
	healthCache    agentapi.Health
	healthCachedAt time.Time

	// sentIDs remembers messages the bridge sent itself, so the phone's copy
	// of them is not imported back into Matrix as a duplicate.
	sentMu  sync.Mutex
	sentIDs map[networkid.MessageID]time.Time

	syncMu            sync.Mutex
	syncStartOverride map[networkid.PortalKey]time.Time

	// echoHoldWait and echoHoldPollInterval bound how long a from-me message
	// with no recipients waits for the pending send's send.result before it is
	// treated as a phone-originated message after all. Zero means the default;
	// tests shorten them.
	echoHoldWait         time.Duration
	echoHoldPollInterval time.Duration
}

// NewMyNetworkClient creates a client for one login. Start() must be called
// (bridgev2 does so through Connect) before events are processed.
func NewMyNetworkClient(log zerolog.Logger, bridge *bridgev2.Bridge, login *bridgev2.UserLogin, connector *MyConnector) *MyNetworkClient {
	nc := &MyNetworkClient{
		log:       log,
		bridge:    bridge,
		login:     login,
		connector: connector,
	}
	nc.ensureTransports()
	return nc
}

// ensureTransports (re)creates the transport handles from the login metadata:
// an AgentClient for ADB agent logins, a pairedDevice for app-paired logins.
func (nc *MyNetworkClient) ensureTransports() {
	nc.ensureDevice()
	nc.ensureAgent()
}

// ensureDevice binds the pairing server's runtime state for app-paired logins.
func (nc *MyNetworkClient) ensureDevice() {
	if nc.login == nil || nc.connector == nil {
		return
	}
	meta, _ := nc.login.Metadata.(*LoginMetadata)
	if meta == nil || meta.Transport != LoginTransportApp || strings.TrimSpace(meta.DeviceID) == "" {
		return
	}
	if nc.device == nil {
		nc.device = nc.connector.pairingServer().ensureDevice(meta)
	}
}

// ensureAgent (re)creates the HTTP client from the login metadata.
func (nc *MyNetworkClient) ensureAgent() {
	if nc.login == nil {
		return
	}
	meta, _ := nc.login.Metadata.(*LoginMetadata)
	if meta == nil || strings.TrimSpace(meta.AgentURL) == "" {
		return
	}
	if nc.agent == nil || nc.agent.BaseURL != normalizeAgentURL(meta.AgentURL) || nc.agent.Token != strings.TrimSpace(meta.AgentToken) {
		nc.agent = NewAgentClient(meta.AgentURL, meta.AgentToken)
	}
}

// Connect implements bridgev2.NetworkAPI. It never blocks: the event loop runs
// in its own goroutine and reports errors through the bridge state.
func (nc *MyNetworkClient) Connect(ctx context.Context) {
	nc.log.Info().Msg("MyNetworkClient Connect called")
	nc.Start()
}

// Start launches the event loop. It is idempotent for a connected client and
// re-creates the loop channels after a Disconnect.
func (nc *MyNetworkClient) Start() {
	nc.startMu.Lock()
	if nc.started {
		nc.startMu.Unlock()
		return
	}
	nc.started = true
	stopCh := make(chan struct{})
	loopDone := make(chan struct{})
	nc.stopCh = stopCh
	nc.loopDone = loopDone
	loopCtx, cancel := context.WithCancel(context.Background())
	nc.cancel = cancel
	nc.startMu.Unlock()

	nc.ensureTransports()
	if nc.device == nil && (nc.agent == nil || strings.TrimSpace(nc.agent.BaseURL) == "") {
		nc.log.Error().Msg("Cannot start the event loop: no agent URL or paired device configured")
		nc.sendBridgeState(status.StateBadCredentials, "The login has no transport configured; log in again.", 300)
		close(loopDone)
		return
	}
	go nc.eventLoop(loopCtx, stopCh, loopDone)
	go nc.cardDAVAutoSyncLoop(loopCtx, stopCh)
}

func (nc *MyNetworkClient) eventLoop(ctx context.Context, stopCh <-chan struct{}, loopDone chan<- struct{}) {
	defer close(loopDone)
	log := nc.log.With().Str("component", "remote-event-loop").Logger()
	backoff := time.Second
	connected := false
	reportedOffline := false
	for {
		select {
		case <-stopCh:
			return
		default:
		}

		page, err := nc.pollRemoteEvents(ctx, nc.getCursor())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn().Err(err).Msg("Remote event poll failed")
			if connected {
				connected = false
				nc.sendBridgeState(status.StateTransientDisconnect, fmt.Sprintf("Phone not reachable: %v", err), int(backoff.Seconds())+5)
			}
			select {
			case <-stopCh:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > agentReconnectMaxBackoff {
				backoff = agentReconnectMaxBackoff
			}
			continue
		}

		backoff = time.Second
		switch {
		case page.Online && !connected:
			connected = true
			reportedOffline = false
			nc.log.Info().Msg("Connected to the phone event feed")
			nc.sendBridgeState(status.StateConnected, "", 0)
			nc.refreshThreadsAndCatchup(ctx)
		case !page.Online && connected:
			connected = false
			reportedOffline = true
			nc.log.Warn().Msg("Paired phone app stopped responding")
			nc.sendBridgeState(status.StateTransientDisconnect, "The paired phone app is not responding.", 60)
		case !page.Online && !reportedOffline:
			// The app login exists but the phone has not contacted the bridge
			// yet (e.g. right after pairing, or while the phone is offline).
			reportedOffline = true
			nc.log.Warn().Msg("Paired phone app has not contacted the bridge yet")
			nc.sendBridgeState(status.StateTransientDisconnect, "Waiting for the paired phone app to connect.", 60)
		}

		if page.Reset {
			target, advanced := cursorAfterReset(page.Cursor, nc.getCursor())
			if nc.device != nil {
				log.Warn().Int64("cursor", target).Msg("Remote event log was reset; skipping to the retained window (app transport has no history endpoint)")
			} else {
				log.Warn().Int64("cursor", target).Msg("Remote event log was reset; re-scanning recent messages")
			}
			if !advanced {
				// The server did not offer a usable cursor; pace the loop so a
				// persistent reset cannot spin the CPU.
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
				continue
			}
			nc.setCursor(target)
			if nc.device == nil {
				nc.rescanRecentMessages(ctx)
			}
			continue
		}
		nc.setCursor(page.Cursor)
		for _, ev := range page.Events {
			nc.handleRemoteEvent(ctx, ev)
		}
	}
}

// pollRemoteEvents fetches the next batch of remote events. Agent logins
// long-poll the agent HTTP API; app logins long-poll the pairing server inbox.
func (nc *MyNetworkClient) pollRemoteEvents(ctx context.Context, since int64) (remoteEventPage, error) {
	if nc.device != nil {
		return nc.device.waitEvents(ctx, since, time.Duration(agentEventPollTimeoutMS)*time.Millisecond)
	}
	nc.ensureAgent()
	if nc.agent == nil {
		return remoteEventPage{}, fmt.Errorf("no agent URL configured for this login")
	}
	response, err := nc.agent.Events(ctx, since, agentEventPollTimeoutMS)
	if err != nil {
		return remoteEventPage{}, err
	}
	events := make([]remoteEvent, len(response.Events))
	for i, ev := range response.Events {
		events[i] = remoteEvent{Event: ev}
	}
	return remoteEventPage{Events: events, Cursor: response.Cursor, Reset: response.Reset, Online: true}, nil
}

// handleRemoteEvent dispatches one event from either transport. Events pushed
// by the app may carry the full message; otherwise message.* events make the
// bridge fetch the message from the agent (app events without a payload are
// synthesized from the event fields).
func (nc *MyNetworkClient) handleRemoteEvent(ctx context.Context, ev remoteEvent) {
	switch ev.Type {
	case "message.new", "mms.downloaded":
		switch {
		case ev.Message != nil:
			nc.bridgeRemoteMessage(ctx, *ev.Message, ev.SendID)
		case nc.device != nil:
			nc.handlePayloadlessEvent(ctx, ev)
		default:
			nc.handleAgentMessage(ctx, ev.MessageID)
		}
	case "message.updated":
		switch {
		case ev.Message != nil:
			nc.handleRemoteMessageUpdate(ctx, *ev.Message)
		case nc.device != nil:
			// Without the full message there is nothing to refresh.
		default:
			nc.handleAgentMessageUpdate(ctx, ev.MessageID)
		}
	case "thread.updated":
		nc.handleAgentThreadUpdate(ctx, ev)
	case "send.result":
		nc.handleSendResult(ctx, ev)
	default:
		nc.log.Debug().Str("event_type", ev.Type).Msg("Ignoring unknown remote event type")
	}
}

// handlePayloadlessEvent mirrors an app event that arrived without its full
// message payload. App transports replay recent provider state without media
// bytes (startup gap snapshots, `reset: true` re-syncs, ring-buffer overflows),
// and the bridge cannot fetch MMS parts from a NATed phone. Media-bearing
// messages are therefore fetched from the app (fetch_message) before they are
// bridged: mirroring them text-only would drop the media for good, because the
// text-only message row would deduplicate the later payload.
func (nc *MyNetworkClient) handlePayloadlessEvent(ctx context.Context, ev remoteEvent) {
	if ev.HasAttachment {
		if nc.hasBridgedMessage(ctx, ev) {
			nc.log.Debug().
				Str("message_id", ev.MessageID).
				Msg("Ignoring replayed event for a message that is already bridged")
			return
		}
		switch nc.device.requestPayload(ev) {
		case payloadRequested:
			nc.log.Info().
				Str("message_id", ev.MessageID).
				Msg("Event arrived without its payload; asking the paired app for the message with inline parts")
			return
		case payloadInFlight:
			nc.log.Debug().
				Str("message_id", ev.MessageID).
				Msg("Payload of the replayed event is still on its way from the paired app")
			return
		case payloadUnavailable:
			// Fall through: mirror the event without media.
		}
	}
	nc.mirrorPayloadlessEvent(ctx, ev)
}

// mirrorPayloadlessEvent bridges an app event whose media bytes are not
// available, keeping the phone's caption/preview and naming the missing media.
func (nc *MyNetworkClient) mirrorPayloadlessEvent(ctx context.Context, ev remoteEvent) {
	nc.bridgeRemoteMessage(ctx, syntheticMessageFromEvent(ev), ev.SendID)
}

// hasBridgedMessage reports whether the bridge already stored this phone
// message, so replayed events for it do not trigger another payload fetch.
func (nc *MyNetworkClient) hasBridgedMessage(ctx context.Context, ev remoteEvent) bool {
	if nc.bridge == nil || ev.MessageID == "" {
		return false
	}
	// Portal keys carry no receiver in this bridge (split portals are off), so
	// the receiver is the empty login ID bridgev2 itself deduplicates with.
	receiver := nc.portalKeyFor(ev.ThreadID, ev.SubID).Receiver
	parts, err := nc.bridge.DB.Message.GetAllPartsByID(ctx, receiver, networkid.MessageID(ev.MessageID))
	if err != nil {
		nc.log.Debug().Err(err).Str("message_id", ev.MessageID).Msg("Failed to check for an already bridged message")
		return false
	}
	return len(parts) > 0
}

// syntheticMessageFromEvent builds a minimal Message when the app pushes an
// event without the full message payload. The body falls back to the preview,
// plus a clear marker when the phone reported media the bridge did not get.
func syntheticMessageFromEvent(ev remoteEvent) agentapi.Message {
	body := ev.Preview
	if ev.HasAttachment {
		body = missingMediaBody(body, nil)
	}
	msg := agentapi.Message{
		ID:          ev.MessageID,
		ThreadID:    ev.ThreadID,
		SubID:       ev.SubID,
		TimestampMS: ev.TimestampMS,
		Sender:      ev.Sender,
		IsMMS:       ev.IsMMS,
		Body:        body,
	}
	if ev.Recipients != nil {
		msg.Recipients = ev.Recipients
	}
	return msg
}

// outboundEchoDecision classifies an app-pushed message that may be the
// phone's copy (echo) of a message this bridge sent itself.
type outboundEchoDecision int

const (
	// bridgeEchoNotOurs: not a known echo; bridge the message normally.
	bridgeEchoNotOurs outboundEchoDecision = iota
	// bridgeEchoSuppressed: the message is our echo; drop it.
	bridgeEchoSuppressed
	// bridgeEchoHeld: the message may be our echo but its correlation has not
	// arrived yet; re-evaluate it (bounded) before bridging.
	bridgeEchoHeld
)

const (
	// heldEchoWaitDefault bounds how long a from-me message without recipients
	// waits for pending sends to resolve. Echoes and their send.result are
	// produced milliseconds apart, so the wait only matters when the app
	// cannot correlate the echo at all (older app version).
	heldEchoWaitDefault = 5 * time.Second
	// heldEchoPollDefault is the re-check interval of a held echo.
	heldEchoPollDefault = 100 * time.Millisecond
)

// bridgeRemoteMessage bridges an app-pushed message, applying the echo
// suppression for sends the bridge dispatched. The decision must not depend on
// whether the app's message.new or the matching send.result arrives first: the
// send id is tracked when the send is dispatched, the phone row id as soon as
// the send.result is applied, and a from-me message without recipients is held
// briefly while a send is still unresolved.
func (nc *MyNetworkClient) bridgeRemoteMessage(ctx context.Context, msg agentapi.Message, sendID string) {
	switch nc.classifyOutboundEcho(msg, sendID) {
	case bridgeEchoSuppressed:
		nc.log.Debug().
			Str("send_id", sendID).
			Str("message_id", msg.ID).
			Msg("Ignoring phone echo of a message sent from Matrix")
		return
	case bridgeEchoHeld:
		nc.log.Debug().
			Str("send_id", sendID).
			Str("message_id", msg.ID).
			Msg("Holding a from-me message without recipients while an outbound send is unresolved")
		nc.holdEchoCandidate(msg, sendID)
		return
	default:
		nc.bridgeAgentMessage(ctx, msg)
	}
}

// classifyOutboundEcho decides whether an app-pushed message is the phone's
// copy of a message this bridge sent itself. Agent-transport sends resolve
// synchronously and keep their suppression inside bridgeAgentMessage.
func (nc *MyNetworkClient) classifyOutboundEcho(msg agentapi.Message, sendID string) outboundEchoDecision {
	if nc.device == nil {
		return bridgeEchoNotOurs
	}
	// The app's echo of a queue-command send carries the send id, which is
	// known before the send.result is processed.
	if sendID != "" && nc.device.consumePendingSend(sendID) {
		if msg.ID != "" {
			nc.markSentFromMatrix(networkid.MessageID(msg.ID))
		}
		return bridgeEchoSuppressed
	}
	// An older app version does not correlate the echo; the phone row id from
	// the send.result is then the only link.
	if msg.ID != "" && nc.consumeSentFromMatrix(networkid.MessageID(msg.ID)) {
		return bridgeEchoSuppressed
	}
	// A from-me message without recipients is exactly what the app's echo
	// looks like. If a dispatched send is still unresolved it may be the echo
	// whose correlation has not arrived yet, so hold it instead of treating it
	// as a phone-originated message (whose empty recipient list would wipe the
	// room's members and name).
	if msg.Sender == "" && len(msg.Recipients) == 0 && nc.device.hasPendingSends() {
		return bridgeEchoHeld
	}
	return bridgeEchoNotOurs
}

// holdEchoCandidate re-evaluates a possible echo in the background: holding
// must not block the event loop, so inbound messages keep flowing while the
// bounded wait ends as soon as the pending sends resolve or the timeout passes.
func (nc *MyNetworkClient) holdEchoCandidate(msg agentapi.Message, sendID string) {
	go func() {
		if nc.waitForEchoResolution(msg, sendID) {
			return
		}
		// The wait is bounded and the message is bridged with the
		// never-clear-on-empty guards in place, so a detached context is safe.
		nc.bridgeAgentMessage(context.Background(), msg)
	}()
}

// waitForEchoResolution reports whether a held message turned out to be the
// echo of a send this bridge dispatched. It polls until the echo is matched,
// no unresolved send is left, or the hold timeout passes.
func (nc *MyNetworkClient) waitForEchoResolution(msg agentapi.Message, sendID string) bool {
	if nc.device == nil {
		return false
	}
	deadline := time.Now().Add(nc.echoHoldTimeout())
	for {
		if sendID != "" && nc.device.consumePendingSend(sendID) {
			if msg.ID != "" {
				nc.markSentFromMatrix(networkid.MessageID(msg.ID))
			}
			return true
		}
		if msg.ID != "" && nc.consumeSentFromMatrix(networkid.MessageID(msg.ID)) {
			return true
		}
		if !nc.device.hasPendingSends() || !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(nc.echoHoldPoll())
	}
}

func (nc *MyNetworkClient) echoHoldTimeout() time.Duration {
	if nc.echoHoldWait > 0 {
		return nc.echoHoldWait
	}
	return heldEchoWaitDefault
}

func (nc *MyNetworkClient) echoHoldPoll() time.Duration {
	if nc.echoHoldPollInterval > 0 {
		return nc.echoHoldPollInterval
	}
	return heldEchoPollDefault
}

// handleRemoteMessageUpdate refreshes the in-memory tapback index from the
// full message payload the app pushes for message.updated events.
func (nc *MyNetworkClient) handleRemoteMessageUpdate(ctx context.Context, msg agentapi.Message) {
	if msg.ID == "" || msg.Type == 3 || msg.Sender == "" {
		return
	}
	nc.cacheThreadForMessage(msg)
	nc.indexMessage(nc.portalKeyFor(msg.ThreadID, msg.SubID), indexedMessage{
		ID:            networkid.MessageID(msg.ID),
		Body:          msg.Body,
		HasAttachment: len(msg.Attachments) > 0,
	})
}

// handleSendResult resolves a pending outgoing send from the app.
func (nc *MyNetworkClient) handleSendResult(ctx context.Context, ev remoteEvent) {
	if nc.device == nil {
		return
	}
	delivered := nc.device.resolveSend(pairSendResult{
		SendID:    ev.SendID,
		MessageID: ev.MessageID,
		ThreadID:  ev.ThreadID,
		PartsSent: ev.PartsSent,
		Pending:   ev.Pending,
		Error:     ev.Error,
	})
	if delivered {
		return
	}
	// No one is waiting anymore: apply the echo suppression directly.
	nc.handleLateSendResult(pairSendResult{SendID: ev.SendID, MessageID: ev.MessageID, Error: ev.Error})
}

// rememberThreadRecipients merges the participant list reported by a
// thread.updated event into the thread cache. App transports have no thread
// list endpoint, so this is the only way to refresh portal recipient metadata
// without waiting for a new message in the thread.
func (nc *MyNetworkClient) rememberThreadRecipients(threadID int64, subID int, recipients []string) {
	if threadID == 0 || len(recipients) == 0 {
		return
	}
	nc.threadMu.Lock()
	defer nc.threadMu.Unlock()
	if nc.threads == nil {
		nc.threads = make(map[int64]agentapi.Thread)
	}
	thread := nc.threads[threadID]
	thread.ID = threadID
	if subID > 0 {
		thread.SubID = subID
	}
	thread.Recipients = append([]string{}, recipients...)
	nc.threads[threadID] = thread
}

// cacheThreadForMessage keeps the thread cache warm for app logins, which
// have no /v1/threads endpoint to refresh from.
//
// An event without participant information (the from-me echo of a send, a
// replayed message whose recipients were not carried) must never replace a
// known list: an empty list would make the room reconciliation clear the
// members and rename the room to "Unknown conversation".
func (nc *MyNetworkClient) cacheThreadForMessage(msg agentapi.Message) {
	if msg.ThreadID == 0 {
		return
	}
	recipients := append([]string{}, msg.Recipients...)
	if msg.Sender != "" {
		recipients = append(recipients, msg.Sender)
	}
	nc.threadMu.Lock()
	defer nc.threadMu.Unlock()
	if nc.threads == nil {
		nc.threads = make(map[int64]agentapi.Thread)
	}
	if len(recipients) == 0 {
		if existing, ok := nc.threads[msg.ThreadID]; ok && len(existing.Recipients) > 0 {
			return
		}
	}
	nc.threads[msg.ThreadID] = agentapi.Thread{
		ID:         msg.ThreadID,
		SubID:      msg.SubID,
		Recipients: recipients,
	}
}

// handleAgentMessage fetches an agent message by ID and bridges it.
func (nc *MyNetworkClient) handleAgentMessage(ctx context.Context, msgID string) {
	if msgID == "" {
		return
	}
	msg, err := nc.agent.Message(ctx, msgID)
	if err != nil {
		nc.log.Warn().Err(err).Str("message_id", msgID).Msg("Failed to fetch message from agent")
		return
	}
	nc.bridgeAgentMessage(ctx, msg)
}

// handleAgentMessageUpdate refreshes the in-memory tapback index for an edited
// message. Body edits are rare on SMS/MMS; re-importing as an edit is left to
// a future revision so no duplicate rooms/messages can be created.
func (nc *MyNetworkClient) handleAgentMessageUpdate(ctx context.Context, msgID string) {
	if msgID == "" {
		return
	}
	msg, err := nc.agent.Message(ctx, msgID)
	if err != nil {
		return
	}
	if msg.Type == 3 || msg.Sender == "" {
		return
	}
	nc.indexMessage(nc.portalKeyFor(msg.ThreadID, msg.SubID), indexedMessage{
		ID:            networkid.MessageID(msg.ID),
		Body:          msg.Body,
		HasAttachment: len(msg.Attachments) > 0,
	})
}

// handleAgentThreadUpdate refreshes the participant cache and, when the portal
// already exists, pushes the new participant list to the Matrix room. Thread ID
// changes never create a new room.
// findPortalForThread locates the existing portal for a phone thread when the
// event does not carry enough identity to rebuild the device-scoped portal
// key. App transports report sub_id -1 on thread.updated events (the phone
// thread identity there is implicit), so fall back to scanning the portals
// already linked to this login.
func (nc *MyNetworkClient) findPortalForThread(ctx context.Context, threadID int64) (*bridgev2.Portal, networkid.PortalKey, bool) {
	userPortals, err := nc.bridge.DB.UserPortal.GetAllForLogin(ctx, nc.login.UserLogin)
	if err != nil {
		nc.log.Warn().Err(err).Msg("Failed to list portals for thread update fallback")
		return nil, networkid.PortalKey{}, false
	}
	for _, userPortal := range userPortals {
		portal, err := nc.bridge.GetExistingPortalByKey(ctx, userPortal.Portal)
		if err != nil || portal == nil || portal.MXID == "" {
			continue
		}
		meta, _ := portal.Metadata.(*PortalMetadata)
		if meta == nil || meta.ThreadID != threadID {
			continue
		}
		return portal, userPortal.Portal, true
	}
	return nil, networkid.PortalKey{}, false
}

func (nc *MyNetworkClient) handleAgentThreadUpdate(ctx context.Context, ev remoteEvent) {
	nc.refreshThreads(ctx)
	// App transports have no thread list endpoint, so the recipients carried
	// by the event are the only way to refresh portal metadata after a bridge
	// restart (e.g. portals that were stored without participants).
	nc.rememberThreadRecipients(ev.ThreadID, ev.SubID, ev.Recipients)
	thread, ok := nc.cachedThread(ev.ThreadID)
	nc.log.Debug().
		Int64("thread_id", ev.ThreadID).
		Int("sub_id", ev.SubID).
		Strs("event_recipients", ev.Recipients).
		Bool("cached", ok).
		Msg("Handling thread update event")
	if !ok {
		return
	}
	portalKey := nc.portalKeyFor(thread.ID, thread.SubID)
	portal, err := nc.bridge.GetExistingPortalByKey(ctx, portalKey)
	if err != nil || portal == nil || portal.MXID == "" {
		fallbackPortal, fallbackKey, ok := nc.findPortalForThread(ctx, thread.ID)
		if !ok {
			nc.log.Debug().
				Str("portal_id", string(portalKey.ID)).
				Any("err", err).
				Msg("No portal to update for thread update event")
			return
		}
		portal = fallbackPortal
		portalKey = fallbackKey
	}
	// Merge routing: a thread update for a merged-away portal reconciles the
	// participant union of the target instead of the tombstoned source room.
	// For the target itself the union accumulates instead of being replaced.
	targetPortal, homeKey, routed := nc.resolveMergedPortal(ctx, portalKey)
	if routed {
		portal = targetPortal
		portalKey = homeKey
		thread = mergeRoutedThread(portalMetadataOf(portal), thread)
	} else {
		meta, _ := portal.Metadata.(*PortalMetadata)
		thread = mergeAccumulateRecipients(meta, thread)
	}
	// Never reconcile membership from an empty participant list: fall back to
	// the participants already stored for the portal.
	thread = nc.withStoredRecipients(ctx, portalKey, thread)
	chatInfo := nc.chatInfoForThread(ctx, thread)
	if nc.applyPortalThread(portal, thread) {
		if err := portal.Save(ctx); err != nil {
			nc.log.Err(err).Str("portal_id", string(portal.ID)).Msg("Failed to save updated portal metadata")
		}
		if len(thread.Recipients) == 0 {
			// The phone reported no participants; a full member list built
			// from it would clear the room. Only reconcile when the event
			// carries a usable list.
			nc.log.Warn().
				Str("portal_id", string(portal.ID)).
				Msg("Thread update carried no participants; not queuing a chat info change")
			return
		}
		nc.bridge.QueueRemoteEvent(nc.login, &simplevent.ChatInfoChange{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatInfoChange,
				PortalKey: portalKey,
				Timestamp: time.Now(),
			},
			ChatInfoChange: &bridgev2.ChatInfoChange{ChatInfo: chatInfo},
		})
		nc.log.Info().
			Str("portal_id", string(portal.ID)).
			Strs("recipients", thread.Recipients).
			Msg("Thread participants changed; queued chat info change")
	}
}

// eventSenderForMessage builds the bridgev2 sender for an agent message.
// Agent messages with an empty sender are the phone's own (outgoing) messages.
// They still need a non-empty remote user ID, because bridgev2 stores it as
// message.sender_id, which has a foreign key to the ghost table: an empty
// sender makes the insert fail with a message_sender_fkey violation. The
// login's own user (its own-number ghost, falling back to the login ID) is
// that user. When double puppeting is available the Matrix event is still sent
// as the user, so the own-number ghost only exists as a database row.
func (nc *MyNetworkClient) eventSenderForMessage(msg agentapi.Message) bridgev2.EventSender {
	if msg.Sender == "" {
		return bridgev2.EventSender{IsFromMe: true, Sender: nc.ownUserID()}
	}
	return bridgev2.EventSender{Sender: MakeUserID(msg.Sender)}
}

// bridgeAgentMessage does the actual bridging of one agent message: room
// setup/participant updates, tapback conversion and the message event itself.
func (nc *MyNetworkClient) bridgeAgentMessage(ctx context.Context, msg agentapi.Message) {
	if msg.ID == "" || msg.ThreadID == 0 {
		return
	}
	if msg.Type == 3 {
		// Drafts are never bridged.
		return
	}
	if nc.device != nil {
		nc.cacheThreadForMessage(msg)
	}
	portalKey := nc.portalKeyFor(msg.ThreadID, msg.SubID)
	fromMe := msg.Sender == ""
	eventSender := nc.eventSenderForMessage(msg)

	// Suppress the phone's copy of messages the bridge sent itself.
	if fromMe && nc.consumeSentFromMatrix(networkid.MessageID(msg.ID)) {
		nc.log.Debug().Str("message_id", msg.ID).Msg("Ignoring echo of a message sent from Matrix")
		return
	}

	// Merge routing: a portal that was merged away hands its events to the
	// target room. Replies still get attributed to the original sender ghost;
	// only the portal (and the participant union) changes.
	homeKey, targetMeta, routed := nc.mergeRoute(ctx, portalKey)

	// Inbound gates: drop messages from blocked contacts and tag the sender of
	// a first-contact message before the portal is created.
	if !fromMe {
		if nc.dropBlockedInbound(ctx, portalKey, msg.Sender) {
			return
		}
		if routed && nc.dropBlockedInbound(ctx, homeKey, msg.Sender) {
			return
		}
	}

	timestamp := agentMessageTimestamp(msg)
	spam := spamDecision{Action: spamActionAllow}
	if !fromMe {
		// The anti-spam gate runs after the block gate and before anything is
		// queued for Matrix. "hold"/"block" decisions drop the message;
		// "label"/"flag" decisions still bridge it.
		var drop bool
		spam, drop = nc.spamGate(ctx, homeKey, msg.Sender, msg.ID, msg.Body, timestamp, nc.messageIsGroup(msg))
		if drop {
			return
		}
	}

	// iPhone tapbacks arrive as text; convert them to reactions when the
	// referenced message can be found, otherwise fall through and forward the
	// text unchanged. Outgoing text is never treated as a tapback: it is
	// regular text typed on the phone.
	if !fromMe {
		if tb, ok := parseTapback(msg.Body); ok {
			searchKeys := []networkid.PortalKey{portalKey}
			if routed {
				searchKeys = append(searchKeys, homeKey)
			}
			if nc.handleIncomingTapback(ctx, homeKey, searchKeys, eventSender.Sender, tb, agentMessageTimestamp(msg)) {
				return
			}
		}
	}

	thread := nc.threadForMessage(ctx, msg)
	if routed {
		thread = mergeRoutedThread(targetMeta, thread)
	} else {
		thread = mergeAccumulateRecipients(targetMeta, thread)
	}
	// A from-me message without recipients must not clear the participants
	// stored for the room; fall back to them instead.
	thread = nc.withStoredRecipients(ctx, homeKey, thread)
	chatInfo := nc.chatInfoForThread(ctx, thread)
	nc.ensurePortal(ctx, homeKey, chatInfo, thread)

	evt := &simplevent.Message[agentapi.Message]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventMessage,
			PortalKey: homeKey,
			Sender:    eventSender,
			Timestamp: timestamp,
			PostHandleFunc: func(ctx context.Context, portal *bridgev2.Portal) {
				nc.markPortalHistoryProgress(ctx, portal, timestamp, msg.ID, "")
			},
		},
		Data: msg,
		ID:   networkid.MessageID(msg.ID),
		ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data agentapi.Message) (*bridgev2.ConvertedMessage, error) {
			converted, err := nc.buildConvertedMessage(ctx, portal, intent, data)
			if err != nil {
				return converted, err
			}
			applySpamMessageMetadata(converted, spam)
			return converted, nil
		},
	}
	nc.bridge.QueueRemoteEvent(nc.login, evt)
	refDirection := cdnRefDirectionReceived
	if fromMe {
		refDirection = cdnRefDirectionSent
	}
	nc.recordCdnRefsFromText(refDirection, msg.Sender, string(homeKey.ID), msg.Body, timestamp)
	nc.indexMessage(homeKey, indexedMessage{
		ID:            networkid.MessageID(msg.ID),
		Body:          msg.Body,
		HasAttachment: len(msg.Attachments) > 0,
	})
	nc.log.Info().
		Str("portal_id", string(homeKey.ID)).
		Str("message_id", msg.ID).
		Bool("is_from_me", fromMe).
		Bool("merge_routed", routed).
		Int("attachment_count", len(msg.Attachments)).
		Msg("Queued incoming message to Matrix")
}

// ensurePortal creates the Matrix room (via ChatResync) when needed and queues
// a ChatInfoChange when the participant set changed since the room was made.
// The portal row is created (and its metadata stored) before the events are
// queued, so GetChatInfo can always answer with the full desired state.
func (nc *MyNetworkClient) ensurePortal(ctx context.Context, portalKey networkid.PortalKey, chatInfo *bridgev2.ChatInfo, thread agentapi.Thread) {
	portal, err := nc.bridge.GetPortalByKey(ctx, portalKey)
	if err != nil || portal == nil {
		nc.log.Warn().Err(err).Str("portal_id", string(portalKey.ID)).Msg("Failed to load or create portal")
		return
	}
	changed := nc.applyPortalThread(portal, thread)
	if changed {
		if err := portal.Save(ctx); err != nil {
			nc.log.Err(err).Str("portal_id", string(portal.ID)).Msg("Failed to save updated portal metadata")
		}
	}
	if portal.MXID == "" {
		nc.bridge.QueueRemoteEvent(nc.login, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    portalKey,
				CreatePortal: true,
				Timestamp:    time.Now(),
			},
			ChatInfo: chatInfo,
		})
		return
	}
	if !changed {
		return
	}
	if len(thread.Recipients) == 0 {
		// Never emit a membership reconciliation built from an empty
		// participant list: a full member list with just the bridge user would
		// clear the room's members and rename it to "Unknown conversation".
		nc.log.Warn().
			Str("portal_id", string(portal.ID)).
			Msg("No participants known for the thread; not queuing a chat info change")
		return
	}
	nc.bridge.QueueRemoteEvent(nc.login, &simplevent.ChatInfoChange{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatInfoChange,
			PortalKey: portalKey,
			Timestamp: time.Now(),
		},
		ChatInfoChange: &bridgev2.ChatInfoChange{ChatInfo: chatInfo},
	})
}

// portalThreadSnapshot returns the thread identity stored in a portal's
// metadata under the portal metadata lock, so history/backfill reads don't race
// with participant updates from the event loop.
func (nc *MyNetworkClient) portalThreadSnapshot(portal *bridgev2.Portal) (threadID int64, subID int, recipients []string) {
	if portal == nil {
		return 0, 0, nil
	}
	nc.portalMetaMu.Lock()
	defer nc.portalMetaMu.Unlock()
	meta, _ := portal.Metadata.(*PortalMetadata)
	if meta == nil {
		return 0, 0, nil
	}
	if len(meta.Recipients) > 0 {
		recipients = append([]string{}, meta.Recipients...)
	} else if meta.OtherUserID != "" {
		recipients = []string{UnescapeAddress(string(meta.OtherUserID))}
	}
	return meta.ThreadID, meta.SubID, recipients
}

// threadRecipients resolves the participant list used for portal metadata and
// room membership. The phone's own numbers are dropped so they can never
// become room members or send targets (PLAN §5.1), except in a self-thread -
// a conversation the phone opened with its own number (e.g. a note to self) -
// where the own number is the only counterpart and dropping it would make the
// thread unsendable.
func (nc *MyNetworkClient) threadRecipients(thread agentapi.Thread) []string {
	recipients := filterOwnNumbers(thread.Recipients, nc.ownNumbers())
	if len(recipients) > 0 {
		return recipients
	}
	return filterOwnNumbers(thread.Recipients, nil)
}

// withStoredRecipients backfills a thread's participant list from the portal's
// stored metadata when the event carries none. The stored participants stay
// the truth until the phone reports a new list: an event without recipients
// (the echo of a send, a replayed message) must never be turned into a full
// membership reconciliation that clears the room's members and name.
func (nc *MyNetworkClient) withStoredRecipients(ctx context.Context, key networkid.PortalKey, thread agentapi.Thread) agentapi.Thread {
	if len(thread.Recipients) > 0 || key.ID == "" || nc.bridge == nil {
		return thread
	}
	portal, err := nc.bridge.GetExistingPortalByKey(ctx, key)
	if err != nil || portal == nil {
		return thread
	}
	meta, _ := portal.Metadata.(*PortalMetadata)
	if meta == nil || len(meta.Recipients) == 0 {
		return thread
	}
	thread.Recipients = append([]string{}, meta.Recipients...)
	return thread
}

// applyPortalThread stores the thread identity and participant set in the
// portal metadata. It reports whether anything changed.
func (nc *MyNetworkClient) applyPortalThread(portal *bridgev2.Portal, thread agentapi.Thread) bool {
	nc.portalMetaMu.Lock()
	defer nc.portalMetaMu.Unlock()
	meta, _ := portal.Metadata.(*PortalMetadata)
	if meta == nil {
		meta = &PortalMetadata{}
		portal.Metadata = meta
	}
	changed := false
	recipients := nc.threadRecipients(thread)
	if len(recipients) == 0 && len(meta.Recipients) > 0 {
		// An event without participant information (e.g. the from-me echo of
		// a send that raced its send.result) must never clear the stored
		// members: that is what turned rooms into "Unknown conversation".
		recipients = append([]string{}, meta.Recipients...)
	}
	if !stringSlicesEqual(meta.Recipients, recipients) {
		meta.Recipients = recipients
		changed = true
	}
	if thread.ID != 0 && meta.ThreadID != thread.ID {
		meta.ThreadID = thread.ID
		changed = true
	}
	if thread.SubID > 0 && meta.SubID != thread.SubID {
		meta.SubID = thread.SubID
		changed = true
	}
	if len(recipients) == 1 {
		other := MakeUserID(recipients[0])
		if meta.OtherUserID != other {
			meta.OtherUserID = other
			changed = true
		}
	} else if meta.OtherUserID != "" {
		meta.OtherUserID = ""
		changed = true
	}
	isGroup := len(recipients) > 1
	if meta.IsGroup != isGroup {
		meta.IsGroup = isGroup
		changed = true
	}
	return changed
}

// chatInfoForThread builds the full desired Matrix room state for a phone
// thread: name, DM/group type and member list (with the phone's own user as
// the "me" member).
func (nc *MyNetworkClient) chatInfoForThread(ctx context.Context, thread agentapi.Thread) *bridgev2.ChatInfo {
	// The agent already strips the phone's own numbers, but filter again so a
	// leaked own number can never become a room member (PLAN §5.1).
	recipients := nc.threadRecipients(thread)
	roomType := database.RoomTypeDM
	if len(recipients) != 1 {
		roomType = database.RoomTypeDefault
	}

	name := nc.nameForRecipients(ctx, recipients)

	members := &bridgev2.ChatMemberList{
		IsFull: true,
		Members: []bridgev2.ChatMember{{
			EventSender: bridgev2.EventSender{IsFromMe: true},
			Membership:  event.MembershipJoin,
		}},
	}
	for _, recipient := range recipients {
		members.Members = append(members.Members, bridgev2.ChatMember{
			EventSender: bridgev2.EventSender{Sender: MakeUserID(recipient)},
			Membership:  event.MembershipJoin,
		})
	}
	if len(recipients) == 1 {
		members.OtherUserID = MakeUserID(recipients[0])
	}

	name = strings.TrimSpace(name)
	if name == "" {
		name = PortalName("", recipients)
	}
	// Every phone-conversation room ends with a suffix ((sms) by default,
	// (sms-<label>) once the number is labeled) so SMS/MMS rooms are easy
	// to tell apart from the other bridges' rooms.
	name = strings.TrimSpace(name + " " + nc.roomNameSuffix(thread.SubID))
	return &bridgev2.ChatInfo{
		Name:        &name,
		Type:        &roomType,
		Members:     members,
		CanBackfill: true,
	}
}

// dmDisplayName looks up the ghost's contact name, falling back to the
// canonical address.
func (nc *MyNetworkClient) dmDisplayName(ctx context.Context, address string) string {
	ghostID := MakeUserID(address)
	if nc.bridge != nil {
		ghost, err := nc.bridge.GetExistingGhostByID(ctx, ghostID)
		if err == nil && ghost != nil {
			meta, _ := ghost.Metadata.(*GhostMetadata)
			return GhostDisplayName(meta, address)
		}
	}
	return CanonicalAddress(address)
}

// messageIsGroup reports whether a message belongs to a multi-participant
// thread, using the same recipient normalization as room membership.
func (nc *MyNetworkClient) messageIsGroup(msg agentapi.Message) bool {
	recipients := append([]string{}, msg.Recipients...)
	if msg.Sender != "" {
		recipients = append(recipients, msg.Sender)
	}
	return len(nc.threadRecipients(agentapi.Thread{
		ID:         msg.ThreadID,
		SubID:      msg.SubID,
		Recipients: recipients,
	})) > 1
}

// threadForMessage returns the cached thread, refreshing the cache when the
// thread is unknown, and falls back to the message's own data.
func (nc *MyNetworkClient) threadForMessage(ctx context.Context, msg agentapi.Message) agentapi.Thread {
	if thread, ok := nc.cachedThread(msg.ThreadID); ok {
		return thread
	}
	nc.refreshThreads(ctx)
	if thread, ok := nc.cachedThread(msg.ThreadID); ok {
		return thread
	}
	recipients := append([]string{}, msg.Recipients...)
	if msg.Sender != "" {
		recipients = append(recipients, msg.Sender)
	}
	return agentapi.Thread{
		ID:         msg.ThreadID,
		SubID:      msg.SubID,
		Recipients: recipients,
	}
}

func (nc *MyNetworkClient) cachedThread(threadID int64) (agentapi.Thread, bool) {
	nc.threadMu.Lock()
	defer nc.threadMu.Unlock()
	thread, ok := nc.threads[threadID]
	return thread, ok
}

// refreshThreads reloads the thread list from the agent. App-paired logins
// have no thread list endpoint; their cache is filled from message events.
func (nc *MyNetworkClient) refreshThreads(ctx context.Context) {
	if nc.device != nil {
		return
	}
	threads, err := nc.agent.Threads(ctx)
	if err != nil {
		nc.log.Warn().Err(err).Msg("Failed to refresh thread list from agent")
		return
	}
	byID := make(map[int64]agentapi.Thread, len(threads))
	for _, thread := range threads {
		byID[thread.ID] = thread
	}
	nc.threadMu.Lock()
	nc.threads = byID
	nc.threadMu.Unlock()
}

// refreshThreadsAndCatchup refreshes the thread cache and queues a catch-up
// backfill for every known portal. Called once per agent connection.
func (nc *MyNetworkClient) refreshThreadsAndCatchup(ctx context.Context) {
	nc.refreshThreads(ctx)
	nc.triggerAutomaticCatchup()
}

// rescanRecentMessages walks all threads and re-bridges messages newer than the
// stored sync position. Used when the agent's event log resets. App-paired
// logins have no history endpoint, so a reset cannot be recovered from here.
func (nc *MyNetworkClient) rescanRecentMessages(ctx context.Context) {
	if nc.device != nil {
		nc.log.Warn().Msg("App pairing transport has no history endpoint; cannot rescan after a reset")
		return
	}
	nc.refreshThreads(ctx)
	nc.threadMu.Lock()
	threads := make([]agentapi.Thread, 0, len(nc.threads))
	for _, thread := range nc.threads {
		threads = append(threads, thread)
	}
	nc.threadMu.Unlock()
	for _, thread := range threads {
		portalKey := nc.portalKeyFor(thread.ID, thread.SubID)
		start := nc.historySyncStartForPortal(ctx, portalKey)
		afterMS := start.UnixMilli()
		msgs, err := nc.agent.ThreadMessages(ctx, thread.ID, 0, afterMS, 200)
		if err != nil {
			nc.log.Warn().Err(err).Int64("thread_id", thread.ID).Msg("Failed to rescan thread messages")
			continue
		}
		for _, msg := range msgs {
			if msg.Type == 3 {
				continue
			}
			nc.bridgeAgentMessage(ctx, msg)
		}
	}
}

// buildConvertedMessage converts an agent message into Matrix parts, uploading
// attachments through the portal intent. Text is never dropped because an
// attachment is missing.
func (nc *MyNetworkClient) buildConvertedMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg agentapi.Message) (*bridgev2.ConvertedMessage, error) {
	body := strings.TrimSpace(msg.Body)
	dbMeta := &MessageMetadata{Body: body, HasAttachment: len(msg.Attachments) > 0}
	if len(msg.Attachments) == 0 {
		if body == "" {
			// Never fail the conversion for a content-less message: bridgev2
			// answers a conversion error with a "processing error" notice, and
			// because no message row is stored, every replay of the event
			// posts that notice again.
			body = noContentBody
			dbMeta.Body = body
		}
		return &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
			Type:       event.EventMessage,
			Content:    &event.MessageEventContent{MsgType: event.MsgText, Body: body},
			DBMetadata: dbMeta,
		}}}, nil
	}

	mmsID := mmsRowID(msg.ID)
	parts := make([]*bridgev2.ConvertedMessagePart, 0, len(msg.Attachments))
	attachmentNames := make([]string, 0, len(msg.Attachments))
	missingNames := make([]string, 0, len(msg.Attachments))
	bodyAssigned := false
	for _, attachment := range msg.Attachments {
		fileName := attachmentFileName(attachment)
		data, contentType, err := attachmentData(ctx, nc.agent, mmsID, attachment)
		if err != nil {
			nc.log.Warn().Err(err).
				Str("message_id", msg.ID).
				Int64("part_id", attachment.PartID).
				Msg("Failed to obtain MMS attachment; skipping")
			missingNames = append(missingNames, fileName)
			continue
		}
		if len(data) == 0 {
			missingNames = append(missingNames, fileName)
			continue
		}
		attachmentNames = append(attachmentNames, fileName)
		if contentType == "" {
			contentType = http.DetectContentType(data)
		}
		partBody := ""
		if !bodyAssigned && body != "" {
			partBody = body
			bodyAssigned = true
		}
		if partBody == "" {
			partBody = bodyFromFileName(fileName)
		}
		mxc, encryptedFile, info, err := uploadAttachmentData(ctx, intent, portal.MXID, data, fileName, contentType)
		if err != nil {
			nc.log.Warn().Err(err).Str("message_id", msg.ID).Msg("Failed to upload MMS part to Matrix; skipping attachment")
			missingNames = append(missingNames, fileName)
			continue
		}
		part := &bridgev2.ConvertedMessagePart{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType:  classifyContentTypeMessageType(contentType, true),
				Body:     partBody,
				URL:      mxc,
				File:     encryptedFile,
				Info:     info,
				FileName: fileName,
			},
		}
		if len(parts) == 0 {
			part.DBMetadata = dbMeta
		}
		parts = append(parts, part)
	}

	if len(parts) == 0 {
		// Nothing came through: mirror the phone's text (or the attachment
		// names) with one clear marker for the missing media instead of
		// failing, which would only produce a repeating error notice.
		fallback := missingMediaBody(body, missingNames)
		dbMeta.Body = fallback
		return &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
			Type:       event.EventMessage,
			Content:    &event.MessageEventContent{MsgType: event.MsgText, Body: fallback},
			DBMetadata: dbMeta,
		}}}, nil
	}
	if dbMeta.Body == "" {
		dbMeta.Body = strings.Join(attachmentNames, ", ")
	}
	return &bridgev2.ConvertedMessage{Parts: parts}, nil
}

func (nc *MyNetworkClient) portalKeyFor(threadID int64, subID int) networkid.PortalKey {
	return networkid.PortalKey{ID: MakePortalID(nc.deviceScope(), threadID, subID)}
}

// deviceScope returns the login's device scope token, derived from the serial
// reported by the agent.
func (nc *MyNetworkClient) deviceScope() string {
	if nc.login == nil {
		return "default"
	}
	meta, _ := nc.login.Metadata.(*LoginMetadata)
	if meta == nil {
		return "default"
	}
	scope := DeviceScope(meta.DeviceSerial)
	if scope == "" {
		return "default"
	}
	return scope
}

// ownNumbers returns the SIM numbers belonging to this phone.
// ownNumbers returns the phone's own numbers: the SIM numbers the app
// reported in its heartbeat plus the configured fallback list.
func (nc *MyNetworkClient) ownNumbers() []string {
	var numbers []string
	if nc.login != nil {
		if meta, _ := nc.login.Metadata.(*LoginMetadata); meta != nil {
			// Subscriptions is a map: iterate in sub ID (SIM slot) order so the
			// list is stable across restarts, and ownUserID's first entry does
			// not flip between SIMs (it is the sender ghost for own messages).
			subIDs := make([]int, 0, len(meta.Subscriptions))
			for subID := range meta.Subscriptions {
				subIDs = append(subIDs, subID)
			}
			slices.Sort(subIDs)
			for _, subID := range subIDs {
				if number := CanonicalAddress(meta.Subscriptions[subID].PhoneNumber); number != "" {
					numbers = append(numbers, number)
				}
			}
		}
	}
	if nc.connector != nil {
		numbers = append(numbers, nc.connector.ownNumbers()...)
	}
	if len(numbers) <= 1 {
		return numbers
	}
	seen := make(map[string]struct{}, len(numbers))
	unique := numbers[:0]
	for _, number := range numbers {
		if _, ok := seen[number]; ok {
			continue
		}
		seen[number] = struct{}{}
		unique = append(unique, number)
	}
	return unique
}

// ownUserID is the network user ID representing the bridge user on this login.
func (nc *MyNetworkClient) ownUserID() networkid.UserID {
	if numbers := nc.ownNumbers(); len(numbers) > 0 {
		return MakeUserID(numbers[0])
	}
	if nc.login != nil {
		return networkid.UserID(nc.login.ID)
	}
	return ""
}

func (nc *MyNetworkClient) getCursor() int64 {
	nc.cursorMu.Lock()
	defer nc.cursorMu.Unlock()
	return nc.cursor
}

func (nc *MyNetworkClient) setCursor(cursor int64) {
	nc.cursorMu.Lock()
	nc.cursor = cursor
	nc.cursorMu.Unlock()
}

// cursorAfterReset picks the cursor to continue from after the server reports
// that its retained event window no longer covers the requested cursor. The
// page carries the edge of the retained window; returning to cursor zero would
// stay below that window and reset forever on transports that cannot rescan.
func cursorAfterReset(pageCursor, current int64) (int64, bool) {
	if pageCursor > current {
		return pageCursor, true
	}
	return current, false
}

// Health returns a (possibly cached) phone health snapshot. Callers that need
// fresh state should pass a zero maxAge. App-paired logins synthesize the
// agent Health shape from the device heartbeat/liveness.
func (nc *MyNetworkClient) Health(ctx context.Context, maxAge time.Duration) (agentapi.Health, error) {
	if nc.device != nil {
		return nc.device.healthSnapshot(), nil
	}
	nc.ensureAgent()
	if nc.agent == nil {
		return agentapi.Health{}, fmt.Errorf("no agent URL configured for this login")
	}
	nc.healthMu.Lock()
	if !nc.healthCachedAt.IsZero() && time.Since(nc.healthCachedAt) <= maxAge {
		health := nc.healthCache
		nc.healthMu.Unlock()
		return health, nil
	}
	nc.healthMu.Unlock()
	health, err := nc.agent.Health(ctx)
	if err != nil {
		return health, err
	}
	nc.healthMu.Lock()
	nc.healthCache = health
	nc.healthCachedAt = time.Now()
	nc.healthMu.Unlock()
	return health, nil
}

// waitForAgentHealth polls the phone until it reports OK or the context ends.
func (nc *MyNetworkClient) waitForAgentHealth(ctx context.Context) (agentapi.Health, error) {
	var lastErr error
	for {
		health, err := nc.Health(ctx, 0)
		if err == nil && health.OK {
			return health, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("phone reports unhealthy state: %s", health.Error)
		}
		select {
		case <-ctx.Done():
			return health, fmt.Errorf("phone did not become reachable: %w", lastErr)
		case <-time.After(3 * time.Second):
		}
	}
}

func (nc *MyNetworkClient) sendBridgeState(state status.BridgeStateEvent, message string, ttl int) {
	if nc.login == nil || nc.login.BridgeState == nil {
		return
	}
	nc.login.BridgeState.Send(status.BridgeState{
		StateEvent: state,
		Message:    message,
		TTL:        ttl,
	})
}

// Disconnect implements bridgev2.NetworkAPI.
func (nc *MyNetworkClient) Disconnect() {
	nc.log.Info().Msg("MyNetworkClient Disconnect called")
	nc.startMu.Lock()
	started := nc.started
	cancel := nc.cancel
	stopCh := nc.stopCh
	loopDone := nc.loopDone
	nc.started = false
	nc.cancel = nil
	nc.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if stopCh != nil {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	}
	if !started || loopDone == nil {
		return
	}
	select {
	case <-loopDone:
	case <-time.After(3 * time.Second):
		nc.log.Warn().Msg("Agent event loop did not stop in time")
	}
}

// LogoutRemote implements bridgev2.NetworkAPI. The agent has no logout
// concept; disconnect is enough.
func (nc *MyNetworkClient) LogoutRemote(ctx context.Context) {
	nc.Disconnect()
}

// IsThisUser implements bridgev2.NetworkAPI.
func (nc *MyNetworkClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	return userID != "" && userID == nc.ownUserID()
}

// IsLoggedIn implements bridgev2.NetworkAPI. It only reports local state; the
// actual phone reachability is tracked via the event loop/bridge state.
func (nc *MyNetworkClient) IsLoggedIn() bool {
	if nc.login == nil {
		return false
	}
	meta, _ := nc.login.Metadata.(*LoginMetadata)
	if meta == nil {
		return false
	}
	if meta.Transport == LoginTransportApp {
		return strings.TrimSpace(meta.DeviceID) != "" && strings.TrimSpace(meta.TokenSHA256) != ""
	}
	return strings.TrimSpace(meta.AgentURL) != ""
}

// agentMessageTimestamp converts the agent's millisecond timestamp into a
// time.Time, falling back to the sent timestamp or now.
func agentMessageTimestamp(msg agentapi.Message) time.Time {
	ts := msg.TimestampMS
	if ts == 0 {
		ts = msg.TimestampSentMS
	}
	if ts == 0 {
		return time.Now().UTC()
	}
	return time.UnixMilli(ts).UTC()
}

// mmsRowID extracts the MMS row ID from an agent message ID of the form
// "mms:<sub_id>:<row_id>". Returns 0 for SMS IDs and malformed values.
func mmsRowID(messageID string) int64 {
	parts := strings.Split(messageID, ":")
	if len(parts) != 3 || parts[0] != "mms" {
		return 0
	}
	rowID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return 0
	}
	return rowID
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// markSentFromMatrix records message IDs the bridge just sent itself. Empty
// IDs and entries older than 30 minutes are pruned on every call.
func (nc *MyNetworkClient) markSentFromMatrix(ids ...networkid.MessageID) {
	nc.sentMu.Lock()
	defer nc.sentMu.Unlock()
	if nc.sentIDs == nil {
		nc.sentIDs = make(map[networkid.MessageID]time.Time)
	}
	now := time.Now()
	for _, id := range ids {
		if id == "" {
			continue
		}
		nc.sentIDs[id] = now
	}
	for id, ts := range nc.sentIDs {
		if now.Sub(ts) > 30*time.Minute {
			delete(nc.sentIDs, id)
		}
	}
}

// consumeSentFromMatrix reports whether an outgoing phone message is the echo
// of a message the bridge sent, and removes it from the pending set.
func (nc *MyNetworkClient) consumeSentFromMatrix(id networkid.MessageID) bool {
	if id == "" {
		return false
	}
	nc.sentMu.Lock()
	defer nc.sentMu.Unlock()
	_, ok := nc.sentIDs[id]
	if ok {
		delete(nc.sentIDs, id)
	}
	return ok
}
