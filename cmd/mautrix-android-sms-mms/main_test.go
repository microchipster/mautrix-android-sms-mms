// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"maunium.net/go/mautrix/event"
)

func TestNeuterBlockedMessage(t *testing.T) {
	evt := &event.Event{
		Type: event.EventMessage,
		Content: event.Content{Parsed: &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    "!sms login",
		}},
	}
	neuterBlockedMessage(evt)
	if evt.Type != event.EventReaction {
		t.Fatalf("event type = %s, want %s", evt.Type, event.EventReaction)
	}
	// bridgev2/queue.go routes EventMessage in the management room (or with the
	// command prefix in a bot DM) to the command processor; a neutered event must
	// match no branch and carry no command text.
	if msg := evt.Content.AsMessage(); msg != nil && msg.Body != "" {
		t.Fatalf("body still present after neutering: %q", msg.Body)
	}
	raw, err := json.Marshal(evt.Content)
	if err != nil {
		t.Fatalf("failed to marshal neutered content: %v", err)
	}
	if strings.Contains(string(raw), "!sms login") {
		t.Fatalf("neutered content still contains the blocked body: %s", raw)
	}
}

func TestNeuterBlockedMessageEmptyContent(t *testing.T) {
	evt := &event.Event{Type: event.EventMessage}
	neuterBlockedMessage(evt)
	if evt.Type != event.EventReaction {
		t.Fatalf("event type = %s, want %s", evt.Type, event.EventReaction)
	}
	if evt.Content.Parsed != nil || evt.Content.VeryRaw != nil || evt.Content.Raw != nil {
		t.Fatalf("expected empty content after neutering, got %+v", evt.Content)
	}
}

func TestNeuterBlockedMessageNilEvent(t *testing.T) {
	neuterBlockedMessage(nil)
}
