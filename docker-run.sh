#!/bin/bash
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later

if [[ -z "$GID" ]]; then
	GID="$UID"
fi

BINARY_NAME=/usr/bin/mautrix-android-sms-mms

function fixperms {
	chown -R $UID:$GID /data
}

if [[ $# -gt 0 ]]; then
	# The command line given to the container (e.g. by the playbook) already names
	# its own config and registration files, so run it as-is. Bare flags (like
	# `-version`) are appended to the default binary instead.
	cd /data
	fixperms
	if [[ "$1" == -* ]]; then
		exec su-exec $UID:$GID $BINARY_NAME "$@"
	fi
	exec su-exec $UID:$GID "$@"
fi

if [[ ! -f /data/config.yaml ]]; then
	$BINARY_NAME -c /data/config.yaml -e
	echo "Didn't find a config file."
	echo "Copied default config file to /data/config.yaml"
	echo "Modify that config file to your liking."
	echo "Start the container again after that to generate the registration file."
	exit
fi

if [[ ! -f /data/registration.yaml ]]; then
	$BINARY_NAME -g -c /data/config.yaml -r /data/registration.yaml || exit $?
	echo "Didn't find a registration file."
	echo "Generated one for you."
	echo "See https://docs.mau.fi/bridges/general/registering-appservices.html on how to use it."
	exit
fi

cd /data
fixperms

exec su-exec $UID:$GID $BINARY_NAME
