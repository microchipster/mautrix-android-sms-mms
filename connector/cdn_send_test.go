// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// cdnSendTestMXID is the Matrix user that owns the bridge's own uploads in
// these tests.
const cdnSendTestMXID = "@alice:example.com"

// testCdnSendClient builds a network client whose connector has an active file
// service backed by a per-test directory. The connector's carrier MMS limit is
// 1 MiB unless a test overrides it.
func testCdnSendClient(t *testing.T, mutate func(*CdnConfig)) (*MyNetworkClient, *CdnService) {
	t.Helper()
	cfg := testCdnConfig(t, mutate)
	service, err := NewCdnService(zerolog.Nop(), cfg, testCdnAuth)
	if err != nil {
		t.Fatalf("NewCdnService: %v", err)
	}
	if service == nil {
		t.Fatal("NewCdnService returned no service for an active configuration")
	}
	t.Cleanup(service.StopGC)
	connector := &MyConnector{
		log:    zerolog.Nop(),
		config: &SimpleConfig{CDN: cfg, MaxAttachmentSizeMB: 1},
		cdn:    service,
	}
	client := &MyNetworkClient{
		log:       zerolog.Nop(),
		connector: connector,
		login: &bridgev2.UserLogin{
			UserLogin: &database.UserLogin{ID: "default"},
			User:      &bridgev2.User{User: &database.User{MXID: cdnSendTestMXID}},
		},
	}
	return client, service
}

// splitCdnLink decomposes a file-service link into its id and fragment key.
func splitCdnLink(t *testing.T, link string) (string, string) {
	t.Helper()
	base, key, found := strings.Cut(link, "#")
	if !found || key == "" {
		t.Fatalf("link %q has no fragment key", link)
	}
	id := strings.TrimPrefix(base, "https://cdn.example.com"+cdnPathPrefix+"/")
	if id == base || !validCdnID(id) {
		t.Fatalf("link %q does not carry a valid cdn id", link)
	}
	return id, key
}

func TestCdnSendModeResolution(t *testing.T) {
	if got := DefaultCdnConfig().EffectiveSendMode(); got != cdnSendModeNever {
		t.Errorf("disabled service mode = %q, want %q", got, cdnSendModeNever)
	}
	if got := testCdnConfig(t, nil).EffectiveSendMode(); got != cdnSendModeLink {
		t.Errorf("active service default mode = %q, want %q", got, cdnSendModeLink)
	}
	if got := testCdnConfig(t, func(c *CdnConfig) { c.SendMode = "never" }).EffectiveSendMode(); got != cdnSendModeNever {
		t.Errorf("configured never mode = %q, want %q", got, cdnSendModeNever)
	}
	if got := testCdnConfig(t, func(c *CdnConfig) { c.SendMode = " LInk " }).EffectiveSendMode(); got != cdnSendModeLink {
		t.Errorf("configured link mode = %q, want %q", got, cdnSendModeLink)
	}
	// Enabled without a domain is not active: the service never starts, so
	// nothing can be linked.
	noDomain := testCdnConfig(t, func(c *CdnConfig) {
		c.Domain = ""
		c.SendMode = cdnSendModeLink
	})
	if got := noDomain.EffectiveSendMode(); got != cdnSendModeNever {
		t.Errorf("enabled-but-inactive service mode = %q, want %q", got, cdnSendModeNever)
	}
}

