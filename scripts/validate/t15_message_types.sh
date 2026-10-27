#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t15_message_types.sh — message-type matrix (SMS): plain text, long multipart
# text and unicode/emoji text, in both directions where the rig allows it.
#
# The iamb/JMP inbound path may deliver a long or unicode text either as a
# multipart SMS or as a text-only MMS (jmpchat decides); both are accepted and
# the payload must hash-match the sent text. Look-at-the-phone checks:
#
# State checks (always):
#   1. phone online and the telephony provider queryable;
#   2. a plausible inbound probe sender exists in the last 24h (evidence only).
#
# Live probes (opt-in: VALIDATE_LIVE_SENDS=1 and IAMB_PHONE_ROOM):
#   A. plain ASCII SMS inbound;
#   B. long (~400 char) ASCII inbound (extension/payload exactly compared);
#   C. unicode/emoji inbound (UTF-8 payload exactly compared);
#   D. long unicode outbound via control.SEND to SMS_TARGET_NUMBER, with the
#      phone row(s) and payload compared and the Matrix echo checked when
#      EXPECT_PORTAL_MIRROR=1 (default).
#
# Short-code-ish senders are not probed: the harness has no short-code
# originator (that needs a carrier-side short code), so the check SKIPs.
#
# Evidence only: ids, thread ids, timestamps, lengths and SHA-256 hashes; texts
# are never logged. Unreachable components SKIP; reachable-but-wrong FAIL.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$SCRIPT_DIR/lib.sh"

FAILURES=0
SKIPS=0

