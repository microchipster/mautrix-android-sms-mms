// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/bridgev2/commands"
)

// This file implements the bridge side of the zero-knowledge file service
// ("CDN") described in docs/cdn-file-service.md. Files are encrypted
// client-side (AES-256-GCM) and the key lives only in the URL fragment, so
// the bridge stores and serves ciphertext only. The unguessable id in the
// path is the whole secret: the download endpoints require no headers or
// cookies.

const (
	// cdnPathPrefix is the public path space of the file service.
	cdnPathPrefix = "/f"

	// cdnIDBytes is the entropy of one file id: 32 random bytes encode to 43
	// base64url characters, far beyond guessing range.
	cdnIDBytes = 32
	// cdnMinIDLen / cdnMaxIDLen bound ids accepted from a URL before any
	// lookup happens.
	cdnMinIDLen = 22
	cdnMaxIDLen = 64

	// cdnDefaultMaxFileBytes caps one ciphertext upload (100 MiB).
	cdnDefaultMaxFileBytes = int64(100) << 20
	// cdnDefaultPerUserCapBytes caps the storage per user (1 GiB). Files
	// beyond the cap are removed oldest-first.
	cdnDefaultPerUserCapBytes = int64(1) << 30
	// cdnDefaultExpiry is the default link lifetime (1 month).
	cdnDefaultExpiry = 30 * 24 * time.Hour
	// cdnMaxExpiry is the hard lifetime cap (1 year). Uploads asking for
	// more are rejected, and the configuration cannot raise it.
	cdnMaxExpiry = 365 * 24 * time.Hour
	// cdnGCInterval is how often the periodic expiry cleanup runs.
	cdnGCInterval = time.Hour
	// cdnGCInitialDelay delays the first cleanup after startup.
	cdnGCInitialDelay = time.Minute
	// cdnStaleTempAge is how old an interrupted temporary upload must be
	// before the cleanup removes it.
	cdnStaleTempAge = 24 * time.Hour
	// cdnObjectDirName is the storage subdirectory inside data_dir.
	cdnObjectDirName = "objects"
	// cdnMetaSuffix names the JSON sidecar holding one file's metadata.
	cdnMetaSuffix = ".meta.json"

	// cdnWrapperCSP locks the wrapper page down to inline script + blob
	// rendering, and sandboxes it so a payload can never navigate or reach
	// the network beyond the ciphertext fetch.
	cdnWrapperCSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
		"connect-src 'self'; img-src blob:; media-src blob:; object-src blob:; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'; sandbox allow-scripts"
)

// cdnDuration is a YAML-friendly duration string ("12h", "30d") for the file
// service configuration.
type cdnDuration time.Duration

// Duration converts the configured value into a time.Duration.
func (d cdnDuration) Duration() time.Duration {
	return time.Duration(d)
}

// UnmarshalYAML parses a scalar duration such as "30d" or "12h".
func (d *cdnDuration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("cdn durations must be strings like \"30d\" or \"12h\"")
	}
	value := strings.ToLower(strings.TrimSpace(node.Value))
	if value == "" || value == "0" {
		*d = 0
		return nil
	}
	parsed, err := parseLookbackDuration(value)
	if err != nil || parsed < 0 {
		return fmt.Errorf("invalid cdn duration %q (use e.g. 12h or 30d)", node.Value)
	}
	*d = cdnDuration(parsed)
	return nil
}

// MarshalYAML keeps the config round-trippable when mxmain re-writes it.
func (d cdnDuration) MarshalYAML() (any, error) {
	if d <= 0 {
		return nil, nil
	}
	return formatTTLDuration(time.Duration(d)), nil
}

// CdnConfig configures the bridge-hosted, zero-knowledge file service. It is
// fully gated on configuration: without `enabled: true` and a domain the
// service never starts. See docs/cdn-file-service.md for the user-facing
// schema.
type CdnConfig struct {
	// Enabled turns the file service on. Default false.
	Enabled bool `yaml:"enabled"`
	// Domain is the public hostname the reverse proxy serves the service on,
	// e.g. "cdn.example.com". Required when enabled.
	Domain string `yaml:"domain,omitempty"`
	// DataDir is the directory ciphertext and metadata are stored in. The
	// playbook points this at the bridge data volume (e.g. "/data/cdn").
	DataDir string `yaml:"data_dir,omitempty"`
	// MaxFileBytes caps one encrypted upload, in bytes.
	MaxFileBytes int64 `yaml:"max_file_bytes,omitempty"`
	// PerUserCapBytes caps the stored ciphertext per user. Older files are
	// pruned first (FIFO) once the cap is exceeded.
	PerUserCapBytes int64 `yaml:"per_user_cap_bytes,omitempty"`
	// DefaultExpiry is the link lifetime used when an upload does not ask
	// for a specific one.
	DefaultExpiry cdnDuration `yaml:"default_expiry,omitempty"`
	// MaxExpiry is the longest lifetime an upload may request. Hard-capped
	// at one year.
	MaxExpiry cdnDuration `yaml:"max_expiry,omitempty"`
	// GCInterval is how often the expiry cleanup runs.
	GCInterval cdnDuration `yaml:"gc_interval,omitempty"`
}

