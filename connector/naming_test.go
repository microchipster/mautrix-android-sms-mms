// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"testing"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

func TestPortalName(t *testing.T) {
	tests := []struct {
		name       string
		title      string
		recipients []string
		ownNumbers []string
		want       string
	}{
		{
			name:       "explicit title wins",
			title:      "Family",
			recipients: []string{"+15550001111"},
			want:       "Family",
		},
		{
			name:       "dm uses canonical address",
			recipients: []string{"555-000-1111"},
			want:       "+15550001111",
		},
		{
			name:       "dm drops own number",
			recipients: []string{"+15550001111", "+15559998888"},
			ownNumbers: []string{"+15559998888"},
			want:       "+15550001111",
		},
		{
			name:       "group joins participants sorted and deduplicated",
			recipients: []string{"+15550002222", "555-000-1111", "+15550002222"},
			want:       "+15550001111, +15550002222",
		},
		{
			name:       "group collapses more than five",
			recipients: []string{"+15550001111", "+15550002222", "+15550003333", "+15550004444", "+15550005555", "+15550006666", "+15550007777"},
			want:       "+15550001111, +15550002222, +15550003333, +15550004444, +15550005555, and 2 more",
		},
		{
			name:       "all recipients are own numbers",
			recipients: []string{"+15559998888"},
			ownNumbers: []string{"+15559998888"},
			want:       "Unknown conversation",
		},
		{
			name: "no recipients",
			want: "Unknown conversation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PortalName(tt.title, tt.recipients, tt.ownNumbers...); got != tt.want {
				t.Fatalf("PortalName(%q, %v, %v) = %q, want %q", tt.title, tt.recipients, tt.ownNumbers, got, tt.want)
			}
		})
	}
}

func TestGhostDisplayName(t *testing.T) {
	if got := GhostDisplayName(nil, "+15551234567"); got != "+15551234567" {
		t.Errorf("GhostDisplayName(nil) = %q", got)
	}
	if got := GhostDisplayName(&GhostMetadata{RemoteName: "Alice"}, "+15551234567"); got != "Alice" {
		t.Errorf("GhostDisplayName(remote) = %q, want Alice", got)
	}
	if got := GhostDisplayName(&GhostMetadata{RemoteName: "Alice", CustomName: "Al"}, "+15551234567"); got != "Al" {
		t.Errorf("GhostDisplayName(custom) = %q, want Al", got)
	}
	if got := GhostDisplayName(&GhostMetadata{RemoteName: "  "}, "5551234567"); got != "+15551234567" {
		t.Errorf("GhostDisplayName(blank remote) = %q, want canonical address", got)
	}
}

func TestDMPortalName(t *testing.T) {
	if got := DMPortalName(MakeUserID("+15551234567"), &GhostMetadata{RemoteName: "Alice"}); got != "Alice" {
		t.Errorf("DMPortalName() = %q, want Alice", got)
	}
	if got := DMPortalName(networkid.UserID("=2b15551234567"), nil); got != "+15551234567" {
		t.Errorf("DMPortalName() = %q, want canonical address", got)
	}
}
