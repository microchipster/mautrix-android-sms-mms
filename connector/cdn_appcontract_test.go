// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// appCdnEncrypt replicates the app-side contract from
// docs/cdn-file-service.md: a random 32-byte AES-256 key (unpadded base64url,
// kept in the URL fragment), a random 12-byte nonce, and a body of
// nonce || ciphertext || tag with no additional authenticated data. The
// returned body is exactly what the app would POST to /f.
func appCdnEncrypt(t *testing.T, plaintext []byte) (body []byte, key string) {
	t.Helper()
	rawKey := make([]byte, 32)
	if _, err := rand.Read(rawKey); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("generate nonce: %v", err)
	}
	block, err := aes.NewCipher(rawKey)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	sealed := gcm.Seal(nil, nonce, plaintext, nil)
	body = append(append([]byte{}, nonce...), sealed...)
	return body, base64.RawURLEncoding.EncodeToString(rawKey)
}

// appCdnDecrypt decrypts a downloaded body with a fragment key. A wrong key
// or a corrupted body must return an error.
func appCdnDecrypt(t *testing.T, body []byte, key string) ([]byte, error) {
	t.Helper()
	rawKey, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	if len(rawKey) != 32 {
		return nil, fmt.Errorf("key is %d bytes, want 32", len(rawKey))
	}
	if len(body) <= 12 {
		return nil, fmt.Errorf("body is %d bytes, too short to be valid", len(body))
	}
	block, err := aes.NewCipher(rawKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, body[:12], body[12:], nil)
}

func appCdnRandomKey(t *testing.T) string {
	t.Helper()
	rawKey := make([]byte, 32)
	if _, err := rand.Read(rawKey); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(rawKey)
}

