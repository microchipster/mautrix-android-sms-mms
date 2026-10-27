#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t16_gif_roundtrip.sh — animated GIF round-trip in both directions with full
# SHA-256 equality and a content-type assertion.
#
# State checks (always):
#   1. fixtures/cat.gif exists, is a GIF, fits MMS_SIZE_LIMIT_BYTES and hashes;
#   2. the fixture round-trips through adb (push + read-back) byte-exact.
#
# Live checks (opt-in: VALIDATE_LIVE_SENDS=1):
#   3. inbound: iamb `--attach` the fixture into IAMB_PHONE_ROOM, then hash the
#      new inbound MMS image part and assert part.ct == image/gif;
#   4. outbound: control.SEND the pushed fixture to MEDIA_TARGET_NUMBER, then
#      hash the new outgoing MMS image part and assert part.ct == image/gif.
#
# Portal mirrors (m.image) are checked when EXPECT_PORTAL_MIRROR=1 (default).
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t16.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_PHONE_ROOM="${IAMB_PHONE_ROOM:-}"
MEDIA_TARGET_NUMBER="${MEDIA_TARGET_NUMBER:-}"
EXPECT_PORTAL_MIRROR="${EXPECT_PORTAL_MIRROR:-1}"
PORTAL_MIRROR_WAIT="${PORTAL_MIRROR_WAIT:-60}"
FIXTURE="$FIXTURES_DIR/cat.gif"
REMOTE_NAME="validate-cat.gif"

log "t16_gif_roundtrip: fixture=$FIXTURE live=$VALIDATE_LIVE_SENDS target=${MEDIA_TARGET_NUMBER:-<unset>}"

# ---------------------------------------------------------------------------
# 1. fixture
# ---------------------------------------------------------------------------
if [ ! -f "$FIXTURE" ]; then
	fail "fixture $FIXTURE is missing (run scripts/validate/fixtures/make-cat-gif.sh)"
	log "t16_gif_roundtrip: summary"
	exit 1
fi
FIXTURE_SHA="$(sha256_file "$FIXTURE")"
FIXTURE_SIZE="$(stat --format=%s "$FIXTURE")"
pass "fixture $(basename "$FIXTURE"): ${FIXTURE_SIZE}B sha256=${FIXTURE_SHA:0:16}…"

if [ "$FIXTURE_SIZE" -le "$MMS_SIZE_LIMIT_BYTES" ]; then
	pass "fixture fits the MMS size limit (${FIXTURE_SIZE}B <= ${MMS_SIZE_LIMIT_BYTES}B)"
else
	fail "fixture exceeds the MMS size limit (${FIXTURE_SIZE}B > ${MMS_SIZE_LIMIT_BYTES}B)"
fi
magic="$(head -c 6 "$FIXTURE" | od -An -tx1 | tr -d ' \n')"
case "$magic" in
474946383961|474946383761) pass "fixture has GIF magic bytes ($magic)" ;;
*) fail "fixture is not a GIF (magic $magic)" ;;
esac

# ---------------------------------------------------------------------------
# 2. phone + push/read-back
# ---------------------------------------------------------------------------
rc=0
adb_device_online || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "phone $ADB_SERIAL not reachable (adb); live checks skipped"
	log "t16_gif_roundtrip: summary"
	[ "$FAILURES" -eq 0 ] && exit 0 || exit 1
fi
pass "phone $ADB_SERIAL is online"

device_path=""
rc=0
device_path="$(prepare_attachment "$FIXTURE" "$REMOTE_NAME")" || rc=$?
if [ "$rc" -ne 0 ] || [ -z "$device_path" ]; then
	skip_check "could not push the fixture to the phone"
	device_path=""
else
	readback_sha="$(phone_read_file "$device_path" | sha256sum | cut -d ' ' -f 1)"
	if [ "$readback_sha" = "$FIXTURE_SHA" ]; then
		pass "fixture pushed to the device and read back byte-exact (${readback_sha:0:16}…)"
	else
		fail "fixture read-back differs (got ${readback_sha:0:16}…, want ${FIXTURE_SHA:0:16}…)"
	fi
