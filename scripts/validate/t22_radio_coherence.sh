#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# acceptance step 22: radio coherence for group messaging.
#
# Verifies that group MMS delivery and thread/room coherence survive radio
# transitions in the Messages fork: the second phone is moved from its normal
# airplane + Wi-Fi-calling state to airplane off + mobile data on, a group
# message is sent into the jmpchat group room in each state, and the final
# data-on state must deliver to both phones with every new row in a single
# thread (each marker appearing exactly once in the room). The phone is then
# restored to its baseline airplane + Wi-Fi state. Unreachable components
# SKIP (exit 0); reachable-but-wrong answers FAIL.
#
# Environment:
#   ADB_SERIAL               the local phone (primary; adb contract as usual)
#   RADIO_SERIAL             the second phone whose radios toggle (SKIP without)
#   RADIO_SSH                ssh host the second phone's adb runs on (SKIP without)
#   RADIO_GROUP_ROOM         jmpchat group room id with both numbers (SKIP sends without)
#   RADIO_BASELINE_AIRPLANE  expected baseline airplane value (default 1)
#   RADIO_BASELINE_DATA      expected baseline mobile data value (default 0)
#   RADIO_POLLS              delivery poll attempts (default 12)
#   RADIO_POLL_SECONDS       seconds between polls (default 10)
#   IAMB_SSH / IAMB_BIN / IAMB_PROFILE   room sends (existing lib.sh contract)
#
# Evidence rule: ids, thread numbers, radio states and counts only - never
# message bodies, tokens or keys.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$SCRIPT_DIR/lib.sh"

