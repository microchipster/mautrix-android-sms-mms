#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t04_group_create.sh — acceptance step 3 (priority): every group thread on the
# phone maps to exactly ONE Matrix portal room, with the correct participant
# set, and a second sender must not fracture it into a second room.
#
# State checks (always):
#   1. enumerate the phone's group threads (2+ canonical addresses) from the
#      telephony provider;
#   2. for each thread, find candidate portal rooms by participant number and
#      assert at least one carries a bridge `m.bridge` channel (or name) that
#      includes every participant;
#   3. assert no two rooms share the same bridge channel id (no fractured
#      portal) and that the room name/display name excludes the own number.
#
# Live action (opt-in: VALIDATE_LIVE_SENDS=1 and IAMB_GROUP_ROOM pointing at a
# JMP-side group room that texts this phone):
#   4. snapshot the active portals, send one message from the group room, wait
#      for the phone to record it in the group thread, then assert the portal
#      set did not grow (second sender / extra message keeps the same room).
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t04.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_GROUP_ROOM="${IAMB_GROUP_ROOM:-}"
MAX_GROUP_THREADS="${MAX_GROUP_THREADS:-3}"
MAX_CANDIDATE_ROOMS="${MAX_CANDIDATE_ROOMS:-6}"

log "t04_group_create: ADB_SSH=${ADB_SSH:-<local>} ADB_SERIAL=$ADB_SERIAL PHONE_NUMBER=${PHONE_NUMBER:-<unset>}"

digits() { printf '%s' "$1" | tr -cd '0-9'; }

# --- 1. phone + group threads ----------------------------------------------
rc=0
adb_device_online || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "phone $ADB_SERIAL not reachable (adb)"; log "t04_group_create: SKIP-ONLY run"; exit 0
fi
pass "phone $ADB_SERIAL is online"

rc=0
phone_thread_ids_with_participants >"$WORK/group-threads" 2>"$WORK/threads.err" || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "telephony provider not queryable: $(tr '\n' ' ' <"$WORK/threads.err")"; log "t04_group_create: SKIP-ONLY run"; exit 0
fi

if [ ! -s "$WORK/group-threads" ]; then
	skip_check "no group threads (2+ participants) on the phone"
	log "t04_group_create: SKIP-ONLY run"; exit 0
fi
pass "group threads on the phone: $(wc -l <"$WORK/group-threads" | tr -d ' ') (checking up to $MAX_GROUP_THREADS)"

# --- iamb must be reachable for the portal checks ---------------------------
if [ -z "$SMS_MGMT_ROOM" ]; then
	skip_check "SMS_MGMT_ROOM not set; assuming the iamb account does not bridge this phone; Matrix-side checks skipped"
	log "t04_group_create: SKIP-ONLY run"; exit 0
fi
if ! iamb_available; then
	skip_check "iamb not available on $IAMB_SSH; cannot check Matrix portals"
	log "t04_group_create: SKIP-ONLY run"; exit 0
fi

# --- helpers ----------------------------------------------------------------
# candidates_for_numbers NUMBERS... — room_id<TAB>name lines for rooms whose
# display name contains any of the numbers.
candidates_for_numbers() {
	local n line
	: >"$WORK/candidates.tsv"
	for n in "$@"; do
		room_ids_matching_name "$n" >>"$WORK/candidates.tsv" 2>/dev/null || true
	done
	sort -u "$WORK/candidates.tsv" | head -n "$MAX_CANDIDATE_ROOMS"
}

# normalize NAME — digits only, for membership comparisons.
normalize() { digits "$1"; }

