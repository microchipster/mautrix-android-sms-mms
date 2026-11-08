<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# SMS/MMS bridge + app - development notes

Completed work from the tracker is archived here; outstanding work lives in `TODO.md`,
and operational state, runbooks and testing-rig notes are in `docs/sms-bridge-handoff.md`.

## Progression

### App transport baseline (2026-10-26)

The adb-agent transport path was dropped as too complex; the bridge always talks to the
paired app now, and non-root operation is the baseline. See `docs/PLAN.md` for the design
narrative and `docs/fork-notes.md` for the app-side fork summary.

### Control intents (automation)

Implemented as ordered broadcasts with `setResultData(json)`:

- `control_enabled` pref (default false) plus a trust gate (control token extra or
  `android.permission.DUMP`); the receiver is inert while disabled.
- `control.STATUS`, `control.CONFIG`, `control.PAIR`, `control.UNPAIR`, `control.SEND`,
  `control.SYNC` - JSON contracts with unit tests; the token is never echoed.
- Drivers: `tools/drive-app.sh <adb-serial> status|config|pair|send|unpair|sync`, plus
  `tools/install-app.sh`, `tools/provision-app.sh` and `tools/pairsim`; usage and
  security notes in the app repo's `docs/bridge-mode.md`.

### Per-repo e2e harness

- Env contract in `scripts/validate/lib.sh` (`ADB_SERIAL`, `BRIDGE_URL`, `BRIDGE_TOKEN`,
  `SMSMMS_INFO_PATH`, `APP_PACKAGE`, `IAMB_*`/`JMP_*` pass-through) with adb/sqlite/iamb/
  attachment helpers; runbook in `docs/validation.md`.
- Acceptance suite `t02`-`t14` plus `fixtures/`, runnable against the local dev rig and
  the test deployment.
- Acceptance-matrix results: pair, group create (exactly one room, no fracture), read
  state, and invite->join all PASS; the remaining matrix items and their evidence are in
  `TODO.md`.

### CDN file service

Implemented as a zero-knowledge attachment service: files are encrypted client-side
(AES-GCM) before upload, the key lives in the URL fragment and never reaches the server,
and the server stores ciphertext only. Wrapper pages render browser-playable types only
(image/audio/video/pdf) with `nosniff` and a CSP sandbox - no HTML/SVG/JS payloads.
The unguessable id in the path is the secret; expiry defaults to 1 month (rejecting more
than 1 year); a per-user quota prunes FIFO; list/revoke/stats happen from the Matrix
management room. The service is config-gated per deployment (no domain configured means
no service). Protocols: `docs/CDN_PROTOCOL.md`; app-side UX: `docs/cdn-file-service.md`
in the app repo.

### Conventions

- Deploy order: the test deployment first, validate, then production.
- LineageOS/degoogled focus; non-root paths must keep working.
- Evidence only: ids, senders, timestamps and hashes - never message bodies.
- Harness rule: full `!room_id`s only (fuzzy names hang iamb); waits of 60-120s with
  `--reply-mode different-sender`.
- Group correctness is the top invariant: any change that risks a second room,
  fractured portals or own-number ghosts requires an acceptance-matrix re-run.

## Design considerations

- Group correctness (above) drives the portal/channel-id design and the membership
  handling.
- The app is the transport: the bridge exposes local HTTP plus heartbeat/queue polling;
  the security model is the pairing token, wrapped in the Android Keystore on the app
  side.
- The CDN security model is zero-knowledge ciphertext plus a fragment key and dumb
  wrapper pages.
- Control intents are opt-in behind an explicit trust gate.

## Current state

The bridge and app are functionally complete for the 1:1 and group SMS/MMS path,
including CDN attachments and control intents. Open items: acceptance-matrix leftovers,
the polish backlog, deployments, multi-number support and the calls follow-up - all
tracked in `TODO.md`.
