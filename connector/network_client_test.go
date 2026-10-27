// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

// TestRememberThreadRecipients covers refreshing the thread cache from
// thread.updated events, which is how app-paired portals recover their
// participant list after a bridge restart.
func TestRememberThreadRecipients(t *testing.T) {
	nc := &MyNetworkClient{}
	nc.rememberThreadRecipients(33, 1, []string{"+15550100100"})
	thread, ok := nc.cachedThread(33)
	if !ok || thread.SubID != 1 || len(thread.Recipients) != 1 || thread.Recipients[0] != "+15550100100" {
		t.Fatalf("cachedThread(33) = %+v, %v", thread, ok)
	}

	// An event without recipients must never clobber the cached list.
	nc.rememberThreadRecipients(33, 0, nil)
	thread, _ = nc.cachedThread(33)
	if len(thread.Recipients) != 1 || thread.Recipients[0] != "+15550100100" {
		t.Fatalf("recipients clobbered by empty update: %v", thread.Recipients)
	}

	// Later updates replace the list while keeping the known sub id.
	nc.rememberThreadRecipients(33, 0, []string{"+15551112222", "+15553334444"})
	thread, _ = nc.cachedThread(33)
	if len(thread.Recipients) != 2 || thread.SubID != 1 {
		t.Fatalf("recipients after update = %+v", thread)
	}

	// App transports report sub_id -1 on thread.updated events when the
	// phone thread identity is implicit; that must not overwrite a known sub.
	nc.rememberThreadRecipients(33, -1, []string{"+15551112222"})
	thread, _ = nc.cachedThread(33)
	if thread.SubID != 1 {
		t.Fatalf("sub id clobbered by unknown update: %+v", thread)
	}

	// A real sub id still wins.
	nc.rememberThreadRecipients(33, 2, []string{"+15551112222"})
	thread, _ = nc.cachedThread(33)
	if thread.SubID != 2 {
		t.Fatalf("sub id not updated: %+v", thread)
	}
}

func TestMmsRowID(t *testing.T) {
	tests := map[string]int64{
		"mms:1:534":   534,
		"mms:2:0":     0,
		"sms:1:534":   0,
		"mms:1:xx":    0,
		"mms:1":       0,
		"":            0,
		"mms:1:534:2": 0,
	}
	for input, want := range tests {
		if got := mmsRowID(input); got != want {
			t.Errorf("mmsRowID(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestAgentMessageTimestamp(t *testing.T) {
	want := time.UnixMilli(1730000000000).UTC()
	if got := agentMessageTimestamp(agentapi.Message{TimestampMS: 1730000000000}); !got.Equal(want) {
		t.Errorf("agentMessageTimestamp(timestamp_ms) = %v, want %v", got, want)
	}
	if got := agentMessageTimestamp(agentapi.Message{TimestampSentMS: 1730000000000}); !got.Equal(want) {
		t.Errorf("agentMessageTimestamp(timestamp_sent_ms) = %v, want %v", got, want)
	}
	if got := agentMessageTimestamp(agentapi.Message{}); got.IsZero() {
		t.Error("agentMessageTimestamp(empty) returned zero time")
	}
}

// TestEventSenderForOwnMessages is a regression test for outgoing (from-me)
// messages mirrored from the phone: the sender must be a non-empty remote user
// ID, otherwise bridgev2 stores message.sender_id = "" and Postgres rejects
// the row with a message_sender_fkey foreign key violation. Own messages are
// attributed to the login's own user (its own-number ghost), which bridgev2
// creates on demand, just like it does for inbound sender ghosts.
func TestEventSenderForOwnMessages(t *testing.T) {
	nc := &MyNetworkClient{
		connector: &MyConnector{config: &SimpleConfig{OwnNumbers: []string{"+15550100100"}}},
		login:     &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "default"}},
	}

	incoming := nc.eventSenderForMessage(agentapi.Message{Sender: "+15551234567"})
	if incoming.IsFromMe || incoming.Sender != MakeUserID("+15551234567") {
		t.Fatalf("incoming sender = %+v, want IsFromMe=false and sender=%q", incoming, MakeUserID("+15551234567"))
	}

	own := nc.eventSenderForMessage(agentapi.Message{ID: "sms:2:2", Sender: ""})
	if !own.IsFromMe {
		t.Fatalf("own sender = %+v, want IsFromMe=true", own)
	}
	if own.Sender != MakeUserID("+15550100100") {
		t.Fatalf("own sender = %q, want the own-number ghost %q", own.Sender, MakeUserID("+15550100100"))
	}

	// Before the first heartbeat and without own_numbers config, the login ID
	// keeps the sender non-empty so the foreign key still holds.
	fallback := (&MyNetworkClient{login: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "default"}}}).
		eventSenderForMessage(agentapi.Message{Sender: ""})
	if !fallback.IsFromMe || fallback.Sender != networkid.UserID("default") {
		t.Fatalf("fallback own sender = %+v, want IsFromMe=true and sender=default", fallback)
	}
}

