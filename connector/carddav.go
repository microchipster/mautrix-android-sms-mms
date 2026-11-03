// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
)

const (
	maxCardDAVAddressbookSize = 32 << 20 // 32 MiB
	cardDAVFetchTimeout       = 60 * time.Second
	cardDAVSyncMaxAdds        = 500
	cardDAVSyncAddInterval    = 200 * time.Millisecond // 5 adds/s
	cardDAVAutoSyncInterval   = 6 * time.Hour
	maxVCardAvatarSize        = 1 << 20 // 1 MiB
)

// defaultCardDAVCountryCode is used for phone numbers without a country code.
const defaultCardDAVCountryCode = "1"

// CardDAVConfig stores the address book import configuration (`carddav` in the
// network config section).
type CardDAVConfig struct {
	AllowInsecureHTTP  bool   `yaml:"allow_insecure_http,omitempty"`
	AllowPrivateHosts  bool   `yaml:"allow_private_hosts,omitempty"`
	DefaultCountryCode string `yaml:"default_country_code,omitempty"`
}

// DefaultCardDAVConfig returns the address book defaults.
func DefaultCardDAVConfig() CardDAVConfig {
	return CardDAVConfig{DefaultCountryCode: defaultCardDAVCountryCode}
}

// CardDAVCredentials stores per-login CardDAV credentials and sync state.
type CardDAVCredentials struct {
	URL           string     `json:"url,omitempty"`
	Username      string     `json:"username,omitempty"`
	Password      string     `json:"password,omitempty"`
	LastSyncAt    *time.Time `json:"last_sync_at,omitempty"`
	LastSyncCount int        `json:"last_sync_count,omitempty"`
}

func (c *MyConnector) cardDAVConfig() CardDAVConfig {
	if c == nil || c.config == nil {
		return DefaultCardDAVConfig()
	}
	cfg := c.config.CardDAV
	if strings.TrimSpace(cfg.DefaultCountryCode) == "" {
		cfg.DefaultCountryCode = defaultCardDAVCountryCode
	}
	return cfg
}

func (nc *MyNetworkClient) cardDAVConfig() CardDAVConfig {
	if nc == nil || nc.connector == nil {
		return DefaultCardDAVConfig()
	}
	return nc.connector.cardDAVConfig()
}

func (nc *MyNetworkClient) cardDAVCredentials() *CardDAVCredentials {
	if nc == nil || nc.login == nil {
		return nil
	}
	meta, _ := nc.login.Metadata.(*LoginMetadata)
	if meta == nil {
		return nil
	}
	return meta.CardDAV
}

func (nc *MyNetworkClient) setCardDAVCredentials(ctx context.Context, creds *CardDAVCredentials) error {
	if nc == nil || nc.login == nil {
		return fmt.Errorf("login unavailable")
	}
	meta, _ := nc.login.Metadata.(*LoginMetadata)
	if meta == nil {
		meta = &LoginMetadata{}
	}
	meta.CardDAV = creds
	nc.login.Metadata = meta
	return nc.login.Save(ctx)
}

func (nc *MyNetworkClient) clearCardDAVCredentials(ctx context.Context) error {
	if nc == nil || nc.login == nil {
		return fmt.Errorf("login unavailable")
	}
	meta, _ := nc.login.Metadata.(*LoginMetadata)
	if meta == nil || meta.CardDAV == nil {
		return nil
	}
	meta.CardDAV = nil
	nc.login.Metadata = meta
	return nc.login.Save(ctx)
}

// validateCardDAVURL checks the URL scheme and rejects private targets unless
// the configuration explicitly allows them.
func validateCardDAVURL(cfg CardDAVConfig, raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("empty CardDAV URL")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("invalid CardDAV URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		if !cfg.AllowInsecureHTTP {
			return fmt.Errorf("plain HTTP is not allowed; use HTTPS or set carddav.allow_insecure_http")
		}
	default:
		return fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("CardDAV URL has no host")
	}
	if !cfg.AllowPrivateHosts {
		if host == "localhost" || strings.HasSuffix(host, ".local") {
			return fmt.Errorf("private hosts are not allowed; set carddav.allow_private_hosts to override")
		}
		if ip := net.ParseIP(host); ip != nil && isPrivateCardDAVIP(ip) {
			return fmt.Errorf("private hosts are not allowed; set carddav.allow_private_hosts to override")
		}
	}
	return nil
}

// isPrivateCardDAVIP reports whether an address is in a private range.
func isPrivateCardDAVIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true // CGNAT
	}
	return false
}

