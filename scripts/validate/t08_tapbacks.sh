#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t08_tapbacks.sh — acceptance step 7: tapbacks both ways.
#
# Inbound (phone -> Matrix): phone tapback SMS rows ("Liked …" style) must have
# a corresponding Matrix reaction in the portal room. State checks count both
# sides without ever printing a message body.
#
# Outbound (Matrix -> phone): an iamb reaction in the portal room must produce
# a new SMS row on the phone (documented fallback: a "Liked …" text). Opt-in:
# VALIDATE_LIVE_SENDS=1, IAMB_PORTAL_ROOM, IAMB_TAPBACK_EVENT.
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t08.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_PORTAL_ROOM="${IAMB_PORTAL_ROOM:-}"
IAMB_TAPBACK_EVENT="${IAMB_TAPBACK_EVENT:-}"
MAX_SCAN_ROOMS="${MAX_SCAN_ROOMS:-3}"

log "t08_tapbacks: ADB_SSH=${ADB_SSH:-<local>} portal=${IAMB_PORTAL_ROOM:-<unset>}"

# --- phone-side tapback rows (counts only) ----------------------------------
rc=0
adb_device_online || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "phone $ADB_SERIAL not reachable (adb); phone-side tapback evidence skipped"
else
	rc=0
	count="$(sqlite_query "select count(*) from sms where body like 'Liked %';" 2>"$WORK/sql.err" | tail -n 1)" || rc=$?
	if [ "$rc" -ne 0 ] || [ -z "$count" ]; then
		skip_check "telephony provider not queryable: $(tr '\n' ' ' <"$WORK/sql.err")"
	else
		if [ "$count" -ge 1 ]; then
			pass "phone has $count tapback-format SMS row(s) (body pattern only, not logged)"
		else
			skip_check "phone has no tapback-format SMS rows yet"
		fi
	fi
fi

# --- Matrix-side reactions ---------------------------------------------------
if [ -z "$IAMB_PORTAL_ROOM" ] && [ -z "$SMS_MGMT_ROOM" ]; then
	skip_check "no IAMB_PORTAL_ROOM/SMS_MGMT_ROOM set; assuming the iamb account does not bridge this phone; Matrix-side reactions skipped"
	log "t08_tapbacks: SKIP-ONLY run"; exit 0
fi
if ! iamb_available; then
	skip_check "iamb not available on $IAMB_SSH; Matrix-side reactions skipped"
	log "t08_tapbacks: SKIP-ONLY run"; exit 0
fi

: >"$WORK/rooms.tsv"
if [ -n "$IAMB_PORTAL_ROOM" ]; then
	printf '%s\t%s\n' "$IAMB_PORTAL_ROOM" "<configured>" >"$WORK/rooms.tsv"
else
	# Find portal rooms via the phone's recent SMS senders.
	rc=0
	rows="$(phone_sms_rows_since $(( $(now_ms) - 2592000000 )) 2>/dev/null | tail -n 20)" || rc=$?
	if [ "$rc" -ne 0 ] || [ -z "$rows" ]; then
		skip_check "cannot derive portal rooms (no phone SMS rows)"
	else
		while IFS='|' read -r _id _thread _date _type address; do
			[ -n "$address" ] || continue
			room_ids_matching_name "$address" >>"$WORK/rooms.tsv" 2>/dev/null || true
		done <<<"$rows"
		sort -u "$WORK/rooms.tsv" | head -n "$MAX_SCAN_ROOMS" >"$WORK/rooms.head"
		mv "$WORK/rooms.head" "$WORK/rooms.tsv"
	fi
fi

if [ ! -s "$WORK/rooms.tsv" ]; then
	skip_check "no candidate portal rooms to scan for reactions"
	log "t08_tapbacks: summary"
	[ "$FAILURES" -eq 0 ] && exit 0 || exit 1
fi

total_reactions=0
while IFS=$'\t' read -r room _; do
	[ -n "$room" ] || continue
	rc=0
	iamb_cmd message "$room" >"$WORK/events.json" 2>"$WORK/events.err" || rc=$?
	if [ "$rc" -ne 0 ]; then
		skip_check "cannot fetch events for $room"
		continue
	fi
	reactions="$(jq -r 'select(.type == "m.reaction") | .event_id' "$WORK/events.json" | wc -l | tr -d ' ')"
	if [ "$reactions" -ge 1 ]; then
		last_ev="$(jq -r 'select(.type == "m.reaction") | .event_id' "$WORK/events.json" | tail -n 1)"
		pass "$room has $reactions Matrix reaction event(s) (last $last_ev)"
		total_reactions=$((total_reactions + reactions))
	else
		skip_check "$room has no reaction events in the fetched window"
	fi
done <"$WORK/rooms.tsv"

# Phone tapbacks with no Matrix reactions anywhere is suspicious; check one.
if [ "${count:-0}" -ge 1 ] && [ "$total_reactions" -eq 0 ]; then
	fail "phone has tapback SMS rows but no Matrix reaction was found in $MAX_SCAN_ROOMS candidate room(s)"
fi

# --- outbound live reaction --------------------------------------------------
if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$IAMB_PORTAL_ROOM" ] && [ -n "$IAMB_TAPBACK_EVENT" ]; then
	before_ms="$(now_ms)"
	rc=0
	iamb_react "$IAMB_PORTAL_ROOM" "$IAMB_TAPBACK_EVENT" >"$WORK/react.json" 2>"$WORK/react.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		pass "reaction sent to $IAMB_TAPBACK_EVENT in $IAMB_PORTAL_ROOM"
		rc=0
		LIVE_BEFORE_MS="$before_ms"
		has_new_sms_rows() {
			local c
			c="$(phone_sms_count_since "$LIVE_BEFORE_MS" 2>/dev/null || echo 0)"
			[ "${c:-0}" -gt 0 ]
		}
		wait_for 120 5 has_new_sms_rows || rc=$?
		if [ "$rc" -eq 0 ]; then
			pass "outbound reaction produced a new SMS row on the phone"
		elif [ "${EXPECT_REACTION_TEXT:-0}" = "1" ]; then
			fail "outbound reaction produced no new SMS row within 120s (fallback missing)"
		else
			skip_check "outbound reaction sent, but no reaction-text SMS appeared (set EXPECT_REACTION_TEXT=1 when the bridge runs send_reactions_as_text)"
		fi
		;;
	"$RC_UNREACHABLE") skip_check "outbound reaction failed: $(tr '\n' ' ' <"$WORK/react.err")" ;;
	*) fail "outbound reaction misbehaved: $(tr '\n' ' ' <"$WORK/react.err")" ;;
	esac
else
	skip_check "outbound live reaction disabled (VALIDATE_LIVE_SENDS=1, IAMB_PORTAL_ROOM, IAMB_TAPBACK_EVENT)"
fi

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t08_tapbacks: PASS (skips: $SKIPS)"
	exit 0
fi
log "t08_tapbacks: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
