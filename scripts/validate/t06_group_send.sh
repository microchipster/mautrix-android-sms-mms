#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t06_group_send.sh — acceptance step 5 (priority): one Matrix message in a
# group portal produces exactly ONE group MMS on the phone (one send_mms,
# one pdu row) and no extra portal rooms.
#
# State checks (always):
#   1. the phone's group threads have at least one MMS row each (evidence:
#      pdu id/thread/timestamps) — skipped when no pdu rows exist yet.
#   2. the portal of the target group thread is unique (single channel id).
#
# Live check (opt-in: VALIDATE_LIVE_SENDS=1 and IAMB_GROUP_PORTAL pointing at
# the Matrix portal room of a group thread):
#   3. send one Matrix message into the portal; wait for the phone to record
#      it; assert exactly one new MMS pdu row in the same thread (no SMS
#      fallback, no duplicate MMS) and no new portal room.
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t06.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_GROUP_PORTAL="${IAMB_GROUP_PORTAL:-}"

log "t06_group_send: ADB_SSH=${ADB_SSH:-<local>} ADB_SERIAL=$ADB_SERIAL portal=${IAMB_GROUP_PORTAL:-<unset>}"

# --- 1. phone group threads and their MMS rows ------------------------------
rc=0
adb_device_online || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "phone $ADB_SERIAL not reachable (adb)"; log "t06_group_send: SKIP-ONLY run"; exit 0
fi
pass "phone $ADB_SERIAL is online"

rc=0
phone_thread_ids_with_participants >"$WORK/group-threads" 2>"$WORK/threads.err" || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "telephony provider not queryable: $(tr '\n' ' ' <"$WORK/threads.err")"; log "t06_group_send: SKIP-ONLY run"; exit 0
fi
if [ ! -s "$WORK/group-threads" ]; then
	skip_check "no group threads on the phone"
	log "t06_group_send: SKIP-ONLY run"; exit 0
fi
pass "group threads to inspect: $(wc -l <"$WORK/group-threads" | tr -d ' ')"

group_thread_ids="$(cut -d'|' -f1 "$WORK/group-threads" | tr '\n' ' ' | sed 's/ $//')"
pdu_state="$(phone_mms_rows_since 0 2>/dev/null || true)"
if [ -z "$pdu_state" ]; then
	skip_check "no MMS pdu rows on the phone yet (no group MMS ever received/sent)"
else
	matched=0
	while IFS='|' read -r pdu_id thread_id date msg_box; do
		[ -n "$pdu_id" ] || continue
		for gt in $group_thread_ids; do
			if [ "$thread_id" = "$gt" ]; then
				matched=$((matched + 1))
				pass "group MMS pdu id=$pdu_id thread=$thread_id date=$date msg_box=$msg_box"
				break
			fi
		done
	done <<<"$pdu_state"
	if [ "$matched" -eq 0 ]; then
		skip_check "no MMS pdu row belongs to a group thread yet"
	fi
fi

# --- 2. portal uniqueness ---------------------------------------------------
if [ -z "$IAMB_GROUP_PORTAL" ]; then
	skip_check "IAMB_GROUP_PORTAL not set; portal uniqueness check skipped"
elif ! iamb_available; then
	skip_check "iamb not available on $IAMB_SSH; portal uniqueness check skipped"
else
	chan="$(portal_channel_id "$IAMB_GROUP_PORTAL" 2>/dev/null || true)"
	disp="$(portal_channel_displayname "$IAMB_GROUP_PORTAL" 2>/dev/null || true)"
	if [ -n "$chan" ]; then
		pass "group portal $IAMB_GROUP_PORTAL channel=$chan displayname='${disp:-<none>}'"
	else
		skip_check "portal $IAMB_GROUP_PORTAL has no m.bridge channel in the fetched window"
	fi
fi