// normalizeCardDAVPhoneNumber converts a vCard phone number to E.164 when
// possible.
func normalizeCardDAVPhoneNumber(raw, defaultCountryCode string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(trimmed), "tel:"))
	if trimmed == "" || strings.ContainsAny(trimmed, ";,") {
		return "", false
	}
	var digits strings.Builder
	plus := false
	for index, r := range trimmed {
		switch {
		case unicode.IsDigit(r):
			digits.WriteRune(r)
		case r == '+' && index == 0:
			plus = true
		case r == ' ' || r == '\t' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return "", false
		}
	}
	number := digits.String()
	if len(number) < 7 || len(number) > 15 {
		return "", false
	}
	switch {
	case plus:
		return "+" + number, true
	case strings.HasPrefix(number, "00") && len(number) > 8:
		return "+" + number[2:], true
	case len(number) == 10 && defaultCountryCode != "":
		return "+" + defaultCountryCode + number, true
	case len(number) == 11 && strings.HasPrefix(number, "1"):
		return "+" + number, true
	case len(number) > 10:
		return "+" + number, true
	}
	return "", false
}

// cardDAVContact is one parsed vCard entry.
type cardDAVContact struct {
	Name      string
	Phones    []string
	Photo     []byte
	PhotoMime string
}

// parseVCards extracts contacts from vCard data.
func parseVCards(data []byte) []cardDAVContact {
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	lines := strings.Split(normalized, "\n")
	unfolded := make([]string, 0, len(lines))
	for _, line := range lines {
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(unfolded) > 0 {
			// RFC 6350 unfolding: drop the line break and the single whitespace
			// character that marks the continuation line.
			unfolded[len(unfolded)-1] += line[1:]
			continue
		}
		unfolded = append(unfolded, line)
	}
	var contacts []cardDAVContact
	var current *cardDAVContact
	for _, line := range unfolded {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		switch {
		case upper == "BEGIN:VCARD":
			current = &cardDAVContact{}
		case upper == "END:VCARD":
			if current != nil && (current.Name != "" || len(current.Phones) > 0) {
				contacts = append(contacts, *current)
			}
			current = nil
		default:
			if current == nil {
				continue
			}
			name, params, value, ok := splitVCardPropertyParams(trimmed)
			if !ok {
				continue
			}
			if index := strings.LastIndex(name, "."); index >= 0 {
				name = name[index+1:]
			}
			switch strings.ToUpper(name) {
			case "FN":
				if current.Name == "" {
					current.Name = strings.TrimSpace(value)
				}
			case "TEL":
				if phone := strings.TrimSpace(value); phone != "" {
					current.Phones = append(current.Phones, phone)
				}
			case "PHOTO":
				if current.Photo == nil {
					if photo, mime, ok := parseVCardPhoto(params, value); ok {
						current.Photo = photo
						current.PhotoMime = mime
					}
				}
			}
		}
	}
	return contacts
}

// splitVCardPropertyParams splits a content line into name, params and value.
func splitVCardPropertyParams(line string) (name, params, value string, ok bool) {
	colon := strings.Index(line, ":")
	if colon < 0 {
		return "", "", "", false
	}
	head := line[:colon]
	value = strings.TrimSpace(line[colon+1:])
	semicolon := strings.Index(head, ";")
	if semicolon < 0 {
		return head, "", value, true
	}
	return head[:semicolon], head[semicolon+1:], value, true
}

// parseVCardPhoto decodes an embedded vCard photo.
func parseVCardPhoto(params, value string) ([]byte, string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, "", false
	}
	hint := vCardPhotoMimeHint(params)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return nil, "", false
	}
	if strings.Contains(lower, "value=uri") {
		return nil, "", false
	}
	if strings.HasPrefix(lower, "data:") {
		comma := strings.Index(trimmed, ",")
		if comma < 0 {
			return nil, "", false
		}
		header := trimmed[:comma]
		payload := trimmed[comma+1:]
		if !strings.Contains(strings.ToLower(header), ";base64") {
			return nil, "", false
		}
		if hint == "" {
			media := strings.TrimPrefix(strings.ToLower(header), "data:")
			if semicolon := strings.Index(media, ";"); semicolon >= 0 {
				media = media[:semicolon]
			}
			if strings.Contains(media, "/") {
				hint = media
			}
		}
		decoded, err := base64.StdEncoding.DecodeString(stripASCIIWhitespace(payload))
		if err != nil {
			return nil, "", false
		}
		return validateVCardPhoto(decoded, hint)
	}
	decoded, err := base64.StdEncoding.DecodeString(stripASCIIWhitespace(trimmed))
	if err != nil {
		return nil, "", false
	}
	return validateVCardPhoto(decoded, hint)
}

