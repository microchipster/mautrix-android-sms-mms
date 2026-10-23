// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// testAgentHandler implements a tiny in-process agent API for agentclient
// tests. The token check mirrors internal/agent/server.go.
func testAgentHandler(t *testing.T, token string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	requireAuth := func(w http.ResponseWriter, r *http.Request) bool {
		if token == "" || r.URL.Path == "/v1/health" {
			return true
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized","message":"missing or invalid bearer token"}`))
			return false
		}
		return true
	}
	writeJSON := func(w http.ResponseWriter, status int, payload any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":               true,
			"protocol_version": 1,
			"device": map[string]any{
				"serial":          "ABC123",
				"model":           "Pixel 8",
				"android_sdk":     35,
				"android_release": "15",
			},
			"root":            false,
			"backend":         map[string]any{"read": "content-provider", "send_sms": "shell", "send_mms": false, "can_mark_read": true},
			"capabilities":    []string{"threads", "messages"},
			"default_sms_app": "com.android.messaging",
		})
	})
	mux.HandleFunc("GET /v1/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, []map[string]any{{"sub_id": 1, "number": "+15550001111", "default_sms": true}})
	})
	mux.HandleFunc("GET /v1/threads", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, []map[string]any{
			{"id": 12, "sub_id": 1, "date_ms": 1730000000000, "recipients": []string{"+15551234567"}},
		})
	})
	mux.HandleFunc("GET /v1/threads/12/messages", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		if got := r.URL.Query().Get("before_ms"); got != "1730000000000" {
			t.Errorf("before_ms = %q, want 1730000000000", got)
		}
		if got := r.URL.Query().Get("after_ms"); got != "1730000000000" {
			t.Errorf("after_ms = %q, want 1730000000000", got)
		}
		if got := r.URL.Query().Get("limit"); got != "25" {
			t.Errorf("limit = %q, want 25", got)
		}
		writeJSON(w, http.StatusOK, []map[string]any{
			{"id": "mms:1:534", "thread_id": 12, "sub_id": 1, "timestamp_ms": 1730000000000, "sender": "+15551234567", "recipients": []string{"+15550001111"}},
		})
	})
	mux.HandleFunc("GET /v1/messages/mms:1:534", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "mms:1:534", "thread_id": 12, "sub_id": 1, "timestamp_ms": 1730000000000,
			"sender": "+15551234567", "is_mms": true,
			"attachments": []map[string]any{{"part_id": 901, "content_type": "image/jpeg", "file_name": "photo.jpg", "size": 4}},
		})
	})
	mux.HandleFunc("GET /v1/messages/bad", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "no such message"})
	})
	mux.HandleFunc("GET /v1/mms/534/parts/901", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("jpeg"))
	})
	mux.HandleFunc("POST /v1/send", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		var req SendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode send request: %v", err)
		}
		if req.SendID != "send-1" || req.ThreadID != 12 || req.Text != "hello" || len(req.Attachments) != 1 {
			t.Errorf("unexpected send request: %+v", req)
		}
		if decoded, err := base64.StdEncoding.DecodeString(req.Attachments[0].Base64); err != nil || string(decoded) != "jpeg" {
			t.Errorf("unexpected attachment bytes: %q err=%v", decoded, err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"send_id": req.SendID, "message_id": "sms:1:8130", "thread_id": 12, "parts_sent": 1, "pending": false})
	})
	mux.HandleFunc("POST /v1/read", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		var req struct {
			ThreadID   int64    `json:"thread_id"`
			UpToMS     int64    `json:"up_to_ms"`
			MessageIDs []string `json:"message_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode read request: %v", err)
		}
		if req.ThreadID != 12 || req.UpToMS != 1730000000000 {
			t.Errorf("unexpected read request: %+v", req)
		}
		writeJSON(w, http.StatusOK, map[string]any{"updated": 2, "supported": true})
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		if got := r.URL.Query().Get("since"); got != "41" {
			t.Errorf("since = %q, want 41", got)
		}
		if got := r.URL.Query().Get("timeout_ms"); got != "25000" {
			t.Errorf("timeout_ms = %q, want 25000", got)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"reset":  false,
			"cursor": 42,
			"events": []map[string]any{{"seq": 42, "type": "message.new", "message_id": "sms:1:99", "thread_id": 12, "sub_id": 1}},
		})
	})
	return mux
}

func newTestAgent(t *testing.T, token string) *AgentClient {
	t.Helper()
	server := httptest.NewServer(testAgentHandler(t, token))
	t.Cleanup(server.Close)
	return NewAgentClient(server.URL, token)
}

func TestAgentClientURLNormalization(t *testing.T) {
	client := NewAgentClient("127.0.0.1:8093/", " tok ")
	if client.BaseURL != "http://127.0.0.1:8093" {
		t.Fatalf("BaseURL = %q", client.BaseURL)
	}
	if client.Token != "tok" {
		t.Fatalf("Token = %q", client.Token)
	}
}

func TestAgentClientHealth(t *testing.T) {
	client := newTestAgent(t, "secret")
	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health() error: %v", err)
	}
	if !health.OK || health.Device.Serial != "ABC123" || health.Device.Model != "Pixel 8" {
		t.Fatalf("unexpected health: %+v", health)
	}
	if health.ProtocolVersion != 1 || health.Backend.SendMMS {
		t.Fatalf("unexpected health backend: %+v", health.Backend)
	}
}

func TestAgentClientBearerAuth(t *testing.T) {
	client := newTestAgent(t, "secret")
	if _, err := client.Subscriptions(context.Background()); err != nil {
		t.Fatalf("Subscriptions() with correct token error: %v", err)
	}

	var apiErr *APIError
	unauthorized := NewAgentClient(client.BaseURL, "wrong")
	if _, err := unauthorized.Subscriptions(context.Background()); !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T (%v)", err, err)
	}
	if apiErr.Status != http.StatusUnauthorized || apiErr.Code != "unauthorized" {
		t.Fatalf("unexpected APIError: %+v", apiErr)
	}
}

func TestAgentClientThreadsAndMessages(t *testing.T) {
	client := newTestAgent(t, "secret")
	ctx := context.Background()

	threads, err := client.Threads(ctx)
	if err != nil {
		t.Fatalf("Threads() error: %v", err)
	}
	if len(threads) != 1 || threads[0].ID != 12 || threads[0].Recipients[0] != "+15551234567" {
		t.Fatalf("unexpected threads: %+v", threads)
	}

	msgs, err := client.ThreadMessages(ctx, 12, 1730000000000, 1730000000000, 25)
	if err != nil {
		t.Fatalf("ThreadMessages() error: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != "mms:1:534" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}

	msg, err := client.Message(ctx, "mms:1:534")
	if err != nil {
		t.Fatalf("Message() error: %v", err)
	}
	if !msg.IsMMS || len(msg.Attachments) != 1 || msg.Attachments[0].PartID != 901 {
		t.Fatalf("unexpected message: %+v", msg)
	}

	var apiErr *APIError
	if _, err := client.Message(ctx, "bad"); !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T (%v)", err, err)
	}
	if apiErr.Status != http.StatusNotFound || apiErr.Code != "not_found" || apiErr.Message != "no such message" {
		t.Fatalf("unexpected APIError: %+v", apiErr)
	}
}

func TestAgentClientPartBytes(t *testing.T) {
	client := newTestAgent(t, "secret")
	data, contentType, err := client.PartBytes(context.Background(), 534, 901)
	if err != nil {
		t.Fatalf("PartBytes() error: %v", err)
	}
	if string(data) != "jpeg" || contentType != "image/jpeg" {
		t.Fatalf("PartBytes() = (%q, %q)", data, contentType)
	}
}

func TestAgentClientSend(t *testing.T) {
	client := newTestAgent(t, "secret")
	resp, err := client.Send(context.Background(), SendRequest{
		SendID:     "send-1",
		ThreadID:   12,
		SubID:      1,
		Recipients: []string{"+15551234567"},
		Text:       "hello",
		Attachments: []SendAttachment{{
			ContentType: "image/jpeg",
			FileName:    "photo.jpg",
			Base64:      base64.StdEncoding.EncodeToString([]byte("jpeg")),
		}},
	})
	if err != nil {
		t.Fatalf("Send() error: %v", err)
	}
	if resp.MessageID != "sms:1:8130" || resp.ThreadID != 12 || resp.Pending {
		t.Fatalf("unexpected send response: %+v", resp)
	}
}

func TestAgentClientReadAndEvents(t *testing.T) {
	client := newTestAgent(t, "secret")
	ctx := context.Background()

	read, err := client.Read(ctx, 12, 1730000000000, nil)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if read.Updated != 2 || !read.Supported {
		t.Fatalf("unexpected read response: %+v", read)
	}

	events, err := client.Events(ctx, 41, 25000)
	if err != nil {
		t.Fatalf("Events() error: %v", err)
	}
	if events.Reset || events.Cursor != 42 || len(events.Events) != 1 {
		t.Fatalf("unexpected events response: %+v", events)
	}
	if events.Events[0].Type != "message.new" || events.Events[0].MessageID != "sms:1:99" {
		t.Fatalf("unexpected event: %+v", events.Events[0])
	}
}

func TestAgentClientNoBaseURL(t *testing.T) {
	client := NewAgentClient("", "")
	if _, err := client.Health(context.Background()); err == nil {
		t.Fatal("Health() without base URL should fail")
	}
}
