// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"regexp"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
)

// iPhone (iMessage) tapbacks reach an SMS bridge as ordinary text messages:
//
//	Liked “see you at 8”
//	Loved an image
//	Removed a like from “see you at 8”
//
// Quoted text uses curly quotes, but some carriers/gateways rewrite them to
// straight quotes, so both are accepted. This file recognizes those texts and
// converts them into Matrix reactions, falling back to a plain text message
// when the referenced message cannot be found.

// tapbackAddedEmoji maps the tapback name used when a tapback is added.
var tapbackAddedEmoji = map[string]string{
	"loved":      "❤️",
	"liked":      "👍",
	"disliked":   "👎",
	"laughed at": "😂",
	"emphasized": "‼️",
	"questioned": "❓",
}

// tapbackRemovedEmoji maps the noun used when a tapback is removed.
var tapbackRemovedEmoji = map[string]string{
	"love":     "❤️",
	"heart":    "❤️",
	"like":     "👍",
	"dislike":  "👎",
	"laugh":    "😂",
	"emphasis": "‼️",
	"question": "❓",
}

// tapbackShortcodes gives each tapback emoji a stable, text-only ID (emoji
// presentation selectors and skin tones have no place in reaction IDs).
var tapbackShortcodes = map[string]string{
	"❤️": "love",
	"👍":  "like",
	"👎":  "dislike",
	"😂":  "laugh",
	"‼️": "emphasis",
	"❓":  "question",
}

var (
	tapbackAddedTextRegex    = regexp.MustCompile(`^(Loved|Liked|Disliked|Laughed at|Emphasized|Questioned) [“"](.*)[”"]$`)
	tapbackAddedMediaRegex   = regexp.MustCompile(`^(Loved|Liked|Disliked|Laughed at|Emphasized|Questioned) (?:an? [A-Za-z ]+)$`)
	tapbackRemovedTextRegex  = regexp.MustCompile(`^Removed (?:a|an) (like|love|heart|dislike|laugh|emphasis|question) from [“"](.*)[”"]$`)
	tapbackRemovedMediaRegex = regexp.MustCompile(`^Removed (?:a|an) (like|love|heart|dislike|laugh|emphasis|question) from an? [A-Za-z ]+$`)
)

// tapback is a parsed iMessage tapback.
type tapback struct {
	// Emoji is the Matrix reaction emoji the tapback maps to.
	Emoji string
	// TargetText is the quoted original message body. Empty for media tapbacks.
	TargetText string
	// IsMedia marks tapbacks that refer to an attachment ("Liked an image").
	IsMedia bool
	// IsRemoval marks tapbacks that remove a previously added tapback.
	IsRemoval bool
}

// parseTapback recognizes an iMessage tapback in a message body.
// It returns false for any body that is not a tapback.
func parseTapback(body string) (tapback, bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return tapback{}, false
	}

	if match := tapbackRemovedTextRegex.FindStringSubmatch(body); match != nil {
		if emoji, ok := tapbackRemovedEmoji[strings.ToLower(match[1])]; ok {
			return tapback{Emoji: emoji, TargetText: match[2], IsRemoval: true}, true
		}
		return tapback{}, false
	}
	if match := tapbackRemovedMediaRegex.FindStringSubmatch(body); match != nil {
		if emoji, ok := tapbackRemovedEmoji[strings.ToLower(match[1])]; ok {
			return tapback{Emoji: emoji, IsMedia: true, IsRemoval: true}, true
		}
		return tapback{}, false
	}
	if match := tapbackAddedTextRegex.FindStringSubmatch(body); match != nil {
		if emoji, ok := tapbackAddedEmoji[strings.ToLower(match[1])]; ok {
			return tapback{Emoji: emoji, TargetText: match[2]}, true
		}
		return tapback{}, false
	}
	if match := tapbackAddedMediaRegex.FindStringSubmatch(body); match != nil {
		if emoji, ok := tapbackAddedEmoji[strings.ToLower(match[1])]; ok {
			return tapback{Emoji: emoji, IsMedia: true}, true
		}
		return tapback{}, false
	}
	return tapback{}, false
}

// tapbackEmojiID returns the network emoji ID for a tapback emoji.
func tapbackEmojiID(emoji string) networkid.EmojiID {
	if shortcode, ok := tapbackShortcodes[emoji]; ok {
		return networkid.EmojiID(shortcode)
	}
	return networkid.EmojiID(emoji)
}

// tapbackLookback is how many recent portal messages are scanned when looking
// for the message a tapback refers to.
const tapbackLookback = 100

// indexedMessage is the in-memory tapback matching index entry for one bridged
// message.
type indexedMessage struct {
	ID            networkid.MessageID
	Body          string
	HasAttachment bool
}

// indexMessage records a bridged message so later tapbacks can find it without
// a database round trip.
func (nc *MyNetworkClient) indexMessage(portalKey networkid.PortalKey, msg indexedMessage) {
	if nc == nil || msg.ID == "" {
		return
	}
	nc.msgIndexMu.Lock()
	defer nc.msgIndexMu.Unlock()
	if nc.msgIndex == nil {
		nc.msgIndex = make(map[networkid.PortalKey][]indexedMessage)
	}
	entries := nc.msgIndex[portalKey]
	// Newest first; drop duplicates of the same message (updates).
	entries = append([]indexedMessage{msg}, entries...)
	if len(entries) > tapbackLookback {
		entries = entries[:tapbackLookback]
	}
	nc.msgIndex[portalKey] = entries
}