// vCardPhotoMimeHint extracts a MIME type from the photo parameters.
func vCardPhotoMimeHint(params string) string {
	for _, param := range strings.Split(params, ";") {
		trimmed := strings.TrimSpace(param)
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "TYPE="):
			switch strings.TrimSpace(upper[len("TYPE="):]) {
			case "JPEG", "JPG":
				return "image/jpeg"
			case "PNG":
				return "image/png"
			case "GIF":
				return "image/gif"
			case "WEBP":
				return "image/webp"
			}
		default:
			if strings.Contains(trimmed, "/") {
				return strings.ToLower(trimmed)
			}
		}
	}
	return ""
}

// stripASCIIWhitespace removes ASCII whitespace from base64 payloads.
func stripASCIIWhitespace(value string) string {
	return strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(value)
}

// validateVCardPhoto checks size and MIME type of an embedded photo.
func validateVCardPhoto(data []byte, hint string) ([]byte, string, bool) {
	if len(data) == 0 || len(data) > maxVCardAvatarSize {
		return nil, "", false
	}
	mime := strings.ToLower(strings.TrimSpace(hint))
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	if !allowedVCardAvatarMime(mime) {
		return nil, "", false
	}
	return data, mime, true
}

// allowedVCardAvatarMime lists the image types accepted for contact photos.
func allowedVCardAvatarMime(mime string) bool {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// fetchCardDAVAddressbook downloads and size-checks the address book.
func fetchCardDAVAddressbook(ctx context.Context, cfg CardDAVConfig, creds *CardDAVCredentials) ([]byte, error) {
	if creds == nil {
		return nil, fmt.Errorf("CardDAV credentials are missing")
	}
	if err := validateCardDAVURL(cfg, creds.URL); err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, cardDAVFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, strings.TrimSpace(creds.URL), nil)
	if err != nil {
		return nil, fmt.Errorf("build CardDAV request: %w", err)
	}
	if strings.TrimSpace(creds.Username) != "" {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	req.Header.Set("Accept", "text/vcard, text/x-vcard, text/directory")
	client := &http.Client{Timeout: cardDAVFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch CardDAV address book: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("addressbook request failed with status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCardDAVAddressbookSize+1))
	if err != nil {
		return nil, fmt.Errorf("read CardDAV address book: %w", err)
	}
	if len(body) > maxCardDAVAddressbookSize {
		return nil, fmt.Errorf("addressbook is larger than %d bytes", maxCardDAVAddressbookSize)
	}
	return body, nil
}

// cardDAVSyncResult summarizes one address book sync.
type cardDAVSyncResult struct {
	Contacts   int
	Numbers    int
	Candidates int
	Added      int
	Updated    int
	Skipped    int
	Conflicts  int
	Photos     int
	DryRun     bool
}

// syncCardDAV imports contact names (and photos) from the address book.
func (nc *MyNetworkClient) syncCardDAV(ctx context.Context, apply bool) (*cardDAVSyncResult, error) {
	creds := nc.cardDAVCredentials()
	if creds == nil || strings.TrimSpace(creds.URL) == "" {
		return nil, fmt.Errorf("CardDAV is not configured. Run: carddav-login <addressbook URL> <username> <password>")
	}
	cfg := nc.cardDAVConfig()
	data, err := fetchCardDAVAddressbook(ctx, cfg, creds)
	if err != nil {
		return nil, err
	}
	contacts := parseVCards(data)
	result := &cardDAVSyncResult{DryRun: !apply}
	seen := map[string]struct{}{}
	for _, contact := range contacts {
		result.Contacts++
		var addresses []string
		for _, raw := range contact.Phones {
			number, ok := normalizeCardDAVPhoneNumber(raw, cfg.DefaultCountryCode)
			if !ok {
				continue
			}
			address := CanonicalAddress(number)
			if address == "" {
				continue
			}
			if _, duplicate := seen[address]; duplicate {
				continue
			}
			seen[address] = struct{}{}
			result.Numbers++
			addresses = append(addresses, address)
		}
		if len(addresses) == 0 {
			continue
		}
		known := true
		changed := false
		for _, address := range addresses {
			ghost, err := nc.bridge.GetExistingGhostByID(ctx, MakeUserID(address))
			if err != nil || ghost == nil {
				known = false
				break
			}
			meta, _ := ghost.Metadata.(*GhostMetadata)
			if meta == nil || strings.TrimSpace(meta.CardDAVName) == "" {
				known = false
				break
			}
			if meta.CardDAVName != contact.Name {
				changed = true
			}
		}
		if known && !changed {
			result.Skipped++
			continue
		}
		result.Candidates++
		if !apply || (!known && result.Added >= cardDAVSyncMaxAdds) {
			continue
		}
		for _, address := range addresses {
			if conflict := nc.setGhostCardDAVName(ctx, address, contact.Name); conflict {
				result.Conflicts++
			}
		}
		if known {
			result.Updated++
		} else {
			result.Added++
		}
		if len(contact.Photo) > 0 {
			result.Photos += nc.applyCardDAVPhoto(ctx, addresses, contact.Photo)
		}
		time.Sleep(cardDAVSyncAddInterval)
	}
	return result, nil
}

// applyCardDAVPhoto stores an address book photo on the given ghosts.
func (nc *MyNetworkClient) applyCardDAVPhoto(ctx context.Context, addresses []string, photo []byte) int {
	hash := sha256.Sum256(photo)
	hashHex := hex.EncodeToString(hash[:8])
	applied := 0
	for _, address := range addresses {
		ghost, err := nc.bridge.GetExistingGhostByID(ctx, MakeUserID(address))
		if err != nil || ghost == nil {
			continue
		}
		meta, _ := ghost.Metadata.(*GhostMetadata)
		if meta == nil {
			meta = &GhostMetadata{}
		}
		if strings.TrimSpace(meta.AvatarURL) != "" {
			continue // the phone knows a live avatar; keep it
		}
		if meta.CardDAVPhotoHash == hashHex && ghost.AvatarMXC != "" {
			continue
		}
		meta.CardDAVPhotoHash = hashHex
		ghost.Metadata = meta
		avatar := &bridgev2.Avatar{
			ID:  networkid.AvatarID("carddav:" + hashHex),
			Get: func(context.Context) ([]byte, error) { return photo, nil },
		}
		ghost.UpdateInfo(ctx, &bridgev2.UserInfo{Avatar: avatar})
		nc.refreshContactPortalAvatars(ctx, address, avatar)
		applied++
	}
	return applied
}

// refreshContactPortalAvatars queues an avatar update for the DM portals that
// belong to a contact.
func (nc *MyNetworkClient) refreshContactPortalAvatars(ctx context.Context, address string, avatar *bridgev2.Avatar) {
	if nc == nil || nc.bridge == nil || nc.login == nil || avatar == nil {
		return
	}
	key := CanonicalAddress(address)
	if key == "" {
		return
	}
	rows, err := nc.bridge.DB.UserPortal.GetAllForLogin(ctx, nc.login.UserLogin)
	if err != nil {
		return
	}
	for _, row := range rows {
		portal, err := nc.bridge.GetExistingPortalByKey(ctx, row.Portal)
		if err != nil || portal == nil || portal.MXID == "" || portal.RoomType != database.RoomTypeDM {
			continue
		}
		if !portalMatchesContactAddress(portal, key) {
			continue
		}
		nc.bridge.QueueRemoteEvent(nc.login, &simplevent.ChatInfoChange{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatInfoChange,
				PortalKey: networkid.PortalKey{ID: portal.ID},
				Timestamp: time.Now(),
			},
			ChatInfoChange: &bridgev2.ChatInfoChange{ChatInfo: &bridgev2.ChatInfo{Avatar: avatar}},
		})
	}
}

// portalMatchesContactAddress reports whether a DM portal belongs to a contact.
func portalMatchesContactAddress(portal *bridgev2.Portal, key string) bool {
	meta, _ := portal.Metadata.(*PortalMetadata)
	if meta != nil {
		recipients := portalParticipantList(meta)
		if len(recipients) == 1 {
			return CanonicalAddress(recipients[0]) == key
		}
		if meta.OtherUserID != "" {
			return CanonicalAddress(UnescapeAddress(string(meta.OtherUserID))) == key
		}
	}
	return CanonicalAddress(UnescapeAddress(string(portal.ID))) == key
}

// cardDAVAutoSyncLoop refreshes the address book import in the background so
// contact names added anywhere (for example in a phone's contacts app synced
// to CardDAV) reach the auto-named rooms without a manual `carddav-sync`.
func (nc *MyNetworkClient) cardDAVAutoSyncLoop(ctx context.Context, stopCh <-chan struct{}) {
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-timer.C:
		}
		if creds := nc.cardDAVCredentials(); creds != nil && strings.TrimSpace(creds.URL) != "" {
			result, err := nc.syncCardDAV(ctx, true)
			if err != nil {
				nc.log.Warn().Err(err).Msg("Background CardDAV sync failed")
			} else if result.Added > 0 || result.Updated > 0 || result.Photos > 0 {
				if changed, err := refreshAutoNamedPortalsForUser(ctx, nc.bridge, nc.login.User); err != nil {
					nc.log.Warn().Err(err).Msg("Background room name refresh failed")
				} else if changed > 0 {
					nc.log.Info().Int("rooms", changed).Msg("Renamed auto-named rooms after a background CardDAV sync")
				}
			}
		}
		timer.Reset(cardDAVAutoSyncInterval)
	}
}
