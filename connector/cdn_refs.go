// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

// This file records the file-service links that pass through an account's
// messages (sent and received), plus the links the bridge creates for outgoing
// attachments, so the management room can list, inspect and expire both the
// account's own uploads and the links it has received. The fragment key and
// the entry TTL remain the only access controls on the service itself; these
// records are a convenience for the account.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Link directions of one recorded file-service link.
const (
	cdnRefDirectionSent     = "sent"
	cdnRefDirectionReceived = "received"
)

// cdnRefMaxPerAccount caps how many link records one account keeps; older
// records are dropped first.
const cdnRefMaxPerAccount = 500

// CdnRef is one file-service link seen in an account's messages or created
// by the bridge for it. URL carries the link exactly as relayed, including
// the fragment key.
type CdnRef struct {
	ID        string    `json:"id"`
	URL       string    `json:"url,omitempty"`
	Direction string    `json:"direction"`
	Party     string    `json:"party,omitempty"`
	RoomID    string    `json:"room_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// refsDir returns the directory holding the per-account link records.
func (s *CdnService) refsDir() string {
	return filepath.Join(s.objectDir(), "refs")
}

// refsPath is the record file of one account. The owner is hashed so no
// account-controlled text lands in a path.
func (s *CdnService) refsPath(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return filepath.Join(s.refsDir(), hex.EncodeToString(sum[:])+".json")
}

// RecordRef stores or refreshes one link record for an account. The first
// sighting time is kept when the same link is seen again.
func (s *CdnService) RecordRef(owner string, ref CdnRef) {
	if s == nil {
		return
	}
	owner = strings.TrimSpace(owner)
	ref.ID = strings.TrimSpace(ref.ID)
	if owner == "" || !validCdnID(ref.ID) {
		return
	}
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	refs, err := s.readRefsLocked(owner)
	if err != nil {
		return
	}
	kept := refs[:0]
	for _, existing := range refs {
		if existing.ID == ref.ID {
			if !existing.CreatedAt.IsZero() {
				ref.CreatedAt = existing.CreatedAt
			}
			continue
		}
		kept = append(kept, existing)
	}
	refs = append(kept, ref)
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].CreatedAt.Before(refs[j].CreatedAt) })
	if len(refs) > cdnRefMaxPerAccount {
		refs = refs[len(refs)-cdnRefMaxPerAccount:]
	}
	_ = s.writeRefsLocked(owner, refs)
}

// Refs returns an account's link records, newest first.
func (s *CdnService) Refs(owner string) []CdnRef {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	refs, err := s.readRefsLocked(strings.TrimSpace(owner))
	if err != nil {
		return nil
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].CreatedAt.After(refs[j].CreatedAt) })
	return refs
}

// Ref returns one recorded link of an account.
func (s *CdnService) Ref(owner, id string) (CdnRef, bool) {
	for _, ref := range s.Refs(owner) {
		if ref.ID == id {
			return ref, true
		}
	}
	return CdnRef{}, false
}

// ForgetRef drops one recorded link of an account.
func (s *CdnService) ForgetRef(owner, id string) bool {
	if s == nil {
		return false
	}
	owner = strings.TrimSpace(owner)
	id = strings.TrimSpace(id)
	if owner == "" || !validCdnID(id) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	refs, err := s.readRefsLocked(owner)
	if err != nil {
		return false
	}
	found := false
	kept := refs[:0]
	for _, ref := range refs {
		if ref.ID == id {
			found = true
			continue
		}
		kept = append(kept, ref)
	}
	if !found {
		return false
	}
	_ = s.writeRefsLocked(owner, kept)
	return true
}

// readRefsLocked loads one account's records. Callers must hold s.mu.
func (s *CdnService) readRefsLocked(owner string) ([]CdnRef, error) {
	if owner == "" {
		return nil, nil
	}
	data, err := os.ReadFile(s.refsPath(owner))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var refs []CdnRef
	if err := json.Unmarshal(data, &refs); err != nil {
		return nil, err
	}
	return refs, nil
}

// writeRefsLocked writes one account's records atomically. Callers must
// hold s.mu.
func (s *CdnService) writeRefsLocked(owner string, refs []CdnRef) error {
	dir := s.refsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(refs)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".tmp-refs-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	return os.Rename(tempName, s.refsPath(owner))
}

// Delete removes one stored object and its metadata regardless of its owner.
// The management room uses it to expire links an account received, which the
// account itself cannot revoke through the owner-checked API.
func (s *CdnService) Delete(id string) (bool, error) {
	if s == nil {
		return false, nil
	}
	id = strings.TrimSpace(id)
	if !validCdnID(id) {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, err := s.readMeta(id)
	if err != nil {
		return false, err
	}
	if entry == nil {
		return false, nil
	}
	return true, s.removeEntryLocked(id)
}

// cdnRefsFromText extracts every file-service link (with its fragment key,
// when present) from one message text.
func cdnRefsFromText(baseURL, text string) []string {
	prefix := strings.TrimRight(baseURL, "/") + cdnPathPrefix + "/"
	var refs []string
	searchFrom := 0
	for searchFrom < len(text) {
		rel := strings.Index(text[searchFrom:], prefix)
		if rel < 0 {
			break
		}
		start := searchFrom + rel
		idStart := start + len(prefix)
		end := idStart
		for end < len(text) && isCdnRefChar(text[end]) {
			end++
		}
		linkEnd := end
		if end > idStart && validCdnID(text[idStart:end]) {
			if linkEnd < len(text) && text[linkEnd] == '#' {
				fragEnd := linkEnd + 1
				for fragEnd < len(text) && isCdnRefChar(text[fragEnd]) {
					fragEnd++
				}
				if fragEnd > linkEnd+1 {
					linkEnd = fragEnd
				}
			}
			refs = append(refs, text[start:linkEnd])
		}
		searchFrom = end
	}
	return refs
}

// isCdnRefChar reports whether c can appear in a file id or fragment key.
func isCdnRefChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		return true
	}
	return false
}

// cdnRefIDFromLink returns the /f/ id of a recorded link that points at our
// file service, or "" for anything else.
func cdnRefIDFromLink(baseURL, link string) string {
	prefix := strings.TrimRight(baseURL, "/") + cdnPathPrefix + "/"
	if !strings.HasPrefix(link, prefix) {
		return ""
	}
	rest := link[len(prefix):]
	if j := strings.IndexAny(rest, "#/"); j >= 0 {
		rest = rest[:j]
	}
	if validCdnID(rest) {
		return rest
	}
	return ""
}

// recordCdnRefsFromText records the file-service links of one relayed
// message for its account, so the management room can list, view and expire
// both sent and received links.
func (nc *MyNetworkClient) recordCdnRefsFromText(direction, party, roomID, text string, ts time.Time) {
	service := nc.connector.cdnService()
	if service == nil || nc.login == nil || nc.login.User == nil {
		return
	}
	owner := string(nc.login.User.MXID)
	for _, link := range cdnRefsFromText(service.BaseURL(), text) {
		id := cdnRefIDFromLink(service.BaseURL(), link)
		if id == "" {
			continue
		}
		service.RecordRef(owner, CdnRef{
			ID:        id,
			URL:       link,
			Direction: direction,
			Party:     party,
			RoomID:    roomID,
			CreatedAt: ts,
		})
	}
}
