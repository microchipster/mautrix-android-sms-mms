#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t18_media_variants.sh — media variants beyond images: video and audio.
#
# Fixtures (generated in-house by make-media-fixtures.sh): tiny.mp4 (H.264),
# tiny.m4a (AAC) and tiny.ogg (Opus).
#
# Live checks (opt-in: VALIDATE_LIVE_SENDS=1):
#   1. inbound video: iamb `--attach tiny.mp4` into IAMB_PHONE_ROOM; the new
#      MMS part must be a video/* part and (MEDIA_BYTE_EXACT_INBOUND=1, the
#      default) byte-exact — otherwise the degradation is reported;
#   2. inbound audio: same with tiny.ogg (audio/* part);
#   3. outbound audio: control.SEND tiny.m4a to MEDIA_TARGET_NUMBER; the new
#      outgoing part must be audio/* and byte-exact.
#
# Portal mirrors (m.video / m.audio) are checked when EXPECT_PORTAL_MIRROR=1.
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t18.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_PHONE_ROOM="${IAMB_PHONE_ROOM:-}"
MEDIA_TARGET_NUMBER="${MEDIA_TARGET_NUMBER:-}"
EXPECT_PORTAL_MIRROR="${EXPECT_PORTAL_MIRROR:-1}"
MEDIA_BYTE_EXACT_INBOUND="${MEDIA_BYTE_EXACT_INBOUND:-1}"
PORTAL_MIRROR_WAIT="${PORTAL_MIRROR_WAIT:-60}"

MP4_FIXTURE="$FIXTURES_DIR/tiny.mp4"
M4A_FIXTURE="$FIXTURES_DIR/tiny.m4a"
OGG_FIXTURE="$FIXTURES_DIR/tiny.ogg"

log "t18_media_variants: live=$VALIDATE_LIVE_SENDS target=${MEDIA_TARGET_NUMBER:-<unset>} byte_exact_inbound=$MEDIA_BYTE_EXACT_INBOUND"

# ---------------------------------------------------------------------------
# 1. fixtures
# ---------------------------------------------------------------------------
for f in "$MP4_FIXTURE" "$M4A_FIXTURE" "$OGG_FIXTURE"; do
	if [ ! -f "$f" ]; then
		fail "fixture $f is missing (run scripts/validate/fixtures/make-media-fixtures.sh)"
	fi
done
[ "$FAILURES" -eq 0 ] || { log "t18_media_variants: summary"; exit 1; }
MP4_SHA="$(sha256_file "$MP4_FIXTURE")"
M4A_SHA="$(sha256_file "$M4A_FIXTURE")"
OGG_SHA="$(sha256_file "$OGG_FIXTURE")"
for spec in "tiny.mp4:$MP4_SHA" "tiny.m4a:$M4A_SHA" "tiny.ogg:$OGG_SHA"; do
	name="${spec%%:*}"
	sha="${spec##*:}"
	size="$(stat --format=%s "$FIXTURES_DIR/$name")"
	if [ "$size" -le "$MMS_SIZE_LIMIT_BYTES" ]; then
		pass "fixture $name: ${size}B sha256=${sha:0:16}… fits the MMS limit"
	else
		fail "fixture $name exceeds the MMS size limit (${size}B)"
	fi
done

# ---------------------------------------------------------------------------
# 2. phone
# ---------------------------------------------------------------------------
rc=0
adb_device_online || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "phone $ADB_SERIAL not reachable (adb)"; log "t18_media_variants: summary"
	[ "$FAILURES" -eq 0 ] && exit 0 || exit 1
fi
pass "phone $ADB_SERIAL is online"

device_m4a_path=""
rc=0
device_m4a_path="$(prepare_attachment "$M4A_FIXTURE" "validate-tiny.m4a")" || rc=$?
if [ "$rc" -ne 0 ] || [ -z "$device_m4a_path" ]; then
	skip_check "could not push tiny.m4a to the phone"
	device_m4a_path=""
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

