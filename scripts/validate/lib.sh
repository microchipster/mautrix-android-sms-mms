#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# lib.sh — shared environment contract and helpers for the sms-mms validation
# harness. Source it from scripts/validate/t*.sh; do not execute it directly.
#
# The harness is env-driven: nothing below hardcodes a specific workstation.
# Defaults target the local dev rig described in docs/testing-e2e-local.md and
# are overridden for a deployment (see docs/validation.md). `ADB_SERIAL`,
# `AGENT_TOKEN` and `IAMB_SSH` are required: helpers that use them fail with a
# clear error when unset instead of falling back to a hardcoded setup value.
#
# Probe return codes (callers SKIP an unreachable component, but FAIL a
# reachable one that answers wrong):
#   RC_OK           0  component answered and passed the check
#   RC_MISBEHAVING  1  component reachable, but the answer is wrong/invalid
#   RC_UNREACHABLE  2  transport error / no verdict — component not available
#
# Environment contract
# --------------------
# adb / phone (all optional unless noted):
#   ADB_SERIAL            adb serial of the phone under test (required).
#   ADB_SSH               when set, run adb on that host: `ssh $ADB_SSH adb ...`
#                         (e.g. ADB_SSH=<ssh-host> for a phone plugged into another host).
#   APP_PACKAGE           app under test (default org.fossify.messages.bridge).
#   CONTROL_TOKEN         optional control-intent token extra; also accepted by
#                         drive_app for token-gated app builds. Never logged.
#   PHONE_TELEPHONY_DB    telephony provider database on the device
#                         (default /data/data/com.android.providers.telephony/databases/mmssms.db).
#   PHONE_SQLITE_BIN      sqlite3 binary on the device (default sqlite3).
#   PHONE_SQLITE_MODE     auto|direct|su (default auto): auto retries with
#                         `su -c` when the direct query fails.
#   PHONE_DOWNLOAD_DIR    shared dir for push/read-back checks (default /sdcard/Download).
#   PHONE_ATTACHMENT_DIR  app-readable dir for control.SEND attachments (default
#                         /sdcard/Android/data/$APP_PACKAGE/files; created on demand).
#   PHONE_NUMBER          the phone's own E.164 number under test (optional;
#                         used for own-number-ghost checks). Never message bodies.
#
# Bridge pairing API:
#   BRIDGE_URL            bridge base URL (default http://127.0.0.1:29322).
#   BRIDGE_TOKEN          optional bearer token for authenticated routes.
#   SMSMMS_INFO_PATH      bridge info path (default /_smsmms/v1/info).
#   SMS_MGMT_ROOM         optional Matrix room id of the bridge management room
#                         (the `!sms` bot DM); enables bot-command checks via iamb.
#
# ADB agent HTTP API (docs/AGENT_API.md):
#   AGENT_URL             agent base URL (default http://127.0.0.1:8093).
#   AGENT_TOKEN           bearer token (required for agent calls; /v1/health is
#                         auth-free but the harness still sends the header).
#   AGENT_INFO_PATH       agent info path (default /v1/health).
#
# HTTP behaviour:
#   CURL_TIMEOUT          seconds per HTTP request (default 10).
#
# Matrix-side checks (iamb, docs/validation.md):
#   IAMB_SSH              ssh host running iamb (required).
#   IAMB_BIN              remote iamb binary; may use a literal $HOME, expanded
#                         on the ssh host (default $HOME/.cargo/bin/iamb).
#   IAMB_PROFILE          iamb profile (default public).
#   IAMB_NO_IPC           1 = pass the global --no-ipc flag (before the subcommand).
#   IAMB_SEND_TIMEOUT     seconds for iamb_send_wait (default 90).
#   IAMB_EVENTS_DIR       remote archive dir (default
#                         $HOME/.local/share/iamb/profiles/$IAMB_PROFILE/events).
#   BRIDGE_MATRIX_URL     optional direct Matrix client API base for polling.
#   BRIDGE_MATRIX_TOKEN   optional access token for that polling.
#
# iamb/quirk notes:
#   - This harness targets the patched iamb CLI on the ssh host: message bodies go in
#     `--inline '<text>'` (or `--attach`); `--wait-output json` returns the
#     machine-readable send/wait summary. `--no-ipc`, when used, is a global
#     flag and must come before the subcommand (iamb_cmd inserts it correctly).
#   - Public-profile archives are `events/<room-without-!>.ndjson`; the
#     personal profile stores the same NDJSON without the `.ndjson` suffix.
#     archive_tail() tries the profile's suffix first and falls back.
#   - iamb `--text` only applies to reactions (`--react-to`); do not use it as
#     a body flag.
#
# Acceptance-suite knobs (all optional; see docs/validation.md):
#   VALIDATE_LIVE_SENDS       1 = allow opt-in real SMS/MMS probes.
#   IAMB_PHONE_ROOM           iamb room that texts the phone (t03/t05 inbound probe).
#   IAMB_GROUP_ROOM           iamb group room with the phone + friends (t04/t07/t14).
#   IAMB_GROUP_PORTAL         Matrix portal room of a group thread (t06/t13).
#   IAMB_PORTAL_ROOM          portal room for the outbound tapback probe (t08).
#   IAMB_TAPBACK_EVENT        event id to react to (t08).
#   ATTACHMENT_TARGET_NUMBER  recipient for the outbound attachment probe (t05).
#   MMS_SIZE_LIMIT_BYTES      carrier MMS limit (default 307200).
#   PHONE_EXPECT_READ         1 = unread inbound SMS fails t09.
#   PORTAL_STRICT_PARTICIPANTS 1 = portals must name every phone participant (t04/t07).
#   EXPECT_REACTION_TEXT      1 = assert the send_reactions_as_text fallback (t08).
#   INVITES_ACCEPT            1 = let t14 accept pending invites.
#   PHONE_HEARTBEAT_MAX_AGE_MS heartbeat staleness limit (t02/t11, default 1800000).
#   BRIDGE_DB / BRIDGE_RESTART_CMD / RESTART_TIMEOUT  restart-dedupe checks (t10).
#   APP_RESTART               1 = let t10 restart the paired app and assert no
#                             phone rows are duplicated (opt-in; disruptive).
#   INBOUND_LOOKBACK_MS / READ_LOOKBACK_MS  lookback windows (t03/t08/t09).
#   MAX_GROUP_THREADS / MAX_CANDIDATE_ROOMS / MAX_SCAN_ROOMS  slow-scan caps.
#   EXPECT_PORTAL_MIRROR      1 = FAIL when a live probe's phone-side message
#                             is missing from its Matrix portal (default 1;
#                             0 degrades that check to a SKIP with evidence).
#   PORTAL_MIRROR_WAIT        seconds to wait for the portal mirror (default 60).
#   SMS_TARGET_NUMBER         outbound target for the SMS message-type probes
#                             (t15; defaults to JMP_NUMBER).
#   MEDIA_TARGET_NUMBER       outbound target for the media-variant probes
#                             (t18; defaults to SMS_TARGET_NUMBER).
#   MEDIA_BYTE_EXACT_INBOUND  1 = inbound video/audio parts must hash-match the
#                             fixture (default 1; 0 documents the degradation as
#                             a SKIP when the JMP/MMS transport transcodes).
#   APP_BRIDGE_URL            app HTTP API base URL (t17) when it is already
#                             reachable (e.g. via a manual tunnel).
#   APP_BRIDGE_TOKEN          bearer token for APP_BRIDGE_URL (never logged).
#   APP_BRIDGE_PORT           app HTTP API port for the adb-forward path
#                             (t17; default 8391, the app's bridge_port).
#   APP_HTTP_ENABLE_TEMP      1 = let t17 temporarily enable the app's
#                             static-token HTTP API via control.CONFIG
#                             (bridge_enabled=true + a generated token) and
#                             restore the preference afterwards.
#
# Bodies, message text and tokens are never logged: evidence is ids, room ids,
# counts, timestamps and SHA-256 hashes only.

# shellcheck shell=bash

set -euo pipefail

# ---------------------------------------------------------------------------
# Environment contract
# ---------------------------------------------------------------------------

