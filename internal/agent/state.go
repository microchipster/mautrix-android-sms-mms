// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// snapshot is the last observed state of a message, used to decide between
// "new" and "updated" events and to suppress duplicates from carrier retries.
type snapshot struct {
	TS int64  `json:"ts"`
	V  string `json:"v"`
}

// Echo describes a message this agent sent, so the provider row appearing later
// can be recognized and not reported back to the bridge as a new message.
type Echo struct {
	SendID   string `json:"send_id"`
	Address  string `json:"address"`
	BodyHash string `json:"body_hash,omitempty"`
	TS       int64  `json:"ts"`
	ThreadID int64  `json:"thread_id,omitempty"`
	SentMS   int64  `json:"sent_ms,omitempty"`
	Resolved string `json:"resolved,omitempty"` // message id once seen
}

type stateData struct {
	SMSWatermarkMS  int64               `json:"sms_watermark_ms"`
	MMSWatermarkMS  int64               `json:"mms_watermark_ms"`
	Snapshots       map[string]snapshot `json:"snapshots"`
	ThreadSnapshots map[string]snapshot `json:"thread_snapshots"`
	Echoes          []Echo              `json:"echoes"`
}

// State is the agent's persistent local state (watermarks, dedupe, echoes).
type State struct {
	path string
	mu   sync.Mutex
	data stateData
}

// OpenState loads (or creates) the state file in dir.
func OpenState(dir string) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	st := &State{path: filepath.Join(dir, "state.json")}
	st.data = stateData{
		Snapshots:       map[string]snapshot{},
		ThreadSnapshots: map[string]snapshot{},
	}
	data, err := os.ReadFile(st.path)
	if err == nil {
		_ = json.Unmarshal(data, &st.data)
		if st.data.Snapshots == nil {
			st.data.Snapshots = map[string]snapshot{}
		}
		if st.data.ThreadSnapshots == nil {
			st.data.ThreadSnapshots = map[string]snapshot{}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read state: %w", err)
	}
	return st, nil
}

// Save writes the state file atomically. Callers should hold no locks.
func (s *State) Save() error {
	s.mu.Lock()
	data, err := json.Marshal(&s.data)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Watermarks returns the SMS (ms) and MMS (ms) sweep watermarks.
func (s *State) Watermarks() (int64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.SMSWatermarkMS, s.data.MMSWatermarkMS
}

// SetWatermarks updates the sweep watermarks if the new values are newer.
func (s *State) SetWatermarks(sms, mms int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sms > s.data.SMSWatermarkMS {
		s.data.SMSWatermarkMS = sms
	}
	if mms > s.data.MMSWatermarkMS {
		s.data.MMSWatermarkMS = mms
	}
}

// Snapshot returns the last observed state for a message.
func (s *State) Snapshot(id string) (snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.data.Snapshots[id]
	return snap, ok
}

// PutSnapshot records the observed state for a message.
func (s *State) PutSnapshot(id string, ts int64, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Snapshots[id] = snapshot{TS: ts, V: value}
}

// ThreadSnapshot returns the last observed state for a thread.
func (s *State) ThreadSnapshot(id int64) (snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.data.ThreadSnapshots[fmt.Sprintf("thread:%d", id)]
	return snap, ok
}

// PutThreadSnapshot records the observed state for a thread.
func (s *State) PutThreadSnapshot(id int64, ts int64, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.ThreadSnapshots[fmt.Sprintf("thread:%d", id)] = snapshot{TS: ts, V: value}
}

// PruneSnapshots drops snapshots older than the retention window.
func (s *State) PruneSnapshots(olderThanMS int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, snap := range s.data.Snapshots {
		if snap.TS > 0 && snap.TS < olderThanMS {
			delete(s.data.Snapshots, id)
		}
	}
	for id, snap := range s.data.ThreadSnapshots {
		if snap.TS > 0 && snap.TS < olderThanMS {
			delete(s.data.ThreadSnapshots, id)
		}
	}
	// Drop resolved/old echoes.
	kept := s.data.Echoes[:0]
	for _, echo := range s.data.Echoes {
		if echo.Resolved != "" && echo.TS < olderThanMS {
			continue
		}
		kept = append(kept, echo)
	}
	s.data.Echoes = kept
}

// AddEcho records a message this agent sent.
func (s *State) AddEcho(echo Echo) {
	if echo.TS == 0 {
		echo.TS = time.Now().UnixMilli()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Echoes = append(s.data.Echoes, echo)
}

// MatchEcho finds and marks an echo matching an outgoing provider row.
func (s *State) MatchEcho(body string, timestampMS int64, recipients []string) (Echo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Echoes {
		echo := &s.data.Echoes[i]
		if echo.Resolved != "" {
			continue
		}
		if body != "" && echo.BodyHash != "" && echo.BodyHash != sha256Hex(body) {
			continue
		}
		delta := timestampMS - echo.TS
		if delta < -10000 || delta > 5*60*1000 {
			continue
		}
		if echo.Address != "" && len(recipients) > 0 && !containsAddress(recipients, echo.Address) {
			continue
		}
		return *echo, true
	}
	return Echo{}, false
}

// ResolveEcho records the provider message id for a previously matched echo.
func (s *State) ResolveEcho(sendID, messageID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Echoes {
		if s.data.Echoes[i].SendID == sendID {
			s.data.Echoes[i].Resolved = messageID
			if s.data.Echoes[i].SentMS == 0 {
				s.data.Echoes[i].SentMS = time.Now().UnixMilli()
			}
		}
	}
}

// EchoMessageID returns the resolved message id for a send_id, if known.
func (s *State) EchoMessageID(sendID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, echo := range s.data.Echoes {
		if echo.SendID == sendID {
			return echo.Resolved
		}
	}
	return ""
}

// PendingEchoes returns echoes waiting to be resolved.
func (s *State) PendingEchoes() []Echo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Echo
	for _, echo := range s.data.Echoes {
		if echo.Resolved == "" {
			out = append(out, echo)
		}
	}
	return out
}

// SeenDuplicate records and checks content-based dedupe keys (carrier retries
// can persist the same message twice as separate rows).
func (s *State) SeenDuplicate(key string, ts int64, retentionMS int64) bool {
	key = "dup:" + key
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap, ok := s.data.Snapshots[key]; ok && ts-snap.TS < retentionMS {
		return true
	}
	s.data.Snapshots[key] = snapshot{TS: ts}
	return false
}

func containsAddress(list []string, address string) bool {
	address = CanonicalAddress(address)
	for _, v := range list {
		if CanonicalAddress(v) == address {
			return true
		}
	}
	return false
}

// stateKey builds the dedupe key used in snapshots.
func stateKey(messageID string) string { return strings.TrimSpace(messageID) }
