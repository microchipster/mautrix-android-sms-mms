// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

// This file is the SMS/MMS analogue of jmpchat's jid.go: it normalizes phone
// numbers / SMS sender IDs and maps them onto Matrix-safe user and portal IDs.
//
// The escaping logic is preserved from jmpchat on purpose: mautrix-go's
// EncodeUserLocalpart leaves '+' unescaped and Synapse rejects user IDs
// containing it, and '_' is encoded to '__', so the escape sequences must not
// contain underscores ('=2b', '=2c').

// CanonicalAddress cleans and canonicalizes an SMS/MMS address.
//
// Handles E.164 (+15551234567), national numbers without country code
// (assumed to be US/CA +1 for 7- and 10-digit values), short codes (3-6
// digits, kept raw), RFC 3966 URI parameters (such as ;phone-context=...),
// alphanumeric sender IDs (kept as-is, uppercased) and email-to-SMS gateways
// (lowercased).
func CanonicalAddress(address string) string {
	address = strings.TrimSpace(UnescapeAddress(address))
	if address == "" {
		return ""
	}
	address = stripURIParameters(address)
	if address == "" {
		return ""
	}
	// Email-to-SMS gateway.
	if strings.Contains(address, "@") {
		return strings.ToLower(address)
	}
	if !isPhoneLike(address) {
		// Alphanumeric sender ID (e.g. "BANK"). Normalize case so IDs are stable.
		return strings.ToUpper(strings.TrimSpace(address))
	}
	hasCountryPrefix := strings.HasPrefix(address, "+")
	var digits strings.Builder
	for _, r := range address {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	value := digits.String()
	if value == "" {
		return ""
	}
	// Short codes (3-6 digits) without explicit '+' stay raw.
	if !hasCountryPrefix && len(value) >= 3 && len(value) <= 6 {
		return value
	}
	if !hasCountryPrefix && (len(value) == 7 || len(value) == 10) {
		value = "1" + value
	}
	return "+" + value
}

// CanonicalRecipients canonicalizes, dedupes and sorts a recipient list.
// Used to produce stable conversation keys and for diagnostics. It is not
// used as the portal ID: the phone's thread ID is the stable conversation
// identity, so that adding/removing a participant does not fracture a room.
func CanonicalRecipients(addresses []string) []string {
	seen := make(map[string]struct{}, len(addresses))
	cleaned := make([]string, 0, len(addresses))
	for _, address := range addresses {
		c := CanonicalAddress(address)
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		cleaned = append(cleaned, c)
	}
	sort.Strings(cleaned)
	return cleaned
}

// stripURIParameters strips RFC 3966 / URI parameters (such as ;phone-context=...).
func stripURIParameters(value string) string {
	if idx := strings.Index(value, ";"); idx != -1 {
		return strings.TrimSpace(value[:idx])
	}
	return strings.TrimSpace(value)
}

func isPhoneLike(value string) bool {
	value = stripURIParameters(value)
	if value == "" {
		return false
	}
	hasDigit := false
	for i, r := range value {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '+' && i == 0:
		case r == '(' || r == ')' || r == '-' || r == '.' || r == ' ' || r == '\t':
		default:
			return false
		}
	}
	return hasDigit
}

// EscapeAddress converts a phone number / address into a format safe for
// Matrix user IDs. See the note at the top of this file for why the escape
// sequences look like this.
func EscapeAddress(address string) string {
	escaped := strings.ReplaceAll(address, "+", "=2b")
	return strings.ReplaceAll(escaped, ",", "=2c")
}

// UnescapeAddress converts an escaped address back to its original form.
func UnescapeAddress(escaped string) string {
	val := strings.ReplaceAll(escaped, "=2b", "+")
	return strings.ReplaceAll(val, "=2c", ",")
}

// MakeUserID returns the network user ID for a phone number / sender ID.
func MakeUserID(address string) networkid.UserID {
	return networkid.UserID(EscapeAddress(CanonicalAddress(address)))
}

// MakePortalID returns the portal ID for a phone thread.
//
// The conversation identity is the telephony thread ID (scoped by SIM and by
// device), not the participant set. On Android, a group thread keeps its ID
// even when the recipient list changes, which is what lets the bridge keep one
// stable Matrix room per conversation instead of fracturing it.
//
// The device scope is derived from the agent's device serial (see DeviceScope)
// so that two phones bridged by the same Matrix user never share rooms.
func MakePortalID(deviceScope string, threadID int64, subID int) networkid.PortalID {
	return networkid.PortalID(fmt.Sprintf("thread-%s-%d-%d", deviceScope, subID, threadID))
}

// ParsePortalID splits a thread portal ID produced by MakePortalID back into
// its parts. ok is false when the ID does not have that shape.
func ParsePortalID(id networkid.PortalID) (deviceScope string, subID int, threadID int64, ok bool) {
	parts := strings.Split(string(id), "-")
	if len(parts) != 4 || parts[0] != "thread" || parts[1] == "" {
		return "", 0, 0, false
	}
	sub, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", 0, 0, false
	}
	thread, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	return parts[1], sub, thread, true
}

// DeviceScope derives a short, stable scope token from a device serial.
//
// It is the first 8 hex characters of sha256("device:" + serial). The login
// metadata stores the serial; the scope is recomputed whenever it is needed so
// no extra state has to be kept in sync.
func DeviceScope(serial string) string {
	serial = strings.TrimSpace(serial)
	if serial == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("device:" + serial))
	return hex.EncodeToString(sum[:])[:8]
}

// MakeAddressPortalID returns a fallback portal ID for cases where no phone
// thread exists yet (for example a room created from Matrix before the first
// message was sent).
func MakeAddressPortalID(address string) networkid.PortalID {
	return networkid.PortalID("addr-" + EscapeAddress(CanonicalAddress(address)))
}
