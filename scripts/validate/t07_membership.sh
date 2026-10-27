#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t07_membership.sh — acceptance step 6 (priority): membership changes stay in
# place — one room per thread (same channel id), current participants covered
# by the portal, and no own-number ghost.
#
# For every group thread on the phone:
#   1. candidate portals are resolved by participant number;
#   2. the bridge channel id must map to exactly one room id (no duplicates);
#   3. the portal display name / room name must mention at least one current
#      participant (STRICT=1 requires all of them; default 0 tolerates a
#      deliberate own-number/participant filter);
#   4. the own number (PHONE_NUMBER) must not be a ghost member.
#
# Live membership change (opt-in: VALIDATE_LIVE_SENDS=1 + IAMB_GROUP_ROOM):
#   5. one message from the group room must not create any new portal room and
#      must not change the channel id -> the membership update is in place.
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t07.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_GROUP_ROOM="${IAMB_GROUP_ROOM:-}"
PORTAL_STRICT_PARTICIPANTS="${PORTAL_STRICT_PARTICIPANTS:-0}"
MAX_GROUP_THREADS="${MAX_GROUP_THREADS:-3}"
MAX_CANDIDATE_ROOMS="${MAX_CANDIDATE_ROOMS:-6}"

log "t07_membership: ADB_SSH=${ADB_SSH:-<local>} PHONE_NUMBER=${PHONE_NUMBER:-<unset>} strict=$PORTAL_STRICT_PARTICIPANTS"

digits() { printf '%s' "$1" | tr -cd '0-9'; }

rc=0
adb_device_online || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "phone $ADB_SERIAL not reachable (adb)"; log "t07_membership: SKIP-ONLY run"; exit 0
fi
pass "phone $ADB_SERIAL is online"

rc=0
phone_thread_ids_with_participants >"$WORK/group-threads" 2>"$WORK/threads.err" || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "telephony provider not queryable: $(tr '\n' ' ' <"$WORK/threads.err")"; log "t07_membership: SKIP-ONLY run"; exit 0
fi
if [ ! -s "$WORK/group-threads" ]; then
	skip_check "no group threads on the phone"; log "t07_membership: SKIP-ONLY run"; exit 0
fi

if [ -z "$SMS_MGMT_ROOM" ]; then
	skip_check "SMS_MGMT_ROOM not set; assuming the iamb account does not bridge this phone; portal membership checks skipped"
	log "t07_membership: SKIP-ONLY run"; exit 0
fi
if ! iamb_available; then
	skip_check "iamb not available on $IAMB_SSH; cannot check portal membership"
	log "t07_membership: SKIP-ONLY run"; exit 0
fi

