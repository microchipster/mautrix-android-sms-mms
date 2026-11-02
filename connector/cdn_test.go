// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// testCdnConfig returns an active file service configuration rooted in a
// per-test temporary directory.
func testCdnConfig(t *testing.T, mutate func(*CdnConfig)) CdnConfig {
	t.Helper()
	cfg := DefaultCdnConfig()
	cfg.Enabled = true
	cfg.Domain = "cdn.example.com"
	cfg.DataDir = t.TempDir()
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

// testCdnAuth authenticates any non-empty bearer token as a user named after
// the token, so tests can act as several users.
func testCdnAuth(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if token == "" {
		return "", false
	}
	return token, true
}

func testCdnService(t *testing.T, mutate func(*CdnConfig)) *CdnService {
	t.Helper()
	service, err := NewCdnService(zerolog.Nop(), testCdnConfig(t, mutate), testCdnAuth)
	if err != nil {
		t.Fatalf("NewCdnService: %v", err)
	}
	if service == nil {
		t.Fatal("NewCdnService returned no service for an active configuration")
	}
	t.Cleanup(service.StopGC)
	return service
}

func testCdnServer(t *testing.T, service *CdnService) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	return server
}

func testCdnRequest(t *testing.T, client *http.Client, method, url, token string, body []byte) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func testCdnReadBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return body
}

// testCdnUpload posts ciphertext and returns the status plus the response id
// (empty unless the upload succeeded).
func testCdnUpload(t *testing.T, server *httptest.Server, token string, payload []byte, query string) (int, string) {
	t.Helper()
	url := server.URL + cdnPathPrefix + query
	resp := testCdnRequest(t, server.Client(), http.MethodPost, url, token, payload)
	body := testCdnReadBody(t, resp)
	id := ""
	if resp.StatusCode == http.StatusCreated {
		var parsed cdnUploadResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("decode upload response %q: %v", body, err)
		}
		id = parsed.ID
	}
	return resp.StatusCode, id
}

func TestCdnIDShape(t *testing.T) {
	pattern := regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	first, err := newCdnID()
	if err != nil {
		t.Fatalf("newCdnID: %v", err)
	}
	second, err := newCdnID()
	if err != nil {
		t.Fatalf("newCdnID: %v", err)
	}
	if !pattern.MatchString(first) {
		t.Fatalf("newCdnID() = %q, want 43 base64url characters", first)
	}
	if first == second {
		t.Fatalf("newCdnID returned the same id twice: %q", first)
	}
	if !validCdnID(first) {
		t.Fatalf("validCdnID(%q) = false, want true", first)
	}
	for _, invalid := range []string{"", "short", strings.Repeat("a", 65), "has/slash", "has.dot", "has+plus"} {
		if validCdnID(invalid) {
			t.Errorf("validCdnID(%q) = true, want false", invalid)
		}
	}
}

func TestCdnUploadDownloadRoundTrip(t *testing.T) {
	service := testCdnService(t, nil)
	server := testCdnServer(t, service)

	payload := make([]byte, 1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	status, id := testCdnUpload(t, server, "alice", payload, "")
	if status != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d", status, http.StatusCreated)
	}
	if !validCdnID(id) {
		t.Fatalf("upload returned invalid id %q", id)
	}

	resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+id+"/raw", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("Cache-Control"); got == "" {
		t.Error("Cache-Control is empty, want a cache policy")
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment disposition", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want * for the sandboxed wrapper", got)
	}
	body := testCdnReadBody(t, resp)
	if !bytes.Equal(body, payload) {
		t.Errorf("downloaded %d bytes, want the %d uploaded bytes", len(body), len(payload))
	}

	entries, err := service.List("alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != id || entries[0].Size != int64(len(payload)) {
		t.Errorf("List = %+v, want one entry for %s of %d bytes", entries, id, len(payload))
	}

	rawURL := service.FileURL(id)
	if !strings.HasPrefix(rawURL, "https://cdn.example.com"+cdnPathPrefix+"/"+id) {
		t.Errorf("FileURL(%q) = %q", id, rawURL)
	}
}