# Phone under test (adb serial) and the app package installed on it.
ADB_SERIAL="${ADB_SERIAL:-}"
ADB_SSH="${ADB_SSH:-}"
APP_PACKAGE="${APP_PACKAGE:-org.fossify.messages.bridge}"
CONTROL_TOKEN="${CONTROL_TOKEN:-}"

# Phone-side state (sqlite over adb).
PHONE_TELEPHONY_DB="${PHONE_TELEPHONY_DB:-/data/data/com.android.providers.telephony/databases/mmssms.db}"
PHONE_SQLITE_BIN="${PHONE_SQLITE_BIN:-sqlite3}"
PHONE_SQLITE_MODE="${PHONE_SQLITE_MODE:-auto}"
PHONE_DOWNLOAD_DIR="${PHONE_DOWNLOAD_DIR:-/sdcard/Download}"
# Where control.SEND attachments are staged: the app's own external files dir
# is always readable by the app under scoped storage (the shared Download dir
# is not). Created on demand by prepare_attachment().
PHONE_ATTACHMENT_DIR="${PHONE_ATTACHMENT_DIR:-/sdcard/Android/data/$APP_PACKAGE/files}"
PHONE_NUMBER="${PHONE_NUMBER:-}"

# Bridge pairing API. GET {BRIDGE_URL}{SMSMMS_INFO_PATH} is public.
BRIDGE_URL="${BRIDGE_URL:-http://127.0.0.1:29322}"
BRIDGE_TOKEN="${BRIDGE_TOKEN:-}"
SMSMMS_INFO_PATH="${SMSMMS_INFO_PATH:-/_smsmms/v1/info}"
SMS_MGMT_ROOM="${SMS_MGMT_ROOM:-}"

# ADB agent HTTP API (docs/AGENT_API.md). GET /v1/health is auth-free.
AGENT_URL="${AGENT_URL:-http://127.0.0.1:8093}"
AGENT_TOKEN="${AGENT_TOKEN:-}"
AGENT_INFO_PATH="${AGENT_INFO_PATH:-/v1/health}"

# HTTP behaviour.
CURL_TIMEOUT="${CURL_TIMEOUT:-10}"

# Matrix-side checks.
BRIDGE_MATRIX_URL="${BRIDGE_MATRIX_URL:-}"
BRIDGE_MATRIX_TOKEN="${BRIDGE_MATRIX_TOKEN:-}"

# Cross-system (iamb/JMP) pass-through.
IAMB_SSH="${IAMB_SSH:-}"
IAMB_BIN="${IAMB_BIN:-\$HOME/.cargo/bin/iamb}"
IAMB_PROFILE="${IAMB_PROFILE:-public}"
IAMB_NO_IPC="${IAMB_NO_IPC:-0}"
IAMB_SEND_TIMEOUT="${IAMB_SEND_TIMEOUT:-90}"
IAMB_EVENTS_DIR="${IAMB_EVENTS_DIR:-\$HOME/.local/share/iamb/profiles/\$IAMB_PROFILE/events}"
JMP_PROFILE="${JMP_PROFILE:-}"
JMP_NUMBER="${JMP_NUMBER:-}"
JMP_MGMT_ROOM="${JMP_MGMT_ROOM:-}"

# Acceptance-suite knobs (defaults; see docs/validation.md). Defined here so
# every test can reference them safely under `set -u`.
VALIDATE_LIVE_SENDS="${VALIDATE_LIVE_SENDS:-0}"
IAMB_PHONE_ROOM="${IAMB_PHONE_ROOM:-}"
IAMB_GROUP_ROOM="${IAMB_GROUP_ROOM:-}"
IAMB_GROUP_PORTAL="${IAMB_GROUP_PORTAL:-}"
IAMB_PORTAL_ROOM="${IAMB_PORTAL_ROOM:-}"
IAMB_TAPBACK_EVENT="${IAMB_TAPBACK_EVENT:-}"
ATTACHMENT_TARGET_NUMBER="${ATTACHMENT_TARGET_NUMBER:-}"
MMS_SIZE_LIMIT_BYTES="${MMS_SIZE_LIMIT_BYTES:-307200}"
PHONE_EXPECT_READ="${PHONE_EXPECT_READ:-0}"
PORTAL_STRICT_PARTICIPANTS="${PORTAL_STRICT_PARTICIPANTS:-0}"
EXPECT_REACTION_TEXT="${EXPECT_REACTION_TEXT:-0}"
INVITES_ACCEPT="${INVITES_ACCEPT:-0}"
PHONE_HEARTBEAT_MAX_AGE_MS="${PHONE_HEARTBEAT_MAX_AGE_MS:-1800000}"
BRIDGE_DB="${BRIDGE_DB:-}"
BRIDGE_RESTART_CMD="${BRIDGE_RESTART_CMD:-}"
RESTART_TIMEOUT="${RESTART_TIMEOUT:-120}"
INBOUND_LOOKBACK_MS="${INBOUND_LOOKBACK_MS:-2592000000}"
READ_LOOKBACK_MS="${READ_LOOKBACK_MS:-2592000000}"
MAX_GROUP_THREADS="${MAX_GROUP_THREADS:-3}"
MAX_CANDIDATE_ROOMS="${MAX_CANDIDATE_ROOMS:-6}"
MAX_SCAN_ROOMS="${MAX_SCAN_ROOMS:-3}"
APP_RESTART="${APP_RESTART:-0}"
EXPECT_PORTAL_MIRROR="${EXPECT_PORTAL_MIRROR:-1}"
PORTAL_MIRROR_WAIT="${PORTAL_MIRROR_WAIT:-60}"
SMS_TARGET_NUMBER="${SMS_TARGET_NUMBER:-${JMP_NUMBER:-}}"
MEDIA_TARGET_NUMBER="${MEDIA_TARGET_NUMBER:-${SMS_TARGET_NUMBER:-}}"
MEDIA_BYTE_EXACT_INBOUND="${MEDIA_BYTE_EXACT_INBOUND:-1}"
APP_BRIDGE_URL="${APP_BRIDGE_URL:-}"
APP_BRIDGE_TOKEN="${APP_BRIDGE_TOKEN:-}"
APP_BRIDGE_PORT="${APP_BRIDGE_PORT:-8391}"
APP_HTTP_ENABLE_TEMP="${APP_HTTP_ENABLE_TEMP:-0}"

export ADB_SERIAL ADB_SSH APP_PACKAGE CONTROL_TOKEN
export PHONE_TELEPHONY_DB PHONE_SQLITE_BIN PHONE_SQLITE_MODE PHONE_DOWNLOAD_DIR PHONE_ATTACHMENT_DIR PHONE_NUMBER
export BRIDGE_URL BRIDGE_TOKEN SMSMMS_INFO_PATH SMS_MGMT_ROOM
export AGENT_URL AGENT_TOKEN AGENT_INFO_PATH
export CURL_TIMEOUT BRIDGE_MATRIX_URL BRIDGE_MATRIX_TOKEN
export IAMB_SSH IAMB_BIN IAMB_PROFILE IAMB_NO_IPC IAMB_SEND_TIMEOUT IAMB_EVENTS_DIR
export VALIDATE_LIVE_SENDS IAMB_PHONE_ROOM IAMB_GROUP_ROOM IAMB_GROUP_PORTAL IAMB_PORTAL_ROOM IAMB_TAPBACK_EVENT
export ATTACHMENT_TARGET_NUMBER MMS_SIZE_LIMIT_BYTES PHONE_EXPECT_READ PORTAL_STRICT_PARTICIPANTS EXPECT_REACTION_TEXT
export INVITES_ACCEPT PHONE_HEARTBEAT_MAX_AGE_MS BRIDGE_DB BRIDGE_RESTART_CMD RESTART_TIMEOUT
export INBOUND_LOOKBACK_MS READ_LOOKBACK_MS MAX_GROUP_THREADS MAX_CANDIDATE_ROOMS MAX_SCAN_ROOMS
export APP_RESTART EXPECT_PORTAL_MIRROR PORTAL_MIRROR_WAIT SMS_TARGET_NUMBER MEDIA_TARGET_NUMBER MEDIA_BYTE_EXACT_INBOUND
export APP_BRIDGE_URL APP_BRIDGE_TOKEN APP_BRIDGE_PORT APP_HTTP_ENABLE_TEMP
export JMP_PROFILE JMP_NUMBER JMP_MGMT_ROOM