func TestStableLoginID(t *testing.T) {
	first := stableLoginID("adb:ABC123")
	second := stableLoginID("adb:ABC123")
	if first != second {
		t.Fatalf("stableLoginID is not deterministic: %q vs %q", first, second)
	}
	if len(first) != 36 {
		t.Fatalf("stableLoginID() = %q, want a 36 character UUID", first)
	}
	if first[14] != '5' {
		t.Errorf("stableLoginID() = %q, want version 5 UUID", first)
	}
	if other := stableLoginID("adb:ABC124"); other == first {
		t.Errorf("stableLoginID collision: %q", first)
	}
}

// fakeMediaIntent embeds the MatrixAPI interface so only UploadMedia needs an
// implementation in tests.
type fakeMediaIntent struct {
	bridgev2.MatrixAPI
	uploads      []string
	lastUploaded []byte
}

func (f *fakeMediaIntent) UploadMedia(ctx context.Context, roomID id.RoomID, data []byte, fileName, mimeType string) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	f.uploads = append(f.uploads, fileName+"|"+mimeType)
	f.lastUploaded = data
	return id.ContentURIString("mxc://example.com/" + fileName), nil, nil
}

func newMediaTestClient(t *testing.T, registerPart bool) *MyNetworkClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/messages/mms:1:534", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "mms:1:534", "thread_id": 12, "sub_id": 1, "timestamp_ms": 1730000000000,
			"sender": "+15551234567", "is_mms": true,
			"attachments": []map[string]any{{"part_id": 901, "content_type": "image/jpeg", "file_name": "photo.jpg", "size": 4}},
		})
	})
	if registerPart {
		mux.HandleFunc("GET /v1/mms/534/parts/901", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("jpeg"))
		})
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &MyNetworkClient{
		log:   zerolog.Nop(),
		agent: NewAgentClient(server.URL, ""),
	}
}

func testPortal() *bridgev2.Portal {
	return &bridgev2.Portal{Portal: &database.Portal{MXID: "!room:example.com"}}
}

