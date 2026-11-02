// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"net/http"
	"testing"

	"github.com/rs/zerolog"
)

const testCdnStaticToken = "bridge-upload-token-static"

func TestCdnStaticUploadToken(t *testing.T) {
	service := testCdnService(t, func(c *CdnConfig) {
		c.UploadToken = testCdnStaticToken
		c.UploadTokenOwner = "@bridge:example.com"
	})
	server := testCdnServer(t, service)

	body, _ := appCdnEncrypt(t, []byte("token upload"))
	if status, _ := testCdnUpload(t, server, testCdnStaticToken, body, ""); status != http.StatusCreated {
		t.Fatalf("static token upload = %d, want 201", status)
	}

	// With the paired-device auth refusing unknown tokens, anything that is
	// not the static token is unauthorized.
	strictService, err := NewCdnService(zerolog.Nop(), testCdnConfig(t, func(c *CdnConfig) {
		c.UploadToken = testCdnStaticToken
		c.UploadTokenOwner = "@bridge:example.com"
	}), func(*http.Request) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("NewCdnService: %v", err)
	}
	strictServer := testCdnServer(t, strictService)
	body, _ = appCdnEncrypt(t, []byte("wrong token"))
	if status, _ := testCdnUpload(t, strictServer, "not-the-token", body, ""); status != http.StatusUnauthorized {
		t.Fatalf("wrong token upload = %d, want 401", status)
	}

	// Paired-device tokens keep working alongside the static token.
	body, _ = appCdnEncrypt(t, []byte("device upload"))
	if status, _ := testCdnUpload(t, server, "device-a", body, ""); status != http.StatusCreated {
		t.Fatalf("device token upload = %d, want 201", status)
	}
}

func TestCdnStaticUploadTokenValidation(t *testing.T) {
	cfg := testCdnConfig(t, func(c *CdnConfig) { c.UploadToken = "token-without-owner" })
	if _, err := NewCdnService(zerolog.Nop(), cfg, testCdnAuth); err == nil {
		t.Fatal("an upload token without an owner must be rejected")
	}
	cfg = testCdnConfig(t, func(c *CdnConfig) { c.UploadTokenOwner = "@bridge:example.com" })
	if _, err := NewCdnService(zerolog.Nop(), cfg, testCdnAuth); err == nil {
		t.Fatal("an owner without an upload token must be rejected")
	}
	cfg = testCdnConfig(t, func(c *CdnConfig) {
		c.UploadToken = "token"
		c.UploadTokenOwner = "not-an-mxid"
	})
	if _, err := NewCdnService(zerolog.Nop(), cfg, testCdnAuth); err == nil {
		t.Fatal("an owner that is not a Matrix user id must be rejected")
	}
}
