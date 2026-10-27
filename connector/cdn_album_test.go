// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// testCdnAlbumSalt is the HKDF salt of the album key spec
// (docs/cdn-file-service.md). The wrapper page must embed the same literal.
const testCdnAlbumSalt = "cdn-album-item"

// appCdnAlbumSubkey derives an album subkey exactly as the app and the
// gallery wrapper do: HKDF-SHA256(fragmentKey, salt "cdn-album-item", info),
// 32 bytes.
func appCdnAlbumSubkey(t *testing.T, fragmentKey []byte, info string) []byte {
	t.Helper()
	subkey, err := hkdf.Key(sha256.New, fragmentKey, []byte(testCdnAlbumSalt), info, 32)
	if err != nil {
		t.Fatalf("derive album subkey %q: %v", info, err)
	}
	return subkey
}

// testHKDFSHA256 is an independent RFC 5869 implementation used to
// cross-check the stdlib derivation the spec relies on.
func testHKDFSHA256(secret, salt []byte, info string, length int) []byte {
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size)
	}
	extract := hmac.New(sha256.New, salt)
	extract.Write(secret)
	prk := extract.Sum(nil)
	expand := hmac.New(sha256.New, prk)
	var previous, out []byte
	counter := byte(1)
	for len(out) < length {
		expand.Reset()
		expand.Write(previous)
		expand.Write([]byte(info))
		expand.Write([]byte{counter})
		previous = expand.Sum(nil)
		out = append(out, previous...)
		counter++
	}
	return out[:length]
}

// appCdnAlbumSeal encrypts one album part exactly like the app: AES-256-GCM,
// a random 12-byte nonce, no AAD, body = nonce || ciphertext || tag.
func appCdnAlbumSeal(t *testing.T, subkey, plaintext []byte) []byte {
	t.Helper()
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("generate nonce: %v", err)
	}
	block, err := aes.NewCipher(subkey)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	return append(append([]byte{}, nonce...), gcm.Seal(nil, nonce, plaintext, nil)...)
}

// appCdnAlbumOpen decrypts one album part with its subkey.
func appCdnAlbumOpen(subkey, body []byte) ([]byte, error) {
	if len(body) <= 12 {
		return nil, fmt.Errorf("body is %d bytes, too short", len(body))
	}
	block, err := aes.NewCipher(subkey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, body[:12], body[12:], nil)
}

// testCdnAlbumItem is one plaintext item of a test album.
type testCdnAlbumItem struct {
	name string
	typ  string
	data []byte
}

// testCdnAlbum is a built album container plus the data the tests decrypt
// back from the service.
type testCdnAlbum struct {
	container   []byte
	fragmentKey []byte
	items       []testCdnAlbumItem
	manifest    []byte
	payloadSize int64
}

// appCdnAlbumManifestItem / appCdnAlbumManifest mirror the manifest JSON of
// the album spec.
type appCdnAlbumManifestItem struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type appCdnAlbumManifest struct {
	Version int                       `json:"version"`
	Items   []appCdnAlbumManifestItem `json:"items"`
}

// testCdnAlbumKey returns a random 32-byte fragment key.
func testCdnAlbumKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate fragment key: %v", err)
	}
	return key
}

// buildTestCdnAlbum builds a complete album upload container exactly the way
// the app does: per-item AES-256-GCM under HKDF-SHA256 subkeys with info
// "item:<index>", then the manifest under info "manifest", framed as
// CDNALB01 + count + item lengths + manifest length + payloads.
func buildTestCdnAlbum(t *testing.T, fragmentKey []byte, items []testCdnAlbumItem) *testCdnAlbum {
	t.Helper()
	manifestItems := make([]appCdnAlbumManifestItem, len(items))
	itemBodies := make([][]byte, len(items))
	for index, item := range items {
		subkey := appCdnAlbumSubkey(t, fragmentKey, fmt.Sprintf("item:%d", index))
		itemBodies[index] = appCdnAlbumSeal(t, subkey, item.data)
		manifestItems[index] = appCdnAlbumManifestItem{Name: item.name, Type: item.typ, Size: int64(len(item.data))}
	}
	manifest, err := json.Marshal(appCdnAlbumManifest{Version: 1, Items: manifestItems})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestBody := appCdnAlbumSeal(t, appCdnAlbumSubkey(t, fragmentKey, "manifest"), manifest)

	buf := bytes.NewBuffer(nil)
	buf.WriteString(cdnAlbumMagic)
	buf.WriteByte(byte(len(items)))
	for _, body := range itemBodies {
		_ = binary.Write(buf, binary.BigEndian, uint64(len(body)))
	}
	_ = binary.Write(buf, binary.BigEndian, uint64(len(manifestBody)))
	payloadSize := int64(len(manifestBody))
	for _, body := range itemBodies {
		buf.Write(body)
		payloadSize += int64(len(body))
	}
	buf.Write(manifestBody)
	return &testCdnAlbum{container: buf.Bytes(), fragmentKey: fragmentKey, items: items, manifest: manifest, payloadSize: payloadSize}
}

// testCdnAlbumHeader builds an album container header (magic, count, declared
// lengths) without a payload.
func testCdnAlbumHeader(count int, lengths []int64, manifestLength int64) []byte {
	buf := bytes.NewBuffer(nil)
	buf.WriteString(cdnAlbumMagic)
	buf.WriteByte(byte(count))
	for _, length := range lengths {
		_ = binary.Write(buf, binary.BigEndian, uint64(length))
	}
	_ = binary.Write(buf, binary.BigEndian, uint64(manifestLength))
	return buf.Bytes()
}