pass() { log "PASS: $*"; }
skip_check() { SKIPS=$((SKIPS + 1)); skip "$*"; }
fail() { FAILURES=$((FAILURES + 1)); printf '[validate] FAIL: %s\n' "$*" >&2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t15.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_PHONE_ROOM="${IAMB_PHONE_ROOM:-}"
SMS_TARGET_NUMBER="${SMS_TARGET_NUMBER:-}"
EXPECT_PORTAL_MIRROR="${EXPECT_PORTAL_MIRROR:-1}"
PORTAL_MIRROR_WAIT="${PORTAL_MIRROR_WAIT:-60}"

log "t15_message_types: ADB_SSH=${ADB_SSH:-<local>} live=${VALIDATE_LIVE_SENDS} iamb_room=${IAMB_PHONE_ROOM:-<unset>} target=${SMS_TARGET_NUMBER:-<unset>}"

# ---------------------------------------------------------------------------
# Local helpers (text through mmssms.db is hex-encoded; never printed)
# ---------------------------------------------------------------------------

# payload_hash_from_sms_ids "ID..." — SHA-256 of the concatenated bodies.
payload_hash_from_sms_ids() {
	local ids="$1" id hex combined=""
	for id in $ids; do
		hex="$(phone_sms_body_hex "$id" 2>/dev/null || true)"
		combined="$combined$hex"
	done
	printf '%s' "$combined" | xxd -r -p | sha256sum | cut -d ' ' -f 1
}

# payload_len_from_sms_ids "ID..." — SUM(length(body)) for the rows.
payload_len_from_sms_ids() {
	sqlite_query "select coalesce(sum(length(body)),0) from sms where _id in ($1);" | tail -n 1
}

# first_text_part_since PDU_ID — print "PDU_ID|part row" for the first new pdu
# that carries a text/plain part; empty when none.
first_text_part_since() {
	local since_pdu="$1" pid row
	while IFS='|' read -r pid _thread _date _box; do
		[ -n "$pid" ] || continue
		row="$(phone_mms_part_row "$pid" "text/plain" 2>/dev/null || true)"
		if [ -n "$row" ]; then
			printf '%s|%s\n' "$pid" "$row"
			return 0
		fi
	done < <(phone_pdu_rows_newer_than "$since_pdu" 2>/dev/null)
	return 0
}

# match_sms_window SENT_HASH ROWS — ROWS is `phone_sms_rows_newer_than` output;
# print "IDS|COUNT|LEN" for the first contiguous run of rows whose concatenated
# body hash equals SENT_HASH; empty when no run matches. Handles a multipart
# split and ignores unrelated concurrent rows.
match_sms_window() {
	local sent_hash="$1" rows="$2"
	local -a lines=() run=()
	local n i j id hex combined total_len
	while IFS= read -r line; do
		[ -n "$line" ] && lines+=("$line")
	done <<<"$rows"
	n="${#lines[@]}"
	[ "$n" -gt 0 ] || return 0
	for ((i = 0; i < n; i++)); do
		run=()
		combined=""
		for ((j = i; j < n; j++)); do
			id="$(cut -d'|' -f1 <<<"${lines[j]}")"
			hex="$(phone_sms_body_hex "$id" 2>/dev/null || true)"
			combined="$combined$hex"
			run+=("$id")
			if [ "$(printf '%s' "$combined" | xxd -r -p | sha256sum | cut -d' ' -f1)" = "$sent_hash" ]; then
				total_len="$(payload_len_from_sms_ids "$(printf '%s ' "${run[@]}")")"
				printf '%s|%s|%s\n' "$(printf '%s,' "${run[@]}" | sed 's/,$//')" "${#run[@]}" "$total_len"
				return 0
			fi
		done
	done
	return 0
}

# match_text_part SENT_HASH SINCE_PDU — print "PDU|PART|CT|LEN" for the first
# new text/plain part whose inline text hash equals SENT_HASH; empty when none.
match_text_part() {
	local sent_hash="$1" since_pdu="$2" pid row part_id part_ct part_len
	while IFS='|' read -r pid _thread _date _box; do
		[ -n "$pid" ] || continue
		row="$(phone_mms_part_row "$pid" "text/plain" 2>/dev/null || true)"
		[ -n "$row" ] || continue
		part_id="$(cut -d'|' -f1 <<<"$row")"
		if [ "$(phone_part_text_sha256 "$part_id" 2>/dev/null || true)" = "$sent_hash" ]; then
			part_ct="$(cut -d'|' -f2 <<<"$row")"
			part_len="$(sqlite_query "select length(coalesce(text,'')) from part where _id = $part_id;" | tail -n 1)"
			printf '%s|%s|%s|%s\n' "$pid" "$part_id" "$part_ct" "$part_len"
			return 0
		fi
	done < <(phone_pdu_rows_newer_than "$since_pdu" 2>/dev/null)
	return 0
}

# portal_room_for_address ADDRESS — first joined room whose name matches the
# address exactly (falling back to a prefix); skips jmpchat/XMPP portals.
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

# check_portal_mirror LABEL SINCE_MS ROOM ACCEPTED_MSGTYPES EXPECTED_LEN —
# PASS when the portal carries a new message event (body length must match
# EXPECTED_LEN when non-zero); FAIL/SKIP per EXPECT_PORTAL_MIRROR.
check_portal_mirror() {
	local label="$1" since_ms="$2" room="$3" accepted="$4" expected_len="${5:-0}" events rc=0
	if [ -z "$room" ]; then
		if [ "$EXPECT_PORTAL_MIRROR" = "1" ]; then
			fail "$label: no Matrix portal room found for the probe sender"
		else
			skip_check "$label: no Matrix portal room found for the probe sender"
		fi
		return 0
	fi
	local started now
	started="$(date +%s)"
	events=""
	while :; do
		rc=0
		events="$(portal_events_since "$room" "$since_ms" 2>/dev/null)" || rc=$?
		if [ "$rc" -eq "$RC_UNREACHABLE" ]; then
			skip_check "$label: iamb unavailable for portal mirror checks"
			return 0
		fi
		if [ -n "$events" ]; then
			break
		fi
		now="$(date +%s)"
		if [ $((now - started)) -ge "$PORTAL_MIRROR_WAIT" ]; then
			break
		fi
		sleep 5
	done
	if [ -n "$events" ]; then
		local summary
		summary="$(printf '%s' "$events" | jq -c -s --arg accepted "$accepted" --argjson expected "$expected_len" \
			'[.[] | select($accepted == "any" or (.msgtype | test($accepted))) | select($expected == 0 or .body_len == $expected)] | length')"
		if [ "$summary" -ge 1 ]; then
			pass "$label: portal $room mirrored the message (body_len=$expected_len; $(printf '%s' "$events" | jq -c -s '.[0] | {event_id, sender, origin_server_ts, msgtype, body_len}'))"
		else
			fail "$label: portal $room has no new message of type $accepted with body_len=$expected_len ($(printf '%s' "$events" | head -n 1))"
		fi
	elif [ "$EXPECT_PORTAL_MIRROR" = "1" ]; then
		fail "$label: portal $room did not mirror the probe within ${PORTAL_MIRROR_WAIT}s"
	else
		skip_check "$label: portal mirror not observed within ${PORTAL_MIRROR_WAIT}s (EXPECT_PORTAL_MIRROR=0)"
	fi
}

# ---------------------------------------------------------------------------
# 1. phone reachable + provider queryable
# ---------------------------------------------------------------------------
rc=0
adb_device_online || rc=$?
case "$rc" in
"$RC_OK") pass "phone $ADB_SERIAL is online" ;;
*) skip_check "phone $ADB_SERIAL not reachable (adb)"; log "t15_message_types: SKIP-ONLY run"; exit 0 ;;
esac

