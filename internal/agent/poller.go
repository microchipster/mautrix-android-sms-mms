// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Sweeper polls the phone for changes and publishes them as events.
type Sweeper struct {
	cfg   *Config
	prov  Provider
	state *State
	log   *EventLog
	zlog  zerolog.Logger

	lastThreadSweep time.Time
}

// NewSweeper creates a sweeper.
func NewSweeper(cfg *Config, prov Provider, state *State, log *EventLog, zlog zerolog.Logger) *Sweeper {
	return &Sweeper{cfg: cfg, prov: prov, state: state, log: log, zlog: zlog}
}

// Run sweeps until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	interval := time.Duration(s.cfg.PollIntervalMS) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// SweepOnce runs a single sweep (used by tests and the run loop).
func (s *Sweeper) SweepOnce(ctx context.Context) error { return s.sweep(ctx) }

const snapshotRetentionMS = int64((7 * 24 * time.Hour) / time.Millisecond)

func (s *Sweeper) sweep(ctx context.Context) error {
	wmSMS, wmMMS := s.state.Watermarks()
	now := time.Now().UnixMilli()
	if wmSMS == 0 {
		wmSMS = now - int64((30*24*time.Hour)/time.Millisecond)
	}
	if wmMMS == 0 {
		wmMMS = now - int64((30*24*time.Hour)/time.Millisecond)
	}
	sinceSMS := wmSMS - s.cfg.SweepOverlapMS
	sinceMMS := wmMMS - s.cfg.SweepOverlapMS

	if smsMsgs, err := s.prov.SweepSMS(ctx, sinceSMS); err != nil {
		s.zlog.Error().Err(err).Msg("SMS sweep failed")
	} else {
		s.handleMessages(ctx, smsMsgs, &wmSMS)
	}
	if mmsMsgs, err := s.prov.SweepMMS(ctx, sinceMMS); err != nil {
		s.zlog.Error().Err(err).Msg("MMS sweep failed")
	} else {
		s.handleMessages(ctx, mmsMsgs, &wmMMS)
	}

	if time.Since(s.lastThreadSweep) > 30*time.Second {
		s.lastThreadSweep = time.Now()
		s.sweepThreads(ctx)
	}

	s.state.SetWatermarks(wmSMS, wmMMS)
	s.state.PruneSnapshots(now - snapshotRetentionMS)
	if err := s.state.Save(); err != nil {
		s.zlog.Warn().Err(err).Msg("Failed to save agent state")
	}
	return nil
}

func (s *Sweeper) handleMessages(ctx context.Context, msgs []Message, watermark *int64) {
	for _, msg := range msgs {
		if msg.TimestampMS > *watermark {
			*watermark = msg.TimestampMS
		}
		if msg.Type == 3 { // draft
			continue
		}
		// Resolve the participant set (group MMS correctness depends on it).
		if len(msg.Recipients) == 0 {
			recipients := s.prov.ThreadRecipients(ctx, msg.ThreadID)
			if msg.Sender != "" {
				recipients = removeAddress(recipients, msg.Sender)
			}
			msg.Recipients = recipients
		}

		value := snapshotValue(msg)
		key := stateKey(msg.ID)
		prev, seen := s.state.Snapshot(key)
		if seen && prev.V == value {
			continue
		}

		if !seen {
			// Content-based dedupe for carrier retries that create a second row.
			dupKey := fmt.Sprintf("%s|%d|%s|%s", msg.Sender, msg.TimestampSentMS, strings.Join(msg.Recipients, ","), msg.Body)
			if s.state.SeenDuplicate(sha256Hex(dupKey), msg.TimestampMS, snapshotRetentionMS) {
				s.zlog.Debug().Str("message_id", msg.ID).Msg("Skipping duplicate provider row")
				s.state.PutSnapshot(key, msg.TimestampMS, value)
				continue
			}
		}

		if !seen && msg.Sender == "" {
			// Could be a message we sent; suppress if it matches an echo.
			if echo, ok := s.state.MatchEcho(msg.Body, msg.TimestampMS, msg.Recipients); ok {
				s.state.PutSnapshot(key, msg.TimestampMS, value)
				if echo.Resolved == "" {
					s.state.ResolveEcho(echo.SendID, msg.ID)
				}
				s.zlog.Debug().Str("message_id", msg.ID).Str("send_id", echo.SendID).Msg("Matched outgoing echo")
				continue
			}
		}

		eventType := "message.new"
		if seen {
			eventType = "message.updated"
		}
		preview := msg.Body
		if len(preview) > 120 {
			preview = preview[:120]
		}
		s.log.Append(Event{
			Type:          eventType,
			MessageID:     msg.ID,
			ThreadID:      msg.ThreadID,
			SubID:         msg.SubID,
			TimestampMS:   msg.TimestampMS,
			Sender:        msg.Sender,
			IsMMS:         msg.IsMMS,
			HasAttachment: len(msg.Attachments) > 0,
			Preview:       preview,
		})
		s.state.PutSnapshot(key, msg.TimestampMS, value)
	}
}

func (s *Sweeper) sweepThreads(ctx context.Context) {
	threads, err := s.prov.Threads(ctx)
	if err != nil {
		s.zlog.Error().Err(err).Msg("Thread sweep failed")
		return
	}
	for _, thread := range threads {
		value := sha256Hex(strings.Join(thread.Recipients, ",")) + fmt.Sprintf("|%t", thread.Read)
		snap, seen := s.state.ThreadSnapshot(thread.ID)
		if seen && snap.V == value {
			continue
		}
		s.state.PutThreadSnapshot(thread.ID, thread.DateMS, value)
		if !seen {
			continue // first observation is not an update
		}
		s.log.Append(Event{
			Type:        "thread.updated",
			ThreadID:    thread.ID,
			SubID:       thread.SubID,
			TimestampMS: thread.DateMS,
		})
	}
}

func snapshotValue(msg Message) string {
	attachments := make([]string, 0, len(msg.Attachments))
	for _, a := range msg.Attachments {
		attachments = append(attachments, fmt.Sprintf("%d/%s", a.PartID, a.ContentType))
	}
	sort.Strings(attachments)
	return fmt.Sprintf("%d|%t|%t|%d|%d|%s|%s",
		msg.Type, msg.Read, msg.Seen, msg.Status, msg.TimestampSentMS,
		strings.Join(attachments, ","), sha256Hex(msg.Body))
}
