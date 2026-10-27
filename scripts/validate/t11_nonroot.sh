#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t11_nonroot.sh — acceptance step 10: the app transport works without root.
#
# The app transport runs entirely inside the app (Keystore token, HTTP poller,
# Android telephony APIs): no adb root, no su, no shell. This test therefore
# asserts that a paired app reports healthy status without the harness taking
# any root path, and, when the adb agent is configured, that a non-root agent
# reports a non-root backend (root:false + content-provider reads).
#
# A rooted device under test SKIPs the root-absence claim (it cannot prove the
# non-root path); a paired app with a fresh heartbeat PASSes the app half.
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t11.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

HEARTBEAT_MAX_AGE_MS="${PHONE_HEARTBEAT_MAX_AGE_MS:-1800000}"

log "t11_nonroot: APP_PACKAGE=$APP_PACKAGE AGENT_URL=$AGENT_URL"

# --- app transport ----------------------------------------------------------
rc=0
drive_app_json status >"$WORK/app.json" 2>"$WORK/app.err" || rc=$?
case "$rc" in
"$RC_OK")
	paired="$(jq -r '.paired // empty' "$WORK/app.json")"
	service="$(jq -r '.service_running // empty' "$WORK/app.json")"
	sms_perm="$(jq -r 'if .permissions.sms == null then empty else (.permissions.sms | tostring) end' "$WORK/app.json")"
	read_perm="$(jq -r 'if .permissions.read_sms == null then empty else (.permissions.read_sms | tostring) end' "$WORK/app.json")"
	if [ "$paired" != "true" ]; then
		code="$(jq -r '.error.code // "?"' "$WORK/app.json")"
		if [ "$code" = "disabled" ]; then
			skip_check "app control surface disabled; cannot verify the non-root app transport"
		else
			fail "app reports paired='${paired:-missing}' (error '$code'); app transport not operational"
		fi
	else
		pass "app transport operational (device_id=$(jq -r '.device_id // "?"' "$WORK/app.json"), service_running=$service)"
		if [ "$service" = "true" ]; then
			pass "bridge foreground service is running inside the app process"
		else
			fail "app is paired but its bridge service is not running"
		fi
		if [ "$sms_perm" = "true" ] && [ "$read_perm" = "true" ]; then
			pass "app holds the telephony permissions it needs (sms=$sms_perm, read_sms=$read_perm)"
		else
			fail "app is missing telephony permissions (sms=$sms_perm, read_sms=$read_perm)"
		fi
		last="$(jq -r '.last_heartbeat_ms // 0' "$WORK/app.json")"
		if [ "$last" -gt 0 ] 2>/dev/null; then
			age=$(( $(now_ms) - last ))
			if [ "$age" -le "$HEARTBEAT_MAX_AGE_MS" ]; then
				pass "app poller heartbeat is fresh (age=${age}ms) — no root involved in the app path"
			else
				fail "app heartbeat stale (age=${age}ms > ${HEARTBEAT_MAX_AGE_MS}ms)"
			fi
		else
			fail "paired app has no heartbeat recorded"
		fi
	fi
	;;
"$RC_UNREACHABLE")
	skip_check "no control verdict from $APP_PACKAGE (app build without control support?)"
	;;
*)
	fail "app control surface answered with an unparseable verdict: $(tr '\n' ' ' <"$WORK/app.err")"
	;;
esac

# --- adb agent (only if one is configured) ----------------------------------
rc=0
agent_info >"$WORK/agent.json" 2>"$WORK/agent.err" || rc=$?
case "$rc" in
"$RC_OK")
	root="$(jq -r '.root // empty' "$WORK/agent.json")"
	read_backend="$(jq -r '.backend.read // "?"' "$WORK/agent.json")"
	if [ "$root" = "false" ]; then
		if [ "$read_backend" = "content-provider" ]; then
			pass "agent reports non-root (root=false) with content-provider reads"
		else
			fail "agent reports non-root but read backend is '$read_backend' (want content-provider)"
		fi
	else
		skip_check "agent at $AGENT_URL runs as root (root=$root); the non-root agent path is not exercised"
	fi
	;;
"$RC_UNREACHABLE")
	skip_check "no adb agent at $AGENT_URL; non-root agent check skipped (app transport is the non-root path)"
	;;
*)
	fail "agent at $AGENT_URL is reachable but misbehaving: $(tr '\n' ' ' <"$WORK/agent.err")"
	;;
esac

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t11_nonroot: PASS (skips: $SKIPS)"
	exit 0
fi
log "t11_nonroot: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
