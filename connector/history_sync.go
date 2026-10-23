// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

const (
	historySyncOverlap = time.Second
)

func shouldUpdateHistoryProgress(existingTS *time.Time, existingID string, newTS time.Time, newID string) bool {
	if newTS.IsZero() {
		return false
	}
	if existingTS == nil || existingTS.IsZero() {
		return true
	}
	if newTS.After(*existingTS) {
		return true
	}
	if newTS.Equal(*existingTS) && newID != "" && existingID == "" {
		return true
	}
	return false
}

func (nc *MyNetworkClient) markPortalHistoryProgress(ctx context.Context, portal *bridgev2.Portal, timestamp time.Time, messageID string, serverMessageID string) {
	if portal == nil || timestamp.IsZero() {
		return
	}
	if messageID != "" && portal.Bridge != nil && portal.Bridge.DB != nil {
		parts, err := portal.Bridge.DB.Message.GetAllPartsByID(ctx, portal.Receiver, networkid.MessageID(messageID))
		if err != nil || len(parts) == 0 {
			nc.log.Debug().
				Str("portal_id", string(portal.ID)).
				Str("message_id", messageID).
				Msg("Message was not persisted to Matrix DB; skipping history sync progress update")
			return
		}
	}
	nc.portalMetaMu.Lock()
	meta, _ := portal.Metadata.(*PortalMetadata)
	if meta == nil {
		meta = &PortalMetadata{}
		portal.Metadata = meta
	}
	timestamp = timestamp.UTC()
	if shouldUpdateHistoryProgress(meta.LastSyncedAt, meta.LastMessageID, timestamp, messageID) {
		meta.LastSyncedAt = &timestamp
		if messageID != "" {
			meta.LastMessageID = messageID
		}
		if serverMessageID != "" {
			meta.LastServerMessageID = serverMessageID
		}
		if err := portal.Save(ctx); err != nil {
			nc.log.Err(err).Str("portal_id", string(portal.ID)).Msg("Failed to save portal history progress")
		}
	}
	nc.portalMetaMu.Unlock()
	loginMeta, _ := nc.login.Metadata.(*LoginMetadata)
	if loginMeta != nil && shouldUpdateHistoryProgress(loginMeta.LastSyncAt, "", timestamp, "") {
		loginMeta.LastSyncAt = &timestamp
		if err := nc.login.Save(ctx); err != nil {
			nc.log.Err(err).Msg("Failed to save login history progress")
		}
	}
}

// historySyncStartForPortal returns the timestamp the next backfill of a
// portal should start at (inclusive-ish, with a small overlap so no message is
// skipped): an explicit sync override, the portal's last synced position, the
// last bridged message, or the configurable default window.
func (nc *MyNetworkClient) historySyncStartForPortal(ctx context.Context, portalKey networkid.PortalKey) time.Time {
	nc.syncMu.Lock()
	override, hasOverride := nc.syncStartOverride[portalKey]
	nc.syncMu.Unlock()
	if hasOverride {
		return override
	}
	if nc.bridge == nil {
		return nc.defaultAutomaticCatchupStart()
	}
	portal, err := nc.bridge.GetExistingPortalByKey(ctx, portalKey)
	if err != nil {
		nc.log.Debug().Err(err).Str("portal_id", string(portalKey.ID)).Msg("Failed to load portal metadata for history sync")
	}
	if portal != nil {
		nc.portalMetaMu.Lock()
		if meta, ok := portal.Metadata.(*PortalMetadata); ok && meta != nil && meta.LastSyncedAt != nil && !meta.LastSyncedAt.IsZero() {
			start := meta.LastSyncedAt.UTC().Add(-historySyncOverlap)
			nc.portalMetaMu.Unlock()
			return start
		}
		nc.portalMetaMu.Unlock()
	}
	msg, err := nc.bridge.DB.Message.GetLastNonFakePartAtOrBeforeTime(ctx, portalKey, time.Now().UTC())
	if err == nil && msg != nil && !msg.Timestamp.IsZero() {
		return msg.Timestamp.UTC().Add(-historySyncOverlap)
	}
	return nc.defaultAutomaticCatchupStart()
}

// defaultAutomaticCatchupStart returns now minus the configured default
// backfill window (history_default_days, default 30 days).
func (nc *MyNetworkClient) defaultAutomaticCatchupStart() time.Time {
	return time.Now().UTC().Add(-nc.connector.historyDefaultDays())
}

func (nc *MyNetworkClient) historySyncStartForDMs() time.Time {
	loginMeta, _ := nc.login.Metadata.(*LoginMetadata)
	if loginMeta != nil && loginMeta.LastSyncAt != nil && !loginMeta.LastSyncAt.IsZero() {
		return loginMeta.LastSyncAt.UTC().Add(-historySyncOverlap)
	}
	return nc.defaultAutomaticCatchupStart()
}

func (nc *MyNetworkClient) triggerAutomaticCatchup() {
	go nc.runAutomaticCatchup(context.Background())
}

