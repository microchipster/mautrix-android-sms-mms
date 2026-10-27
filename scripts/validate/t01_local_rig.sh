#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t01_local_rig.sh — smoke test: verify the components a validation run needs.
#
# Checks, in order:
#   1. adb:    the device $ADB_SERIAL is attached and online.
#   2. bridge: $BRIDGE_URL speaks protocol 1 (GET $SMSMMS_INFO_PATH).
#   3. agent:  $AGENT_URL answers ok=true with protocol 1 (GET $AGENT_INFO_PATH).
#   4. app:    the control surface (tools/drive-app.sh status) responds, when
#              the app package is installed. An app build without control
#              support SKIPs with a clear reason.
#
# Unreachable components SKIP (exit 0); a reachable component that answers with
# the wrong protocol/state FAILs (exit 1). See docs/validation.md.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$SCRIPT_DIR/lib.sh"

FAILURES=0
SKIPS=0

pass() { log "PASS: $*"; }
skip_check() { SKIPS=$((SKIPS + 1)); skip "$*"; }
fail() { FAILURES=$((FAILURES + 1)); printf '[validate] FAIL: %s\n' "$*" >&2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t01.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

log "t01_local_rig: ADB_SERIAL=$ADB_SERIAL BRIDGE_URL=$BRIDGE_URL AGENT_URL=$AGENT_URL APP_PACKAGE=$APP_PACKAGE"

# --- 1. adb -----------------------------------------------------------------
ADB_OK=0
if ! command -v adb >/dev/null 2>&1; then
	skip_check "adb not installed; skipping device checks"
elif state="$(adb -s "$ADB_SERIAL" get-state 2>&1 | tr -d '\r')"; then
	if [ "$state" = "device" ]; then
		pass "adb device $ADB_SERIAL is online"
		ADB_OK=1
	else
		skip_check "adb device $ADB_SERIAL state is '$state', not 'device'"
	fi
else
	skip_check "adb device $ADB_SERIAL not reachable: ${state:-no output}"
fi

# --- 2. bridge --------------------------------------------------------------
rc=0
bridge_info >"$WORK/bridge.json" 2>"$WORK/bridge.err" || rc=$?
case "$rc" in
"$RC_OK")
	pass "bridge at $BRIDGE_URL speaks protocol 1 (bridge_name=$(jq -r '.bridge_name' "$WORK/bridge.json"))"
	;;
"$RC_UNREACHABLE")
	skip_check "bridge at $BRIDGE_URL not reachable: $(tr '\n' ' ' <"$WORK/bridge.err")"
	;;
*)
	fail "bridge at $BRIDGE_URL is reachable but misbehaving: $(tr '\n' ' ' <"$WORK/bridge.err")"
	;;
esac

# --- 3. agent ---------------------------------------------------------------
rc=0
agent_info >"$WORK/agent.json" 2>"$WORK/agent.err" || rc=$?
case "$rc" in
"$RC_OK")
	pass "agent at $AGENT_URL speaks protocol 1 (device=$(jq -r '.device.serial // "?"' "$WORK/agent.json"), ok=true)"
	;;
"$RC_UNREACHABLE")
	skip_check "agent at $AGENT_URL not reachable: $(tr '\n' ' ' <"$WORK/agent.err")"
	;;
*)
	fail "agent at $AGENT_URL is reachable but misbehaving: $(tr '\n' ' ' <"$WORK/agent.err")"
	;;
esac

# --- 4. app control surface -------------------------------------------------
if [ "$ADB_OK" -ne 1 ]; then
	skip_check "app control surface: adb device unavailable"
else
	installed="$(adb -s "$ADB_SERIAL" shell pm list packages 2>/dev/null | tr -d '\r' | grep -x "package:${APP_PACKAGE}" || true)"
	if [ -z "$installed" ]; then
		skip_check "app control surface: package $APP_PACKAGE is not installed"
	else
		rc=0
		drive_app_json status >"$WORK/app.json" 2>"$WORK/app.err" || rc=$?
		case "$rc" in
		"$RC_OK")
			if jq -e '.error' "$WORK/app.json" >/dev/null 2>&1; then
				code="$(jq -r '.error.code // "?"' "$WORK/app.json")"
				if [ "$code" = "disabled" ]; then
					pass "app control surface present but disabled (control_enabled=false; enable it for intent-driven tests)"
				else
					fail "app control surface answered with error code '$code'"
				fi
			else
				pass "app control surface responded (control_enabled=$(jq -r '.enabled // "?"' "$WORK/app.json"), service_running=$(jq -r '.service_running // "?"' "$WORK/app.json"), paired=$(jq -r '.paired // "?"' "$WORK/app.json"))"
			fi
			;;
		"$RC_UNREACHABLE")
			skip_check "app package $APP_PACKAGE installed, but no Broadcast verdict — app build without control support (control intents not merged?)"
			;;
		*)
			fail "app control surface answered with an unparseable verdict: $(tr '\n' ' ' <"$WORK/app.err")"
			;;
		esac
	fi
fi

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t01_local_rig: PASS (skips: $SKIPS)"
	exit 0
fi
log "t01_local_rig: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
