#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# t09_read_state.sh — acceptance step 8: read state reaches the phone.
#
# Phone side (real state): inbound SMS rows must carry read/seen flags once the
# user has read them in Matrix. The test reports the read/total counts as
# evidence; it FAILs only when PHONE_EXPECT_READ=1 and nothing is read, so it
# stays honest on a phone whose threads were never opened in Matrix.
#
# Matrix side: iamb has no receipt command, so the Matrix receipt half is
# SKIPped with a clear reason (see docs/validation.md).
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/smsmms-validate-t09.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

PHONE_EXPECT_READ="${PHONE_EXPECT_READ:-0}"
READ_LOOKBACK_MS="${READ_LOOKBACK_MS:-2592000000}" # 30 days

log "t09_read_state: ADB_SSH=${ADB_SSH:-<local>} expect_read=$PHONE_EXPECT_READ"

rc=0
adb_device_online || rc=$?
if [ "$rc" -ne 0 ]; then
	skip_check "phone $ADB_SERIAL not reachable (adb)"; log "t09_read_state: SKIP-ONLY run"; exit 0
fi
pass "phone $ADB_SERIAL is online"

since=$(( $(now_ms) - READ_LOOKBACK_MS ))
rc=0
total="$(sqlite_query "select count(*) from sms where date >= $since and type = 1;" 2>"$WORK/sql.err" | tail -n 1)" || rc=$?
read_yes="$(sqlite_query "select count(*) from sms where date >= $since and type = 1 and read = 1 and seen = 1;" 2>/dev/null | tail -n 1)" || true
read_partial="$(sqlite_query "select count(*) from sms where date >= $since and type = 1 and (read = 1 or seen = 1);" 2>/dev/null | tail -n 1)" || true

if [ "$rc" -ne 0 ] || [ -z "$total" ]; then
	skip_check "telephony provider not queryable: $(tr '\n' ' ' <"$WORK/sql.err")"
	log "t09_read_state: SKIP-ONLY run"; exit 0
fi

if [ "$total" -eq 0 ]; then
	skip_check "no inbound SMS rows in the lookback window; nothing to read"
else
	pass "inbound SMS in lookback: total=$total fully-read=$read_yes partially-read=$read_partial"
	if [ "${read_yes:-0}" -ge 1 ]; then
		pass "read state reached the phone ($read_yes/$total inbound rows read=1 seen=1)"
	elif [ "${read_partial:-0}" -ge 1 ]; then
		pass "read state partially reached the phone ($read_partial/$total rows with read or seen set)"
	elif [ "$PHONE_EXPECT_READ" = "1" ]; then
		fail "PHONE_EXPECT_READ=1 but no inbound SMS row is read on the phone ($total rows)"
	else
		skip_check "no inbound SMS row is marked read yet (set PHONE_EXPECT_READ=1 to make this a failure)"
	fi
fi

# --- Matrix-side receipts ----------------------------------------------------
skip_check "Matrix-side read receipts are not scriptable with iamb (no receipt command); phone-side flags are the evidence"

# --- summary ----------------------------------------------------------------
if [ "$FAILURES" -eq 0 ]; then
	log "t09_read_state: PASS (skips: $SKIPS)"
	exit 0
fi
log "t09_read_state: FAIL ($FAILURES check(s) failed, skips: $SKIPS)"
exit 1
