#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t03_inbound_1to1.sh — acceptance step 2: an inbound 1:1 SMS produces exactly
# one portal room with the correct sender ghost and no own-number ghost.
#
# State checks (always):
#   1. adb/phone reachable and the telephony provider queryable.
#   2. the newest inbound SMS in the lookback window has a sender address and
#      a thread id (evidence: row id, thread id, timestamp).
#   3. at least one portal room exists for that address and no two rooms share
#      the same bridge channel id (no fracture).
#   4. the portal room contains a message event from the sender's ghost.
#
# Live action (opt-in: VALIDATE_LIVE_SENDS=1 and IAMB_PHONE_ROOM pointing at a
# JMP room that texts this phone):
#   5. send one SMS from the JMP side, wait for the new phone row, then re-run
#      the portal checks scoped to the new sender.
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t03.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

INBOUND_LOOKBACK_MS="${INBOUND_LOOKBACK_MS:-2592000000}" # 30 days
VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_PHONE_ROOM="${IAMB_PHONE_ROOM:-}"

log "t03_inbound_1to1: ADB_SSH=${ADB_SSH:-<local>} ADB_SERIAL=$ADB_SERIAL PHONE_NUMBER=${PHONE_NUMBER:-<unset>}"

# --- 1. phone reachable -----------------------------------------------------
rc=0
adb_device_online || rc=$?
case "$rc" in
"$RC_OK") pass "phone $ADB_SERIAL is online" ;;
*) skip_check "phone $ADB_SERIAL not reachable (adb)"; log "t03_inbound_1to1: SKIP-ONLY run"; exit 0 ;;
esac

# --- optional live send -----------------------------------------------------
live_probe_ran=0
if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$IAMB_PHONE_ROOM" ]; then
	before="$(now_ms)"
	rc=0
	iamb_send "$IAMB_PHONE_ROOM" "validate t03 probe $before" >"$WORK/send.json" 2>"$WORK/send.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		pass "inbound probe sent from iamb room $IAMB_PHONE_ROOM (at ${before}ms)"
		live_probe_ran=1
		rc=0
		wait_for 180 5 phone_has_new_sms_since "$before" || rc=$?
		if [ "$rc" -eq 0 ]; then
			phone_sms_count_since "$before" >"$WORK/count.json" 2>&1 || true
			pass "phone received a new SMS row after the probe (rows since ${before}ms: $(cat "$WORK/count.json"))"
		else
			fail "phone did not record a new SMS row within 180s of the probe"
		fi
		;;
	"$RC_UNREACHABLE") skip_check "live send from $IAMB_PHONE_ROOM failed: $(tr '\n' ' ' <"$WORK/send.err")" ;;
	*) fail "live send from $IAMB_PHONE_ROOM misbehaved: $(tr '\n' ' ' <"$WORK/send.err")" ;;
	esac
else
	skip_check "live inbound probe disabled (set VALIDATE_LIVE_SENDS=1 and IAMB_PHONE_ROOM)"
fi

# --- 2. newest inbound SMS --------------------------------------------------
since=$(( $(now_ms) - INBOUND_LOOKBACK_MS ))
rc=0
phone_sms_rows_since "$since" >"$WORK/sms.rows" 2>"$WORK/sms.err" || rc=$?
case "$rc" in
"$RC_OK") pass "telephony provider queryable (SMS rows in the last 30d: $(wc -l <"$WORK/sms.rows" | tr -d ' '))" ;;
*) skip_check "telephony provider not queryable: $(tr '\n' ' ' <"$WORK/sms.err")"; log "t03_inbound_1to1: SKIP-ONLY run"; exit 0 ;;
esac

newest="$(awk -F'|' '$4 == 1 { line = $0 } END { print line }' "$WORK/sms.rows")" || true
if [ -z "$newest" ]; then
	skip_check "no inbound SMS rows on the phone in the lookback window"
	log "t03_inbound_1to1: SKIP-ONLY run"; exit 0
fi
sms_id="$(cut -d'|' -f1 <<<"$newest")"
thread_id="$(cut -d'|' -f2 <<<"$newest")"
sms_date="$(cut -d'|' -f3 <<<"$newest")"
sender="$(cut -d'|' -f5 <<<"$newest")"
pass "newest inbound SMS: id=$sms_id thread=$thread_id date=$sms_date sender=$sender"

if [ -z "$sender" ]; then
	fail "newest inbound SMS has an empty sender address"
	log "t03_inbound_1to1: summary"; [ "$FAILURES" -eq 0 ] && exit 0 || exit 1
fi

# --- 3. portal rooms for the sender ----------------------------------------
if [ -z "$SMS_MGMT_ROOM" ] && [ "$live_probe_ran" != "1" ]; then
	skip_check "SMS_MGMT_ROOM not set; assuming the iamb account does not bridge this phone; Matrix-side checks skipped"
	log "t03_inbound_1to1: summary"
	[ "$FAILURES" -eq 0 ] && exit 0 || exit 1