func TestCdnUploadSizeCap(t *testing.T) {
	service := testCdnService(t, func(c *CdnConfig) {
		c.MaxFileBytes = 64
		c.PerUserCapBytes = 1 << 20
	})
	server := testCdnServer(t, service)

	status, _ := testCdnUpload(t, server, "alice", make([]byte, 65), "")
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload status = %d, want %d", status, http.StatusRequestEntityTooLarge)
	}
	resp := testCdnRequest(t, server.Client(), http.MethodPost, server.URL+cdnPathPrefix, "alice", make([]byte, 65))
	body := testCdnReadBody(t, resp)
	if !strings.Contains(string(body), "file_too_large") {
		t.Errorf("oversized upload body = %q, want the file_too_large error", body)
	}

	status, _ = testCdnUpload(t, server, "alice", make([]byte, 64), "")
	if status != http.StatusCreated {
		t.Fatalf("at-limit upload status = %d, want %d", status, http.StatusCreated)
	}
	status, _ = testCdnUpload(t, server, "alice", nil, "")
	if status != http.StatusBadRequest {
		t.Fatalf("empty upload status = %d, want %d", status, http.StatusBadRequest)
	}

	entries, err := service.List("alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("List returned %d entries after the capped uploads, want 1", len(entries))
	}
}

func TestCdnPerUserQuotaFIFO(t *testing.T) {
	service := testCdnService(t, func(c *CdnConfig) {
		c.MaxFileBytes = 64
		c.PerUserCapBytes = 100
	})
	server := testCdnServer(t, service)

	ids := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		status, id := testCdnUpload(t, server, "alice", bytes.Repeat([]byte{byte('a' + index)}, 40), "")
		if status != http.StatusCreated {
			t.Fatalf("upload %d status = %d, want %d", index, status, http.StatusCreated)
		}
		ids = append(ids, id)
	}
	if len(ids) != 3 || ids[0] == ids[1] || ids[1] == ids[2] {
		t.Fatalf("unexpected upload ids: %v", ids)
	}

	// 120 bytes uploaded against a 100 byte cap: the oldest file must be gone.
	resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+ids[0]+"/raw", "", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("oldest file status = %d, want %d after FIFO pruning", resp.StatusCode, http.StatusNotFound)
	}
	for _, id := range ids[1:] {
		resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+id+"/raw", "", nil)
		_ = testCdnReadBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("file %s status = %d, want %d", id, resp.StatusCode, http.StatusOK)
		}
	}

	entries, err := service.List("alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 || entries[0].ID != ids[2] || entries[1].ID != ids[1] {
		t.Errorf("List = %+v, want %s then %s (newest first)", entries, ids[2], ids[1])
	}
	stats := service.Stats()
	if stats.Files != 2 || stats.Bytes != 80 {
		t.Errorf("Stats = %+v, want 2 files / 80 bytes", stats)
	}

	// Another user's quota is independent.
	status, _ := testCdnUpload(t, server, "bob", bytes.Repeat([]byte{'z'}, 40), "")
	if status != http.StatusCreated {
		t.Fatalf("bob upload status = %d, want %d", status, http.StatusCreated)
	}
	entries, err = service.List("alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("alice's files changed with bob's upload: %+v", entries)
	}
}

