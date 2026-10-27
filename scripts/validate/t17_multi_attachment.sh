#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t17_multi_attachment.sh — one multi-attachment MMS must be ONE phone message.
#
# The upstream app bug sends one klinker Transaction per attachment (two MMS
# instead of one). The fork's BridgeSender builds a single Message with every
# attachment; this test proves that on the live phone: POST TWO images in one
# /v1/send request and require exactly ONE new pdu row with TWO image parts.
#
# Paths, tried in order:
#   A. APP_BRIDGE_URL is already reachable (operator-provided tunnel/host):
#      GET /v1/health, then POST /v1/send (bearer APP_BRIDGE_TOKEN when set).
#   B. the app HTTP API is reachable via `adb forward` on APP_BRIDGE_PORT:
#      when the app's static-token mode is already enabled (control.STATUS
#      enabled=true) and APP_BRIDGE_TOKEN is set, that token is used; when
#      APP_HTTP_ENABLE_TEMP=1 the test enables static-token mode through
#      control.CONFIG with a locally generated token and restores the
#      preference afterwards (the token is cleared, the port forward removed).
#   C. otherwise the Matrix-side path is documented and SKIPped: a Matrix media
#      message carries a single content URL, `iamb --attach` attaches one file
#      per event, and the bridge forwards one attachment per Matrix message —
#      so the harness cannot produce a multi-attachment send through Matrix.
#
# Outcome rules:
#   exactly one new MMS pdu with both image parts (byte-exact)  -> PASS
#   two (or more) new MMS pdus for the single request           -> FAIL (bug)
#   one MMS with fewer parts, or no MMS at all                  -> FAIL
#
# Evidence only: ids, part counts, content types, lengths, hashes. Tokens are
# generated in-memory and never logged.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$SCRIPT_DIR/lib.sh"

FAILURES=0
SKIPS=0