// runAutomaticCatchup queues a backfill for every connected portal of this
// login. It is deliberately conservative: it only makes sure a backfill task
// exists (or is marked undone) for portals that already have a Matrix room, and
// FetchMessages decides how far back to go from the stored progress.
func (nc *MyNetworkClient) runAutomaticCatchup(ctx context.Context) {
	if nc.bridge == nil || nc.login == nil {
		return
	}
	portals, err := nc.bridge.DB.UserPortal.GetAllForLogin(ctx, nc.login.UserLogin)
	if err != nil {
		nc.log.Err(err).Msg("Failed to enumerate portals for automatic catchup")
		return
	}
	queued := 0
	for _, userPortal := range portals {
		if userPortal == nil || userPortal.Portal.ID == "" {
			continue
		}
		portal, err := nc.bridge.GetExistingPortalByKey(ctx, userPortal.Portal)
		if err != nil || portal == nil || portal.MXID == "" {
			continue
		}
		if err := nc.bridge.DB.BackfillTask.EnsureExists(ctx, portal.PortalKey, nc.login.ID); err != nil {
			nc.log.Err(err).Str("portal_id", string(portal.ID)).Msg("Failed to ensure backfill task for portal")
			continue
		}
		queued++
	}
	if queued > 0 {
		nc.bridge.WakeupBackfillQueue()
		nc.log.Info().Int("portal_count", queued).Msg("Queued automatic catchup backfills")
	}
}

// setSyncStartOverride pins the backfill start time for a portal. An epoch
// timestamp means "full history".
func (nc *MyNetworkClient) setSyncStartOverride(portalKey networkid.PortalKey, start time.Time) {
	nc.syncMu.Lock()
	defer nc.syncMu.Unlock()
	if nc.syncStartOverride == nil {
		nc.syncStartOverride = make(map[networkid.PortalKey]time.Time)
	}
	nc.syncStartOverride[portalKey] = start
}

// clearSyncStartOverride drops any pending override (e.g. after a full sync
// completed and progress was persisted).
func (nc *MyNetworkClient) clearSyncStartOverride(portalKey networkid.PortalKey) {
	nc.syncMu.Lock()
	defer nc.syncMu.Unlock()
	delete(nc.syncStartOverride, portalKey)
}

// StartFullSync resets the stored history progress of all portals of this
// login and re-queues their backfill tasks. `full` means "from the beginning
// of the phone's history", otherwise lookback limits how far back to go.
func (nc *MyNetworkClient) StartFullSync(ctx context.Context, notify func(string), full bool, lookback time.Duration) error {
	if nc.bridge == nil || nc.login == nil {
		return fmt.Errorf("network client is not attached to a bridge login")
	}
	portals, err := nc.bridge.DB.UserPortal.GetAllForLogin(ctx, nc.login.UserLogin)
	if err != nil {
		return fmt.Errorf("failed to list portals: %w", err)
	}
	if len(portals) == 0 {
		if notify != nil {
			notify("No portals found for this login yet; nothing to sync.")
		}
		return nil
	}
	if full {
		lookback = 0
	} else if lookback <= 0 {
		lookback = nc.connector.historyDefaultDays()
	}
	var start time.Time
	if full {
		start = time.Unix(0, 0).UTC()
	} else {
		start = time.Now().UTC().Add(-lookback)
	}

	queued := 0
	for _, userPortal := range portals {
		if userPortal == nil || userPortal.Portal.ID == "" {
			continue
		}
		portal, err := nc.bridge.GetExistingPortalByKey(ctx, userPortal.Portal)
		if err != nil || portal == nil {
			continue
		}
		nc.portalMetaMu.Lock()
		meta, _ := portal.Metadata.(*PortalMetadata)
		if meta != nil {
			meta.LastSyncedAt = nil
			meta.LastMessageID = ""
		} else {
			meta = &PortalMetadata{}
			portal.Metadata = meta
		}
		nc.portalMetaMu.Unlock()
		if err := portal.Save(ctx); err != nil {
			nc.log.Err(err).Str("portal_id", string(portal.ID)).Msg("Failed to reset portal history progress")
			continue
		}
		nc.setSyncStartOverride(portal.PortalKey, start)
		if portal.MXID != "" {
			if err := nc.bridge.DB.BackfillTask.MarkNotDone(ctx, portal.PortalKey, nc.login.ID); err != nil {
				nc.log.Warn().Err(err).Str("portal_id", string(portal.ID)).Msg("Failed to mark backfill task not done")
			}
			if err := nc.bridge.DB.BackfillTask.EnsureExists(ctx, portal.PortalKey, nc.login.ID); err != nil {
				nc.log.Warn().Err(err).Str("portal_id", string(portal.ID)).Msg("Failed to ensure backfill task")
				continue
			}
		}
		queued++
	}
	nc.bridge.WakeupBackfillQueue()
	loginMeta, _ := nc.login.Metadata.(*LoginMetadata)
	if loginMeta != nil {
		loginMeta.LastSyncAt = nil
		_ = nc.login.Save(ctx)
	}
	if notify != nil {
		window := "the full phone history"
		if !full {
			window = fmt.Sprintf("the last %s", lookback)
		}
		notify(fmt.Sprintf("Queued history sync for %d room(s) from %s.", queued, window))
	}
	nc.log.Info().Int("portal_count", queued).Bool("full", full).Msg("Queued full history sync")
	return nil
}

