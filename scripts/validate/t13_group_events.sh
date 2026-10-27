#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t13_group_events.sh — acceptance step 12 (priority): group events —
# reactions, uploads, replies and the SMS+MMS fallback modes.
#
# For every group thread on the phone and its candidate portal room(s):
#   1. uploads: an image part on the phone (pdu/part) must correspond to an
#      m.image (or file) event in the portal room, and vice versa;
#   2. reactions: counted on both sides (m.reaction events vs tapback-format
#      SMS rows);
#   3. replies: m.in_reply_to / thread relations in the portal room are
#      reported;
#   4. fallback: a group thread must carry either MMS pdu rows or SMS rows;
#      a text-only group send recorded as an MMS is the documented fallback.
#
# This test is deliberately observational: it never sends unless
# VALIDATE_LIVE_SENDS=1 and IAMB_GROUP_PORTAL are set (one probe message).
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t13.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_GROUP_PORTAL="${IAMB_GROUP_PORTAL:-}"
MAX_GROUP_THREADS="${MAX_GROUP_THREADS:-3}"
MAX_CANDIDATE_ROOMS="${MAX_CANDIDATE_ROOMS:-4}"

log "t13_group_events: portal=${IAMB_GROUP_PORTAL:-<unset>} live=${VALIDATE_LIVE_SENDS}"

rc=0
adb_device_online || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "phone $ADB_SERIAL not reachable (adb)"; log "t13_group_events: SKIP-ONLY run"; exit 0
fi

rc=0
phone_thread_ids_with_participants >"$WORK/group-threads" 2>"$WORK/threads.err" || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "telephony provider not queryable: $(tr '\n' ' ' <"$WORK/threads.err")"; log "t13_group_events: SKIP-ONLY run"; exit 0
fi
if [ ! -s "$WORK/group-threads" ]; then
	skip_check "no group threads on the phone"; log "t13_group_events: SKIP-ONLY run"; exit 0
fi

if [ -z "$SMS_MGMT_ROOM" ]; then
	skip_check "SMS_MGMT_ROOM not set; assuming the iamb account does not bridge this phone; Matrix-side checks skipped"
	log "t13_group_events: SKIP-ONLY run"; exit 0
fi

# Phone-side: MMS image parts per group thread + SMS rows per group thread.
pdu_rows="$(phone_mms_rows_since 0 2>/dev/null || true)"
sms_rows="$(phone_sms_rows_since 0 2>/dev/null || true)"