func (nc *MyNetworkClient) indexedMessages(portalKey networkid.PortalKey) []indexedMessage {
	if nc == nil {
		return nil
	}
	nc.msgIndexMu.Lock()
	defer nc.msgIndexMu.Unlock()
	entries := nc.msgIndex[portalKey]
	out := make([]indexedMessage, len(entries))
	copy(out, entries)
	return out
}

// matchTapbackTarget finds the most recent message a tapback refers to.
// Messages are expected newest-first, as returned by GetLastNInPortal.
//
// Text tapbacks quote the original body, but SMS gateways may truncate long
// messages, so an exact match is preferred and a prefix match is accepted as a
// fallback. Media tapbacks ("Liked an image") match the newest attachment.
func matchTapbackTarget(messages []*database.Message, tb tapback) *database.Message {
	quoted := normalizeTapbackQuotedText(tb.TargetText)
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		meta, ok := msg.Metadata.(*MessageMetadata)
		if !ok || meta == nil {
			continue
		}
		if tb.IsMedia {
			if meta.HasAttachment {
				return msg
			}
			continue
		}
		body := strings.TrimSpace(meta.Body)
		if body == "" || quoted == "" {
			continue
		}
		if body == quoted || strings.HasPrefix(body, quoted) {
			return msg
		}
	}
	return nil
}

// matchIndexedTapbackTarget is the in-memory equivalent of matchTapbackTarget.
func matchIndexedTapbackTarget(messages []indexedMessage, tb tapback) networkid.MessageID {
	quoted := normalizeTapbackQuotedText(tb.TargetText)
	for _, msg := range messages {
		if msg.ID == "" {
			continue
		}
		if tb.IsMedia {
			if msg.HasAttachment {
				return msg.ID
			}
			continue
		}
		body := strings.TrimSpace(msg.Body)
		if body == "" || quoted == "" {
			continue
		}
		if body == quoted || strings.HasPrefix(body, quoted) {
			return msg.ID
		}
	}
	return ""
}

// normalizeTapbackQuotedText strips trailing ellipses that gateways add when
// they truncate the quoted message.
func normalizeTapbackQuotedText(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimSuffix(text, "…")
	text = strings.TrimSuffix(text, "...")
	return strings.TrimSpace(text)
}

// findTapbackTarget locates the message a tapback refers to, first in the
// in-memory index (populated as messages are bridged) and then in the bridge
// database (which stores MessageMetadata.Body for every bridged message).
func (nc *MyNetworkClient) findTapbackTarget(ctx context.Context, portalKey networkid.PortalKey, tb tapback) networkid.MessageID {
	if target := matchIndexedTapbackTarget(nc.indexedMessages(portalKey), tb); target != "" {
		return target
	}
	if nc.bridge == nil || nc.bridge.DB == nil {
		return ""
	}
	messages, err := nc.bridge.DB.Message.GetLastNInPortal(ctx, portalKey, tapbackLookback)
	if err != nil {
		nc.log.Warn().Err(err).Msg("Failed to load recent messages for tapback matching")
		return ""
	}
	if target := matchTapbackTarget(messages, tb); target != nil {
		return target.ID
	}
	return ""
}

// handleIncomingTapback converts an incoming tapback into a Matrix reaction.
// It reports whether the tapback was handled; when the target message is not
// found the caller should forward the original text instead, so no information
// is lost.
func (nc *MyNetworkClient) handleIncomingTapback(ctx context.Context, portalKey networkid.PortalKey, sender networkid.UserID, tb tapback, timestamp time.Time) bool {
	if nc.bridge == nil || nc.bridge.DB == nil || sender == "" {
		return false
	}
	target := nc.findTapbackTarget(ctx, portalKey, tb)
	if target == "" {
		nc.log.Debug().
			Str("portal_id", string(portalKey.ID)).
			Bool("media", tb.IsMedia).
			Bool("removal", tb.IsRemoval).
			Str("target_text", tb.TargetText).
			Msg("Tapback target not found; forwarding as a regular message")
		return false
	}

	eventSender := bridgev2.EventSender{Sender: sender}

	// Keep the same portal-resync behavior as regular messages, so the portal
	// exists and has up-to-date metadata even if this is the first event.
	nc.bridge.QueueRemoteEvent(nc.login, &simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventChatResync,
			PortalKey:    portalKey,
			CreatePortal: true,
			Timestamp:    timestamp,
		},
		ChatInfo: nil,
	})

	reactionItems := make([]*bridgev2.BackfillReaction, 0, 1)
	if !tb.IsRemoval {
		reactionItems = append(reactionItems, &bridgev2.BackfillReaction{
			Timestamp: timestamp,
			Sender:    eventSender,
			EmojiID:   tapbackEmojiID(tb.Emoji),
			Emoji:     tb.Emoji,
		})
	}

	// HasAllReactions makes this an authoritative snapshot for the sender:
	// an added tapback replaces any previous one, a removal clears it.
	nc.bridge.QueueRemoteEvent(nc.login, &simplevent.ReactionSync{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReactionSync,
			PortalKey: portalKey,
			Sender:    eventSender,
			Timestamp: timestamp,
		},
		TargetMessage: target,
		Reactions: &bridgev2.ReactionSyncData{
			Users: map[networkid.UserID]*bridgev2.ReactionSyncUser{
				sender: {
					Reactions:       reactionItems,
					HasAllReactions: true,
				},
			},
		},
	})

	nc.log.Info().
		Str("portal_id", string(portalKey.ID)).
		Str("sender", string(sender)).
		Str("target_message_id", string(target)).
		Str("emoji", tb.Emoji).
		Bool("removal", tb.IsRemoval).
		Msg("Converted tapback into Matrix reaction")
	return true
}
