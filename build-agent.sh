#!/bin/sh
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Builds the ADB agent binary (runs on the host the phone is plugged into).
set -eu

cd "$(dirname "$0")"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o android-sms-mms-adb-agent ./cmd/android-sms-mms-adb-agent "$@"
echo "Built ./android-sms-mms-adb-agent"
