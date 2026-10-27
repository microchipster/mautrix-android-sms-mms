#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t10_restart_dedupe.sh — acceptance step 9: restart dedupe.
#
# State checks against a bridge database (BRIDGE_DB; defaults to the local
# E2E rig's bridge.db when present):
#   1. portal keys are unique (one row per thread/channel id);
#   2. no Matrix event is stored twice for the same (room, mxid, part).
#
# Optional restart check (BRIDGE_RESTART_CMD): snapshot the portal table, run
# the restart command, wait for the bridge to answer, then assert the portal
# rows (ids, mxids, receivers) are unchanged — a restart must not remap rooms
# or drop recipients.
#
# Without a database the test SKIPs; it never restarts a deployment by itself.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$SCRIPT_DIR/lib.sh"

FAILURES=0
SKIPS=0

pass() { log "PASS: $*"; }
skip_check() { SKIPS=$((SKIPS + 1)); skip "$*"; }
fail() { FAILURES=$((FAILURES + 1)); printf '[validate] FAIL: %s\n' "$*" >&2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t10.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

BRIDGE_DB="${BRIDGE_DB:-}"
BRIDGE_RESTART_CMD="${BRIDGE_RESTART_CMD:-}"
RESTART_TIMEOUT="${RESTART_TIMEOUT:-120}"

if [ -z "$BRIDGE_DB" ] && [ -f /tmp/opencode/e2e-pairing/bridge/bridge.db ]; then
	BRIDGE_DB=/tmp/opencode/e2e-pairing/bridge/bridge.db
fi

log "t10_restart_dedupe: BRIDGE_DB=${BRIDGE_DB:-<unset>} BRIDGE_RESTART_CMD=${BRIDGE_RESTART_CMD:+<set>}"

if [ -z "$BRIDGE_DB" ]; then
	skip_check "BRIDGE_DB not set; bridge database checks skipped"
	log "t10_restart_dedupe: SKIP-ONLY run"; exit 0
fi

command -v sqlite3 >/dev/null 2>&1 || { skip_check "sqlite3 not installed"; exit 0; }
if [ ! -f "$BRIDGE_DB" ]; then
	skip_check "bridge database $BRIDGE_DB not found"
	log "t10_restart_dedupe: SKIP-ONLY run"; exit 0
fi

snapshot() {
	sqlite3 "$BRIDGE_DB" "select id, coalesce(mxid,''), receiver from portal order by id;"
}

rc=0
portals="$(snapshot 2>"$WORK/db.err")" || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "cannot read $BRIDGE_DB: $(tr '\n' ' ' <"$WORK/db.err")"
	log "t10_restart_dedupe: SKIP-ONLY run"; exit 0
fi
total_portals="$(printf '%s\n' "$portals" | sed '/^$/d' | wc -l | tr -d ' ')"
pass "bridge database readable ($total_portals portal rows)"

dups="$(sqlite3 "$BRIDGE_DB" "select id, count(*) c from portal group by id having c > 1;")"
if [ -n "$dups" ]; then
	fail "duplicate portal keys in $BRIDGE_DB: $(printf '%s' "$dups" | tr '\n' ' ')"
else
	pass "portal keys are unique ($total_portals rows)"
fi

msg_dups="$(sqlite3 "$BRIDGE_DB" "select room_id, mxid, part_id, count(*) c from message group by room_id, mxid, part_id having c > 1;" 2>/dev/null || true)"
if [ -n "$msg_dups" ]; then
	fail "duplicate message rows (same room/mxid/part): $(printf '%s' "$msg_dups" | head -n 2 | tr '\n' ' ')"
else
	msgs="$(sqlite3 "$BRIDGE_DB" "select count(*) from message;" 2>/dev/null || echo '?')"
	pass "no duplicate (room, mxid, part) message rows (message rows: $msgs)"
fi

# --- optional restart -------------------------------------------------------
if [ -z "$BRIDGE_RESTART_CMD" ]; then
	skip_check "BRIDGE_RESTART_CMD not set; restart-dedupe action skipped"
else
	before_ms="$(now_ms)"
	rc=0
	bash -c "$BRIDGE_RESTART_CMD" >"$WORK/restart.log" 2>&1 || rc=$?
	if [ "$rc" -ne 0 ]; then
		fail "restart command failed (exit $rc): $(tail -n 2 "$WORK/restart.log" | tr '\n' ' ')"
	else
		pass "restart command completed"
		rc=0
		wait_for "$RESTART_TIMEOUT" 3 bridge_info >/dev/null 2>&1 || rc=$?
		if [ "$rc" -ne 0 ]; then
			fail "bridge did not answer within ${RESTART_TIMEOUT}s after restart"
		else
			after="$(snapshot)"
			if [ "$after" = "$portals" ]; then
				pass "portal rows (ids/mxids/receivers) unchanged after restart"
			else
				fail "portal rows changed after restart (before $(wc -l <<<"$portals" | tr -d ' ') rows, after $(wc -l <<<"$after" | tr -d ' ') rows)"
			fi
			# Re-check dedupe after the restart.
			dups_after="$(sqlite3 "$BRIDGE_DB" "select id, count(*) c from portal group by id having c > 1;")"
			if [ -n "$dups_after" ]; then
				fail "duplicate portal keys after restart: $(printf '%s' "$dups_after" | tr '\n' ' ')"
			else
				pass "still one row per portal key after restart"
			fi
		fi
	fi
fi

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t10_restart_dedupe: PASS (skips: $SKIPS)"
	exit 0
fi
log "t10_restart_dedupe: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