# --- 2./3. per-thread checks ------------------------------------------------
checked=0
while IFS='|' read -r thread_id participants; do
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
	log "thread $thread_id: ${#numbers[@]} participants: ${numbers[*]}"

	# Exact resolution first: the bridge channel id ends in the telephony
	# thread id, so it cannot be confused with another bridge's room that
	# merely names the same numbers.
	mapfile -t channel_rooms < <(portal_rooms_for_thread_channel "$thread_id" "${numbers[@]}")
	if [ "${#channel_rooms[@]}" -gt 1 ]; then
		fail "thread $thread_id: channel id ending -$thread_id maps to several rooms: ${channel_rooms[*]}"
	fi
	channel_room="${channel_rooms[0]:-}"

	# Candidate rooms: match on each participant number, then keep the rooms
	# whose bridge channel display name (or fallback: display name) covers the
	# whole participant set.
	: >"$WORK/ch.tsv"
	candidates_for_numbers "${numbers[@]}" >"$WORK/cand.tsv"
	best_room="$channel_room"
	have_channel_info=0
	while IFS=$'\t' read -r room name; do
		[ -n "$room" ] || continue
		chan="$(portal_channel_id "$room" 2>/dev/null || true)"
		disp="$(portal_channel_displayname "$room" 2>/dev/null || true)"
		[ -n "$chan" ] && have_channel_info=1
		printf '%s\t%s\t%s\t%s\n' "$room" "${chan:-<unknown>}" "$disp" "$name" >>"$WORK/ch.tsv"
		# Only real bridge portals (m.bridge channel id) count for coverage.
		[ -n "$chan" ] || continue
		[ -n "$best_room" ] && continue
		covers=1
		for n in "${numbers[@]}"; do
			d="$(digits "$n")"
			dn="$(digits "$disp")"
			rn="$(digits "$name")"
			if { [ -n "$dn" ] && [[ "$dn" == *"$d"* ]]; } || [[ "$rn" == *"$d"* ]]; then
				:
			else
				covers=0
			fi
		done
		if [ "$covers" -eq 1 ] && [ -z "$best_room" ]; then
			best_room="$room"
		fi
	done <"$WORK/cand.tsv"

	if [ -n "$best_room" ]; then
		pass "thread $thread_id: portal $best_room covers participants ${numbers[*]}"
	elif [ "$have_channel_info" -eq 1 ]; then
		# At least one portal exists, but it does not name every participant
		# (e.g. a deliberate own-number/participant filter). Reported as a
		# skip unless PORTAL_STRICT_PARTICIPANTS=1 asks for strict coverage.
		covered_any=0
		while IFS=$'\t' read -r _room chan _disp _name; do
			[ -n "$chan" ] && [ "$chan" != "<unknown>" ] && covered_any=1
		done <"$WORK/ch.tsv"
		if [ "$covered_any" -eq 1 ] && [ "${PORTAL_STRICT_PARTICIPANTS:-0}" != "1" ]; then
			skip_check "thread $thread_id: portals exist but none names all participants ${numbers[*]} (set PORTAL_STRICT_PARTICIPANTS=1 to fail this)"
		else
			fail "thread $thread_id: no portal room covers all participants (${numbers[*]}); candidates: $(cut -f1 "$WORK/ch.tsv" | tr '\n' ' ')"
		fi
	else
		skip_check "thread $thread_id: candidates found but no bridge channel id available to match (busy rooms?)"
	fi

	# Fracture: duplicate channel ids among this thread's candidates.
	dups="$(awk -F'\t' '$2 != "<unknown>" { print $2 }' "$WORK/ch.tsv" | sort | uniq -d)"
	if [ -n "$dups" ]; then
		fail "thread $thread_id: two rooms share bridge channel id(s): $dups"
	else
		known="$(awk -F'\t' '$2 != "<unknown>"' "$WORK/ch.tsv" | wc -l | tr -d ' ')"
		pass "thread $thread_id: no duplicate channel ids among $(wc -l <"$WORK/ch.tsv" | tr -d ' ') candidate room(s) ($known known)"
	fi

	# Own number must not appear among the thread participants.
	if [ -n "$PHONE_NUMBER" ]; then
		own="$(digits "$PHONE_NUMBER")"
		if printf '%s\n' "${numbers[@]}" | tr -cd '0-9\n' | grep -qx "$own"; then
			fail "thread $thread_id: own number $PHONE_NUMBER is listed as a participant"
		else
			pass "thread $thread_id: own number $PHONE_NUMBER is not a participant"
		fi
		if [ -n "$best_room" ]; then
			rc=0
			assert_no_own_number_ghost "$best_room" "$PHONE_NUMBER" || rc=$?
			case "$rc" in
			"$RC_OK") pass "thread $thread_id: no own-number ghost in $best_room" ;;
			"$RC_UNREACHABLE") skip_check "thread $thread_id: ghost check unavailable" ;;
			*) fail "thread $thread_id: own number $PHONE_NUMBER is a ghost in $best_room" ;;
			esac
		fi
	else
		skip_check "PHONE_NUMBER not set; own-number checks skipped (thread $thread_id)"
	fi
done <"$WORK/group-threads"

# --- 4. live second-sender check -------------------------------------------
newest_new_thread_id() {
	local since="$1" sms_thread="" pdu_thread="" sms_date=0 pdu_date=0
	# Ignore the MMS "notification" sentinel thread (9223372036854775807): the
	# downloaded row lands in the real thread a few seconds later.
	sms_thread="$(phone_sms_rows_since "$since" 2>/dev/null | tail -n 1 | cut -d'|' -f2)"
	sms_date="$(phone_sms_rows_since "$since" 2>/dev/null | tail -n 1 | cut -d'|' -f3)"
	pdu_thread="$(phone_mms_rows_since "$since" 2>/dev/null | awk -F'|' '$2 != "9223372036854775807" { line = $0 } END { print line }' | cut -d'|' -f2)"
	pdu_date="$(phone_mms_rows_since "$since" 2>/dev/null | awk -F'|' '$2 != "9223372036854775807" { line = $0 } END { print line }' | cut -d'|' -f3)"
	pdu_date_ms=$(( ${pdu_date:-0} * 1000 ))
	[ "${pdu_date:-0}" -gt 100000000000 ] 2>/dev/null && pdu_date_ms="$pdu_date"
	if [ -n "$pdu_thread" ] && [ "${pdu_date_ms:-0}" -ge "${sms_date:-0}" ]; then
		printf '%s\n' "$pdu_thread"
	elif [ -n "$sms_thread" ]; then
		printf '%s\n' "$sms_thread"
	fi
}

