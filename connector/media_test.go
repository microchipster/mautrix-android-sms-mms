// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"encoding/base64"
	"testing"

	"maunium.net/go/mautrix/event"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

func TestClassifyContentTypeMessageType(t *testing.T) {
	tests := []struct {
		contentType string
		asFile      bool
		want        event.MessageType
	}{
		{"image/jpeg", true, event.MsgImage},
		{"image/png; charset=binary", true, event.MsgImage},
		{"video/mp4", true, event.MsgVideo},
		{"audio/amr", true, event.MsgAudio},
		{"application/pdf", true, event.MsgFile},
		{"text/plain", true, event.MsgText},
		{"application/octet-stream", false, event.MsgText},
		{"", true, event.MsgText},
	}
	for _, tt := range tests {
		if got := classifyContentTypeMessageType(tt.contentType, tt.asFile); got != tt.want {
			t.Errorf("classifyContentTypeMessageType(%q, %t) = %q, want %q", tt.contentType, tt.asFile, got, tt.want)
		}
	}
}

func TestSanitizeFileName(t *testing.T) {
	tests := map[string]string{
		"photo.jpg":            "photo.jpg",
		"../../etc/passwd":     "passwd",
		`C:\Users\a\photo.jpg`: "photo.jpg",
		"  spaced.png  ":       "spaced.png",
		"":                     "",
		"...":                  "",
		"with\nnewline.jpg":    "withnewline.jpg",
		"very/weird\\name.vcf": "name.vcf",
	}
	for input, want := range tests {
		if got := sanitizeFileName(input); got != want {
			t.Errorf("sanitizeFileName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAttachmentFileName(t *testing.T) {
	if got := attachmentFileName(agentapi.Attachment{FileName: "photo.jpg"}); got != "photo.jpg" {
		t.Errorf("attachmentFileName(named) = %q", got)
	}
	if got := attachmentFileName(agentapi.Attachment{ContentType: "image/jpeg"}); got != "attachment.jpg" {
		t.Errorf("attachmentFileName(jpeg) = %q", got)
	}
	if got := attachmentFileName(agentapi.Attachment{}); got != "attachment" {
		t.Errorf("attachmentFileName(unknown) = %q", got)
	}
}

func TestBodyFromFileName(t *testing.T) {
	if got := bodyFromFileName("photo.jpg"); got != "photo.jpg" {
		t.Errorf("bodyFromFileName() = %q", got)
	}
	if got := bodyFromFileName(""); got != "(attachment)" {
		t.Errorf("bodyFromFileName(empty) = %q", got)
	}
}

func TestAttachmentDataURIless(t *testing.T) {
	if got := attachmentDataURIless([]byte("jpeg")); got != base64.StdEncoding.EncodeToString([]byte("jpeg")) {
		t.Errorf("attachmentDataURIless() = %q", got)
	}
}
