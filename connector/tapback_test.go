// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"fmt"
	"testing"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func TestParseTapback(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		want   tapback
		wantOK bool
	}{
		{name: "liked text", body: "Liked “see you at 8”", want: tapback{Emoji: "👍", TargetText: "see you at 8"}, wantOK: true},
		{name: "loved text", body: "Loved “good morning”", want: tapback{Emoji: "❤️", TargetText: "good morning"}, wantOK: true},
		{name: "disliked text", body: "Disliked “bad news”", want: tapback{Emoji: "👎", TargetText: "bad news"}, wantOK: true},
		{name: "laughed at text", body: "Laughed at “that joke”", want: tapback{Emoji: "😂", TargetText: "that joke"}, wantOK: true},
		{name: "emphasized text", body: "Emphasized “very important”", want: tapback{Emoji: "‼️", TargetText: "very important"}, wantOK: true},
		{name: "questioned text", body: "Questioned “are you sure?”", want: tapback{Emoji: "❓", TargetText: "are you sure?"}, wantOK: true},
		{name: "straight quotes", body: `Liked "plain quotes"`, want: tapback{Emoji: "👍", TargetText: "plain quotes"}, wantOK: true},
		{name: "liked image", body: "Liked an image", want: tapback{Emoji: "👍", IsMedia: true}, wantOK: true},
		{name: "liked video", body: "Liked a video", want: tapback{Emoji: "👍", IsMedia: true}, wantOK: true},
		{name: "loved image", body: "Loved an image", want: tapback{Emoji: "❤️", IsMedia: true}, wantOK: true},
		{name: "removed like", body: "Removed a like from “see you at 8”", want: tapback{Emoji: "👍", TargetText: "see you at 8", IsRemoval: true}, wantOK: true},
		{name: "removed dislike", body: "Removed a dislike from “bad news”", want: tapback{Emoji: "👎", TargetText: "bad news", IsRemoval: true}, wantOK: true},
		{name: "removed love", body: "Removed a love from “good morning”", want: tapback{Emoji: "❤️", TargetText: "good morning", IsRemoval: true}, wantOK: true},
		{name: "removed emphasis", body: "Removed an emphasis from “very important”", want: tapback{Emoji: "‼️", TargetText: "very important", IsRemoval: true}, wantOK: true},
		{name: "removed like from image", body: "Removed a like from an image", want: tapback{Emoji: "👍", IsMedia: true, IsRemoval: true}, wantOK: true},
		{name: "plain sentence", body: "Liked your photo a lot", wantOK: false},
		{name: "tapback word alone", body: "Liked", wantOK: false},
		{name: "empty body", body: "", wantOK: false},
		{name: "tapback word embedded", body: "I Liked “this”", wantOK: false},
		{name: "removal with unknown noun", body: "Removed a bookmark from “x”", wantOK: false},
		{name: "negated tapback", body: "Didn't like “see you at 8”", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseTapback(tc.body)
			if ok != tc.wantOK {
				t.Fatalf("parseTapback(%q) ok = %v, want %v", tc.body, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got != tc.want {
				t.Fatalf("parseTapback(%q) = %+v, want %+v", tc.body, got, tc.want)
			}
		})
	}
}