func TestBuildConvertedMessageTextOnly(t *testing.T) {
	nc := newMediaTestClient(t, false)
	converted, err := nc.buildConvertedMessage(context.Background(), testPortal(), &fakeMediaIntent{}, agentapi.Message{
		ID: "sms:1:99", ThreadID: 12, Body: "hello",
	})
	if err != nil {
		t.Fatalf("buildConvertedMessage() error: %v", err)
	}
	if len(converted.Parts) != 1 || converted.Parts[0].Content.MsgType != event.MsgText || converted.Parts[0].Content.Body != "hello" {
		t.Fatalf("unexpected converted message: %+v", converted.Parts[0])
	}
	meta, _ := converted.Parts[0].DBMetadata.(*MessageMetadata)
	if meta == nil || meta.Body != "hello" || meta.HasAttachment {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
}

func TestBuildConvertedMessageMMSWithCaption(t *testing.T) {
	nc := newMediaTestClient(t, true)
	intent := &fakeMediaIntent{}
	converted, err := nc.buildConvertedMessage(context.Background(), testPortal(), intent, agentapi.Message{
		ID: "mms:1:534", ThreadID: 12, Body: "look at this",
		Attachments: []agentapi.Attachment{{PartID: 901, ContentType: "image/jpeg", FileName: "photo.jpg"}},
	})
	if err != nil {
		t.Fatalf("buildConvertedMessage() error: %v", err)
	}
	if len(converted.Parts) != 1 {
		t.Fatalf("expected one part, got %d", len(converted.Parts))
	}
	part := converted.Parts[0]
	if part.Content.MsgType != event.MsgImage || part.Content.Body != "look at this" || part.Content.FileName != "photo.jpg" {
		t.Fatalf("unexpected part content: %+v", part.Content)
	}
	if part.Content.URL != "mxc://example.com/photo.jpg" || part.Content.Info == nil || part.Content.Info.MimeType != "image/jpeg" {
		t.Fatalf("unexpected media info: url=%q info=%+v", part.Content.URL, part.Content.Info)
	}
	meta, _ := part.DBMetadata.(*MessageMetadata)
	if meta == nil || meta.Body != "look at this" || !meta.HasAttachment {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
	if len(intent.uploads) != 1 || intent.uploads[0] != "photo.jpg|image/jpeg" {
		t.Fatalf("unexpected uploads: %v", intent.uploads)
	}
}

func TestBuildConvertedMessageMMSAttachmentOnly(t *testing.T) {
	nc := newMediaTestClient(t, true)
	converted, err := nc.buildConvertedMessage(context.Background(), testPortal(), &fakeMediaIntent{}, agentapi.Message{
		ID: "mms:1:534", ThreadID: 12,
		Attachments: []agentapi.Attachment{{PartID: 901, ContentType: "image/jpeg", FileName: "photo.jpg"}},
	})
	if err != nil {
		t.Fatalf("buildConvertedMessage() error: %v", err)
	}
	if len(converted.Parts) != 1 || converted.Parts[0].Content.Body != "photo.jpg" {
		t.Fatalf("attachment-only body = %q, want photo.jpg", converted.Parts[0].Content.Body)
	}
}

func TestBuildConvertedMessageMissingPartFallsBackToText(t *testing.T) {
	nc := newMediaTestClient(t, false)
	converted, err := nc.buildConvertedMessage(context.Background(), testPortal(), &fakeMediaIntent{}, agentapi.Message{
		ID: "mms:1:534", ThreadID: 12, Body: "text survives",
		Attachments: []agentapi.Attachment{{PartID: 901, ContentType: "image/jpeg", FileName: "photo.jpg"}},
	})
	if err != nil {
		t.Fatalf("buildConvertedMessage() error: %v", err)
	}
	if len(converted.Parts) != 1 || converted.Parts[0].Content.MsgType != event.MsgText {
		t.Fatalf("expected text fallback, got %+v", converted.Parts[0])
	}
	want := "text survives\n\n[MMS media could not be downloaded: photo.jpg]"
	if converted.Parts[0].Content.Body != want {
		t.Fatalf("fallback body = %q, want %q", converted.Parts[0].Content.Body, want)
	}
	meta, _ := converted.Parts[0].DBMetadata.(*MessageMetadata)
	if meta == nil || meta.Body != want {
		t.Fatalf("fallback metadata = %+v", meta)
	}
}

func TestBuildConvertedMessageInlineBase64Attachment(t *testing.T) {
	// servePart=false makes every HTTP part fetch fail, so success proves the
	// inline base64 path (used by the app transport) was taken.
	nc := newMediaTestClient(t, false)
	intent := &fakeMediaIntent{}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'i', 'n', 'l', 'i', 'n', 'e'}
	converted, err := nc.buildConvertedMessage(context.Background(), testPortal(), intent, agentapi.Message{
		ID: "mms:1:534", ThreadID: 12, Body: "look at this",
		Attachments: []agentapi.Attachment{{
			PartID:      901,
			ContentType: "image/jpeg",
			FileName:    "photo.jpg",
			Base64:      base64.StdEncoding.EncodeToString(jpeg),
		}},
	})
	if err != nil {
		t.Fatalf("buildConvertedMessage() error: %v", err)
	}
	if len(converted.Parts) != 1 {
		t.Fatalf("expected one part, got %d", len(converted.Parts))
	}
	part := converted.Parts[0]
	if part.Content.MsgType != event.MsgImage || part.Content.Body != "look at this" || part.Content.FileName != "photo.jpg" {
		t.Fatalf("unexpected part content: %+v", part.Content)
	}
	if len(intent.uploads) != 1 || intent.uploads[0] != "photo.jpg|image/jpeg" {
		t.Fatalf("unexpected uploads: %v", intent.uploads)
	}
	if intent.lastUploaded == nil || !bytes.Equal(intent.lastUploaded, jpeg) {
		t.Fatalf("uploaded bytes = %v, want %v", intent.lastUploaded, jpeg)
	}
}

func TestAttachmentDataErrors(t *testing.T) {
	nc := newMediaTestClient(t, false)
	if _, _, err := attachmentData(context.Background(), nc.agent, 534, agentapi.Attachment{PartID: 901}); err == nil {
		t.Fatal("expected error when the part cannot be fetched")
	}
	if _, _, err := attachmentData(context.Background(), nc.agent, 534, agentapi.Attachment{Base64: "!!not-base64!!"}); err == nil {
		t.Fatal("expected error for invalid inline base64")
	}
	data, contentType, err := attachmentData(context.Background(), nc.agent, 534, agentapi.Attachment{
		ContentType: "image/png",
		Base64:      base64.StdEncoding.EncodeToString([]byte("png")),
	})
	if err != nil || string(data) != "png" || contentType != "image/png" {
		t.Fatalf("inline data = %q, %q, %v", data, contentType, err)
	}
	// App-paired logins have no agent client: inline data must work, and a
	// missing inline payload must return an error instead of panicking on the
	// nil client.
	data, contentType, err = attachmentData(context.Background(), nil, 534, agentapi.Attachment{
		ContentType: "image/png",
		PartID:      901,
		Base64:      base64.StdEncoding.EncodeToString([]byte("png")),
	})
	if err != nil || string(data) != "png" || contentType != "image/png" {
		t.Fatalf("inline data with nil agent = %q, %q, %v", data, contentType, err)
	}
	if _, _, err = attachmentData(context.Background(), nil, 534, agentapi.Attachment{PartID: 901}); err == nil {
		t.Fatal("expected error when there is no inline data and no agent client")
	}
}

// TestBuildConvertedMessageMultiPartMMS covers a multi-part MMS with a
// caption: every attachment becomes its own event part, the caption is only
// put on the first part and only the first part carries the message metadata
// (bridgev2 stores that as the message row).
func TestBuildConvertedMessageMultiPartMMS(t *testing.T) {
	t.Parallel()
	nc := newMediaTestClient(t, false)
	intent := &fakeMediaIntent{}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'j', 'p', 'e', 'g'}
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A}

	converted, err := nc.buildConvertedMessage(context.Background(), testPortal(), intent, agentapi.Message{
		ID: "mms:1:534", ThreadID: 12, Body: "look at these",
		Attachments: []agentapi.Attachment{
			{PartID: 901, ContentType: "image/jpeg", FileName: "one.jpg", Base64: base64.StdEncoding.EncodeToString(jpeg)},
			{PartID: 902, ContentType: "image/png", FileName: "two.png", Base64: base64.StdEncoding.EncodeToString(png)},
		},
	})
	if err != nil {
		t.Fatalf("buildConvertedMessage() error: %v", err)
	}
	if len(converted.Parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(converted.Parts))
	}
	first, second := converted.Parts[0], converted.Parts[1]
	if first.Content.MsgType != event.MsgImage || first.Content.Body != "look at these" || first.Content.FileName != "one.jpg" {
		t.Fatalf("first part = %+v", first.Content)
	}
	if second.Content.MsgType != event.MsgImage || second.Content.Body != "two.png" || second.Content.FileName != "two.png" {
		t.Fatalf("second part = %+v", second.Content)
	}
	if second.DBMetadata != nil {
		t.Fatalf("second part carries metadata: %+v", second.DBMetadata)
	}
	meta, _ := first.DBMetadata.(*MessageMetadata)
	if meta == nil || meta.Body != "look at these" || !meta.HasAttachment {
		t.Fatalf("first part metadata = %+v", meta)
	}
	if want := []string{"one.jpg|image/jpeg", "two.png|image/png"}; !slices.Equal(intent.uploads, want) {
		t.Fatalf("uploads = %v, want %v", intent.uploads, want)
	}
	if !bytes.Equal(intent.lastUploaded, png) {
		t.Fatalf("last uploaded bytes = %v, want the second part", intent.lastUploaded)
	}
}