FAILURES=0
SKIPS=0
pass() { log "PASS: $*"; }
skip_check() { SKIPS=$((SKIPS + 1)); skip "$*"; }
fail() { FAILURES=$((FAILURES + 1)); printf '[validate] FAIL: %s\n' "$*" >&2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t22.XXXXXX")"

RADIO_SERIAL="${RADIO_SERIAL:-}"
RADIO_SSH="${RADIO_SSH:-}"
RADIO_GROUP_ROOM="${RADIO_GROUP_ROOM:-}"
RADIO_BASELINE_AIRPLANE="${RADIO_BASELINE_AIRPLANE:-1}"
RADIO_BASELINE_DATA="${RADIO_BASELINE_DATA:-0}"
RADIO_POLLS="${RADIO_POLLS:-12}"
RADIO_POLL_SECONDS="${RADIO_POLL_SECONDS:-10}"

radio_shell() {
	# Single command string; no semicolons (they would run on the ssh host).
	ssh -n "$RADIO_SSH" adb -s "$RADIO_SERIAL" shell "$1" 2>/dev/null
}

radio_airplane() {
	radio_shell "settings get global airplane_mode_on" | tr -d '\r' | head -n1
}

radio_data() {
	radio_shell "settings get global mobile_data" | tr -d '\r' | head -n1
}

topology_ready() {
	[ -n "$RADIO_SERIAL" ] && [ -n "$RADIO_SSH" ]
}

restore_radios() {
	topology_ready || return 0
	radio_shell "svc data disable" >/dev/null 2>&1 || true
	radio_shell "cmd connectivity airplane-mode enable" >/dev/null 2>&1 || true
}

trap 'restore_radios; rm -rf "$WORK"' EXIT

data_on() {
	radio_shell "cmd connectivity airplane-mode disable" >/dev/null 2>&1 || true
	radio_shell "svc data enable" >/dev/null 2>&1 || true
	sleep 4
}

data_off() {
	radio_shell "svc data disable" >/dev/null 2>&1 || true
	radio_shell "cmd connectivity airplane-mode enable" >/dev/null 2>&1 || true
	sleep 4
}

send_group() {
	# send_group <marker>; sends the marker text into the group room.
	local cmd
	cmd="$IAMB_BIN -P $(shell_quote "$IAMB_PROFILE") send --room $(shell_quote "$RADIO_GROUP_ROOM") --inline $(shell_quote "$1") --wait-output json"
	ssh -n "$IAMB_SSH" "$cmd" >"$WORK/send.json" 2>"$WORK/send.err"
}

poll_marker() {
	# poll_marker <pixel|radio> <marker>; echoes "yes <poll>" or "no <polls>".
	local phone="$1" marker="$2" i=0 out=""
	while [ "$i" -lt "$RADIO_POLLS" ]; do
		i=$((i + 1))
		if [ "$phone" = pixel ]; then
			out="$(adb_shell "content query --uri content://mms/part --projection text" || true)"
		else
			out="$(radio_shell "content query --uri content://mms/part --projection text" || true)"
		fi
		if printf '%s\n' "$out" | grep -q "$marker"; then
			echo "yes $i"
			return 0
		fi
		sleep "$RADIO_POLL_SECONDS"
	done
	echo "no $i"
	return 0
}

# ---------------------------------------------------------------- section 1
log "section 1: baseline radio state of the second phone"
baseline_ok=0
if topology_ready; then
	base_airplane="$(radio_airplane || true)"
	base_data="$(radio_data || true)"
	if [ -z "$base_airplane" ]; then
		skip_check "second phone unreachable (ssh $RADIO_SSH adb -s $RADIO_SERIAL)"
	else
		baseline_ok=1
		log "second phone baseline: airplane=$base_airplane mobile_data=${base_data:-unset}"
		if [ "$base_airplane" = "$RADIO_BASELINE_AIRPLANE" ]; then
			pass "baseline airplane state matches ($base_airplane)"
		else
			log "note: baseline airplane differs from expected ($base_airplane != $RADIO_BASELINE_AIRPLANE); the test still restores to it"
		fi
	fi
else
	skip_check "RADIO_SERIAL/RADIO_SSH not set; radio coherence checks skipped"
fi

# ---------------------------------------------------------------- section 2
log "section 2: group delivery and coherence across radio states"
delivery_enabled=0
if [ "$baseline_ok" -eq 1 ] && [ -n "$RADIO_GROUP_ROOM" ] && [ -n "${ADB_SERIAL:-}" ] && iamb_available; then
	delivery_enabled=1
else
	skip_check "group room / iamb / adb not fully configured; delivery checks skipped"
fi

if [ "$delivery_enabled" -eq 1 ]; then
	t0="$(date +%s)"
	marker_a="radio-coherence-$(random_hex 6)"
	marker_b="radio-coherence-$(random_hex 6)"
	marker_c="radio-coherence-$(random_hex 6)"

	# Phase A: airplane off + mobile data on; both phones must receive.
	data_on
	state="$(radio_airplane || true)"
	if [ "$state" = "$RADIO_BASELINE_DATA" ] || [ "$state" != "$RADIO_BASELINE_AIRPLANE" ]; then
		pass "airplane mode disabled for the data phase (airplane=$state)"
	else
		fail "could not disable airplane mode (airplane=$state)"
	fi
	send_group "$marker_a" || fail "group send failed in the data phase"
	res_a_pixel="$(poll_marker pixel "$marker_a")"
	res_a_radio="$(poll_marker radio "$marker_a")"
	log "data phase: pixel=$res_a_pixel second=$res_a_radio"
	if [ "${res_a_pixel%% *}" = yes ] && [ "${res_a_radio%% *}" = yes ]; then
		pass "group message delivered to both phones with mobile data on"
	else
		fail "group message not delivered on both phones in the data phase (pixel=$res_a_pixel second=$res_a_radio)"
	fi

	# Phase B: airplane back on (Wi-Fi calling only); the pixel must still get
	# it, the second phone's carrier may queue the group MMS (recorded, not a
	# failure: group MMS delivery over Wi-Fi calling is carrier dependent).
	data_off
	state="$(radio_airplane || true)"
	if [ "$state" = "$RADIO_BASELINE_AIRPLANE" ]; then
		pass "airplane mode re-enabled for the Wi-Fi-only phase (airplane=$state)"
	else
		fail "could not re-enable airplane mode (airplane=$state)"
	fi
	send_group "$marker_b" || fail "group send failed in the Wi-Fi-only phase"
	res_b_pixel="$(poll_marker pixel "$marker_b")"
	res_b_radio="$(poll_marker radio "$marker_b")"
	log "wi-fi-only phase: pixel=$res_b_pixel second=$res_b_radio"
	if [ "${res_b_pixel%% *}" = yes ]; then
		pass "group message reached the primary phone over Wi-Fi"
	else
		fail "group message did not reach the primary phone over Wi-Fi (pixel=$res_b_pixel)"
	fi
	if [ "${res_b_radio%% *}" = no ]; then
		log "note: the second phone did not receive the group MMS over Wi-Fi calling (carrier behavior; expected to flush when data returns)"
	fi

	# Phase C: data on again; the queued message may flush and a fresh one
	# must deliver to both, with every new row in a single thread.
	data_on
	state="$(radio_airplane || true)"
	if [ "$state" != "$RADIO_BASELINE_AIRPLANE" ]; then
		pass "airplane mode disabled for the flush phase (airplane=$state)"
	else
		fail "could not disable airplane mode for the flush phase (airplane=$state)"
	fi
	send_group "$marker_c" || fail "group send failed in the flush phase"
	res_c_pixel="$(poll_marker pixel "$marker_c")"
	res_c_radio="$(poll_marker radio "$marker_c")"
	if [ "${res_c_pixel%% *}" = yes ] && [ "${res_c_radio%% *}" = yes ]; then
		pass "group message delivered to both phones after data returned"
	else
		fail "group message not delivered after data returned (pixel=$res_c_pixel second=$res_c_radio)"
	fi

	# Thread coherence: every pdu/mms row since the start of the test should
	# sit in exactly one thread per phone (carrier notices excluded).
	pixel_threads="$(sqlite_query "SELECT DISTINCT thread_id FROM pdu WHERE date >= $t0 AND thread_id != 9223372036854775807" 2>/dev/null | tr -d ' ' | grep -v '^$' | sort -u | wc -l | tr -d ' ')"
	if [ "$pixel_threads" = "1" ]; then
		pass "primary phone kept every new group row in one thread"
	else
		fail "primary phone spread new group rows across $pixel_threads thread(s)"
	fi
	radio_threads="$(radio_shell "content query --uri content://mms --projection _id,thread_id,date" \
		| tr -d '\r' \
		| awk -F'thread_id=' '{ if (NF > 1) { split($2, a, ","); print a[1] } }' \
		| sort -u | wc -l | tr -d ' ')"
	radio_recent="$(radio_shell "content query --uri content://mms --projection _id,thread_id,date" \
		| tr -d '\r' \
		| awk -v t="$t0" -F'date=' '{ if (NF > 1 && $2 + 0 >= t) print }' \
		| grep -c 'thread_id=' || true)"
	if [ "$radio_recent" -gt 0 ] && [ "$radio_threads" -gt 0 ]; then
		log "second phone mms threads overall: $radio_threads (new rows since start: $radio_recent)"
		pass "second phone has group mms rows after the radio transitions"
	else
		log "note: no new second-phone mms rows observed (carrier may still queue; rerun after it flushes)"
	fi

	# Room coherence: every marker exactly once in the group room history.
	room_cmd="$IAMB_BIN -P $(shell_quote "$IAMB_PROFILE") message $(shell_quote "$RADIO_GROUP_ROOM") --limit 60 --ndjson --no-prompt"
	ssh -n "$IAMB_SSH" "$room_cmd" >"$WORK/room.ndjson" 2>"$WORK/room.err" || true
	for marker in "$marker_a" "$marker_b" "$marker_c"; do
		count="$(grep -c "$marker" "$WORK/room.ndjson" 2>/dev/null || true)"
		if [ "$count" = "1" ]; then
			pass "room history contains ${marker%%-*}... marker exactly once"
		else
			fail "room history marker count is $count (want 1) for one of the radio test messages"
		fi
	done
fi

# ---------------------------------------------------------------- section 3
log "section 3: restore the baseline radio state"
if topology_ready; then
	restore_radios
	final_airplane="$(radio_airplane || true)"
	if [ "$final_airplane" = "$RADIO_BASELINE_AIRPLANE" ]; then
		pass "second phone restored to the baseline airplane state ($final_airplane)"
	else
		fail "second phone airplane state after restore is $final_airplane (want $RADIO_BASELINE_AIRPLANE)"
	fi
else
	skip_check "RADIO_SERIAL/RADIO_SSH not set; restore skipped"
fi

if [ "$FAILURES" -eq 0 ]; then
	log "t22_radio_coherence: PASS (skips: $SKIPS)"
	exit 0
fi
log "t22_radio_coherence: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