// TestParseTapbackEdgeCases covers the whitespace, quoting and rejection
// corners of the tapback grammar. Anything that parses as a tapback but has no
// match in the recent history is forwarded as plain text by the caller, so
// false positives here are visible as odd reaction-looking messages.
func TestParseTapbackEdgeCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		body   string
		want   tapback
		wantOK bool
	}{
		{name: "surrounding whitespace", body: "  Liked “see you at 8”  ", want: tapback{Emoji: "👍", TargetText: "see you at 8"}, wantOK: true},
		{name: "trailing carriage return on media", body: "Liked an image\r", want: tapback{Emoji: "👍", IsMedia: true}, wantOK: true},
		{name: "laughed at media", body: "Laughed at a video", want: tapback{Emoji: "😂", IsMedia: true}, wantOK: true},
		{name: "emphasized media", body: "Emphasized a photo", want: tapback{Emoji: "‼️", IsMedia: true}, wantOK: true},
		{name: "removed heart", body: "Removed a heart from “good morning”", want: tapback{Emoji: "❤️", TargetText: "good morning", IsRemoval: true}, wantOK: true},
		{name: "removed laugh", body: "Removed a laugh from “that joke”", want: tapback{Emoji: "😂", TargetText: "that joke", IsRemoval: true}, wantOK: true},
		{name: "removed question from media", body: "Removed a question from an image", want: tapback{Emoji: "❓", IsMedia: true, IsRemoval: true}, wantOK: true},
		// Empty quotes parse (the matcher then refuses an empty target text
		// and the caller forwards the text unchanged).
		{name: "empty quote", body: "Liked “”", want: tapback{Emoji: "👍"}, wantOK: true},
		{name: "lowercase verb", body: "liked “see you at 8”", wantOK: false},
		{name: "double space after verb", body: "Loved  an image", wantOK: false},
		{name: "multiline quote", body: "Liked “multi\nline”", wantOK: false},
		{name: "digit in media noun", body: "Liked a 3D video", wantOK: false},
		{name: "punctuation after media noun", body: "Liked an image!", wantOK: false},
		{name: "trailing text after quote", body: "Removed a like from “x” extra", wantOK: false},
		{name: "missing from clause", body: "Removed a like", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseTapback(tc.body)
			if ok != tc.wantOK {
				t.Fatalf("parseTapback(%q) ok = %v, want %v", tc.body, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got != tc.want {
				t.Fatalf("parseTapback(%q) = %+v, want %+v", tc.body, got, tc.want)
			}
		})
	}
}