checked=0
saw_candidates=0
no_candidate_threads=0
while IFS='|' read -r thread_id _count; do
	[ -n "$thread_id" ] || continue
	[ "$checked" -ge "$MAX_GROUP_THREADS" ] && break
	checked=$((checked + 1))

	rc=0
	phone_thread_participants "$thread_id" >"$WORK/participants" 2>/dev/null || rc=$?
	if [ "$rc" -ne 0 ] || [ ! -s "$WORK/participants" ]; then
		skip_check "thread $thread_id: cannot list participants"
		continue
	fi
	mapfile -t numbers <"$WORK/participants"

	: >"$WORK/candidates.tsv"
	for n in "${numbers[@]}"; do
		room_ids_matching_name "$n" >>"$WORK/candidates.tsv" 2>/dev/null || true
	done
	sort -u "$WORK/candidates.tsv" | head -n "$MAX_CANDIDATE_ROOMS" >"$WORK/cand.tsv"

	: >"$WORK/ch.tsv"
	while IFS=$'\t' read -r room name; do
		[ -n "$room" ] || continue
		chan="$(portal_channel_id "$room" 2>/dev/null || true)"
		disp="$(portal_channel_displayname "$room" 2>/dev/null || true)"
		printf '%s\t%s\t%s\t%s\n' "$room" "${chan:-<unknown>}" "$disp" "$name" >>"$WORK/ch.tsv"
	done <"$WORK/cand.tsv"

	if [ ! -s "$WORK/ch.tsv" ]; then
		no_candidate_threads=$((no_candidate_threads + 1))
		skip_check "thread $thread_id: no candidate portal room found (does IAMB_PROFILE=$IAMB_PROFILE bridge this phone?)"
		continue
	fi
	saw_candidates=1

	# Duplicate channel ids: a channel id must map to exactly one room id.
	dups="$(awk -F'\t' '$2 != "<unknown>" { print $2 }' "$WORK/ch.tsv" | sort | uniq -d)"
	if [ -n "$dups" ]; then
		fail "thread $thread_id: channel id(s) mapped to several rooms: $dups"
	else
		pass "thread $thread_id: channel ids unique across $(wc -l <"$WORK/ch.tsv" | tr -d ' ') candidate(s)"
	fi

	# Participant coverage: strict=all, else at least one.
	best_room=""
	best_covered=0
	while IFS=$'\t' read -r room _chan disp name; do
		[ -n "$room" ] || continue
		covered=0
		for n in "${numbers[@]}"; do
			d="$(digits "$n")"
			hay="$(digits "$disp$name")"
			[[ "$hay" == *"$d"* ]] && covered=$((covered + 1))
		done
		if [ "$covered" -gt "$best_covered" ]; then
			best_covered="$covered"
			best_room="$room"
		fi
	done <"$WORK/ch.tsv"

	want_all="${#numbers[@]}"
	if [ "$best_covered" -ge "$want_all" ]; then
		pass "thread $thread_id: portal $best_room covers all $want_all participants"
	elif [ "$PORTAL_STRICT_PARTICIPANTS" = "1" ]; then
		fail "thread $thread_id: portal $best_room covers only $best_covered/$want_all participants (${numbers[*]})"
	elif [ "$best_covered" -ge 1 ]; then
		skip_check "thread $thread_id: portal $best_room covers $best_covered/$want_all participants (strict checks disabled)"
	else
		fail "thread $thread_id: no candidate room mentions any participant (${numbers[*]})"
	fi

	if [ -n "$PHONE_NUMBER" ] && [ -n "$best_room" ]; then
		rc=0
		assert_no_own_number_ghost "$best_room" "$PHONE_NUMBER" || rc=$?
		case "$rc" in
		"$RC_OK") pass "thread $thread_id: no own-number ghost in $best_room" ;;
		"$RC_UNREACHABLE") skip_check "thread $thread_id: ghost check unavailable" ;;
		*) fail "thread $thread_id: own number $PHONE_NUMBER is a ghost in $best_room" ;;
		esac
	else
		skip_check "thread $thread_id: PHONE_NUMBER not set; ghost check skipped"
	fi
done <"$WORK/group-threads"

if [ "$no_candidate_threads" -ge 1 ] && [ "$saw_candidates" -eq 1 ]; then
	fail "$no_candidate_threads thread(s) have no portal candidate while others do — check the IAMB_* rig pairing"
elif [ "$saw_candidates" -eq 0 ] && [ "$checked" -ge 1 ]; then
	skip_check "no portal candidates found for any of the phone's group threads in $IAMB_PROFILE"
fi

