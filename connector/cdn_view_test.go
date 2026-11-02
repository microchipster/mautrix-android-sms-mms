// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

const (
	testViewFingerprintA = "viewer-fingerprint-alpha"
	testViewFingerprintB = "viewer-fingerprint-beta"
)

func testViewHash(fingerprint string) string {
	sum := sha256.Sum256([]byte(fingerprint))
	return hex.EncodeToString(sum[:])
}

func testCdnFetchWithFingerprint(t *testing.T, server *httptest.Server, path, fingerprint string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if fingerprint != "" {
		req.Header.Set(cdnViewerHeader, fingerprint)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("fetch %s: %v", path, err)
	}
	return resp
}

func buildTestAlbumContainer(t *testing.T, items [][]byte, manifest []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("CDNALB01")
	buf.WriteByte(byte(len(items)))
	for _, item := range items {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(item)))
		buf.Write(length[:])
	}
	var manifestLength [8]byte
	binary.BigEndian.PutUint64(manifestLength[:], uint64(len(manifest)))
	buf.Write(manifestLength[:])
	for _, item := range items {
		buf.Write(item)
	}
	buf.Write(manifest)
	return buf.Bytes()
}

func TestCdnFirstView(t *testing.T) {
	if DefaultCdnConfig().EffectiveDefaultView() != cdnViewFirst {
		t.Fatalf("default view = %q, want %q", DefaultCdnConfig().EffectiveDefaultView(), cdnViewFirst)
	}
	service := testCdnService(t, func(c *CdnConfig) { c.DefaultView = cdnViewFirst })
	server := testCdnServer(t, service)

	body, key := appCdnEncrypt(t, []byte("first-view secret"))
	status, id := testCdnUpload(t, server, "device-a", body, "")
	if status != http.StatusCreated || id == "" {
		t.Fatalf("upload = %d, %q", status, id)
	}
	rawPath := cdnPathPrefix + "/" + id + "/raw"

	// A first-view link refuses anonymous fetches without a fingerprint.
	resp := testCdnFetchWithFingerprint(t, server, rawPath, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-fingerprint fetch = %d, want 403", resp.StatusCode)
	}
	_ = testCdnReadBody(t, resp)

	// The first fingerprint claims the link and decrypts it.
	resp = testCdnFetchWithFingerprint(t, server, rawPath, testViewFingerprintA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first fingerprint fetch = %d, want 200", resp.StatusCode)
	}
	plaintext, err := appCdnDecrypt(t, testCdnReadBody(t, resp), key)
	if err != nil || string(plaintext) != "first-view secret" {
		t.Fatalf("decrypt = %q, %v", plaintext, err)
	}

	// Later anonymous fingerprints are refused.
	resp = testCdnFetchWithFingerprint(t, server, rawPath, testViewFingerprintB)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("second fingerprint fetch = %d, want 403", resp.StatusCode)
	}
	_ = testCdnReadBody(t, resp)

	// The claiming fingerprint still works.
	resp = testCdnFetchWithFingerprint(t, server, rawPath, testViewFingerprintA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claiming fingerprint refetch = %d, want 200", resp.StatusCode)
	}
	_ = testCdnReadBody(t, resp)

	// Authenticated (paired-device) fetches are exempt from the claim.
	resp = testCdnRequest(t, server.Client(), http.MethodGet, server.URL+rawPath, "device-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated fetch = %d, want 200", resp.StatusCode)
	}
	_ = testCdnReadBody(t, resp)

	// A paired device of a different user is not exempt.
	resp = testCdnRequest(t, server.Client(), http.MethodGet, server.URL+rawPath, "device-b", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign authenticated fetch = %d, want 403", resp.StatusCode)
	}
	_ = testCdnReadBody(t, resp)

	// The claim is persisted in the entry metadata.
	meta, err := os.ReadFile(service.metaPath(id))
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var stored CdnEntry
	if err := json.Unmarshal(meta, &stored); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if stored.View != cdnViewFirst || stored.ViewClaimSHA256 != testViewHash(testViewFingerprintA) || stored.ViewClaimedAtMS == 0 {
		t.Fatalf("stored claim = %+v", stored)
	}
}