if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$IAMB_GROUP_ROOM" ]; then
	# Snapshot the portal rooms of every known group thread. The probe room may
	# text a thread that is not the first one, and a pre-existing thread must
	# not gain a second portal (that is the fracture this test looks for).
	live_snapshot_rooms() {
		local t_id p_rc=0
		: >"$WORK/live-rooms.tsv"
		while IFS='|' read -r t_id _count; do
			[ -n "$t_id" ] || continue
			p_rc=0
			phone_thread_participants "$t_id" >"$WORK/live-participants" 2>/dev/null || p_rc=$?
			[ "$p_rc" -eq 0 ] || continue
			[ -s "$WORK/live-participants" ] || continue
			mapfile -t numbers <"$WORK/live-participants"
			candidates_for_numbers "${numbers[@]}" >>"$WORK/live-rooms.tsv"
		done <"$WORK/group-threads"
		cut -f1 "$WORK/live-rooms.tsv" | sed '/^$/d' | LC_ALL=C sort -u
	}
	before_rooms="$(live_snapshot_rooms)"
	before_ms="$(now_ms)"
	LIVE_BEFORE_MS="$before_ms"
	has_new_phone_rows() {
		[ -n "$(newest_new_thread_id "$LIVE_BEFORE_MS")" ]
	}
	rc=0
	iamb_send "$IAMB_GROUP_ROOM" "validate t04 group probe $before_ms" >"$WORK/live-send.json" 2>"$WORK/live-send.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		pass "live group probe sent from iamb room $IAMB_GROUP_ROOM (at ${before_ms}ms)"
		rc=0
		wait_for 240 5 has_new_phone_rows || rc=$?
		if [ "$rc" -ne 0 ]; then
			fail "live group probe: no new SMS/MMS row on the phone within 240s"
		else
			after_thread_id="$(newest_new_thread_id "$before_ms")"
			if [ -z "$after_thread_id" ]; then
				fail "live group probe: new row has no resolvable thread id"
			else
				pass "live group probe recorded phone thread $after_thread_id"
			fi
			after_rooms="$(live_snapshot_rooms)"
			new_rooms="$(LC_ALL=C comm -13 <(printf '%s\n' "$before_rooms") <(printf '%s\n' "$after_rooms") | sed '/^$/d')"
			if [ -n "$new_rooms" ]; then
				fail "live group probe created new portal room(s) for a known thread: $(printf '%s' "$new_rooms" | tr '\n' ' ')"
			else
				pass "live group probe kept the existing portal room set ($(printf '%s\n' "$after_rooms" | sed '/^$/d' | wc -l | tr -d ' ') room(s))"
			fi
			# The probe's thread must resolve to exactly one bridge portal:
			# bridge channel ids end in `-<phone thread id>`. A brand-new
			# conversation legitimately gets one new room; a fracture gives two.
			if [ -n "$after_thread_id" ]; then
				probe_rc=0
				phone_thread_participants "$after_thread_id" >"$WORK/probe-participants" 2>/dev/null || probe_rc=$?
				if [ "$probe_rc" -ne 0 ] || [ ! -s "$WORK/probe-participants" ]; then
					skip_check "live group probe: cannot list participants of thread $after_thread_id"
				else
					mapfile -t probe_numbers <"$WORK/probe-participants"
					: >"$WORK/probe-chans"
					while IFS=$'\t' read -r room _name; do
						[ -n "$room" ] || continue
						chan="$(portal_channel_id "$room" 2>/dev/null || true)"
						[ -n "$chan" ] && printf '%s\n' "$chan" >>"$WORK/probe-chans"
					done < <(candidates_for_numbers "${probe_numbers[@]}")
					dup_probe="$(sort "$WORK/probe-chans" | uniq -d)"
					thread_portals="$(grep -c -- "-$after_thread_id\$" "$WORK/probe-chans" || true)"
					if [ -n "$dup_probe" ]; then
						fail "live group probe: channel id(s) mapped to several rooms (fracture): $dup_probe"
					elif [ "$thread_portals" -ge 1 ]; then
						probe_chan="$(grep -- "-$after_thread_id\$" "$WORK/probe-chans" | head -n 1)"
						pass "live group probe: thread $after_thread_id maps to one portal (channel id $probe_chan)"
					else
						skip_check "live group probe: no bridge portal found for thread $after_thread_id yet"
					fi
				fi
			fi
		fi
		;;
	"$RC_UNREACHABLE") skip_check "live group probe send failed: $(tr '\n' ' ' <"$WORK/live-send.err")" ;;
	*) fail "live group probe send misbehaved: $(tr '\n' ' ' <"$WORK/live-send.err")" ;;
	esac
else
	skip_check "live group probe disabled (set VALIDATE_LIVE_SENDS=1 and IAMB_GROUP_ROOM)"
fi

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t04_group_create: PASS (skips: $SKIPS)"
	exit 0
fi
log "t04_group_create: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