// TestBuildConvertedMessageMultiPartAttachmentOnly covers an attachment-only
// multi-part MMS: each part is named after its file and the message metadata
// body lists all attachments.
func TestBuildConvertedMessageMultiPartAttachmentOnly(t *testing.T) {
	t.Parallel()
	nc := newMediaTestClient(t, false)
	converted, err := nc.buildConvertedMessage(context.Background(), testPortal(), &fakeMediaIntent{}, agentapi.Message{
		ID: "mms:1:534", ThreadID: 12,
		Attachments: []agentapi.Attachment{
			{PartID: 901, ContentType: "image/jpeg", FileName: "one.jpg", Base64: base64.StdEncoding.EncodeToString([]byte("jpeg"))},
			{PartID: 902, ContentType: "image/png", FileName: "two.png", Base64: base64.StdEncoding.EncodeToString([]byte("png"))},
		},
	})
	if err != nil {
		t.Fatalf("buildConvertedMessage() error: %v", err)
	}
	if len(converted.Parts) != 2 || converted.Parts[0].Content.Body != "one.jpg" || converted.Parts[1].Content.Body != "two.png" {
		t.Fatalf("unexpected parts: %+v", converted.Parts)
	}
	meta, _ := converted.Parts[0].DBMetadata.(*MessageMetadata)
	if meta == nil || meta.Body != "one.jpg, two.png" || !meta.HasAttachment {
		t.Fatalf("attachment-only metadata = %+v", meta)
	}
}