fi

# find_new_pdu_with_matching_part SINCE_ID CT_PREFIX TIMEOUT_S FIXTURE_SHA [FIXTURE_SIZE]
# — print "PDU_ID|part row" for the first new MMS part that matches the fixture
# hash (preferred) or, as a fallback for degradation reporting, a part with the
# expected content type and payload size. Empty when nothing arrives.
find_new_pdu_with_matching_part() {
	local since="$1" prefix="$2" timeout="$3" fixture_sha="${4:-}" fixture_size="${5:-}"
	local attempts=0 pid prow part_id part_sha part_path part_size fallback_size="" fallback_any=""
	while [ "$attempts" -lt "$timeout" ]; do
		while IFS='|' read -r pid _t _d _b; do
			[ -n "$pid" ] || continue
			while IFS= read -r prow; do
				[ -n "$prow" ] || continue
				part_id="$(cut -d'|' -f1 <<<"$prow")"
				part_sha="$(phone_part_sha256 "$part_id" 2>/dev/null || true)"
				if [ -n "$fixture_sha" ] && [ "$part_sha" = "$fixture_sha" ]; then
					printf '%s|%s\n' "$pid" "$prow"
					return 0
				fi
				[ -n "$fallback_any" ] || fallback_any="$pid|$prow"
				if [ -z "$fallback_size" ] && [ -n "$fixture_size" ]; then
					part_path="$(cut -d'|' -f6 <<<"$prow")"
					part_size=""
					[ -n "$part_path" ] && part_size="$(phone_read_file "$part_path" 2>/dev/null | wc -c | tr -d ' ')"
					if [ "$part_size" = "$fixture_size" ]; then
						fallback_size="$pid|$prow"
					fi
				fi
			done < <(phone_mms_parts "$pid" 2>/dev/null | awk -F'|' -v p="$prefix" '$2 ~ "^" p { print }')
		done < <(phone_pdu_rows_newer_than "$since" 2>/dev/null)
		attempts=$((attempts + 1))
		sleep 5
	done
	if [ -n "$fallback_size" ]; then
		printf '%s\n' "$fallback_size"
	elif [ -n "$fallback_any" ]; then
		printf '%s\n' "$fallback_any"
	fi
	return 0
}

# portal_room_for_address ADDRESS — first non-jmpchat room whose name contains it.
portal_room_for_address() {
	local address="$1" rows room
	rows="$(room_ids_matching_name "$address" 2>/dev/null || true)"
	while IFS=$'\t' read -r room _name; do
		[ -n "$room" ] || continue
		case "$_name" in
		*jmp*|*cheogram*|*@cheogram.com*) continue ;;
		esac
		printf '%s\n' "$room"
		return 0
	done <<<"$rows"
	return 0
}

