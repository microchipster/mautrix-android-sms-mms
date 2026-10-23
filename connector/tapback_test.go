// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
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