# Repository paths.
VALIDATE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$VALIDATE_DIR/../.." && pwd)"
DRIVE_APP_TOOL="${DRIVE_APP_TOOL:-$REPO_ROOT/tools/drive-app.sh}"
FIXTURES_DIR="${FIXTURES_DIR:-$VALIDATE_DIR/fixtures}"

RC_OK=0
RC_MISBEHAVING=1
RC_UNREACHABLE=2

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

log()  { printf '[validate] %s\n' "$*" >&2; }
skip() { printf '[validate] SKIP: %s\n' "$*" >&2; }
die()  { printf '[validate] FATAL: %s\n' "$*" >&2; exit 1; }

# now_ms — current time in Unix milliseconds.
now_ms() {
	printf '%s\n' "$(( $(date +%s) * 1000 + 10#$(date +%N) / 1000000 ))"
}

# require_env VAR... — die unless every named variable is non-empty.
require_env() {
	local name
	for name in "$@"; do
		if [ -z "${!name:-}" ]; then
			die "environment variable $name is required"
		fi
	done
}

# require_int NAME VALUE — die unless VALUE is a non-negative integer.
require_int() {
	case "$2" in
	''|*[!0-9]*) die "$1 must be a non-negative integer, got '${2:-<empty>}'" ;;
	*) ;;
	esac
}

# ---------------------------------------------------------------------------
# HTTP helpers
# ---------------------------------------------------------------------------

# url_join BASE PATH — join a base URL and a path without doubling slashes.
url_join() {
	local base="$1" path="$2"
	while [ "${base%/}" != "$base" ]; do
		base="${base%/}"
	done
	printf '%s/%s\n' "$base" "${path#/}"
}

# curl_json URL [curl args...] — GET a JSON document, print the body.
# Returns RC_OK, RC_UNREACHABLE (transport error) or RC_MISBEHAVING (HTTP >=
# 400 or a body that is not valid JSON). Callers can pipe the body into
# `jq -e`.
curl_json() {
	local url="$1"
	shift
	local response code body rc=0
	response="$(curl --silent --show-error --max-time "$CURL_TIMEOUT" \
		--write-out $'\n%{http_code}' "$@" "$url")" || rc=$?
	if [ "$rc" -ne 0 ]; then
		log "curl_json: request to $url failed (curl exit $rc)"
		return "$RC_UNREACHABLE"
	fi
	code="${response##*$'\n'}"
	body="${response%$'\n'*}"
	case "$code" in
	2??) ;;
	*)
		log "curl_json: $url returned HTTP $code: $(printf '%s' "$body" | head -c 400)"
		return "$RC_MISBEHAVING"
		;;
	esac
	if ! printf '%s' "$body" | jq -e . >/dev/null 2>&1; then
		log "curl_json: $url returned invalid JSON: $(printf '%s' "$body" | head -c 400)"
		return "$RC_MISBEHAVING"
	fi
	printf '%s\n' "$body"
}

# bridge_info — GET {BRIDGE_URL}{SMSMMS_INFO_PATH} (docs/PAIRING_PROTOCOL.md
# §4) and validate protocol_version=1 plus a non-empty bridge_name. Prints the
# JSON body on success.
bridge_info() {
	local body rc=0 proto name
	local -a curl_args=()
	if [ -n "$BRIDGE_TOKEN" ]; then
		curl_args+=(-H "Authorization: Bearer $BRIDGE_TOKEN")
	fi
	body="$(curl_json "$(url_join "$BRIDGE_URL" "$SMSMMS_INFO_PATH")" ${curl_args[@]+"${curl_args[@]}"})" || rc=$?
	[ "$rc" -eq 0 ] || return "$rc"
	proto="$(jq -r '.protocol_version // empty' <<<"$body")"
	name="$(jq -r '.bridge_name // empty' <<<"$body")"
	if [ "$proto" != "1" ]; then
		log "bridge_info: $BRIDGE_URL reports protocol_version='${proto:-<missing>}' (want 1)"
		return "$RC_MISBEHAVING"
	fi
	if [ -z "$name" ]; then
		log "bridge_info: $BRIDGE_URL is missing bridge_name"
		return "$RC_MISBEHAVING"
	fi
	printf '%s\n' "$body"
}

# agent_info — GET {AGENT_URL}{AGENT_INFO_PATH} (docs/AGENT_API.md; the
# documented info endpoint is /v1/health, auth-free) and validate ok=true plus
# protocol_version=1. Prints the JSON body on success.
agent_info() {
	require_env AGENT_TOKEN
	local body rc=0 proto ok err
	local -a curl_args=()
	if [ -n "$AGENT_TOKEN" ]; then
		curl_args+=(-H "Authorization: Bearer $AGENT_TOKEN")
	fi
	body="$(curl_json "$(url_join "$AGENT_URL" "$AGENT_INFO_PATH")" ${curl_args[@]+"${curl_args[@]}"})" || rc=$?
	[ "$rc" -eq 0 ] || return "$rc"
	proto="$(jq -r '.protocol_version // empty' <<<"$body")"
	if [ "$proto" != "1" ]; then
		log "agent_info: $AGENT_URL reports protocol_version='${proto:-<missing>}' (want 1)"
		return "$RC_MISBEHAVING"
	fi
	ok="$(jq -r '.ok // empty' <<<"$body")"
	if [ "$ok" != "true" ]; then
		err="$(jq -r '.error // "no error field"' <<<"$body")"
		log "agent_info: $AGENT_URL reports ok='${ok:-<missing>}' ($err)"
		return "$RC_MISBEHAVING"
	fi
	printf '%s\n' "$body"
}

# ---------------------------------------------------------------------------
# adb transport (local, or on another host via ADB_SSH)
# ---------------------------------------------------------------------------

# adb_cmd ARGS... — run adb, locally or on $ADB_SSH.
adb_cmd() {
	if [ -n "$ADB_SSH" ]; then
		ssh "$ADB_SSH" adb "$@"
	else
		adb "$@"
	fi
}

# adb_serial_args — print the `-s SERIAL` prefix as separate words.
adb_serial_args() {
	if [ -n "$ADB_SERIAL" ]; then
		printf '%s\n%s\n' "-s" "$ADB_SERIAL"
	fi
}

# adb_shell ARGS... — run `adb [-s SERIAL] shell ARGS...`.
adb_shell() {
	require_env ADB_SERIAL
	local -a args=()
	while IFS= read -r arg; do
		[ -n "$arg" ] && args+=("$arg")
	done < <(adb_serial_args)
	adb_cmd "${args[@]}" shell "$@"
}

# adb_exec_out ARGS... — run `adb [-s SERIAL] exec-out ARGS...` (binary-safe).
adb_exec_out() {
	require_env ADB_SERIAL
	local -a args=()
	while IFS= read -r arg; do
		[ -n "$arg" ] && args+=("$arg")
	done < <(adb_serial_args)
	adb_cmd "${args[@]}" exec-out "$@"
}

# adb_push LOCAL REMOTE — push a file to the device. With ADB_SSH the file is
# copied to the ssh host first (adb reads it there) and the remote copy is
# removed again.
adb_push() {
	require_env ADB_SERIAL
	local src="$1" dst="$2" rc=0 remote_src="$src" remote_tmp=""
	local -a args=()
	while IFS= read -r arg; do
		[ -n "$arg" ] && args+=("$arg")
	done < <(adb_serial_args)
	if [ -n "$ADB_SSH" ]; then
		remote_tmp="/tmp/smsmms-validate-push.$$.$(basename "$src")"
		if ! scp -q "$src" "$ADB_SSH:$remote_tmp"; then
			return "$RC_UNREACHABLE"
		fi
		remote_src="$remote_tmp"
	fi
	adb_cmd "${args[@]}" push "$remote_src" "$dst" >/dev/null 2>&1 || rc=$?
	if [ -n "$remote_tmp" ]; then
		ssh -n "$ADB_SSH" "rm -f $(shell_quote "$remote_tmp")" >/dev/null 2>&1 || true
	fi
	if [ "$rc" -ne 0 ]; then
		return "$RC_UNREACHABLE"
	fi
	return 0
}

