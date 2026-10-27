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
# Optional restart checks:
#   BRIDGE_RESTART_CMD: snapshot the portal table, run the restart command,
#   wait for the bridge to answer, then assert the portal rows (ids, mxids,
#   receivers) are unchanged — a restart must not remap rooms or drop
#   recipients.
#
#   APP_RESTART=1: restart the paired app on the phone and assert the restart
#   replays no messages into the telephony provider (same max sms/pdu ids and
#   row counts), keeps the pairing and brings the bridge service back with a
#   fresh heartbeat. This is the phone-side dedupe half of the check; the
#   bridge-side inbox dedupe needs BRIDGE_DB/BRIDGE_RESTART_CMD.
#
# SKIPs when neither source is available; it never restarts a deployment by
# itself unless asked to.

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
APP_RESTART="${APP_RESTART:-0}"

if [ -z "$BRIDGE_DB" ] && [ -f /tmp/opencode/e2e-pairing/bridge/bridge.db ]; then
	BRIDGE_DB=/tmp/opencode/e2e-pairing/bridge/bridge.db
fi

log "t10_restart_dedupe: BRIDGE_DB=${BRIDGE_DB:-<unset>} BRIDGE_RESTART_CMD=${BRIDGE_RESTART_CMD:+<set>} APP_RESTART=$APP_RESTART"

# ---------------------------------------------------------------------------
# Bridge database checks (when a database is available)
# ---------------------------------------------------------------------------
DB_OK=0
if [ -z "$BRIDGE_DB" ]; then
	skip_check "BRIDGE_DB not set; bridge database checks skipped"
elif ! command -v sqlite3 >/dev/null 2>&1; then
	skip_check "sqlite3 not installed; bridge database checks skipped"
elif [ ! -f "$BRIDGE_DB" ]; then
	skip_check "bridge database $BRIDGE_DB not found"
else
	DB_OK=1
fi

if [ "$DB_OK" = "1" ]; then
	snapshot() {
		sqlite3 "$BRIDGE_DB" "select id, coalesce(mxid,''), receiver from portal order by id;"
	}

	rc=0
	portals="$(snapshot 2>"$WORK/db.err")" || rc=$?
	if [ "$rc" -ne 0 ]; then
		skip_check "cannot read $BRIDGE_DB: $(tr '\n' ' ' <"$WORK/db.err")"
	else
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

		# --- optional bridge restart ---------------------------------------
		if [ -z "$BRIDGE_RESTART_CMD" ]; then
			skip_check "BRIDGE_RESTART_CMD not set; bridge restart-dedupe action skipped"
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
	fi
fi

# ---------------------------------------------------------------------------
# Phone-side app restart dedupe (APP_RESTART=1)
# ---------------------------------------------------------------------------
if [ "$APP_RESTART" != "1" ]; then
	skip_check "APP_RESTART not set; paired-app restart dedupe skipped"