# check_image_mirror LABEL SINCE_MS ROOM MSGSPEC [EXPECTED_LEN] — PASS when the
# portal shows a new message event matching MSGSPEC ("any" or a regex) and,
# when EXPECTED_LEN is non-zero, that body length. Text-only fallbacks are
# reported precisely (the bridge can drop an attachment and post the caption).
check_image_mirror() {
	local label="$1" since_ms="$2" room="$3" msgspec="${4:-m.image}" expected_len="${5:-0}" events rc=0 started now observed
	if [ -z "$room" ]; then
		if [ "$EXPECT_PORTAL_MIRROR" = "1" ]; then
			fail "$label: no Matrix portal room found for the probe sender"
		else
			skip_check "$label: no Matrix portal room found for the probe sender"
		fi
		return 0
	fi
	started="$(date +%s)"
	events=""
	while :; do
		rc=0
		events="$(portal_events_since "$room" "$since_ms" 2>/dev/null)" || rc=$?
		if [ "$rc" -eq "$RC_UNREACHABLE" ]; then
			skip_check "$label: iamb unavailable for portal mirror checks"
			return 0
		fi
		events="$(printf '%s\n' "$events" | jq -c --arg t "$msgspec" --argjson len "$expected_len" \
			'select(($t == "any" or (.msgtype | test($t))) and ($len == 0 or .body_len == $len))' 2>/dev/null || true)"
		[ -n "$events" ] && break
		now="$(date +%s)"
		[ $((now - started)) -ge "$PORTAL_MIRROR_WAIT" ] && break
		sleep 5
	done
	if [ -n "$events" ]; then
		observed="$(printf '%s' "$events" | head -n 1 | jq -r '.msgtype')"
		pass "$label: portal $room mirrored the image (observed msgtype=$observed; $(printf '%s' "$events" | head -n 1))"
		if [ "$observed" = "m.text" ] || [ "$observed" = "m.notice" ]; then
			log "note: $label mirror is a text/notice event — the bridge did not upload the attachment file (caption-only fallback)"
		fi
	elif [ "$EXPECT_PORTAL_MIRROR" = "1" ]; then
		fail "$label: portal $room did not mirror the MMS image within ${PORTAL_MIRROR_WAIT}s"
	else
		skip_check "$label: portal mirror not observed within ${PORTAL_MIRROR_WAIT}s (EXPECT_PORTAL_MIRROR=0)"
	fi
}

# ---------------------------------------------------------------------------
# 3. inbound GIF probe
# ---------------------------------------------------------------------------
if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$IAMB_PHONE_ROOM" ]; then
	# The probe sender for the portal lookup: the newest inbound address.
	since_24h=$(( $(now_ms) - 86400000 ))
	PROBE_SENDER="$(sqlite_query "select address from sms where type = 1 and date >= $since_24h order by date desc limit 1;" 2>/dev/null | tail -n 1)"
	before_pdu="$(phone_max_pdu_id 2>/dev/null || echo 0)"
	started_ms="$(now_ms)"
	rc=0
	iamb_send_file "$IAMB_PHONE_ROOM" "$FIXTURE" "$REMOTE_NAME" >"$WORK/in-send.json" 2>"$WORK/in-send.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		att="$(jq -r '.attachments.sent // 0' "$WORK/in-send.json")"
		pass "inbound GIF probe sent from iamb room $IAMB_PHONE_ROOM (attachments=$att, fixture sha256=${FIXTURE_SHA:0:16}…)"
		found="$(find_new_pdu_with_matching_part "$before_pdu" "image/" 36 "$FIXTURE_SHA" "$FIXTURE_SIZE")"
		if [ -z "$found" ]; then
			fail "inbound GIF probe: no new MMS image part on the phone within 180s"
		else
			pdu_id="$(cut -d'|' -f1 <<<"$found")"
			part_row="$(cut -d'|' -f2- <<<"$found")"
			part_id="$(cut -d'|' -f1 <<<"$part_row")"
			part_ct="$(cut -d'|' -f2 <<<"$part_row")"
			part_sha="$(phone_part_sha256 "$part_id")"
			pass "inbound MMS pdu=$pdu_id part=$part_id ct=$part_ct sha256=${part_sha:0:16}…"
			if [ "$part_ct" = "image/gif" ]; then
				pass "inbound part content-type is image/gif"
			else
				fail "inbound part content-type is '$part_ct' (want image/gif)"
			fi
			if [ "$part_sha" = "$FIXTURE_SHA" ]; then
				pass "inbound GIF is byte-exact ($part_sha)"
			else
				fail "inbound GIF differs (got ${part_sha:0:16}…, want ${FIXTURE_SHA:0:16}…)"
			fi
		fi
		check_image_mirror "inbound GIF" "$((started_ms - 2000))" "$(portal_room_for_address "${PROBE_SENDER:-}")" "m.image" 0
		;;
	"$RC_UNREACHABLE") skip_check "inbound GIF probe send failed: $(tr '\n' ' ' <"$WORK/in-send.err")" ;;
	*) fail "inbound GIF probe send misbehaved: $(tr '\n' ' ' <"$WORK/in-send.err")" ;;
	esac