func TestCdnSendDecisionMatrix(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		threshold int64
		size      int64
		want      bool
	}{
		{"never mode ignores an oversized attachment", cdnSendModeNever, 1024, 1 << 20, false},
		{"link mode below the threshold", cdnSendModeLink, 1024, 1023, false},
		{"link mode exactly at the threshold", cdnSendModeLink, 1024, 1024, false},
		{"link mode one byte above the threshold", cdnSendModeLink, 1024, 1025, true},
		{"link mode with a zero threshold links every attachment", cdnSendModeLink, 0, 1 << 20, true},
		{"link mode with a zero threshold links even a tiny attachment", cdnSendModeLink, 0, 1, true},
		{"link mode with a zero threshold still skips an empty attachment", cdnSendModeLink, 0, 0, false},
		{"never mode with a zero threshold links nothing", cdnSendModeNever, 0, 1 << 20, false},
		{"an empty mode never links", "", 1024, 1 << 20, false},
		{"an empty attachment never links", cdnSendModeLink, 1024, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cdnSendViaLink(tc.mode, tc.threshold, tc.size); got != tc.want {
				t.Errorf("cdnSendViaLink(%q, %d, %d) = %v, want %v", tc.mode, tc.threshold, tc.size, got, tc.want)
			}
		})
	}
}

func TestCdnSendConfigDefaultsAndValidation(t *testing.T) {
	base := DefaultCdnConfig()
	if base.SendMode != "" || base.SendThresholdBytes != 0 || base.SendExpiry.Duration() != 0 {
		t.Fatalf("send defaults = (%q, %d, %s), want empty/zero/zero",
			base.SendMode, base.SendThresholdBytes, formatTTLDuration(base.SendExpiry.Duration()))
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("disabled configuration must validate, got %v", err)
	}

	var parsed CdnConfig
	if err := yaml.Unmarshal([]byte("send_mode: never\nsend_threshold: 4096\nsend_expiry: 7d\n"), &parsed); err != nil {
		t.Fatalf("unmarshal cdn send config: %v", err)
	}
	if parsed.SendMode != "never" || parsed.SendThresholdBytes != 4096 || parsed.SendExpiry.Duration() != 7*24*time.Hour {
		t.Errorf("parsed send config = (%q, %d, %s), want (never, 4096, 168h)",
			parsed.SendMode, parsed.SendThresholdBytes, formatTTLDuration(parsed.SendExpiry.Duration()))
	}
	if parsed.MaxFileBytes != cdnDefaultMaxFileBytes || parsed.DefaultExpiry.Duration() != cdnDefaultExpiry {
		t.Errorf("partial cdn mapping wiped unrelated defaults: %+v", parsed)
	}

	valid := testCdnConfig(t, func(c *CdnConfig) {
		c.SendMode = "link"
		c.SendThresholdBytes = 1024
		c.SendExpiry = cdnDuration(7 * 24 * time.Hour)
	})
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid send configuration rejected: %v", err)
	}
	for _, invalid := range []struct {
		name     string
		mutate   func(*CdnConfig)
		mentions string
	}{
		{"unknown mode", func(c *CdnConfig) { c.SendMode = "sometimes" }, "send_mode"},
		{"negative threshold", func(c *CdnConfig) { c.SendThresholdBytes = -1 }, "send_threshold"},
		{"negative expiry", func(c *CdnConfig) { c.SendExpiry = cdnDuration(-time.Hour) }, "send_expiry"},
		{"expiry above the maximum", func(c *CdnConfig) { c.SendExpiry = cdnDuration(400 * 24 * time.Hour) }, "send_expiry"},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			err := testCdnConfig(t, invalid.mutate).Validate()
			if err == nil {
				t.Fatal("invalid send configuration must not validate")
			}
			if !strings.Contains(err.Error(), invalid.mentions) {
				t.Errorf("error %q does not mention %q", err, invalid.mentions)
			}
		})
	}
}

