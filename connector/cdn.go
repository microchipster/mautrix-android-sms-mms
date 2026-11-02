// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
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
//
// Albums (connector/cdn_album.go) extend the same service: one unguessable
// id holds several encrypted items plus a client-encrypted manifest, and the
// link's single fragment key derives per-item subkeys with HKDF.

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

	// cdnDefaultMaxFileBytes caps one ciphertext upload (512 MiB).
	cdnDefaultMaxFileBytes = int64(512) << 20
	// cdnDefaultPerUserCapBytes caps the storage per user (10 GiB). Files
	// beyond the cap are removed oldest-first.
	cdnDefaultPerUserCapBytes = int64(10) << 30
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

	// cdnKindAlbum marks a stored entry as a multi-item album. File entries
	// carry an empty kind, so metadata written before albums stays valid.
	cdnKindAlbum = "album"
	// cdnDefaultMaxAlbumItems caps one album at 20 items.
	cdnDefaultMaxAlbumItems = 20
	// cdnHardMaxAlbumItems is the wire-format limit of the album container:
	// the item count is a single byte, so no configuration can raise it.
	cdnHardMaxAlbumItems = 255
	// cdnDefaultMaxAlbumBytes caps one album's total ciphertext (500 MiB).
	cdnDefaultMaxAlbumBytes = int64(500) << 20
	// cdnMaxManifestBytes caps the client-encrypted manifest object (64 KiB).
	// The manifest is small metadata (item names/types/sizes), not a second
	// item; it is not configurable.
	cdnMaxManifestBytes = int64(64) << 10

	// cdnObjectOverheadBytes is the AES-GCM framing every stored object
	// carries: a 12-byte nonce plus a 16-byte authentication tag. A
	// plaintext fits an upload cap only while it is at most the cap minus
	// this overhead.
	cdnObjectOverheadBytes = 12 + 16

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
	// MaxAlbumItems caps how many items one album may contain. The wire
	// format allows at most 255.
	MaxAlbumItems int `yaml:"max_album_items,omitempty"`
	// MaxAlbumBytes caps the total ciphertext of one album, in bytes.
	// Albums also count against PerUserCapBytes.
	MaxAlbumBytes int64 `yaml:"max_album_bytes,omitempty"`
	// PerUserCapBytes caps the stored ciphertext per user (files and
	// albums). Older entries are pruned first (FIFO) once the cap is
	// exceeded.
	PerUserCapBytes int64 `yaml:"per_user_cap_bytes,omitempty"`
	// DefaultExpiry is the link lifetime used when an upload does not ask
	// for a specific one.
	DefaultExpiry cdnDuration `yaml:"default_expiry,omitempty"`
	// MaxExpiry is the longest lifetime an upload may request. Hard-capped
	// at one year.
	MaxExpiry cdnDuration `yaml:"max_expiry,omitempty"`
	// GCInterval is how often the expiry cleanup runs.
	GCInterval cdnDuration `yaml:"gc_interval,omitempty"`
	// SendMode controls what the bridge does with the media of its own
	// outgoing messages: "link" uploads an attachment to the service and
	// sends the link as text (restricted to attachments above
	// SendThresholdBytes when that is positive), "never" always sends the
	// attachment through the phone. Empty means "link" while the service is
	// active.
	SendMode string `yaml:"send_mode,omitempty"`
	// SendThresholdBytes is the plaintext size above which an outgoing
	// attachment is sent as a link, in bytes. Zero (the default) links
	// every attachment; set a positive value to keep smaller media on the
	// carrier.
	SendThresholdBytes int64 `yaml:"send_threshold,omitempty"`
	// SendExpiry is the lifetime of links the bridge creates for its own
	// outgoing attachments. Zero means DefaultExpiry.
	SendExpiry cdnDuration `yaml:"send_expiry,omitempty"`
	// UploadToken is an optional static bearer token for trusted clients that
	// cannot hold a paired-device token (for example the jmpchat bridge).
	// When set, UploadTokenOwner must be set too.
	UploadToken string `yaml:"upload_token,omitempty"`
	// UploadTokenOwner is the Matrix user id that owns uploads made with
	// UploadToken.
	UploadTokenOwner string `yaml:"upload_token_owner,omitempty"`
}