// TestCdnAppContractEndToEnd drives the real HTTP handler through httptest
// exactly the way the app does: client-side AES-256-GCM encryption, bearer
// upload with an expiry override, anonymous ciphertext fetch, local
// decryption, wrapper page headers, wrong-key failure, 401/404 handling,
// revoke and per-user isolation. The shell matrix (scripts/validate/
// t20_cdn.sh) runs the same contract against a live base URL.
func TestCdnAppContractEndToEnd(t *testing.T) {
	service := testCdnService(t, func(c *CdnConfig) {
		c.DefaultExpiry = cdnDuration(time.Hour)
		c.MaxExpiry = cdnDuration(24 * time.Hour)
	})
	server := testCdnServer(t, service)

	binaryPayload := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{0x42}, 512)...)
	payloads := []struct {
		name      string
		plaintext []byte
	}{
		{"text", []byte(strings.Repeat("cdn app contract round trip line\n", 32))},
		{"binary", binaryPayload},
	}

	for _, payload := range payloads {
		t.Run(payload.name, func(t *testing.T) {
			body, key := appCdnEncrypt(t, payload.plaintext)

			resp := testCdnRequest(t, server.Client(), http.MethodPost, server.URL+cdnPathPrefix+"?expires=1h", "alice", body)
			raw := testCdnReadBody(t, resp)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("upload status = %d (body %q), want %d", resp.StatusCode, raw, http.StatusCreated)
			}
			var upload cdnUploadResponse
			if err := json.Unmarshal(raw, &upload); err != nil {
				t.Fatalf("decode upload response %q: %v", raw, err)
			}
			if !validCdnID(upload.ID) {
				t.Fatalf("upload id %q is not a valid cdn id", upload.ID)
			}
			if upload.Size != int64(len(body)) {
				t.Errorf("upload size = %d, want %d (nonce+ct+tag)", upload.Size, len(body))
			}
			expires := time.UnixMilli(upload.ExpiresAtMS)
			if expires.Before(time.Now().Add(30*time.Minute)) || expires.After(time.Now().Add(90*time.Minute)) {
				t.Errorf("expires_at_ms = %d (%s), want about one hour from now", upload.ExpiresAtMS, expires)
			}

			resp = testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+upload.ID+"/raw", "", nil)
			fetched := testCdnReadBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("raw fetch status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
				t.Errorf("raw Content-Type = %q, want application/octet-stream", got)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("raw X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "attachment") {
				t.Errorf("raw Content-Disposition = %q, want attachment", got)
			}
			if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("raw Access-Control-Allow-Origin = %q, want *", got)
			}
			if !bytes.Equal(fetched, body) {
				t.Errorf("raw fetch returned %d bytes, want the %d uploaded ciphertext bytes", len(fetched), len(body))
			}

			plaintext, err := appCdnDecrypt(t, fetched, key)
			if err != nil {
				t.Fatalf("decrypt with the fragment key failed: %v", err)
			}
			if !bytes.Equal(plaintext, payload.plaintext) {
				t.Errorf("decrypted %d bytes, want the %d plaintext bytes", len(plaintext), len(payload.plaintext))
			}
			if _, err := appCdnDecrypt(t, fetched, appCdnRandomKey(t)); err == nil {
				t.Error("decrypt with a wrong key succeeded, want failure")
			}

			// The fragment-less wrapper URL must serve the decryptor page
			// with the locked-down headers, embed the id (and only the id) as
			// data, and never expose the key or plaintext.
			resp = testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+upload.ID, "", nil)
			page := testCdnReadBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("wrapper status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if got := resp.Header.Get("Content-Security-Policy"); got != cdnWrapperCSP || !strings.Contains(got, "sandbox allow-scripts") {
				t.Errorf("wrapper CSP = %q, want the sandboxed service policy", got)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("wrapper X-Content-Type-Options = %q, want nosniff", got)
			}
			if !strings.Contains(string(page), `const fileID = "`+upload.ID+`";`) {
				t.Error("wrapper page does not embed the id as the fileID data literal")
			}
			if count := strings.Count(string(page), upload.ID); count != 1 {
				t.Errorf("wrapper page mentions the id %d times, want exactly the 1 data literal", count)
			}
			if bytes.Contains(page, []byte(key)) {
				t.Error("wrapper page leaked the decryption key")
			}
			if bytes.Contains(page, payload.plaintext) {
				t.Error("wrapper page leaked plaintext bytes")
			}
		})
	}

	// Upload one file for the auth/revoke/isolation tail shared by both
	// payload shapes.
	body, _ := appCdnEncrypt(t, []byte("shared tail payload"))
	status, id := testCdnUpload(t, server, "alice", body, "")
	if status != http.StatusCreated || id == "" {
		t.Fatalf("tail upload = (%d, %q), want a 201 with an id", status, id)
	}

	resp := testCdnRequest(t, server.Client(), http.MethodPost, server.URL+cdnPathPrefix, "", body)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated upload status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	missing := strings.Repeat("z", 43)
	for _, path := range []string{"/f/" + missing, "/f/" + missing + "/raw"} {
		resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+path, "", nil)
		_ = testCdnReadBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want %d", path, resp.StatusCode, http.StatusNotFound)
		}
	}

	// Invalid or past-looking expiries are rejected before anything is read.
	for _, bad := range []string{"0", "bogus", "-5m", "400d"} {
		resp := testCdnRequest(t, server.Client(), http.MethodPost, server.URL+cdnPathPrefix+"?expires="+bad, "alice", body)
		raw := testCdnReadBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expires=%s status = %d, want %d", bad, resp.StatusCode, http.StatusBadRequest)
		}
		var envelope map[string]string
		if err := json.Unmarshal(raw, &envelope); err != nil || envelope["error"] != "bad_request" {
			t.Errorf("expires=%s error envelope = %q", bad, raw)
		}
	}

	// A second user sees an unknown id: no list entry, no revoke, file kept.
	resp = testCdnRequest(t, server.Client(), http.MethodDelete, server.URL+cdnPathPrefix+"/"+id, "bob", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("foreign revoke status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	resp = testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+id+"/raw", "", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("file status after foreign revoke = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	resp = testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix, "bob", nil)
	listBody := testCdnReadBody(t, resp)
	var bobList cdnListResponse
	if err := json.Unmarshal(listBody, &bobList); err != nil {
		t.Fatalf("decode bob list %q: %v", listBody, err)
	}
	for _, entry := range bobList.Files {
		if entry.ID == id {
			t.Error("bob's list reveals alice's file id")
		}
	}

	// The owner revokes; the link dies immediately and the second revoke is
	// indistinguishable from an unknown id.
	resp = testCdnRequest(t, server.Client(), http.MethodDelete, server.URL+cdnPathPrefix+"/"+id, "alice", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("owner revoke status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	for _, path := range []string{"/f/" + id, "/f/" + id + "/raw"} {
		resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+path, "", nil)
		_ = testCdnReadBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s after revoke status = %d, want %d", path, resp.StatusCode, http.StatusNotFound)
		}
	}
	resp = testCdnRequest(t, server.Client(), http.MethodDelete, server.URL+cdnPathPrefix+"/"+id, "alice", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second revoke status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}