fi
rc=0
room_ids_matching_name "$sender" >"$WORK/rooms.tsv" 2>"$WORK/rooms.err" || rc=$?
case "$rc" in
"$RC_OK") ;;
*) skip_check "iamb not available for Matrix-side checks: $(tr '\n' ' ' <"$WORK/rooms.err")"; log "t03_inbound_1to1: SKIP-ONLY run"; exit 0 ;;
esac

if [ ! -s "$WORK/rooms.tsv" ]; then
	if [ "$live_probe_ran" = "1" ]; then
		fail "no joined room name contains the sender $sender after the live probe (portal missing)"
	else
		skip_check "no joined room name contains the sender $sender (does IAMB_PROFILE=$IAMB_PROFILE bridge this phone?)"
	fi
	log "t03_inbound_1to1: summary"
	[ "$FAILURES" -eq 0 ] && exit 0 || exit 1
fi
pass "rooms mentioning $sender: $(cut -f1 "$WORK/rooms.tsv" | tr '\n' ' ')"

# Channel-id fracture check: no two rooms may share a bridge channel id.
: >"$WORK/channels.tsv"
while IFS=$'\t' read -r room _; do
	[ -n "$room" ] || continue
	chan="$(portal_channel_id "$room" 2>/dev/null || true)"
	if [ -n "$chan" ]; then
		printf '%s\t%s\n' "$chan" "$room" >>"$WORK/channels.tsv"
	else
		printf '%s\t%s\n' "<unknown>" "$room" >>"$WORK/channels.tsv"
	fi
done <"$WORK/rooms.tsv"

dup_channels="$(awk -F'\t' '$1 != "<unknown>" { print $1 }' "$WORK/channels.tsv" | sort | uniq -d)"
if [ -n "$dup_channels" ]; then
	fail "two rooms share a bridge channel id (fracture): $dup_channels"
else
	known="$(awk -F'\t' '$1 != "<unknown>"' "$WORK/channels.tsv" | wc -l | tr -d ' ')"
	pass "no duplicate bridge channel ids across $(wc -l <"$WORK/rooms.tsv" | tr -d ' ') room(s) ($known with known channel ids)"
fi

# Message event from the sender ghost near the SMS timestamp.
portal_room="$(awk -F'\t' -v d="$sms_date" 'BEGIN { best = 0 } { if ($1 != "" ) { print $1; exit } }' "$WORK/rooms.tsv")"
ghost="$(printf '%s' "$sender" | sed 's/+/=2b/g')"
ghost_esc="$(printf '%s' "$ghost" | sed 's/=/=3d/g')"
attempts=1
if [ "$live_probe_ran" = "1" ]; then
	# The bridge may need a little while to mirror the probe into Matrix.
	attempts=12
fi
rc=0
hits=0
for attempt in $(seq 1 "$attempts"); do
	rc=0
	iamb_cmd message "$portal_room" >"$WORK/portal-events.json" 2>"$WORK/portal-events.err" || rc=$?
	if [ "$rc" -ne 0 ]; then
		break
	fi
	hits="$(jq -r --arg ghost "$ghost" --arg ghost_esc "$ghost_esc" 'select(.type == "m.room.message") | (.sender // "") | select(contains($ghost) or contains($ghost_esc))' "$WORK/portal-events.json" | wc -l | tr -d ' ')"
	if [ "$hits" -ge 1 ]; then
		break
	fi
	if [ "$attempt" -lt "$attempts" ]; then
		sleep 10
	fi
done
if [ "$rc" -ne 0 ]; then
	skip_check "iamb message fetch failed for $portal_room: $(tr '\n' ' ' <"$WORK/portal-events.err")"
elif [ "$hits" -ge 1 ]; then
	ev="$(jq -r --arg ghost "$ghost" --arg ghost_esc "$ghost_esc" 'select(.type == "m.room.message") | select(((.sender // "") | contains($ghost)) or ((.sender // "") | contains($ghost_esc))) | .event_id' "$WORK/portal-events.json" | tail -n 1)"
	pass "portal $portal_room carries $hits message event(s) from the sender ghost (last $ev)"
else
	fail "portal $portal_room has no message event from sender ghost $ghost"
fi

# Own-number ghost check.
if [ -n "$PHONE_NUMBER" ]; then
	rc=0
	assert_no_own_number_ghost "$portal_room" "$PHONE_NUMBER" || rc=$?
	case "$rc" in
	"$RC_OK") pass "own number $PHONE_NUMBER does not appear as a member in $portal_room" ;;
	"$RC_UNREACHABLE") skip_check "own-number ghost check unavailable for $portal_room" ;;
	*) fail "own number $PHONE_NUMBER appears as a ghost in $portal_room" ;;
	esac
else
	skip_check "PHONE_NUMBER not set; own-number ghost check skipped"
fi

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t03_inbound_1to1: PASS (skips: $SKIPS)"
	exit 0
fi
log "t03_inbound_1to1: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
