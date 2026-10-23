// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

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
	if converted.Parts[0].Content.Body != "text survives" {
		t.Fatalf("fallback body = %q, want the original text", converted.Parts[0].Content.Body)
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
