// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	if got := groupDisplayName([]string{"+1", "+2"}, []string{"Alice", "Bob"}, ""); got != "Alice, Bob" {
		t.Fatalf("all named = %q, want AB", got)
	}
	if got := groupDisplayName([]string{"+1", "+2"}, []string{"Alice", ""}, ""); got != "Alice, +2" {
		t.Fatalf("mixed = %q", got)
	}
	if got := groupDisplayName([]string{"+1", "+2", "+3"}, []string{"", "", ""}, ""); got != "+1, +2, +3" {
		t.Fatalf("numbers = %q", got)
	}
	if got := groupDisplayName([]string{"+1"}, []string{"Alice"}, ""); got != "Alice" {
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

func TestGroupDisplayNameFallbacks(t *testing.T) {
	if got := groupDisplayName([]string{"+1", "+2"}, []string{"Alexandra Thompson", "Benjamin Franklin"}, ""); got != "Alexandra, Benjamin" {
		t.Fatalf("first-name fallback = %q", got)
	}
	if got := groupDisplayName([]string{"+1", "+2", "+3"}, []string{"Alexandra Thompson", "Benjamin Franklin", "Christopher Lee"}, "D"); got != "ABCD" {
		t.Fatalf("initials fallback = %q", got)
	}
	if got := groupDisplayName([]string{"+15550100204", "+15550100205", "+15550100206", "+15550100207", "+15550100208", "+15550100209"}, []string{"", "", "", "", "", ""}, ""); !strings.HasSuffix(got, "and 1 more") {
		t.Fatalf("number collapse = %q", got)
	}
}

func TestNameTokensLookGenerated(t *testing.T) {
	known := []string{"+15550100202", "+15550100201", "TestOffice", "Test"}
	if !nameTokensLookGenerated("TestOffice, +15550100202, +15550100201", known) {
		t.Fatal("mixed stale + known list should be recognized")
	}
	if !nameTokensLookGenerated("+15550100202, +15550100201", known) {
		t.Fatal("plain number list should be recognized")
	}
	if nameTokensLookGenerated("My Vacation Plans", known) {
		t.Fatal("custom name must not be recognized")
	}
	if nameTokensLookGenerated("Alice, +15550100202", []string{"+15550100202"}) {
		t.Fatal("single known token among two must not be enough")
	}
	if !nameTokensLookGenerated("TestOffice", []string{"TestOffice"}) {
		t.Fatal("single known token alone should be recognized")
	}
}

func TestFetchCardDAVAddressbookDAVDiscovery(t *testing.T) {
	const aliceVCard = "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice Example\r\nTEL:+15551230001\r\nEND:VCARD\r\n"
	const bobVCard = "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Bob Example\r\nTEL:+15551230002\r\nEND:VCARD\r\n"

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if strings.HasSuffix(r.URL.Path, ".vcf") {
				w.Header().Set("Content-Type", "text/vcard")
				fmt.Fprint(w, aliceVCard, bobVCard)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>not a vcard export</body></html>")
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
  <d:response><d:href>/dav/</d:href><d:propstat><d:prop><d:resourcetype><c:addressbook/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
  <d:response><d:href>/dav/alice.vcf</d:href><d:propstat><d:prop><d:getcontenttype>text/vcard</d:getcontenttype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
  <d:response><d:href>/dav/bob.vcf</d:href><d:propstat><d:prop><d:getcontenttype>text/vcard</d:getcontenttype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
</d:multistatus>`)
		case "REPORT":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
  <d:response><d:href>/dav/alice.vcf</d:href><d:propstat><d:prop><c:address-data>%s</c:address-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
  <d:response><d:href>/dav/bob.vcf</d:href><d:propstat><d:prop><c:address-data>%s</c:address-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
</d:multistatus>`, aliceVCard, bobVCard)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(handler))
	defer server.Close()

	cfg := CardDAVConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true, DefaultCountryCode: "1"}
	creds := &CardDAVCredentials{URL: server.URL + "/dav/", Username: "alice", Password: "secret"}
	data, err := fetchCardDAVAddressbook(context.Background(), cfg, creds)
	if err != nil {
		t.Fatalf("fetchCardDAVAddressbook: %v", err)
	}
	contacts := parseVCards(data)
	if len(contacts) != 2 {
		t.Fatalf("expected 2 contacts, got %d", len(contacts))
	}
	if contacts[0].Name != "Alice Example" || contacts[1].Name != "Bob Example" {
		t.Fatalf("unexpected contact names: %q, %q", contacts[0].Name, contacts[1].Name)
	}
}

func TestFetchCardDAVAddressbookDAVFallback(t *testing.T) {
	const carolVCard = "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Carol Example\r\nTEL:+15551230003\r\nEND:VCARD\r\n"

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if strings.HasSuffix(r.URL.Path, ".vcf") {
				w.Header().Set("Content-Type", "text/vcard")
				fmt.Fprint(w, carolVCard)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>not a vcard export</body></html>")
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
  <d:response><d:href>/dav/carol.vcf</d:href><d:propstat><d:prop><d:getcontenttype>text/vcard</d:getcontenttype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
</d:multistatus>`)
		case "REPORT":
			w.WriteHeader(http.StatusNotImplemented)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(handler))
	defer server.Close()

	cfg := CardDAVConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true, DefaultCountryCode: "1"}
	creds := &CardDAVCredentials{URL: server.URL + "/dav/", Username: "alice", Password: "secret"}
	data, err := fetchCardDAVAddressbook(context.Background(), cfg, creds)
	if err != nil {
		t.Fatalf("fetchCardDAVAddressbook: %v", err)
	}
	contacts := parseVCards(data)
	if len(contacts) != 1 || contacts[0].Name != "Carol Example" {
		t.Fatalf("expected Carol Example contact, got %#v", contacts)
	}
}