// testCdnAlbumUpload posts an album container and returns the status plus the
// decoded response (zero on failure).
func testCdnAlbumUpload(t *testing.T, server *httptest.Server, token string, container []byte, query string) (int, cdnAlbumUploadResponse) {
	t.Helper()
	resp := testCdnRequest(t, server.Client(), http.MethodPost, server.URL+cdnPathPrefix+"/albums"+query, token, container)
	body := testCdnReadBody(t, resp)
	var parsed cdnAlbumUploadResponse
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("decode album upload response %q: %v", body, err)
		}
	}
	return resp.StatusCode, parsed
}

// testCdnAlbumFetch fetches one public album route.
func testCdnAlbumFetch(t *testing.T, server *httptest.Server, path string) (int, []byte, http.Header) {
	t.Helper()
	resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+path, "", nil)
	return resp.StatusCode, testCdnReadBody(t, resp), resp.Header
}

func TestCdnAlbumDefaultsAndConfig(t *testing.T) {
	cfg := DefaultCdnConfig()
	if cfg.MaxAlbumItems != 20 {
		t.Errorf("default max_album_items = %d, want 20", cfg.MaxAlbumItems)
	}
	if cfg.MaxAlbumBytes != int64(500)<<20 {
		t.Errorf("default max_album_bytes = %d, want %d", cfg.MaxAlbumBytes, int64(500)<<20)
	}
	if cdnDefaultMaxAlbumItems != 20 || cdnDefaultMaxAlbumBytes != int64(500)<<20 {
		t.Errorf("album default constants drifted: %d items, %d bytes", cdnDefaultMaxAlbumItems, cdnDefaultMaxAlbumBytes)
	}
	if cdnHardMaxAlbumItems != 255 {
		t.Errorf("album hard item limit = %d, want 255 (single count byte)", cdnHardMaxAlbumItems)
	}

	bad := testCdnConfig(t, func(c *CdnConfig) { c.MaxAlbumItems = 0 })
	if err := bad.Validate(); err == nil {
		t.Error("zero max_album_items must not validate")
	}
	bad = testCdnConfig(t, func(c *CdnConfig) { c.MaxAlbumItems = cdnHardMaxAlbumItems + 1 })
	if err := bad.Validate(); err == nil {
		t.Error("max_album_items above 255 must not validate")
	}
	bad = testCdnConfig(t, func(c *CdnConfig) { c.MaxAlbumBytes = 0 })
	if err := bad.Validate(); err == nil {
		t.Error("zero max_album_bytes must not validate")
	}
	bad = testCdnConfig(t, func(c *CdnConfig) { c.MaxAlbumBytes = -1 })
	if err := bad.Validate(); err == nil {
		t.Error("negative max_album_bytes must not validate")
	}
	if err := testCdnConfig(t, nil).Validate(); err != nil {
		t.Fatalf("default album caps must validate: %v", err)
	}

	// A partial YAML mapping must not wipe the album or unrelated defaults.
	var parsed CdnConfig
	if err := yaml.Unmarshal([]byte("max_album_items: 7\nmax_album_bytes: 1048576\n"), &parsed); err != nil {
		t.Fatalf("unmarshal cdn config: %v", err)
	}
	if parsed.MaxAlbumItems != 7 || parsed.MaxAlbumBytes != 1048576 {
		t.Errorf("parsed album caps = (%d, %d), want (7, 1048576)", parsed.MaxAlbumItems, parsed.MaxAlbumBytes)
	}
	if parsed.MaxFileBytes != cdnDefaultMaxFileBytes || parsed.PerUserCapBytes != cdnDefaultPerUserCapBytes {
		t.Errorf("partial cdn mapping wiped unrelated defaults: %+v", parsed)
	}
}

func TestCdnAlbumSubkeyDerivation(t *testing.T) {
	// RFC 5869 appendix A.1 (test case 1, SHA-256) pins the independent
	// reference implementation, which then cross-checks the stdlib call the
	// spec relies on.
	ikm := bytes.Repeat([]byte{0x0b}, 22)
	salt := make([]byte, 13)
	for index := range salt {
		salt[index] = byte(index)
	}
	info := make([]byte, 10)
	for index := range info {
		info[index] = byte(0xf0 + index)
	}
	want, err := hex.DecodeString("3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865")
	if err != nil {
		t.Fatalf("decode RFC 5869 vector: %v", err)
	}
	if got := testHKDFSHA256(ikm, salt, string(info), 42); !bytes.Equal(got, want) {
		t.Fatalf("reference HKDF-SHA256 does not match RFC 5869: %x", got)
	}

	fragmentKey := testCdnAlbumKey(t)
	item0 := appCdnAlbumSubkey(t, fragmentKey, "item:0")
	item1 := appCdnAlbumSubkey(t, fragmentKey, "item:1")
	manifestKey := appCdnAlbumSubkey(t, fragmentKey, "manifest")
	if len(item0) != 32 {
		t.Fatalf("subkey is %d bytes, want 32", len(item0))
	}
	if bytes.Equal(item0, item1) || bytes.Equal(item0, manifestKey) || bytes.Equal(item1, manifestKey) {
		t.Error("distinct HKDF infos must derive distinct subkeys")
	}
	if reference := testHKDFSHA256(fragmentKey, []byte(testCdnAlbumSalt), "item:0", 32); !bytes.Equal(item0, reference) {
		t.Errorf("stdlib subkey %x != reference %x", item0, reference)
	}
	if again := appCdnAlbumSubkey(t, fragmentKey, "item:0"); !bytes.Equal(item0, again) {
		t.Error("subkey derivation must be deterministic (app and wrapper agree)")
	}
	if bytes.Equal(item0, appCdnAlbumSubkey(t, testCdnAlbumKey(t), "item:0")) {
		t.Error("different fragment keys must derive different subkeys")
	}

	body := appCdnAlbumSeal(t, item0, []byte("hello item"))
	if plaintext, err := appCdnAlbumOpen(item0, body); err != nil || string(plaintext) != "hello item" {
		t.Fatalf("own subkey failed to open the part: (%q, %v)", plaintext, err)
	}
	if _, err := appCdnAlbumOpen(item1, body); err == nil {
		t.Error("item:1 subkey opened the item:0 part")
	}
	if _, err := appCdnAlbumOpen(manifestKey, body); err == nil {
		t.Error("manifest subkey opened an item part")
	}
	if _, err := appCdnAlbumOpen(fragmentKey, body); err == nil {
		t.Error("the raw fragment key opened a sealed part (subkeys must go through HKDF)")
	}
}