// DefaultCdnConfig returns the file service defaults: disabled, 512 MiB per
// file, 20 items and 500 MiB per album, 10 GiB per user, one month links,
// cleanup hourly, and outgoing attachments linked in link mode (no size
// threshold, so every attachment takes the link path).
func DefaultCdnConfig() CdnConfig {
	return CdnConfig{
		MaxFileBytes:    cdnDefaultMaxFileBytes,
		MaxAlbumItems:   cdnDefaultMaxAlbumItems,
		MaxAlbumBytes:   cdnDefaultMaxAlbumBytes,
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
		Enabled          *bool        `yaml:"enabled"`
		Domain           *string      `yaml:"domain"`
		DataDir          *string      `yaml:"data_dir"`
		MaxFileBytes     *int64       `yaml:"max_file_bytes"`
		MaxAlbumItems    *int         `yaml:"max_album_items"`
		MaxAlbumBytes    *int64       `yaml:"max_album_bytes"`
		PerUserCapBytes  *int64       `yaml:"per_user_cap_bytes"`
		DefaultExpiry    *cdnDuration `yaml:"default_expiry"`
		MaxExpiry        *cdnDuration `yaml:"max_expiry"`
		GCInterval       *cdnDuration `yaml:"gc_interval"`
		SendMode         *string      `yaml:"send_mode"`
		SendThreshold    *int64       `yaml:"send_threshold"`
		SendExpiry       *cdnDuration `yaml:"send_expiry"`
		UploadToken      *string      `yaml:"upload_token"`
		UploadTokenOwner *string      `yaml:"upload_token_owner"`
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
	if raw.MaxAlbumItems != nil {
		cfg.MaxAlbumItems = *raw.MaxAlbumItems
	}
	if raw.MaxAlbumBytes != nil {
		cfg.MaxAlbumBytes = *raw.MaxAlbumBytes
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
	if raw.SendMode != nil {
		cfg.SendMode = strings.TrimSpace(*raw.SendMode)
	}
	if raw.SendThreshold != nil {
		cfg.SendThresholdBytes = *raw.SendThreshold
	}
	if raw.SendExpiry != nil {
		cfg.SendExpiry = *raw.SendExpiry
	}
	if raw.UploadToken != nil {
		cfg.UploadToken = strings.TrimSpace(*raw.UploadToken)
	}
	if raw.UploadTokenOwner != nil {
		cfg.UploadTokenOwner = strings.TrimSpace(*raw.UploadTokenOwner)
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
	if c.MaxAlbumItems <= 0 {
		return fmt.Errorf("cdn.max_album_items must be positive")
	}
	if c.MaxAlbumItems > cdnHardMaxAlbumItems {
		return fmt.Errorf("cdn.max_album_items cannot exceed %d", cdnHardMaxAlbumItems)
	}
	if c.MaxAlbumBytes <= 0 {
		return fmt.Errorf("cdn.max_album_bytes must be positive")
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
	switch strings.ToLower(strings.TrimSpace(c.SendMode)) {
	case "", cdnSendModeLink, cdnSendModeNever:
	default:
		return fmt.Errorf("cdn.send_mode must be %q or %q, got %q", cdnSendModeLink, cdnSendModeNever, c.SendMode)
	}
	if (c.UploadToken == "") != (c.UploadTokenOwner == "") {
		return fmt.Errorf("cdn.upload_token and cdn.upload_token_owner must be set together")
	}
	if c.UploadTokenOwner != "" && !strings.HasPrefix(c.UploadTokenOwner, "@") {
		return fmt.Errorf("cdn.upload_token_owner must be a Matrix user id")
	}
	if c.SendThresholdBytes < 0 {
		return fmt.Errorf("cdn.send_threshold cannot be negative")
	}
	sendExpiry := c.SendExpiry.Duration()
	if sendExpiry < 0 {
		return fmt.Errorf("cdn.send_expiry cannot be negative")
	}
	if sendExpiry > 0 && sendExpiry > maxExpiry {
		return fmt.Errorf("cdn.send_expiry cannot exceed cdn.max_expiry")
	}
	return nil
}

// EffectiveSendMode returns the outgoing-attachment policy: the configured
// mode, defaulting to "link" while the service is active and to "never" when
// it is not running.
func (c CdnConfig) EffectiveSendMode() string {
	if !c.Active() {
		return cdnSendModeNever
	}
	if strings.EqualFold(strings.TrimSpace(c.SendMode), cdnSendModeNever) {
		return cdnSendModeNever
	}
	return cdnSendModeLink
}

// CdnAuthFunc resolves the owner key of an authenticated file-service
// request (upload, list, revoke). Returning ok=false rejects the request. The
// wiring maps the app's paired-device bearer token to the Matrix user ID, so
// quotas and revocation are per Matrix user.
type CdnAuthFunc func(r *http.Request) (owner string, ok bool)

// CdnEntry is the stored metadata of one encrypted file or album. File
// entries leave Kind empty; album entries carry cdnKindAlbum and the item
// count. Size is the stored ciphertext total either way, so quotas treat both
// kinds uniformly.
type CdnEntry struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Kind      string    `json:"kind,omitempty"`
	Size      int64     `json:"size"`
	ItemCount int       `json:"item_count,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// isAlbum reports whether the entry is a multi-item album.
func (e CdnEntry) isAlbum() bool {
	return e.Kind == cdnKindAlbum
}

// CdnStats summarizes the stored entries for the management room.
type CdnStats struct {
	Files   int       `json:"files"`
	Albums  int       `json:"albums"`
	Bytes   int64     `json:"bytes"`
	Owners  int       `json:"owners"`
	Expired int       `json:"expired"`
	LastGC  time.Time `json:"last_gc,omitempty"`
	GCError string    `json:"gc_error,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// CdnService is the bridge-side file service: unguessable ids, ciphertext
// storage, per-user quotas with FIFO pruning, and expiry cleanup. The
// matching decryptor page is served for every entry (the key stays in the URL
// fragment and never reaches the service): a file viewer for single files and
// a gallery for albums.
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
	if cfg.UploadToken != "" {
		inner := auth
		token := cfg.UploadToken
		owner := cfg.UploadTokenOwner
		auth = func(r *http.Request) (string, bool) {
			if presented, ok := cdnBearerToken(r); ok && subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1 {
				return owner, true
			}
			if inner == nil {
				return "", false
			}
			return inner(r)
		}
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

// cdnBearerToken extracts the bearer token of a request, if any.
func cdnBearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
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
	mux.HandleFunc("POST "+cdnPathPrefix+"/albums", s.handleAlbumUpload)
	mux.HandleFunc("GET "+cdnPathPrefix, s.handleList)
	mux.HandleFunc("DELETE "+cdnPathPrefix+"/{id}", s.handleRevoke)
	mux.HandleFunc("GET "+cdnPathPrefix+"/{id}", s.handleWrapper)
	mux.HandleFunc("GET "+cdnPathPrefix+"/{id}/raw", s.handleDownload)
	mux.HandleFunc("GET "+cdnPathPrefix+"/{id}/manifest", s.handleAlbumManifest)
	mux.HandleFunc("GET "+cdnPathPrefix+"/{id}/items/{index}", s.handleAlbumItem)
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

	entry, err := s.publishCiphertext(owner, id, tempName, written, expiry)
	if err != nil {
		s.log.Warn().Err(err).Msg("Failed to store a CDN upload")
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to store the upload")
		return
	}
	writeCdnJSON(w, http.StatusCreated, cdnUploadResponse{
		ID:          entry.ID,
		Size:        entry.Size,
		ExpiresAtMS: entry.ExpiresAt.UnixMilli(),
	})
}

// CdnUpload is the result of an in-process store: the id a link embeds, the
// stored ciphertext size and the absolute expiry.
type CdnUpload struct {
	ID        string
	Size      int64
	ExpiresAt time.Time
}

// Store publishes one already-encrypted payload and reports the entry a link
// points at. It is the in-process equivalent of POST /f for the bridge's own
// uploads, which have no device bearer token to present: the same size caps,
// expiry rules and per-user FIFO pruning apply, and owner is the Matrix user
// id that owns the entry. The bridge never needs to see the key: callers
// encrypt, keep the fragment key and pass the ciphertext only.
func (s *CdnService) Store(owner string, ciphertext []byte, expiry time.Duration) (*CdnUpload, error) {
	if s == nil {
		return nil, fmt.Errorf("the cdn file service is not active")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, fmt.Errorf("store cdn object: an owner is required")
	}
	size := int64(len(ciphertext))
	if size == 0 {
		return nil, fmt.Errorf("store cdn object: the encrypted payload is empty")
	}
	if size > s.cfg.MaxFileBytes {
		return nil, fmt.Errorf("the encrypted payload exceeds the %d byte upload limit", s.cfg.MaxFileBytes)
	}
	if size > s.cfg.PerUserCapBytes {
		return nil, fmt.Errorf("the encrypted payload exceeds the %d byte storage quota", s.cfg.PerUserCapBytes)
	}
	linkExpiry, err := s.resolveStoreExpiry(expiry)
	if err != nil {
		return nil, err
	}
	id, err := newCdnID()
	if err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp(s.objectDir(), ".tmp-store-*")
	if err != nil {
		return nil, fmt.Errorf("store cdn object: %w", err)
	}
	tempName := temp.Name()
	if _, err := temp.Write(ciphertext); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return nil, fmt.Errorf("store cdn object: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return nil, fmt.Errorf("store cdn object: %w", err)
	}
	entry, err := s.publishCiphertext(owner, id, tempName, size, linkExpiry)
	if err != nil {
		return nil, err
	}
	return &CdnUpload{ID: entry.ID, Size: entry.Size, ExpiresAt: entry.ExpiresAt}, nil
}

// resolveStoreExpiry normalizes the requested lifetime of an in-process
// store: zero or negative means the configured default, and anything above
// max_expiry is rejected.
func (s *CdnService) resolveStoreExpiry(requested time.Duration) (time.Duration, error) {
	if requested <= 0 {
		return s.cfg.DefaultExpiry.Duration(), nil
	}
	if max := s.cfg.MaxExpiry.Duration(); max > 0 && requested > max {
		return 0, fmt.Errorf("requested expiry %s is above the %s maximum",
			formatTTLDuration(requested), formatTTLDuration(max))
	}
	return requested, nil
}

// publishCiphertext commits one staged ciphertext file: it renames the
// temporary object into place, writes the metadata sidecar and prunes the
// owner's oldest entries until the per-user cap holds. All three steps run
// under the service lock, so no reader sees a half-published entry. The
// temporary file is removed when the commit fails.
func (s *CdnService) publishCiphertext(owner, id, tempPath string, size int64, expiry time.Duration) (*CdnEntry, error) {
	now := time.Now().UTC()
	entry := CdnEntry{
		ID:        id,
		Owner:     owner,
		Size:      size,
		CreatedAt: now,
		ExpiresAt: now.Add(expiry),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(tempPath, s.objectPath(id)); err != nil {
		_ = os.Remove(tempPath)
		return nil, fmt.Errorf("store cdn object: %w", err)
	}
	if err := s.writeMetaLocked(entry); err != nil {
		_ = os.Remove(s.objectPath(id))
		return nil, fmt.Errorf("store cdn metadata: %w", err)
	}
	pruned, err := s.pruneOwnerLocked(owner, id)
	if err != nil {
		s.log.Warn().Err(err).Str("owner", owner).Msg("Failed to prune CDN files over the per-user quota")
	}
	if pruned > 0 {
		s.log.Info().Str("owner", owner).Int("pruned", pruned).Msg("Pruned oldest CDN files over the per-user quota")
	}
	return &entry, nil
}

// handleDownload serves the stored ciphertext of one file. The URL is the
// secret: no headers or cookies are required. Albums have their own manifest
// and item routes, so the file-only route on an album is a 404.
func (s *CdnService) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entry, err := s.loadEntry(id)
	if err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to read the file metadata")
		return
	}
	if entry == nil || entry.isAlbum() {
		http.NotFound(w, r)
		return
	}
	s.writeCdnObject(w, r, s.objectPath(id), id+".bin")
}

// writeCdnObject streams one stored ciphertext object with the public
// download headers. Missing objects are a 404; callers resolve the metadata
// (and reject foreign kinds) before calling.
func (s *CdnService) writeCdnObject(w http.ResponseWriter, r *http.Request, path, filename string) {
	file, err := os.Open(path)
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
	if err != nil || info.IsDir() {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to open the file")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
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

// handleWrapper serves the static decryptor page: the file viewer or, for
// album ids, the gallery. The page fetches the ciphertext and decrypts it in
// the browser with the fragment key; the server never sees that key.
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
	s.setCdnWrapperHeaders(w, entry.isAlbum())
	w.WriteHeader(http.StatusOK)
	page := cdnWrapperTemplate
	if entry.isAlbum() {
		page = cdnAlbumWrapperTemplate
	}
	if err := page.Execute(w, cdnWrapperData{ID: id}); err != nil {
		s.log.Warn().Err(err).Msg("Failed to render the CDN wrapper page")
	}
}

// setCdnWrapperHeaders applies the locked-down policy shared by the file and
// album wrapper pages.
func (s *CdnService) setCdnWrapperHeaders(w http.ResponseWriter, album bool) {
	csp := cdnWrapperCSP
	if album {
		csp = cdnAlbumWrapperCSP
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}

// cdnListResponse is the GET /f response body. Files and albums are listed
// separately so existing file-only clients keep working; UsedBytes and
// CapBytes cover both, since albums count against the same quota.
type cdnListResponse struct {
	Files     []CdnEntry `json:"files"`
	Albums    []CdnEntry `json:"albums"`
	CapBytes  int64      `json:"cap_bytes"`
	UsedBytes int64      `json:"used_bytes"`
}

// handleList returns the caller's active files and albums.
func (s *CdnService) handleList(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.authenticate(r)
	if !ok {
		writeCdnError(w, http.StatusUnauthorized, "unauthorized", "a valid device bearer token is required")
		return
	}
	entries, used, err := s.Usage(owner)
	if err != nil {
		writeCdnError(w, http.StatusInternalServerError, "internal", "failed to list your files")
		return
	}
	files := make([]CdnEntry, 0, len(entries))
	albums := make([]CdnEntry, 0)
	for _, entry := range entries {
		if entry.isAlbum() {
			albums = append(albums, entry)
		} else {
			files = append(files, entry)
		}
	}
	writeCdnJSON(w, http.StatusOK, cdnListResponse{Files: files, Albums: albums, CapBytes: s.cfg.PerUserCapBytes, UsedBytes: used})
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

// Usage returns the owner's active files and albums, newest first, with the
// byte total that counts against the per-user quota. Albums are one entry
// each, sized as the total ciphertext of their items plus the manifest.
func (s *CdnService) Usage(owner string) ([]CdnEntry, int64, error) {
	if s == nil {
		return nil, 0, nil
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.listAllLocked()
	if err != nil {
		return nil, 0, err
	}
	now := time.Now()
	entries := make([]CdnEntry, 0, len(all))
	var used int64
	for _, entry := range all {
		if entry.Owner != owner || !entry.ExpiresAt.After(now) {
			continue
		}
		entries = append(entries, entry)
		used += entry.Size
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.After(entries[j].CreatedAt)
		}
		return entries[i].ID > entries[j].ID
	})
	return entries, used, nil
}

// List returns the owner's active files, newest first. Albums are excluded;
// use ListAlbums or Usage for the album view and the quota total.
func (s *CdnService) List(owner string) ([]CdnEntry, error) {
	entries, _, err := s.Usage(owner)
	if err != nil {
		return nil, err
	}
	return filterCdnEntries(entries, false), nil
}

// ListAlbums returns the owner's active albums, newest first.
func (s *CdnService) ListAlbums(owner string) ([]CdnEntry, error) {
	entries, _, err := s.Usage(owner)
	if err != nil {
		return nil, err
	}
	return filterCdnEntries(entries, true), nil
}

// filterCdnEntries keeps only files or only albums.
func filterCdnEntries(entries []CdnEntry, albums bool) []CdnEntry {
	filtered := make([]CdnEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.isAlbum() == albums {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// Revoke removes a file or album owned by owner. It reports ok=false for
// unknown ids and for entries owned by someone else. Albums are removed
// whole, manifest and items together.
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
// counted separately until the cleanup removes them. Bytes covers files and
// albums; Owners counts users with either.
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
		if entry.isAlbum() {
			stats.Albums++
		} else {
			stats.Files++
		}
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

// removeEntryLocked deletes an entry's metadata and payload. File payloads
// are single objects; album payloads are directories (manifest + items).
// os.RemoveAll handles both and never follows symlinks. Callers must hold
// s.mu.
func (s *CdnService) removeEntryLocked(id string) error {
	if !validCdnID(id) {
		return nil
	}
	if err := os.Remove(s.metaPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(s.objectPath(id)); err != nil {
		return err
	}
	return nil
}

// pruneOwnerLocked removes the owner's oldest entries (files and albums,
// never keepID) until the stored bytes fit within the per-user cap. Albums
// are removed whole. Callers must hold s.mu.
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

// runGCLocked removes expired entries, orphaned objects/directories left by
// interruptions, incomplete albums and stale temporary uploads. Each id is
// judged exactly once, so an expired album is one removal, not two. Callers
// must hold s.mu.
func (s *CdnService) runGCLocked(now time.Time) (int, error) {
	dirEntries, err := os.ReadDir(s.objectDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	seen := make(map[string]struct{}, len(dirEntries))
	for _, dirEntry := range dirEntries {
		name := dirEntry.Name()
		if strings.HasPrefix(name, ".") {
			if info, statErr := dirEntry.Info(); statErr == nil && now.Sub(info.ModTime()) > cdnStaleTempAge {
				// Interrupted uploads are files (single uploads) or
				// directories (album containers staged item by item).
				_ = os.RemoveAll(filepath.Join(s.objectDir(), name))
			}
			continue
		}
		id := name
		if strings.HasSuffix(name, cdnMetaSuffix) {
			id = strings.TrimSuffix(name, cdnMetaSuffix)
		}
		if !validCdnID(id) {
			continue
		}
		if _, done := seen[id]; done {
			continue
		}
		seen[id] = struct{}{}
		gone, err := s.gcEntryLocked(id, now)
		if err != nil {
			return removed, err
		}
		if gone {
			removed++
		}
	}
	return removed, nil
}

// gcEntryLocked decides the fate of one metadata id: keep an active, complete
// file or album, remove anything that is expired, missing its payload or
// missing its metadata. Callers must hold s.mu. It reports whether an entry
// was removed.
func (s *CdnService) gcEntryLocked(id string, now time.Time) (bool, error) {
	entry, err := s.readMeta(id)
	if err != nil {
		return false, err
	}
	if entry == nil {
		// No readable metadata: whatever object or directory is left is an
		// interrupted upload.
		return true, s.removeEntryLocked(id)
	}
	if !entry.ExpiresAt.After(now) {
		return true, s.removeEntryLocked(id)
	}
	if entry.isAlbum() {
		// An album is kept only while manifest and every promised item exist;
		// a partial album is removed whole, so item bytes never outlive their
		// metadata (or survive a crash mid-write).
		if !s.albumDirComplete(id, entry) {
			return true, s.removeEntryLocked(id)
		}
		return false, nil
	}
	info, err := os.Stat(s.objectPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return true, s.removeEntryLocked(id)
		}
		return false, err
	}
	if info.IsDir() {
		// A file entry must map to a plain object, not a directory.
		return true, s.removeEntryLocked(id)
	}
	return false, nil
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
				entries, used, err := service.Usage(owner)
				if err != nil {
					ce.Reply("Failed to list your file links: %v", err)
					return
				}
				lines := make([]string, 0, len(entries)+6)
				for _, entry := range entries {
					if entry.isAlbum() {
						lines = append(lines, fmt.Sprintf("- `%s` — album: %d item(s), %s, expires %s",
							entry.ID, entry.ItemCount, formatCdnBytes(entry.Size), formatCdnTime(entry.ExpiresAt)))
						continue
					}
					lines = append(lines, fmt.Sprintf("- `%s` — %s, expires %s",
						entry.ID, formatCdnBytes(entry.Size), formatCdnTime(entry.ExpiresAt)))
				}
				if len(entries) == 0 {
					lines = append(lines, "No active file links or albums uploaded by you.")
				} else {
					lines = append(lines, fmt.Sprintf("Storage: %s used of %s.",
						formatCdnBytes(used), formatCdnBytes(service.cfg.PerUserCapBytes)))
				}
				received := make([]string, 0)
				for _, ref := range service.Refs(owner) {
					if ref.Direction != cdnRefDirectionReceived {
						continue
					}
					party := ref.Party
					if party == "" {
						party = "unknown sender"
					}
					received = append(received, fmt.Sprintf("- `%s` — from %s, %s", ref.ID, party, formatCdnTime(ref.CreatedAt)))
				}
				if len(received) > 0 {
					lines = append(lines, "", fmt.Sprintf("Received links (%d):", len(received)))
					lines = append(lines, received...)
				}
				ce.Reply("Your file links and albums (%d):\n%s\n\nInspect or expire any recorded link with `$cmdprefix cdn view|expire <id>`.",
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
			case "view":
				if len(ce.Args) < 2 {
					ce.Reply("Usage: $cmdprefix cdn view <id>")
					return
				}
				id := strings.TrimSpace(ce.Args[1])
				entry, err := service.readMeta(id)
				if err != nil {
					ce.Reply("Failed to read file `%s`: %v", id, err)
					return
				}
				ref, hasRef := service.Ref(owner, id)
				if entry == nil && !hasRef {
					ce.Reply("Nothing with id `%s`: no active file and no link recorded for you.", id)
					return
				}
				lines := make([]string, 0, 3)
				if entry == nil {
					lines = append(lines, fmt.Sprintf("- `%s`: the stored object is gone (expired or removed).", id))
				} else {
					kind := "file"
					detail := formatCdnBytes(entry.Size)
					if entry.isAlbum() {
						kind = "album"
						detail = fmt.Sprintf("%d item(s), %s", entry.ItemCount, formatCdnBytes(entry.Size))
					}
					state := "expires " + formatCdnTime(entry.ExpiresAt)
					if !entry.ExpiresAt.After(time.Now()) {
						state = "expired (awaiting cleanup)"
					}
					lines = append(lines, fmt.Sprintf("- `%s`: %s, %s, owned by %s, %s",
						entry.ID, kind, detail, entry.Owner, state))
				}
				if hasRef {
					party := ref.Party
					switch {
					case party == "" && ref.Direction == cdnRefDirectionSent:
						party = "unknown recipient"
					case party == "":
						party = "unknown sender"
					}
					lines = append(lines, fmt.Sprintf("- link %s %s, %s: %s",
						ref.Direction, party, formatCdnTime(ref.CreatedAt), ref.URL))
				}
				lines = append(lines, "", fmt.Sprintf("Expire it with `$cmdprefix cdn expire %s`.", id))
				ce.Reply("%s", strings.Join(lines, "\n"))
			case "expire":
				if len(ce.Args) < 2 {
					ce.Reply("Usage: $cmdprefix cdn expire <id>")
					return
				}
				id := strings.TrimSpace(ce.Args[1])
				ok, err := service.Revoke(owner, id)
				if err != nil {
					ce.Reply("Failed to expire `%s`: %v", id, err)
					return
				}
				if ok {
					ce.Reply("Expired `%s`. The link is dead now.", id)
					return
				}
				if _, hasRef := service.Ref(owner, id); hasRef {
					deleted, err := service.Delete(id)
					if err != nil {
						ce.Reply("Failed to expire `%s`: %v", id, err)
						return
					}
					_ = service.ForgetRef(owner, id)
					if deleted {
						ce.Reply("Expired `%s` and removed it from the file service.", id)
						return
					}
					ce.Reply("`%s` was already gone from the file service; removed it from your records.", id)
					return
				}
				if entry, err := service.readMeta(id); err == nil && entry != nil && entry.Owner == owner {
					if _, err := service.Delete(id); err != nil {
						ce.Reply("Failed to expire `%s`: %v", id, err)
						return
					}
					ce.Reply("Expired `%s` (it had already expired and was awaiting cleanup).", id)
					return
				}
				ce.Reply("No active file with id `%s` is owned by you and no link with that id is recorded for you.", id)
			case "stats":
				if ce.User.Permissions.Admin {
					stats := service.Stats()
					lastGC := "never"
					if !stats.LastGC.IsZero() {
						lastGC = formatCdnTime(stats.LastGC)
					}
					lines := []string{fmt.Sprintf(
						"File service: %d active file(s), %d active album(s), %s across %d user(s), %d expired awaiting cleanup. Last cleanup: %s.",
						stats.Files, stats.Albums, formatCdnBytes(stats.Bytes), stats.Owners, stats.Expired, lastGC)}
					lines = append(lines, cdnLimitsText(service.cfg))
					if stats.GCError != "" {
						lines = append(lines, "Last cleanup error: "+stats.GCError)
					}
					if stats.Error != "" {
						lines = append(lines, "Storage error: "+stats.Error)
					}
					ce.Reply("%s", strings.Join(lines, "\n"))
					return
				}
				entries, used, err := service.Usage(owner)
				if err != nil {
					ce.Reply("Failed to read your file service usage: %v", err)
					return
				}
				ce.Reply("Your file links and albums: %d active, %s used.\n%s",
					len(entries), formatCdnBytes(used), cdnLimitsText(service.cfg))
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
			Description: "List, inspect, and expire bridge-hosted encrypted file links and albums",
			Args:        "list | view <id> | expire <id> | revoke <id> | stats | gc",
		},
	}
}

// cdnCommandHelpText is the `cdn` command usage shown in the management room.
func cdnCommandHelpText() string {
	return "Usage: $cmdprefix cdn <list|view <id>|expire <id>|revoke <id>|stats|gc>\n" +
		"- list: show your active file links and albums plus the links you received\n" +
		"- view <id>: show one file's details and its recorded link\n" +
		"- expire <id>: delete a file you own, or a received link on this service\n" +
		"- revoke <id>: delete one of your own file links or albums\n" +
		"- stats: show storage usage (global for admins, yours otherwise)\n" +
		"- gc: run expiry cleanup now (admins only)"
}

// cdnLimitsText renders the effective size caps of the file service for the
// management room.
func cdnLimitsText(cfg CdnConfig) string {
	return fmt.Sprintf("Limits: %s per upload, %s stored per user, %d item(s) and %s per album.",
		formatCdnBytes(cfg.MaxFileBytes), formatCdnBytes(cfg.PerUserCapBytes),
		cfg.MaxAlbumItems, formatCdnBytes(cfg.MaxAlbumBytes))
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