// ensureBackfillTaskForPortal makes sure a backfill task exists for a portal
// that was just created or rebound.
func (nc *MyNetworkClient) ensureBackfillTaskForPortal(ctx context.Context, portal *bridgev2.Portal) {
	if nc.bridge == nil || nc.login == nil || portal == nil || portal.MXID == "" {
		return
	}
	if err := nc.bridge.DB.BackfillTask.EnsureExists(ctx, portal.PortalKey, nc.login.ID); err != nil {
		nc.log.Warn().Err(err).Str("portal_id", string(portal.ID)).Msg("Failed to ensure backfill task for portal")
		return
	}
	nc.bridge.WakeupBackfillQueue()
}

// FetchMessages implements bridgev2.BackfillingNetworkAPI by paging backwards
// through the phone's thread history. App-paired logins have no history
// endpoint yet, so backfill is a no-op for them.
func (nc *MyNetworkClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	portal := params.Portal
	if portal == nil {
		return &bridgev2.FetchMessagesResponse{HasMore: false}, nil
	}
	if nc.device != nil {
		return &bridgev2.FetchMessagesResponse{HasMore: false}, nil
	}
	if params.ThreadRoot != "" {
		// SMS/MMS threads are not Matrix threads; refuse politely.
		return &bridgev2.FetchMessagesResponse{HasMore: false}, nil
	}
	threadID, _, _ := nc.portalThreadSnapshot(portal)
	if threadID == 0 {
		if _, _, parsedThreadID, ok := ParsePortalID(portal.ID); ok {
			threadID = parsedThreadID
		} else {
			return &bridgev2.FetchMessagesResponse{HasMore: false}, nil
		}
	}

	start := nc.historySyncStartForPortal(ctx, portal.PortalKey)
	var beforeMS int64
	if params.AnchorMessage != nil && !params.AnchorMessage.Timestamp.IsZero() {
		beforeMS = params.AnchorMessage.Timestamp.UnixMilli()
	}
	count := params.Count
	if count <= 0 {
		count = 50
	}

	msgs, err := nc.agent.ThreadMessages(ctx, threadID, beforeMS, 0, count)
	if err != nil {
		return nil, fmt.Errorf("fetch thread messages for backfill: %w", err)
	}
	if len(msgs) == 0 {
		nc.clearSyncStartOverride(portal.PortalKey)
		return &bridgev2.FetchMessagesResponse{HasMore: false}, nil
	}

	startMS := start.UnixMilli()
	out := make([]*bridgev2.BackfillMessage, 0, len(msgs))
	var oldestFetchedMS int64
	for _, msg := range msgs {
		if msg.Type == 3 {
			continue
		}
		if oldestFetchedMS == 0 || msg.TimestampMS < oldestFetchedMS {
			oldestFetchedMS = msg.TimestampMS
		}
		if startMS > 0 && msg.TimestampMS < startMS {
			continue
		}
		backfillMsg := nc.makeBackfillMessage(ctx, portal, msg)
		if backfillMsg == nil {
			continue
		}
		out = append(out, backfillMsg)
	}

	hasMore := len(msgs) >= count
	if oldestFetchedMS > 0 && oldestFetchedMS <= startMS {
		hasMore = false
	}
	if !hasMore {
		nc.clearSyncStartOverride(portal.PortalKey)
	}
	return &bridgev2.FetchMessagesResponse{
		Messages: out,
		HasMore:  hasMore,
		Forward:  false,
	}, nil
}

// makeBackfillMessage converts an agent message into a bridgev2 backfill
// message, including uploading any attachments. Returns nil when the message
// has no convertible content.
func (nc *MyNetworkClient) makeBackfillMessage(ctx context.Context, portal *bridgev2.Portal, msg agentapi.Message) *bridgev2.BackfillMessage {
	sender := bridgev2.EventSender{
		Sender:   MakeUserID(msg.Sender),
		IsFromMe: msg.Sender == "",
	}
	intent, ok := portal.GetIntentFor(ctx, sender, nc.login, bridgev2.RemoteEventMessage)
	if !ok || intent == nil {
		return nil
	}
	converted, err := nc.buildConvertedMessage(ctx, portal, intent, msg)
	if err != nil {
		nc.log.Debug().Err(err).Str("message_id", msg.ID).Msg("Skipping unconvertible message during backfill")
		return nil
	}
	timestamp := agentMessageTimestamp(msg)
	return &bridgev2.BackfillMessage{
		ConvertedMessage: converted,
		Sender:           sender,
		ID:               networkid.MessageID(msg.ID),
		Timestamp:        timestamp,
	}
}

var _ bridgev2.BackfillingNetworkAPI = (*MyNetworkClient)(nil)