# assert_part LABEL PDU_ID PART_ROW FIXTURE_SHA FIXTURE_SIZE PREFIX BYTE_EXACT
assert_part() {
	local label="$1" pdu_id="$2" part_row="$3" fixture_sha="$4" fixture_size="$5" prefix="$6" byte_exact="$7"
	local part_id part_ct part_sha part_path part_size
	part_id="$(cut -d'|' -f1 <<<"$part_row")"
	part_ct="$(cut -d'|' -f2 <<<"$part_row")"
	part_sha="$(phone_part_sha256 "$part_id" 2>/dev/null || true)"
	part_path="$(phone_part_data_path "$part_id" 2>/dev/null || true)"
	part_size=""
	if [ -n "$part_path" ]; then
		part_size="$(phone_read_file "$part_path" 2>/dev/null | wc -c | tr -d ' ')"
	fi
	pass "$label: MMS pdu=$pdu_id part=$part_id ct=$part_ct size=${part_size:-?}B fixture_size=${fixture_size}B sha256=${part_sha:0:16}…"
	case "$part_ct" in
	"$prefix"*) pass "$label: part content-type is $prefix* ($part_ct)" ;;
	*) fail "$label: part content-type is '$part_ct' (want $prefix*)"; return 0 ;;
	esac
	if [ "$part_sha" = "$fixture_sha" ]; then
		pass "$label: payload is byte-exact ($part_sha)"
	elif [ "$byte_exact" = "1" ]; then
		fail "$label: payload differs (got ${part_sha:0:16}…, want ${fixture_sha:0:16}…; sizes ${part_size:-?}B vs ${fixture_size}B) — transport degradation"
	else
		skip_check "$label: payload differs (got ${part_sha:0:16}…, want ${fixture_sha:0:16}…; sizes ${part_size:-?}B vs ${fixture_size}B) — degradation documented, not asserted (MEDIA_BYTE_EXACT_INBOUND=0)"
	fi
}

check_media_mirror() {
	local label="$1" since_ms="$2" room="$3" msgspec="$4" expected_len="${5:-0}" events rc=0 started now observed
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
		pass "$label: portal $room mirrored the media (observed msgtype=$observed; $(printf '%s' "$events" | head -n 1))"
		if [ "$msgspec" = "any" ]; then
			log "note: $label echo carried msgtype=$observed (re-uploading the phone-originated attachment is not expected)"
		fi
	elif [ "$EXPECT_PORTAL_MIRROR" = "1" ]; then
		fail "$label: portal $room did not mirror the media message within ${PORTAL_MIRROR_WAIT}s"
	else
		skip_check "$label: portal mirror not observed within ${PORTAL_MIRROR_WAIT}s (EXPECT_PORTAL_MIRROR=0)"
	fi
}

# run_inbound_media_probe LABEL FIXTURE REMOTE_NAME PREFIX BYTE_EXACT MSGTYPE
run_inbound_media_probe() {
	local label="$1" fixture="$2" remote_name="$3" prefix="$4" byte_exact="$5" msgtype="$6"
	local fixture_sha fixture_size before_pdu started_ms rc=0 found
	fixture_sha="$(sha256_file "$fixture")"
	fixture_size="$(stat --format=%s "$fixture")"
	since_24h=$(( $(now_ms) - 86400000 ))
	PROBE_SENDER="$(sqlite_query "select address from sms where type = 1 and date >= $since_24h order by date desc limit 1;" 2>/dev/null | tail -n 1)"
	before_pdu="$(phone_max_pdu_id 2>/dev/null || echo 0)"
	started_ms="$(now_ms)"
	rc=0
	iamb_send_file "$IAMB_PHONE_ROOM" "$fixture" "$remote_name" >"$WORK/$label.send.json" 2>"$WORK/$label.send.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		pass "$label: inbound probe sent from $IAMB_PHONE_ROOM (fixture $(basename "$fixture") ${fixture_size}B sha256=${fixture_sha:0:16}…)"
		;;
	"$RC_UNREACHABLE") skip_check "$label: inbound probe send failed: $(tr '\n' ' ' <"$WORK/$label.send.err")"; return 0 ;;
	*) fail "$label: inbound probe send misbehaved: $(tr '\n' ' ' <"$WORK/$label.send.err")"; return 0 ;;
	esac
	found="$(find_new_pdu_with_matching_part "$before_pdu" "$prefix" 36 "$fixture_sha" "$fixture_size")"
	if [ -z "$found" ]; then
		fail "$label: no new MMS $prefix* part on the phone within 180s"
	else
		assert_part "$label" "$(cut -d'|' -f1 <<<"$found")" "$(cut -d'|' -f2- <<<"$found")" "$fixture_sha" "$fixture_size" "$prefix" "$byte_exact"
	fi
	check_media_mirror "$label" "$((started_ms - 2000))" "$(portal_room_for_address "${PROBE_SENDER:-}")" "$msgtype"
}