pass() { log "PASS: $*"; }
skip_check() { SKIPS=$((SKIPS + 1)); skip "$*"; }
fail() { FAILURES=$((FAILURES + 1)); printf '[validate] FAIL: %s\n' "$*" >&2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t17.XXXXXX")"

VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
MEDIA_TARGET_NUMBER="${MEDIA_TARGET_NUMBER:-}"
EXPECT_PORTAL_MIRROR="${EXPECT_PORTAL_MIRROR:-1}"
PORTAL_MIRROR_WAIT="${PORTAL_MIRROR_WAIT:-60}"
APP_BRIDGE_URL="${APP_BRIDGE_URL:-}"
APP_BRIDGE_TOKEN="${APP_BRIDGE_TOKEN:-}"
APP_BRIDGE_PORT="${APP_BRIDGE_PORT:-8391}"
APP_HTTP_ENABLE_TEMP="${APP_HTTP_ENABLE_TEMP:-0}"

GIF_FIXTURE="$FIXTURES_DIR/cat.gif"
PNG_FIXTURE="$FIXTURES_DIR/paw.png"
GIF_REMOTE_NAME="validate-cat.gif"
PNG_REMOTE_NAME="validate-paw.png"

log "t17_multi_attachment: target=${MEDIA_TARGET_NUMBER:-<unset>} live=$VALIDATE_LIVE_SENDS app_url=${APP_BRIDGE_URL:-<unset>} temp_enable=$APP_HTTP_ENABLE_TEMP"

# ---------------------------------------------------------------------------
# Cleanup: restore the app HTTP preference and the adb forward when we set them.
# ---------------------------------------------------------------------------
ENABLED_BY_US=0
FWD_PORT=""
restore_app_http() {
	if [ "$ENABLED_BY_US" = "1" ]; then
		drive_app_json config '{"bridge_enabled":false}' >/dev/null 2>&1 || log "restore: could not set bridge_enabled=false"
		drive_app_json config '{"bridge_token":""}' >/dev/null 2>&1 || log "restore: could not clear the generated token"
		log "restore: app static-token HTTP mode disabled again"
	fi
	if [ -n "$FWD_PORT" ]; then
		adb_forward_remove "$FWD_PORT" >/dev/null 2>&1 || true
		log "restore: removed adb forward tcp:$FWD_PORT"
	fi
}
trap 'restore_app_http; rm -rf "$WORK"' EXIT

# ---------------------------------------------------------------------------
# 1. fixtures
# ---------------------------------------------------------------------------
for f in "$GIF_FIXTURE" "$PNG_FIXTURE"; do
	if [ ! -f "$f" ]; then
		fail "fixture $f is missing (run scripts/validate/fixtures/make-cat-gif.sh and make-media-fixtures.sh)"
	fi
done
[ "$FAILURES" -eq 0 ] || { log "t17_multi_attachment: summary"; exit 1; }
GIF_SHA="$(sha256_file "$GIF_FIXTURE")"
PNG_SHA="$(sha256_file "$PNG_FIXTURE")"
GIF_SIZE="$(stat --format=%s "$GIF_FIXTURE")"
PNG_SIZE="$(stat --format=%s "$PNG_FIXTURE")"
if [ $((GIF_SIZE + PNG_SIZE)) -le "$MMS_SIZE_LIMIT_BYTES" ]; then
	pass "fixtures fit the MMS size limit (${GIF_SIZE}B + ${PNG_SIZE}B <= ${MMS_SIZE_LIMIT_BYTES}B)"
else
	fail "fixtures exceed the MMS size limit (${GIF_SIZE}B + ${PNG_SIZE}B > ${MMS_SIZE_LIMIT_BYTES}B)"
fi
pass "fixture hashes: cat.gif=${GIF_SHA:0:16}… paw.png=${PNG_SHA:0:16}…"

# ---------------------------------------------------------------------------
# 2. phone + rollback state
# ---------------------------------------------------------------------------
rc=0
adb_device_online || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "phone $ADB_SERIAL not reachable (adb)"; log "t17_multi_attachment: SKIP-ONLY run"; exit 0
fi
pass "phone $ADB_SERIAL is online"

role="$(app_default_sms_role 2>/dev/null || true)"
STATUS_JSON="$(drive_app_json status 2>/dev/null || true)"
[ -n "$STATUS_JSON" ] || STATUS_JSON='{}'
APP_HTTP_PREF_ENABLED="$(jq -r '.enabled // false' <<<"$STATUS_JSON")"
if [ "$role" != "true" ]; then
	skip_check "multi-attachment MMS send: app is not the default SMS app (default_sms_role=${role:-unknown}); MMS sends are refused by design"
	log "t17_multi_attachment: summary"
	[ "$FAILURES" -eq 0 ] && exit 0 || exit 1
fi

if [ "$VALIDATE_LIVE_SENDS" != "1" ] || [ -z "$MEDIA_TARGET_NUMBER" ]; then
	skip_check "multi-attachment probe disabled (set VALIDATE_LIVE_SENDS=1 and MEDIA_TARGET_NUMBER)"
	log "t17_multi_attachment: summary"
	[ "$FAILURES" -eq 0 ] && exit 0 || exit 1
fi

# ---------------------------------------------------------------------------
# 3. choose the app HTTP path
# ---------------------------------------------------------------------------
BASE=""
TOKEN=""
BASE_SOURCE=""
if [ -n "$APP_BRIDGE_URL" ]; then
	BASE="$APP_BRIDGE_URL"
	TOKEN="$APP_BRIDGE_TOKEN"
	BASE_SOURCE="APP_BRIDGE_URL"
elif [ "$APP_HTTP_ENABLE_TEMP" = "1" ]; then
	BASE="http://127.0.0.1:$APP_BRIDGE_PORT"
	BASE_SOURCE="temporary control.CONFIG enable"
elif [ "$APP_HTTP_PREF_ENABLED" = "true" ] && [ -n "$APP_BRIDGE_TOKEN" ]; then
	BASE="http://127.0.0.1:$APP_BRIDGE_PORT"
	TOKEN="$APP_BRIDGE_TOKEN"
	BASE_SOURCE="already-enabled app HTTP mode"
fi

if [ -z "$BASE" ]; then
	skip_check "multi-attachment MMS: the app HTTP API path is unavailable (not APP_BRIDGE_URL, app bridge_enabled=${APP_HTTP_PREF_ENABLED}, APP_HTTP_ENABLE_TEMP=0/APP_BRIDGE_TOKEN unset)"
	if [ "$APP_HTTP_PREF_ENABLED" != "true" ]; then
		skip_check "matrix-side multi-attachment is not expressible: a Matrix media event carries one content URL, iamb attaches one file per event and the bridge forwards one attachment per message (connector: downloadMatrixAttachment is single-attachment)"
	else
		skip_check "matrix-side multi-attachment is not expressible: same single-attachment limit on the Matrix leg"
	fi
	log "t17_multi_attachment: SKIP-ONLY run (no multi-attachment send produced)"
	exit 0
fi

if [ -z "$TOKEN" ] && [ "$APP_HTTP_ENABLE_TEMP" != "1" ] && [ "$BASE_SOURCE" != "APP_BRIDGE_URL" ]; then
	skip_check "multi-attachment MMS: app HTTP API needs a bearer token but APP_BRIDGE_TOKEN is unset"
	log "t17_multi_attachment: SKIP-ONLY run"
	exit 0
fi

# Temporary enable: generate a token, turn static-token mode on, forward the port.
if [ "$APP_HTTP_ENABLE_TEMP" = "1" ] && [ "$APP_HTTP_PREF_ENABLED" != "true" ]; then
	TOKEN="$(random_hex 32)"
	rc=0
	verdict="$(drive_app_json config "{\"bridge_enabled\":true,\"bridge_token\":\"$TOKEN\"}")" || rc=$?
	enabled_ok="false"
	if [ -n "${verdict:-}" ]; then
		enabled_ok="$(jq -r '.bridge_enabled // false' <<<"$verdict" 2>/dev/null || echo false)"
	fi
	if [ "$rc" -ne 0 ] || [ "$enabled_ok" != "true" ]; then
		fail "could not enable the app HTTP API via control.CONFIG (rc=$rc)"
		log "t17_multi_attachment: summary"
		exit 1
	fi
	ENABLED_BY_US=1
	pass "app static-token HTTP mode enabled temporarily (bridge_port=$APP_BRIDGE_PORT; preference is restored on exit)"
elif [ "$APP_HTTP_PREF_ENABLED" = "true" ] && [ "$APP_HTTP_ENABLE_TEMP" = "1" ]; then
	# Already enabled: only touch the port forward, never the preference.
	:
fi

if [ "$ENABLED_BY_US" = "1" ] || [ "$BASE_SOURCE" = "already-enabled app HTTP mode" ]; then
	rc=0
	adb_forward "$APP_BRIDGE_PORT" || rc=$?
	if [ "$rc" -ne 0 ]; then
		fail "could not set up adb forward tcp:$APP_BRIDGE_PORT"
		log "t17_multi_attachment: summary"
		exit 1
	fi
	FWD_PORT="$APP_BRIDGE_PORT"
fi

# Wait for the HTTP API to answer.
health=""
attempts=0
while [ "$attempts" -lt 15 ]; do
	health="$(app_http_health "$BASE" "$TOKEN" 2>/dev/null || true)"
	[ -n "$health" ] && break
	attempts=$((attempts + 1))
	sleep 2
done
if [ -z "$health" ]; then
	fail "app HTTP API at $BASE ($BASE_SOURCE) did not answer /v1/health"
	log "t17_multi_attachment: summary"
	exit 1
fi
if [ -n "$TOKEN" ]; then
	pass "app HTTP API reachable via $BASE_SOURCE (health ok)"
else
	pass "app HTTP API reachable via $BASE_SOURCE (health ok, no token needed)"
fi

# ---------------------------------------------------------------------------
# 4. one send request with TWO attachments
# ---------------------------------------------------------------------------
before_sms="$(phone_max_sms_id 2>/dev/null || echo 0)"
before_pdu="$(phone_max_pdu_id 2>/dev/null || echo 0)"
started_ms="$(now_ms)"

gif_b64="$(base64 --wrap=0 "$GIF_FIXTURE")"
png_b64="$(base64 --wrap=0 "$PNG_FIXTURE")"
jq -n \
	--arg to "$MEDIA_TARGET_NUMBER" \
	--arg gif "$gif_b64" \
	--arg png "$png_b64" \
	'{recipients: [$to], text: "", attachments: [
		{content_type: "image/gif", file_name: "cat.gif", base64: $gif},
		{content_type: "image/png", file_name: "paw.png", base64: $png}
	]}' >"$WORK/send.json"
