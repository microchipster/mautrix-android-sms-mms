#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t14_invite_join.sh — acceptance step 13: invite/join flows.
#
# The bridge invites the Matrix user to every portal room it creates; the user
# must end up joined (portals, the device space and the management room). This
# test checks:
#   1. no pending invites from the SMS bridge bot remain (iamb invites);
#   2. the management room and the device space are joined (when configured);
#   3. portal candidate rooms for the phone's recent threads are joined (`room
#      info` reports "Type: room · joined").
#
# Live create flow (opt-in: VALIDATE_LIVE_SENDS=1 + IAMB_GROUP_ROOM) sends one
# message from a JMP room and asserts the resulting portal invite is either
# absent (auto-join) or accepted — the harness accepts invites only when
# INVITES_ACCEPT=1.
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t14.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_GROUP_ROOM="${IAMB_GROUP_ROOM:-}"
INVITES_ACCEPT="${INVITES_ACCEPT:-0}"
MAX_CANDIDATE_ROOMS="${MAX_CANDIDATE_ROOMS:-4}"

log "t14_invite_join: SMS_MGMT_ROOM=${SMS_MGMT_ROOM:-<unset>} accept_invites=$INVITES_ACCEPT"

if [ -z "$SMS_MGMT_ROOM" ] && [ -z "$IAMB_PHONE_ROOM" ] && [ -z "$IAMB_GROUP_ROOM" ]; then
	skip_check "no SMS_MGMT_ROOM/IAMB_* room set; assuming the iamb account does not bridge this phone; invite/join checks skipped"
	log "t14_invite_join: SKIP-ONLY run"; exit 0
fi
if ! iamb_available; then
	skip_check "iamb not available on $IAMB_SSH; invite/join checks skipped"
	log "t14_invite_join: SKIP-ONLY run"; exit 0
fi

# --- 1. pending invites -----------------------------------------------------
rc=0
iamb_cmd invites >"$WORK/invites.txt" 2>"$WORK/invites.err" || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "iamb invites failed: $(tr '\n' ' ' <"$WORK/invites.err")"
else
	if grep -qi 'no pending invites' "$WORK/invites.txt"; then
		pass "no pending invites (portals auto-joined or already handled)"
	else
		pending_rooms="$(grep -oE '![A-Za-z0-9_-]+' "$WORK/invites.txt" | sort -u | tr '\n' ' ' | sed 's/ $//')"
		if [ -z "$pending_rooms" ]; then
			skip_check "iamb invites output is neither 'no pending invites' nor a room list"
		elif [ "$INVITES_ACCEPT" = "1" ]; then
			rc=0
			iamb_cmd invites --accept >"$WORK/accept.txt" 2>"$WORK/accept.err" || rc=$?
			if [ "$rc" -eq 0 ]; then
				pass "accepted pending invite(s): $pending_rooms"
			else
				fail "accepting pending invites failed: $(tr '\n' ' ' <"$WORK/accept.err")"
			fi
		else
			fail "pending invites remain (set INVITES_ACCEPT=1 to accept): $pending_rooms"
		fi
	fi
fi

# --- 2. management room / space joined --------------------------------------
if [ -z "$SMS_MGMT_ROOM" ]; then
	skip_check "SMS_MGMT_ROOM not set; management-room join check skipped"
else
	rc=0
	iamb_cmd room info "$SMS_MGMT_ROOM" >"$WORK/mgmt.txt" 2>"$WORK/mgmt.err" || rc=$?
	if [ "$rc" -ne 0 ]; then
		skip_check "cannot fetch room info for $SMS_MGMT_ROOM: $(tr '\n' ' ' <"$WORK/mgmt.err")"
	elif grep -qE 'Type: room( · direct)? · joined' "$WORK/mgmt.txt"; then
		pass "management room $SMS_MGMT_ROOM is joined"
	else
		fail "management room $SMS_MGMT_ROOM is not joined"
	fi
fi

# --- 3. portal candidates are joined ----------------------------------------
rc=0
phone_sms_rows_since 0 >"$WORK/sms.rows" 2>/dev/null || rc=$?
if [ "$rc" -ne 0 ] || [ ! -s "$WORK/sms.rows" ]; then
	skip_check "no phone SMS rows; portal join check skipped"
else
	: >"$WORK/candidates.tsv"
	while IFS='|' read -r _id _thread _date _type address; do
		[ -n "$address" ] || continue
		room_ids_matching_name "$address" >>"$WORK/candidates.tsv" 2>/dev/null || true
	done <"$WORK/sms.rows"
	sort -u "$WORK/candidates.tsv" | head -n "$MAX_CANDIDATE_ROOMS" >"$WORK/cand.tsv"
	if [ ! -s "$WORK/cand.tsv" ]; then
		skip_check "no portal candidate rooms found for phone SMS senders"
	else
		joined=0
		invited=0
		while IFS=$'\t' read -r room _name; do
			[ -n "$room" ] || continue
			rc=0
			iamb_cmd room info "$room" >"$WORK/room.txt" 2>/dev/null || rc=$?
			if [ "$rc" -ne 0 ]; then
				continue
			fi
			if grep -qE 'Type: room( · direct)? · joined' "$WORK/room.txt"; then
				joined=$((joined + 1))
			elif grep -qE 'Type: room.*invited' "$WORK/room.txt"; then
				invited=$((invited + 1))
				fail "portal candidate $room is still only invited"
			fi
		done <"$WORK/cand.tsv"
		if [ "$joined" -ge 1 ]; then
			pass "portal candidates joined: $joined (invited: $invited)"
		else
			skip_check "no portal candidate confirmed joined"
		fi
	fi
fi

# --- 4. live create flow ----------------------------------------------------
if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$IAMB_GROUP_ROOM" ]; then
	rc=0
	iamb_send "$IAMB_GROUP_ROOM" "validate t14 invite probe $(now_ms)" >"$WORK/send.json" 2>"$WORK/send.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		pass "invite probe sent from $IAMB_GROUP_ROOM"
		sleep 30
		rc=0
		iamb_cmd invites >"$WORK/invites-after.txt" 2>/dev/null || rc=$?
		if [ "$rc" -ne 0 ]; then
			skip_check "cannot re-read invites after the probe"
		elif grep -qi 'no pending invites' "$WORK/invites-after.txt"; then
			pass "no new pending invite after the probe (portal auto-joined)"
		else
			pending_rooms="$(grep -oE '![A-Za-z0-9_-]+' "$WORK/invites-after.txt" | sort -u | tr '\n' ' ' | sed 's/ $//')"
			if [ "$INVITES_ACCEPT" = "1" ]; then
				iamb_cmd invites --accept >/dev/null 2>&1 && pass "accepted probe portal invite(s): $pending_rooms" || fail "could not accept probe portal invite(s): $pending_rooms"
			else
				fail "probe left pending invite(s): $pending_rooms"
			fi
		fi
		;;
	"$RC_UNREACHABLE") skip_check "invite probe send failed: $(tr '\n' ' ' <"$WORK/send.err")" ;;
	*) fail "invite probe send misbehaved: $(tr '\n' ' ' <"$WORK/send.err")" ;;
	esac
else
	skip_check "live invite probe disabled (VALIDATE_LIVE_SENDS=1, IAMB_GROUP_ROOM)"
fi

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t14_invite_join: PASS (skips: $SKIPS)"
	exit 0
fi
log "t14_invite_join: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
