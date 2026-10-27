// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"

	"maunium.net/go/mautrix/bridgev2/database"
)

// App-requested merges (`merge_threads`): the paired app's conversation list
// can select several phone threads and ask the bridge to combine their rooms.
// The request travels in the app -> bridge events batch (the app cannot reach
// the management room), the bridge resolves each thread to its portal, runs the
// same planMerge/executeMerge path as `!sms merge` and answers with a
// `merge_threads.result` queue command carrying the target room or the error.
// The command queues are in-memory, so the reply is only as durable as the
// other queue commands (24 h retention for a reconnecting device).

// mergeThreadsRequest is a parsed `merge_threads` app request.
type mergeThreadsRequest struct {
	// ThreadIDs are the selected phone threads, in the app's order.
	ThreadIDs []int64
	// TargetThread is the thread whose room becomes the merged target; 0 means
	// the first selected thread.
	TargetThread int64
	// SendID correlates the request with its result command (the app sets it).
	SendID string
}

// mergeThreadsError builds the error object of a merge_threads.result.
func mergeThreadsError(code, message string) *remoteEventError {
	return &remoteEventError{Code: code, Message: message}
}

// handleMergeThreadsEvent answers an app `merge_threads` request. Failures are
// reported to the app through the result command, never as an events-batch
// error: the app must be able to show what went wrong.
func (d *pairedDevice) handleMergeThreadsEvent(ctx context.Context, ev remoteEvent) {
	req := mergeThreadsRequest{
		ThreadIDs:    ev.ThreadIDs,
		TargetThread: ev.TargetThread,
		SendID:       ev.SendID,
	}
	roomID, resultErr := d.executeMergeThreadsRequest(ctx, req)
	d.enqueue(pairCommand{
		Type:         pairCommandMergeResult,
		SendID:       req.SendID,
		ThreadIDs:    req.ThreadIDs,
		TargetThread: req.TargetThread,
		RoomID:       roomID,
		Error:        resultErr,
	})
}

// executeMergeThreadsRequest runs the requested merge against the device's
// login and returns the target room ID or the error to report to the app.
func (d *pairedDevice) executeMergeThreadsRequest(ctx context.Context, req mergeThreadsRequest) (string, *remoteEventError) {
	runtime := d.runtimeHandler()
	if runtime == nil || runtime.bridge == nil || runtime.login == nil {
		return "", mergeThreadsError(pairMergeErrorInternal, "the paired login is not ready yet")
	}
	if len(req.ThreadIDs) < 2 {
		return "", mergeThreadsError(pairMergeErrorInvalid, "select at least two conversations to merge")
	}
	rooms, target, err := resolveMergeThreadsRooms(ctx, runtime, req)
	if err != nil {
		return "", mergeThreadsError(pairMergeErrorInvalid, err.Error())
	}
	plan, err := planMerge(rooms, target)
	if err != nil {
		return "", mergeThreadsError(pairMergeErrorInvalid, err.Error())
	}
	if len(plan.Sources) == 0 {
		// Every selected room is already merged into the target: report the
		// target instead of running the merge (and its room state rewrite)
		// again.
		return string(plan.Rooms[plan.Target].RoomID), nil
	}
	// The app's confirmation dialog is the `--yes` of the command path: the
	// user saw the participant list and the tombstone warning before the
	// request was sent, so the confirmation gates of planMerge do not need to
	// be repeated here.
	if err := executeMerge(ctx, runtime.bridge, runtime.login.User, runtime.login, plan); err != nil {
		runtime.log.Warn().Err(err).
			Interface("thread_ids", req.ThreadIDs).
			Int64("target_thread", req.TargetThread).
			Msg("App-requested merge failed")
		return "", mergeThreadsError(pairMergeErrorInternal, err.Error())
	}
	return string(plan.Rooms[plan.Target].RoomID), nil
}

// resolveMergeThreadsRooms maps the requested phone threads to the login's
// portals (the same thread -> portal derivation thread.updated uses) and builds
// the rooms planMerge validates. It returns the index of the target room.
func resolveMergeThreadsRooms(ctx context.Context, nc *MyNetworkClient, req mergeThreadsRequest) ([]mergeRoom, int, error) {
	if nc == nil || nc.bridge == nil || nc.login == nil {
		return nil, 0, fmt.Errorf("bridge is not available")
	}
	rooms := make([]mergeRoom, 0, len(req.ThreadIDs))
	target := -1
	for i, threadID := range req.ThreadIDs {
		if threadID <= 0 {
			return nil, 0, fmt.Errorf("thread %d is not a valid phone thread", threadID)
		}
		portal, _, ok := nc.findPortalForThread(ctx, threadID)
		if !ok {
			return nil, 0, fmt.Errorf("no bridged room found for phone thread %d", threadID)
		}
		room := mergeRoomFromPortalRow(portal.Portal, mergeRoom{Input: fmt.Sprintf("thread %d", threadID)})
		room.Portal = portal
		room.IsSpace = portal.RoomType == database.RoomTypeSpace || roomIsSpace(ctx, nc.bridge, portal.MXID)
		if user := nc.login.User; user != nil && user.ManagementRoom != "" {
			room.IsManagementRoom = portal.MXID != "" && portal.MXID == user.ManagementRoom
		}
		rooms = append(rooms, room)
		if threadID == req.TargetThread {
			target = i
		}
	}
	if req.TargetThread != 0 && target < 0 {
		return nil, 0, fmt.Errorf("the target thread %d is not part of the selection", req.TargetThread)
	}
	if target < 0 {
		target = 0
	}
	return rooms, target, nil
}