func TestCdnAlbumRoundTrip(t *testing.T) {
	service := testCdnService(t, nil)
	server := testCdnServer(t, service)

	fragmentKey := testCdnAlbumKey(t)
	items := []testCdnAlbumItem{
		{name: "photo.png", typ: "image/png", data: append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{0x42}, 256)...)},
		{name: "voice.flac", typ: "audio/flac", data: append([]byte("fLaC"), bytes.Repeat([]byte{0x11}, 128)...)},
		{name: "notes.txt", typ: "text/plain", data: []byte("album attachment plaintext")},
	}
	album := buildTestCdnAlbum(t, fragmentKey, items)

	status, uploaded := testCdnAlbumUpload(t, server, "alice", album.container, "")
	if status != http.StatusCreated {
		t.Fatalf("album upload status = %d, want %d", status, http.StatusCreated)
	}
	if !validCdnID(uploaded.ID) {
		t.Fatalf("album upload returned invalid id %q", uploaded.ID)
	}
	if uploaded.Items != len(items) {
		t.Errorf("album upload reported %d items, want %d", uploaded.Items, len(items))
	}
	if uploaded.Size != album.payloadSize {
		t.Errorf("album upload reported %d bytes, want %d stored ciphertext bytes", uploaded.Size, album.payloadSize)
	}
	if uploaded.ExpiresAtMS <= 0 {
		t.Errorf("album upload expires_at_ms = %d, want a positive timestamp", uploaded.ExpiresAtMS)
	}

	// Storage layout: directory, manifest, indexed items and an album sidecar.
	objectDir := filepath.Join(service.cfg.DataDir, cdnObjectDirName)
	for _, path := range []string{
		filepath.Join(objectDir, uploaded.ID),
		filepath.Join(objectDir, uploaded.ID, cdnAlbumManifestName),
		filepath.Join(objectDir, uploaded.ID, cdnAlbumItemsDirName, "0"),
		filepath.Join(objectDir, uploaded.ID, cdnAlbumItemsDirName, "2"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected album part %s on disk: %v", path, err)
		}
	}
	meta, err := service.readMeta(uploaded.ID)
	if err != nil || meta == nil {
		t.Fatalf("read album meta: (%v, %v)", meta, err)
	}
	if !meta.isAlbum() || meta.ItemCount != len(items) || meta.Size != album.payloadSize {
		t.Errorf("album meta = %+v, want kind=album, %d items, %d bytes", meta, len(items), album.payloadSize)
	}

	// Gallery wrapper: locked-down headers, the id once as a data literal,
	// the HKDF salt, and no markup sinks or external assets.
	status, page, headers := testCdnAlbumFetch(t, server, cdnPathPrefix+"/"+uploaded.ID)
	if status != http.StatusOK {
		t.Fatalf("album wrapper status = %d, want %d", status, http.StatusOK)
	}
	pageText := string(page)
	if got := headers.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("wrapper Content-Type = %q, want text/html", got)
	}
	if got := headers.Get("Content-Security-Policy"); got != cdnAlbumWrapperCSP {
		t.Errorf("album wrapper CSP = %q, want %q", got, cdnAlbumWrapperCSP)
	}
	if got := headers.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox allow-scripts allow-downloads") {
		t.Errorf("album wrapper CSP must sandbox with downloads enabled: %q", got)
	}
	if got := headers.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("album wrapper X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := headers.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("album wrapper X-Frame-Options = %q, want DENY", got)
	}
	if !strings.Contains(pageText, `const albumID = "`+uploaded.ID+`";`) {
		t.Error("album wrapper does not embed the id as the albumID data literal")
	}
	if count := strings.Count(pageText, uploaded.ID); count != 1 {
		t.Errorf("album wrapper mentions the id %d times, want exactly the 1 data literal", count)
	}
	if !strings.Contains(pageText, testCdnAlbumSalt) {
		t.Error("album wrapper does not embed the HKDF salt literal")
	}
	if !strings.Contains(pageText, "HKDF") || !strings.Contains(pageText, "item:") {
		t.Error("album wrapper does not derive per-item subkeys with HKDF")
	}
	if strings.Contains(pageText, "innerHTML") || strings.Contains(pageText, "<svg") {
		t.Error("album wrapper must not render markup from item data")
	}
	if strings.Contains(pageText, "http://") || strings.Contains(pageText, "https://") {
		t.Error("album wrapper must not reference external assets")
	}
	if bytes.Contains(page, []byte(fragmentKeyString(t, fragmentKey))) || bytes.Contains(page, album.manifest) {
		t.Error("album wrapper leaked the fragment key or the plaintext manifest")
	}
	for _, item := range items {
		if strings.Contains(pageText, item.name) {
			t.Errorf("album wrapper leaked the encrypted-away item name %q", item.name)
		}
	}

	// The encrypted manifest round-trips through the service and decrypts with
	// the existing app-contract helper under the "manifest" subkey.
	status, manifestBody, manifestHeaders := testCdnAlbumFetch(t, server, cdnPathPrefix+"/"+uploaded.ID+"/manifest")
	if status != http.StatusOK {
		t.Fatalf("manifest fetch status = %d, want %d", status, http.StatusOK)
	}
	if got := manifestHeaders.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("manifest Content-Type = %q, want application/octet-stream", got)
	}
	if got := manifestHeaders.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("manifest X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := manifestHeaders.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("manifest Access-Control-Allow-Origin = %q, want *", got)
	}
	manifestKey := appCdnAlbumSubkey(t, fragmentKey, "manifest")
	manifestPlain, err := appCdnDecrypt(t, manifestBody, base64.RawURLEncoding.EncodeToString(manifestKey))
	if err != nil {
		t.Fatalf("decrypt manifest with the manifest subkey: %v", err)
	}
	if !bytes.Equal(manifestPlain, album.manifest) {
		t.Errorf("decrypted manifest differs from the built one")
	}
	var manifest appCdnAlbumManifest
	if err := json.Unmarshal(manifestPlain, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Version != 1 || len(manifest.Items) != len(items) {
		t.Errorf("manifest = %+v, want version 1 with %d items", manifest, len(items))
	}
	for index, item := range items {
		if manifest.Items[index].Name != item.name || manifest.Items[index].Type != item.typ || manifest.Items[index].Size != int64(len(item.data)) {
			t.Errorf("manifest item %d = %+v, want %+v", index, manifest.Items[index], item)
		}
	}

	// Partial fetch: one item at a time, each under its own subkey, cross
	// checked through the existing AES-256-GCM helper. The order is
	// deliberately reversed to prove items are independent.
	for index := len(items) - 1; index >= 0; index-- {
		path := fmt.Sprintf("%s/%s/items/%d", cdnPathPrefix, uploaded.ID, index)
		status, body, itemHeaders := testCdnAlbumFetch(t, server, path)
		if status != http.StatusOK {
			t.Fatalf("item %d fetch status = %d, want %d", index, status, http.StatusOK)
		}
		if got := itemHeaders.Get("Content-Disposition"); !strings.Contains(got, "attachment") {
			t.Errorf("item %d Content-Disposition = %q, want attachment", index, got)
		}
		subkey := appCdnAlbumSubkey(t, fragmentKey, fmt.Sprintf("item:%d", index))
		plaintext, err := appCdnDecrypt(t, body, base64.RawURLEncoding.EncodeToString(subkey))
		if err != nil {
			t.Fatalf("decrypt item %d with its subkey: %v", index, err)
		}
		if !bytes.Equal(plaintext, items[index].data) {
			t.Errorf("item %d decrypted to %d bytes, want %d", index, len(plaintext), len(items[index].data))
		}
		wrong := appCdnAlbumSubkey(t, fragmentKey, fmt.Sprintf("item:%d", index+1))
		if _, err := appCdnDecrypt(t, body, base64.RawURLEncoding.EncodeToString(wrong)); err == nil {
			t.Errorf("item %d decrypted with a foreign subkey", index)
		}
	}

	// The file-only route is a 404 on an album.
	for _, path := range []string{
		cdnPathPrefix + "/" + uploaded.ID + "/raw",
		fmt.Sprintf("%s/%s/items/%d", cdnPathPrefix, uploaded.ID, len(items)),
		fmt.Sprintf("%s/%s/items/01", cdnPathPrefix, uploaded.ID),
		fmt.Sprintf("%s/%s/items/-1", cdnPathPrefix, uploaded.ID),
	} {
		status, _, _ := testCdnAlbumFetch(t, server, path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want %d", path, status, http.StatusNotFound)
		}
	}

	// Album routes are 404 on a plain file.
	status, fileID := testCdnUpload(t, server, "alice", []byte("file ciphertext"), "")
	if status != http.StatusCreated {
		t.Fatalf("file upload status = %d, want %d", status, http.StatusCreated)
	}
	for _, path := range []string{
		cdnPathPrefix + "/" + fileID + "/manifest",
		cdnPathPrefix + "/" + fileID + "/items/0",
	} {
		status, _, _ := testCdnAlbumFetch(t, server, path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s on a file status = %d, want %d", path, status, http.StatusNotFound)
		}
	}

	// Listing: files and albums are separate arrays; the album counts against
	// the same quota.
	resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix, "alice", nil)
	listBody := testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var listed cdnListResponse
	if err := json.Unmarshal(listBody, &listed); err != nil {
		t.Fatalf("decode list response %q: %v", listBody, err)
	}
	if len(listed.Files) != 1 || listed.Files[0].ID != fileID {
		t.Errorf("files list = %+v, want only the plain file", listed.Files)
	}
	if len(listed.Albums) != 1 || listed.Albums[0].ID != uploaded.ID || listed.Albums[0].ItemCount != len(items) {
		t.Errorf("albums list = %+v, want the uploaded album", listed.Albums)
	}
	if listed.UsedBytes != uploaded.Size+int64(len("file ciphertext")) {
		t.Errorf("used_bytes = %d, want the file plus the album", listed.UsedBytes)
	}
	albums, err := service.ListAlbums("alice")
	if err != nil || len(albums) != 1 || albums[0].ID != uploaded.ID {
		t.Errorf("ListAlbums = (%+v, %v), want the uploaded album", albums, err)
	}
	files, err := service.List("alice")
	if err != nil || len(files) != 1 || files[0].ID != fileID {
		t.Errorf("List = (%+v, %v), want only the plain file", files, err)
	}
	stats := service.Stats()
	if stats.Files != 1 || stats.Albums != 1 || stats.Bytes != listed.UsedBytes {
		t.Errorf("Stats = %+v, want 1 file, 1 album, %d bytes", stats, listed.UsedBytes)
	}
}

