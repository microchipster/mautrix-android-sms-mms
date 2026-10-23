# Guidance for AI agents

## What this is

`mautrix-android-sms-mms`: a Matrix bridge (bridgev2, Go) to Android SMS/MMS, plus
`android-sms-mms-adb-agent`, an agent binary that talks to a phone over ADB (root or
non-root) and exposes the HTTP API in `docs/AGENT_API.md`. A fork of Fossify Messages will
provide MMS sending, true push and read-state sync (see `docs/PLAN.md`).

## Layout

- `cmd/mautrix-android-sms-mms/` — bridge entrypoint (+ management room / encryption gate).
- `cmd/android-sms-mms-adb-agent/` — agent entrypoint.
- `connector/` — bridgev2 connector: metadata types (`types.go`), ID helpers
  (`phoneid.go`), agent HTTP client, event loop, Matrix handlers, history sync, tapbacks.
- `internal/agent/` — agent internals: ADB access, telephony provider queries, sweepers,
  send backends, event log, HTTP server.
- `docs/` — plan and research. Keep `PLAN.md` and `docs/AGENT_API.md` current when the
  design changes.

## Conventions

- SPDX headers on every file (`SPDX-FileCopyrightText: 2026 Slavi Pantaleev`,
  `SPDX-License-Identifier: AGPL-3.0-or-later`).
- One Matrix room per phone **thread** (`MakePortalID`), never per sender or participant
  set; ghosts are canonical addresses (`MakeUserID`). Sender attribution: agent
  `Message.Sender == ""` = outgoing, otherwise it is the sender address.
- No fixed shared temp paths; use `os.CreateTemp` and clean up.
- Use `ansible.builtin.tempfile`-style safety in any playbook role work (roles live in the
  parent playbook repo, not here).

## Build & test

```sh
go build ./...
go vet ./...
go test ./...
./build-go.sh          # bridge binary (maubuild)
./build-agent.sh       # agent binary
```

There is no adb in CI: agent tests use fixtures/fakes; connector tests use `httptest`.
