#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t05_attachments.sh — acceptance step 4 (priority): attachments are byte-exact
# in both directions (full SHA-256) and the fixture fits the MMS size limit.
#
# State checks (always):
#   1. fixtures/cat.gif exists, is under MMS_SIZE_LIMIT_BYTES and hashes.
#   2. the fixture can be pushed to the phone and read back byte-exact.
#   3. any existing MMS part on the phone is hashable (evidence only).
#
# Live checks (opt-in: VALIDATE_LIVE_SENDS=1):
#   4. inbound: send cat.gif from IAMB_PHONE_ROOM (iamb --attach), then hash the
#      new MMS part on the phone and compare with the fixture.
#   5. outbound: control.SEND a cat.gif copy to ATTACHMENT_TARGET_NUMBER, then
#      hash the new outgoing MMS part and compare with the fixture.
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t05.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_PHONE_ROOM="${IAMB_PHONE_ROOM:-}"
ATTACHMENT_TARGET_NUMBER="${ATTACHMENT_TARGET_NUMBER:-}"
MMS_SIZE_LIMIT_BYTES="${MMS_SIZE_LIMIT_BYTES:-307200}"
FIXTURE="$FIXTURES_DIR/cat.gif"

log "t05_attachments: ADB_SSH=${ADB_SSH:-<local>} fixture=$FIXTURE limit=${MMS_SIZE_LIMIT_BYTES}B"

# --- 1. fixture -------------------------------------------------------------
if [ ! -f "$FIXTURE" ]; then
	fail "fixture $FIXTURE is missing (run scripts/validate/fixtures/make-cat-gif.sh)"
	log "t05_attachments: summary"; exit 1
fi
FIXTURE_SHA="$(sha256_file "$FIXTURE")"
FIXTURE_SIZE="$(stat --format=%s "$FIXTURE")"
FIXTURE_REMOTE_NAME="validate-cat.gif"
pass "fixture $(basename "$FIXTURE"): ${FIXTURE_SIZE}B sha256=${FIXTURE_SHA:0:16}…"

if [ "$FIXTURE_SIZE" -le "$MMS_SIZE_LIMIT_BYTES" ]; then
	pass "fixture fits the MMS size limit (${FIXTURE_SIZE}B <= ${MMS_SIZE_LIMIT_BYTES}B)"
else
	fail "fixture exceeds the MMS size limit (${FIXTURE_SIZE}B > ${MMS_SIZE_LIMIT_BYTES}B)"
fi

# Verify the GIF magic bytes so a broken generator is caught early.
magic="$(head -c 6 "$FIXTURE" | od -An -tx1 | tr -d ' \n')"
case "$magic" in
474946383961|474946383761) pass "fixture has GIF magic bytes ($magic)" ;;
*) fail "fixture is not a GIF (magic $magic)" ;;
esac

# --- 2. push + read back ----------------------------------------------------
rc=0
adb_device_online || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "phone $ADB_SERIAL not reachable (adb); push/read-back checks skipped"
else
	rc=0
	device_path="$(prepare_attachment "$FIXTURE" "$FIXTURE_REMOTE_NAME")" || rc=$?
	case "$rc" in
	"$RC_OK") ;;
	*) skip_check "could not push fixture to the phone" ;;
	esac
	if [ "$rc" -eq 0 ]; then
		readback_sha="$(phone_read_file "$device_path" | sha256sum | cut -d ' ' -f 1)"
		if [ "$readback_sha" = "$FIXTURE_SHA" ]; then
			pass "fixture pushed to $device_path reads back byte-exact ($readback_sha)"
		else
			fail "fixture read-back from $device_path differs (got ${readback_sha:0:16}…, want ${FIXTURE_SHA:0:16}…)"
		fi

		# --- 5. outbound live send via the app control surface -----------------
		role="$(app_default_sms_role 2>/dev/null || true)"
		if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$ATTACHMENT_TARGET_NUMBER" ] && [ "$role" = "true" ]; then
			before_ms="$(now_ms)"
			rc=0
			control_send "$ATTACHMENT_TARGET_NUMBER" "validate t05 outbound probe $before_ms" "$device_path" >"$WORK/out-send.json" 2>"$WORK/out-send.err" || rc=$?
			case "$rc" in
			"$RC_OK")
				mode="$(jq -r '.mode // "?"' "$WORK/out-send.json")"
				pass "outbound attachment send accepted (mode=$mode, message_id=$(jq -r '.message_id // "?"' "$WORK/out-send.json"))"
				out_pdu=""
				wait_for_attempts=0
				while [ "$wait_for_attempts" -lt 36 ]; do
					out_pdu="$(phone_mms_rows_since "$before_ms" | tail -n 1)"
					[ -n "$out_pdu" ] && break
					wait_for_attempts=$((wait_for_attempts + 1))
					sleep 5
				done
				if [ -z "$out_pdu" ]; then
					fail "outbound attachment send: no new MMS row on the phone within 180s"
				else
					out_pdu_id="$(cut -d'|' -f1 <<<"$out_pdu")"
					pass "phone recorded outgoing MMS pdu id=$out_pdu_id"
					part_row="$(phone_mms_parts "$out_pdu_id" | awk -F'|' '$2 ~ /^image\// { print; exit }')"
					if [ -z "$part_row" ]; then
						fail "outgoing MMS pdu $out_pdu_id has no image part"
					else
						part_id="$(cut -d'|' -f1 <<<"$part_row")"
						part_sha="$(phone_part_sha256 "$part_id")"
						if [ "$part_sha" = "$FIXTURE_SHA" ]; then
							pass "outbound MMS part $part_id is byte-exact ($part_sha)"
						else
							fail "outbound MMS part $part_id differs (got ${part_sha:0:16}…, want ${FIXTURE_SHA:0:16}…)"
						fi
					fi
				fi
				;;
			"$RC_UNREACHABLE") skip_check "outbound attachment send: no control verdict ($(tr '\n' ' ' <"$WORK/out-send.err"))" ;;
			*) fail "outbound attachment send misbehaved: $(tr '\n' ' ' <"$WORK/out-send.err")" ;;
			esac
		else
			if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$ATTACHMENT_TARGET_NUMBER" ] && [ "$role" != "true" ]; then
				skip_check "outbound live attachment send: app is not the default SMS app (default_sms_role=${role:-unknown}); MMS sends are refused by design"
			else
				skip_check "outbound live attachment send disabled (VALIDATE_LIVE_SENDS=1, ATTACHMENT_TARGET_NUMBER)"
			fi
		fi
	fi
