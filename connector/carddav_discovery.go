// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// cardDAVDiscoveryTimeout caps a full address book discovery run.
const cardDAVDiscoveryTimeout = 90 * time.Second

// cardDAVProbeTimeout caps each individual candidate probe.
const cardDAVProbeTimeout = 15 * time.Second

// cardDAVFetcher downloads an address book for a candidate URL.
type cardDAVFetcher func(ctx context.Context, cfg CardDAVConfig, creds *CardDAVCredentials) ([]byte, error)

// cardDAVLooksLikeURL reports whether the value is already an explicit URL.
func cardDAVLooksLikeURL(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// cardDAVCandidateURLs builds the list of known CardDAV URL patterns for an
// email address or host hint. The caller probes them in order and uses the
// first one that serves vCards.
func cardDAVCandidateURLs(hint, username string) []string {
	hint = strings.ToLower(strings.TrimSpace(hint))
	hint = strings.TrimRight(hint, "/")
	hint = strings.TrimSuffix(hint, ".")
	if hint == "" || cardDAVLooksLikeURL(hint) {
		return nil
	}
	local := ""
	domain := hint
	if index := strings.LastIndex(hint, "@"); index >= 0 {
		local = hint[:index]
		domain = hint[index+1:]
	}
	if domain == "" || strings.ContainsAny(domain, " /:") {
		return nil
	}
	user := local
	email := ""
	if local != "" {
		email = local + "@" + domain
	}
	trimmedUsername := strings.ToLower(strings.TrimSpace(username))
	if strings.Contains(trimmedUsername, "@") {
		if index := strings.LastIndex(trimmedUsername, "@"); index >= 0 {
			user = trimmedUsername[:index]
			email = trimmedUsername
			if usernameDomain := trimmedUsername[index+1:]; usernameDomain != "" {
				domain = usernameDomain
			}
		}
	} else if trimmedUsername != "" {
		user = trimmedUsername
		if email == "" {
			email = user + "@" + domain
		}
	}
	if user == "" || domain == "" {
		return nil
	}
	if email == "" {
		email = user + "@" + domain
	}
	var candidates []string
	add := func(values ...string) {
		for _, value := range values {
			if value != "" {
				candidates = append(candidates, value)
			}
		}
	}
	// Provider-specific endpoints first.
	switch domain {
	case "fastmail.com", "fastmail.fm", "fastmail.net", "fmail.co.uk":
		add(fmt.Sprintf("https://carddav.fastmail.com/dav/addressbooks/user/%s/Default/", email))
	case "gmail.com", "googlemail.com":
		add(fmt.Sprintf("https://www.googleapis.com/carddav/v1/principals/%s/lists/default/", email))
	case "posteo.de":
		add("https://posteo.de:8843/")
	case "mailbox.org":
		add("https://dav.mailbox.org/")
	}
	// Standard discovery and common self-hosted layouts on the given domain.
	add(
		fmt.Sprintf("https://%s/.well-known/carddav", domain),
		fmt.Sprintf("https://%s/SOGo/dav/%s/Contacts/personal/", domain, email),
		fmt.Sprintf("https://%s/remote.php/dav/addressbooks/users/%s/contacts/", domain, user),
		fmt.Sprintf("https://%s/remote.php/dav/addressbooks/users/%s/contacts/", domain, email),
		fmt.Sprintf("https://%s/dav.php/addressbooks/%s/default/", domain, user),
		fmt.Sprintf("https://%s/dav.php/addressbooks/%s/default/", domain, email),
		fmt.Sprintf("https://%s/%s/", domain, user),
		fmt.Sprintf("https://%s/cloud/remote.php/dav/addressbooks/users/%s/contacts/", domain, email),
	)
	// Subdomain variants for shared hosting setups.
	prefixes := []string{"mail", "cloud", "dav", "nextcloud", "carddav"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(domain, prefix+".") {
			continue
		}
		sub := prefix + "." + domain
		add(fmt.Sprintf("https://%s/.well-known/carddav", sub))
		switch prefix {
		case "mail":
			add(
				fmt.Sprintf("https://%s/SOGo/dav/%s/Contacts/personal/", sub, email),
				fmt.Sprintf("https://%s/remote.php/dav/addressbooks/users/%s/contacts/", sub, user),
				fmt.Sprintf("https://%s/dav.php/addressbooks/%s/default/", sub, email),
			)
		case "cloud", "nextcloud":
			add(
				fmt.Sprintf("https://%s/remote.php/dav/addressbooks/users/%s/contacts/", sub, user),
			)
		default: // dav, carddav
			add(
				fmt.Sprintf("https://%s/dav.php/addressbooks/%s/default/", sub, user),
				fmt.Sprintf("https://%s/%s/", sub, user),
			)
		}
	}
	// Hosted providers for custom domains.
	add(
		fmt.Sprintf("https://carddav.fastmail.com/dav/addressbooks/user/%s/Default/", email),
		fmt.Sprintf("https://www.googleapis.com/carddav/v1/principals/%s/lists/default/", email),
	)
	seen := map[string]struct{}{}
	unique := candidates[:0]
	for _, candidate := range candidates {
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		unique = append(unique, candidate)
	}
	return unique
}

// discoverCardDAVAddressbook probes the candidate URLs and returns the first
// one that serves an address book.
func discoverCardDAVAddressbook(ctx context.Context, cfg CardDAVConfig, candidates []string, username, password string) (string, []byte, int, error) {
	return discoverCardDAVAddressbookWith(ctx, cfg, candidates, username, password, fetchCardDAVAddressbook)
}

// discoverCardDAVAddressbookWith is the injectable variant used by tests.
func discoverCardDAVAddressbookWith(ctx context.Context, cfg CardDAVConfig, candidates []string, username, password string, fetch cardDAVFetcher) (string, []byte, int, error) {
	if fetch == nil {
		return "", nil, 0, fmt.Errorf("no CardDAV fetcher is configured")
	}
	deadline := time.Now().Add(cardDAVDiscoveryTimeout)
	tried := 0
	for _, candidate := range candidates {
		if time.Now().After(deadline) {
			break
		}
		if err := validateCardDAVURL(cfg, candidate); err != nil {
			continue
		}
		creds := &CardDAVCredentials{URL: candidate, Username: username, Password: password}
		probeCtx, cancel := context.WithTimeout(ctx, cardDAVProbeTimeout)
		data, err := fetch(probeCtx, cfg, creds)
		cancel()
		tried++
		if err == nil && looksLikeVCardData(data) {
			return candidate, data, tried, nil
		}
	}
	if tried == 0 {
		return "", nil, 0, fmt.Errorf("no usable CardDAV candidates were generated")
	}
	return "", nil, tried, fmt.Errorf("no CardDAV address book was found")
}
