# mautrix-android-sms-mms

A Matrix bridge to Android **SMS and MMS** — group chats, attachments/images and
tapback reactions included — without Google services. Works with DeGoogled devices
(GrapheneOS, LineageOS) and never touches RCS.

Two components, one repo:

| Component | Runs on | Purpose |
|---|---|---|
| `mautrix-android-sms-mms` | Matrix server | The bridgev2 appservice (Matrix ↔ agent). |
| `android-sms-mms-adb-agent` | Host with the phone plugged in over USB | Talks to the phone (ADB, non-root or root; later: the Fossify fork app API) and exposes a small authenticated HTTP API the bridge consumes. |

## Status

Phase 1 (ADB read path) is being built:

- Agent: reads SMS/MMS over ADB (`content query`/`content read`, non-root capable),
  polls/sweeps, long-polls events, marks read; shell backend sends one-to-one SMS.
- Bridge: bridgev2 connector, one login per device, one Matrix room per phone
  thread (stable, no fracturing), history backfill (default 30 days), tapback
  reactions, MMS attachments.
- Not yet: MMS sending and true push — those come from the Fossify Messages fork
  (app backend), see `docs/research-app-base.md` and `docs/PLAN.md`.

## Quick start (agent)

```sh
./build-agent.sh
./android-sms-mms-adb-agent -e > agent.yaml   # example config
$EDITOR agent.yaml                            # set token, own_numbers, ...
./android-sms-mms-adb-agent -c agent.yaml
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:29320/v1/health
```

Requires `adb` on the host and a device with USB debugging enabled. Root is
optional and only adds speed/back-compat paths; GrapheneOS works without it.

## Docs

- `docs/PLAN.md` — full plan, phases, correctness spec, group-chat checklist.
- `docs/AGENT_API.md` — agent HTTP API contract.
- `docs/SIGNING.md` — Android signing flow for the app backend.
- `docs/research-adb-reference.md` — telephony provider / ADB reference.
- `docs/research-app-base.md` — app base evaluation (Fossify fork).

## License

AGPL-3.0-or-later.
