#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# drive-app.sh — drive the bridge app from a computer over adb using the
# control intents documented in the app's docs/bridge-mode.md.
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
  pair <json>                   pair headlessly with a pairing payload
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
