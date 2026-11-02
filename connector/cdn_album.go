// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

// This file implements the CDN album extension described in
// docs/cdn-file-service.md. An album is one unguessable id holding up to
// max_album_items encrypted items plus one client-encrypted manifest (item
// names/types/sizes). The link carries a single fragment key; per-item
// subkeys are derived with HKDF-SHA256(fragmentKey, salt "cdn-album-item",
// info "item:<index>"), and the manifest uses info "manifest". The bridge
// never sees names, types, or any key: it stores and serves ciphertext only,
// exactly like single files.
//
// Storage layout (inside the shared object directory):
//
//	<data_dir>/objects/<id>                  album directory
//	<data_dir>/objects/<id>.meta.json        metadata: kind=album, item count, total size
//	<data_dir>/objects/<id>/manifest         encrypted manifest object
//	<data_dir>/objects/<id>/items/<index>    encrypted item ciphertext, 0-based

import (
	"encoding/binary"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	// cdnAlbumMagic starts every album upload container. The trailing 01 is
	// the container version and part of the protocol.
	cdnAlbumMagic = "CDNALB01"
	// cdnAlbumManifestName is the stored manifest object inside an album
	// directory.
	cdnAlbumManifestName = "manifest"
	// cdnAlbumItemsDirName is the item directory inside an album directory.
	cdnAlbumItemsDirName = "items"
)

// cdnAlbumWrapperCSP extends the file wrapper policy with allow-downloads:
// the album page offers "other types" as download cards, and downloads still
// require a user gesture.
const cdnAlbumWrapperCSP = cdnWrapperCSP + " allow-downloads"

// cdnAlbumError is a rejected album container: the HTTP status and API error
// code the upload endpoint reports. Internal failures use plain errors and
// surface as 500.
type cdnAlbumError struct {
	status  int
	code    string
	message string
}

// Error implements error.
func (e *cdnAlbumError) Error() string {
	return e.message
}

// cdnAlbumBadRequest builds the 400 envelope of a malformed container.
func cdnAlbumBadRequest(code, message string) *cdnAlbumError {
	return &cdnAlbumError{status: http.StatusBadRequest, code: code, message: message}
}

// cdnAlbumTooLarge builds the 413 envelope of an album above max_album_bytes.
func (s *CdnService) cdnAlbumTooLarge() *cdnAlbumError {
	return &cdnAlbumError{
		status:  http.StatusRequestEntityTooLarge,
		code:    "album_too_large",
		message: fmt.Sprintf("the encrypted album exceeds the %d byte limit", s.cfg.MaxAlbumBytes),
	}
}

// cdnAlbumUpload is a parsed album container staged in a temporary directory.
// dir is emptied once the directory has been claimed by a rename, so cleanup
// never follows a stored album.
type cdnAlbumUpload struct {
	dir   string
	size  int64
	items int
}

// cleanup removes a staged container that was not stored. It is a no-op after
// a successful rename or on an already-cleaned upload.
func (u *cdnAlbumUpload) cleanup() {
	if u == nil || u.dir == "" {
		return
	}
	_ = os.RemoveAll(u.dir)
	u.dir = ""
}

// albumManifestPath is the stored manifest object of an album.
func (s *CdnService) albumManifestPath(id string) string {
	return filepath.Join(s.objectPath(id), cdnAlbumManifestName)
}

// albumItemsDir is the directory holding an album's item ciphertexts.
func (s *CdnService) albumItemsDir(id string) string {
	return filepath.Join(s.objectPath(id), cdnAlbumItemsDirName)
}

// albumItemPath is the stored ciphertext of one album item.
func (s *CdnService) albumItemPath(id, index string) string {
	return filepath.Join(s.albumItemsDir(id), index)
}