# --- live membership probe --------------------------------------------------
if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$IAMB_GROUP_ROOM" ]; then
	# Snapshot the portal rooms of every known group thread: the probe room may
	# text a thread that is not the first one, and the check must catch an
	# existing thread gaining a second portal (fracture) or losing a channel id.
	membership_snapshot_rooms() {
		local t_id p_rc=0
		: >"$WORK/live-rooms.tsv"
		while IFS='|' read -r t_id _count; do
			[ -n "$t_id" ] || continue
			p_rc=0
			phone_thread_participants "$t_id" >"$WORK/live-participants" 2>/dev/null || p_rc=$?
			[ "$p_rc" -eq 0 ] || continue
			[ -s "$WORK/live-participants" ] || continue
			mapfile -t numbers <"$WORK/live-participants"
			: >"$WORK/live-cand.tsv"
			for n in "${numbers[@]}"; do
				room_ids_matching_name "$n" >>"$WORK/live-cand.tsv" 2>/dev/null || true
			done
			LC_ALL=C sort -u "$WORK/live-cand.tsv" >>"$WORK/live-rooms.tsv"
		done <"$WORK/group-threads"
		cut -f1 "$WORK/live-rooms.tsv" | sed '/^$/d' | LC_ALL=C sort -u
	}
	channel_list() {
		local room
		while IFS= read -r room; do
			[ -n "$room" ] || continue
			portal_channel_id "$room" 2>/dev/null || true
		done
	}
	existing_threads="$(cut -d'|' -f1 "$WORK/group-threads" | sed '/^$/d')"
	before_rooms="$(membership_snapshot_rooms)"
	before_channels="$(channel_list <<<"$before_rooms" | sed '/^$/d')"
	before_ms="$(now_ms)"
	rc=0
	iamb_send "$IAMB_GROUP_ROOM" "validate t07 membership probe $before_ms" >"$WORK/send.json" 2>"$WORK/send.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		pass "membership probe sent from $IAMB_GROUP_ROOM (at ${before_ms}ms)"
		sleep 30
		after_rooms="$(membership_snapshot_rooms)"
		new_rooms="$(LC_ALL=C comm -13 <(printf '%s\n' "$before_rooms") <(printf '%s\n' "$after_rooms") | sed '/^$/d')"
		fractured=""
		created=""
		while IFS= read -r room; do
			[ -n "$room" ] || continue
			chan="$(portal_channel_id "$room" 2>/dev/null || true)"
			suffix="${chan##*-}"
			if [ -n "$chan" ] && printf '%s\n' "$existing_threads" | grep -qx "$suffix"; then
				fractured="$fractured $room"
			else
				created="$created $room"
			fi
		done <<<"$new_rooms"
		if [ -n "$fractured" ]; then
			fail "membership probe gave an existing thread a second portal:$fractured"
		else
			pass "membership probe kept one portal per existing thread (new conversation rooms:${created:- none})"
		fi
		after_channels="$(channel_list <<<"$after_rooms" | sed '/^$/d')"
		dups_after="$(printf '%s\n' "$after_channels" | LC_ALL=C sort | uniq -d | sed '/^$/d')"
		missing_channels="$(LC_ALL=C comm -23 <(printf '%s\n' "$before_channels" | LC_ALL=C sort -u | sed '/^$/d') <(printf '%s\n' "$after_channels" | LC_ALL=C sort -u | sed '/^$/d') | sed '/^$/d')"
		unique_after="$(printf '%s\n' "$after_channels" | LC_ALL=C sort -u | sed '/^$/d' | wc -l | tr -d ' ')"
		if [ -n "$dups_after" ]; then
			fail "membership probe duplicated channel id(s): $dups_after"
		elif [ -n "$missing_channels" ]; then
			fail "membership probe lost channel id(s): $missing_channels"
		else
			pass "membership probe kept every channel id ($unique_after unique), no duplicates"
		fi
		;;
	"$RC_UNREACHABLE") skip_check "membership probe send failed: $(tr '\n' ' ' <"$WORK/send.err")" ;;
	*) fail "membership probe send misbehaved: $(tr '\n' ' ' <"$WORK/send.err")" ;;
	esac
else
	skip_check "live membership probe disabled (set VALIDATE_LIVE_SENDS=1 and IAMB_GROUP_ROOM)"
fi

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t07_membership: PASS (skips: $SKIPS)"
	exit 0
fi
log "t07_membership: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
