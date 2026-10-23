// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"testing"
	"time"
)

func TestShouldUpdateHistoryProgress(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	later := now.Add(time.Minute)

	tests := []struct {
		name       string
		existingTS *time.Time
		existingID string
		newTS      time.Time
		newID      string
		want       bool
	}{
		{name: "zero timestamp ignored", newTS: time.Time{}, want: false},
		{name: "empty history updates", newTS: now, want: true},
		{name: "newer timestamp updates", existingTS: &now, newTS: later, want: true},
		{name: "older timestamp ignored", existingTS: &later, newTS: now, want: false},
		{name: "same timestamp fills missing id", existingTS: &now, newTS: now, newID: "msg-1", want: true},
		{name: "same timestamp keeps existing id", existingTS: &now, existingID: "msg-0", newTS: now, newID: "msg-1", want: false},
	}

	for _, tt := range tests {
		if got := shouldUpdateHistoryProgress(tt.existingTS, tt.existingID, tt.newTS, tt.newID); got != tt.want {
			t.Fatalf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
}