func TestNormalizeTapbackQuotedText(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"see you at 8":     "see you at 8",
		"see you at 8…":    "see you at 8",
		"see you at 8...":  "see you at 8",
		"see you at 8 …":   "see you at 8",
		"  spaced  ":       "spaced",
		"…":                "",
		"...":              "",
		"trailing…...":     "trailing…",
		"look at this....": "look at this.",
	}
	for input, want := range tests {
		if got := normalizeTapbackQuotedText(input); got != want {
			t.Errorf("normalizeTapbackQuotedText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIndexMessageOrderAndLookback(t *testing.T) {
	t.Parallel()
	client := &MyNetworkClient{}
	key := networkid.PortalKey{ID: "thread-deadbeef-1-42"}
	for i := 0; i < tapbackLookback+5; i++ {
		client.indexMessage(key, indexedMessage{ID: networkid.MessageID(fmt.Sprintf("m%d", i)), Body: "body"})
	}

	entries := client.indexedMessages(key)
	if len(entries) != tapbackLookback {
		t.Fatalf("index size = %d, want the %d entry lookback cap", len(entries), tapbackLookback)
	}
	if entries[0].ID != networkid.MessageID(fmt.Sprintf("m%d", tapbackLookback+4)) {
		t.Fatalf("newest entry = %q, want the last indexed message", entries[0].ID)
	}
	if entries[len(entries)-1].ID != networkid.MessageID("m5") {
		t.Fatalf("oldest entry = %q, want m5 after the cap", entries[len(entries)-1].ID)
	}

	// Indexes are per portal.
	if other := client.indexedMessages(networkid.PortalKey{ID: "thread-deadbeef-2-42"}); len(other) != 0 {
		t.Fatalf("other portal index = %v, want empty", other)
	}

	// An update moves to the front (newest-first) so tapbacks match the new body.
	client.indexMessage(key, indexedMessage{ID: "m100", Body: "edited"})
	if got := client.indexedMessages(key)[0]; got.ID != "m100" || got.Body != "edited" {
		t.Fatalf("updated index entry = %+v, want m100 at the front", got)
	}

	// Empty message IDs are never indexed.
	client.indexMessage(key, indexedMessage{})
	if got := len(client.indexedMessages(key)); got != tapbackLookback {
		t.Fatalf("index size after empty ID = %d, want %d", got, tapbackLookback)
	}
}

func TestMatchIndexedTapbackTargetEdgeCases(t *testing.T) {
	t.Parallel()
	// Entries without an ID are skipped instead of returning an empty match.
	messages := []indexedMessage{
		{Body: "target"},
		{ID: "m1", Body: "target"},
	}
	if got := matchIndexedTapbackTarget(messages, tapback{Emoji: "👍", TargetText: "target"}); got != "m1" {
		t.Fatalf("match with an ID-less entry = %q, want m1", got)
	}

	// A quote longer than the stored body is not a prefix match.
	short := []indexedMessage{{ID: "m1", Body: "short"}}
	if got := matchIndexedTapbackTarget(short, tapback{Emoji: "👍", TargetText: "short and more"}); got != "" {
		t.Fatalf("longer quote matched %q, want no match", got)
	}

	// Media tapbacks only match entries with attachments.
	textOnly := []indexedMessage{{ID: "m1", Body: "pic"}}
	if got := matchIndexedTapbackTarget(textOnly, tapback{Emoji: "👍", IsMedia: true}); got != "" {
		t.Fatalf("media tapback matched a text-only entry: %q", got)
	}

	// Carrier-truncated quotes still match after the ellipsis is normalized.
	long := []indexedMessage{{ID: "m1", Body: "a long body"}}
	if got := matchIndexedTapbackTarget(long, tapback{Emoji: "👍", TargetText: "a long…"}); got != "m1" {
		t.Fatalf("truncated quote match = %q, want m1", got)
	}
}

func TestTapbackEmojiID(t *testing.T) {
	tests := map[string]string{
		"👍":  "like",
		"❤️": "love",
		"👎":  "dislike",
		"😂":  "laugh",
		"‼️": "emphasis",
		"❓":  "question",
	}
	for emoji, want := range tests {
		if got := tapbackEmojiID(emoji); got != networkid.EmojiID(want) {
			t.Errorf("tapbackEmojiID(%q) = %q, want %q", emoji, got, want)
		}
	}
	if got := tapbackEmojiID("🌟"); got != networkid.EmojiID("🌟") {
		t.Errorf("tapbackEmojiID(unknown) = %q, want the emoji itself", got)
	}
}

func TestMatchTapbackTarget(t *testing.T) {
	messages := []*database.Message{
		{ID: "m4", Metadata: &MessageMetadata{Body: "see you at 8"}},
		{ID: "m3", Metadata: &MessageMetadata{Body: "see you at 8", HasAttachment: true}},
		{ID: "m2", Metadata: &MessageMetadata{Body: "an older message with a very long body that got truncated by the carrier", HasAttachment: true}},
		{ID: "m1", Metadata: &MessageMetadata{Body: "hello"}},
		{ID: "m0"},
		{ID: "m00", Metadata: &MessageMetadata{}},
	}

	tests := []struct {
		name string
		tb   tapback
		want networkid.MessageID
	}{
		{
			name: "exact body match returns newest",
			tb:   tapback{Emoji: "👍", TargetText: "see you at 8"},
			want: "m4",
		},
		{
			name: "truncated quote matches by prefix",
			tb:   tapback{Emoji: "👍", TargetText: "an older message with a very long body that got trunc…"},
			want: "m2",
		},
		{
			name: "media tapback returns newest attachment",
			tb:   tapback{Emoji: "👍", IsMedia: true},
			want: "m3",
		},
		{
			name: "unknown text finds nothing",
			tb:   tapback{Emoji: "👍", TargetText: "not in history"},
			want: "",
		},
		{
			name: "empty quote finds nothing",
			tb:   tapback{Emoji: "👍", TargetText: "  "},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := matchTapbackTarget(messages, tc.tb)
			if tc.want == "" {
				if target != nil {
					t.Fatalf("matchTapbackTarget() = %q, want no match", target.ID)
				}
				return
			}
			if target == nil {
				t.Fatalf("matchTapbackTarget() = nil, want %q", tc.want)
			}
			if target.ID != tc.want {
				t.Fatalf("matchTapbackTarget() = %q, want %q", target.ID, tc.want)
			}
		})
	}
}

func TestMatchIndexedTapbackTarget(t *testing.T) {
	messages := []indexedMessage{
		{ID: "m3", Body: "see you at 8", HasAttachment: true},
		{ID: "m2", Body: "see you at 8"},
		{ID: "m1", Body: "hello"},
	}
	if got := matchIndexedTapbackTarget(messages, tapback{Emoji: "👍", TargetText: "see you at 8"}); got != "m3" {
		t.Errorf("indexed text match = %q, want m3", got)
	}
	if got := matchIndexedTapbackTarget(messages, tapback{Emoji: "👍", IsMedia: true}); got != "m3" {
		t.Errorf("indexed media match = %q, want m3", got)
	}
	if got := matchIndexedTapbackTarget(messages, tapback{Emoji: "👍", TargetText: "missing"}); got != "" {
		t.Errorf("indexed miss = %q, want empty", got)
	}
}
