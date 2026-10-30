// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"maunium.net/go/mautrix/bridgev2/commands"
)

// RoomNameSuffixDefault is the room-name suffix used when no per-number label
// applies. Every phone-conversation room name ends with a suffix so the
// SMS/MMS rooms are easy to tell apart from the other bridges' rooms.
const RoomNameSuffixDefault = "(sms)"

// SanitizeNumberLabel normalizes a user-supplied number label for use in a
// room-name suffix: lowercased, surrounding parentheses stripped, inner runs
// of whitespace/dashes/dots collapsed into single dashes, only a-z, 0-9 and
// dashes kept, max 24 characters.
func SanitizeNumberLabel(raw string) string {
	label := strings.ToLower(strings.TrimSpace(raw))
	label = strings.Trim(label, "()")
	var b strings.Builder
	lastDash := false
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == ' ' || r == '-' || r == '_' || r == '.':
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 24 {
		out = strings.Trim(out[:24], "-")
	}
	return out
}

// RoomNameSuffix computes the room-name suffix for a conversation on the SIM
// with the given subscription id: RoomNameSuffixDefault, or "(sms-<label>)"
// when the subscription's phone number has a label assigned.
func RoomNameSuffix(subID int, subs map[int]SubscriptionInfo, labels map[string]string) string {
	if info, ok := subs[subID]; ok {
		if canonical := CanonicalAddress(info.PhoneNumber); canonical != "" {
			if label := labels[canonical]; label != "" {
				return "(sms-" + label + ")"
			}
		}
	}
	return RoomNameSuffixDefault
}

// roomNameSuffix is the MyNetworkClient wrapper.
func (nc *MyNetworkClient) roomNameSuffix(subID int) string {
	if nc == nil || nc.login == nil {
		return RoomNameSuffixDefault
	}
	meta, _ := nc.login.Metadata.(*LoginMetadata)
	if meta == nil {
		return RoomNameSuffixDefault
	}
	return RoomNameSuffix(subID, meta.Subscriptions, meta.NumberLabels)
}

// numberLabelTarget is one of the phone's own numbers and the SIM it belongs to.
type numberLabelTarget struct {
	SubID  int
	Number string
}

// numberLabelTargets returns the phone's own numbers (canonical, ascending by
// subscription id), deduplicated.
func numberLabelTargets(subs map[int]SubscriptionInfo) []numberLabelTarget {
	targets := make([]numberLabelTarget, 0, len(subs))
	seen := make(map[string]struct{}, len(subs))
	for subID, info := range subs {
		canonical := CanonicalAddress(info.PhoneNumber)
		if canonical == "" {
			continue
		}
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		targets = append(targets, numberLabelTarget{SubID: subID, Number: canonical})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].SubID < targets[j].SubID })
	return targets
}

// resolveNumberLabelTarget accepts either one of the phone's numbers or a
// subscription id.
func resolveNumberLabelTarget(arg string, targets []numberLabelTarget) (numberLabelTarget, bool) {
	if canonical := CanonicalAddress(arg); canonical != "" {
		for _, target := range targets {
			if target.Number == canonical {
				return target, true
			}
		}
	}
	if subID, err := strconv.Atoi(strings.TrimSpace(arg)); err == nil {
		for _, target := range targets {
			if target.SubID == subID {
				return target, true
			}
		}
	}
	return numberLabelTarget{}, false
}

// numberLabelHint describes the known numbers for command replies.
func numberLabelHint(meta *LoginMetadata, targets []numberLabelTarget) string {
	if len(targets) == 0 {
		return " No phone numbers have been reported yet; the app sends them with its next heartbeat."
	}
	parts := make([]string, 0, len(targets))
	for _, target := range targets {
		label := meta.NumberLabels[target.Number]
		if label == "" {
			label = "(none)"
		}
		parts = append(parts, fmt.Sprintf("%s [sub %d, label %s]", target.Number, target.SubID, label))
	}
	return " Numbers: " + strings.Join(parts, ", ") + "."
}