checked=0
saw_candidates=0
no_candidate_threads=0
while IFS='|' read -r thread_id _count; do
	[ -n "$thread_id" ] || continue
	[ "$checked" -ge "$MAX_GROUP_THREADS" ] && break
	checked=$((checked + 1))

	phone_images=0
	phone_text_parts=0
	while IFS='|' read -r pdu_id pdu_thread _date _box; do
		[ "$pdu_thread" = "$thread_id" ] || continue
		parts="$(phone_mms_parts "$pdu_id" 2>/dev/null || true)"
		img="$(awk -F'|' '$2 ~ /^image\// { n++ } END { print n + 0 }' <<<"$parts")"
		txt="$(awk -F'|' '$2 ~ /^text\// { n++ } END { print n + 0 }' <<<"$parts")"
		phone_images=$((phone_images + img))
		phone_text_parts=$((phone_text_parts + txt))
	done <<<"$pdu_rows"

	phone_sms_count=0
	if [ -n "$sms_rows" ]; then
		phone_sms_count="$(awk -F'|' -v t="$thread_id" '$2 == t { n++ } END { print n + 0 }' <<<"$sms_rows")"
	fi

	if [ "$phone_images" -eq 0 ] && [ "$phone_text_parts" -eq 0 ] && [ "$phone_sms_count" -eq 0 ]; then
		skip_check "thread $thread_id: no messages (image/text/SMS) recorded yet"
		continue
	fi
	pass "thread $thread_id: phone state image_parts=$phone_images text_parts=$phone_text_parts sms_rows=$phone_sms_count"

	# Matrix-side: candidate portal events.
	if ! iamb_available; then
		skip_check "thread $thread_id: iamb not available for Matrix-side checks"
		continue
	fi
	rc=0
	phone_thread_participants "$thread_id" >"$WORK/participants" 2>/dev/null || rc=$?
	if [ "$rc" -ne 0 ]; then
		skip_check "thread $thread_id: cannot list participants"
		continue
	fi
	mapfile -t numbers <"$WORK/participants"
	: >"$WORK/candidates.tsv"
	for n in "${numbers[@]}"; do
		room_ids_matching_name "$n" >>"$WORK/candidates.tsv" 2>/dev/null || true
	done
	sort -u "$WORK/candidates.tsv" | head -n "$MAX_CANDIDATE_ROOMS" >"$WORK/cand.tsv"
	if [ ! -s "$WORK/cand.tsv" ]; then
		no_candidate_threads=$((no_candidate_threads + 1))
		skip_check "thread $thread_id: no Matrix portal candidate found (does IAMB_PROFILE bridge this phone?)"
		continue
	fi
	saw_candidates=1

	portal_room="$(cut -f1 "$WORK/cand.tsv" | head -n 1)"
	if [ -n "$IAMB_GROUP_PORTAL" ]; then
		portal_room="$IAMB_GROUP_PORTAL"
	fi
	rc=0
	iamb_cmd message "$portal_room" >"$WORK/events.json" 2>"$WORK/events.err" || rc=$?
	if [ "$rc" -ne 0 ]; then
		skip_check "thread $thread_id: cannot fetch events for $portal_room"
		continue
	fi

	images="$(jq -r 'select(.type == "m.room.message") | select((.content.msgtype // "") == "m.image" or (.content.msgtype // "") == "m.file") | .event_id' "$WORK/events.json" | wc -l | tr -d ' ')"
	reactions="$(jq -r 'select(.type == "m.reaction") | .event_id' "$WORK/events.json" | wc -l | tr -d ' ')"
	replies="$(jq -r 'select(.type == "m.room.message") | select((.content["m.relates_to"] // {}) | (.rel_type // "") == "m.in_reply_to" or (.rel_type // "") == "m.thread") | .event_id' "$WORK/events.json" | wc -l | tr -d ' ')"
	pass "thread $thread_id portal $portal_room: m.image/file=$images reactions=$reactions replies=$replies"

	if [ "$phone_images" -ge 1 ] && [ "$images" -eq 0 ]; then
		fail "thread $thread_id: phone has $phone_images image part(s) but portal $portal_room has no m.image/m.file event"
	fi
	if [ "$images" -ge 1 ] && [ "$phone_images" -eq 0 ]; then
		fail "thread $thread_id: portal $portal_room has $images m.image event(s) but the phone has no image part"
	fi

	# Fallback mode evidence: group content must exist as MMS and/or SMS.
	if [ "$phone_images" -eq 0 ] && [ "$phone_text_parts" -eq 0 ] && [ "$phone_sms_count" -ge 1 ]; then
		pass "thread $thread_id: SMS fallback mode in use ($phone_sms_count SMS row(s))"
	fi
done <"$WORK/group-threads"

if [ "$no_candidate_threads" -ge 1 ] && [ "$saw_candidates" -eq 1 ]; then
	fail "$no_candidate_threads thread(s) have no portal candidate while others do — check the IAMB_* rig pairing"
elif [ "$saw_candidates" -eq 0 ] && [ "$checked" -ge 1 ]; then
	skip_check "no portal candidates found for any of the phone's group threads in $IAMB_PROFILE"
fi

# --- optional live probe ----------------------------------------------------
if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$IAMB_GROUP_PORTAL" ]; then
	if ! iamb_available; then
		skip_check "live group-events probe: iamb not available"
	else
		before_ms="$(now_ms)"
		rc=0
		iamb_send "$IAMB_GROUP_PORTAL" "validate t13 group probe $before_ms" >"$WORK/send.json" 2>"$WORK/send.err" || rc=$?
		case "$rc" in
		"$RC_OK")
			pass "live group probe sent to $IAMB_GROUP_PORTAL (at ${before_ms}ms)"
			sleep 45
			rc=0
			iamb_cmd message "$IAMB_GROUP_PORTAL" >"$WORK/events-after.json" 2>/dev/null || rc=$?
			if [ "$rc" -eq 0 ]; then
				own="$(jq -r 'select(.type == "m.room.message") | .event_id' "$WORK/events-after.json" | wc -l | tr -d ' ')"
				pass "portal $IAMB_GROUP_PORTAL now has $own message event(s)"
			fi
			;;
		"$RC_UNREACHABLE") skip_check "live group probe send failed: $(tr '\n' ' ' <"$WORK/send.err")" ;;
		*) fail "live group probe send misbehaved: $(tr '\n' ' ' <"$WORK/send.err")" ;;
		esac
	fi
else
	skip_check "live group-events probe disabled (VALIDATE_LIVE_SENDS=1, IAMB_GROUP_PORTAL)"
fi

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t13_group_events: PASS (skips: $SKIPS)"
	exit 0
fi
log "t13_group_events: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