fi

# --- 3. existing MMS parts on the phone (state evidence) --------------------
rc=0
recent_pdu="$(phone_mms_rows_since 0 2>/dev/null | tail -n 1)" || rc=$?
if [ "$rc" -ne 0 ] || [ -z "$recent_pdu" ]; then
	skip_check "no MMS rows on the phone to inspect"
else
	pdu_id="$(cut -d'|' -f1 <<<"$recent_pdu")"
	parts="$(phone_mms_parts "$pdu_id" 2>/dev/null || true)"
	image_part="$(awk -F'|' '$2 ~ /^image\// { print $1; exit }' <<<"$parts")"
	if [ -n "$image_part" ]; then
		sha="$(phone_part_sha256 "$image_part" 2>/dev/null || true)"
		if [ -n "$sha" ]; then
			pass "existing MMS pdu $pdu_id part $image_part hashable (sha256=${sha:0:16}…)"
		else
			skip_check "existing MMS pdu $pdu_id part $image_part payload not readable"
		fi
	else
		skip_check "newest MMS pdu $pdu_id has no image part"
	fi
fi

# --- 4. inbound live send ---------------------------------------------------
if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$IAMB_PHONE_ROOM" ]; then
	before_ms="$(now_ms)"
	rc=0
	iamb_send_file "$IAMB_PHONE_ROOM" "$FIXTURE" "$FIXTURE_REMOTE_NAME" >"$WORK/in-send.json" 2>"$WORK/in-send.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		att="$(jq -r '.attachments.sent // 0' "$WORK/in-send.json")"
		pass "inbound probe attachment sent from iamb room $IAMB_PHONE_ROOM (attachments=$att, at ${before_ms}ms)"
		in_pdu=""
		attempts=0
		while [ "$attempts" -lt 36 ]; do
			in_pdu="$(phone_mms_rows_since "$before_ms" | tail -n 1)"
			[ -n "$in_pdu" ] && break
			attempts=$((attempts + 1))
			sleep 5
		done
		if [ -z "$in_pdu" ]; then
			fail "inbound probe: no new MMS row on the phone within 180s"
		else
			pdu_id="$(cut -d'|' -f1 <<<"$in_pdu")"
			pass "phone recorded inbound MMS pdu id=$pdu_id"
			part_row="$(phone_mms_parts "$pdu_id" | awk -F'|' '$2 ~ /^image\// { print; exit }')"
			if [ -z "$part_row" ]; then
				fail "inbound MMS pdu $pdu_id has no image part"
			else
				part_id="$(cut -d'|' -f1 <<<"$part_row")"
				part_sha="$(phone_part_sha256 "$part_id")"
				if [ "$part_sha" = "$FIXTURE_SHA" ]; then
					pass "inbound MMS part $part_id is byte-exact ($part_sha)"
				else
					fail "inbound MMS part $part_id differs (got ${part_sha:0:16}…, want ${FIXTURE_SHA:0:16}…)"
				fi
			fi
		fi
		;;
	"$RC_UNREACHABLE") skip_check "inbound probe send failed: $(tr '\n' ' ' <"$WORK/in-send.err")" ;;
	*) fail "inbound probe send misbehaved: $(tr '\n' ' ' <"$WORK/in-send.err")" ;;
	esac
else
	skip_check "inbound live attachment probe disabled (VALIDATE_LIVE_SENDS=1, IAMB_PHONE_ROOM)"
fi

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t05_attachments: PASS (skips: $SKIPS)"
	exit 0
fi
log "t05_attachments: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