// fragmentKeyString encodes a fragment key the way links do (unpadded
// base64url), so tests can assert it never appears in a page.
func fragmentKeyString(t *testing.T, fragmentKey []byte) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString(fragmentKey)
}

func TestCdnAlbumCapsRejected(t *testing.T) {
	// Default caps: 20 items, 500 MB total. Both rejections happen from the
	// declared header alone, before any payload byte is read.
	service := testCdnService(t, nil)
	server := testCdnServer(t, service)

	// Album creation is authenticated like file uploads.
	resp := testCdnRequest(t, server.Client(), http.MethodPost, server.URL+cdnPathPrefix+"/albums", "", []byte("probe"))
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated album upload status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	status, _ := testCdnAlbumUpload(t, server, "alice", testCdnAlbumHeader(21, nil, 1), "")
	if status != http.StatusBadRequest {
		t.Fatalf("21-item album status = %d, want %d", status, http.StatusBadRequest)
	}

	sizes := make([]int64, 6)
	for index := range sizes {
		sizes[index] = int64(90) << 20 // each under the 100 MiB per-item cap
	}
	resp = testCdnRequest(t, server.Client(), http.MethodPost,
		server.URL+cdnPathPrefix+"/albums", "alice", testCdnAlbumHeader(6, sizes, 1<<10))
	body := testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf(">500 MB album status = %d, want %d (body %q)", resp.StatusCode, http.StatusRequestEntityTooLarge, body)
	}
	if !strings.Contains(string(body), "album_too_large") {
		t.Errorf(">500 MB album body = %q, want album_too_large", body)
	}

	// Nothing was staged or stored by the rejected uploads.
	if leftovers, err := os.ReadDir(service.objectDir()); err != nil || len(leftovers) != 0 {
		t.Errorf("rejected uploads left %d entries in the object dir (err = %v)", len(leftovers), err)
	}

	// Custom caps are honored: 2 items and 1 KiB per album.
	small := testCdnService(t, func(c *CdnConfig) {
		c.MaxAlbumItems = 2
		c.MaxAlbumBytes = 1 << 10
		c.MaxFileBytes = 1 << 12
	})
	smallServer := testCdnServer(t, small)

	status, _ = testCdnAlbumUpload(t, smallServer, "alice", testCdnAlbumHeader(3, nil, 1), "")
	if status != http.StatusBadRequest {
		t.Errorf("3-item album under max_album_items=2 status = %d, want %d", status, http.StatusBadRequest)
	}
	status, _ = testCdnAlbumUpload(t, smallServer, "alice", testCdnAlbumHeader(2, []int64{512, 512}, 1), "")
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("1025-byte album under max_album_bytes=1024 status = %d, want %d", status, http.StatusRequestEntityTooLarge)
	}

	fragmentKey := testCdnAlbumKey(t)
	album := buildTestCdnAlbum(t, fragmentKey, []testCdnAlbumItem{
		{name: "one.bin", typ: "application/octet-stream", data: bytes.Repeat([]byte{0x21}, 64)},
		{name: "two.bin", typ: "application/octet-stream", data: bytes.Repeat([]byte{0x22}, 64)},
	})
	status, uploaded := testCdnAlbumUpload(t, smallServer, "alice", album.container, "")
	if status != http.StatusCreated {
		t.Fatalf("2-item album within the custom caps status = %d, want %d", status, http.StatusCreated)
	}
	if uploaded.Items != 2 || uploaded.Size != album.payloadSize {
		t.Errorf("custom-cap album response = %+v, want 2 items / %d bytes", uploaded, album.payloadSize)
	}
	status, body, _ = testCdnAlbumFetch(t, smallServer, fmt.Sprintf("%s/%s/items/1", cdnPathPrefix, uploaded.ID))
	if status != http.StatusOK {
		t.Fatalf("item fetch under custom caps status = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	subkey := appCdnAlbumSubkey(t, fragmentKey, "item:1")
	if plaintext, err := appCdnDecrypt(t, body, base64.RawURLEncoding.EncodeToString(subkey)); err != nil || !bytes.Equal(plaintext, album.items[1].data) {
		t.Errorf("custom-cap item decrypt = (%q, %v), want %q", plaintext, err, album.items[1].data)
	}
}

func TestCdnAlbumContainerValidation(t *testing.T) {
	service := testCdnService(t, func(c *CdnConfig) {
		c.MaxAlbumItems = 2
		c.MaxAlbumBytes = 1 << 10
		c.MaxFileBytes = 1 << 10
	})

	valid := buildTestCdnAlbum(t, testCdnAlbumKey(t), []testCdnAlbumItem{
		{name: "one.bin", typ: "application/octet-stream", data: bytes.Repeat([]byte{0x31}, 100)},
	})
	trailing := append(append([]byte{}, valid.container...), 0x00)

	cases := []struct {
		name       string
		body       []byte
		wantStatus int
		wantCode   string
	}{
		{"short header", []byte("CDN"), http.StatusBadRequest, "bad_request"},
		{"bad magic", append([]byte("NOPE0001"), 1), http.StatusBadRequest, "bad_request"},
		{"zero items", append([]byte(cdnAlbumMagic), 0), http.StatusBadRequest, "bad_request"},
		{"too many items", testCdnAlbumHeader(3, nil, 1), http.StatusBadRequest, "album_too_many_items"},
		{"truncated lengths", append([]byte(cdnAlbumMagic), 1), http.StatusBadRequest, "bad_request"},
		{"empty item", testCdnAlbumHeader(1, []int64{0}, 1), http.StatusBadRequest, "bad_request"},
		{"item above per-item cap", testCdnAlbumHeader(1, []int64{1<<10 + 1}, 1), http.StatusRequestEntityTooLarge, "file_too_large"},
		{"album above cap", testCdnAlbumHeader(1, []int64{1 << 10}, 2), http.StatusRequestEntityTooLarge, "album_too_large"},
		{"empty manifest", testCdnAlbumHeader(1, []int64{10}, 0), http.StatusBadRequest, "bad_request"},
		{"manifest above cap", testCdnAlbumHeader(1, []int64{10}, cdnMaxManifestBytes+1), http.StatusBadRequest, "album_manifest_too_large"},
		{"truncated item", append(testCdnAlbumHeader(1, []int64{10}, 5), []byte{1, 2, 3, 4}...), http.StatusBadRequest, "bad_request"},
		{"truncated manifest", append(testCdnAlbumHeader(1, []int64{4}, 5), []byte{1, 2, 3, 4}...), http.StatusBadRequest, "bad_request"},
		{"trailing data", trailing, http.StatusBadRequest, "bad_request"},
	}
	for _, testCase := range cases {
		_, err := service.readAlbumContainer(bytes.NewReader(testCase.body))
		if err == nil {
			t.Errorf("%s: container accepted, want rejection", testCase.name)
			continue
		}
		var albumErr *cdnAlbumError
		if !errors.As(err, &albumErr) {
			t.Errorf("%s: error %v is not a cdnAlbumError", testCase.name, err)
			continue
		}
		if albumErr.status != testCase.wantStatus || albumErr.code != testCase.wantCode {
			t.Errorf("%s: got (%d, %q), want (%d, %q)",
				testCase.name, albumErr.status, albumErr.code, testCase.wantStatus, testCase.wantCode)
		}
	}

	if leftovers, err := os.ReadDir(service.objectDir()); err != nil || len(leftovers) != 0 {
		t.Errorf("rejected containers left %d entries in the object dir (err = %v)", len(leftovers), err)
	}
}

func TestCdnAlbumQuotaFIFO(t *testing.T) {
	service := testCdnService(t, func(c *CdnConfig) {
		c.MaxFileBytes = 4096
		c.MaxAlbumBytes = 4096
		c.PerUserCapBytes = 8000
	})
	server := testCdnServer(t, service)

	status, fileID := testCdnUpload(t, server, "alice", bytes.Repeat([]byte{'f'}, 4000), "")
	if status != http.StatusCreated {
		t.Fatalf("file upload status = %d, want %d", status, http.StatusCreated)
	}

	makeAlbum := func(label byte) (string, int64) {
		t.Helper()
		album := buildTestCdnAlbum(t, testCdnAlbumKey(t), []testCdnAlbumItem{
			{name: string(label) + ".bin", typ: "application/octet-stream", data: bytes.Repeat([]byte{label}, 1900)},
		})
		status, uploaded := testCdnAlbumUpload(t, server, "alice", album.container, "")
		if status != http.StatusCreated {
			t.Fatalf("album %c upload status = %d, want %d", label, status, http.StatusCreated)
		}
		return uploaded.ID, uploaded.Size
	}
	albumA, sizeA := makeAlbum('a')
	albumB, sizeB := makeAlbum('b')

	// The combined total crossed the cap: the oldest entry (the file) must be
	// gone, the albums kept, and used_bytes must match the albums exactly.
	resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix+"/"+fileID+"/raw", "", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("oldest file status = %d, want %d after FIFO pruning", resp.StatusCode, http.StatusNotFound)
	}
	entries, used, err := service.Usage("alice")
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(entries) != 2 || used != sizeA+sizeB {
		t.Errorf("Usage after pruning = (%d entries, %d bytes), want (2, %d)", len(entries), used, sizeA+sizeB)
	}
	if used > service.cfg.PerUserCapBytes {
		t.Errorf("used bytes %d exceed the %d cap", used, service.cfg.PerUserCapBytes)
	}

	// The next upload prunes the oldest album whole: manifest, items and all.
	status, _ = testCdnUpload(t, server, "alice", bytes.Repeat([]byte{'g'}, 4000), "")
	if status != http.StatusCreated {
		t.Fatalf("second file upload status = %d, want %d", status, http.StatusCreated)
	}
	for _, path := range []string{
		cdnPathPrefix + "/" + albumA,
		cdnPathPrefix + "/" + albumA + "/manifest",
		cdnPathPrefix + "/" + albumA + "/items/0",
	} {
		status, _, _ := testCdnAlbumFetch(t, server, path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s after album pruning status = %d, want %d", path, status, http.StatusNotFound)
		}
	}
	if _, err := os.Stat(filepath.Join(service.objectDir(), albumA)); !os.IsNotExist(err) {
		t.Errorf("pruned album directory still on disk (err = %v)", err)
	}
	if _, err := os.Stat(service.metaPath(albumA)); !os.IsNotExist(err) {
		t.Errorf("pruned album metadata still on disk (err = %v)", err)
	}
	status, _, _ = testCdnAlbumFetch(t, server, cdnPathPrefix+"/"+albumB+"/manifest")
	if status != http.StatusOK {
		t.Errorf("newer album manifest status = %d, want %d", status, http.StatusOK)
	}

	// Another user's quota and list stay independent.
	status, bobAlbum := testCdnAlbumUpload(t, server, "bob", buildTestCdnAlbum(t, testCdnAlbumKey(t), []testCdnAlbumItem{
		{name: "bob.bin", typ: "application/octet-stream", data: bytes.Repeat([]byte{'z'}, 512)},
	}).container, "")
	if status != http.StatusCreated {
		t.Fatalf("bob album upload status = %d, want %d", status, http.StatusCreated)
	}
	resp = testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix, "alice", nil)
	listBody := testCdnReadBody(t, resp)
	var listed cdnListResponse
	if err := json.Unmarshal(listBody, &listed); err != nil {
		t.Fatalf("decode alice list %q: %v", listBody, err)
	}
	for _, entry := range listed.Albums {
		if entry.ID == bobAlbum.ID {
			t.Error("alice's list reveals bob's album id")
		}
	}
	stats := service.Stats()
	if stats.Albums != 2 || stats.Files != 1 {
		t.Errorf("Stats = %+v, want 2 albums and 1 file across both users", stats)
	}
}

