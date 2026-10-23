// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Server exposes the agent HTTP API.
type Server struct {
	cfg    *Config
	prov   Provider
	state  *State
	log    *EventLog
	sender *Sender
	health Health
	zlog   zerolog.Logger
}

// NewServer creates the API server.
func NewServer(cfg *Config, prov Provider, state *State, log *EventLog, sender *Sender, health Health, zlog zerolog.Logger) *Server {
	return &Server{cfg: cfg, prov: prov, state: state, log: log, sender: sender, health: health, zlog: zlog}
}

// Handler returns the authenticated HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/subscriptions", s.handleSubscriptions)
	mux.HandleFunc("GET /v1/threads", s.handleThreads)
	mux.HandleFunc("GET /v1/threads/{id}", s.handleThread)
	mux.HandleFunc("GET /v1/threads/{id}/messages", s.handleThreadMessages)
	mux.HandleFunc("GET /v1/messages/{id}", s.handleMessage)
	mux.HandleFunc("GET /v1/mms/{id}/parts/{part_id}", s.handlePart)
	mux.HandleFunc("POST /v1/read", s.handleRead)
	mux.HandleFunc("POST /v1/send", s.handleSend)
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("GET /v1/contacts", s.handleContacts)
	return s.withAuth(mux)
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" || s.cfg.Token == "" {
			next.ServeHTTP(w, r)
			return
		}
		header := r.Header.Get("Authorization")
		token := strings.TrimPrefix(header, "Bearer ")
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.health)
}

func (s *Server) handleSubscriptions(w http.ResponseWriter, r *http.Request) {
	subs, err := s.prov.Subscriptions(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "agent_degraded", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, subs)
}

func (s *Server) handleThreads(w http.ResponseWriter, r *http.Request) {
	threads, err := s.prov.Threads(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "agent_degraded", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, threads)
}

func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "not_found", "invalid thread id")
		return
	}
	recipients := s.prov.ThreadRecipients(r.Context(), id)
	if recipients == nil {
		writeErr(w, http.StatusNotFound, "not_found", fmt.Sprintf("thread %d not found", id))
		return
	}
	writeJSON(w, http.StatusOK, Thread{ID: id, SubID: -1, Recipients: recipients})
}

func (s *Server) handleThreadMessages(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "not_found", "invalid thread id")
		return
	}
	q := r.URL.Query()
	limit := s.cfg.HistoryLimit
	if v := q.Get("limit"); v != "" {
		limit = atoi(v)
	}
	beforeMS, _ := strconv.ParseInt(q.Get("before_ms"), 10, 64)
	afterMS, _ := strconv.ParseInt(q.Get("after_ms"), 10, 64)
	msgs, err := s.prov.ThreadMessages(r.Context(), id, beforeMS, afterMS, limit)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "agent_degraded", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	msg, err := s.prov.Message(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

func (s *Server) handlePart(w http.ResponseWriter, r *http.Request) {
	mmsID, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	partID, err2 := strconv.ParseInt(r.PathValue("part_id"), 10, 64)
	if err1 != nil || err2 != nil {
		writeErr(w, http.StatusBadRequest, "not_found", "invalid ids")
		return
	}
	data, contentType, err := s.prov.PartBytes(r.Context(), mmsID, partID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ThreadID   int64    `json:"thread_id"`
		UpToMS     int64    `json:"up_to_ms"`
		MessageIDs []string `json:"message_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "not_found", "invalid JSON body")
		return
	}
	updated := 0
	mark := func(threadID, upToMS int64) {
		n, err := s.prov.MarkRead(r.Context(), threadID, upToMS)
		if err != nil {
			s.zlog.Warn().Err(err).Int64("thread_id", threadID).Msg("mark read failed")
			return
		}
		updated += n
	}
	if req.ThreadID != 0 {
		mark(req.ThreadID, req.UpToMS)
	} else {
		for _, messageID := range req.MessageIDs {
			msg, err := s.prov.Message(r.Context(), messageID)
			if err != nil {
				continue
			}
			mark(msg.ThreadID, msg.TimestampMS)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated, "supported": true})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SendID      string   `json:"send_id"`
		ThreadID    int64    `json:"thread_id"`
		SubID       int      `json:"sub_id"`
		Recipients  []string `json:"recipients"`
		Text        string   `json:"text"`
		Attachments []struct {
			ContentType string `json:"content_type"`
			FileName    string `json:"file_name"`
			Base64      string `json:"base64"`
		} `json:"attachments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "not_found", "invalid JSON body")
		return
	}
	if len(req.Attachments) > 0 {
		writeErr(w, http.StatusNotImplemented, "mms_send_unsupported",
			"the shell backend cannot send MMS; use the app backend (Fossify fork) or install a supported send backend")
		return
	}
	if req.SendID == "" {
		req.SendID = fmt.Sprintf("agent-%d", time.Now().UnixNano())
	}
	result, err := s.sender.SendSMS(r.Context(), req.SendID, req.SubID, req.Recipients, req.Text)
	if err != nil {
		code := "send_failed"
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, ErrMMSUnsupported):
			code, status = "mms_send_unsupported", http.StatusNotImplemented
		case errors.Is(err, ErrGroupSMS):
			code = "unsupported"
		case errors.Is(err, ErrMessageTooLong):
			code = "too_long"
		}
		writeErr(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"send_id":    req.SendID,
		"message_id": result.MessageID,
		"thread_id":  result.ThreadID,
		"parts_sent": result.PartsSent,
		"pending":    result.Pending,
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	timeoutMS := 25000
	if v := r.URL.Query().Get("timeout_ms"); v != "" {
		timeoutMS = atoi(v)
	}
	if timeoutMS <= 0 || timeoutMS > 30000 {
		timeoutMS = 25000
	}
	events, cursor, reset := s.log.Wait(r.Context(), since, time.Duration(timeoutMS)*time.Millisecond)
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "cursor": cursor, "reset": reset})
}

func (s *Server) handleContacts(w http.ResponseWriter, _ *http.Request) {
	writeErr(w, http.StatusNotImplemented, "unsupported", "contact lookup is not implemented yet")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}
