// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// cardDAVEncryptedPrefix marks values encrypted at rest. Values without it
// are legacy plaintext rows and keep working until rewritten.
const cardDAVEncryptedPrefix = "enc:v1:"

// defaultCardDAVKeyFile is where the CardDAV encryption key is kept when the
// configuration does not point somewhere else. Both bridge containers mount
// their data directory at /data.
const defaultCardDAVKeyFile = "/data/carddav.key"

// cardDAVEncryptionKey resolves the key used to encrypt CardDAV data at rest.
// An explicit encryption_key wins; otherwise a key file is loaded, and
// generated with 0600 permissions on first use.
func cardDAVEncryptionKey(cfg CardDAVConfig) ([]byte, error) {
	if key := strings.TrimSpace(cfg.EncryptionKey); key != "" {
		sum := sha256.Sum256([]byte(key))
		return sum[:], nil
	}
	path := strings.TrimSpace(cfg.KeyFile)
	if path == "" {
		path = defaultCardDAVKeyFile
	}
	return cardDAVKeyFromFile(path)
}

// cardDAVKeyFromFile loads the key file, creating it when it does not exist.
func cardDAVKeyFromFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return parseCardDAVKeyFile(data, path)
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read CardDAV key file %s: %w", path, err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate CardDAV key: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// Another process may have created the file in the meantime.
		if data, readErr := os.ReadFile(path); readErr == nil {
			return parseCardDAVKeyFile(data, path)
		}
		return nil, fmt.Errorf("create CardDAV key file %s: %w", path, err)
	}
	if _, err := file.WriteString(base64.StdEncoding.EncodeToString(raw) + "\n"); err != nil {
		file.Close()
		return nil, fmt.Errorf("write CardDAV key file %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("write CardDAV key file %s: %w", path, err)
	}
	return raw, nil
}

// parseCardDAVKeyFile accepts a base64-encoded 32-byte key or 32 raw bytes.
func parseCardDAVKeyFile(data []byte, path string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data))); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if len(data) == 32 {
		return data, nil
	}
	return nil, fmt.Errorf("CardDAV key file %s must contain a base64-encoded 32-byte key", path)
}

// encryptCardDAVValue encrypts a secret for storage, unless it is empty or
// already encrypted.
func encryptCardDAVValue(cfg CardDAVConfig, plaintext string) (string, error) {
	if plaintext == "" || strings.HasPrefix(plaintext, cardDAVEncryptedPrefix) {
		return plaintext, nil
	}
	key, err := cardDAVEncryptionKey(cfg)
	if err != nil {
		return "", err
	}
	return encryptCardDAVWithKey(key, plaintext)
}

// decryptCardDAVValue decrypts a stored secret. Values without the encryption
// prefix are legacy plaintext rows and are returned unchanged.
func decryptCardDAVValue(cfg CardDAVConfig, stored string) (string, error) {
	if !strings.HasPrefix(stored, cardDAVEncryptedPrefix) {
		return stored, nil
	}
	key, err := cardDAVEncryptionKey(cfg)
	if err != nil {
		return "", err
	}
	return decryptCardDAVWithKey(key, stored)
}

func encryptCardDAVWithKey(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("initialize CardDAV cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("initialize CardDAV cipher: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate CardDAV nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return cardDAVEncryptedPrefix + base64.StdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func decryptCardDAVWithKey(key []byte, stored string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, cardDAVEncryptedPrefix))
	if err != nil {
		return "", fmt.Errorf("decode CardDAV ciphertext: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("initialize CardDAV cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("initialize CardDAV cipher: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", fmt.Errorf("CardDAV ciphertext is too short")
	}
	plain, err := gcm.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt CardDAV value: %w", err)
	}
	return string(plain), nil
}

// cardDAVNameIsEncrypted reports whether a stored name value is encrypted.
func cardDAVNameIsEncrypted(stored string) bool {
	return strings.HasPrefix(strings.TrimSpace(stored), cardDAVEncryptedPrefix)
}

// cardDAVDecryptStoredName decrypts a stored contact name for display. It
// falls back to returning the value unchanged for legacy plaintext rows.
func cardDAVDecryptStoredName(stored string) string {
	trimmed := strings.TrimSpace(stored)
	if trimmed == "" || !cardDAVNameIsEncrypted(trimmed) {
		return trimmed
	}
	key, err := cardDAVProcessKey()
	if err != nil {
		return ""
	}
	plain, err := decryptCardDAVWithKey(key, trimmed)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(plain)
}

// cardDAVStoredName returns the decrypted CardDAV name for a ghost.
func (nc *MyNetworkClient) cardDAVStoredName(meta *GhostMetadata) string {
	if meta == nil {
		return ""
	}
	return cardDAVDecryptStoredName(meta.CardDAVName)
}

// The process-wide key resolver lets naming helpers decrypt stored names
// without access to the network client (the configuration is process-wide).
var (
	cardDAVProcessKeyMu     sync.Mutex
	cardDAVProcessKeySource func() ([]byte, error)
	cardDAVProcessKeyValue  []byte
	cardDAVProcessKeyErr    error
	cardDAVProcessKeyReady  bool
)

// setCardDAVProcessKeyResolver installs the function that resolves the
// process-wide CardDAV encryption key (called from the connector start).
func setCardDAVProcessKeyResolver(source func() ([]byte, error)) {
	cardDAVProcessKeyMu.Lock()
	defer cardDAVProcessKeyMu.Unlock()
	cardDAVProcessKeySource = source
	cardDAVProcessKeyValue = nil
	cardDAVProcessKeyErr = nil
	cardDAVProcessKeyReady = false
}

// cardDAVProcessKey returns the cached process-wide encryption key.
func cardDAVProcessKey() ([]byte, error) {
	cardDAVProcessKeyMu.Lock()
	defer cardDAVProcessKeyMu.Unlock()
	if cardDAVProcessKeyReady {
		return cardDAVProcessKeyValue, cardDAVProcessKeyErr
	}
	if cardDAVProcessKeySource == nil {
		return nil, fmt.Errorf("CardDAV encryption is not configured")
	}
	cardDAVProcessKeyValue, cardDAVProcessKeyErr = cardDAVProcessKeySource()
	cardDAVProcessKeyReady = true
	return cardDAVProcessKeyValue, cardDAVProcessKeyErr
}
