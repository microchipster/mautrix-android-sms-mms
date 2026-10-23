#!/bin/sh
# SPDX-FileCopyrightText: 2026 Slavi Pantaleev
#
# SPDX-License-Identifier: AGPL-3.0-or-later
set -eu

BINARY_NAME=mautrix-android-sms-mms go tool maubuild "$@"
