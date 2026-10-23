// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"

	"maunium.net/go/mautrix/id"
)

func TestChooseManagementRoom(t *testing.T) {
	const (
		roomA = id.RoomID("!roomA:example.com")
		roomB = id.RoomID("!roomB:example.com")
	)

	tests := []struct {
		name                string
		storedRoom          id.RoomID
		currentRoom         id.RoomID
		storedLive          bool
		currentLive         bool
		currentNewerOrEqual bool
		wantAdopt           bool
		wantWarn            bool
		wantReason          string
	}{
		{
			name:                "same room already current",
			storedRoom:          roomA,
			currentRoom:         roomA,
			storedLive:          true,
			currentLive:         true,
			currentNewerOrEqual: true,
			wantAdopt:           false,
			wantWarn:            false,
			wantReason:          "already_current",
		},
		{
			name:                "current room not live",
			storedRoom:          roomA,
			currentRoom:         roomB,
			storedLive:          true,
			currentLive:         false,
			currentNewerOrEqual: true,
			wantAdopt:           false,
			wantWarn:            false,
			wantReason:          "current_not_live",
		},
		{
			name:                "stored room empty initial adoption",
			storedRoom:          "",
			currentRoom:         roomA,
			storedLive:          false,
			currentLive:         true,
			currentNewerOrEqual: true,
			wantAdopt:           true,
			wantWarn:            false,
			wantReason:          "stored_empty_or_dead",
		},
		{
			name:                "stored room dead stale adoption",
			storedRoom:          roomA,
			currentRoom:         roomB,
			storedLive:          false,
			currentLive:         true,
			currentNewerOrEqual: true,
			wantAdopt:           true,
			wantWarn:            false,
			wantReason:          "stored_empty_or_dead",
		},
		{
			name:                "both live and current is newer",
			storedRoom:          roomA,
			currentRoom:         roomB,
			storedLive:          true,
			currentLive:         true,
			currentNewerOrEqual: true,
			wantAdopt:           true,
			wantWarn:            true,
			wantReason:          "current_is_newer_candidate",
		},
		{
			name:                "both live and stored is newer",
			storedRoom:          roomA,
			currentRoom:         roomB,
			storedLive:          true,
			currentLive:         true,
			currentNewerOrEqual: false,
			wantAdopt:           false,
			wantWarn:            true,
			wantReason:          "stored_is_newer_candidate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adopt, warn, reason := chooseManagementRoom(tt.storedRoom, tt.currentRoom, tt.storedLive, tt.currentLive, tt.currentNewerOrEqual)
			if adopt != tt.wantAdopt {
				t.Errorf("chooseManagementRoom() adopt = %v, want %v", adopt, tt.wantAdopt)
			}
			if warn != tt.wantWarn {
				t.Errorf("chooseManagementRoom() warn = %v, want %v", warn, tt.wantWarn)
			}
			if reason != tt.wantReason {
				t.Errorf("chooseManagementRoom() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}