pass "built one /v1/send request with 2 attachments (payload $(stat --format=%s "$WORK/send.json")B; base64 never logged)"

rc=0
response="$(app_http_request POST "$BASE" "/v1/send" "$WORK/send.json" "$TOKEN" 2>"$WORK/http.err")" || rc=$?
case "$rc" in
"$RC_OK")
	if ! printf '%s' "$response" | jq -e . >/dev/null 2>&1; then
		fail "app /v1/send returned a non-JSON body: $(head -c 200 <<<"$response")"
		log "t17_multi_attachment: summary"
		exit 1
	fi
	if printf '%s' "$response" | jq -e '.error' >/dev/null 2>&1; then
		err_code="$(jq -r '.error' <<<"$response")"
		if [ "$err_code" = "unauthorized" ] || [ "$err_code" = "forbidden" ]; then
			skip_check "app /v1/send rejected the bearer token ($err_code); set APP_BRIDGE_TOKEN to the app's stored token"
			log "t17_multi_attachment: summary"
			exit 0
		fi
		fail "app /v1/send refused the multi-attachment request: error=$(printf '%s' "$response" | jq -c '{error, message}')"
		log "t17_multi_attachment: summary"
		exit 1
	fi
	message_id="$(jq -r '.message_id // empty' <<<"$response")"
	parts_sent="$(jq -r '.parts_sent // 0' <<<"$response")"
	pending="$(jq -r '.pending // false' <<<"$response")"
	pass "app accepted the multi-attachment send (message_id=${message_id:-?} parts_sent=$parts_sent pending=$pending)"
	;;
