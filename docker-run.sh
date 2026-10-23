#!/bin/bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later

BINARY_NAME=/usr/bin/mautrix-android-sms-mms

# `printenv` reads the environment, unlike bash's readonly `$UID`/`$GID`
# builtins, so a container can still request a specific user (like other
# bridge images do with `ENV UID=... GID=...`).
TARGET_UID="$(printenv UID || true)"
TARGET_GID="$(printenv GID || true)"
TARGET_UID="${TARGET_UID:-$(id -u)}"
TARGET_GID="${TARGET_GID:-$(id -g)}"

# Drop privileges only when we start as root. When the caller already chose a
# user (e.g. the playbook runs the container with `--user=` and all
# capabilities dropped), `su-exec` cannot set groups, so run the command as-is.
function run {
	if [[ "$(id -u)" == "0" ]]; then
		exec su-exec "$TARGET_UID:$TARGET_GID" "$@"
	fi
	exec "$@"
}

function fixperms {
	if [[ "$(id -u)" == "0" ]]; then
		chown -R "$TARGET_UID:$TARGET_GID" /data
	fi
}

if [[ $# -gt 0 ]]; then
	# The command line given to the container (e.g. by the playbook) already names
	# its own config and registration files, so run it as-is. Bare flags (like
	# `-version`) are appended to the default binary instead.
	cd /data
	fixperms
	if [[ "$1" == -* ]]; then
		run "$BINARY_NAME" "$@"
	fi
	run "$@"
fi

if [[ ! -f /data/config.yaml ]]; then
	run "$BINARY_NAME" -c /data/config.yaml -e
	echo "Didn't find a config file."
	echo "Copied default config file to /data/config.yaml"
	echo "Modify that config file to your liking."
	echo "Start the container again after that to generate the registration file."
	exit
fi

if [[ ! -f /data/registration.yaml ]]; then
	run "$BINARY_NAME" -g -c /data/config.yaml -r /data/registration.yaml || exit $?
	echo "Didn't find a registration file."
	echo "Generated one for you."
	echo "See https://docs.mau.fi/bridges/general/registering-appservices.html on how to use it."
	exit
fi

cd /data
fixperms

run "$BINARY_NAME"