rc=0
max_sms="$(phone_max_sms_id 2>"$WORK/db.err")" || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "telephony provider not queryable: $(tr '\n' ' ' <"$WORK/db.err")"
	log "t15_message_types: SKIP-ONLY run"
	exit 0
fi
pass "telephony provider queryable (max sms _id=$max_sms)"

# Best-effort probe sender for portal lookups when an MMS text part arrives.
since_24h=$(( $(now_ms) - 86400000 ))
PROBE_SENDER="$(sqlite_query "select address from sms where type = 1 and date >= $since_24h order by date desc limit 1;" 2>/dev/null | tail -n 1)"
if [ -n "$PROBE_SENDER" ]; then
	pass "recent inbound sender for portal lookups: $PROBE_SENDER"
else
	skip_check "no recent inbound SMS row to learn the probe sender from; portal lookups may SKIP"
fi

# ---------------------------------------------------------------------------
# Live probes A–C: inbound plain / long / unicode
# ---------------------------------------------------------------------------

# run_inbound_probe LABEL TEXT
run_inbound_probe() {
	local label="$1" text="$2"
	local before_sms before_pdu sent_hash sent_len started_ms rc=0
	before_sms="$(phone_max_sms_id 2>/dev/null || echo 0)"
	before_pdu="$(phone_max_pdu_id 2>/dev/null || echo 0)"
	started_ms="$(now_ms)"
	sent_hash="$(sha256_str "$text")"
	sent_len="$(jq -n --arg s "$text" '$s | length')"

	rc=0
	iamb_send "$IAMB_PHONE_ROOM" "$text" >"$WORK/$label.send.json" 2>"$WORK/$label.send.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		pass "$label: inbound probe sent from $IAMB_PHONE_ROOM (text_len=$sent_len, sha256=${sent_hash:0:16}…)"
		;;
	"$RC_UNREACHABLE") skip_check "$label: inbound probe send failed: $(tr '\n' ' ' <"$WORK/$label.send.err")"; return 0 ;;
	*) fail "$label: inbound probe send misbehaved: $(tr '\n' ' ' <"$WORK/$label.send.err")"; return 0 ;;
	esac

	# Match the arrival by payload hash so unrelated concurrent traffic (shared
	# phone) cannot be mistaken for the probe. Contiguous SMS runs cover a
	# multipart split; a text-only MMS part covers the JMP/MMS delivery path.
	local attempts=0 sms_match="" mms_match="" new_rows="" cand_rows="" cand_parts=""
	while [ "$attempts" -lt 36 ]; do
		new_rows="$(phone_sms_rows_newer_than "$before_sms" 1 2>/dev/null || true)"
		if [ -n "$new_rows" ]; then
			sms_match="$(match_sms_window "$sent_hash" "$new_rows")"
			[ -n "$sms_match" ] && break
		fi
		mms_match="$(match_text_part "$sent_hash" "$before_pdu" 2>/dev/null || true)"
		[ -n "$mms_match" ] && break
		attempts=$((attempts + 1))
		sleep 5
	done

	if [ -n "$sms_match" ]; then
		local ids count total_len
		ids="${sms_match%%|*}"
		count="$(cut -d'|' -f2 <<<"$sms_match")"
		total_len="$(cut -d'|' -f3 <<<"$sms_match")"
		pass "$label: phone recorded matching inbound SMS after ~$((attempts * 5))s (rows=$count ids=$ids total_body_len=$total_len)"
		if [ "$count" -ge 2 ]; then
			pass "$label: multipart split confirmed ($count SMS rows for $sent_len chars)"
		else
			pass "$label: single SMS row (sent_len=$sent_len, stored_len=$total_len)"
		fi
		pass "$label: payload is byte-exact ($(sha256_str "$text"))"
	elif [ -n "$mms_match" ]; then
		local pdu_id part_id part_ct part_len
		pdu_id="$(cut -d'|' -f1 <<<"$mms_match")"
		part_id="$(cut -d'|' -f2 <<<"$mms_match")"
		part_ct="$(cut -d'|' -f3 <<<"$mms_match")"
		part_len="$(cut -d'|' -f4 <<<"$mms_match")"
		pass "$label: phone recorded a matching text-only MMS after ~$((attempts * 5))s (pdu=$pdu_id part=$part_id ct=$part_ct text_len=$part_len)"
		pass "$label: payload is byte-exact ($sent_hash)"
	else
		cand_rows="$(phone_sms_rows_newer_than "$before_sms" 1 2>/dev/null || true)"
		cand_parts="$(phone_pdu_rows_newer_than "$before_pdu" 2>/dev/null | while IFS='|' read -r pid _t _d _b; do
			[ -n "$pid" ] || continue
			row="$(phone_mms_part_row "$pid" "text/plain" 2>/dev/null || true)"
			[ -n "$row" ] || continue
			part_id_x="$(cut -d'|' -f1 <<<"$row")"
			printf 'pdu=%s part=%s ct=%s len=%s sha256=%s ' "$pid" "$part_id_x" "$(cut -d'|' -f2 <<<"$row")" \
				"$(sqlite_query "select length(coalesce(text,'')) from part where _id = $part_id_x;" | tail -n 1)" \
				"$(phone_part_text_sha256 "$part_id_x" 2>/dev/null | cut -c1-16)"
		done)"
		if [ -z "$cand_rows" ] && [ -z "$cand_parts" ]; then
			fail "$label: no inbound payload observed within 180s (sent_len=$sent_len sha256=${sent_hash:0:16}…, sms before=$before_sms pdu before=$before_pdu)"
		else
			fail "$label: no inbound payload matched the probe text within 180s (sent_len=$sent_len sha256=${sent_hash:0:16}…; candidates: sms_rows=$(printf '%s' "$cand_rows" | sed '/^$/d' | wc -l | tr -d ' ') ${cand_parts})"
		fi
	fi

	# Portal mirror evidence.
	local room
	room="$(portal_room_for_address "${PROBE_SENDER:-}")"
	check_portal_mirror "$label" "$((started_ms - 2000))" "$room" "m.text" "$sent_len"
}