// albumDirComplete reports whether an album directory still holds the
// manifest and every item its metadata promises. It is used by the cleanup to
// remove albums that lost parts (or grew a hostile item count) whole.
func (s *CdnService) albumDirComplete(id string, entry *CdnEntry) bool {
	if !entry.isAlbum() || entry.ItemCount <= 0 || entry.ItemCount > cdnHardMaxAlbumItems {
		return false
	}
	if info, err := os.Stat(s.albumManifestPath(id)); err != nil || info.IsDir() {
		return false
	}
	if info, err := os.Stat(s.albumItemsDir(id)); err != nil || !info.IsDir() {
		return false
	}
	for index := 0; index < entry.ItemCount; index++ {
		if info, err := os.Stat(s.albumItemPath(id, strconv.Itoa(index))); err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

// validCdnAlbumIndex accepts the canonical decimal spelling of a position in
// a bounded album: digits only, no leading zeros, no signs, and inside the
// stored item count.
func validCdnAlbumIndex(raw string, count int) (int, bool) {
	if raw == "" || len(raw) > 3 || count <= 0 || count > cdnHardMaxAlbumItems {
		return 0, false
	}
	for index := 0; index < len(raw); index++ {
		if raw[index] < '0' || raw[index] > '9' {
			return 0, false
		}
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value >= count || strconv.Itoa(value) != raw {
		return 0, false
	}
	return value, true
}

// loadAlbumEntry returns one active album entry. Files, missing, malformed
// and expired ids all return nil, which callers surface as 404.
func (s *CdnService) loadAlbumEntry(id string) (*CdnEntry, error) {
	entry, err := s.loadEntry(id)
	if err != nil || entry == nil || !entry.isAlbum() {
		return nil, err
	}
	return entry, nil
}

// readAlbumContainer parses and stages one album upload body. The declared
// item count and sizes are validated before any payload is read, so oversized
// or over-count albums are rejected without transferring bytes. The payload
// is streamed into a temporary directory (never a shared path); the caller
// either renames it into the object directory or cleans it up.
func (s *CdnService) readAlbumContainer(body io.Reader) (*cdnAlbumUpload, error) {
	header := make([]byte, len(cdnAlbumMagic)+1)
	if _, err := io.ReadFull(body, header); err != nil {
		return nil, cdnAlbumBadRequest("bad_request", "the request body is not an album container (missing the header)")
	}
	if string(header[:len(cdnAlbumMagic)]) != cdnAlbumMagic {
		return nil, cdnAlbumBadRequest("bad_request", "the request body is not a supported album container (bad magic)")
	}
	count := int(header[len(cdnAlbumMagic)])
	if count == 0 {
		return nil, cdnAlbumBadRequest("bad_request", "an album needs at least one item")
	}
	if count > s.cfg.MaxAlbumItems {
		return nil, cdnAlbumBadRequest("album_too_many_items",
			fmt.Sprintf("the album has %d items, above the %d item limit", count, s.cfg.MaxAlbumItems))
	}

	sizes := make([]byte, count*8+8)
	if _, err := io.ReadFull(body, sizes); err != nil {
		return nil, cdnAlbumBadRequest("bad_request", "the album container header is truncated")
	}
	lengths := make([]int64, count)
	var total int64
	for index := 0; index < count; index++ {
		length := int64(binary.BigEndian.Uint64(sizes[index*8:]))
		if length <= 0 {
			return nil, cdnAlbumBadRequest("bad_request", fmt.Sprintf("album item %d is empty", index))
		}
		if length > s.cfg.MaxFileBytes {
			return nil, &cdnAlbumError{
				status:  http.StatusRequestEntityTooLarge,
				code:    "file_too_large",
				message: fmt.Sprintf("album item %d exceeds the %d byte per-item limit", index, s.cfg.MaxFileBytes),
			}
		}
		lengths[index] = length
		if length > s.cfg.MaxAlbumBytes-total {
			return nil, s.cdnAlbumTooLarge()
		}
		total += length
	}
	manifestLength := int64(binary.BigEndian.Uint64(sizes[count*8:]))
	if manifestLength <= 0 {
		return nil, cdnAlbumBadRequest("bad_request", "the album manifest is empty")
	}
	if manifestLength > cdnMaxManifestBytes {
		return nil, cdnAlbumBadRequest("album_manifest_too_large",
			fmt.Sprintf("the album manifest exceeds the %d byte limit", cdnMaxManifestBytes))
	}
	if manifestLength > s.cfg.MaxAlbumBytes-total {
		return nil, s.cdnAlbumTooLarge()
	}
	total += manifestLength

	dir, err := os.MkdirTemp(s.objectDir(), ".tmp-album-*")
	if err != nil {
		return nil, fmt.Errorf("create album staging dir: %w", err)
	}
	upload := &cdnAlbumUpload{dir: dir, size: total, items: count}
	fail := func(err error) (*cdnAlbumUpload, error) {
		upload.cleanup()
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(dir, cdnAlbumItemsDirName), 0o700); err != nil {
		return fail(fmt.Errorf("create album items dir: %w", err))
	}
	for index := 0; index < count; index++ {
		path := filepath.Join(dir, cdnAlbumItemsDirName, strconv.Itoa(index))
		if err := copyCdnAlbumPart(body, path, lengths[index]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return fail(cdnAlbumBadRequest("bad_request", fmt.Sprintf("the album container is truncated in item %d", index)))
			}
			return fail(fmt.Errorf("store album item %d: %w", index, err))
		}
	}
	if err := copyCdnAlbumPart(body, filepath.Join(dir, cdnAlbumManifestName), manifestLength); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fail(cdnAlbumBadRequest("bad_request", "the album container is truncated in the manifest"))
		}
		return fail(fmt.Errorf("store album manifest: %w", err))
	}

	// The declared container is the whole body: a trailing byte means the
	// client and the bridge disagree about the framing, so nothing is stored.
	var trailing [1]byte
	if n, err := io.ReadFull(body, trailing[:]); n > 0 {
		return fail(cdnAlbumBadRequest("bad_request", "unexpected data after the album container"))
	} else if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fail(fmt.Errorf("read album trailer: %w", err))
	}
	return upload, nil
}