# ---------------------------------------------------------------------------
# 3. inbound video (tiny.mp4)
# ---------------------------------------------------------------------------
if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$IAMB_PHONE_ROOM" ]; then
	run_inbound_media_probe "inbound video" "$MP4_FIXTURE" "validate-tiny.mp4" "video/" "$MEDIA_BYTE_EXACT_INBOUND" "m.video"
	run_inbound_media_probe "inbound audio" "$OGG_FIXTURE" "validate-tiny.ogg" "audio/" "$MEDIA_BYTE_EXACT_INBOUND" "m.audio"
else
	skip_check "inbound video/audio probes disabled (set VALIDATE_LIVE_SENDS=1 and IAMB_PHONE_ROOM)"
fi

# ---------------------------------------------------------------------------
# 4. outbound audio (tiny.m4a)
# ---------------------------------------------------------------------------
role="$(app_default_sms_role 2>/dev/null || true)"
if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$MEDIA_TARGET_NUMBER" ] && [ -n "$device_m4a_path" ] && [ "$role" = "true" ]; then
	before_pdu="$(phone_max_pdu_id 2>/dev/null || echo 0)"
	started_ms="$(now_ms)"
	sent_text="validate t18 outbound audio probe $started_ms"
	sent_len="$(jq -n --arg s "$sent_text" '$s | length')"
	rc=0
	control_send "$MEDIA_TARGET_NUMBER" "$sent_text" "$device_m4a_path" >"$WORK/out-send.json" 2>"$WORK/out-send.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		mode="$(jq -r '.mode // "?"' "$WORK/out-send.json")"
		message_id="$(jq -r '.message_id // empty' "$WORK/out-send.json")"
		pass "outbound audio send accepted (mode=$mode message_id=${message_id:-?})"
		found=""
		if [ -n "$message_id" ] && [ "$message_id" -gt 0 ] 2>/dev/null; then
			attempts=0
			while [ "$attempts" -lt 36 ]; do
				row="$(phone_mms_part_row "$message_id" "audio/" 2>/dev/null || true)"
				if [ -n "$row" ]; then
					found="$message_id|$row"
					break
				fi
				attempts=$((attempts + 1))
				sleep 5
			done
		fi
		if [ -z "$found" ]; then
			found="$(find_new_pdu_with_matching_part "$before_pdu" "audio/" 36 "$M4A_SHA" "$(stat --format=%s "$M4A_FIXTURE")")"
		fi
		if [ -z "$found" ]; then
			fail "outbound audio: no new MMS audio/* part on the phone within 180s"
		else
			assert_part "outbound audio" "$(cut -d'|' -f1 <<<"$found")" "$(cut -d'|' -f2- <<<"$found")" "$M4A_SHA" "$(stat --format=%s "$M4A_FIXTURE")" "audio/" "1"
		fi
		check_media_mirror "outbound audio" "$((started_ms - 2000))" "$(portal_room_for_address "$MEDIA_TARGET_NUMBER")" "any" "$sent_len"
		;;
	"$RC_UNREACHABLE") skip_check "outbound audio: no control verdict: $(tr '\n' ' ' <"$WORK/out-send.err")" ;;
	*) fail "outbound audio misbehaved: $(tr '\n' ' ' <"$WORK/out-send.err")" ;;
	esac
elif [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ "$role" != "true" ]; then
	skip_check "outbound audio probe: app is not the default SMS app (default_sms_role=${role:-unknown})"
else
	skip_check "outbound audio probe disabled (set VALIDATE_LIVE_SENDS=1 and MEDIA_TARGET_NUMBER)"
fi

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t18_media_variants: PASS (skips: $SKIPS)"
	exit 0
fi
log "t18_media_variants: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
