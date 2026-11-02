#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# drive-app.sh — drive the bridge app from a computer over adb using the
# control intents documented in the app's docs/bridge-mode.md.
#
# `pair` also stops local adb agents that serve the phone, so app pairing
# does not duplicate the adb-based bridging.
#
# Usage:
#   tools/drive-app.sh <adb-serial> status
#   tools/drive-app.sh <adb-serial> config '<json>'
#   tools/drive-app.sh <adb-serial> pair   '<json>'
#   tools/drive-app.sh <adb-serial> unpair
#   tools/drive-app.sh <adb-serial> send '<to[,to...]>' '<text>' [file]
#   tools/drive-app.sh <adb-serial> sync
#
# Environment:
#   CONTROL_TOKEN  optional control token extra. The app also accepts callers
#                  holding android.permission.DUMP (adb shell), so this stays
#                  unset during local testing unless the app requires it.
#   APP_PACKAGE    default: org.fossify.messages.bridge
#                  the debug build: org.fossify.messages.bridge.debug
#   ACTION_PREFIX  default: org.fossify.messages.bridge.control
#
# The app answers with a single-line JSON verdict in the `data=` field of the
# `Broadcast completed: result=...` output line.

set -euo pipefail

usage() {
	cat >&2 <<'EOF'
Usage: tools/drive-app.sh <adb-serial> <command> [args...]

Commands:
  status                        show bridge/control status JSON
  config <json>                 apply configuration JSON
  pair <json|payload>           pair headlessly (also stops local adb agents for the phone)
  unpair                        remove the pairing
  send <to[,to...]> <text> [file]   one group-aware send
  sync                          heartbeat + queue poll now
EOF
}

SERIAL="${1:-}"
CMD="${2:-}"
if [ -z "$SERIAL" ] || [ -z "$CMD" ]; then
	usage
	exit 2
fi
shift 2

PKG="${APP_PACKAGE:-org.fossify.messages.bridge}"
PREFIX="${ACTION_PREFIX:-org.fossify.messages.bridge.control}"
# The application id differs from the Kotlin namespace (`org.fossify.messages`),
# so `am -n` must be given the fully qualified class after the slash: the
# shorthand `$PKG/.control.ControlReceiver` would expand to
# `$PKG.control.ControlReceiver`, which is not an installed class.
RECEIVER="$PKG/org.fossify.messages.control.ControlReceiver"

# sh_quote single-quotes its argument for the device shell: `adb shell` joins
# its arguments into one command string, so values with spaces, quotes or
# newlines (JSON configs, message texts) must be quoted for the shell that
# parses it.
sh_quote() {
	printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

run() {
	local args=(shell am broadcast -n "$RECEIVER")
	if [ -n "${CONTROL_TOKEN:-}" ]; then
		args+=(--es control_token "$(sh_quote "$CONTROL_TOKEN")")
	fi
	adb -s "$SERIAL" "${args[@]}" "$@"
}

# stop_adb_agents stops local adb agents (android-sms-mms-adb-agent, or the
# sms-agent dev binary) whose configuration serves <serial>, so switching the
# phone to app pairing cannot leave it bridged twice. Agents for other phones
# are left alone.
stop_adb_agents() {
	local serial="$1" pid cmd config stopped=0
	for pid in $(pgrep -f 'android-sms-mms-adb-agent|sms-agent' 2>/dev/null || true); do
		cmd="$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)"
		[ -z "$cmd" ] && continue
		config="$(printf '%s' "$cmd" | sed -n 's/.*-c \([^ ]*\).*/\1/p')"
		case "$cmd" in
		*"$serial"*)
			;;
		*)
			if [ -z "$config" ] || [ ! -r "$config" ] || ! grep -q "$serial" "$config" 2>/dev/null; then
				continue
			fi
			;;
		esac
		if kill "$pid" 2>/dev/null; then
			echo "stopped the adb agent (pid $pid) that serves $serial"
			stopped=1
		fi
	done
	if [ "$stopped" = "0" ]; then
		echo "no local adb agent serving $serial is running"
	else
		echo "note: if the bridge still lists an agent login for this phone, remove it with 'unpair <device_id>' in the management room"
	fi
}

case "$CMD" in
status)
	run -a "$PREFIX.STATUS"
	;;
config)
	if [ "$#" -lt 1 ]; then
		echo "config needs a JSON argument" >&2
		exit 2
	fi
	run -a "$PREFIX.CONFIG" --es config "$(sh_quote "$1")"
	;;
pair)
	if [ "$#" -lt 1 ]; then
		echo "pair needs a JSON argument" >&2
		exit 2
	fi
	stop_adb_agents "$SERIAL"
	run -a "$PREFIX.PAIR" --es pair "$(sh_quote "$1")"
	;;
unpair)
	run -a "$PREFIX.UNPAIR"
	;;
send)
	if [ "$#" -lt 2 ]; then
		echo "send needs <to[,to...]> and <text>" >&2
		exit 2
	fi
	to="$1"
	text="$2"
	shift 2
	args=(-a "$PREFIX.SEND" --es to "$(sh_quote "$to")" --es text "$(sh_quote "$text")")
	if [ "$#" -ge 1 ]; then
		args+=(--es file "$(sh_quote "$1")")
	fi
	run "${args[@]}"
	;;
sync)
	run -a "$PREFIX.SYNC"
	;;
*)
	usage
	exit 2
	;;
esac
