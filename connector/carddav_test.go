// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import "testing"

func TestNormalizeCardDAVPhoneNumber(t *testing.T) {
	cases := []struct {
		raw  string
		cc   string
		want string
		ok   bool
	}{
		{"+1 (555) 010-0201", "1", "+15550100201", true},
		{"5550100201", "1", "+15550100201", true},
		{"15550100201", "1", "+15550100201", true},
		{"0044 20 7946 0958", "1", "+442079460958", true},
		{"+44 20 7946 0958", "1", "+442079460958", true},
		{"tel:+15550100201", "1", "+15550100201", true},
		{"628", "1", "", false},
		{"abc", "1", "", false},
		{"", "1", "", false},
	}
	for _, tc := range cases {
		got, ok := normalizeCardDAVPhoneNumber(tc.raw, tc.cc)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("normalizeCardDAVPhoneNumber(%q) = %q, %v; want %q, %v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseVCards(t *testing.T) {
	card := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice Example\r\n" +
		"TEL;TYPE=CELL:+1 555 010 0201\r\n" +
		"PHOTO;ENCODING=b;TYPE=GIF:R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7\r\n" +
		"END:VCARD\r\n"
	contacts := parseVCards([]byte(card))
	if len(contacts) != 1 {
		t.Fatalf("contacts = %d, want 1", len(contacts))
	}
	contact := contacts[0]
	if contact.Name != "Alice Example" {
		t.Fatalf("name = %q", contact.Name)
	}
	if len(contact.Phones) != 1 || contact.Phones[0] != "+1 555 010 0201" {
		t.Fatalf("phones = %v", contact.Phones)
	}
	if len(contact.Photo) == 0 || contact.PhotoMime != "image/gif" {
		t.Fatalf("photo = %d bytes, mime %q", len(contact.Photo), contact.PhotoMime)
	}
}

func TestParseVCardsFoldedLine(t *testing.T) {
	card := "BEGIN:VCARD\nFN:Bob Smi\n th\nTEL:+15550100201\nEND:VCARD\n"
	contacts := parseVCards([]byte(card))
	if len(contacts) != 1 || contacts[0].Name != "Bob Smith" {
		t.Fatalf("contacts = %+v", contacts)
	}
}

func TestValidateCardDAVURL(t *testing.T) {
	cfg := DefaultCardDAVConfig()
	if err := validateCardDAVURL(cfg, "http://example.com/book"); err == nil {
		t.Fatal("plain HTTP must be rejected by default")
	}
	cfg.AllowInsecureHTTP = true
	if err := validateCardDAVURL(cfg, "http://example.com/book"); err != nil {
		t.Fatalf("http with allow_insecure_http = %v", err)
	}
	if err := validateCardDAVURL(cfg, "https://localhost/book"); err == nil {
		t.Fatal("private hosts must be rejected by default")
	}
	if err := validateCardDAVURL(cfg, "https://10.0.0.5/book"); err == nil {
		t.Fatal("private IPs must be rejected by default")
	}
	cfg.AllowPrivateHosts = true
	if err := validateCardDAVURL(cfg, "https://local.example.com/book"); err != nil {
		t.Fatalf("public host = %v", err)
	}
}

func TestGroupDisplayName(t *testing.T) {
	if got := groupDisplayName([]string{"+1", "+2"}, []string{"Alice", "Bob"}); got != "AB" {
		t.Fatalf("all named = %q, want AB", got)
	}
	if got := groupDisplayName([]string{"+1", "+2"}, []string{"Alice", ""}); got != "Alice, +2" {
		t.Fatalf("mixed = %q", got)
	}
	if got := groupDisplayName([]string{"+1", "+2", "+3"}, []string{"", "", ""}); got != "+1, +2, +3" {
		t.Fatalf("numbers = %q", got)
	}
	if got := groupDisplayName([]string{"+1"}, []string{"Alice"}); got != "Alice" {
		t.Fatalf("single = %q", got)
	}
}

func TestInitialsLikeName(t *testing.T) {
	if !initialsLikeName("AB", 2) {
		t.Fatal("AB should look like initials")
	}
	if initialsLikeName("AB", 1) {
		t.Fatal("AB is too long for one contact")
	}
	if initialsLikeName("Alice", 2) {
		t.Fatal("Alice is not initials")
	}
}

func TestStripRoomNameSuffix(t *testing.T) {
	if got := stripRoomNameSuffix("Alice, Bob (sms)"); got != "Alice, Bob" {
		t.Fatalf("strip = %q", got)
	}
	if got := stripRoomNameSuffix("Alice, Bob (sms-lab)"); got != "Alice, Bob" {
		t.Fatalf("strip labeled = %q", got)
	}
	if got := stripRoomNameSuffix("Alice (work)"); got != "Alice (work)" {
		t.Fatalf("strip unrelated = %q", got)
	}
}

func TestParseContactNameSource(t *testing.T) {
	if source, ok := parseContactNameSource("--source"); ok || source != "" {
		t.Fatal("unknown source must be rejected")
	}
	if source, ok := parseContactNameSource("carddav"); !ok || source != contactNameSourceCardDAV {
		t.Fatal("carddav must parse")
	}
	if source, ok := parseContactNameSource(""); !ok || source != "" {
		t.Fatal("empty must mean all sources")
	}
}
