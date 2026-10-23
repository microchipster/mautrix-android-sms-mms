// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/rs/zerolog"
)

// Errors returned by the sender.
var (
	ErrMMSUnsupported = errors.New("mms_send_unsupported")
	ErrGroupSMS       = errors.New("group_sms_unsupported")
	ErrMessageTooLong = errors.New("sms_too_long")
	ErrSendFailed     = errors.New("send_failed")
)

// Sender sends SMS through the shell (`service call isms`) backend.
// This is text-only and single-PDU; MMS requires the app backend (phase 3).
type Sender struct {
	cfg     *Config
	adb     *Adb
	prov    Provider
	state   *State
	zlog    zerolog.Logger
	txCode  int
	callPkg string
}

// NewSender creates the shell sender.
func NewSender(cfg *Config, adb *Adb, prov Provider, state *State, sdkVersion int, zlog zerolog.Logger) *Sender {
	tx := cfg.Send.TxCode
	if tx == 0 {
		tx = ismsSendTxCode(sdkVersion)
	}
	return &Sender{
		cfg:     cfg,
		adb:     adb,
		prov:    prov,
		state:   state,
		zlog:    zlog,
		txCode:  tx,
		callPkg: cfg.Send.CallerPackage,
	}
}

// Enabled reports whether sending is configured.
func (s *Sender) Enabled() bool { return s.cfg.Send.Enabled }

// SupportsMMS reports MMS send support (shell backend: no).
func (s *Sender) SupportsMMS() bool { return false }

// ismsSendTxCode maps the Android SDK level to the ISms.sendTextForSubscriber
// transaction code (codes changed with Android releases; see docs/research-adb-reference.md).
func ismsSendTxCode(sdk int) int {
	switch {
	case sdk >= 30: // Android 11+ (current)
		return 5
	case sdk >= 23: // Android 6.0–10
		return 6
	case sdk >= 21: // Android 5.x
		return 9
	default: // Android 4.4
		return 5
	}
}

// SendResult is the outcome of a send request.
type SendResult struct {
	MessageID string
	ThreadID  int64
	PartsSent int
	Pending   bool
}

// SendSMS sends a text message to one recipient (group SMS = MMS, unsupported here).
func (s *Sender) SendSMS(ctx context.Context, sendID string, subID int, recipients []string, text string) (SendResult, error) {
	if !s.cfg.Send.Enabled {
		return SendResult{}, fmt.Errorf("%w: sending disabled in agent config", ErrSendFailed)
	}
	if len(recipients) != 1 {
		return SendResult{}, fmt.Errorf("%w: shell backend can only send one-to-one SMS; group messages require MMS", ErrGroupSMS)
	}
	dest := CanonicalAddress(recipients[0])
	if dest == "" {
		return SendResult{}, fmt.Errorf("%w: empty recipient", ErrSendFailed)
	}

	parts := []string{text}
	if countUTF16(text) > 160 {
		if !s.cfg.Send.AllowSMSSplit {
			return SendResult{}, fmt.Errorf("%w: message is %d UTF-16 units; shell backend cannot send multipart SMS (enable allow_sms_split to accept splitting into separate messages)", ErrMessageTooLong, countUTF16(text))
		}
		parts = splitMessage(text, 153)
	}

	if subID < 0 {
		subID = -1
	}
	sendTS := time.Now().UnixMilli()
	s.state.AddEcho(Echo{SendID: sendID, Address: dest, Body: text, TS: sendTS, ThreadID: 0})

	for i, part := range parts {
		if err := s.sendPart(ctx, subID, dest, part); err != nil {
			return SendResult{}, fmt.Errorf("%w: %v", ErrSendFailed, err)
		}
		s.zlog.Info().Str("to", dest).Int("part", i+1).Int("parts", len(parts)).Msg("Sent SMS part")
	}

	resolved, threadID, err := s.waitForEcho(ctx, sendID)
	if err != nil {
		s.zlog.Warn().Err(err).Msg("Could not resolve sent message id")
		return SendResult{PartsSent: len(parts), Pending: true}, nil
	}
	return SendResult{MessageID: resolved, ThreadID: threadID, PartsSent: len(parts)}, nil
}

func (s *Sender) sendPart(ctx context.Context, subID int, dest, text string) error {
	scr := "service call isms " + fmt.Sprintf("%d", s.txCode) +
		" i32 " + fmt.Sprintf("%d", subID) +
		" s16 " + ShellQuote(s.callPkg) +
		` s16 "null"` +
		" s16 " + ShellQuote(dest) +
		` s16 "null"` +
		" s16 " + ShellQuote(text) +
		` s16 "null"` +
		` s16 "null"` +
		" i32 1" +
		" i64 0"
	out, err := s.adb.Shell(ctx, scr)
	if err != nil {
		return err
	}
	if strings.Contains(out, "Exception") || strings.Contains(out, "Error:") {
		return fmt.Errorf("%s", strings.TrimSpace(out))
	}
	if strings.Contains(out, "Result: Parcel(00000000") {
		// Service returns a null binder result on some versions even on success.
		s.zlog.Debug().Str("out", strings.TrimSpace(out)).Msg("isms send returned empty parcel")
	}
	return nil
}

// waitForEcho waits for the sweeper to match the sent provider row to our echo.
func (s *Sender) waitForEcho(ctx context.Context, sendID string) (string, int64, error) {
	timeout := time.Duration(s.cfg.Send.ResolveTimeoutMS) * time.Millisecond
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if id := s.state.EchoMessageID(sendID); id != "" {
			msg, err := s.prov.Message(ctx, id)
			if err == nil {
				return id, msg.ThreadID, nil
			}
			return id, 0, nil
		}
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return "", 0, fmt.Errorf("timeout waiting for provider row")
}

func countUTF16(s string) int { return len(utf16.Encode([]rune(s))) }

func splitMessage(text string, maxUnits int) []string {
	var parts []string
	remaining := text
	for len(remaining) > 0 {
		units := 0
		cut := 0
		for i, r := range remaining {
			u := len(utf16.Encode([]rune{r}))
			if units+u > maxUnits {
				break
			}
			units += u
			cut = i + len(string(r))
		}
		if cut == 0 {
			cut = len(remaining)
		}
		parts = append(parts, remaining[:cut])
		remaining = remaining[cut:]
	}
	return parts
}