func TestCdnSendConnectorAccessors(t *testing.T) {
	client, _ := testCdnSendClient(t, nil)
	if got := client.connector.cdnSendThresholdBytes(); got != 0 {
		t.Errorf("default threshold = %d, want 0 (link every attachment)", got)
	}
	if !client.cdnLinkMode() {
		t.Error("an active service in default mode must enable link mode")
	}
	if got := client.connector.cdnSendExpiry(); got != 0 {
		t.Errorf("default send expiry = %s, want the service default (0)", got)
	}

	configured := &MyConnector{config: &SimpleConfig{
		MaxAttachmentSizeMB: 2,
		CDN: testCdnConfig(t, func(c *CdnConfig) {
			c.SendThresholdBytes = 4096
			c.SendExpiry = cdnDuration(3 * 24 * time.Hour)
		}),
	}}
	if got := configured.cdnSendThresholdBytes(); got != 4096 {
		t.Errorf("configured threshold = %d, want 4096", got)
	}
	if got := configured.cdnSendExpiry(); got != 3*24*time.Hour {
		t.Errorf("configured send expiry = %s, want 72h", got)
	}
	// No service means no link mode, whatever the (inert) configuration says.
	if got := configured.cdnSendMode(); got != cdnSendModeNever {
		t.Errorf("mode without a service = %q, want %q", got, cdnSendModeNever)
	}

	var nilConnector *MyConnector
	if got := nilConnector.cdnSendThresholdBytes(); got != 0 {
		t.Errorf("nil connector threshold = %d, want 0", got)
	}
	if got := nilConnector.cdnPlaintextCapacityBytes(); got != 0 {
		t.Errorf("nil connector capacity = %d, want 0", got)
	}
}

func TestCdnSendAttachmentLimitSelection(t *testing.T) {
	// A carrier limit above the file-service capacity keeps the carrier limit.
	client, _ := testCdnSendClient(t, func(c *CdnConfig) { c.MaxFileBytes = 256 << 10 })
	if got, want := client.outgoingAttachmentLimitBytes(), int64(1)<<20; got != want {
		t.Errorf("limit with a small service cap = %d, want the %d carrier limit", got, want)
	}
	// A file-service capacity above the carrier limit raises the download cap
	// so bigger attachments can be linked.
	client, _ = testCdnSendClient(t, func(c *CdnConfig) { c.MaxFileBytes = 8 << 20 })
	if got, want := client.outgoingAttachmentLimitBytes(), int64(8)<<20-cdnObjectOverheadBytes; got != want {
		t.Errorf("limit with a larger service cap = %d, want %d", got, want)
	}
	// Without a service the carrier limit applies.
	noService := &MyNetworkClient{connector: &MyConnector{config: &SimpleConfig{MaxAttachmentSizeMB: 2}}}
	if got, want := noService.outgoingAttachmentLimitBytes(), int64(2)<<20; got != want {
		t.Errorf("limit without a service = %d, want %d", got, want)
	}
}

