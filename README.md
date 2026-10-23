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
- `docs/PAIRING_PROTOCOL.md` — app pairing protocol (QR + manual, queue/events API).
- `docs/SIGNING.md` — Android signing flow for the app backend.
- `docs/research-adb-reference.md` — telephony provider / ADB reference.
- `docs/research-app-base.md` — app base evaluation (Fossify fork).

## Bridge configuration (`simple-config.yaml`)

The bridge keeps connector settings in `simple-config.yaml` (referenced from the
main config as `network: simple-config.yaml`). All values have safe defaults;
older configs without the new sections keep working.

```yaml
device_name: ""                 # optional login/room-topic name override

pairing:
  # Serve the public app pairing API (`/_smsmms/v1/`). Default: true.
  enabled: true
  # Address the pairing API binds to. Default: ":29322".
  # Reverse-proxy it under the bridge's public host with TLS.
  listen: ":29322"
  # Externally reachable bridge base URL embedded in the pairing payload.
  # Default: "" (falls back to the homeserver public address).
  public_url: "https://matrix.example.com"
```

Pair with the Fossify Messages bridge app via `login app-pair` in the management
room (QR code or manual code). `devices` lists paired devices and
`unpair <device_id|all>` revokes them.

## License

AGPL-3.0-or-later.