func TestCdnAlbumRevokeAndIsolation(t *testing.T) {
	service := testCdnService(t, nil)
	server := testCdnServer(t, service)

	album := buildTestCdnAlbum(t, testCdnAlbumKey(t), []testCdnAlbumItem{
		{name: "one.png", typ: "image/png", data: append([]byte{0x89, 0x50, 0x4e, 0x47}, bytes.Repeat([]byte{1}, 32)...)},
	})
	status, uploaded := testCdnAlbumUpload(t, server, "alice", album.container, "")
	if status != http.StatusCreated {
		t.Fatalf("album upload status = %d, want %d", status, http.StatusCreated)
	}

	// A foreign user cannot see or revoke it.
	resp := testCdnRequest(t, server.Client(), http.MethodGet, server.URL+cdnPathPrefix, "bob", nil)
	listBody := testCdnReadBody(t, resp)
	var listed cdnListResponse
	if err := json.Unmarshal(listBody, &listed); err != nil {
		t.Fatalf("decode bob list %q: %v", listBody, err)
	}
	for _, entry := range listed.Albums {
		if entry.ID == uploaded.ID {
			t.Error("bob's list reveals alice's album id")
		}
	}
	resp = testCdnRequest(t, server.Client(), http.MethodDelete, server.URL+cdnPathPrefix+"/"+uploaded.ID, "bob", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob revoke status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	status, _, _ = testCdnAlbumFetch(t, server, cdnPathPrefix+"/"+uploaded.ID+"/manifest")
	if status != http.StatusOK {
		t.Fatalf("album after foreign revoke status = %d, want %d", status, http.StatusOK)
	}

	// The owner revokes the whole album: every route dies and the directory is
	// gone; the second revoke looks like an unknown id.
	resp = testCdnRequest(t, server.Client(), http.MethodDelete, server.URL+cdnPathPrefix+"/"+uploaded.ID, "alice", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("owner revoke status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	for _, path := range []string{
		cdnPathPrefix + "/" + uploaded.ID,
		cdnPathPrefix + "/" + uploaded.ID + "/manifest",
		cdnPathPrefix + "/" + uploaded.ID + "/items/0",
	} {
		status, _, _ := testCdnAlbumFetch(t, server, path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s after revoke status = %d, want %d", path, status, http.StatusNotFound)
		}
	}
	if _, err := os.Stat(filepath.Join(service.objectDir(), uploaded.ID)); !os.IsNotExist(err) {
		t.Errorf("revoked album directory still on disk (err = %v)", err)
	}
	resp = testCdnRequest(t, server.Client(), http.MethodDelete, server.URL+cdnPathPrefix+"/"+uploaded.ID, "alice", nil)
	_ = testCdnReadBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second revoke status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	ok, err := service.Revoke("alice", uploaded.ID)
	if err != nil || ok {
		t.Errorf("Revoke after deletion = (%t, %v), want (false, nil)", ok, err)
	}
}

func TestCdnAlbumExpiryAndGC(t *testing.T) {
	service := testCdnService(t, nil)
	server := testCdnServer(t, service)

	// Expiry overrides are validated before the body is read.
	status, _ := testCdnAlbumUpload(t, server, "alice", []byte("not a container"), "?expires=400d")
	if status != http.StatusBadRequest {
		t.Fatalf("over-a-year album expiry status = %d, want %d", status, http.StatusBadRequest)
	}
	status, _ = testCdnAlbumUpload(t, server, "alice", []byte("not a container"), "?expires=0")
	if status != http.StatusBadRequest {
		t.Fatalf("zero album expiry status = %d, want %d", status, http.StatusBadRequest)
	}

	album := buildTestCdnAlbum(t, testCdnAlbumKey(t), []testCdnAlbumItem{
		{name: "short.bin", typ: "application/octet-stream", data: []byte("short-lived album")},
	})
	status, uploaded := testCdnAlbumUpload(t, server, "alice", album.container, "?expires=50ms")
	if status != http.StatusCreated {
		t.Fatalf("short-lived album upload status = %d, want %d", status, http.StatusCreated)
	}
	if stats := service.Stats(); stats.Albums != 1 {
		t.Fatalf("Stats before expiry = %+v, want 1 active album", stats)
	}

	time.Sleep(120 * time.Millisecond)
	for _, path := range []string{
		cdnPathPrefix + "/" + uploaded.ID,
		cdnPathPrefix + "/" + uploaded.ID + "/manifest",
		cdnPathPrefix + "/" + uploaded.ID + "/items/0",
	} {
		status, _, _ := testCdnAlbumFetch(t, server, path)
		if status != http.StatusNotFound {
			t.Errorf("expired album route %s status = %d, want %d before cleanup", path, status, http.StatusNotFound)
		}
	}
	if stats := service.Stats(); stats.Albums != 0 || stats.Expired != 1 {
		t.Errorf("Stats after expiry = %+v, want 0 active and 1 expired album", stats)
	}

	removed, err := service.RunGC(time.Now())
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if removed != 1 {
		t.Errorf("RunGC removed %d entries, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(service.objectDir(), uploaded.ID)); !os.IsNotExist(err) {
		t.Errorf("expired album directory still on disk after cleanup (err = %v)", err)
	}
	if _, err := os.Stat(service.metaPath(uploaded.ID)); !os.IsNotExist(err) {
		t.Errorf("expired album metadata still on disk after cleanup (err = %v)", err)
	}
}

func TestCdnAlbumHostileRoutes(t *testing.T) {
	service := testCdnService(t, nil)
	server := testCdnServer(t, service)

	album := buildTestCdnAlbum(t, testCdnAlbumKey(t), []testCdnAlbumItem{
		{name: "first.bin", typ: "application/octet-stream", data: bytes.Repeat([]byte{7}, 48)},
		{name: "second.bin", typ: "application/octet-stream", data: bytes.Repeat([]byte{8}, 48)},
	})
	status, uploaded := testCdnAlbumUpload(t, server, "alice", album.container, "")
	if status != http.StatusCreated {
		t.Fatalf("album upload status = %d, want %d", status, http.StatusCreated)
	}

	// Hostile ids on every album route: 404 and never reflected.
	for _, id := range []string{"..%2F..%2Fetc%2Fpasswd", "not-an-id!", strings.Repeat("a", 65)} {
		for _, suffix := range []string{"", "/manifest", "/items/0", "/raw"} {
			path := cdnPathPrefix + "/" + id + suffix
			status, body, _ := testCdnAlbumFetch(t, server, path)
			if status != http.StatusNotFound {
				t.Errorf("GET %s status = %d, want %d", path, status, http.StatusNotFound)
			}
			if bytes.Contains(body, []byte("passwd")) || bytes.Contains(body, []byte("<script")) {
				t.Errorf("GET %s reflected the hostile id", path)
			}
		}
	}

	// A well-formed but unknown id is a 404 on all album routes.
	missing := strings.Repeat("z", 43)
	for _, suffix := range []string{"", "/manifest", "/items/0"} {
		path := cdnPathPrefix + "/" + missing + suffix
		status, _, _ := testCdnAlbumFetch(t, server, path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want %d", path, status, http.StatusNotFound)
		}
	}

	// Hostile or out-of-range indices on a live album are 404s.
	for _, index := range []string{"2", "999", "-1", "0x0", "01", "1.0", "..%2F0", "%2e%2e", "0%00"} {
		path := fmt.Sprintf("%s/%s/items/%s", cdnPathPrefix, uploaded.ID, index)
		status, _, _ := testCdnAlbumFetch(t, server, path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want %d", path, status, http.StatusNotFound)
		}
	}
}

func TestCdnAlbumManifestStaysOpaque(t *testing.T) {
	service := testCdnService(t, nil)
	server := testCdnServer(t, service)

	hostileName := `</script><script>alert("album")</script>`
	fragmentKey := testCdnAlbumKey(t)
	album := buildTestCdnAlbum(t, fragmentKey, []testCdnAlbumItem{
		{name: hostileName, typ: `"><img src=x onerror="alert(1)">`, data: []byte("hostile item data")},
		{name: "safe.txt", typ: "text/plain", data: []byte("safe item data")},
	})
	status, uploaded := testCdnAlbumUpload(t, server, "alice", album.container, "")
	if status != http.StatusCreated {
		t.Fatalf("album upload status = %d, want %d", status, http.StatusCreated)
	}

	// The manifest object is served as opaque ciphertext: the plaintext names
	// and types never appear on the wire.
	status, manifestBody, _ := testCdnAlbumFetch(t, server, cdnPathPrefix+"/"+uploaded.ID+"/manifest")
	if status != http.StatusOK {
		t.Fatalf("manifest fetch status = %d, want %d", status, http.StatusOK)
	}
	if bytes.Contains(manifestBody, []byte(hostileName)) || bytes.Contains(manifestBody, []byte("alert(1)")) {
		t.Error("the manifest route served plaintext names")
	}
	manifestKey := appCdnAlbumSubkey(t, fragmentKey, "manifest")
	manifestPlain, err := appCdnDecrypt(t, manifestBody, base64.RawURLEncoding.EncodeToString(manifestKey))
	if err != nil {
		t.Fatalf("decrypt manifest: %v", err)
	}
	var manifest appCdnAlbumManifest
	if err := json.Unmarshal(manifestPlain, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.Items) != 2 || manifest.Items[0].Name != hostileName {
		t.Errorf("manifest item 0 = %+v, want the hostile name verbatim", manifest.Items[0])
	}

	// The gallery page never embeds the hostile name/types: they arrive only
	// through the encrypted manifest and are written with textContent.
	status, page, _ := testCdnAlbumFetch(t, server, cdnPathPrefix+"/"+uploaded.ID)
	if status != http.StatusOK {
		t.Fatalf("album wrapper status = %d, want %d", status, http.StatusOK)
	}
	if bytes.Contains(page, []byte("alert(")) || bytes.Contains(page, []byte(hostileName)) {
		t.Error("the gallery page embedded the hostile manifest content")
	}
	if !bytes.Contains(page, []byte("textContent")) {
		t.Error("the gallery page must render manifest strings with textContent")
	}
}
