// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"sync"
	"time"
)

// EventLog is an in-memory ring buffer of recent events with long-poll support.
// The bridge is expected to run a full catch-up via the REST routes whenever it
// receives reset=true (cursor fell out of the retained window).
type EventLog struct {
	mu     sync.Mutex
	buf    []Event
	max    int
	seq    int64
	notify chan struct{}
}

// NewEventLog creates an event log retaining at most max events.
func NewEventLog(max int) *EventLog {
	if max <= 0 {
		max = 10000
	}
	return &EventLog{max: max, notify: make(chan struct{})}
}

// Append stores an event, assigning it the next sequence number.
func (l *EventLog) Append(ev Event) Event {
	l.mu.Lock()
	l.seq++
	ev.Seq = l.seq
	l.buf = append(l.buf, ev)
	if len(l.buf) > l.max {
		l.buf = l.buf[len(l.buf)-l.max:]
	}
	close(l.notify)
	l.notify = make(chan struct{})
	l.mu.Unlock()
	return ev
}

// Cursor returns the newest sequence number.
func (l *EventLog) Cursor() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// Since returns events after seq. reset is true when seq is older than the
// retention window (the caller must run a full catch-up).
func (l *EventLog) Since(seq int64) (events []Event, cursor int64, reset bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cursor = l.seq
	if len(l.buf) > 0 && seq < l.buf[0].Seq-1 {
		return nil, cursor, true
	}
	for _, ev := range l.buf {
		if ev.Seq > seq {
			events = append(events, ev)
		}
	}
	return events, cursor, false
}

// Wait blocks until new events exist after seq, the timeout elapses, or ctx ends.
func (l *EventLog) Wait(ctx context.Context, seq int64, timeout time.Duration) (events []Event, cursor int64, reset bool) {
	deadline := time.Now().Add(timeout)
	for {
		events, cursor, reset = l.Since(seq)
		if len(events) > 0 || reset {
			return events, cursor, reset
		}
		l.mu.Lock()
		notify := l.notify
		l.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, cursor, false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, cursor, false
		case <-notify:
			timer.Stop()
		case <-timer.C:
			return nil, cursor, false
		}
	}
}