func TestCdnSendEncryptObject(t *testing.T) {
	plaintext := append([]byte{0x89, 0x50, 0x4e, 0x47}, bytes.Repeat([]byte{0x5a}, 1024)...)
	key, object, err := cdnEncryptObject(plaintext)
	if err != nil {
		t.Fatalf("cdnEncryptObject: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key is %d bytes, want 32", len(key))
	}
	if len(object) != len(plaintext)+cdnObjectOverheadBytes {
		t.Fatalf("object is %d bytes, want %d (nonce+ct+tag)", len(object), len(plaintext)+cdnObjectOverheadBytes)
	}
	// The app-contract decryptor pins the exact on-wire format.
	decrypted, err := appCdnDecrypt(t, object, base64.RawURLEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("decrypting the bridge-encrypted object failed: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("decrypted %d bytes, want the %d plaintext bytes", len(decrypted), len(plaintext))
	}
	if _, err := appCdnDecrypt(t, object, appCdnRandomKey(t)); err == nil {
		t.Error("decrypting with a wrong key succeeded, want failure")
	}
	// Every call must use a fresh key and nonce.
	secondKey, secondObject, err := cdnEncryptObject(plaintext)
	if err != nil {
		t.Fatalf("second cdnEncryptObject: %v", err)
	}
	if bytes.Equal(key, secondKey) {
		t.Error("two encryptions reused the same key")
	}
	if bytes.Equal(object[:12], secondObject[:12]) {
		t.Error("two encryptions reused the same nonce")
	}
	if bytes.Equal(object, secondObject) {
		t.Error("two encryptions produced the same object")
	}
}

func TestCdnSendStoreInProcess(t *testing.T) {
	service := testCdnService(t, func(c *CdnConfig) {
		c.DefaultExpiry = cdnDuration(time.Hour)
		c.MaxExpiry = cdnDuration(24 * time.Hour)
	})
	server := testCdnServer(t, service)

	plaintext := []byte("bridge-side outgoing attachment")
	key, object, err := cdnEncryptObject(plaintext)
	if err != nil {
		t.Fatalf("cdnEncryptObject: %v", err)
	}
	upload, err := service.Store("alice", object, 0)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if !validCdnID(upload.ID) {
		t.Fatalf("stored id %q is not valid", upload.ID)
	}
	if upload.Size != int64(len(object)) {
		t.Errorf("stored size = %d, want %d", upload.Size, len(object))
	}
	if upload.ExpiresAt.Before(time.Now().Add(30*time.Minute)) || upload.ExpiresAt.After(time.Now().Add(90*time.Minute)) {
		t.Errorf("stored expiry = %s, want about the one-hour default", upload.ExpiresAt)
	}
	entries, err := service.List("alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != upload.ID {
		t.Errorf("List = %+v, want the stored entry %s", entries, upload.ID)
	}

	// The stored object is the ciphertext the app contract downloads.
	resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+upload.ID+"/raw", "", nil)
	body := testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw fetch status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if !bytes.Equal(body, object) {
		t.Errorf("raw fetch returned %d bytes, want the %d stored bytes", len(body), len(object))
	}
	decrypted, err := appCdnDecrypt(t, body, base64.RawURLEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("decrypting the stored object failed: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("decrypted %d bytes, want the %d plaintext bytes", len(decrypted), len(plaintext))
	}

	// An explicit lifetime is honoured and bounded by max_expiry.
	upload, err = service.Store("alice", object, 2*time.Hour)
	if err != nil {
		t.Fatalf("Store with an explicit expiry: %v", err)
	}
	if upload.ExpiresAt.Before(time.Now().Add(time.Hour)) || upload.ExpiresAt.After(time.Now().Add(3*time.Hour)) {
		t.Errorf("stored expiry = %s, want about two hours", upload.ExpiresAt)
	}
	if _, err := service.Store("alice", object, 48*time.Hour); err == nil {
		t.Error("a lifetime above max_expiry must fail")
	}

	// Invalid inputs are rejected before anything is stored.
	if _, err := service.Store("alice", nil, 0); err == nil {
		t.Error("an empty payload must fail")
	}
	if _, err := service.Store("   ", object, 0); err == nil {
		t.Error("an empty owner must fail")
	}
	var nilService *CdnService
	if _, err := nilService.Store("alice", object, 0); err == nil {
		t.Error("a nil service must fail")
	}

	small := testCdnService(t, func(c *CdnConfig) {
		c.MaxFileBytes = 256
		c.PerUserCapBytes = 64
	})
	if _, err := small.Store("alice", bytes.Repeat([]byte{1}, 257), 0); err == nil {
		t.Error("a payload above max_file_bytes must fail")
	}
	if _, err := small.Store("alice", bytes.Repeat([]byte{1}, 65), 0); err == nil {
		t.Error("a payload above per_user_cap_bytes must fail")
	}
	if _, err := small.Store("alice", bytes.Repeat([]byte{1}, 64), 0); err != nil {
		t.Errorf("a payload within both caps must store, got %v", err)
	}
}

func TestCdnSendLinkText(t *testing.T) {
	link := "https://cdn.example.com/f/abc123#key"
	cases := []struct {
		name     string
		caption  string
		fileName string
		want     string
	}{
		{"label only", "", "IMG_0001.jpg", "IMG_0001.jpg: " + link},
		{"caption and label", "Look at this", "IMG_0001.jpg", "Look at this\nIMG_0001.jpg: " + link},
		{"caption only", "Look at this", "", "Look at this\n" + link},
		{"bare link", "", "", link},
		{"body repeats the filename", "IMG_0001.jpg", "IMG_0001.jpg", "IMG_0001.jpg: " + link},
		{"body repeats the filename with different case", "img_0001.JPG", "IMG_0001.jpg", "IMG_0001.jpg: " + link},
		{"trims whitespace", "  Look  ", "  IMG.jpg  ", "Look\nIMG.jpg: " + link},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cdnLinkMessageText(tc.caption, tc.fileName, link); got != tc.want {
				t.Errorf("cdnLinkMessageText(%q, %q) = %q, want %q", tc.caption, tc.fileName, got, tc.want)
			}
		})
	}
}