// TestBuildConvertedMessageSkipsUnreadablePart covers a partially broken
// multi-part MMS: the readable parts must still be bridged.
func TestBuildConvertedMessageSkipsUnreadablePart(t *testing.T) {
	t.Parallel()
	nc := newMediaTestClient(t, false)
	intent := &fakeMediaIntent{}
	converted, err := nc.buildConvertedMessage(context.Background(), testPortal(), intent, agentapi.Message{
		ID: "mms:1:534", ThreadID: 12, Body: "caption survives",
		Attachments: []agentapi.Attachment{
			{PartID: 901, ContentType: "image/jpeg", FileName: "one.jpg", Base64: base64.StdEncoding.EncodeToString([]byte("jpeg"))},
			{PartID: 902, ContentType: "image/png", FileName: "two.png", Base64: "!!not-base64!!"},
		},
	})
	if err != nil {
		t.Fatalf("buildConvertedMessage() error: %v", err)
	}
	if len(converted.Parts) != 1 || converted.Parts[0].Content.Body != "caption survives" {
		t.Fatalf("unexpected parts: %+v", converted.Parts)
	}
	if want := []string{"one.jpg|image/jpeg"}; !slices.Equal(intent.uploads, want) {
		t.Fatalf("uploads = %v, want only the readable part", intent.uploads)
	}
	meta, _ := converted.Parts[0].DBMetadata.(*MessageMetadata)
	if meta == nil || !meta.HasAttachment {
		t.Fatalf("metadata after a skipped part = %+v", meta)
	}
}