else
	skip_check "inbound GIF probe disabled (set VALIDATE_LIVE_SENDS=1 and IAMB_PHONE_ROOM)"
fi

# ---------------------------------------------------------------------------
# 4. outbound GIF probe
# ---------------------------------------------------------------------------
role="$(app_default_sms_role 2>/dev/null || true)"
if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$MEDIA_TARGET_NUMBER" ] && [ -n "$device_path" ] && [ "$role" = "true" ]; then
	before_pdu="$(phone_max_pdu_id 2>/dev/null || echo 0)"
	started_ms="$(now_ms)"
	sent_text="validate t16 outbound GIF probe $started_ms"
	sent_len="$(jq -n --arg s "$sent_text" '$s | length')"
	rc=0
	control_send "$MEDIA_TARGET_NUMBER" "$sent_text" "$device_path" >"$WORK/out-send.json" 2>"$WORK/out-send.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		mode="$(jq -r '.mode // "?"' "$WORK/out-send.json")"
		message_id="$(jq -r '.message_id // empty' "$WORK/out-send.json")"
		pass "outbound GIF send accepted (mode=$mode message_id=${message_id:-?})"
		found=""
		if [ -n "$message_id" ] && [ "$message_id" -gt 0 ] 2>/dev/null; then
			attempts=0
			while [ "$attempts" -lt 36 ]; do
				row="$(phone_mms_part_row "$message_id" "image/" 2>/dev/null || true)"
				if [ -n "$row" ]; then
					found="$message_id|$row"
					break
				fi
				attempts=$((attempts + 1))
				sleep 5
			done
		fi
		if [ -z "$found" ]; then
			found="$(find_new_pdu_with_matching_part "$before_pdu" "image/" 36 "$FIXTURE_SHA" "$FIXTURE_SIZE")"
		fi
		if [ -z "$found" ]; then
			fail "outbound GIF probe: no new MMS image part on the phone within 180s"
		else
			pdu_id="$(cut -d'|' -f1 <<<"$found")"
			part_row="$(cut -d'|' -f2- <<<"$found")"
			part_id="$(cut -d'|' -f1 <<<"$part_row")"
			part_ct="$(cut -d'|' -f2 <<<"$part_row")"
			part_sha="$(phone_part_sha256 "$part_id")"
			pass "outbound MMS pdu=$pdu_id part=$part_id ct=$part_ct sha256=${part_sha:0:16}…"
			if [ "$part_ct" = "image/gif" ]; then
				pass "outbound part content-type is image/gif"
			else
				fail "outbound part content-type is '$part_ct' (want image/gif)"
			fi
			if [ "$part_sha" = "$FIXTURE_SHA" ]; then
				pass "outbound GIF is byte-exact ($part_sha)"
			else
				fail "outbound GIF differs (got ${part_sha:0:16}…, want ${FIXTURE_SHA:0:16}…)"
			fi
		fi
		check_image_mirror "outbound GIF" "$((started_ms - 2000))" "$(portal_room_for_address "$MEDIA_TARGET_NUMBER")" "any" "$sent_len"
		;;
	"$RC_UNREACHABLE") skip_check "outbound GIF send: no control verdict: $(tr '\n' ' ' <"$WORK/out-send.err")" ;;
	*) fail "outbound GIF send misbehaved: $(tr '\n' ' ' <"$WORK/out-send.err")" ;;
	esac
elif [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ "$role" != "true" ]; then
	skip_check "outbound GIF probe: app is not the default SMS app (default_sms_role=${role:-unknown})"
else
	skip_check "outbound GIF probe disabled (set VALIDATE_LIVE_SENDS=1 and MEDIA_TARGET_NUMBER)"
fi

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t16_gif_roundtrip: PASS (skips: $SKIPS)"
	exit 0
fi
log "t16_gif_roundtrip: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
