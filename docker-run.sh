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