# Deterministic probe texts (never logged).
PLAIN_TEXT="validate t15 plain probe $(now_ms)"
LONG_ASCII_TEXT="$(awk 'BEGIN { s=""; for (i = 0; i < 400; i++) s = s sprintf("%d", i % 10); print s }')"
UNICODE_TEXT="validate t15 unicode probe $(now_ms) — ünïcödé ✓ 🚀 🌍"

if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$IAMB_PHONE_ROOM" ]; then
	run_inbound_probe "plain" "$PLAIN_TEXT"
	run_inbound_probe "long400" "$LONG_ASCII_TEXT"
	run_inbound_probe "unicode" "$UNICODE_TEXT"
else
	skip_check "inbound message-type probes disabled (set VALIDATE_LIVE_SENDS=1 and IAMB_PHONE_ROOM)"
fi

# ---------------------------------------------------------------------------
# Live probe D: long unicode outbound via the app control surface
# ---------------------------------------------------------------------------
if [ "$VALIDATE_LIVE_SENDS" = "1" ] && [ -n "$SMS_TARGET_NUMBER" ]; then
	role="$(app_default_sms_role 2>/dev/null || true)"
	if [ "$role" != "true" ]; then
		skip_check "outbound long SMS: app is not the default SMS app (default_sms_role=${role:-unknown})"
	else
		OUTBOUND_TEXT="$(awk 'BEGIN { s=""; for (i = 0; i < 300; i++) s = s sprintf("%d", i % 10); print s }') — ünïcödé 🚀 ✓"
		sent_hash="$(sha256_str "$OUTBOUND_TEXT")"
		sent_len="$(jq -n --arg s "$OUTBOUND_TEXT" '$s | length')"
		before_sms="$(phone_max_sms_id 2>/dev/null || echo 0)"
		before_pdu="$(phone_max_pdu_id 2>/dev/null || echo 0)"
		started_ms="$(now_ms)"
		rc=0
		control_send "$SMS_TARGET_NUMBER" "$OUTBOUND_TEXT" >"$WORK/out-send.json" 2>"$WORK/out-send.err" || rc=$?
		case "$rc" in
		"$RC_OK")
			mode="$(jq -r '.mode // "?"' "$WORK/out-send.json")"
			message_id="$(jq -r '.message_id // empty' "$WORK/out-send.json")"
			pass "outbound probe accepted (target=$SMS_TARGET_NUMBER mode=$mode message_id=${message_id:-?} text_len=$sent_len sha256=${sent_hash:0:16}…)"
			attempts=0
			new_rows=""
			sms_match=""
			mms_match=""
			while [ "$attempts" -lt 36 ]; do
				new_rows="$(phone_sms_rows_newer_than "$before_sms" 2 2>/dev/null || true)"
				if [ -n "$new_rows" ]; then
					sms_match="$(match_sms_window "$sent_hash" "$new_rows")"
					[ -n "$sms_match" ] && break
				fi
				mms_match="$(match_text_part "$sent_hash" "$before_pdu" 2>/dev/null || true)"
				[ -n "$mms_match" ] && break
				attempts=$((attempts + 1))
				sleep 5
			done
			if [ -n "$sms_match" ]; then
				ids="${sms_match%%|*}"
				count="$(cut -d'|' -f2 <<<"$sms_match")"
				total_len="$(cut -d'|' -f3 <<<"$sms_match")"
				thread_id="$(printf '%s\n' "$new_rows" | head -n 1 | cut -d'|' -f2)"
				pass "phone recorded matching outbound SMS rows=$count thread=$thread_id ids=$ids total_body_len=$total_len"
				pass "outbound payload is byte-exact ($sent_hash)"
			elif [ -n "$mms_match" ]; then
				pdu_id="$(cut -d'|' -f1 <<<"$mms_match")"
				part_id="$(cut -d'|' -f2 <<<"$mms_match")"
				part_ct="$(cut -d'|' -f3 <<<"$mms_match")"
				pass "phone recorded a matching outbound text-only MMS pdu=$pdu_id part=$part_id ct=$part_ct (mode=$mode)"
				pass "outbound payload is byte-exact ($sent_hash)"
			else
				fail "outbound probe: no sent payload matched the probe text within 180s (sent_len=$sent_len sha256=${sent_hash:0:16}…)"
			fi
			room="$(portal_room_for_address "$SMS_TARGET_NUMBER")"
			check_portal_mirror "outbound long" "$((started_ms - 2000))" "$room" "any" "$sent_len"
			;;
		"$RC_UNREACHABLE") skip_check "outbound long SMS: no control verdict: $(tr '\n' ' ' <"$WORK/out-send.err")" ;;
		*) fail "outbound long SMS misbehaved: $(tr '\n' ' ' <"$WORK/out-send.err")" ;;
		esac
	fi
else
	skip_check "outbound long SMS probe disabled (set VALIDATE_LIVE_SENDS=1 and SMS_TARGET_NUMBER)"
fi

# ---------------------------------------------------------------------------
# Short-code sender: not practical on this rig
# ---------------------------------------------------------------------------
skip_check "short-code sender not probed: the harness has no short-code originator (needs a carrier-side short code); would need a real short code texting $PHONE_NUMBER"

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t15_message_types: PASS (skips: $SKIPS)"
	exit 0
fi
log "t15_message_types: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