# adb_device_online — RC_OK when the device answers `get-state` with "device".
adb_device_online() {
	require_env ADB_SERIAL
	local state rc=0
	local -a args=()
	while IFS= read -r arg; do
		[ -n "$arg" ] && args+=("$arg")
	done < <(adb_serial_args)
	state="$(adb_cmd "${args[@]}" get-state 2>&1 | tr -d '\r')" || rc=$?
	if [ "$rc" -ne 0 ]; then
		return "$RC_UNREACHABLE"
	fi
	[ "$state" = "device" ] || return "$RC_MISBEHAVING"
	return 0
}

# device_escape_once — escape one shell layer (device `sh -c` parse of the
# command adb joins from argv). Everything outside a conservative safe set is
# backslash-escaped, including backslashes themselves.
device_escape_once() {
	sed 's#[^A-Za-z0-9._/@:+,-]#\\&#g'
}

# device_arg VALUE — escape VALUE so it survives the layers between this shell
# and the device shell (one layer locally, two when adb runs over ssh).
device_arg() {
	local rounds=1 value="$1" i=0
	[ -n "$ADB_SSH" ] && rounds=2
	while [ "$i" -lt "$rounds" ]; do
		value="$(printf '%s' "$value" | device_escape_once)"
		i=$((i + 1))
	done
	printf '%s' "$value"
}

# ---------------------------------------------------------------------------
# Phone-side telephony provider queries (sqlite over adb)
# ---------------------------------------------------------------------------

# sqlite_query SQL — run SQL against $PHONE_TELEPHONY_DB on the device and
# print rows as pipe-separated lines (`.mode list`). SQL travels on stdin, so
# no shell-quoting of the query is needed. PHONE_SQLITE_MODE=auto retries with
# `su -c` when the direct attempt fails (Magisk-style su that joins argv).
# Returns RC_OK, RC_MISBEHAVING (sqlite error) or RC_UNREACHABLE (no adb).
sqlite_query() {
	require_env ADB_SERIAL
	local sql="$1" rc=0 out
	local -a runner=()
	if ! command -v adb >/dev/null 2>&1 && [ -z "$ADB_SSH" ]; then
		return "$RC_UNREACHABLE"
	fi
	while IFS= read -r arg; do
		[ -n "$arg" ] && runner+=("$arg")
	done < <(adb_serial_args)

	# Direct attempt.
	rc=0
	out="$(printf '.mode list\n.separator |\n%s\n' "$sql" | adb_cmd "${runner[@]}" shell "$PHONE_SQLITE_BIN" "$PHONE_TELEPHONY_DB" 2>&1)" || rc=$?
	if [ "$rc" -eq 0 ] && ! printf '%s' "$out" | grep -qE '(^|\n)(Error|error:|sqlite3:)|Permission denied|unable to open database'; then
		printf '%s\n' "$out"
		return 0
	fi
	if [ "$PHONE_SQLITE_MODE" = "direct" ] || [ "$PHONE_SQLITE_MODE" = "never" ]; then
		log "sqlite_query: direct sqlite failed (rc=$rc): $(printf '%s' "$out" | head -c 200)"
		return "$RC_MISBEHAVING"
	fi
	# su fallback (Magisk `su -c <cmd> <args...>` joins the arguments).
	rc=0
	out="$(printf '.mode list\n.separator |\n%s\n' "$sql" | adb_shell su -c "$PHONE_SQLITE_BIN $PHONE_TELEPHONY_DB" 2>&1)" || rc=$?
	if [ "$rc" -ne 0 ] || printf '%s' "$out" | grep -qE '(^|\n)(Error|error:|sqlite3:)|Permission denied|unable to open database'; then
		log "sqlite_query: sqlite failed (rc=$rc, mode=$PHONE_SQLITE_MODE): $(printf '%s' "$out" | head -c 200)"
		return "$RC_MISBEHAVING"
	fi
	printf '%s\n' "$out"
}

# phone_sms_count_since EPOCH_MS — count SMS provider rows dated >= EPOCH_MS.
phone_sms_count_since() {
	require_int "EPOCH_MS" "$1"
	sqlite_query "select count(*) from sms where date >= $1;" | tail -n 1
}

# phone_has_new_sms_since EPOCH_MS — condition for `wait_for`: succeeds once the
# phone has at least one SMS row (inbox or sent) dated >= EPOCH_MS.
phone_has_new_sms_since() {
	local count
	count="$(phone_sms_count_since "$1" 2>/dev/null | tail -n 1)"
	[ -n "$count" ] && [ "$count" -gt 0 ]
}

# phone_sms_rows_since EPOCH_MS — SMS rows (`_id|thread_id|date|type|address`)
# dated >= EPOCH_MS, oldest first. type: 1 inbox, 2 sent, others per Android.
phone_sms_rows_since() {
	require_int "EPOCH_MS" "$1"
	sqlite_query "select _id,thread_id,date,type,address from sms where date >= $1 order by date asc;"
}

# phone_pdu_epoch_expr — SQL expression yielding the MMS `pdu.date` in
# milliseconds. Some providers store MMS dates in seconds (the test Pixel
# does); normalize by the table's own magnitude.
phone_pdu_epoch_expr() {
	printf 'case when (select max(date) from pdu) > 100000000000 then date else date * 1000 end'
}

# phone_pdu_count_since EPOCH_MS — count MMS `pdu` rows dated >= EPOCH_MS.
phone_pdu_count_since() {
	require_int "EPOCH_MS" "$1"
	sqlite_query "select count(*) from pdu where $(phone_pdu_epoch_expr) >= $1;" | tail -n 1
}

# phone_mms_count_since EPOCH_MS — alias of phone_pdu_count_since.
phone_mms_count_since() {
	phone_pdu_count_since "$1"
}

# phone_mms_rows_since EPOCH_MS — MMS rows (`_id|thread_id|date|msg_box`),
# date in the provider's own unit, filtered with the normalized expression.
phone_mms_rows_since() {
	require_int "EPOCH_MS" "$1"
	sqlite_query "select _id,thread_id,date,msg_box from pdu where $(phone_pdu_epoch_expr) >= $1 order by date asc;"
}

# phone_pdu_rows_since EPOCH_MS — alias of phone_mms_rows_since.
phone_pdu_rows_since() {
	phone_mms_rows_since "$1"
}

# phone_threads — threads (`_id|date|recipient_ids|message_count`), newest first.
phone_threads() {
	sqlite_query "select _id,date,recipient_ids,message_count from threads order by date desc;"
}

# phone_thread_participants THREAD_ID — canonical addresses of a thread, one per line.
phone_thread_participants() {
	require_int "THREAD_ID" "$1"
	sqlite_query "select ca.address from canonical_addresses ca join threads t on instr(','||replace(t.recipient_ids,' ',',')||',', ','||ca._id||',') > 0 where t._id = $1 order by ca.address;"
}

# phone_thread_ids_with_participants — thread ids that have 2+ canonical addresses.
phone_thread_ids_with_participants() {
	sqlite_query "select t._id, count(ca._id) from threads t join canonical_addresses ca on instr(','||replace(t.recipient_ids,' ',',')||',', ','||ca._id||',') > 0 group by t._id having count(ca._id) >= 2 order by t._id;"
}

# phone_mms_parts PDU_ID — MMS parts of one pdu row
# (`_id|ct|name|chset|seq|_data`).
phone_mms_parts() {
	require_int "PDU_ID" "$1"
	sqlite_query "select _id,ct,name,chset,seq,coalesce(_data,'') from part where mid = $1 order by seq asc;"
}