*) fail "app /v1/send transport failed: $(tr '\n' ' ' <"$WORK/http.err")"; log "t17_multi_attachment: summary"; exit 1 ;;
esac

# ---------------------------------------------------------------------------
# 5. phone assertions: ONE MMS with BOTH parts
# ---------------------------------------------------------------------------
row_id=""
case "$message_id" in
mms:*:*) row_id="${message_id##*:}" ;;
esac

attempts=0
while [ "$attempts" -lt 36 ]; do
	if [ -n "$row_id" ] && [ "$row_id" -gt 0 ] 2>/dev/null; then
		found_row="$(sqlite_query "select _id from pdu where _id = $row_id;" 2>/dev/null | tail -n 1)"
		[ -n "$found_row" ] && break
	fi
	if [ -n "$(phone_pdu_rows_newer_than "$before_pdu" 2>/dev/null || true)" ]; then
		break
	fi
	attempts=$((attempts + 1))
	sleep 5
done

new_pdus="$(phone_pdu_rows_newer_than "$before_pdu" 2>/dev/null || true)"
if [ -z "$new_pdus" ]; then
	fail "multi-attachment probe: no new MMS pdu row on the phone within 180s"
else
	pdu_ids="$(printf '%s\n' "$new_pdus" | cut -d'|' -f1 | tr '\n' ' ' | sed 's/ $//')"
	pdu_count="$(printf '%s\n' "$new_pdus" | sed '/^$/d' | wc -l | tr -d ' ')"
	pass "new MMS pdu rows after one send request: count=$pdu_count ids=$(printf '%s' "$pdu_ids" | tr ' ' ',')"

	# Classify by fixture hash so unrelated concurrent image MMS traffic cannot
	# be mistaken for this send. The pdu row can appear before all of its parts
	# are persisted, so poll the classification until the expected shape shows
	# up (one row, both fixture payloads) or the timeout passes.
	our_rows=0
	our_matched=0
	our_pdu=""
	total_image_parts=0
	classify_attempts=0
	while :; do
		our_rows=0
		our_matched=0
		our_pdu=""
		total_image_parts=0
		: >"$WORK/all-image-parts.txt"
		for pid in $pdu_ids; do
			row_matched=0
			while IFS='|' read -r part_id part_ct _name _chset _seq _data; do
				[ -n "$part_id" ] || continue
				total_image_parts=$((total_image_parts + 1))
				part_sha="$(phone_part_sha256 "$part_id" 2>/dev/null || true)"
				printf '%s|%s|%s|%s\n' "$pid" "$part_id" "$part_ct" "$part_sha" >>"$WORK/all-image-parts.txt"
				if [ "$part_sha" = "$GIF_SHA" ] || [ "$part_sha" = "$PNG_SHA" ]; then
					row_matched=$((row_matched + 1))
				fi
			done < <(phone_mms_parts "$pid" 2>/dev/null | awk -F'|' '$2 ~ /^image\//')
			if [ "$row_matched" -gt 0 ]; then
				our_rows=$((our_rows + 1))
				if [ "$row_matched" -gt "$our_matched" ]; then
					our_matched="$row_matched"
					our_pdu="$pid"
				fi
			fi
		done
		if [ "$our_rows" -eq 1 ] && [ "$our_matched" -ge 2 ]; then
			break
		fi
		if [ "$our_rows" -gt 1 ]; then
			break
		fi
		classify_attempts=$((classify_attempts + 1))
		[ "$classify_attempts" -ge 24 ] && break
		sleep 5
	done
	pass "MMS pdu rows carrying fixture payloads=$our_rows; image parts total=$total_image_parts; partless/other rows=$((pdu_count - our_rows)); classification polls=$classify_attempts"

	if [ "$our_rows" -eq 0 ]; then
		fail "multi-attachment send: no new MMS row contains either fixture payload ($total_image_parts image part(s) present)"
	elif [ "$our_rows" -gt 1 ]; then
		fail "UPSTREAM BUG REPRODUCED: one multi-attachment request produced $our_rows separate MMS rows carrying fixture payloads — one Transaction per attachment"
	elif [ "$our_matched" -lt 2 ]; then
		fail "multi-attachment MMS pdu=$our_pdu carries only $our_matched of the 2 fixture payloads; an attachment was dropped"
	else
		pass "multi-attachment behaviour: ONE MMS (pdu=$our_pdu) with $our_matched fixture image parts — one Transaction for both attachments"
		while IFS='|' read -r pid part_id part_ct part_sha; do
			[ "$pid" = "$our_pdu" ] || continue
			pass "part $part_id ct=$part_ct sha256=${part_sha:0:16}…"
			case "$part_ct" in
			image/gif)
				if [ "$part_sha" = "$GIF_SHA" ]; then
					pass "part $part_id image/gif is byte-exact"
				else
					fail "part $part_id image/gif differs (got ${part_sha:0:16}…, want ${GIF_SHA:0:16}…)"
				fi
				;;
			image/png)
				if [ "$part_sha" = "$PNG_SHA" ]; then
					pass "part $part_id image/png is byte-exact"
				else
					fail "part $part_id image/png differs (got ${part_sha:0:16}…, want ${PNG_SHA:0:16}…)"
				fi
				;;
			*) fail "unexpected image part content-type '$part_ct'" ;;
			esac
		done <"$WORK/all-image-parts.txt"
	fi

	new_sms="$(phone_sms_rows_newer_than "$before_sms" 2 2>/dev/null || true)"
	sms_fallback=""
	if [ -n "$new_sms" ]; then
		sms_fallback="$(printf '%s\n' "$new_sms" | awk -F'|' -v to="$MEDIA_TARGET_NUMBER" '$5 == to { print $1 }')"
	fi
	if [ -n "$sms_fallback" ]; then
		fail "multi-attachment send also produced sent SMS row(s) for the target: $(printf '%s' "$sms_fallback" | tr '\n' ' ')"
	else
		pass "no SMS fallback rows were created for $MEDIA_TARGET_NUMBER"
	fi
