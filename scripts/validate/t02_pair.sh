#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t02_pair.sh — acceptance step 1: the bridge pairing API answers, a phone is
# paired (app control surface), and the bridge sees a recent heartbeat.
#
# Checks, in order:
#   1. bridge: $BRIDGE_URL speaks protocol 1 and pairing_enabled=true.
#   2. app:    control.STATUS reports paired=true (SKIP without control build).
#   3. heartbeat: the app's last_heartbeat_ms is fresh (PHONE_HEARTBEAT_MAX_AGE_MS).
#   4. bridge: `!sms devices` (SMS_MGMT_ROOM + iamb) lists at least one device.
#
# Unreachable components SKIP (exit 0); a reachable component that answers with
# the wrong state FAILs (exit 1). Evidence is ids/cursors/timestamps only.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$SCRIPT_DIR/lib.sh"

FAILURES=0
SKIPS=0

pass() { log "PASS: $*"; }
skip_check() { SKIPS=$((SKIPS + 1)); skip "$*"; }
fail() { FAILURES=$((FAILURES + 1)); printf '[validate] FAIL: %s\n' "$*" >&2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t02.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

PHONE_HEARTBEAT_MAX_AGE_MS="${PHONE_HEARTBEAT_MAX_AGE_MS:-1800000}"

log "t02_pair: BRIDGE_URL=$BRIDGE_URL APP_PACKAGE=$APP_PACKAGE ADB_SSH=${ADB_SSH:-<local>}"

# --- 1. bridge pairing API --------------------------------------------------
rc=0
bridge_info >"$WORK/bridge.json" 2>"$WORK/bridge.err" || rc=$?
case "$rc" in
"$RC_OK")
	if jq -e '.pairing_enabled == true' "$WORK/bridge.json" >/dev/null 2>&1; then
		pass "bridge speaks protocol 1 with pairing_enabled=true ($(jq -r '.bridge_name' "$WORK/bridge.json"))"
	else
		fail "bridge answers protocol 1 but pairing_enabled is not true ($(jq -r '.pairing_enabled // "missing"' "$WORK/bridge.json"))"
	fi
	;;
"$RC_UNREACHABLE")
	skip_check "bridge at $BRIDGE_URL not reachable: $(tr '\n' ' ' <"$WORK/bridge.err")"
	;;
*)
	fail "bridge at $BRIDGE_URL is reachable but misbehaving: $(tr '\n' ' ' <"$WORK/bridge.err")"
	;;
esac

# --- 2./3. app pairing + heartbeat age -------------------------------------
rc=0
drive_app_json status >"$WORK/app.json" 2>"$WORK/app.err" || rc=$?
case "$rc" in
"$RC_OK")
	paired="$(jq -r 'if .paired == null then empty else (.paired | tostring) end' "$WORK/app.json")"
	if [ "$paired" != "true" ]; then
		code="$(jq -r '.error.code // "?"' "$WORK/app.json")"
		if [ "$code" = "disabled" ]; then
			skip_check "app control surface disabled (control_enabled=false); cannot verify pairing"
		else
			fail "app control surface reports paired='${paired:-missing}' (error code '$code')"
		fi
	else
		device_id="$(jq -r '.device_id // "?"' "$WORK/app.json")"
		user_id="$(jq -r '.user_id // "?"' "$WORK/app.json")"
		pass "app is paired (device_id=$device_id, user_id=$user_id, version=$(jq -r '.version_name // "?"' "$WORK/app.json"))"

		last="$(jq -r '.last_heartbeat_ms // 0' "$WORK/app.json")"
		now="$(now_ms)"
		if [ "$last" -le 0 ] 2>/dev/null; then
			fail "paired app has no last_heartbeat_ms (bridge never saw this device)"
		else
			age=$((now - last))
			if [ "$age" -le "$PHONE_HEARTBEAT_MAX_AGE_MS" ]; then
				pass "heartbeat is fresh (age=${age}ms, last_heartbeat_ms=$last)"
			else
				fail "heartbeat is stale (age=${age}ms > ${PHONE_HEARTBEAT_MAX_AGE_MS}ms, last_heartbeat_ms=$last)"
			fi
		fi
	fi
	;;
"$RC_UNREACHABLE")
	skip_check "no control verdict from $APP_PACKAGE (app build without control support?): $(tr '\n' ' ' <"$WORK/app.err")"
	;;
*)
	fail "app control surface answered with an unparseable verdict: $(tr '\n' ' ' <"$WORK/app.err")"
	;;
esac

# --- 4. bridge-side device list via `!sms devices` --------------------------
if [ -z "$SMS_MGMT_ROOM" ]; then
	skip_check "SMS_MGMT_ROOM not set; skipping bridge-side \`!sms devices\` check"
elif ! iamb_available; then
	skip_check "iamb not available on $IAMB_SSH; skipping bridge-side \`!sms devices\` check"
else
	rc=0
	iamb_send_wait "$SMS_MGMT_ROOM" "!sms devices" 60 >"$WORK/devices.json" 2>"$WORK/devices.err" || rc=$?
	case "$rc" in
	"$RC_OK")
		replies="$(jq -r '.wait.replies | length' "$WORK/devices.json")"
		if [ "$replies" = "0" ]; then
			fail "!sms devices got no reply from the bridge bot (room $SMS_MGMT_ROOM)"
		else
			body="$(jq -r '.wait.replies[0].body // ""' "$WORK/devices.json")"
			if printf '%s' "$body" | grep -qiE 'no .*device|no devices'; then
				fail "!sms devices reports no paired devices"
			elif printf '%s\n' "$body" | grep -qE '^\* '; then
				devices="$(printf '%s\n' "$body" | grep -cE '^\* ' | tr -d ' ')"
				device_ids="$(printf '%s\n' "$body" | grep -E '^\* ' | grep -oE '`[^`]+`' | sed 's/`//g' | tr '\n' ' ' | sed 's/ $//')"
				pass "bridge lists $devices paired device entr(y|ies): $device_ids (reply event $(jq -r '.wait.replies[0].event_id' "$WORK/devices.json"))"
			else
				fail "!sms devices reply has no device entries: reply event $(jq -r '.wait.replies[0].event_id' "$WORK/devices.json")"
			fi
		fi
		;;
	"$RC_UNREACHABLE")
		skip_check "iamb send to $SMS_MGMT_ROOM failed: $(tr '\n' ' ' <"$WORK/devices.err")"
		;;
	*)
		fail "iamb send to $SMS_MGMT_ROOM misbehaved: $(tr '\n' ' ' <"$WORK/devices.err")"
		;;
	esac
fi

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t02_pair: PASS (skips: $SKIPS)"
	exit 0
fi
log "t02_pair: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