# phone_part_data_path PART_ID — filesystem path of a part's payload ('' when
# the part is inline text and has no file).
phone_part_data_path() {
	require_int "PART_ID" "$1"
	sqlite_query "select coalesce(_data,'') from part where _id = $1;" | tail -n 1
}

# phone_read_file REMOTE_PATH — stream a device file to stdout (binary-safe;
# exec-out first, `su -c cat` fallback for root-only paths).
phone_read_file() {
	local path="$1" rc=0
	if adb_exec_out cat "$path" 2>/dev/null; then
		return 0
	fi
	adb_shell su -c "cat $path" 2>/dev/null || rc=$?
	[ "$rc" -ne 0 ] && log "phone_read_file: cannot read $path"
	return "$rc"
}

# phone_part_sha256 PART_ID — SHA-256 (full) of an MMS part's payload bytes.
phone_part_sha256() {
	local path rc=0 sum
	path="$(phone_part_data_path "$1")" || rc=$?
	[ "$rc" -eq 0 ] || return "$rc"
	if [ -z "$path" ]; then
		log "phone_part_sha256: part $1 has no payload file (inline text part)"
		return "$RC_MISBEHAVING"
	fi
	sum="$(phone_read_file "$path" | sha256sum | cut -d ' ' -f 1)" || rc=$?
	[ "$rc" -eq 0 ] || return "$rc"
	[ -n "$sum" ] || return "$RC_MISBEHAVING"
	printf '%s\n' "$sum"
}

# phone_part_text_hex PART_ID — hex of a part's inline text payload (`part.text`;
# '' for file-backed parts). Hex survives newlines in the text; only the hex is
# handled locally and it is never logged as a body.
phone_part_text_hex() {
	require_int "PART_ID" "$1"
	sqlite_query "select coalesce(hex(text),'') from part where _id = $1;" | tail -n 1
}

# phone_part_text_sha256 PART_ID — SHA-256 of the part's inline text bytes.
phone_part_text_sha256() {
	local hex
	hex="$(phone_part_text_hex "$1")" || return $?
	printf '%s' "$hex" | xxd -r -p | sha256sum | cut -d ' ' -f 1
}

# phone_mms_part_row PDU_ID CT_PREFIX — first part of PDU_ID whose content type
# starts with CT_PREFIX (`_id|ct|name|chset|seq|_data`); empty when none match.
phone_mms_part_row() {
	require_int "PDU_ID" "$1"
	phone_mms_parts "$1" | awk -F'|' -v prefix="$2" '$2 ~ "^" prefix { print; exit }'
}

# phone_max_sms_id — current highest SMS `_id` (0 when the table is empty).
phone_max_sms_id() {
	sqlite_query "select coalesce(max(_id),0) from sms;" | tail -n 1
}

# phone_max_pdu_id — current highest MMS `pdu._id` (0 when the table is empty).
phone_max_pdu_id() {
	sqlite_query "select coalesce(max(_id),0) from pdu;" | tail -n 1
}

# phone_sms_rows_newer_than SMS_ID [TYPE] — rows
# (`_id|thread_id|date|type|address|body_len|sub_id`) newer than SMS_ID,
# oldest first; TYPE (1 inbox, 2 sent) filters when given.
phone_sms_rows_newer_than() {
	require_int "SMS_ID" "$1"
	local type_filter=""
	if [ "${2:-}" != "" ]; then
		require_int "TYPE" "$2"
		type_filter=" and type = $2"
	fi
	sqlite_query "select _id,thread_id,date,type,address,length(body),sub_id from sms where _id > $1${type_filter} order by _id asc;"
}

# phone_pdu_rows_newer_than PDU_ID — MMS rows (`_id|thread_id|date|msg_box`)
# newer than PDU_ID, oldest first.
phone_pdu_rows_newer_than() {
	require_int "PDU_ID" "$1"
	sqlite_query "select _id,thread_id,date,msg_box from pdu where _id > $1 order by _id asc;"
}

# phone_sms_body_hex SMS_ID — hex of one SMS row's body ('' when absent).
phone_sms_body_hex() {
	require_int "SMS_ID" "$1"
	sqlite_query "select coalesce(hex(body),'') from sms where _id = $1;" | tail -n 1
}

# phone_sms_body_sha256 SMS_ID — SHA-256 of one SMS row's body bytes.
phone_sms_body_sha256() {
	local hex
	hex="$(phone_sms_body_hex "$1")" || return $?
	printf '%s' "$hex" | xxd -r -p | sha256sum | cut -d ' ' -f 1
}

# ---------------------------------------------------------------------------
# App control surface (intent broadcasts)
# ---------------------------------------------------------------------------
# Mirrors tools/drive-app.sh, with two differences: it honours $ADB_SSH (adb
# can run on another host) and it escapes JSON/text extras for the device
# shell, so `config`/`pair`/`send` survive the ssh + adb + sh layers.

CONTROL_ACTION_PREFIX="${CONTROL_ACTION_PREFIX:-org.fossify.messages.bridge.control}"
CONTROL_RECEIVER_CLASS="${CONTROL_RECEIVER_CLASS:-org.fossify.messages.control.ControlReceiver}"

# drive_app CMD [args...] — one control command against $APP_PACKAGE.
drive_app() {
	require_env ADB_SERIAL
	local cmd="${1:-}"
	if [ -z "$cmd" ]; then
		die "drive_app requires a command (status|config|pair|unpair|send|sync)"
	fi
	shift
	local -a args=()
	while IFS= read -r arg; do
		[ -n "$arg" ] && args+=("$arg")
	done < <(adb_serial_args)
	args+=(shell am broadcast -n "$APP_PACKAGE/$CONTROL_RECEIVER_CLASS")
	if [ -n "$CONTROL_TOKEN" ]; then
		args+=(--es control_token "$(device_arg "$CONTROL_TOKEN")")
	fi
	case "$cmd" in
	status)
		args+=(-a "$CONTROL_ACTION_PREFIX.STATUS")
		;;
	config)
		[ "$#" -ge 1 ] || die "drive_app config needs a JSON argument"
		args+=(-a "$CONTROL_ACTION_PREFIX.CONFIG" --es config "$(device_arg "$1")")
		;;
	pair)
		[ "$#" -ge 1 ] || die "drive_app pair needs a JSON argument"
		args+=(-a "$CONTROL_ACTION_PREFIX.PAIR" --es pair "$(device_arg "$1")")
		;;
	unpair)
		args+=(-a "$CONTROL_ACTION_PREFIX.UNPAIR")
		;;
	send)
		[ "$#" -ge 2 ] || die "drive_app send needs <to[,to...]> <text> [file]"
		args+=(-a "$CONTROL_ACTION_PREFIX.SEND" --es to "$(device_arg "$1")" --es text "$(device_arg "$2")")
		if [ "$#" -ge 3 ]; then
			args+=(--es file "$(device_arg "$3")")
		fi
		;;
	sync)
		args+=(-a "$CONTROL_ACTION_PREFIX.SYNC")
		;;
	*)
		die "unknown drive_app command '$cmd'"
		;;
	esac
	adb_cmd "${args[@]}"
}

# drive_app_json CMD [args...] — run a control command and print the JSON
# verdict.
# Returns RC_OK, RC_MISBEHAVING (receiver answered, verdict not JSON) or
# RC_UNREACHABLE when there is no `data=` verdict — i.e. an "app build without
# control support" (a missing receiver still makes `am broadcast` exit 0 with
# no data on LineageOS).
drive_app_json() {
	local out rc=0 verdict
	out="$(drive_app "$@" 2>&1)" || rc=$?
	verdict="$(printf '%s\n' "$out" | sed -n 's/.*data=//p' | head -n 1)"
	# `am` prints the data value quoted; strip one leading/trailing quote pair.
	verdict="${verdict#\"}"
	verdict="${verdict%\"}"
	if [ -z "$verdict" ]; then
		log "drive_app_json: no Broadcast verdict for '$*' (drive-app exit $rc; app build without control support?)"
		return "$RC_UNREACHABLE"
	fi
	if ! printf '%s' "$verdict" | jq -e . >/dev/null 2>&1; then
		log "drive_app_json: unparseable verdict for '$*': $verdict"
		return "$RC_MISBEHAVING"
	fi
	printf '%s\n' "$verdict"
}