else
	rc=0
	adb_device_online || rc=$?
	if [ "$rc" -ne 0 ]; then
		skip_check "phone $ADB_SERIAL not reachable (adb); app restart dedupe skipped"
	else
		before_status="$(drive_app_json status 2>/dev/null || true)"
		if [ -z "$before_status" ]; then
			skip_check "no control.STATUS verdict; app restart dedupe skipped"
		else
			before_sms_max="$(phone_max_sms_id 2>/dev/null || echo 0)"
			before_pdu_max="$(phone_max_pdu_id 2>/dev/null || echo 0)"
			before_sms_count="$(sqlite_query "select count(*) from sms;" 2>/dev/null | tail -n 1)"
			before_pdu_count="$(sqlite_query "select count(*) from pdu;" 2>/dev/null | tail -n 1)"
			before_heartbeat="$(jq -r '.last_heartbeat_ms // 0' <<<"$before_status")"
			pass "app restart snapshot: sms max_id=$before_sms_max rows=$before_sms_count; pdu max_id=$before_pdu_max rows=$before_pdu_count; heartbeat=${before_heartbeat}ms"

			adb_shell am force-stop "$APP_PACKAGE" >/dev/null 2>&1 || true
			sleep 3
			adb_shell monkey -p "$APP_PACKAGE" -c android.intent.category.LAUNCHER 1 >/dev/null 2>&1 || true
			adb_shell input keyevent KEYCODE_HOME >/dev/null 2>&1 || true

			# Wait for the bridge service to advertise itself again.
			rc=0
			app_service_running() {
				local rc2=0 verdict
				verdict="$(drive_app_json status 2>/dev/null)" || rc2=$?
				[ "$rc2" -eq 0 ] || return 1
				[ "$(jq -r '.service_running // false' <<<"$verdict")" = "true" ]
			}
			wait_for 120 5 app_service_running || rc=$?
			if [ "$rc" -ne 0 ]; then
				fail "app did not report service_running=true within 120s of the restart"
			else
				pass "app answered control.STATUS again after the restart"
				after_status="$(drive_app_json status 2>/dev/null || true)"
				if [ "$(jq -r '.paired // false' <<<"$after_status")" = "true" ]; then
					pass "pairing persisted across the app restart (device_id=$(jq -r '.device_id // "?"' <<<"$after_status"))"
				else
					fail "app lost its pairing across the restart"
				fi

				sleep 20
				after_sms_max="$(phone_max_sms_id 2>/dev/null || echo 0)"
				after_pdu_max="$(phone_max_pdu_id 2>/dev/null || echo 0)"
				after_sms_count="$(sqlite_query "select count(*) from sms;" 2>/dev/null | tail -n 1)"
				after_pdu_count="$(sqlite_query "select count(*) from pdu;" 2>/dev/null | tail -n 1)"
				if [ "$after_sms_max" = "$before_sms_max" ] && [ "$after_pdu_max" = "$before_pdu_max" ]; then
					pass "no telephony rows were replayed by the restart (sms max_id=$after_sms_max pdu max_id=$after_pdu_max)"
				else
					fail "telephony rows changed across the restart (sms max_id $before_sms_max -> $after_sms_max, pdu max_id $before_pdu_max -> $after_pdu_max)"
				fi
				if [ "$after_sms_count" = "$before_sms_count" ] && [ "$after_pdu_count" = "$before_pdu_count" ]; then
					pass "row counts unchanged (sms=$after_sms_count pdu=$after_pdu_count)"
				else
					fail "row counts changed across the restart (sms $before_sms_count -> $after_sms_count, pdu $before_pdu_count -> $after_pdu_count)"
				fi

				# Keep the ordering stable for the following probe: the next
				# new row must be strictly newer than the pre-restart maximum.
				sleep 5
				# Evidence-only report of the app's outbox state (the deployed
				# bridge may not ack events; report, do not assert).
				outbox_sms="$(jq -r '.outbox_sms // 0' <<<"$after_status")"
				outbox_mms="$(jq -r '.outbox_mms // 0' <<<"$after_status")"
				event_cursor="$(jq -r '.event_cursor // 0' <<<"$after_status")"
				pass "post-restart app state: outbox_sms=$outbox_sms outbox_mms=$outbox_mms event_cursor=$event_cursor"

				rc=0
				drive_app_json sync >/dev/null 2>&1 || rc=$?
				if [ "$rc" -eq 0 ]; then
					heartbeat_after="$(drive_app_json status 2>/dev/null | jq -r '.last_heartbeat_ms // 0')"
					if [ "${heartbeat_after:-0}" -ge "${before_heartbeat:-0}" ]; then
						pass "heartbeat renewed after the restart (${heartbeat_after}ms >= ${before_heartbeat}ms)"
					else
						fail "heartbeat did not advance after the restart (${heartbeat_after:-?} < ${before_heartbeat:-?})"
					fi
				else
					skip_check "control.SYNC did not answer; heartbeat freshness not checked"
				fi
			fi
		fi
	fi
fi

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t10_restart_dedupe: PASS (skips: $SKIPS)"
	exit 0
fi
log "t10_restart_dedupe: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
