// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"strings"
	"testing"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

func TestCanonicalAddress(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "+15551234567", want: "+15551234567"},
		{input: "+1 (555) 123-4567", want: "+15551234567"},
		{input: "555-123-4567", want: "+15551234567"},
		{input: "5551234567", want: "+15551234567"},
		{input: "5551234", want: "+15551234"},
		{input: "+44 7123 456789", want: "+447123456789"},
		{input: "12345", want: "12345"},
		{input: "123456", want: "123456"},
		{input: "+12345", want: "+12345"},
		{input: "BANK", want: "BANK"},
		{input: "bank", want: "BANK"},
		{input: "Alert@Example.COM", want: "alert@example.com"},
		{input: "+15551234567;phone-context=ca-us.phone-context.example.com", want: "+15551234567"},
		{input: "  +15551234567  ", want: "+15551234567"},
		{input: "", want: ""},
		{input: "   ", want: ""},
	}
	for _, tt := range tests {
		if got := CanonicalAddress(tt.input); got != tt.want {
			t.Errorf("CanonicalAddress(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestCanonicalAddressEscapingRoundTrip(t *testing.T) {
	for _, address := range []string{"+15551234567", "12345", "BANK", "alert@example.com"} {
		escaped := EscapeAddress(address)
		if strings.ContainsAny(escaped, "+,") {
			t.Errorf("EscapeAddress(%q) = %q, still contains + or ,", address, escaped)
		}
		if got := UnescapeAddress(escaped); got != address {
			t.Errorf("UnescapeAddress(EscapeAddress(%q)) = %q", address, got)
		}
		if got := MakeUserID(address); got != MakeUserID(escaped) {
			t.Errorf("MakeUserID(%q) = %q, but MakeUserID(%q) = %q", address, got, escaped, MakeUserID(escaped))
		}
	}
	if got := MakeUserID("+15551234567"); got != "=2b15551234567" {
		t.Errorf("MakeUserID(+15551234567) = %q, want =2b15551234567", got)
	}
}

func TestCanonicalRecipients(t *testing.T) {
	got := CanonicalRecipients([]string{"555-123-4567", "+15551234567", "bank", "BANK", "", "12345"})
	want := []string{"+15551234567", "12345", "BANK"}
	if len(got) != len(want) {
		t.Fatalf("CanonicalRecipients() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CanonicalRecipients() = %v, want %v", got, want)
		}
	}
}

func TestDeviceScope(t *testing.T) {
	scope := DeviceScope("ABC123")
	if len(scope) != 8 {
		t.Fatalf("DeviceScope() = %q, want 8 hex characters", scope)
	}
	for _, r := range scope {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("DeviceScope() = %q, contains non-hex character %q", scope, r)
		}
	}
	if again := DeviceScope(" ABC123 "); again != scope {
		t.Fatalf("DeviceScope is not stable across whitespace: %q vs %q", scope, again)
	}
	if other := DeviceScope("ABC124"); other == scope {
		t.Fatalf("DeviceScope collision between different serials: %q", scope)
	}
	if empty := DeviceScope("  "); empty != "" {
		t.Fatalf("DeviceScope(empty) = %q, want empty", empty)
	}
}

func TestMakePortalIDAndParse(t *testing.T) {
	scope := DeviceScope("ABC123")
	portalID := MakePortalID(scope, 42, 1)
	want := "thread-" + scope + "-1-42"
	if string(portalID) != want {
		t.Fatalf("MakePortalID() = %q, want %q", portalID, want)
	}
	gotScope, gotSub, gotThread, ok := ParsePortalID(portalID)
	if !ok {
		t.Fatalf("ParsePortalID(%q) failed", portalID)
	}
	if gotScope != scope || gotSub != 1 || gotThread != 42 {
		t.Fatalf("ParsePortalID(%q) = (%q, %d, %d), want (%q, 1, 42)", portalID, gotScope, gotSub, gotThread, scope)
	}

	for _, bad := range []string{"", "thread-", "thread-x-1-2-3", "thread-scope-x-2", "thread-scope-1-x", "addr-1-2", "other-scope-1-2"} {
		if _, _, _, ok := ParsePortalID(networkid.PortalID(bad)); ok {
			t.Errorf("ParsePortalID(%q) unexpectedly succeeded", bad)
		}
	}
}

func TestMakeAddressPortalID(t *testing.T) {
	if got := MakeAddressPortalID("+15551234567"); got != "addr-=2b15551234567" {
		t.Fatalf("MakeAddressPortalID() = %q", got)
	}
	if got := MakeAddressPortalID("555-123-4567"); got != "addr-=2b15551234567" {
		t.Fatalf("MakeAddressPortalID() is not canonical: %q", got)
	}
}