fi

# ---------------------------------------------------------------------------
# 6. Matrix portal mirror (report only; the phone-side count is the verdict)
# ---------------------------------------------------------------------------
room="$(room_ids_matching_name "$MEDIA_TARGET_NUMBER" 2>/dev/null | awk -F'\t' '$2 !~ /jmp|cheogram/ { print $1; exit }')"
if [ "$EXPECT_PORTAL_MIRROR" = "1" ] && [ -n "$room" ]; then
	events=""
	attempts=0
	while [ "$attempts" -lt $((PORTAL_MIRROR_WAIT / 5)) ]; do
		events="$(portal_events_since "$room" "$((started_ms - 2000))" 2>/dev/null || true)"
		[ -n "$events" ] && break
		attempts=$((attempts + 1))
		sleep 5
	done
	if [ -n "$events" ]; then
		image_events="$(printf '%s\n' "$events" | jq -c 'select(.msgtype == "m.image")' | wc -l | tr -d ' ')"
		notice_events="$(printf '%s\n' "$events" | jq -c 'select(.msgtype == "m.notice")' | wc -l | tr -d ' ')"
		text_events="$(printf '%s\n' "$events" | jq -c 'select(.msgtype == "m.text")' | wc -l | tr -d ' ')"
		if [ "$image_events" -ge 1 ]; then
			pass "portal $room mirrored the multi-attachment MMS as $image_events m.image event(s)"
		elif [ "$notice_events" -ge 1 ]; then
			fail "portal $room mirrored $notice_events processing error notice(s) and no m.image for the multi-attachment MMS (bridge-side inbound failure; text=$text_events)"
		else
			fail "portal $room mirrored the send without an m.image event (text=$text_events notice=$notice_events; $(printf '%s\n' "$events" | head -n 1))"
		fi
	else
		fail "portal $room did not mirror the multi-attachment send within ${PORTAL_MIRROR_WAIT}s"
	fi
elif [ -z "$room" ]; then
	skip_check "no portal room for $MEDIA_TARGET_NUMBER yet; Matrix echo not checked"
fi

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t17_multi_attachment: PASS (skips: $SKIPS)"
	exit 0
fi
log "t17_multi_attachment: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