// DefaultCdnConfig returns the file service defaults: disabled, 100 MiB per
// file, 1 GiB per user, one month links, cleanup hourly.
func DefaultCdnConfig() CdnConfig {
	return CdnConfig{
		MaxFileBytes:    cdnDefaultMaxFileBytes,
		PerUserCapBytes: cdnDefaultPerUserCapBytes,
		DefaultExpiry:   cdnDuration(cdnDefaultExpiry),
		MaxExpiry:       cdnDuration(cdnMaxExpiry),
		GCInterval:      cdnDuration(cdnGCInterval),
	}
}

// UnmarshalYAML applies per-field defaults so a partial `cdn` mapping never
// wipes the size or expiry defaults.
func (c *CdnConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Enabled         *bool        `yaml:"enabled"`
		Domain          *string      `yaml:"domain"`
		DataDir         *string      `yaml:"data_dir"`
		MaxFileBytes    *int64       `yaml:"max_file_bytes"`
		PerUserCapBytes *int64       `yaml:"per_user_cap_bytes"`
		DefaultExpiry   *cdnDuration `yaml:"default_expiry"`
		MaxExpiry       *cdnDuration `yaml:"max_expiry"`
		GCInterval      *cdnDuration `yaml:"gc_interval"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	cfg := DefaultCdnConfig()
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if raw.Domain != nil {
		cfg.Domain = strings.TrimSpace(*raw.Domain)
	}
	if raw.DataDir != nil {
		cfg.DataDir = strings.TrimSpace(*raw.DataDir)
	}
	if raw.MaxFileBytes != nil {
		cfg.MaxFileBytes = *raw.MaxFileBytes
	}
	if raw.PerUserCapBytes != nil {
		cfg.PerUserCapBytes = *raw.PerUserCapBytes
	}
	if raw.DefaultExpiry != nil {
		cfg.DefaultExpiry = *raw.DefaultExpiry
	}
	if raw.MaxExpiry != nil {
		cfg.MaxExpiry = *raw.MaxExpiry
	}
	if raw.GCInterval != nil {
		cfg.GCInterval = *raw.GCInterval
	}
	*c = cfg
	return nil
}

// Active reports whether the service should run: an explicitly enabled
// feature with a public domain. No domain (or no config at all) means no
// service.
func (c CdnConfig) Active() bool {
	return c.Enabled && strings.TrimSpace(c.Domain) != ""
}

// Validate rejects configurations that would start a broken service. A
// disabled service is always valid: the feature is fully configuration-gated.
func (c CdnConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	domain := strings.TrimSpace(c.Domain)
	if domain == "" {
		return fmt.Errorf("cdn.domain is required when the file service is enabled")
	}
	if strings.ContainsAny(domain, " \t/") || strings.Contains(domain, "://") {
		return fmt.Errorf("cdn.domain must be a bare hostname like cdn.example.com, got %q", c.Domain)
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("cdn.data_dir is required when the file service is enabled")
	}
	if c.MaxFileBytes <= 0 {
		return fmt.Errorf("cdn.max_file_bytes must be positive")
	}
	if c.PerUserCapBytes <= 0 {
		return fmt.Errorf("cdn.per_user_cap_bytes must be positive")
	}
	defaultExpiry := c.DefaultExpiry.Duration()
	maxExpiry := c.MaxExpiry.Duration()
	if defaultExpiry <= 0 {
		return fmt.Errorf("cdn.default_expiry must be positive")
	}
	if maxExpiry <= 0 {
		return fmt.Errorf("cdn.max_expiry must be positive")
	}
	if maxExpiry > cdnMaxExpiry {
		return fmt.Errorf("cdn.max_expiry cannot exceed 365d")
	}
	if defaultExpiry > maxExpiry {
		return fmt.Errorf("cdn.default_expiry cannot exceed cdn.max_expiry")
	}
	if c.GCInterval.Duration() < 0 {
		return fmt.Errorf("cdn.gc_interval cannot be negative")
	}
	return nil
}

// CdnAuthFunc resolves the owner key of an authenticated file-service
// request (upload, list, revoke). Returning ok=false rejects the request. The
// wiring maps the app's paired-device bearer token to the Matrix user ID, so
// quotas and revocation are per Matrix user.
type CdnAuthFunc func(r *http.Request) (owner string, ok bool)

// CdnEntry is the stored metadata of one encrypted file.
type CdnEntry struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CdnStats summarizes the stored files for the management room.
type CdnStats struct {
	Files   int       `json:"files"`
	Bytes   int64     `json:"bytes"`
	Owners  int       `json:"owners"`
	Expired int       `json:"expired"`
	LastGC  time.Time `json:"last_gc,omitempty"`
	GCError string    `json:"gc_error,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// CdnService is the bridge-side file service: unguessable ids, ciphertext
// storage, per-user quotas with FIFO pruning, and expiry cleanup. The
// matching decryptor page is served for every file (the key stays in the URL
// fragment and never reaches the service).
type CdnService struct {
	log  zerolog.Logger
	cfg  CdnConfig
	auth CdnAuthFunc

	mu        sync.Mutex
	gcCancel  context.CancelFunc
	lastGC    time.Time
	lastGCErr string
}

// NewCdnService validates the configuration and prepares the storage
// directory. It returns (nil, nil) when the feature is inactive, so callers
// can treat a nil service as "no file service configured".
func NewCdnService(log zerolog.Logger, cfg CdnConfig, auth CdnAuthFunc) (*CdnService, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Active() {
		return nil, nil
	}
	service := &CdnService{
		log:  log.With().Str("component", "cdn-file-service").Logger(),
		cfg:  cfg,
		auth: auth,
	}
	if err := os.MkdirAll(service.objectDir(), 0o700); err != nil {
		return nil, fmt.Errorf("create cdn data dir: %w", err)
	}
	return service, nil
}

// FileURL returns the public wrapper URL of a file. The decryption key is
// appended by the uploader as the URL fragment and never reaches the bridge.
func (s *CdnService) FileURL(id string) string {
	return s.BaseURL() + cdnPathPrefix + "/" + id
}

// BaseURL returns the public base URL of the service ("https://<domain>").
// The app appends "/f/<id>#<key>" to build a shareable link.
func (s *CdnService) BaseURL() string {
	return "https://" + strings.TrimSpace(s.cfg.Domain)
}

// objectDir is where ciphertext files and their metadata live.
func (s *CdnService) objectDir() string {
	return filepath.Join(s.cfg.DataDir, cdnObjectDirName)
}

func (s *CdnService) objectPath(id string) string {
	return filepath.Join(s.objectDir(), id)
}

func (s *CdnService) metaPath(id string) string {
	return filepath.Join(s.objectDir(), id+cdnMetaSuffix)
}

// validCdnID reports whether a path id can be one the service generated. It
// runs before any filesystem lookup so a hostile path can never be echoed or
// resolved.
func validCdnID(id string) bool {
	if len(id) < cdnMinIDLen || len(id) > cdnMaxIDLen {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// newCdnID generates an unguessable file id: 32 random bytes as unpadded
// base64url (43 characters).
func newCdnID() (string, error) {
	buf := make([]byte, cdnIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate cdn id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Register adds the file service routes to an existing mux. It is used to
// mount the service on the bridge's pairing listener; each call must target a
// fresh mux.
func (s *CdnService) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+cdnPathPrefix, s.handleUpload)
	mux.HandleFunc("GET "+cdnPathPrefix, s.handleList)
	mux.HandleFunc("DELETE "+cdnPathPrefix+"/{id}", s.handleRevoke)
	mux.HandleFunc("GET "+cdnPathPrefix+"/{id}", s.handleWrapper)
	mux.HandleFunc("GET "+cdnPathPrefix+"/{id}/raw", s.handleDownload)
}

// Handler returns the file service HTTP handler (auth-required upload,
// list and revoke routes plus the public wrapper and ciphertext routes).
func (s *CdnService) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

// authenticate resolves the owner key of a request, rejecting empty owners.
func (s *CdnService) authenticate(r *http.Request) (string, bool) {
	if s == nil || s.auth == nil {
		return "", false
	}
	owner, ok := s.auth(r)
	owner = strings.TrimSpace(owner)
	return owner, ok && owner != ""
}

// resolveUploadExpiry applies the per-upload `expires` override, defaulting
// to the configured lifetime and rejecting anything above the maximum.
func (s *CdnService) resolveUploadExpiry(r *http.Request) (time.Duration, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("expires"))
	if raw == "" {
		return s.cfg.DefaultExpiry.Duration(), nil
	}
	parsed, err := parseLookbackDuration(strings.ToLower(raw))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid expires value %q (use e.g. 7d or 12h)", raw)
	}
	if max := s.cfg.MaxExpiry.Duration(); max > 0 && parsed > max {
		return 0, fmt.Errorf("expires %q is above the %s maximum", raw, formatTTLDuration(max))
	}
	return parsed, nil
}

// cdnUploadResponse is the POST /f response: the id the uploader embeds in
// the link. The key never appears here.
type cdnUploadResponse struct {
	ID          string `json:"id"`
	Size        int64  `json:"size"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
}

// handleUpload stores one ciphertext body. The body is read before the
// quota lock is taken, so a slow upload cannot block cleanup or other users.
func (s *CdnService) handleUpload(w http.ResponseWriter, r *http.Request) {
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

	maxBytes := s.cfg.MaxFileBytes
	id, err := newCdnID()
	if err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to generate a file id")
		return
	}
	temp, err := os.CreateTemp(s.objectDir(), ".tmp-upload-*")
	if err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to open a temporary file")
		return
	}
	tempName := temp.Name()
	written, copyErr := io.Copy(temp, io.LimitReader(r.Body, maxBytes+1))
	closeErr := temp.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tempName)
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to store the upload")
		return
	}
	if written == 0 {
		_ = os.Remove(tempName)
		writeCdnError(w, http.StatusBadRequest, "bad_request", "the request body is empty")
		return
	}
	if written > maxBytes {
		_ = os.Remove(tempName)
		writeCdnError(w, http.StatusRequestEntityTooLarge, "file_too_large",
			fmt.Sprintf("the encrypted upload exceeds the %d byte limit", maxBytes))
		return
	}
	if written > s.cfg.PerUserCapBytes {
		_ = os.Remove(tempName)
		writeCdnError(w, http.StatusRequestEntityTooLarge, "quota_exceeded",
			fmt.Sprintf("the encrypted upload exceeds your %d byte storage quota", s.cfg.PerUserCapBytes))
		return
	}

	now := time.Now().UTC()
	entry := CdnEntry{
		ID:        id,
		Owner:     owner,
		Size:      written,
		CreatedAt: now,
		ExpiresAt: now.Add(expiry),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(tempName, s.objectPath(id)); err != nil {
		_ = os.Remove(tempName)
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to store the upload")
		return
	}
	if err := s.writeMetaLocked(entry); err != nil {
		_ = os.Remove(s.objectPath(id))
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to store the upload metadata")
		return
	}
	pruned, err := s.pruneOwnerLocked(owner, id)
	if err != nil {
		s.log.Warn().Err(err).Str("owner", owner).Msg("Failed to prune CDN files over the per-user quota")
	}
	if pruned > 0 {
		s.log.Info().Str("owner", owner).Int("pruned", pruned).Msg("Pruned oldest CDN files over the per-user quota")
	}
	writeCdnJSON(w, http.StatusCreated, cdnUploadResponse{
		ID:          id,
		Size:        written,
		ExpiresAtMS: entry.ExpiresAt.UnixMilli(),
	})
}

// handleDownload serves the stored ciphertext. The URL is the secret: no
// headers or cookies are required.
func (s *CdnService) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entry, err := s.loadEntry(id)
	if err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to read the file metadata")
		return
	}
	if entry == nil {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(s.objectPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to open the file")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to open the file")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id+".bin"))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	// The wrapper page is sandboxed, so its origin is opaque and fetching the
	// ciphertext is a cross-origin request. The URL is the secret and the
	// bytes are useless without the fragment key, so a wildcard CORS header
	// is safe and keeps the sandbox strict.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

// handleWrapper serves the static decryptor page. The page fetches the
// ciphertext and decrypts it in the browser with the fragment key; the
// server never sees that key.
func (s *CdnService) handleWrapper(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entry, err := s.loadEntry(id)
	if err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to read the file metadata")
		return
	}
	if entry == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", cdnWrapperCSP)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := cdnWrapperTemplate.Execute(w, cdnWrapperData{ID: id}); err != nil {
		s.log.Warn().Err(err).Msg("Failed to render the CDN wrapper page")
	}
}

// cdnListResponse is the GET /f response body.
type cdnListResponse struct {
	Files     []CdnEntry `json:"files"`
	CapBytes  int64      `json:"cap_bytes"`
	UsedBytes int64      `json:"used_bytes"`
}

// handleList returns the caller's active files.
func (s *CdnService) handleList(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.authenticate(r)
	if !ok {
		writeCdnError(w, http.StatusUnauthorized, "unauthorized", "a valid device bearer token is required")
		return
	}
	entries, err := s.List(owner)
	if err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to list your files")
		return
	}
	var used int64
	for _, entry := range entries {
		used += entry.Size
	}
	if entries == nil {
		entries = []CdnEntry{}
	}
	writeCdnJSON(w, http.StatusOK, cdnListResponse{Files: entries, CapBytes: s.cfg.PerUserCapBytes, UsedBytes: used})
}

// handleRevoke deletes one of the caller's files. Foreign ids look exactly
// like unknown ones, so the API cannot be used to probe other users' files.
func (s *CdnService) handleRevoke(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.authenticate(r)
	if !ok {
		writeCdnError(w, http.StatusUnauthorized, "unauthorized", "a valid device bearer token is required")
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, err := s.loadEntry(id)
	if err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to read the file metadata")
		return
	}
	if entry == nil || entry.Owner != owner {
		http.NotFound(w, r)
		return
	}
	if err := s.removeEntryLocked(id); err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to delete the file")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// List returns the owner's active files, newest first.
func (s *CdnService) List(owner string) ([]CdnEntry, error) {
	if s == nil {
		return nil, nil
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.listAllLocked()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	entries := make([]CdnEntry, 0, len(all))
	for _, entry := range all {
		if entry.Owner != owner || !entry.ExpiresAt.After(now) {
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.After(entries[j].CreatedAt)
		}
		return entries[i].ID > entries[j].ID
	})
	return entries, nil
}

// Revoke removes a file owned by owner. It reports ok=false for unknown ids
// and for files owned by someone else.
func (s *CdnService) Revoke(owner, id string) (bool, error) {
	if s == nil {
		return false, nil
	}
	owner = strings.TrimSpace(owner)
	id = strings.TrimSpace(id)
	if owner == "" || !validCdnID(id) {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, err := s.loadEntry(id)
	if err != nil {
		return false, err
	}
	if entry == nil || entry.Owner != owner {
		return false, nil
	}
	return true, s.removeEntryLocked(id)
}

// Stats summarizes the service for the management room. Expired files are
// counted separately until the cleanup removes them.
func (s *CdnService) Stats() CdnStats {
	if s == nil {
		return CdnStats{}
	}
	s.mu.Lock()
	all, err := s.listAllLocked()
	stats := CdnStats{LastGC: s.lastGC, GCError: s.lastGCErr}
	s.mu.Unlock()
	if err != nil {
		stats.Error = err.Error()
		return stats
	}
	now := time.Now()
	owners := make(map[string]struct{})
	for _, entry := range all {
		if !entry.ExpiresAt.After(now) {
			stats.Expired++
			continue
		}
		stats.Files++
		stats.Bytes += entry.Size
		owners[entry.Owner] = struct{}{}
	}
	stats.Owners = len(owners)
	return stats
}

// RunGC removes expired files, orphaned metadata/objects left by
// interruptions, and old temporary uploads. The periodic loop calls it too.
func (s *CdnService) RunGC(now time.Time) (int, error) {
	if s == nil {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed, err := s.runGCLocked(now)
	s.lastGC = now.UTC()
	if err != nil {
		s.lastGCErr = err.Error()
		return removed, err
	}
	s.lastGCErr = ""
	if removed > 0 {
		s.log.Info().Int("removed", removed).Msg("CDN expiry cleanup removed files")
	}
	return removed, nil
}

// StartGC starts the periodic expiry cleanup. It is a no-op when one is
// already running.
func (s *CdnService) StartGC() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.gcCancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.gcCancel = cancel
	interval := s.cfg.GCInterval.Duration()
	if interval <= 0 {
		interval = cdnGCInterval
	}
	s.mu.Unlock()
	s.log.Info().Dur("interval", interval).Msg("Starting CDN expiry cleanup loop")
	go func() {
		timer := time.NewTimer(cdnGCInitialDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				_, _ = s.RunGC(time.Now())
				timer.Reset(interval)
			}
		}
	}()
}

// StopGC cancels the periodic expiry cleanup.
func (s *CdnService) StopGC() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.gcCancel
	s.gcCancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// loadEntry returns one active entry. Missing, malformed and expired entries
// all return nil, which callers surface as 404.
func (s *CdnService) loadEntry(id string) (*CdnEntry, error) {
	entry, err := s.readMeta(id)
	if err != nil || entry == nil {
		return entry, err
	}
	if !entry.ExpiresAt.After(time.Now()) {
		return nil, nil
	}
	return entry, nil
}

// readMeta reads one metadata sidecar without expiry filtering.
func (s *CdnService) readMeta(id string) (*CdnEntry, error) {
	if !validCdnID(id) {
		return nil, nil
	}
	data, err := os.ReadFile(s.metaPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var entry CdnEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, nil
	}
	if entry.ID != id {
		return nil, nil
	}
	return &entry, nil
}

// listAllLocked reads every parseable metadata sidecar (expired ones
// included). Callers must hold s.mu.
func (s *CdnService) listAllLocked() ([]CdnEntry, error) {
	dirEntries, err := os.ReadDir(s.objectDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	entries := make([]CdnEntry, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		name := dirEntry.Name()
		if dirEntry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, cdnMetaSuffix) {
			continue
		}
		id := strings.TrimSuffix(name, cdnMetaSuffix)
		entry, err := s.readMeta(id)
		if err != nil {
			return nil, err
		}
		if entry != nil {
			entries = append(entries, *entry)
		}
	}
	return entries, nil
}

// writeMetaLocked writes one metadata sidecar atomically (temp file +
// rename). Callers must hold s.mu.
func (s *CdnService) writeMetaLocked(entry CdnEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.objectDir(), ".tmp-meta-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, s.metaPath(entry.ID)); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// removeEntryLocked deletes a file's metadata and ciphertext. Callers must
// hold s.mu.
func (s *CdnService) removeEntryLocked(id string) error {
	if !validCdnID(id) {
		return nil
	}
	if err := os.Remove(s.metaPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(s.objectPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// pruneOwnerLocked removes the owner's oldest files (never keepID) until the
// stored bytes fit within the per-user cap. Callers must hold s.mu.
func (s *CdnService) pruneOwnerLocked(owner, keepID string) (int, error) {
	all, err := s.listAllLocked()
	if err != nil {
		return 0, err
	}
	owned := make([]CdnEntry, 0, len(all))
	var total int64
	for _, entry := range all {
		if entry.Owner != owner {
			continue
		}
		owned = append(owned, entry)
		total += entry.Size
	}
	capBytes := s.cfg.PerUserCapBytes
	if total <= capBytes {
		return 0, nil
	}
	sort.Slice(owned, func(i, j int) bool {
		if !owned[i].CreatedAt.Equal(owned[j].CreatedAt) {
			return owned[i].CreatedAt.Before(owned[j].CreatedAt)
		}
		return owned[i].ID < owned[j].ID
	})
	removed := 0
	for _, entry := range owned {
		if total <= capBytes {
			break
		}
		if entry.ID == keepID {
			continue
		}
		if err := s.removeEntryLocked(entry.ID); err != nil {
			return removed, err
		}
		total -= entry.Size
		removed++
	}
	return removed, nil
}

// runGCLocked removes expired entries, orphaned files and stale temporary
// uploads. Callers must hold s.mu.
func (s *CdnService) runGCLocked(now time.Time) (int, error) {
	dirEntries, err := os.ReadDir(s.objectDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, dirEntry := range dirEntries {
		name := dirEntry.Name()
		if strings.HasPrefix(name, ".") {
			if info, statErr := dirEntry.Info(); statErr == nil && now.Sub(info.ModTime()) > cdnStaleTempAge {
				_ = os.Remove(filepath.Join(s.objectDir(), name))
			}
			continue
		}
		if dirEntry.IsDir() {
			continue
		}
		if strings.HasSuffix(name, cdnMetaSuffix) {
			id := strings.TrimSuffix(name, cdnMetaSuffix)
			entry, err := s.readMeta(id)
			if err != nil {
				return removed, err
			}
			orphan := entry == nil
			expired := entry != nil && !entry.ExpiresAt.After(now)
			missingObject := false
			if _, statErr := os.Stat(s.objectPath(id)); statErr != nil {
				if os.IsNotExist(statErr) {
					missingObject = true
				} else {
					return removed, statErr
				}
			}
			if orphan || expired || missingObject {
				if err := s.removeEntryLocked(id); err != nil {
					return removed, err
				}
				removed++
			}
			continue
		}
		// A ciphertext file without metadata: the upload was interrupted
		// between the rename and the metadata write.
		if !validCdnID(name) {
			continue
		}
		meta, err := s.readMeta(name)
		if err != nil {
			return removed, err
		}
		if meta == nil {
			if err := os.Remove(s.objectPath(name)); err != nil && !os.IsNotExist(err) {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// cdnWrapperData feeds the decryptor page. ID is escaped for the JS context
// by html/template; only ids that passed validCdnID are ever rendered.
type cdnWrapperData struct {
	ID string
}

// cdnWrapperTemplate is the only frontend asset of the service: a minimal
// page that decrypts the fetched ciphertext with the fragment key and
// renders it from a blob URL. Only sniffed image/audio/video/pdf payloads
// are rendered; HTML, SVG and JS are never executed.
var cdnWrapperTemplate = template.Must(template.New("cdn-wrapper").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="referrer" content="no-referrer">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Encrypted file</title>
<style>
body { margin: 0; background: #111; color: #eee; font-family: system-ui, sans-serif; display: flex; flex-direction: column; align-items: center; justify-content: center; min-height: 100vh; }
#status { padding: 1rem 1.5rem; text-align: center; }
img, video, embed { max-width: 100%; max-height: 90vh; }
audio { width: 90vw; max-width: 640px; }
embed { width: 90vw; height: 85vh; border: 0; background: #222; }
</style>
</head>
<body>
<p id="status">Decrypting…</p>
<script>
"use strict";
const fileID = {{.ID}};
const rawURL = "/f/" + encodeURIComponent(fileID) + "/raw";
const status = document.getElementById("status");

function fail(message) { status.textContent = message; }

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

async function main() {
	const keyText = location.hash.slice(1);
	if (!keyText) { fail("Missing decryption key: the part of the link after # is required."); return; }
	let keyBytes;
	try { keyBytes = decodeKey(keyText); } catch { fail("The decryption key in the link is invalid."); return; }
	if (keyBytes.length !== 32) { fail("The link key is not a 32-byte AES-256 key."); return; }

	let ciphertext;
	try {
		const response = await fetch(rawURL, { cache: "no-store" });
		if (!response.ok) { fail("File not found or expired (HTTP " + response.status + ")."); return; }
		ciphertext = new Uint8Array(await response.arrayBuffer());
	} catch { fail("Failed to download the encrypted file."); return; }
	if (ciphertext.length <= 12) { fail("The encrypted file is too short to be valid."); return; }

	let plaintext;
	try {
		const key = await crypto.subtle.importKey("raw", keyBytes, "AES-GCM", false, ["decrypt"]);
		plaintext = new Uint8Array(await crypto.subtle.decrypt(
			{ name: "AES-GCM", iv: ciphertext.slice(0, 12) }, key, ciphertext.slice(12)));
	} catch { fail("Decryption failed: wrong key or corrupted file."); return; }

	const type = sniffType(plaintext);
	let element;
	if (type.startsWith("image/")) { element = document.createElement("img"); element.alt = "Decrypted image"; }
	else if (type.startsWith("audio/")) { element = document.createElement("audio"); element.controls = true; }
	else if (type.startsWith("video/")) { element = document.createElement("video"); element.controls = true; element.playsInline = true; }
	else if (type === "application/pdf") { element = document.createElement("embed"); element.type = "application/pdf"; }
	if (!element) { fail("This file type cannot be displayed here. Ask the sender for the original file."); return; }
	element.src = URL.createObjectURL(new Blob([plaintext], { type: type }));
	status.remove();
	document.body.appendChild(element);
}

main();
</script>
</body>
</html>
`))