func TestCdnExpiryRejectionAndGC(t *testing.T) {
	service := testCdnService(t, nil)
	server := testCdnServer(t, service)

	status, _ := testCdnUpload(t, server, "alice", []byte("ciphertext"), "?expires=400d")
	if status != http.StatusBadRequest {
		t.Fatalf("over-a-year expiry status = %d, want %d", status, http.StatusBadRequest)
	}
	status, _ = testCdnUpload(t, server, "alice", []byte("ciphertext"), "?expires=0")
	if status != http.StatusBadRequest {
		t.Fatalf("zero expiry status = %d, want %d", status, http.StatusBadRequest)
	}
	status, longID := testCdnUpload(t, server, "alice", []byte("long-lived"), "?expires=1h")
	if status != http.StatusCreated {
		t.Fatalf("one-hour expiry status = %d, want %d", status, http.StatusCreated)
	}
	status, shortID := testCdnUpload(t, server, "alice", []byte("short-lived"), "?expires=50ms")
	if status != http.StatusCreated {
		t.Fatalf("short expiry status = %d, want %d", status, http.StatusCreated)
	}

	time.Sleep(120 * time.Millisecond)
	resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+shortID+"/raw", "", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expired file status = %d, want %d before cleanup", resp.StatusCode, http.StatusNotFound)
	}

	removed, err := service.RunGC(time.Now())
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if removed != 1 {
		t.Errorf("RunGC removed %d files, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(service.cfg.DataDir, cdnObjectDirName, shortID)); !os.IsNotExist(err) {
		t.Errorf("expired file %s still on disk after cleanup (err = %v)", shortID, err)
	}
	if _, err := os.Stat(filepath.Join(service.cfg.DataDir, cdnObjectDirName, shortID+cdnMetaSuffix)); !os.IsNotExist(err) {
		t.Errorf("expired metadata %s still on disk after cleanup (err = %v)", shortID, err)
	}
	resp = testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+longID+"/raw", "", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("one-hour file status = %d after cleanup, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestCdnWrapperPageEscapesID(t *testing.T) {
	service := testCdnService(t, nil)
	server := testCdnServer(t, service)

	status, id := testCdnUpload(t, server, "alice", []byte("ciphertext"), "")
	if status != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d", status, http.StatusCreated)
	}
	resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+id, "", nil)
	body := string(testCdnReadBody(t, resp))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wrapper status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("wrapper Content-Type = %q, want text/html", got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != cdnWrapperCSP {
		t.Errorf("wrapper CSP = %q, want %q", got, cdnWrapperCSP)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox allow-scripts") {
		t.Error("wrapper CSP must sandbox the page")
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("wrapper X-Content-Type-Options = %q, want nosniff", got)
	}
	if !strings.Contains(body, `"`+id+`"`) {
		t.Errorf("wrapper page does not echo the id as a JS string literal")
	}
	if !strings.Contains(body, "location.hash") {
		t.Error("wrapper page must read the decryption key from the URL fragment")
	}
	if strings.Contains(body, "<svg") || strings.Contains(body, "innerHTML") {
		t.Error("wrapper page must not render markup or use innerHTML")
	}

	// A hostile id must never be reflected into the page.
	for _, path := range []string{"/f/aaa%3Cscript%3Ebbb", "/f/..%2F..%2Fetc%2Fpasswd", "/f/notanid!"} {
		resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+path, "", nil)
		body := string(testCdnReadBody(t, resp))
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want %d", path, resp.StatusCode, http.StatusNotFound)
		}
		if strings.Contains(body, "<script") || strings.Contains(body, "passwd") {
			t.Errorf("%s reflected the hostile id: %q", path, body)
		}
	}
}

func TestCdnNotFoundAndAuth(t *testing.T) {
	service := testCdnService(t, nil)
	server := testCdnServer(t, service)

	missing := strings.Repeat("a", 43)
	for _, path := range []string{"/f/" + missing, "/f/" + missing + "/raw"} {
		resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+path, "", nil)
		_ = testCdnReadBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want %d", path, resp.StatusCode, http.StatusNotFound)
		}
	}

	resp := testCdnRequest(t, server.Client(), http.MethodDelete, server.URL+cdnPathPrefix+"/"+missing, "alice", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE unknown id status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	for _, request := range []struct {
		method string
		url    string
	}{
		{http.MethodPost, server.URL + cdnPathPrefix},
		{http.MethodGet, server.URL + cdnPathPrefix},
		{http.MethodDelete, server.URL + cdnPathPrefix + "/" + missing},
	} {
		resp := testCdnRequest(t, server.Client(), request.method, request.url, "", nil)
		_ = testCdnReadBody(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: status = %d, want %d", request.method, request.url, resp.StatusCode, http.StatusUnauthorized)
		}
	}

	resp = testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix, "alice", nil)
	body := testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /f status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var listed cdnListResponse
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode list response %q: %v", body, err)
	}
	if len(listed.Files) != 0 || listed.CapBytes != service.cfg.PerUserCapBytes {
		t.Errorf("empty list response = %+v", listed)
	}
}