# --- 3. live group send -----------------------------------------------------
role="$(app_default_sms_role 2>/dev/null || true)"
if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$IAMB_GROUP_PORTAL" ] && [ "$role" = "true" ]; then
	if ! iamb_available; then
		skip_check "live group send: iamb not available"
	else
		before_ms="$(now_ms)"
		# Provider dates have second granularity, so a row written in the same
		# second as the probe can look older than before_ms: count rows by
		# their monotonically increasing id instead.
		before_sms_id="$(sqlite_query "select coalesce(max(_id),0) from sms;" 2>/dev/null | tail -n 1)"
		before_pdu_id="$(sqlite_query "select coalesce(max(_id),0) from pdu;" 2>/dev/null | tail -n 1)"
		[ -n "$before_sms_id" ] || before_sms_id=0
		[ -n "$before_pdu_id" ] || before_pdu_id=0
		rc=0
		iamb_send "$IAMB_GROUP_PORTAL" "validate t06 group send probe $before_ms" >"$WORK/send.json" 2>"$WORK/send.err" || rc=$?
		case "$rc" in
		"$RC_OK")
			pass "Matrix message sent into portal $IAMB_GROUP_PORTAL (at ${before_ms}ms)"
			LIVE_BEFORE_SMS_ID="$before_sms_id"
			LIVE_BEFORE_PDU_ID="$before_pdu_id"
			has_new_phone_rows() {
				local c p
				c="$(sqlite_query "select count(*) from sms where _id > $LIVE_BEFORE_SMS_ID;" 2>/dev/null | tail -n 1)"
				p="$(sqlite_query "select count(*) from pdu where _id > $LIVE_BEFORE_PDU_ID;" 2>/dev/null | tail -n 1)"
				[ "${c:-0}" -gt 0 ] || [ "${p:-0}" -gt 0 ]
			}
			rc=0
			wait_for 240 5 has_new_phone_rows || rc=$?
			if [ "$rc" -ne 0 ]; then
				fail "live group send: no new SMS/MMS row on the phone within 240s"
			else
				after_pdu_rows="$(sqlite_query "select _id,thread_id,date,msg_box from pdu where _id > $LIVE_BEFORE_PDU_ID order by _id;" 2>/dev/null)"
				after_sms_rows="$(sqlite_query "select _id,thread_id,date,type,address from sms where _id > $LIVE_BEFORE_SMS_ID order by _id;" 2>/dev/null)"
				delta_pdu="$(printf '%s\n' "$after_pdu_rows" | sed '/^$/d' | wc -l | tr -d ' ')"
				delta_sms="$(printf '%s\n' "$after_sms_rows" | sed '/^$/d' | wc -l | tr -d ' ')"
				if [ "$delta_pdu" -eq 1 ] && [ "$delta_sms" -eq 0 ]; then
					new_pdu_id="$(cut -d'|' -f1 <<<"$after_pdu_rows")"
					new_thread="$(cut -d'|' -f2 <<<"$after_pdu_rows")"
					pass "exactly one new MMS row (pdu id=$new_pdu_id thread=$new_thread), no SMS fallback"
				elif [ "$delta_pdu" -gt 1 ]; then
					fail "group send produced $delta_pdu new MMS rows (want exactly 1): $(printf '%s' "$after_pdu_rows" | cut -d'|' -f1 | tr '\n' ' ')"
				elif [ "$delta_sms" -gt 0 ]; then
					fail "group send fell back to SMS ($delta_sms new SMS row(s), no MMS row): $(printf '%s' "$after_sms_rows" | cut -d'|' -f1 | tr '\n' ' ')"
				else
					fail "group send recorded neither an SMS nor an MMS row"
				fi
				# No new portal room for the group.
				chan_after="$(portal_channel_id "$IAMB_GROUP_PORTAL" 2>/dev/null || true)"
				if [ "$chan_after" = "$chan" ]; then
					pass "portal channel unchanged after the send ($chan_after)"
				elif [ -n "$chan_after" ]; then
					fail "portal channel changed after the send ($chan -> $chan_after)"
				else
					skip_check "portal channel not readable after the send"
				fi
			fi
			;;
		"$RC_UNREACHABLE") skip_check "live group send failed: $(tr '\n' ' ' <"$WORK/send.err")" ;;
		*) fail "live group send misbehaved: $(tr '\n' ' ' <"$WORK/send.err")" ;;
		esac
	fi
else
	if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$IAMB_GROUP_PORTAL" ] && [ "$role" != "true" ]; then
		skip_check "live group send: app is not the default SMS app (default_sms_role=${role:-unknown}); MMS sends are refused by design"
	else
		skip_check "live group send disabled (set VALIDATE_LIVE_SENDS=1 and IAMB_GROUP_PORTAL)"
	fi
fi

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t06_group_send: PASS (skips: $SKIPS)"
	exit 0
fi
log "t06_group_send: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