// NewCdnCommand builds the management-room `cdn` command for one service
// instance. The wiring patch registers it with
// proc.AddHandler(NewCdnCommand(cdnService)).
func NewCdnCommand(service *CdnService) *commands.FullHandler {
	return &commands.FullHandler{
		Func: func(ce *commands.Event) {
			if service == nil {
				ce.Reply("The bridge-hosted file service is disabled (no `cdn.domain` configured).")
				return
			}
			if ce.User == nil {
				ce.Reply("This command needs a Matrix user context.")
				return
			}
			owner := string(ce.User.MXID)
			action := "list"
			if len(ce.Args) > 0 {
				action = strings.ToLower(strings.TrimSpace(ce.Args[0]))
			}
			switch action {
			case "list":
				entries, err := service.List(owner)
				if err != nil {
					ce.Reply("Failed to list your files: %v", err)
					return
				}
				if len(entries) == 0 {
					ce.Reply("No active file links uploaded by you.")
					return
				}
				lines := make([]string, 0, len(entries)+1)
				for _, entry := range entries {
					lines = append(lines, fmt.Sprintf("- `%s` — %s, expires %s",
						entry.ID, formatCdnBytes(entry.Size), formatCdnTime(entry.ExpiresAt)))
				}
				ce.Reply("Your file links (%d):\n%s\n\nRevoke with `$cmdprefix cdn revoke <id>`.",
					len(entries), strings.Join(lines, "\n"))
			case "revoke":
				if len(ce.Args) < 2 {
					ce.Reply("Usage: $cmdprefix cdn revoke <id>")
					return
				}
				id := strings.TrimSpace(ce.Args[1])
				ok, err := service.Revoke(owner, id)
				switch {
				case err != nil:
					ce.Reply("Failed to revoke `%s`: %v", id, err)
				case !ok:
					ce.Reply("No active file with id `%s` is owned by you.", id)
				default:
					ce.Reply("Revoked `%s`. The link is dead now.", id)
				}
			case "stats":
				if ce.User.Permissions.Admin {
					stats := service.Stats()
					lastGC := "never"
					if !stats.LastGC.IsZero() {
						lastGC = formatCdnTime(stats.LastGC)
					}
					lines := []string{fmt.Sprintf(
						"File service: %d active file(s), %s across %d user(s), %d expired awaiting cleanup. Last cleanup: %s.",
						stats.Files, formatCdnBytes(stats.Bytes), stats.Owners, stats.Expired, lastGC)}
					if stats.GCError != "" {
						lines = append(lines, "Last cleanup error: "+stats.GCError)
					}
					if stats.Error != "" {
						lines = append(lines, "Storage error: "+stats.Error)
					}
					ce.Reply("%s", strings.Join(lines, "\n"))
					return
				}
				entries, err := service.List(owner)
				if err != nil {
					ce.Reply("Failed to read your file service usage: %v", err)
					return
				}
				var used int64
				for _, entry := range entries {
					used += entry.Size
				}
				ce.Reply("Your file links: %d active, %s used of %s.",
					len(entries), formatCdnBytes(used), formatCdnBytes(service.cfg.PerUserCapBytes))
			case "gc":
				if !ce.User.Permissions.Admin {
					ce.Reply("Only bridge administrators can run the file service cleanup.")
					return
				}
				removed, err := service.RunGC(time.Now())
				if err != nil {
					ce.Reply("Cleanup failed after removing %d file(s): %v", removed, err)
					return
				}
				ce.Reply("Cleanup done: removed %d expired or orphaned file(s).", removed)
			default:
				ce.Reply("%s", cdnCommandHelpText())
			}
		},
		Name:    "cdn",
		Aliases: []string{"files"},
		Help: commands.HelpMeta{
			Section:     commands.HelpSectionAdmin,
			Description: "List, revoke, and inspect bridge-hosted encrypted file links",
			Args:        "list | revoke <id> | stats | gc",
		},
	}
}

// cdnCommandHelpText is the `cdn` command usage shown in the management room.
func cdnCommandHelpText() string {
	return "Usage: $cmdprefix cdn <list|revoke <id>|stats|gc>\n" +
		"- list: show your active file links (id, size, expiry)\n" +
		"- revoke <id>: delete one of your file links\n" +
		"- stats: show storage usage (global for admins, yours otherwise)\n" +
		"- gc: run expiry cleanup now (admins only)"
}

// formatCdnBytes renders a byte count for the management room.
func formatCdnBytes(size int64) string {
	switch {
	case size >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(size)/float64(int64(1)<<30))
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(size)/float64(int64(1)<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(size)/float64(int64(1)<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

// formatCdnTime renders a timestamp for the management room.
func formatCdnTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04 MST")
}

// writeCdnJSON writes a JSON response body.
func writeCdnJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeCdnError writes the file service error envelope.
func writeCdnError(w http.ResponseWriter, status int, code, message string) {
	writeCdnJSON(w, status, map[string]string{"error": code, "message": message})
}