func TestCdnSendOutgoingAttachmentBecomesLink(t *testing.T) {
	client, service := testCdnSendClient(t, func(c *CdnConfig) {
		c.SendThresholdBytes = 512
		c.SendExpiry = cdnDuration(48 * time.Hour)
	})
	data := append([]byte{0xff, 0xd8, 0xff}, bytes.Repeat([]byte{0x11}, 1024)...)

	text, attachments, linked, err := client.prepareOutgoingAttachment("Look at this", data, "image/jpeg", "photo.jpg", true)
	if err != nil {
		t.Fatalf("prepareOutgoingAttachment: %v", err)
	}
	if !linked || len(attachments) != 0 {
		t.Fatalf("linked = %v with %d attachment(s), want a link and no attachment", linked, len(attachments))
	}
	prefix := "Look at this\nphoto.jpg: https://cdn.example.com" + cdnPathPrefix + "/"
	if !strings.HasPrefix(text, prefix) {
		t.Fatalf("link text = %q, want the caption and labeled link %q…", text, prefix)
	}
	id, key := splitCdnLink(t, strings.TrimPrefix(text, "Look at this\nphoto.jpg: "))

	// The stored entry belongs to the Matrix user and decrypts to the
	// original attachment.
	entries, err := service.List(cdnSendTestMXID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != id {
		t.Fatalf("List = %+v, want one entry for %s", entries, id)
	}
	if got := entries[0].ExpiresAt.Sub(time.Now()); got < 47*time.Hour || got > 49*time.Hour {
		t.Errorf("stored entry expires in %s, want about the configured 48h", got)
	}
	server := testCdnServer(t, service)
	resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+id+"/raw", "", nil)
	body := testCdnReadBody(t, resp)
	decrypted, err := appCdnDecrypt(t, body, key)
	if err != nil {
		t.Fatalf("decrypting the linked attachment failed: %v", err)
	}
	if !bytes.Equal(decrypted, data) {
		t.Errorf("decrypted %d bytes, want the %d attachment bytes", len(decrypted), len(data))
	}

	// Below the threshold the attachment stays an attachment.
	smallText, smallAttachments, smallLinked, err := client.prepareOutgoingAttachment("", data[:100], "image/jpeg", "photo.jpg", true)
	if err != nil {
		t.Fatalf("prepareOutgoingAttachment (below threshold): %v", err)
	}
	if smallLinked || len(smallAttachments) != 1 || smallText != "" {
		t.Fatalf("below-threshold result = (%q, %d attachment(s), linked=%v), want the attachment path",
			smallText, len(smallAttachments), smallLinked)
	}
	if smallAttachments[0].Base64 != attachmentDataURIless(data[:100]) ||
		smallAttachments[0].ContentType != "image/jpeg" ||
		smallAttachments[0].FileName != "photo.jpg" {
		t.Errorf("below-threshold attachment = %+v, want the original bytes and metadata", smallAttachments[0])
	}

	// "never" mode never links, even above the threshold.
	client.connector.config.CDN.SendMode = cdnSendModeNever
	_, neverAttachments, neverLinked, err := client.prepareOutgoingAttachment("", data, "image/jpeg", "photo.jpg", true)
	if err != nil {
		t.Fatalf("prepareOutgoingAttachment (never mode): %v", err)
	}
	if neverLinked || len(neverAttachments) != 1 {
		t.Fatalf("never-mode result = (%d attachment(s), linked=%v), want the attachment path", len(neverAttachments), neverLinked)
	}
}

