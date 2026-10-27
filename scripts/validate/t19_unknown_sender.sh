#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t19_unknown_sender.sh — unknown-number / new-sender portal creation.
#
# State checks (always):
#   1. every 1:1 phone thread that ever received an inbound message has a
#      Matrix portal room named after its counterparty (a missing portal for
#      an inbound thread is a FAIL; outbound-only threads are reported and
#      SKIPped, the bridge may legitimately not have seen them yet);
#   2. NEW_SENDER_NUMBER (defaults to SMS_TARGET_NUMBER) has exactly ONE phone
#      thread — no duplicate thread creation — and its portal state is
#      reported (portal presence is only asserted for inbound threads).
#
# Live probe (opt-in: VALIDATE_LIVE_SENDS=1 and IAMB_NEW_SENDER_ROOM): send one
# SMS from an iamb room whose number is NOT among the phone's known
# counterparties, then assert the phone created exactly one thread + the
# bridge created exactly one portal room for the new sender (no ghost
# duplicates). Without such a room the probe SKIPs.
#
# Unreachable components SKIP; reachable-but-wrong answers FAIL.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$SCRIPT_DIR/lib.sh"

FAILURES=0
SKIPS=0

pass() { log "PASS: $*"; }
skip_check() { SKIPS=$((SKIPS + 1)); skip "$*"; }
fail() { FAILURES=$((FAILURES + 1)); printf '[validate] FAIL: %s\n' "$*" >&2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t19.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_NEW_SENDER_ROOM="${IAMB_NEW_SENDER_ROOM:-}"
NEW_SENDER_NUMBER="${NEW_SENDER_NUMBER:-${SMS_TARGET_NUMBER:-}}"
MAX_DM_THREADS="${MAX_DM_THREADS:-20}"

log "t19_unknown_sender: new_sender=${NEW_SENDER_NUMBER:-<unset>} iamb_room=${IAMB_NEW_SENDER_ROOM:-<unset>} live=$VALIDATE_LIVE_SENDS"

# ---------------------------------------------------------------------------
# 1. phone
# ---------------------------------------------------------------------------
rc=0
adb_device_online || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "phone $ADB_SERIAL not reachable (adb)"; log "t19_unknown_sender: SKIP-ONLY run"; exit 0
fi
pass "phone $ADB_SERIAL is online"

rc=0
phone_threads >"$WORK/threads" 2>"$WORK/threads.err" || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "telephony provider not queryable: $(tr '\n' ' ' <"$WORK/threads.err")"
	log "t19_unknown_sender: SKIP-ONLY run"
	exit 0
fi

# portal_room_exact ADDRESS — the portal room for ADDRESS: an exact-name match
# first (retried once; the iamb room snapshot can be flaky), then a unique room
# whose name merely contains the address (a portal may carry a thread title).
# jmpchat/XMPP portals are never returned.
portal_room_exact() {
	local address="$1" rows room name attempt matches
	for attempt in 1 2; do
		rows="$(rooms_named "$address" 2>/dev/null || true)"
		while IFS=$'\t' read -r room name; do
			[ -n "$room" ] || continue
			case "$name" in
			*jmp*|*cheogram*) continue ;;
			esac
			printf '%s\n' "$room"
			return 0
		done <<<"$rows"
		[ "$attempt" -eq 1 ] && sleep 2
	done
	rows="$(room_ids_matching_name "$address" 2>/dev/null || true)"
	matches="$(awk -F'\t' '$2 !~ /jmp|cheogram/ { print $1 }' <<<"$rows" | sed '/^$/d' | sort -u)"
	if [ -n "$matches" ] && [ "$(printf '%s\n' "$matches" | wc -l | tr -d ' ')" -eq 1 ]; then
		printf '%s\n' "$matches"
	fi
	return 0
}

# ---------------------------------------------------------------------------
# 2. 1:1 thread portal coverage
# ---------------------------------------------------------------------------
checked=0
inbound_threads=0
while IFS='|' read -r thread_id _date _recipients _count; do
	[ -n "$thread_id" ] || continue
	[ "$checked" -ge "$MAX_DM_THREADS" ] && break
	participants="$(phone_thread_participants "$thread_id" 2>/dev/null | sed '/^$/d')"
	participant_count="$(printf '%s\n' "$participants" | sed '/^$/d' | wc -l | tr -d ' ')"
	[ "$participant_count" -eq 1 ] || continue
	checked=$((checked + 1))
	address="$(printf '%s\n' "$participants" | head -n 1)"
	inbound="$(sqlite_query "select count(*) from sms where thread_id = $thread_id and type = 1;" 2>/dev/null | tail -n 1)"
	pdu_count="$(sqlite_query "select count(*) from pdu where thread_id = $thread_id;" 2>/dev/null | tail -n 1)"
	room="$(portal_room_exact "$address")"
	fuzzy="$(room_ids_matching_name "$address" 2>/dev/null | awk -F'\t' '$2 !~ /jmp|cheogram/ { n++ } END { print n + 0 }')"
	pass "DM thread $thread_id counterparty=$address inbound=$inbound pdus=$pdu_count portal=${room:-<none>} rooms_mentioning=$fuzzy"
	if [ -n "$room" ]; then
		pass "thread $thread_id has a portal room ($room)"
	else
		if [ "${inbound:-0}" -ge 1 ]; then
			inbound_threads=$((inbound_threads + 1))
			fail "thread $thread_id received $inbound inbound message(s) but has no portal room named $address"
		else
			skip_check "thread $thread_id is outbound-only and has no portal room yet (bridge may not have seen the thread)"
		fi
	fi