// TestBuildConvertedMessageAllPartsUnreadable covers an MMS whose bytes cannot
// be obtained at all: the fallback must keep the message and name the missing
// media, instead of failing the conversion (which would make bridgev2 post a
// "processing error" notice again on every replay).
func TestBuildConvertedMessageAllPartsUnreadable(t *testing.T) {
	t.Parallel()
	nc := newMediaTestClient(t, false)
	converted, err := nc.buildConvertedMessage(context.Background(), testPortal(), &fakeMediaIntent{}, agentapi.Message{
		ID: "mms:1:534", ThreadID: 12,
		Attachments: []agentapi.Attachment{
			{PartID: 901, ContentType: "image/jpeg", FileName: "one.jpg", Base64: "!!bad!!"},
			{PartID: 902, ContentType: "image/png", FileName: "two.png", Base64: "!!bad!!"},
		},
	})
	if err != nil {
		t.Fatalf("buildConvertedMessage() error: %v", err)
	}
	if len(converted.Parts) != 1 || converted.Parts[0].Content.MsgType != event.MsgText {
		t.Fatalf("expected a text fallback, got %+v", converted.Parts)
	}
	want := "[MMS media could not be downloaded: one.jpg, two.png]"
	if converted.Parts[0].Content.Body != want {
		t.Fatalf("fallback body = %q, want %q", converted.Parts[0].Content.Body, want)
	}
}

// TestBuildConvertedMessageNoContentMirrorsInsteadOfFailing is a regression test
// for replayed media events: a message without text or media must still produce
// a part, because a conversion error stores no message row and therefore makes
// bridgev2 repeat its generic error notice on every replay.
func TestBuildConvertedMessageNoContentMirrorsInsteadOfFailing(t *testing.T) {
	t.Parallel()
	nc := newMediaTestClient(t, false)
	converted, err := nc.buildConvertedMessage(context.Background(), testPortal(), &fakeMediaIntent{}, agentapi.Message{
		ID: "sms:1:99", ThreadID: 12,
	})
	if err != nil {
		t.Fatalf("buildConvertedMessage() error: %v", err)
	}
	if len(converted.Parts) != 1 || converted.Parts[0].Content.MsgType != event.MsgText || converted.Parts[0].Content.Body != noContentBody {
		t.Fatalf("unexpected fallback: %+v", converted.Parts[0].Content)
	}
	meta, _ := converted.Parts[0].DBMetadata.(*MessageMetadata)
	if meta == nil || meta.Body != noContentBody {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
}

// TestMissingMediaBody covers the single clear marker used when MMS media could
// not be downloaded: the caption is kept, never replaced.
func TestMissingMediaBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		caption string
		names   []string
		want    string
	}{
		{"caption and names", "look", []string{"one.jpg", "two.png"}, "look\n\n[MMS media could not be downloaded: one.jpg, two.png]"},
		{"caption only", "look", nil, "look\n\n[MMS media could not be downloaded]"},
		{"names only", "", []string{"one.jpg"}, "[MMS media could not be downloaded: one.jpg]"},
		{"nothing known", "  ", nil, "[MMS media could not be downloaded]"},
	}
	for _, test := range tests {
		if got := missingMediaBody(test.caption, test.names); got != test.want {
			t.Errorf("%s: missingMediaBody(%q, %v) = %q, want %q", test.name, test.caption, test.names, got, test.want)
		}
	}
}

