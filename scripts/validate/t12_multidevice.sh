#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t12_multidevice.sh — acceptance step 11: multi-device / multi-participant
# coverage. Checks the bridge's device inventory and verifies that group
# threads of every observed size (2..4 participants) map to portals without
# duplication. Extra iamb profiles (JMP_PROFILE) are reported when configured.
#
# Bridge device list: SMS_MGMT_ROOM + `!sms devices` via iamb.
# Group combos: phone provider threads with 2, 3 or 4 participants; each combo
# must have exactly one candidate portal (channel ids unique).
#
# Unreachable components SKIP (exit 0); reachable-but-wrong answers FAIL.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$SCRIPT_DIR/lib.sh"

FAILURES=0
SKIPS=0

pass() { log "PASS: $*"; }
skip_check() { SKIPS=$((SKIPS + 1)); skip "$*"; }
fail() { FAILURES=$((FAILURES + 1)); printf '[validate] FAIL: %s\n' "$*" >&2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t12.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

MAX_CANDIDATE_ROOMS="${MAX_CANDIDATE_ROOMS:-6}"

log "t12_multidevice: SMS_MGMT_ROOM=${SMS_MGMT_ROOM:-<unset>} JMP_PROFILE=${JMP_PROFILE:-<unset>}"

# --- 1. bridge device inventory ---------------------------------------------
if [ -z "$SMS_MGMT_ROOM" ]; then
	skip_check "SMS_MGMT_ROOM not set; bridge device inventory skipped"
elif ! iamb_available; then
	skip_check "iamb not available on $IAMB_SSH; bridge device inventory skipped"
else
	rc=0
	iamb_send_wait "$SMS_MGMT_ROOM" "!sms devices" 60 >"$WORK/devices.json" 2>"$WORK/devices.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		body="$(jq -r '.wait.replies[0].body // ""' "$WORK/devices.json")"
		# Device entries are the "* `<device_id>` …" lines; ignore backticked
		# help text such as `unpair <device_id|all>`.
		devices="$(printf '%s\n' "$body" | grep -cE '^\* ' | tr -d ' ')"
		device_ids="$(printf '%s\n' "$body" | grep -E '^\* ' | grep -oE '`[^`]+`' | sed 's/`//g' | tr '\n' ' ' | sed 's/ $//')"
		if [ "${devices:-0}" -ge 1 ]; then
			pass "bridge knows $devices paired device entr(y|ies): $device_ids"
		else
			fail "!sms devices lists no paired devices"
		fi
		;;
	"$RC_UNREACHABLE") skip_check "!sms devices failed: $(tr '\n' ' ' <"$WORK/devices.err")" ;;
	*) fail "!sms devices misbehaved: $(tr '\n' ' ' <"$WORK/devices.err")" ;;
	esac
fi

# --- 2. group combos by participant count -----------------------------------
rc=0
phone_thread_ids_with_participants >"$WORK/threads.tsv" 2>"$WORK/threads.err" || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "telephony provider not queryable: $(tr '\n' ' ' <"$WORK/threads.err")"
	log "t12_multidevice: summary"
	[ "$FAILURES" -eq 0 ] && exit 0 || exit 1
fi

saw_candidates=0
no_candidate_combos=0
combos_enabled=1
if [ -z "$SMS_MGMT_ROOM" ]; then
	skip_check "SMS_MGMT_ROOM not set; assuming the iamb account does not bridge this phone; combo portal checks skipped"
	combos_enabled=0
