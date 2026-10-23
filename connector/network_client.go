// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"net/http"
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
// long-polls the agent event feed, bridges messages into Matrix rooms and
// implements the Matrix -> phone direction.
type MyNetworkClient struct {
	log       zerolog.Logger
	bridge    *bridgev2.Bridge
	login     *bridgev2.UserLogin
	connector *MyConnector
	agent     *AgentClient

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
	nc.ensureAgent()
	return nc
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

	nc.ensureAgent()
	if nc.agent == nil || strings.TrimSpace(nc.agent.BaseURL) == "" {
		nc.log.Error().Msg("Cannot start agent event loop: no agent URL configured")
		nc.sendBridgeState(status.StateBadCredentials, "The login has no agent URL configured; log in again.", 300)
		close(loopDone)
		return
	}
	go nc.eventLoop(loopCtx, stopCh, loopDone)
}

func (nc *MyNetworkClient) eventLoop(ctx context.Context, stopCh <-chan struct{}, loopDone chan<- struct{}) {
	defer close(loopDone)
	log := nc.log.With().Str("component", "agent-event-loop").Logger()
	backoff := time.Second
	connected := false
	for {
		select {
		case <-stopCh:
			return
		default:
		}

		events, err := nc.agent.Events(ctx, nc.getCursor(), agentEventPollTimeoutMS)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn().Err(err).Msg("Agent event poll failed")
			if connected {
				connected = false
				nc.sendBridgeState(status.StateTransientDisconnect, fmt.Sprintf("Agent unreachable: %v", err), int(backoff.Seconds())+5)
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
		if !connected {
			connected = true
			nc.log.Info().Msg("Connected to android-sms-mms agent event feed")
			nc.sendBridgeState(status.StateConnected, "", 0)
			nc.refreshThreadsAndCatchup(ctx)
		}

		if events.Reset {
			log.Warn().Msg("Agent event log was reset; re-scanning recent messages")
			nc.setCursor(0)
			nc.rescanRecentMessages(ctx)
			continue
		}
		nc.setCursor(events.Cursor)
		for _, ev := range events.Events {
			nc.handleAgentEvent(ctx, ev)
		}
	}
}

func (nc *MyNetworkClient) handleAgentEvent(ctx context.Context, ev agentapi.Event) {
	switch ev.Type {
	case "message.new", "mms.downloaded":
		nc.handleAgentMessage(ctx, ev.MessageID)
	case "message.updated":
		nc.handleAgentMessageUpdate(ctx, ev.MessageID)
	case "thread.updated":
		nc.handleAgentThreadUpdate(ctx, ev.ThreadID)
	default:
		nc.log.Debug().Str("event_type", ev.Type).Msg("Ignoring unknown agent event type")
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
func (nc *MyNetworkClient) handleAgentThreadUpdate(ctx context.Context, threadID int64) {
	nc.refreshThreads(ctx)
	thread, ok := nc.cachedThread(threadID)
	if !ok {
		return
	}
	portalKey := nc.portalKeyFor(thread.ID, thread.SubID)
	portal, err := nc.bridge.GetExistingPortalByKey(ctx, portalKey)
	if err != nil || portal == nil || portal.MXID == "" {
		return
	}
	chatInfo := nc.chatInfoForThread(ctx, thread)
	if nc.applyPortalThread(portal, thread) {
		if err := portal.Save(ctx); err != nil {
			nc.log.Err(err).Str("portal_id", string(portal.ID)).Msg("Failed to save updated portal metadata")
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
	portalKey := nc.portalKeyFor(msg.ThreadID, msg.SubID)
	fromMe := msg.Sender == ""
	var sender networkid.UserID
	if !fromMe {
		sender = MakeUserID(msg.Sender)
	}

	// Suppress the phone's copy of messages the bridge sent itself.
	if fromMe && nc.consumeSentFromMatrix(networkid.MessageID(msg.ID)) {
		nc.log.Debug().Str("message_id", msg.ID).Msg("Ignoring echo of a message sent from Matrix")
		return
	}

	// iPhone tapbacks arrive as text; convert them to reactions when the
	// referenced message can be found, otherwise fall through and forward the
	// text unchanged. Outgoing text is never treated as a tapback: it is
	// regular text typed on the phone.
	if !fromMe {
		if tb, ok := parseTapback(msg.Body); ok {
			if nc.handleIncomingTapback(ctx, portalKey, sender, tb, agentMessageTimestamp(msg)) {
				return
			}
		}
	}

	thread := nc.threadForMessage(ctx, msg)
	chatInfo := nc.chatInfoForThread(ctx, thread)
	nc.ensurePortal(ctx, portalKey, chatInfo, thread)

	timestamp := agentMessageTimestamp(msg)
	evt := &simplevent.Message[agentapi.Message]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventMessage,
			PortalKey: portalKey,
			Sender: bridgev2.EventSender{
				Sender:   sender,
				IsFromMe: fromMe,
			},
			Timestamp: timestamp,
			PostHandleFunc: func(ctx context.Context, portal *bridgev2.Portal) {
				nc.markPortalHistoryProgress(ctx, portal, timestamp, msg.ID, "")
			},
		},
		Data: msg,
		ID:   networkid.MessageID(msg.ID),
		ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data agentapi.Message) (*bridgev2.ConvertedMessage, error) {
			return nc.buildConvertedMessage(ctx, portal, intent, data)
		},
	}
	nc.bridge.QueueRemoteEvent(nc.login, evt)
	nc.indexMessage(portalKey, indexedMessage{
		ID:            networkid.MessageID(msg.ID),
		Body:          msg.Body,
		HasAttachment: len(msg.Attachments) > 0,
	})
	nc.log.Info().
		Str("portal_id", string(portalKey.ID)).
		Str("message_id", msg.ID).
		Bool("is_from_me", fromMe).
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
	if changed {
		nc.bridge.QueueRemoteEvent(nc.login, &simplevent.ChatInfoChange{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatInfoChange,
				PortalKey: portalKey,
				Timestamp: time.Now(),
			},
			ChatInfoChange: &bridgev2.ChatInfoChange{ChatInfo: chatInfo},
		})
	}
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
	recipients := filterOwnNumbers(thread.Recipients, nc.ownNumbers())
	if !stringSlicesEqual(meta.Recipients, recipients) {
		meta.Recipients = recipients
		changed = true
	}
	if thread.ID != 0 && meta.ThreadID != thread.ID {
		meta.ThreadID = thread.ID
		changed = true
	}
	if thread.SubID != 0 && meta.SubID != thread.SubID {
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
	recipients := filterOwnNumbers(thread.Recipients, nc.ownNumbers())
	roomType := database.RoomTypeDM
	if len(recipients) != 1 {
		roomType = database.RoomTypeDefault
	}

	var name string
	if len(recipients) == 1 {
		name = nc.dmDisplayName(ctx, recipients[0])
	} else {
		name = PortalName("", recipients, nc.ownNumbers()...)
	}

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

// refreshThreads reloads the thread list from the agent.
func (nc *MyNetworkClient) refreshThreads(ctx context.Context) {
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
// stored sync position. Used when the agent's event log resets.
func (nc *MyNetworkClient) rescanRecentMessages(ctx context.Context) {
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
			return nil, fmt.Errorf("message %s has no content", msg.ID)
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
	bodyAssigned := false
	for _, attachment := range msg.Attachments {
		if attachment.PartID == 0 || mmsID == 0 {
			continue
		}
		fileName := attachmentFileName(attachment)
		attachmentNames = append(attachmentNames, fileName)
		data, contentType, err := nc.agent.PartBytes(ctx, mmsID, attachment.PartID)
		if err != nil {
			nc.log.Warn().Err(err).
				Str("message_id", msg.ID).
				Int64("part_id", attachment.PartID).
				Msg("Failed to download MMS part; skipping attachment")
			continue
		}
		if len(data) == 0 {
			continue
		}
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
		fallback := body
		if fallback == "" {
			if len(attachmentNames) > 0 {
				fallback = strings.Join(attachmentNames, ", ")
			} else {
				fallback = "[MMS attachment could not be downloaded]"
			}
		}
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
func (nc *MyNetworkClient) ownNumbers() []string {
	if nc.login == nil {
		return nil
	}
	meta, _ := nc.login.Metadata.(*LoginMetadata)
	if meta == nil || len(meta.Subscriptions) == 0 {
		return nil
	}
	numbers := make([]string, 0, len(meta.Subscriptions))
	for _, sub := range meta.Subscriptions {
		if number := CanonicalAddress(sub.PhoneNumber); number != "" {
			numbers = append(numbers, number)
		}
	}
	return numbers
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

// Health returns a (possibly cached) agent health snapshot. Callers that need
// fresh state should pass a zero maxAge.
func (nc *MyNetworkClient) Health(ctx context.Context, maxAge time.Duration) (agentapi.Health, error) {
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

// waitForAgentHealth polls the agent until it reports OK or the context ends.
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
			lastErr = fmt.Errorf("agent reports unhealthy state: %s", health.Error)
		}
		select {
		case <-ctx.Done():
			return health, fmt.Errorf("agent did not become healthy: %w", lastErr)
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
// actual agent reachability is tracked via the event loop/bridge state.
func (nc *MyNetworkClient) IsLoggedIn() bool {
	if nc.login == nil {
		return false
	}
	meta, _ := nc.login.Metadata.(*LoginMetadata)
	return meta != nil && strings.TrimSpace(meta.AgentURL) != ""
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