# control_send TO[,TO...] TEXT [DEVICE_FILE] — one group-aware app send via the
# control surface; prints the app's JSON verdict (tokened builds need
# CONTROL_TOKEN). Never logs the message text. An error verdict (`.error`) is
# reported as RC_MISBEHAVING.
control_send() {
	local verdict rc=0
	verdict="$(drive_app_json send "$@")" || rc=$?
	[ "$rc" -eq 0 ] || return "$rc"
	if printf '%s' "$verdict" | jq -e '.error' >/dev/null 2>&1; then
		log "control_send: app refused the send (error code $(printf '%s' "$verdict" | jq -r '.error.code // "?"'))"
		return "$RC_MISBEHAVING"
	fi
	printf '%s\n' "$verdict"
}

# prepare_attachment LOCAL_FILE [REMOTE_NAME] — push a file into
# $PHONE_ATTACHMENT_DIR (created on demand) and print the device path, ready
# for `control.SEND --es file`. Returns RC_UNREACHABLE when adb is absent.
prepare_attachment() {
	local src="$1" name="${2:-$(basename "$src")}" dst
	if [ ! -f "$src" ]; then
		die "prepare_attachment: $src is not a file"
	fi
	dst="$PHONE_ATTACHMENT_DIR/$name"
	adb_shell mkdir -p "$PHONE_ATTACHMENT_DIR" >/dev/null 2>&1 || true
	adb_push "$src" "$dst" >/dev/null || return "$RC_UNREACHABLE"
	printf '%s\n' "$dst"
}

# app_default_sms_role — print the app's default-SMS-role flag
# ("true"/"false") from control.STATUS; empty when unavailable.
app_default_sms_role() {
	local verdict rc=0
	verdict="$(drive_app_json status)" || return $?
	jq -r 'if .default_sms_role == null then empty else (.default_sms_role | tostring) end' <<<"$verdict"
}

# random_hex [BYTES] — N random bytes as lowercase hex (default 32). Used for
# generated control/app tokens; the value is never logged by the harness.
random_hex() {
	od -An -tx1 -N "${1:-32}" /dev/urandom | tr -d ' \n'
}

# adb_forward PORT — forward localhost:PORT on the adb host (local machine or
# $ADB_SSH) to the same port on the device.
adb_forward() {
	require_env ADB_SERIAL
	local port="$1"
	require_int "PORT" "$port"
	local -a args=()
	while IFS= read -r arg; do
		[ -n "$arg" ] && args+=("$arg")
	done < <(adb_serial_args)
	adb_cmd "${args[@]}" forward "tcp:$port" "tcp:$port" >/dev/null
}

# adb_forward_remove PORT — drop a forward created by adb_forward (best effort).
adb_forward_remove() {
	require_env ADB_SERIAL
	local port="$1"
	require_int "PORT" "$port"
	local -a args=()
	while IFS= read -r arg; do
		[ -n "$arg" ] && args+=("$arg")
	done < <(adb_serial_args)
	adb_cmd "${args[@]}" forward --remove "tcp:$port" >/dev/null 2>&1 || true
}

# app_http_request METHOD BASE PATH [BODY_FILE] [TOKEN] — call the app's HTTP
# API (docs/AGENT_API.md) from the host that owns the adb connection: on
# $ADB_SSH when set (so an `adb forward` there is reachable), locally otherwise.
# Prints the response body; RC_UNREACHABLE on transport failure. The token is
# passed as a header and never logged.
app_http_request() {
	local method="$1" base="$2" path="$3" body="${4:-}" token="${5:-}"
	local url rc=0 remote
	url="$(url_join "$base" "$path")"
	if [ -n "$ADB_SSH" ]; then
		remote="curl --silent --show-error --max-time $CURL_TIMEOUT --request $method $(shell_quote "$url")"
		if [ -n "$token" ]; then
			remote="$remote -H $(shell_quote "Authorization: Bearer $token")"
		fi
		if [ -n "$body" ]; then
			remote="$remote --header 'Content-Type: application/json' --data-binary @-"
			ssh "$ADB_SSH" "$remote" <"$body" || rc=$?
		else
			ssh "$ADB_SSH" "$remote" || rc=$?
		fi
	else
		local -a curl_args=(--silent --show-error --max-time "$CURL_TIMEOUT" --request "$method" "$url")
		if [ -n "$token" ]; then
			curl_args+=(-H "Authorization: Bearer $token")
		fi
		if [ -n "$body" ]; then
			curl_args+=(--header 'Content-Type: application/json' --data-binary "@$body")
		fi
		curl "${curl_args[@]}" || rc=$?
	fi
	if [ "$rc" -ne 0 ]; then
		log "app_http_request: $method $path transport failed (exit $rc)"
		return "$RC_UNREACHABLE"
	fi
}

# app_http_health BASE [TOKEN] — GET {BASE}/v1/health; prints the JSON body.
# RC_UNREACHABLE when the app HTTP API is not listening at all.
app_http_health() {
	local base="$1" token="${2:-}" body rc=0
	body="$(app_http_request GET "$base" "/v1/health" "" "$token")" || rc=$?
	[ "$rc" -eq 0 ] || return "$rc"
	printf '%s\n' "$body" | jq -e . >/dev/null 2>&1 || return "$RC_MISBEHAVING"
	printf '%s\n' "$body"
}

# ---------------------------------------------------------------------------
# Evidence helpers
# ---------------------------------------------------------------------------

# sha256_16 [file] — first 16 hex chars of the SHA-256 of a file (or stdin).
# Evidence rule: report ids/hashes only, never message bodies.
sha256_16() {
	if [ "$#" -gt 0 ]; then
		sha256sum -- "$1" | cut -c1-16
	else
		sha256sum | cut -c1-16
	fi
}

# sha256_str STRING — full SHA-256 of a string's bytes (printf without a
# trailing newline). Used to compare probe texts without ever logging them.
sha256_str() {
	printf '%s' "$1" | sha256sum | cut -d ' ' -f 1
}

# sha256_file FILE — full SHA-256 of a file.
sha256_file() {
	local file="$1"
	[ -f "$file" ] || die "sha256_file: $file is not a file"
	sha256sum -- "$file" | cut -d ' ' -f 1
}

# sha256_compare EXPECTED FILE — full-hash comparison; 0 when equal.
# Logs only the (truncated) hashes, never the file content.
sha256_compare() {
	local expected="$1" file="$2" actual
	actual="$(sha256_file "$file")"
	if [ "$expected" = "$actual" ]; then
		log "sha256_compare: $(basename "$file") matches (${actual:0:16}…)"
		return 0
	fi
	log "sha256_compare: $(basename "$file") mismatch (expected ${expected:0:16}…, got ${actual:0:16}…)"
	return 1
}

# sha256_compare_hash EXPECTED ACTUAL — full-hash comparison of two hashes.
sha256_compare_hash() {
	local expected="$1" actual="$2"
	if [ "$expected" = "$actual" ]; then
		log "sha256_compare_hash: match (${actual:0:16}…)"
		return 0
	fi
	log "sha256_compare_hash: mismatch (expected ${expected:0:16}…, got ${actual:0:16}…)"
	return 1
}

# --- iamb ------------------------------------------------------------------

