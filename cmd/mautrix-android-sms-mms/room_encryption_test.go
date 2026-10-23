// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestValidateEncryptionContent(t *testing.T) {
	if err := validateEncryptionContent(&event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}); err != nil {
		t.Fatalf("megolm content rejected: %v", err)
	}
	if err := validateEncryptionContent(nil); err == nil {
		t.Fatal("nil content accepted")
	}
	if err := validateEncryptionContent(&event.EncryptionEventContent{Algorithm: "org.example.unsupported"}); err == nil {
		t.Fatal("unsupported algorithm accepted")
	}
}

func TestNotFoundAndPermissionErrors(t *testing.T) {
	notFound := mautrix.HTTPError{RespError: &mautrix.RespError{ErrCode: mautrix.MNotFound.ErrCode}}
	if !isNotFoundError(notFound) {
		t.Fatal("M_NOT_FOUND not classified as missing state")
	}
	if isNotFoundError(errors.New("network error")) {
		t.Fatal("plain error classified as missing state")
	}

	forbidden := mautrix.HTTPError{RespError: &mautrix.RespError{ErrCode: mautrix.MForbidden.ErrCode}}
	if !isPermissionError(forbidden) {
		t.Fatal("M_FORBIDDEN not classified as permission error")
	}
	if isPermissionError(notFound) {
		t.Fatal("M_NOT_FOUND classified as permission error")
	}
}

func TestRoomEncryptionGateFailsClosed(t *testing.T) {
	gate := newRoomEncryptionGate()
	established, err := gate.ensureBotRoomEncrypted(t.Context(), nil, "!room:example.com")
	if err == nil {
		t.Fatal("gate succeeded without a bridge")
	}
	if established {
		t.Fatal("gate reported newly-established encryption despite failure")
	}
	gate.mu.Lock()
	_, verified := gate.enabled["!room:example.com"]
	gate.mu.Unlock()
	if verified {
		t.Fatal("failed gate run marked the room as verified")
	}
}