func TestCdnSendDefaultLinksEveryAttachment(t *testing.T) {
	// The shipped default has no threshold: even an attachment far below the
	// carrier MMS limit takes the link path while the service is active,
	// because the link keeps the original bytes instead of a carrier-optimised
	// copy.
	client, service := testCdnSendClient(t, nil)
	data := append([]byte{0xff, 0xd8, 0xff}, bytes.Repeat([]byte{0x21}, 100)...)

	text, attachments, linked, err := client.prepareOutgoingAttachment("", data, "image/jpeg", "small.jpg", true)
	if err != nil {
		t.Fatalf("prepareOutgoingAttachment: %v", err)
	}
	if !linked || len(attachments) != 0 {
		t.Fatalf("linked = %v with %d attachment(s), want the default to link every attachment", linked, len(attachments))
	}
	if !strings.HasPrefix(text, "small.jpg: https://cdn.example.com"+cdnPathPrefix+"/") {
		t.Fatalf("link text = %q, want the labeled link", text)
	}
	entries, err := service.List(cdnSendTestMXID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stored entries = %d, want the one linked attachment", len(entries))
	}
}

func TestCdnSendOutgoingAttachmentFallsBackOnUploadFailure(t *testing.T) {
	client, service := testCdnSendClient(t, func(c *CdnConfig) { c.SendThresholdBytes = 64 })
	// Break the storage directory: staging the upload now fails.
	if err := os.RemoveAll(service.objectDir()); err != nil {
		t.Fatalf("remove cdn object dir: %v", err)
	}
	data := bytes.Repeat([]byte{0x42}, 256)

	text, attachments, linked, err := client.prepareOutgoingAttachment("caption", data, "application/pdf", "doc.pdf", true)
	if err != nil {
		t.Fatalf("prepareOutgoingAttachment must fall back, got %v", err)
	}
	if linked || text != "caption" || len(attachments) != 1 {
		t.Fatalf("fallback result = (%q, %d attachment(s), linked=%v), want the plain caption and attachment",
			text, len(attachments), linked)
	}
	if attachments[0].Base64 != attachmentDataURIless(data) || attachments[0].FileName != "doc.pdf" {
		t.Errorf("fallback attachment = %+v, want the original bytes", attachments[0])
	}

	// Without an MMS backend there is no fallback: the error says why.
	_, _, _, err = client.prepareOutgoingAttachment("caption", data, "application/pdf", "doc.pdf", false)
	if !errors.Is(err, errMMSNeedsAppBackend) {
		t.Fatalf("fallback without MMS = %v, want the MMS-unavailable error", err)
	}
	if !strings.Contains(err.Error(), "file service upload also failed") {
		t.Errorf("error %q does not mention the failed file service upload", err)
	}
	// A below-threshold attachment never reaches the service, so the error
	// stays the plain MMS one.
	_, _, _, err = client.prepareOutgoingAttachment("caption", data[:32], "application/pdf", "doc.pdf", false)
	if !errors.Is(err, errMMSNeedsAppBackend) || strings.Contains(err.Error(), "also failed") {
		t.Fatalf("below-threshold error = %v, want the plain MMS-unavailable error", err)
	}
	// An oversized attachment that could not be linked fails with the size
	// error instead of being handed to a phone that cannot send it.
	oversized := bytes.Repeat([]byte{0x42}, int(client.connector.maxAttachmentSizeBytes())+1)
	_, _, _, err = client.prepareOutgoingAttachment("caption", oversized, "application/pdf", "big.pdf", true)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized fallback error = %v, want a size error", err)
	}
}