# shell_quote VALUE — POSIX single-quote quoting for one remote shell layer.
shell_quote() {
	printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

# iamb_available — RC_OK when the iamb binary is present on $IAMB_SSH.
iamb_available() {
	require_env IAMB_SSH
	local rc=0
	ssh -n "$IAMB_SSH" "test -x $IAMB_BIN" >/dev/null 2>&1 || rc=$?
	if [ "$rc" -ne 0 ]; then
		return "$RC_UNREACHABLE"
	fi
	return 0
}

# iamb_cmd ARGS... — run one iamb subcommand on $IAMB_SSH. Global flags
# (--no-ipc) are inserted before the subcommand when IAMB_NO_IPC=1.
# `ssh -n` keeps the caller's stdin (while-read loops) intact; iamb bodies are
# passed with --inline/--attach, never stdin.
# Returns RC_UNREACHABLE when iamb is absent or the ssh transport fails.
iamb_cmd() {
	require_env IAMB_SSH
	local remote="$IAMB_BIN -P $(shell_quote "$IAMB_PROFILE")"
	if [ "$IAMB_NO_IPC" = "1" ]; then
		remote="$remote --no-ipc"
	fi
	local arg
	for arg in "$@"; do
		remote="$remote $(shell_quote "$arg")"
	done
	local rc=0
	ssh -n "$IAMB_SSH" "$remote" || rc=$?
	if [ "$rc" -ne 0 ]; then
		log "iamb_cmd: iamb $1 on $IAMB_SSH failed (exit $rc)"
		return "$RC_UNREACHABLE"
	fi
}

# iamb_rooms_json [QUERY] — `iamb rooms <query> --format json` (a targeted
# query is used: `rooms .` can omit freshly joined rooms in the daemon snapshot).
iamb_rooms_json() {
	iamb_cmd rooms "${1:-.}" --format json
}

# rooms_named NAME — print `room_id<TAB>name` for exact-name matches.
rooms_named() {
	local name="$1" json rc=0
	json="$(iamb_rooms_json "$name")" || return $?
	jq -r --arg n "$name" \
		'.matches[]? | select(type == "object") | select(.name == $n) | "\(.room_id)\t\(.name)"' <<<"$json"
}

# count_rooms_named NAME — number of joined rooms whose display name is exactly NAME.
count_rooms_named() {
	local name="$1" json rc=0
	json="$(iamb_rooms_json "$name")" || return $?
	jq -r --arg n "$name" \
		'[.matches[]? | select(type == "object") | select(.name == $n)] | length' <<<"$json"
}

# room_id_for_name NAME — first room id whose display name matches NAME
# (exact match first, then unique prefix). Empty when nothing matches.
# Returns RC_UNREACHABLE when iamb cannot be reached.
room_id_for_name() {
	local name="$1" json rc=0 id
	json="$(iamb_rooms_json "$name")" || return $?
	id="$(jq -r --arg n "$name" '.matches[]? | select(type == "object") | select(.name == $n) | .room_id' <<<"$json" | head -n 1)"
	if [ -z "$id" ]; then
		id="$(jq -r --arg n "$name" '.matches[]? | select(type == "object") | select(.name | startswith($n)) | .room_id' <<<"$json" | head -n 1)"
	fi
	[ -n "$id" ] && printf '%s\n' "$id"
	return 0
}

# room_ids_matching_name SUBSTR — every room whose display name contains SUBSTR.
room_ids_matching_name() {
	local substr="$1" json rc=0
	json="$(iamb_rooms_json "$substr")" || return $?
	jq -r --arg n "$substr" \
		'.matches[]? | select(type == "object") | select(.name | contains($n)) | "\(.room_id)\t\(.name)"' <<<"$json"
}

# portal_for_thread LABEL — best-effort bridge portal lookup by display name
# (for SMS portals the label is usually the phone number/participant list).
# Prints the room id; RC_UNREACHABLE when iamb is unavailable.
portal_for_thread() {
	local label="$1"
	room_id_for_name "$label"
}

# assert_one_room NAME — PASS (prints evidence line, returns 0) when exactly
# one joined room carries NAME; RC_MISBEHAVING when zero or several do.
# Never prints message bodies.
assert_one_room() {
	local name="$1" rows count
	rows="$(rooms_named "$name")" || return "$RC_UNREACHABLE"
	count="$(printf '%s\n' "$rows" | sed '/^$/d' | wc -l | tr -d ' ')"
	if [ "$count" -eq 1 ]; then
		log "assert_one_room: exactly one room named '$name': $(printf '%s' "$rows" | cut -f1)"
		return 0
	fi
	if [ "$count" -eq 0 ]; then
		log "assert_one_room: no joined room named '$name' (0 matches)"
	else
		log "assert_one_room: $count rooms named '$name': $(printf '%s' "$rows" | cut -f1 | tr '\n' ' ')"
	fi
	return "$RC_MISBEHAVING"
}

# iamb_send ROOM TEXT — send one message; prints the iamb JSON summary.
# Returns RC_UNREACHABLE on transport failure, RC_MISBEHAVING when iamb
# reports failures. TEXT is never logged.
iamb_send() {
	local room="$1" text="$2" json rc=0
	json="$(iamb_cmd send --room "$room" --inline "$text" --wait-output json)" || return "$RC_UNREACHABLE"
	if ! printf '%s' "$json" | jq -e '.messages.failures == 0 and ((.errors // []) | length) == 0' >/dev/null 2>&1; then
		log "iamb_send: send failed (room $room, failures=$(printf '%s' "$json" | jq -r '.messages.failures // "?"'), errors=$(printf '%s' "$json" | jq -r '(.errors // []) | length'))"
		return "$RC_MISBEHAVING"
	fi
	printf '%s\n' "$json"
}

# iamb_send_wait ROOM TEXT [TIMEOUT] — send and wait for a different-sender
# reply; prints the iamb JSON summary (`.wait.replies[]`).
iamb_send_wait() {
	local room="$1" text="$2" timeout="${3:-$IAMB_SEND_TIMEOUT}" json rc=0
	json="$(iamb_cmd send --room "$room" --inline "$text" --wait --timeout "$timeout" \
		--reply-mode different-sender --wait-output json)" || return "$RC_UNREACHABLE"
	if ! printf '%s' "$json" | jq -e '.messages.failures == 0 and ((.errors // []) | length) == 0' >/dev/null 2>&1; then
		log "iamb_send_wait: send failed (room $room)"
		return "$RC_MISBEHAVING"
	fi
	printf '%s\n' "$json"
}

# send_and_wait — alias used by the acceptance tests.
send_and_wait() {
	iamb_send_wait "$@"
}

# iamb_send_file ROOM LOCAL_FILE [REMOTE_NAME] — copy a local file to the ssh
# host and send it as an attachment (`send --attach`). Prints the JSON summary.
iamb_send_file() {
	require_env IAMB_SSH
	local room="$1" file="$2" name="${3:-$(basename "$2")}" rc=0 json remote
	remote="/tmp/validate-$name"
	[ -f "$file" ] || die "iamb_send_file: $file is not a file"
	scp -q "$file" "$IAMB_SSH:$remote" || return "$RC_UNREACHABLE"
	json="$(iamb_cmd send --room "$room" --attach "$remote" --wait-output json)" || rc=$?
	ssh -n "$IAMB_SSH" "rm -f $remote" >/dev/null 2>&1 || true
	[ "$rc" -eq 0 ] || return "$rc"
	if ! printf '%s' "$json" | jq -e '.attachments.failures == 0 and .messages.failures == 0 and ((.errors // []) | length) == 0' >/dev/null 2>&1; then
		log "iamb_send_file: attachment send failed (room $room)"
		return "$RC_MISBEHAVING"
	fi
	printf '%s\n' "$json"
}

# iamb_react ROOM EVENT_ID EMOJI — send a reaction to an event; prints the
# JSON summary. Used by the tapback test.
iamb_react() {
	local room="$1" event="$2" emoji="${3:-👍}" json rc=0
	json="$(iamb_cmd send --room "$room" --react-to "$event" --text "$emoji" --reaction-literal --wait-output json)" || return "$RC_UNREACHABLE"
	if ! printf '%s' "$json" | jq -e '.reactions.failures == 0 and ((.errors // []) | length) == 0' >/dev/null 2>&1; then
		log "iamb_react: reaction failed (room $room, event $event)"
		return "$RC_MISBEHAVING"
	fi
	printf '%s\n' "$json"
}

# archive_tail ROOM [LINES] — print the last LINES archive NDJSON records for
# ROOM from the iamb daemon's archive on $IAMB_SSH. Public-profile files carry
# a .ndjson suffix; personal-profile files do not — both are tried.
archive_tail() {
	require_env IAMB_SSH
	local room="$1" lines="${2:-50}" base="${room#!}"
	ssh -n "$IAMB_SSH" "tail -n $lines \"\$HOME/.local/share/iamb/profiles/$IAMB_PROFILE/events/$base.ndjson\" 2>/dev/null || tail -n $lines \"\$HOME/.local/share/iamb/profiles/$IAMB_PROFILE/events/$base\" 2>/dev/null"
}

# archive_events_since ROOM EPOCH_MS [LINES] — archive records newer than
# EPOCH_MS, newest last (ids/type/timestamps only; bodies are dropped).
archive_events_since() {
	local room="$1" since="$2" lines="${3:-200}"
	require_int "EPOCH_MS" "$since"
	archive_tail "$room" "$lines" | jq -c --argjson since "$since" \
		'select(type == "object") | select(.origin_server_ts >= $since) | {type, event_id, sender, origin_server_ts, room_id}'
}

# portal_events_since ROOM EPOCH_MS — m.room.message events of ROOM newer than
# EPOCH_MS as compact JSON lines with bodies dropped:
# {event_id, sender, origin_server_ts, msgtype, body_len, mimetype, file_name}.
# Returns RC_UNREACHABLE when iamb cannot fetch the room.
portal_events_since() {
	local room="$1" since="$2" json rc=0
	require_int "EPOCH_MS" "$since"
	json="$(iamb_cmd message "$room")" || return "$RC_UNREACHABLE"
	printf '%s' "$json" | jq -c --argjson since "$since" \
		'select(type == "object") | select(.type == "m.room.message") | select(.origin_server_ts >= $since) |
		 {event_id, sender, origin_server_ts, msgtype: (.content.msgtype // ""),
		  body_len: ((.content.body // "") | length),
		  mimetype: (.content.info.mimetype // ""),
		  file_name: (if (.content.msgtype // "") == "m.image" or (.content.msgtype // "") == "m.audio" or (.content.msgtype // "") == "m.video" or (.content.msgtype // "") == "m.file" then (.content.body // "") else "" end)}'
}

# room_member_changes ROOM [LINES] — recent membership events in ROOM
# (user ids/membership only, no message bodies).
room_member_changes() {
	local room="$1" lines="${2:-200}"
	iamb_cmd message "$room" 2>/dev/null | jq -c 'select(.type == "m.room.member") | {sender, state_key, membership: .content.membership, origin_server_ts}' 2>/dev/null | tail -n "$lines"
}

# portal_channel_id ROOM — the bridge portal key from the room's `m.bridge`
# state event (e.g. `thread-<scope>-<sub>-<thread>`). Empty when the event is
# not in the fetched window (busy rooms).
portal_channel_id() {
	local room="$1"
	iamb_cmd message "$room" 2>/dev/null | jq -r 'select(.type == "m.bridge" or .type == "uk.half-shot.bridge") | .content.channel.id // empty' 2>/dev/null | tail -n 1
}

# portal_channel_displayname ROOM — the bridge channel display name (usually
# the participant list) from `m.bridge`.
portal_channel_displayname() {
	local room="$1"
	iamb_cmd message "$room" 2>/dev/null | jq -r 'select(.type == "m.bridge" or .type == "uk.half-shot.bridge") | .content.channel.displayname // empty' 2>/dev/null | tail -n 1
}

# assert_no_own_number_ghost ROOM OWN_NUMBER — FAIL (RC_MISBEHAVING) when the
# room's own member list/timeline contains a ghost for OWN_NUMBER (escaped as
# =2b in user ids and, usually, as the raw digits in names).
assert_no_own_number_ghost() {
	local room="$1" number="$2" escaped digits hits
	digits="${number#+}"
	escaped="$(printf '%s' "$number" | sed 's/+/=2b/g')"
	hits="$(iamb_cmd message "$room" 2>/dev/null | jq -r --arg num "$number" --arg digits "$digits" --arg esc "$escaped" \
		'select(.type == "m.room.member") | .state_key // .sender // empty | select(contains($num) or contains($digits) or contains($esc))' 2>/dev/null | sort -u)"
	if [ -n "$hits" ]; then
		log "assert_no_own_number_ghost: own number $number appears as a member in $room ($(printf '%s' "$hits" | head -n 1))"
		return "$RC_MISBEHAVING"
	fi
	log "assert_no_own_number_ghost: own number $number is not a ghost in $room"
	return 0
}

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

# assert_agent_reachable — die unless $AGENT_URL answers a valid v1 health.
assert_agent_reachable() {
	local rc=0
	agent_info >/dev/null || rc=$?
	case "$rc" in
	"$RC_OK") log "agent reachable at $AGENT_URL" ;;
	"$RC_UNREACHABLE") die "agent not reachable at $AGENT_URL" ;;
	*) die "agent at $AGENT_URL is reachable but misbehaving (see above)" ;;
	esac
}

# assert_paired — best-effort pairing check.
# 1. app control surface (control.STATUS) when the app build supports it;
# 2. Matrix-side check via BRIDGE_MATRIX_URL/BRIDGE_MATRIX_TOKEN (not
#    implemented yet).
# SKIPs (returns 0) when neither source can answer; dies when a source that
# answered says "not paired".
assert_paired() {
	local rc=0 verdict paired
	verdict="$(drive_app_json status)" || rc=$?
	case "$rc" in
	"$RC_OK") ;;
	"$RC_UNREACHABLE")
		if [ -n "$BRIDGE_MATRIX_URL" ] && [ -n "$BRIDGE_MATRIX_TOKEN" ]; then
			skip "assert_paired: Matrix-side pairing check not implemented yet"
		else
			skip "assert_paired: no control verdict (app build without control support?) and no BRIDGE_MATRIX_URL/BRIDGE_MATRIX_TOKEN; cannot verify pairing"
		fi
		return 0
		;;
	*) die "assert_paired: control surface answered with an unparseable verdict" ;;
	esac
	paired="$(jq -r '.paired // empty' <<<"$verdict")"
	if [ "$paired" = "true" ]; then
		log "assert_paired: paired (device_id=$(jq -r '.device_id // "?"' <<<"$verdict"), user_id=$(jq -r '.user_id // "?"' <<<"$verdict"))"
		return 0
	fi
	if jq -e '.error.code == "disabled"' >/dev/null 2>&1 <<<"$verdict"; then
		die "assert_paired: control surface disabled (control_enabled=false); cannot verify pairing"
	fi
	die "assert_paired: app reports not paired"
}

# wait_for_room ROOM_ID — poll a Matrix-side check until the room appears.
# SKIPs unless BRIDGE_MATRIX_URL/BRIDGE_MATRIX_TOKEN are set, and SKIPs the
# real check until it is implemented (the future implementation publishes full
# !room_id evidence only).
wait_for_room() {
	local room_id="${1:-}"
	if [ -z "$room_id" ]; then
		die "wait_for_room requires a room id"
	fi
	if [ -z "$BRIDGE_MATRIX_URL" ] || [ -z "$BRIDGE_MATRIX_TOKEN" ]; then
		skip "wait_for_room: BRIDGE_MATRIX_URL/BRIDGE_MATRIX_TOKEN not set; cannot poll Matrix side"
		return 0
	fi
	# TODO(t02+): GET ${BRIDGE_MATRIX_URL}/_matrix/client/v3/rooms/{room_id}/...
	# until 200 or timeout; log ids/cursors only.
	skip "wait_for_room: Matrix-side polling not implemented yet (acceptance tests t02+)"
	return 0
}

# wait_for CONDITION_CMD TIMEOUT_SECONDS [INTERVAL] — poll a shell command
# (arguments are the command) until it exits 0 or the timeout passes.
# Returns RC_OK when the condition held, RC_UNREACHABLE on timeout.
wait_for() {
	local timeout="$1" interval="${2:-3}" started
	shift 2
	require_int "TIMEOUT_SECONDS" "$timeout"
	started="$(date +%s)"
	while :; do
		if "$@"; then
			return 0
		fi
		if [ $(( $(date +%s) - started )) -ge "$timeout" ]; then
			log "wait_for: condition timed out after ${timeout}s: $*"
			return "$RC_UNREACHABLE"
		fi
		sleep "$interval"
	done
}