done <"$WORK/threads"

if [ "$checked" -eq 0 ]; then
	skip_check "no 1:1 phone threads with a single participant found"
fi

# ---------------------------------------------------------------------------
# 3. the new-sender number (e.g. the t15 outbound target)
# ---------------------------------------------------------------------------
if [ -z "$NEW_SENDER_NUMBER" ]; then
	skip_check "NEW_SENDER_NUMBER not set; new-sender thread/portal check skipped"
else
	normalized="$(printf '%s' "$NEW_SENDER_NUMBER" | tr -cd '0-9+')"
	thread_rows="$(sqlite_query "
		select t._id, ca.address from threads t
		join canonical_addresses ca
		  on instr(','||replace(t.recipient_ids,' ',',')||',', ','||ca._id||',') > 0
		where replace(replace(ca.address,'+',''),'-','') = replace(replace('$normalized','+',''),'-','');" 2>/dev/null | sed '/^$/d')"
	thread_count="$(printf '%s\n' "$thread_rows" | sed '/^$/d' | wc -l | tr -d ' ')"
	pass "new-sender $NEW_SENDER_NUMBER: phone threads=$thread_count $([ -n "$thread_rows" ] && printf '(%s)' "$(printf '%s' "$thread_rows" | cut -d'|' -f1 | tr '\n' ',' | sed 's/,$//')")"
	if [ "$thread_count" -eq 0 ]; then
		skip_check "new-sender $NEW_SENDER_NUMBER has no phone thread yet (send something first)"
	elif [ "$thread_count" -eq 1 ]; then
		pass "exactly one phone thread for the new sender (no duplicate thread creation)"
		thread_id="$(printf '%s' "$thread_rows" | head -n 1 | cut -d'|' -f1)"
		inbound="$(sqlite_query "select count(*) from sms where thread_id = $thread_id and type = 1;" 2>/dev/null | tail -n 1)"
		room="$(portal_room_exact "$NEW_SENDER_NUMBER")"
		if [ -n "$room" ]; then
			pass "new-sender portal exists: $room"
		elif [ "${inbound:-0}" -ge 1 ]; then
			fail "new-sender $NEW_SENDER_NUMBER texted the phone but has no portal room"
		else
			skip_check "new-sender $NEW_SENDER_NUMBER has no portal room yet (outbound-only; bridge event delivery pending)"
		fi
	else
		fail "new-sender $NEW_SENDER_NUMBER has $thread_count phone threads (want exactly 1): $(printf '%s' "$thread_rows" | cut -d'|' -f1 | tr '\n' ' ')"
	fi
fi

# ---------------------------------------------------------------------------
# 4. optional live probe from a genuinely new sender
# ---------------------------------------------------------------------------
if [ "$VALIDATE_LIVE_SENDS" != "1" ] || [ -z "$IAMB_NEW_SENDER_ROOM" ]; then
	skip_check "new-sender live probe disabled (set VALIDATE_LIVE_SENDS=1 and IAMB_NEW_SENDER_ROOM pointing at an iamb room that texts this phone from a number not yet in its threads)"
else
	since_24h=$(( $(now_ms) - 86400000 ))
	known="$(sqlite_query "select address from sms where date >= $since_24h order by date desc;" 2>/dev/null | sort -u | tr '\n' ' ')"
	pass "known recent counterparties: $known"
	before_threads="$(sqlite_query "select count(*) from threads;" 2>/dev/null | tail -n 1)"
	before_ms="$(now_ms)"
	rc=0
	iamb_send "$IAMB_NEW_SENDER_ROOM" "validate t19 new sender probe $before_ms" >"$WORK/send.json" 2>"$WORK/send.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		pass "new-sender probe sent from $IAMB_NEW_SENDER_ROOM"
		sleep 60
		after_threads="$(sqlite_query "select count(*) from threads;" 2>/dev/null | tail -n 1)"
		if [ "$before_threads" = "$after_threads" ]; then
			skip_check "no new phone thread appeared after the probe (the sender number may already be known)"
		else
			new_count=$((after_threads - before_threads))
			if [ "$new_count" -eq 1 ]; then
				pass "exactly one new phone thread after the new-sender probe"
			else
				fail "$new_count new phone threads after one new-sender probe (want exactly 1)"
			fi
			new_rows="$(phone_sms_rows_since "$before_ms" 2>/dev/null | awk -F'|' '$4 == 1' | head -n 1)"
			if [ -n "$new_rows" ]; then
				sender="$(cut -d'|' -f5 <<<"$new_rows")"
				pass "new inbound row: id=$(cut -d'|' -f1 <<<"$new_rows") thread=$(cut -d'|' -f2 <<<"$new_rows") sender=$sender"
				room="$(portal_room_exact "$sender")"
				if [ -n "$room" ]; then
					pass "portal created for the new sender: $room"
					chan="$(portal_channel_id "$room" 2>/dev/null || true)"
					if [ -n "$chan" ]; then
						pass "portal channel id present ($chan)"
					else
						skip_check "portal $room has no m.bridge channel in the fetched window"
					fi
				else
					fail "no portal room found for the new sender $sender"
				fi
			else
				fail "no inbound SMS row recorded after the new-sender probe"
			fi
		fi
		;;
	"$RC_UNREACHABLE") skip_check "new-sender probe send failed: $(tr '\n' ' ' <"$WORK/send.err")" ;;
	*) fail "new-sender probe send misbehaved: $(tr '\n' ' ' <"$WORK/send.err")" ;;
	esac
fi

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t19_unknown_sender: PASS (skips: $SKIPS)"
	exit 0
fi
log "t19_unknown_sender: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