// TestSyntheticMessageFromEvent covers the fallback payload for app events that
// arrive without their message: the preview is kept and a media marker is added
// only when the phone reported an attachment the bridge did not receive.
func TestSyntheticMessageFromEvent(t *testing.T) {
	t.Parallel()
	plain := syntheticMessageFromEvent(remoteEvent{
		Event: agentapi.Event{Type: "message.new", MessageID: "sms:1:7", ThreadID: 3, SubID: 1, Sender: "+15551234567", Preview: "hi"},
	})
	if plain.Body != "hi" || plain.ID != "sms:1:7" || plain.Sender != "+15551234567" {
		t.Fatalf("unexpected synthetic message: %+v", plain)
	}

	media := syntheticMessageFromEvent(remoteEvent{
		Event: agentapi.Event{Type: "message.new", MessageID: "mms:1:534", ThreadID: 12, SubID: 1, IsMMS: true, HasAttachment: true, Preview: "look"},
	})
	if media.Body != "look\n\n[MMS media could not be downloaded]" {
		t.Fatalf("synthetic media body = %q", media.Body)
	}

	mediaNoCaption := syntheticMessageFromEvent(remoteEvent{
		Event: agentapi.Event{Type: "message.new", MessageID: "mms:1:535", ThreadID: 12, SubID: 1, IsMMS: true, HasAttachment: true},
	})
	if mediaNoCaption.Body != "[MMS media could not be downloaded]" {
		t.Fatalf("synthetic media body without caption = %q", mediaNoCaption.Body)
	}
}

// TestHandlePayloadlessEventRequestsPayload covers the replay path: an event
// without a payload that reports media makes the bridge ask the app for the
// message instead of mirroring a media-less copy. Repeated events do not queue
// a second request while one is in flight, a request that was never answered is
// retried later (a queue reset can swallow the command), and a request the app
// rejected stays resolved.
func TestHandlePayloadlessEventRequestsPayload(t *testing.T) {
	t.Parallel()
	device := newPairedDevice(nil, "android-payload")
	nc := &MyNetworkClient{log: zerolog.Nop(), device: device}
	ev := remoteEvent{
		Event: agentapi.Event{
			Type: "message.new", MessageID: "mms:1:534", ThreadID: 12, SubID: 1,
			IsMMS: true, HasAttachment: true, Preview: "look",
		},
	}

	nc.handlePayloadlessEvent(context.Background(), ev)

	device.mu.Lock()
	commands := append([]pairCommand(nil), device.commands...)
	device.mu.Unlock()
	if len(commands) != 1 || commands[0].Type != "fetch_message" || commands[0].MessageID != "mms:1:534" {
		t.Fatalf("expected one fetch_message command, got %+v", commands)
	}

	// A replayed event for the same message must not queue another request.
	nc.handlePayloadlessEvent(context.Background(), ev)
	device.mu.Lock()
	count := len(device.commands)
	device.mu.Unlock()
	if count != 1 {
		t.Fatalf("duplicate replay queued another command: %d commands", count)
	}

	// A replay after the retry window asks again: a queue reset can swallow the
	// pending command, so the request must not wait forever.
	device.mu.Lock()
	device.payloadRequests["mms:1:534"].requestedAt = time.Now().Add(-2 * pairingPayloadRetryAfter)
	device.mu.Unlock()
	if state := nc.device.requestPayload(ev); state != payloadRequested {
		t.Fatalf("stale requestPayload() = %v, want payloadRequested", state)
	}

	// Once the app reports that it cannot provide the payload, the request is
	// resolved as unavailable exactly once.
	if _, first := device.failPayload("mms:1:534"); !first {
		t.Fatal("first failPayload() call should return the event to mirror")
	}
	if _, first := device.failPayload("mms:1:534"); first {
		t.Fatal("second failPayload() call should not mirror the event again")
	}
	if state := nc.device.requestPayload(ev); state != payloadUnavailable {
		t.Fatalf("requestPayload() after a failure = %v, want payloadUnavailable", state)
	}

	// The message payload of an unrelated (or already answered) request is a
	// no-op.
	device.clearPayload("mms:1:534")
	if _, first := device.failPayload("mms:1:534"); first {
		t.Fatal("cleared payload request must not resurface")
	}
	if got := nc.device.requestPayload(remoteEvent{Event: agentapi.Event{MessageID: ""}}); got != payloadUnavailable {
		t.Fatalf("requestPayload without a message id = %v, want payloadUnavailable", got)
	}
}