// maybePromptNumberLabels asks the user once to label the phone's numbers when
// more than one number is known and none is labeled yet. The prompt goes to
// the management room; labels are assigned with the number-label command.
func (nc *MyNetworkClient) maybePromptNumberLabels(ctx context.Context) {
	if nc == nil || nc.login == nil || nc.bridge == nil {
		return
	}
	meta, _ := nc.login.Metadata.(*LoginMetadata)
	if meta == nil || meta.NumberLabelsPrompted {
		return
	}
	targets := numberLabelTargets(meta.Subscriptions)
	if len(targets) < 2 {
		// One number needs no label; a second SIM appearing later still
		// triggers the prompt.
		return
	}
	meta.NumberLabelsPrompted = true
	if err := nc.login.Save(ctx); err != nil {
		nc.log.Warn().Err(err).Msg("Failed to save the number-label prompt state")
		return
	}
	if len(meta.NumberLabels) > 0 {
		return
	}
	numbers := make([]string, 0, len(targets))
	for _, target := range targets {
		numbers = append(numbers, target.Number)
	}
	sendNotice(nc.bridge.Bot, nc.login.User.ManagementRoom, fmt.Sprintf(
		"This bridge sees %d phone numbers: %s\n\n"+
			"Conversation room names currently end in \"(sms)\". To tell the numbers apart, "+
			"give each one a label, e.g.:\n\n"+
			"  number-label %s personal\n"+
			"  number-label %s work\n\n"+
			"Run number-labels to see the current setup. Labels are local to the bridge "+
			"and rooms are renamed automatically.",
		len(numbers), strings.Join(numbers, ", "), numbers[0], numbers[1]))
}

// CommandNumberLabel assigns or clears a room-name suffix label for one of the
// phone's own numbers.
var CommandNumberLabel = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No login found for this user.")
			return
		}
		meta, _ := login.Metadata.(*LoginMetadata)
		if meta == nil {
			ce.Reply("Login metadata is unavailable.")
			return
		}
		targets := numberLabelTargets(meta.Subscriptions)
		if len(ce.Args) == 0 {
			ce.Reply("Usage: number-label <number|sub_id> <label|--clear>%s", numberLabelHint(meta, targets))
			return
		}
		target, ok := resolveNumberLabelTarget(ce.Args[0], targets)
		if !ok {
			ce.Reply("Unknown phone number or subscription %q.%s", ce.Args[0], numberLabelHint(meta, targets))
			return
		}
		if len(ce.Args) < 2 {
			ce.Reply("Usage: number-label %s <label|--clear>", target.Number)
			return
		}
		clear := ce.Args[1] == "--clear"
		if clear {
			delete(meta.NumberLabels, target.Number)
		} else {
			label := SanitizeNumberLabel(strings.Join(ce.Args[1:], " "))
			if label == "" {
				ce.Reply("The label %q is empty after normalization; use letters, digits, spaces or dashes.",
					strings.Join(ce.Args[1:], " "))
				return
			}
			if meta.NumberLabels == nil {
				meta.NumberLabels = make(map[string]string)
			}
			meta.NumberLabels[target.Number] = label
		}
		if err := login.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save the label: %v", err)
			return
		}
		suffix := RoomNameSuffix(target.SubID, meta.Subscriptions, meta.NumberLabels)
		if clear {
			ce.Reply("Cleared the label for %s; its rooms end in %s again.", target.Number, suffix)
		} else {
			ce.Reply("Label for %s is now %q; its rooms end in %s. Rooms are being renamed.",
				target.Number, meta.NumberLabels[target.Number], suffix)
		}
		if client, ok := login.Client.(*MyNetworkClient); ok {
			client.refreshThreadsAndCatchup(ce.Ctx)
		}
	},
	Name: "number-label",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Label one of the phone's own numbers for room-name suffixes (e.g. personal, work)",
		Args:        "<number|sub_id> <label|--clear>",
	},
	RequiresLogin: true,
}

// CommandNumberLabels lists the phone's numbers and their labels.
var CommandNumberLabels = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No login found for this user.")
			return
		}
		meta, _ := login.Metadata.(*LoginMetadata)
		if meta == nil {
			ce.Reply("Login metadata is unavailable.")
			return
		}
		targets := numberLabelTargets(meta.Subscriptions)
		if len(targets) == 0 {
			ce.Reply("No phone numbers have been reported yet; the app sends them with its next heartbeat.")
			return
		}
		lines := make([]string, 0, len(targets))
		for _, target := range targets {
			label := meta.NumberLabels[target.Number]
			if label == "" {
				label = "(none)"
			}
			lines = append(lines, fmt.Sprintf("- %s (sub %d): %s -> rooms end in %s",
				target.Number, target.SubID, label, RoomNameSuffix(target.SubID, meta.Subscriptions, meta.NumberLabels)))
		}
		ce.Reply("Number labels:\n%s\n\nSet one with: number-label <number> <label>", strings.Join(lines, "\n"))
	},
	Name:    "number-labels",
	Aliases: []string{"list-number-labels"},
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "List the phone's numbers and their room-name suffix labels",
	},
	RequiresLogin: true,
}