func TestCdnRevokeOwnFilesOnly(t *testing.T) {
	service := testCdnService(t, nil)
	server := testCdnServer(t, service)

	status, id := testCdnUpload(t, server, "alice", []byte("ciphertext"), "")
	if status != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d", status, http.StatusCreated)
	}

	resp := testCdnRequest(t, server.Client(), http.MethodDelete, server.URL+cdnPathPrefix+"/"+id, "bob", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob revoke status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	resp = testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+id+"/raw", "", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("alice's file status after bob's attempt = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	resp = testCdnRequest(t, server.Client(), http.MethodDelete, server.URL+cdnPathPrefix+"/"+id, "alice", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("alice revoke status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	for _, path := range []string{"/f/" + id, "/f/" + id + "/raw"} {
		resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+path, "", nil)
		_ = testCdnReadBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s after revoke: status = %d, want %d", path, resp.StatusCode, http.StatusNotFound)
		}
	}
	resp = testCdnRequest(t, server.Client(), http.MethodDelete, server.URL+cdnPathPrefix+"/"+id, "alice", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second revoke status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	ok, err := service.Revoke("alice", id)
	if err != nil || ok {
		t.Errorf("Revoke after deletion = (%t, %v), want (false, nil)", ok, err)
	}
}

func TestCdnConfigValidation(t *testing.T) {
	base := DefaultCdnConfig()
	if base.Active() {
		t.Fatal("default configuration must be inactive")
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("disabled configuration must validate, got %v", err)
	}
	if service, err := NewCdnService(zerolog.Nop(), base, nil); err != nil || service != nil {
		t.Fatalf("disabled configuration created a service: (%v, %v)", service, err)
	}
	if base.MaxFileBytes != 512<<20 || base.PerUserCapBytes != 10<<30 {
		t.Fatalf("default size caps = %d bytes per file / %d bytes per user, want 512 MiB / 10 GiB",
			base.MaxFileBytes, base.PerUserCapBytes)
	}
	if base.MaxAlbumItems != 20 || base.MaxAlbumBytes != 500<<20 {
		t.Fatalf("default album caps = %d item(s) / %d bytes, want 20 / 500 MiB",
			base.MaxAlbumItems, base.MaxAlbumBytes)
	}
	if base.DefaultExpiry.Duration() != 30*24*time.Hour || base.MaxExpiry.Duration() != 365*24*time.Hour {
		t.Fatalf("default expiries = %s / %s, want 30d / 365d",
			formatTTLDuration(base.DefaultExpiry.Duration()), formatTTLDuration(base.MaxExpiry.Duration()))
	}

	noDomain := DefaultCdnConfig()
	noDomain.Enabled = true
	if err := noDomain.Validate(); err == nil {
		t.Fatal("enabled configuration without a domain must not validate")
	}
	if _, err := NewCdnService(zerolog.Nop(), noDomain, nil); err == nil {
		t.Fatal("NewCdnService accepted an enabled configuration without a domain")
	}

	if err := testCdnConfig(t, nil).Validate(); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	tooLong := testCdnConfig(t, func(c *CdnConfig) {
		c.MaxExpiry = cdnDuration(400 * 24 * time.Hour)
	})
	if err := tooLong.Validate(); err == nil {
		t.Fatal("max_expiry above one year must not validate")
	}
	defaultAboveMax := testCdnConfig(t, func(c *CdnConfig) {
		c.DefaultExpiry = cdnDuration(c.MaxExpiry.Duration() + time.Hour)
	})
	if err := defaultAboveMax.Validate(); err == nil {
		t.Fatal("default_expiry above max_expiry must not validate")
	}
	badDomain := testCdnConfig(t, func(c *CdnConfig) {
		c.Domain = "https://cdn.example.com/path"
	})
	if err := badDomain.Validate(); err == nil {
		t.Fatal("a domain with a scheme or path must not validate")
	}
	noDataDir := testCdnConfig(t, func(c *CdnConfig) {
		c.DataDir = " "
	})
	if err := noDataDir.Validate(); err == nil {
		t.Fatal("an empty data_dir must not validate")
	}
}