fi
for size in 2 3 4; do
	[ "$combos_enabled" = "1" ] || break
	combo_threads="$(awk -F'|' -v s="$size" '$2 == s { print $1 }' "$WORK/threads.tsv" | head -n 3)"
	if [ -z "$combo_threads" ]; then
		skip_check "no observed group thread with exactly $size participants (local number + 1..3 friends)"
		continue
	fi
	if ! iamb_available; then
		skip_check "$size-participant combos: iamb not available for portal checks"
		continue
	fi
	while IFS= read -r thread_id; do
		[ -n "$thread_id" ] || continue
		rc=0
		phone_thread_participants "$thread_id" >"$WORK/participants" 2>/dev/null || rc=$?
		if [ "$rc" -ne 0 ]; then
			skip_check "combo thread $thread_id: cannot list participants"
			continue
		fi
		mapfile -t numbers <"$WORK/participants"

		# Exact resolution first: the bridge channel id ends in the telephony
		# thread id, so a combo cannot be mistaken for another bridge's room
		# that merely names the same numbers.
		mapfile -t channel_rooms < <(portal_rooms_for_thread_channel "$thread_id" "${numbers[@]}")
		if [ "${#channel_rooms[@]}" -gt 1 ]; then
			fail "combo thread $thread_id: channel id ending -$thread_id maps to several rooms: ${channel_rooms[*]}"
		elif [ "${#channel_rooms[@]}" -eq 1 ]; then
			saw_candidates=1
			pass "combo thread $thread_id ($size participants): exactly one portal for the telephony thread (${channel_rooms[0]})"
			continue
		fi

		: >"$WORK/candidates.tsv"
		for n in "${numbers[@]}"; do
			room_ids_matching_name "$n" >>"$WORK/candidates.tsv" 2>/dev/null || true
		done
		sort -u "$WORK/candidates.tsv" | head -n "$MAX_CANDIDATE_ROOMS" >"$WORK/cand.tsv"
		if [ ! -s "$WORK/cand.tsv" ]; then
			no_candidate_combos=$((no_candidate_combos + 1))
			skip_check "combo thread $thread_id ($size participants): no candidate portal room (does IAMB_PROFILE bridge this phone?)"
			continue
		fi
		saw_candidates=1
		known=0
		dups=""
		: >"$WORK/ch.tsv"
		while IFS=$'\t' read -r room _name; do
			[ -n "$room" ] || continue
			chan="$(portal_channel_id "$room" 2>/dev/null || true)"
			[ -n "$chan" ] && { known=$((known + 1)); printf '%s\n' "$chan" >>"$WORK/ch.tsv"; }
		done <"$WORK/cand.tsv"
		dups="$(sort "$WORK/ch.tsv" | uniq -d)"
		if [ -n "$dups" ]; then
			fail "combo thread $thread_id: duplicate channel id(s) $dups"
		elif [ "$known" -ge 1 ]; then
			pass "combo thread $thread_id ($size participants): $known portal channel(s), all unique"
		else
			skip_check "combo thread $thread_id ($size participants): no channel id readable"
		fi
	done <<<"$combo_threads"
done

if [ "$no_candidate_combos" -ge 1 ] && [ "$saw_candidates" -eq 1 ]; then
	fail "$no_candidate_combos combo thread(s) have no portal candidate while others do — check the IAMB_* rig pairing"
elif [ "$saw_candidates" -eq 0 ] && [ "$no_candidate_combos" -ge 1 ]; then
	skip_check "no portal candidates found for any observed combo in $IAMB_PROFILE"
fi

# --- 3. extra iamb profile (optional) ---------------------------------------
if [ -n "$JMP_PROFILE" ]; then
	rc=0
	ssh -n "$IAMB_SSH" "$IAMB_BIN -P $(shell_quote "$JMP_PROFILE") rooms . --format json" >"$WORK/jmp.json" 2>"$WORK/jmp.err" || rc=$?
	if [ "$rc" -ne 0 ]; then
		skip_check "iamb profile $JMP_PROFILE not reachable: $(tr '\n' ' ' <"$WORK/jmp.err")"
	else
		rooms="$(jq -r '.matches | length' "$WORK/jmp.json" 2>/dev/null || echo 0)"
		pass "iamb profile $JMP_PROFILE reachable ($rooms joined rooms)"
	fi
else
	skip_check "JMP_PROFILE not set; extra iamb profile check skipped"
fi

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t12_multidevice: PASS (skips: $SKIPS)"
	exit 0
fi
log "t12_multidevice: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