// copyCdnAlbumPart stores exactly length bytes of one container part.
func copyCdnAlbumPart(body io.Reader, path string, length int64) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(file, body, length); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// cdnAlbumUploadResponse is the POST /f/albums response: the album id the
// uploader embeds in the link. The key never appears here.
type cdnAlbumUploadResponse struct {
	ID          string `json:"id"`
	Size        int64  `json:"size"`
	Items       int    `json:"items"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
}

// handleAlbumUpload stores one album container: up to max_album_items
// encrypted items plus an encrypted manifest, in one request, under one
// unguessable id. Item names and types stay inside the client-encrypted
// manifest, so the bridge only ever sees ciphertext and sizes.
func (s *CdnService) handleAlbumUpload(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.authenticate(r)
	if !ok {
		writeCdnError(w, http.StatusUnauthorized, "unauthorized", "a valid device bearer token is required")
		return
	}
	expiry, err := s.resolveUploadExpiry(r)
	if err != nil {
		writeCdnError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	upload, err := s.readAlbumContainer(r.Body)
	if err != nil {
		var albumErr *cdnAlbumError
		if errors.As(err, &albumErr) {
			writeCdnError(w, albumErr.status, albumErr.code, albumErr.message)
		} else {
			s.log.Warn().Err(err).Msg("Failed to store a CDN album upload")
			writeCdnError(w, http.StatusInternalServerError, "internal", "failed to store the album")
		}
		return
	}
	defer upload.cleanup()
	if upload.size > s.cfg.PerUserCapBytes {
		writeCdnError(w, http.StatusRequestEntityTooLarge, "quota_exceeded",
			fmt.Sprintf("the album exceeds your %d byte storage quota", s.cfg.PerUserCapBytes))
		return
	}
	id, err := newCdnID()
	if err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to generate an album id")
		return
	}

	now := time.Now().UTC()
	entry := CdnEntry{
		ID:        id,
		Owner:     owner,
		Kind:      cdnKindAlbum,
		Size:      upload.size,
		ItemCount: upload.items,
		CreatedAt: now,
		ExpiresAt: now.Add(expiry),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(upload.dir, s.objectPath(id)); err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to store the album")
		return
	}
	upload.dir = ""
	if err := s.writeMetaLocked(entry); err != nil {
		_ = os.RemoveAll(s.objectPath(id))
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to store the album metadata")
		return
	}
	pruned, err := s.pruneOwnerLocked(owner, id)
	if err != nil {
		s.log.Warn().Err(err).Str("owner", owner).Msg("Failed to prune CDN entries over the per-user quota")
	}
	if pruned > 0 {
		s.log.Info().Str("owner", owner).Int("pruned", pruned).Msg("Pruned oldest CDN entries over the per-user quota")
	}
	writeCdnJSON(w, http.StatusCreated, cdnAlbumUploadResponse{
		ID:          id,
		Size:        upload.size,
		Items:       upload.items,
		ExpiresAtMS: entry.ExpiresAt.UnixMilli(),
	})
}

// handleAlbumManifest serves the client-encrypted manifest object. The
// manifest format and its subkey are specified in docs/cdn-file-service.md;
// the bridge never parses it.
func (s *CdnService) handleAlbumManifest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entry, err := s.loadAlbumEntry(id)
	if err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to read the album metadata")
		return
	}
	if entry == nil {
		http.NotFound(w, r)
		return
	}
	s.writeCdnObject(w, r, s.albumManifestPath(id), id+"-manifest.bin")
}

// handleAlbumItem serves one item of an active album by its 0-based position,
// so viewers can fetch partial albums item by item.
func (s *CdnService) handleAlbumItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entry, err := s.loadAlbumEntry(id)
	if err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to read the album metadata")
		return
	}
	if entry == nil {
		http.NotFound(w, r)
		return
	}
	index, ok := validCdnAlbumIndex(r.PathValue("index"), entry.ItemCount)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.writeCdnObject(w, r, s.albumItemPath(id, strconv.Itoa(index)), fmt.Sprintf("%s-%d.bin", id, index))
}

// cdnAlbumWrapperTemplate is the gallery page for album links: it decrypts
// the manifest and each requested item in the browser, renders images in a
// grid, audio/video through blob players, and everything else as a download
// card. Only sniffed image/audio/video payloads are rendered; names and types
// are always written with textContent, never as markup.
var cdnAlbumWrapperTemplate = template.Must(template.New("cdn-album-wrapper").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="referrer" content="no-referrer">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Encrypted album</title>
<style>
body { margin: 0; background: #111; color: #eee; font-family: system-ui, sans-serif; }
h1 { font-size: 1.1rem; padding: 1rem 1.5rem 0; margin: 0; }
#status { padding: 1rem 1.5rem; }
#grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(180px, 1fr)); gap: .75rem; padding: 0 1.5rem 1.5rem; }
.cell { background: #1b1b1b; border: 1px solid #2a2a2a; border-radius: .5rem; overflow: hidden; display: flex; flex-direction: column; }
.preview { min-height: 120px; display: flex; align-items: center; justify-content: center; background: #000; color: #666; font-size: 2rem; overflow: hidden; }
.preview img, .preview video { max-width: 100%; max-height: 220px; display: block; }
.preview audio { width: 100%; }
.meta { padding: .5rem .6rem; font-size: .85rem; word-break: break-word; }
.meta .name { color: #fff; }
.meta .info { color: #999; font-size: .78rem; margin-top: .15rem; }
button { margin: 0 .6rem .6rem; padding: .4rem .6rem; background: #2f6feb; color: #fff; border: 0; border-radius: .3rem; cursor: pointer; font: inherit; }
button:disabled { background: #333; color: #999; cursor: default; }
</style>
</head>
<body>
<h1>Encrypted album</h1>
<p id="status">Decrypting…</p>
<main id="grid"></main>
<script>
"use strict";
const albumID = {{.ID}};
const manifestURL = "/f/" + encodeURIComponent(albumID) + "/manifest";
const saltBytes = new TextEncoder().encode("cdn-album-item");
const status = document.getElementById("status");
const grid = document.getElementById("grid");
const MAX_ITEMS = 255;

function fail(message) { status.textContent = message; }

function itemURL(index) { return "/f/" + encodeURIComponent(albumID) + "/items/" + index; }

function decodeKey(text) {
	if (!/^[A-Za-z0-9_-]+$/.test(text)) throw new Error("invalid key");
	const base64 = text.replace(/-/g, "+").replace(/_/g, "/");
	const padded = base64 + "=".repeat((4 - (base64.length % 4)) % 4);
	const binary = atob(padded);
	const bytes = new Uint8Array(binary.length);
	for (let index = 0; index < binary.length; index++) bytes[index] = binary.charCodeAt(index);
	return bytes;
}

function matchesAt(bytes, offset, text) {
	if (bytes.length < offset + text.length) return false;
	for (let index = 0; index < text.length; index++) {
		if (bytes[offset + index] !== text.charCodeAt(index)) return false;
	}
	return true;
}

function sniffType(bytes) {
	if (bytes.length >= 3 && bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) return "image/jpeg";
	if (bytes.length >= 8 && bytes[0] === 0x89 && bytes[1] === 0x50 && bytes[2] === 0x4e && bytes[3] === 0x47) return "image/png";
	if (matchesAt(bytes, 0, "GIF87a") || matchesAt(bytes, 0, "GIF89a")) return "image/gif";
	if (matchesAt(bytes, 0, "RIFF") && matchesAt(bytes, 8, "WEBP")) return "image/webp";
	if (matchesAt(bytes, 4, "ftyp") && (matchesAt(bytes, 8, "avif") || matchesAt(bytes, 8, "avis"))) return "image/avif";
	if (matchesAt(bytes, 0, "OggS")) return "audio/ogg";
	if (matchesAt(bytes, 0, "fLaC")) return "audio/flac";
	if (matchesAt(bytes, 0, "RIFF") && matchesAt(bytes, 8, "WAVE")) return "audio/wav";
	if (matchesAt(bytes, 0, "ID3")) return "audio/mpeg";
	if (bytes.length >= 2 && bytes[0] === 0xff && (bytes[1] & 0xe0) === 0xe0) return "audio/mpeg";
	if (matchesAt(bytes, 4, "ftyp") && matchesAt(bytes, 8, "M4A ")) return "audio/mp4";
	if (bytes.length >= 4 && bytes[0] === 0x1a && bytes[1] === 0x45 && bytes[2] === 0xdf && bytes[3] === 0xa3) return "video/webm";
	if (matchesAt(bytes, 0, "%PDF-")) return "application/pdf";
	if (matchesAt(bytes, 4, "ftyp")) return "video/mp4";
	return "";
}

function formatBytes(size) {
	if (!Number.isFinite(size) || size < 0) return "";
	if (size >= 1 << 30) return (size / (1 << 30)).toFixed(1) + " GiB";
	if (size >= 1 << 20) return (size / (1 << 20)).toFixed(1) + " MiB";
	if (size >= 1 << 10) return (size / (1 << 10)).toFixed(1) + " KiB";
	return size + " B";
}

async function deriveItemKey(fragmentKey, info) {
	const hkdfKey = await crypto.subtle.importKey("raw", fragmentKey, "HKDF", false, ["deriveBits"]);
	const bits = await crypto.subtle.deriveBits(
		{ name: "HKDF", hash: "SHA-256", salt: saltBytes, info: new TextEncoder().encode(info) },
		hkdfKey, 256);
	return crypto.subtle.importKey("raw", bits, "AES-GCM", false, ["decrypt"]);
}

async function fetchDecrypt(url, aesKey) {
	const response = await fetch(url, { cache: "no-store" });
	if (!response.ok) throw new Error("HTTP " + response.status);
	const ciphertext = new Uint8Array(await response.arrayBuffer());
	if (ciphertext.length <= 12) throw new Error("ciphertext is too short");
	return new Uint8Array(await crypto.subtle.decrypt(
		{ name: "AES-GCM", iv: ciphertext.slice(0, 12) }, aesKey, ciphertext.slice(12)));
}

function element(tag, className) {
	const node = document.createElement(tag);
	if (className) node.className = className;
	return node;
}

function showError(infoNode, message) {
	infoNode.textContent = (infoNode.textContent ? infoNode.textContent + " — " : "") + message;
}

async function loadPreview(preview, index, fragmentKey, kind) {
	const aesKey = await deriveItemKey(fragmentKey, "item:" + index);
	const bytes = await fetchDecrypt(itemURL(index), aesKey);
	const type = sniffType(bytes);
	const wanted = kind === "image" ? type.indexOf("image/") === 0
		: type.indexOf("audio/") === 0 || type.indexOf("video/") === 0;
	if (!wanted) throw new Error("the payload does not match its type");
	let node;
	if (type.indexOf("image/") === 0) {
		node = element("img");
		node.alt = "Decrypted image";
		node.loading = "lazy";
	} else if (type.indexOf("video/") === 0) {
		node = element("video");
		node.controls = true;
		node.playsInline = true;
	} else {
		node = element("audio");
		node.controls = true;
	}
	node.src = URL.createObjectURL(new Blob([bytes], { type: type }));
	preview.replaceChildren(node);
}

async function downloadItem(index, fragmentKey, name) {
	const aesKey = await deriveItemKey(fragmentKey, "item:" + index);
	const bytes = await fetchDecrypt(itemURL(index), aesKey);
	const url = URL.createObjectURL(new Blob([bytes], { type: "application/octet-stream" }));
	const link = element("a");
	link.href = url;
	link.download = name;
	link.rel = "noopener";
	link.hidden = true;
	document.body.appendChild(link);
	link.click();
	link.remove();
}

function addDownloadButton(cell, index, fragmentKey, name, infoNode) {
	const button = element("button");
	button.type = "button";
	button.textContent = "Download";
	button.addEventListener("click", () => {
		button.disabled = true;
		button.textContent = "Decrypting…";
		downloadItem(index, fragmentKey, name).then(() => {
			button.disabled = false;
			button.textContent = "Download";
		}).catch((error) => {
			button.disabled = false;
			button.textContent = "Retry";
			showError(infoNode, "download failed: " + error.message);
		});
	});
	cell.appendChild(button);
}

function makeCell(item, index, fragmentKey) {
	const name = typeof item.name === "string" && item.name ? item.name : "item " + (index + 1);
	const type = typeof item.type === "string" ? item.type : "";
	const size = typeof item.size === "number" && Number.isFinite(item.size) && item.size >= 0 ? item.size : -1;
	const kind = type.indexOf("image/") === 0 ? "image"
		: type.indexOf("audio/") === 0 || type.indexOf("video/") === 0 ? "av" : "file";

	const cell = element("div", "cell");
	const preview = element("div", "preview");
	const meta = element("div", "meta");
	const nameNode = element("div", "name");
	nameNode.textContent = name;
	const infoNode = element("div", "info");
	infoNode.textContent = (type || "file") + (size >= 0 ? " · " + formatBytes(size) : "");
	meta.appendChild(nameNode);
	meta.appendChild(infoNode);
	cell.appendChild(preview);
	cell.appendChild(meta);

	if (kind === "image") {
		const load = () => {
			preview.textContent = "Decrypting…";
			loadPreview(preview, index, fragmentKey, "image").catch((error) => {
				preview.textContent = "";
				showError(infoNode, "preview failed: " + error.message);
				addDownloadButton(cell, index, fragmentKey, name, infoNode);
			});
		};
		if (typeof IntersectionObserver === "undefined") {
			load();
		} else {
			preview.textContent = "Image";
			const observer = new IntersectionObserver((entries) => {
				if (!entries.some((entry) => entry.isIntersecting)) return;
				observer.disconnect();
				load();
			}, { rootMargin: "200px" });
			observer.observe(cell);
		}
		return cell;
	}

	if (kind === "av") {
		const button = element("button");
		button.type = "button";
		button.textContent = type.indexOf("video/") === 0 ? "Load video" : "Load audio";
		button.addEventListener("click", () => {
			button.disabled = true;
			button.textContent = "Decrypting…";
			loadPreview(preview, index, fragmentKey, "av").then(() => button.remove()).catch((error) => {
				button.disabled = false;
				button.textContent = "Retry";
				showError(infoNode, "load failed: " + error.message);
			});
		});
		preview.textContent = "▸";
		cell.appendChild(button);
		return cell;
	}

	preview.textContent = "⇩";
	addDownloadButton(cell, index, fragmentKey, name, infoNode);
	return cell;
}

function parseManifest(raw) {
	let manifest;
	try {
		manifest = JSON.parse(new TextDecoder().decode(raw));
	} catch {
		throw new Error("the manifest is not JSON");
	}
	if (!manifest || typeof manifest !== "object" || manifest.version !== 1) throw new Error("unsupported manifest version");
	if (!Array.isArray(manifest.items) || manifest.items.length === 0) throw new Error("the manifest has no items");
	if (manifest.items.length > MAX_ITEMS) throw new Error("the manifest has too many items");
	return manifest.items;
}

async function main() {
	const keyText = location.hash.slice(1);
	if (!keyText) { fail("Missing decryption key: the part of the link after # is required."); return; }
	let keyBytes;
	try { keyBytes = decodeKey(keyText); } catch { fail("The decryption key in the link is invalid."); return; }
	if (keyBytes.length !== 32) { fail("The link key is not a 32-byte AES-256 key."); return; }

	let items;
	try {
		const manifestKey = await deriveItemKey(keyBytes, "manifest");
		items = parseManifest(await fetchDecrypt(manifestURL, manifestKey));
	} catch (error) {
		fail("Could not decrypt the album manifest: " + (error && error.message ? error.message : "unknown error") + ".");
		return;
	}
	status.remove();
	for (let index = 0; index < items.length; index++) {
		grid.appendChild(makeCell(items[index], index, keyBytes));
	}
}

main();
</script>
</body>
</html>
`))