func TestCdnFirstViewModel(t *testing.T) {
	service := testCdnService(t, func(c *CdnConfig) { c.DefaultView = cdnViewFirst })
	server := testCdnServer(t, service)

	// view=public is readable by anyone with the link.
	body, key := appCdnEncrypt(t, []byte("public secret"))
	status, id := testCdnUpload(t, server, "device-a", body, "?view=public")
	if status != http.StatusCreated || id == "" {
		t.Fatalf("upload = %d, %q", status, id)
	}
	rawPath := cdnPathPrefix + "/" + id + "/raw"
	resp := testCdnFetchWithFingerprint(t, server, rawPath, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public anonymous fetch = %d, want 200", resp.StatusCode)
	}
	if plaintext, err := appCdnDecrypt(t, testCdnReadBody(t, resp), key); err != nil || string(plaintext) != "public secret" {
		t.Fatalf("decrypt = %q, %v", plaintext, err)
	}
	resp = testCdnFetchWithFingerprint(t, server, rawPath, testViewFingerprintB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public second fetch = %d, want 200", resp.StatusCode)
	}
	_ = testCdnReadBody(t, resp)

	// default_view: public makes uploads public without the query parameter.
	publicService := testCdnService(t, func(c *CdnConfig) { c.DefaultView = cdnViewPublic })
	publicServer := testCdnServer(t, publicService)
	body, _ = appCdnEncrypt(t, []byte("configured public"))
	status, id = testCdnUpload(t, publicServer, "device-a", body, "")
	if status != http.StatusCreated || id == "" {
		t.Fatalf("configured public upload = %d, %q", status, id)
	}
	resp = testCdnFetchWithFingerprint(t, publicServer, cdnPathPrefix+"/"+id+"/raw", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("configured public fetch = %d, want 200", resp.StatusCode)
	}
	_ = testCdnReadBody(t, resp)

	// The upload response reports the applied view.
	body, _ = appCdnEncrypt(t, []byte("view field"))
	resp = testCdnRequest(t, server.Client(), http.MethodPost, server.URL+cdnPathPrefix, "device-a", body)
	raw := testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", resp.StatusCode, raw)
	}
	var parsed cdnUploadResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if parsed.View != cdnViewFirst {
		t.Fatalf("response view = %q, want %q", parsed.View, cdnViewFirst)
	}

	// An invalid view is rejected.
	body, _ = appCdnEncrypt(t, []byte("bad view"))
	if status, _ := testCdnUpload(t, server, "device-a", body, "?view=sometimes"); status != http.StatusBadRequest {
		t.Fatalf("invalid view upload = %d, want 400", status)
	}
}

func TestCdnFirstViewAlbum(t *testing.T) {
	service := testCdnService(t, func(c *CdnConfig) { c.DefaultView = cdnViewFirst })
	server := testCdnServer(t, service)

	itemA, _ := appCdnEncrypt(t, []byte("album item a"))
	itemB, _ := appCdnEncrypt(t, []byte("album item b"))
	manifest, manifestKey := appCdnEncrypt(t, []byte("album manifest"))
	container := buildTestAlbumContainer(t, [][]byte{itemA, itemB}, manifest)

	resp := testCdnRequest(t, server.Client(), http.MethodPost, server.URL+cdnPathPrefix+"/albums", "device-a", container)
	raw := testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("album upload = %d: %s", resp.StatusCode, raw)
	}
	var uploaded cdnAlbumUploadResponse
	if err := json.Unmarshal(raw, &uploaded); err != nil {
		t.Fatalf("decode album response: %v", err)
	}
	if uploaded.View != cdnViewFirst {
		t.Fatalf("album view = %q, want %q", uploaded.View, cdnViewFirst)
	}
	manifestPath := cdnPathPrefix + "/" + uploaded.ID + "/manifest"

	// The manifest is claim-gated the same way as a single file.
	resp = testCdnFetchWithFingerprint(t, server, manifestPath, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("manifest without fingerprint = %d, want 403", resp.StatusCode)
	}
	_ = testCdnReadBody(t, resp)
	resp = testCdnFetchWithFingerprint(t, server, manifestPath, testViewFingerprintA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest first fingerprint = %d, want 200", resp.StatusCode)
	}
	if plaintext, err := appCdnDecrypt(t, testCdnReadBody(t, resp), manifestKey); err != nil || string(plaintext) != "album manifest" {
		t.Fatalf("manifest decrypt = %q, %v", plaintext, err)
	}
	resp = testCdnFetchWithFingerprint(t, server, manifestPath, testViewFingerprintB)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("manifest second fingerprint = %d, want 403", resp.StatusCode)
	}
	_ = testCdnReadBody(t, resp)

	// Items share the same claim as the manifest.
	itemPath := cdnPathPrefix + "/" + uploaded.ID + "/items/0"
	resp = testCdnFetchWithFingerprint(t, server, itemPath, testViewFingerprintA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("item claiming fingerprint = %d, want 200", resp.StatusCode)
	}
	_ = testCdnReadBody(t, resp)
	resp = testCdnFetchWithFingerprint(t, server, itemPath, testViewFingerprintB)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("item second fingerprint = %d, want 403", resp.StatusCode)
	}
	_ = testCdnReadBody(t, resp)
}
