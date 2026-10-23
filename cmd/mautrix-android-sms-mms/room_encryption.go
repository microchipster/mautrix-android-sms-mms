// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// roomEncryptionGate verifies that bot rooms are encrypted with megolm before
// the bridge routes or sends anything into them. Room encryption on the
// Matrix leg is required between users and the bot, so the gate fails closed:
// any failure to prove encryption blocks all bridge activity in the room.
type roomEncryptionGate struct {
	mu      sync.Mutex
	enabled map[id.RoomID]bool
}

func newRoomEncryptionGate() *roomEncryptionGate {
	return &roomEncryptionGate{enabled: make(map[id.RoomID]bool)}
}

// ensureBotRoomEncrypted checks the live m.room.encryption state, establishes
// megolm encryption when the room has none (if permitted), updates the bridge
// state cache, and remembers the verified room. It returns establishedNow=true
// when the room had no encryption state and the bridge enabled it.
func (g *roomEncryptionGate) ensureBotRoomEncrypted(ctx context.Context, main *mxmain.BridgeMain, roomID id.RoomID) (establishedNow bool, err error) {
	g.mu.Lock()
	verified := g.enabled[roomID]
	g.mu.Unlock()
	if verified {
		return false, nil
	}

	if main == nil || main.Matrix == nil || main.Matrix.Crypto == nil {
		return false, fmt.Errorf("bridge matrix encryption is unavailable")
	}
	if main.Matrix.Config == nil || !main.Matrix.Config.Encryption.Allow {
		return false, fmt.Errorf("bridge encryption is disabled in configuration")
	}

	content, err := g.liveEncryptionEvent(ctx, main, roomID)
	if err != nil {
		return false, err
	}
	if content == nil {
		content = &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}
		if _, err := main.Matrix.Bot.SendStateEvent(ctx, roomID, event.StateEncryption, "", content); err != nil {
			return false, fmt.Errorf("failed to enable encryption in room %s: %w", roomID, err)
		}
		recheck, err := g.liveEncryptionEvent(ctx, main, roomID)
		if err != nil {
			return false, err
		}
		if recheck == nil {
			return false, fmt.Errorf("encryption state did not appear in room %s after enabling it", roomID)
		}
		content = recheck
		establishedNow = true
	}

	// Update the local state cache so every subsequent bot send into this room
	// is encrypted instead of falling back to plaintext.
	if err := main.Matrix.StateStore.SetEncryptionEvent(ctx, roomID, content); err != nil {
		return false, fmt.Errorf("failed to cache encryption state for room %s: %w", roomID, err)
	}

	g.mu.Lock()
	g.enabled[roomID] = true
	g.mu.Unlock()
	return establishedNow, nil
}

// liveEncryptionEvent fetches the current m.room.encryption state directly
// from the homeserver and validates its algorithm. A missing state event is
// reported as (nil, nil) so callers can establish encryption.
func (g *roomEncryptionGate) liveEncryptionEvent(ctx context.Context, main *mxmain.BridgeMain, roomID id.RoomID) (*event.EncryptionEventContent, error) {
	evt, err := main.Matrix.GetStateEvent(ctx, roomID, event.StateEncryption, "")
	if err != nil {
		if isNotFoundError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to fetch encryption state for room %s: %w", roomID, err)
	}
	if evt == nil {
		return nil, nil
	}
	content := evt.Content.AsEncryption()
	if content == nil || content.Algorithm == "" {
		return nil, fmt.Errorf("room %s has a malformed m.room.encryption event", roomID)
	}
	if err := validateEncryptionContent(content); err != nil {
		return nil, fmt.Errorf("room %s: %w", roomID, err)
	}
	return content, nil
}

// validateEncryptionContent rejects any encryption algorithm other than
// megolm v1, so unsupported room encryption never gets treated as protected.
func validateEncryptionContent(content *event.EncryptionEventContent) error {
	if content == nil {
		return fmt.Errorf("missing encryption state")
	}
	if content.Algorithm != id.AlgorithmMegolmV1 {
		return fmt.Errorf("unsupported encryption algorithm %q; only %q is supported", content.Algorithm, id.AlgorithmMegolmV1)
	}
	return nil
}

// isNotFoundError reports whether err is an M_NOT_FOUND response, which the
// homeserver uses when a state event has never been set.
func isNotFoundError(err error) bool {
	var httpErr mautrix.HTTPError
	if errors.As(err, &httpErr) && httpErr.RespError != nil && httpErr.RespError.ErrCode == mautrix.MNotFound.ErrCode {
		return true
	}
	return false
}

// isPermissionError reports whether err indicates the bot lacks permission in
// the room (e.g. sending m.room.encryption requires the relevant power level).
func isPermissionError(err error) bool {
	var httpErr mautrix.HTTPError
	if errors.As(err, &httpErr) && httpErr.RespError != nil && httpErr.RespError.ErrCode == mautrix.MForbidden.ErrCode {
		return true
	}
	return false
}