func TestExtensionForContentType(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"image/jpeg":                      ".jpg",
		"IMAGE/JPEG; charset=binary":      ".jpg",
		"image/jpg":                       ".jpg",
		"image/png":                       ".png",
		"image/gif":                       ".gif",
		"image/webp":                      ".webp",
		"video/mp4":                       ".mp4",
		"video/3gpp":                      ".3gp",
		"audio/mpeg":                      ".mp3",
		"audio/amr":                       ".amr",
		"audio/ogg":                       ".ogg",
		"text/plain":                      ".txt",
		"application/pdf":                 ".pdf",
		"application/pdf; charset=binary": ".pdf",
		"":                                "",
	}
	for contentType, want := range tests {
		if got := extensionForContentType(contentType); got != want {
			t.Errorf("extensionForContentType(%q) = %q, want %q", contentType, got, want)
		}
	}
}

// TestUploadAttachmentData covers the metadata attached to uploaded MMS
// parts: the mxc URI, the file info (mime type and size) and the defaults for
// missing content type / file name.
func TestUploadAttachmentData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, _, _, err := uploadAttachmentData(ctx, nil, "!room:example.com", []byte("x"), "f.jpg", "image/jpeg"); !errors.Is(err, errNoIntent) {
		t.Fatalf("uploadAttachmentData(nil intent) error = %v, want errNoIntent", err)
	}

	intent := &fakeMediaIntent{}
	mxc, enc, info, err := uploadAttachmentData(ctx, intent, "!room:example.com", []byte("abc"), "photo.jpg", "image/jpeg")
	if err != nil {
		t.Fatalf("uploadAttachmentData() error: %v", err)
	}
	if mxc != "mxc://example.com/photo.jpg" || enc != nil {
		t.Fatalf("uploadAttachmentData() = (%q, %v)", mxc, enc)
	}
	if info == nil || info.MimeType != "image/jpeg" || info.Size != 3 {
		t.Fatalf("file info = %+v, want image/jpeg with size 3", info)
	}

	// Missing content type and file name fall back to octet-stream and a
	// generated "attachment" name.
	_, _, info, err = uploadAttachmentData(ctx, intent, "", []byte("ab"), "", "")
	if err != nil {
		t.Fatalf("uploadAttachmentData(defaults) error: %v", err)
	}
	if info.MimeType != "application/octet-stream" || info.Size != 2 {
		t.Fatalf("default file info = %+v", info)
	}
	if last := intent.uploads[len(intent.uploads)-1]; !strings.HasPrefix(last, "attachment") {
		t.Fatalf("default file name = %q, want an attachment name", last)
	}
}

func TestBuildRoomFeatures(t *testing.T) {
	unsupported := buildRoomFeatures(false)
	if unsupported.File[event.MsgImage].MimeTypes["*/*"] != event.CapLevelUnsupported {
		t.Error("files should be unsupported when MMS send is unavailable")
	}
	if unsupported.Reaction != event.CapLevelPartialSupport || unsupported.Reply != event.CapLevelPartialSupport {
		t.Error("unexpected reaction/reply capabilities")
	}
	supported := buildRoomFeatures(true)
	image := supported.File[event.MsgImage]
	if image.MimeTypes["*/*"] != event.CapLevelFullySupported || image.Caption != event.CapLevelFullySupported {
		t.Error("files and captions should be fully supported when MMS send is available")
	}
	if supported.File[event.CapMsgVoice] == nil || supported.File[event.MsgFile] == nil {
		t.Error("voice and file message types should be advertised")
	}
}
